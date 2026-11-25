// Package verified provides proof-verified, partially cached Ethereum state.
//
// State is read through a regular go-ethereum StateDB whose Reader resolves
// accounts and storage by walking a *partial* Merkle-Patricia trie kept in a
// bounded content-addressed cache. Because every node is keyed by its hash and
// reached through hash links from a consensus-provided state root, anything
// that resolves locally is authenticated by construction. Missing paths are
// recorded instead of being guessed; the round driver then fetches EIP-1186
// proofs for exactly those keys, verifies them with trie.VerifyProof, inserts
// the proven nodes and re-runs. A run is only accepted once a round completes
// with no misses at all, so results never depend on unverified data.
package verified

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/trie"
	"github.com/ethereum/go-ethereum/triedb"
	"github.com/pulsechainlite/pulselite/internal/chain"
	"github.com/pulsechainlite/pulselite/internal/metrics"
	"github.com/pulsechainlite/pulselite/internal/proof"
)

// ErrTooManyRounds is returned when a run keeps discovering new state.
var ErrTooManyRounds = errors.New("state discovery did not converge")

// ErrSiblingUnresolvable is returned when a trie collapse needs a sibling
// node that cannot be located via eth_getProof within the search budget.
var ErrSiblingUnresolvable = errors.New("trie collapse sibling not resolvable via proofs")

// Source owns the verified cache and fetches missing witness material.
type Source struct {
	kv      ethdb.KeyValueStore
	mu      sync.RWMutex
	tdb     *triedb.Database
	sdb     *state.CachingDB
	backend Backend
	chain   *chain.Chain

	// SiblingDepth bounds the brute-force search for keys under a trie path.
	SiblingDepth int
	// CoFetchCode fetches contract code together with account proofs.
	CoFetchCode bool
	// NodeFetchDepth is the minimum account-trie depth (nibbles) at which a
	// miss is fetched as a single trie node rather than as a key proof.
	NodeFetchDepth int
	preimages      sync.Map // string(owner||path) -> []byte key preimage
}

// NewSource creates a Source over kv (normally a store.GenStore).
func NewSource(kv ethdb.KeyValueStore, backend Backend, ch *chain.Chain) *Source {
	s := &Source{kv: kv, backend: backend, chain: ch, SiblingDepth: 6, CoFetchCode: true, NodeFetchDepth: 0}
	s.resetTrieDB()
	return s
}

func (s *Source) resetTrieDB() {
	tdb := triedb.NewDatabase(rawdb.NewDatabase(s.kv), triedb.HashDefaults)
	s.mu.Lock()
	s.tdb = tdb
	s.sdb = state.NewDatabase(tdb, nil)
	s.mu.Unlock()
}

// StateDatabase returns the current caching database.
func (s *Source) StateDatabase() *state.CachingDB {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.sdb
}

func (s *Source) trieDB() *triedb.Database {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.tdb
}

// Chain returns the block window.
func (s *Source) Chain() *chain.Chain { return s.chain }

// Persist commits a validated post-state into the cache so subsequent blocks
// and RPC queries at the new head resolve locally.
func (s *Source) Persist(sdb *state.StateDB, number uint64, deleteEmpty bool) error {
	root, err := sdb.Commit(number, deleteEmpty, false)
	if err != nil {
		return err
	}
	tdb := s.trieDB()
	if err := tdb.Commit(root, false); err != nil {
		return err
	}
	// The hash scheme keeps uncommitted dirty nodes in memory; recycle the
	// trie database if anything lingers so memory stays bounded.
	if _, dirty, _ := tdb.Size(); dirty > 64<<20 {
		s.resetTrieDB()
	}
	return nil
}

// HasNode reports whether a trie node is cached.
func (s *Source) HasNode(hash common.Hash) bool {
	return len(rawdb.ReadLegacyTrieNode(s.kv, hash)) > 0
}

// Misses collects everything a run needed but could not resolve locally.
type Misses struct {
	mu sync.Mutex
	// Account and slot misses, each with the trie node the lookup stopped
	// at (nil if unknown), so deep misses can be fetched node by node.
	accounts map[common.Address]*sibling
	slots    map[common.Address]map[common.Hash]*sibling
	codes    map[common.Address]common.Hash
	headers  map[common.Hash]uint64
	siblings map[string]sibling
}

// sibling identifies a trie node: owner (zero for the account trie), the
// nibble path from the trie root, and the node's hash.
type sibling struct {
	owner common.Hash
	path  []byte
	hash  common.Hash
}

func nodeOf(err error) *sibling {
	var mn *trie.MissingNodeError
	if errors.As(err, &mn) {
		return &sibling{owner: mn.Owner, path: common.CopyBytes(mn.Path), hash: mn.NodeHash}
	}
	return nil
}

func newMisses() *Misses {
	return &Misses{
		accounts: make(map[common.Address]*sibling),
		slots:    make(map[common.Address]map[common.Hash]*sibling),
		codes:    make(map[common.Address]common.Hash),
		headers:  make(map[common.Hash]uint64),
		siblings: make(map[string]sibling),
	}
}

// Empty reports whether nothing was missing.
func (m *Misses) Empty() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.accounts)+len(m.slots)+len(m.codes)+len(m.headers)+len(m.siblings) == 0
}

// String summarises the misses (for diagnostics).
func (m *Misses) String() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var parts []string
	for a := range m.accounts {
		parts = append(parts, "account "+a.Hex())
	}
	for a, slots := range m.slots {
		for s := range slots {
			parts = append(parts, fmt.Sprintf("slot %s/%s", a.Hex(), s.Hex()))
		}
	}
	for a, h := range m.codes {
		parts = append(parts, fmt.Sprintf("code %s (%x)", a.Hex(), h))
	}
	for h, n := range m.headers {
		parts = append(parts, fmt.Sprintf("header #%d %x", n, h))
	}
	for _, s := range m.siblings {
		parts = append(parts, fmt.Sprintf("sibling owner=%x path=%x", s.owner, s.path))
	}
	if len(parts) > 8 {
		parts = append(parts[:8], fmt.Sprintf("... %d more", len(parts)-8))
	}
	return strings.Join(parts, "; ")
}

// Count returns the number of missing items.
func (m *Misses) Count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := len(m.accounts) + len(m.codes) + len(m.headers) + len(m.siblings)
	for _, s := range m.slots {
		n += len(s)
	}
	return n
}

func (m *Misses) addAccount(a common.Address, at *sibling) {
	m.mu.Lock()
	if _, ok := m.accounts[a]; !ok || at != nil {
		m.accounts[a] = at
	}
	m.mu.Unlock()
}

func (m *Misses) addSlot(a common.Address, slot common.Hash, at *sibling) {
	m.mu.Lock()
	if m.slots[a] == nil {
		m.slots[a] = make(map[common.Hash]*sibling)
	}
	if _, ok := m.slots[a][slot]; !ok || at != nil {
		m.slots[a][slot] = at
	}
	m.mu.Unlock()
}

func (m *Misses) addCode(a common.Address, h common.Hash) {
	m.mu.Lock()
	m.codes[a] = h
	m.mu.Unlock()
}

// AddHeader records a missing ancestor header.
func (m *Misses) AddHeader(h common.Hash, n uint64) {
	m.mu.Lock()
	m.headers[h] = n
	m.mu.Unlock()
}

func (m *Misses) addSibling(owner common.Hash, path []byte, hash common.Hash) {
	m.mu.Lock()
	m.siblings[string(owner.Bytes())+string(path)] = sibling{owner: owner, path: common.CopyBytes(path), hash: hash}
	m.mu.Unlock()
}

// Reader implements state.Reader on top of the partial trie for one root.
// Misses are recorded and reported as "absent" so that execution can
// continue and discover further accesses in the same round.
type Reader struct {
	src    *Source
	root   common.Hash
	tdb    *triedb.Database
	misses *Misses

	// In blocking mode a miss is fetched (and verified) inline instead of
	// being deferred to the next round; used for the long tail of discovery.
	blocking bool
	ctx      context.Context
	at       BlockRef
	inline   int
	post     *PostState

	mu        sync.Mutex
	accTrie   *trie.StateTrie
	accErr    error
	accounts  map[common.Address]*types.StateAccount // nil value = proven absent
	unknown   map[common.Address]bool                // account read missed
	storTries map[common.Address]*trie.StateTrie
	owners    map[common.Hash]common.Address
	slotsRead map[common.Address]map[common.Hash]struct{}
}

func (s *Source) newReader(root common.Hash) *Reader {
	return &Reader{
		src:       s,
		root:      root,
		tdb:       s.trieDB(),
		misses:    newMisses(),
		accounts:  make(map[common.Address]*types.StateAccount),
		unknown:   make(map[common.Address]bool),
		storTries: make(map[common.Address]*trie.StateTrie),
		owners:    make(map[common.Hash]common.Address),
		slotsRead: make(map[common.Address]map[common.Hash]struct{}),
	}
}

// Misses returns the misses recorded so far.
func (r *Reader) Misses() *Misses { return r.misses }

// resetRound clears per-round state before the next discovery round. Data
// that resolved locally stays memoised (it is verified and root-bound);
// lookups that missed are retried against the updated cache.
func (r *Reader) resetRound() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.misses = newMisses()
	r.unknown = make(map[common.Address]bool)
	r.inline = 0
	r.blocking = false
}

// Root returns the state root this reader serves.
func (r *Reader) Root() common.Hash { return r.root }

func isMissing(err error) bool {
	var mn *trie.MissingNodeError
	return errors.As(err, &mn)
}

func (r *Reader) accountLocked(addr common.Address) (*types.StateAccount, bool, error) {
	if acc, ok := r.accounts[addr]; ok {
		return acc, true, nil
	}
	if r.unknown[addr] {
		return nil, false, nil
	}
	r.owners[crypto.Keccak256Hash(addr.Bytes())] = addr
	if r.accTrie == nil && r.accErr == nil {
		r.accTrie, r.accErr = trie.NewStateTrie(trie.StateTrieID(r.root), r.tdb)
	}
	if r.accErr != nil {
		if isMissing(r.accErr) {
			r.accErr = nil // root node may arrive in a later round; retry opening next time
			if r.fetchInline(func(m *Misses) { m.addAccount(addr, nil) }) {
				r.accTrie, r.accErr = trie.NewStateTrie(trie.StateTrieID(r.root), r.tdb)
				if r.accErr == nil {
					return r.accountLocked(addr)
				}
				r.accErr = nil
			}
			r.unknown[addr] = true
			r.misses.addAccount(addr, nil)
			return nil, false, nil
		}
		return nil, false, r.accErr
	}
	acc, err := r.accTrie.GetAccount(addr)
	if err != nil {
		if isMissing(err) {
			if r.fetchInline(func(m *Misses) { m.addAccount(addr, nil) }) {
				if acc, err = r.accTrie.GetAccount(addr); err == nil {
					r.accounts[addr] = acc
					return acc, true, nil
				}
			}
			r.unknown[addr] = true
			r.misses.addAccount(addr, nodeOf(err))
			return nil, false, nil
		}
		return nil, false, err
	}
	r.accounts[addr] = acc
	return acc, true, nil
}

// fetchInline resolves a miss synchronously in blocking mode. The caller
// holds r.mu. It returns true if the fetch succeeded.
func (r *Reader) fetchInline(add func(m *Misses)) bool {
	if !r.blocking {
		return false
	}
	tmp := r.src.newReader(r.root)
	add(tmp.misses)
	tmp.owners = r.owners
	if _, err := r.src.fetch(r.ctx, r.root, r.at, tmp); err != nil {
		log.Debug("Inline fetch failed", "err", err)
		return false
	}
	r.inline++
	return true
}

// ResolveHeader returns an ancestor header, fetching it inline in blocking
// mode or recording a miss otherwise.
func (r *Reader) ResolveHeader(hash common.Hash, number uint64) *types.Header {
	ch := r.src.chain
	if ch == nil {
		return nil
	}
	if h := ch.Header(hash); h != nil {
		return h
	}
	r.mu.Lock()
	ok := r.fetchInline(func(m *Misses) { m.AddHeader(hash, number) })
	r.mu.Unlock()
	if ok {
		if h := ch.Header(hash); h != nil {
			return h
		}
	}
	r.misses.AddHeader(hash, number)
	return nil
}

// Account implements state.StateReader.
func (r *Reader) Account(addr common.Address) (*types.StateAccount, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	acc, _, err := r.accountLocked(addr)
	if err != nil || acc == nil {
		return nil, err
	}
	return acc.Copy(), nil
}

// Storage implements state.StateReader.
func (r *Reader) Storage(addr common.Address, slot common.Hash) (common.Hash, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.slotsRead[addr] == nil {
		r.slotsRead[addr] = make(map[common.Hash]struct{})
	}
	r.slotsRead[addr][slot] = struct{}{}
	acc, known, err := r.accountLocked(addr)
	if err != nil {
		return common.Hash{}, err
	}
	if !known {
		// Fetch the slot together with the account in the next round.
		r.misses.addSlot(addr, slot, nil)
		return common.Hash{}, nil
	}
	if acc == nil || acc.Root == types.EmptyRootHash {
		return common.Hash{}, nil
	}
	val, err := r.storageLocked(addr, acc, slot)
	if isMissing(err) && r.fetchInline(func(m *Misses) { m.addSlot(addr, slot, nil) }) {
		val, err = r.storageLocked(addr, acc, slot)
	}
	if err != nil {
		if isMissing(err) {
			r.misses.addSlot(addr, slot, nodeOf(err))
			return common.Hash{}, nil
		}
		return common.Hash{}, err
	}
	return val, nil
}

func (r *Reader) storageLocked(addr common.Address, acc *types.StateAccount, slot common.Hash) (common.Hash, error) {
	st := r.storTries[addr]
	if st == nil {
		var err error
		st, err = trie.NewStateTrie(trie.StorageTrieID(r.root, crypto.Keccak256Hash(addr.Bytes()), acc.Root), r.tdb)
		if err != nil {
			return common.Hash{}, err
		}
		r.storTries[addr] = st
	}
	val, err := st.GetStorage(addr, slot.Bytes())
	if err != nil {
		return common.Hash{}, err
	}
	return common.BytesToHash(val), nil
}

// Code implements state.ContractCodeReader.
func (r *Reader) Code(addr common.Address, codeHash common.Hash) ([]byte, error) {
	if codeHash == types.EmptyCodeHash || codeHash == (common.Hash{}) {
		return nil, nil
	}
	if code := rawdb.ReadCode(r.src.kv, codeHash); len(code) > 0 {
		return code, nil
	}
	r.mu.Lock()
	ok := r.fetchInline(func(m *Misses) { m.addCode(addr, codeHash) })
	r.mu.Unlock()
	if ok {
		if code := rawdb.ReadCode(r.src.kv, codeHash); len(code) > 0 {
			return code, nil
		}
	}
	r.misses.addCode(addr, codeHash)
	return nil, nil
}

// CodeSize implements state.ContractCodeReader.
func (r *Reader) CodeSize(addr common.Address, codeHash common.Hash) (int, error) {
	code, err := r.Code(addr, codeHash)
	return len(code), err
}

// RecordStateError inspects a StateDB error. Missing trie nodes hit while
// hashing (typically the sibling of a deleted key during branch collapse)
// are recorded for resolution; true is returned if the error was absorbed.
func (r *Reader) RecordStateError(err error) bool {
	if err == nil {
		return true
	}
	mn, ok := ParseMissingNode(err)
	if !ok {
		return false
	}
	r.misses.addSibling(mn.Owner, mn.Path, mn.NodeHash)
	return true
}

func (r *Reader) ownerAddress(owner common.Hash) (common.Address, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	a, ok := r.owners[owner]
	return a, ok
}

// RunFunc executes against a reader; misses are detected by the driver.
type RunFunc func(r *Reader) error

// RunStats describes a completed run.
type RunStats struct {
	Rounds  int
	Fetched int
}

// Run executes fn against root until a round completes without misses,
// fetching and verifying the missing witness material between rounds.
func (s *Source) Run(ctx context.Context, root common.Hash, at BlockRef, maxRounds int, fn RunFunc) (RunStats, error) {
	return s.RunWithPost(ctx, root, at, nil, maxRounds, fn)
}

// RunWithPost is Run with a claimed post-state used to locate collapse
// siblings (block validation).
func (s *Source) RunWithPost(ctx context.Context, root common.Hash, at BlockRef, post *PostState, maxRounds int, fn RunFunc) (RunStats, error) {
	var stats RunStats
	prevMisses := -1
	// One reader serves all rounds: verified lookups (accounts, opened
	// storage tries) carry over; only the per-round miss state is reset.
	r := s.newReader(root)
	for round := 1; ; round++ {
		stats.Rounds = round
		r.resetRound()
		r.ctx, r.at, r.post = ctx, at, post
		// Bulk speculative discovery first; switch to inline fetching for the
		// long tail (few misses left, or several rounds already spent).
		r.blocking = round > 1 && (prevMisses <= 2 || round >= maxRounds-4)
		execStart := time.Now()
		ferr := fn(r)
		stats.Fetched += r.inline
		log.Debug("Discovery round", "block", at.Number, "round", round, "blocking", r.blocking, "inline", r.inline,
			"misses", r.misses.Count(), "elapsed", time.Since(execStart).Round(time.Millisecond))
		if r.misses.Empty() {
			return stats, ferr
		}
		prevMisses = r.misses.Count()
		if round >= maxRounds {
			return stats, fmt.Errorf("%w after %d rounds (%d items still missing: %s)", ErrTooManyRounds, round, r.misses.Count(), r.misses)
		}
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		fetchStart := time.Now()
		n, err := s.fetch(ctx, root, at, r)
		stats.Fetched += n
		log.Debug("Fetched round witness", "block", at.Number, "round", round, "items", n, "elapsed", time.Since(fetchStart).Round(time.Millisecond), "err", err)
		if err != nil {
			return stats, err
		}
	}
}

// Prefetch runs fn once with inline (blocking) fetching to warm the cache.
// The outcome of fn is discarded; it only serves to pull in witness data.
func (s *Source) Prefetch(ctx context.Context, root common.Hash, at BlockRef, fn func(r *Reader)) int {
	r := s.newReader(root)
	r.ctx, r.at, r.blocking = ctx, at, true
	fn(r)
	return r.inline
}

// Hints asks the backend for access lists, if it supports them.
func (s *Source) Hints(ctx context.Context, at BlockRef, calls []CallHint) ([]types.AccessList, bool) {
	hb, ok := s.backend.(HintBackend)
	if !ok {
		return nil, false
	}
	lists, err := hb.AccessLists(ctx, at, calls)
	if err != nil {
		return nil, false
	}
	return lists, true
}

// PrefetchHinted fetches, in one verified batch, every hinted account and
// slot that does not already resolve from the local partial trie.
func (s *Source) PrefetchHinted(ctx context.Context, root common.Hash, at BlockRef, addrs []common.Address, lists []types.AccessList) (int, error) {
	r := s.newReader(root)
	for _, a := range addrs {
		r.Account(a)
	}
	for _, l := range lists {
		for _, t := range l {
			r.Account(t.Address)
			for _, k := range t.StorageKeys {
				r.Storage(t.Address, k)
			}
		}
	}
	if r.misses.Empty() {
		return 0, nil
	}
	n, err := s.fetch(ctx, root, at, r)
	if err != nil {
		return n, err
	}
	// Second pass: storage of accounts that were unknown in the first pass.
	r2 := s.newReader(root)
	for _, l := range lists {
		for _, t := range l {
			for _, k := range t.StorageKeys {
				r2.Storage(t.Address, k)
			}
		}
	}
	if r2.misses.Empty() {
		return n, nil
	}
	m, err := s.fetch(ctx, root, at, r2)
	return n + m, err
}

// PrefetchAccounts fetches proofs for a set of accounts in one batch.
func (s *Source) PrefetchAccounts(ctx context.Context, root common.Hash, at BlockRef, addrs []common.Address) error {
	r := s.newReader(root)
	for _, a := range addrs {
		r.Account(a) // records a miss only if not resolvable locally
	}
	if r.misses.Empty() {
		return nil
	}
	_, err := s.fetch(ctx, root, at, r)
	return err
}

// fetch resolves all misses of r and inserts verified data into the cache.
func (s *Source) fetch(ctx context.Context, root common.Hash, at BlockRef, r *Reader) (int, error) {
	m := r.misses
	m.mu.Lock()
	// Deep misses (below locally cached upper trie levels) are fetched node
	// by node in bulk when the backend supports it; everything else (cold,
	// shallow, or inline) by key proofs, which return a whole path at once.
	batchNodes, _ := s.backend.(BatchPathBackend)
	useNodes := batchNodes != nil && !r.blocking
	type nodeWant struct {
		req   NodeRequest
		addr  common.Address
		slots []common.Hash
		acct  bool
	}
	nodeWants := make(map[common.Hash]*nodeWant)
	byAddr := make(map[common.Address]map[common.Hash]struct{})
	addKey := func(a common.Address, slot *common.Hash) {
		if byAddr[a] == nil {
			byAddr[a] = make(map[common.Hash]struct{})
		}
		if slot != nil {
			byAddr[a][*slot] = struct{}{}
		}
	}
	for a, at := range m.accounts {
		if useNodes && at != nil && len(at.path) >= s.NodeFetchDepth {
			w := nodeWants[at.hash]
			if w == nil {
				w = &nodeWant{req: NodeRequest{Owner: at.owner, Path: at.path, Hash: at.hash}, addr: a, acct: true}
				nodeWants[at.hash] = w
			}
			w.req.Keys = append(w.req.Keys, crypto.Keccak256(a.Bytes()))
			continue
		}
		addKey(a, nil)
	}
	for a, slots := range m.slots {
		for sl, at := range slots {
			if useNodes && at != nil {
				w := nodeWants[at.hash]
				if w == nil {
					w = &nodeWant{req: NodeRequest{Owner: at.owner, Path: at.path, Hash: at.hash}, addr: a}
					nodeWants[at.hash] = w
				}
				w.slots = append(w.slots, sl)
				w.req.Keys = append(w.req.Keys, crypto.Keccak256(sl.Bytes()))
				continue
			}
			sl := sl
			addKey(a, &sl)
		}
	}
	codes := make(map[common.Address]common.Hash, len(m.codes))
	for a, h := range m.codes {
		codes[a] = h
	}
	headers := make([]common.Hash, 0, len(m.headers))
	for h := range m.headers {
		headers = append(headers, h)
	}
	sibs := make([]sibling, 0, len(m.siblings))
	for _, sb := range m.siblings {
		sibs = append(sibs, sb)
	}
	m.mu.Unlock()

	// Collapse siblings: find a key whose hashed path runs through the missing
	// node; a proof for that key (present or absent) contains the node.
	for _, sb := range sibs {
		if s.HasNode(sb.hash) {
			continue
		}
		if pb, ok := s.backend.(PathNodeBackend); ok {
			if blob, err := pb.TrieNode(ctx, at, sb.owner, sb.path); err == nil && crypto.Keccak256Hash(blob) == sb.hash {
				rawdb.WriteLegacyTrieNode(s.kv, sb.hash, blob)
				metrics.SiblingSearches.Inc()
				continue
			}
		}
		if s.resolveFromPostState(ctx, r.post, r, sb, sb.hash) {
			continue
		}
		if sb.owner == (common.Hash{}) {
			pre, ok := s.findPreimage(sb.owner, sb.path, 20)
			if !ok {
				metrics.SiblingFailed.Inc()
				return 0, fmt.Errorf("%w: account trie path %x", ErrSiblingUnresolvable, sb.path)
			}
			metrics.SiblingSearches.Inc()
			a := common.BytesToAddress(pre)
			if byAddr[a] == nil {
				byAddr[a] = make(map[common.Hash]struct{})
			}
			continue
		}
		addr, ok := r.ownerAddress(sb.owner)
		if !ok {
			metrics.SiblingFailed.Inc()
			return 0, fmt.Errorf("%w: unknown storage owner %x", ErrSiblingUnresolvable, sb.owner)
		}
		pre, ok := s.findPreimage(sb.owner, sb.path, 32)
		if !ok {
			metrics.SiblingFailed.Inc()
			return 0, fmt.Errorf("%w: storage trie %x path %x", ErrSiblingUnresolvable, sb.owner, sb.path)
		}
		metrics.SiblingSearches.Inc()
		if byAddr[addr] == nil {
			byAddr[addr] = make(map[common.Hash]struct{})
		}
		byAddr[addr][common.BytesToHash(pre)] = struct{}{}
	}

	fetched := 0
	if len(nodeWants) > 0 {
		wants := make([]*nodeWant, 0, len(nodeWants))
		reqs := make([]NodeRequest, 0, len(nodeWants))
		for _, w := range nodeWants {
			wants = append(wants, w)
			reqs = append(reqs, w.req)
		}
		nodes, err := batchNodes.TrieNodesBatch(ctx, at, reqs)
		if err != nil {
			nodes = nil
		}
		batch := s.kv.NewBatch()
		for h, blob := range nodes {
			if crypto.Keccak256Hash(blob) == h {
				rawdb.WriteLegacyTrieNode(batch, h, blob)
			}
		}
		for _, w := range wants {
			if _, ok := nodes[w.req.Hash]; ok {
				metrics.ProofOK.Inc()
				fetched++
				continue
			}
			// Not served (or wrong): fall back to key proofs.
			if w.acct {
				addKey(w.addr, nil)
			}
			for _, sl := range w.slots {
				sl := sl
				addKey(w.addr, &sl)
			}
		}
		if err := batch.Write(); err != nil {
			return fetched, err
		}
		// Accounts that now resolve locally and are contracts will need their
		// code next: request it in this pass instead of the next round.
		if s.CoFetchCode {
			if tr, err := trie.NewStateTrie(trie.StateTrieID(root), s.trieDB()); err == nil {
				for _, w := range wants {
					if !w.acct {
						continue
					}
					if acc, err := tr.GetAccount(w.addr); err == nil && acc != nil {
						ch := common.BytesToHash(acc.CodeHash)
						if ch != types.EmptyCodeHash && !rawdb.HasCode(s.kv, ch) {
							codes[w.addr] = ch
						}
					}
				}
			}
		}
	}

	var reqs []ProofRequest
	for a, slots := range byAddr {
		list := make([]common.Hash, 0, len(slots))
		for sl := range slots {
			list = append(list, sl)
		}
		if len(list) == 0 {
			reqs = append(reqs, ProofRequest{Address: a})
		}
		for i := 0; i < len(list); i += 64 {
			reqs = append(reqs, ProofRequest{Address: a, Slots: list[i:min(i+64, len(list))]})
		}
	}
	if len(reqs) > 0 {
		results, err := s.backend.Proofs(ctx, at, reqs)
		if err != nil {
			return fetched, fmt.Errorf("fetch proofs at #%d: %w", at.Number, err)
		}
		batch := s.kv.NewBatch()
		for i, res := range results {
			v, err := proof.VerifyAccount(root, reqs[i].Address, res)
			if err != nil {
				metrics.ProofFail.Inc()
				return fetched, fmt.Errorf("proof for %x rejected: %w", reqs[i].Address, err)
			}
			metrics.ProofOK.Inc()
			for h, n := range v.Nodes {
				rawdb.WriteLegacyTrieNode(batch, h, n)
			}
			fetched += 1 + len(reqs[i].Slots)
			// Contracts touched during execution almost always need their
			// code next; fetch it in the same pass to save a round trip.
			if v.Account != nil && s.CoFetchCode {
				ch := common.BytesToHash(v.Account.CodeHash)
				if ch != types.EmptyCodeHash && ch != (common.Hash{}) && !rawdb.HasCode(s.kv, ch) {
					if _, ok := codes[reqs[i].Address]; !ok {
						codes[reqs[i].Address] = ch
					}
				}
			}
		}
		if err := batch.Write(); err != nil {
			return fetched, err
		}
	}
	if len(codes) > 0 {
		creqs := make([]CodeRequest, 0, len(codes))
		for a, h := range codes {
			creqs = append(creqs, CodeRequest{Address: a, Hash: h})
		}
		blobs, err := s.backend.Codes(ctx, at, creqs)
		if err != nil {
			return fetched, fmt.Errorf("fetch code: %w", err)
		}
		for i, code := range blobs {
			if err := proof.VerifyCode(creqs[i].Hash, code); err != nil {
				metrics.CodeFail.Inc()
				return fetched, fmt.Errorf("code for %x rejected: %w", creqs[i].Address, err)
			}
			metrics.CodeOK.Inc()
			rawdb.WriteCode(s.kv, creqs[i].Hash, code)
			fetched++
		}
	}
	if len(headers) > 0 && s.chain != nil {
		hs, err := s.backend.HeadersByHash(ctx, headers)
		if err != nil {
			return fetched, fmt.Errorf("fetch headers: %w", err)
		}
		for i, h := range hs {
			if h.Hash() != headers[i] {
				metrics.HeaderFail.Inc()
				return fetched, fmt.Errorf("header %x rejected: hash mismatch", headers[i])
			}
			metrics.HeaderOK.Inc()
			s.chain.AddHeader(h)
			fetched++
		}
	}
	log.Debug("Fetched verified witness", "block", at.Number, "proofs", len(reqs), "codes", len(codes), "headers", len(headers), "siblings", len(sibs))
	return fetched, nil
}
