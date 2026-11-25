package p2pnet

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/eth/protocols/eth"
	"github.com/ethereum/go-ethereum/eth/protocols/snap"
	"github.com/ethereum/go-ethereum/ethdb/memorydb"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/trie"
	"github.com/ethereum/go-ethereum/triedb"
	"github.com/pulsechainlite/pulselite/internal/proof"
	"github.com/pulsechainlite/pulselite/internal/verified"
)

// Backend implements verified.Backend over the devp2p snap/eth protocols.
// Snap range responses are converted into EIP-1186-shaped results so they go
// through exactly the same verification as any other source.
type Backend struct {
	Node SnapClient
	// RootOf maps a block to its (consensus-provided) state root.
	RootOf func(at verified.BlockRef) (common.Hash, bool)
	// Parallel bounds concurrent snap requests.
	Parallel int
}

// SnapClient is the subset of the node used by the state backend.
type SnapClient interface {
	AccountRange(ctx context.Context, root, key common.Hash) (*snap.AccountRangePacket, error)
	StorageRange(ctx context.Context, root, account, slot common.Hash) (*snap.StorageRangesPacket, error)
	ByteCodes(ctx context.Context, hashes []common.Hash) ([][]byte, error)
	TrieNodes(ctx context.Context, root common.Hash, paths []snap.TrieNodePathSet) ([][]byte, error)
	HeaderByHash(ctx context.Context, h common.Hash) (*types.Header, error)
}

func toHex(nodes [][]byte) []hexutil.Bytes {
	out := make([]hexutil.Bytes, len(nodes))
	for i, n := range nodes {
		out[i] = n
	}
	return out
}

// Proofs implements verified.Backend.
func (b *Backend) Proofs(ctx context.Context, at verified.BlockRef, reqs []verified.ProofRequest) ([]*proof.AccountResult, error) {
	root, ok := b.RootOf(at)
	if !ok {
		return nil, fmt.Errorf("unknown state root for block %d", at.Number)
	}
	out := make([]*proof.AccountResult, len(reqs))
	errs := make([]error, len(reqs))
	sem := make(chan struct{}, max(b.Parallel, 1))
	var wg sync.WaitGroup
	for i := range reqs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			out[i], errs[i] = b.accountProof(ctx, root, reqs[i], sem)
		}(i)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (b *Backend) accountProof(ctx context.Context, root common.Hash, req verified.ProofRequest, sem chan struct{}) (*proof.AccountResult, error) {
	accHash := crypto.Keccak256Hash(req.Address.Bytes())
	sem <- struct{}{}
	ar, err := b.Node.AccountRange(ctx, root, accHash)
	<-sem
	if err != nil {
		return nil, fmt.Errorf("account %x: %w", req.Address, err)
	}
	res := &proof.AccountResult{
		Address:      req.Address,
		AccountProof: toHex(ar.Proof),
		Balance:      (*hexutil.Big)(new(big.Int)),
		CodeHash:     types.EmptyCodeHash,
		StorageHash:  types.EmptyRootHash,
	}
	if len(ar.Accounts) > 0 && ar.Accounts[0].Hash == accHash {
		acc, err := types.FullAccount(ar.Accounts[0].Body)
		if err != nil {
			return nil, fmt.Errorf("account %x: bad body: %w", req.Address, err)
		}
		res.Balance = (*hexutil.Big)(acc.Balance.ToBig())
		res.Nonce = hexutil.Uint64(acc.Nonce)
		res.CodeHash = common.BytesToHash(acc.CodeHash)
		res.StorageHash = acc.Root
	}
	if len(req.Slots) == 0 {
		return res, nil
	}
	res.StorageProof = make([]proof.StorageResult, len(req.Slots))
	if res.StorageHash == types.EmptyRootHash {
		for i, s := range req.Slots {
			res.StorageProof[i] = proof.StorageResult{Key: s.Hex(), Value: (*hexutil.Big)(new(big.Int))}
		}
		return res, nil
	}
	errs := make([]error, len(req.Slots))
	var wg sync.WaitGroup
	for i, s := range req.Slots {
		wg.Add(1)
		go func(i int, s common.Hash) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			res.StorageProof[i], errs[i] = b.slotProof(ctx, root, accHash, res.StorageHash, s)
		}(i, s)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, fmt.Errorf("account %x: %w", req.Address, err)
		}
	}
	return res, nil
}

func (b *Backend) slotProof(ctx context.Context, root, accHash, storageRoot, slot common.Hash) (proof.StorageResult, error) {
	slotHash := crypto.Keccak256Hash(slot.Bytes())
	sr, err := b.Node.StorageRange(ctx, root, accHash, slotHash)
	if err != nil {
		return proof.StorageResult{}, fmt.Errorf("slot %x: %w", slot, err)
	}
	out := proof.StorageResult{Key: slot.Hex(), Value: (*hexutil.Big)(new(big.Int)), Proof: toHex(sr.Proof)}
	var slots []*snap.StorageData
	if len(sr.Slots) > 0 {
		slots = sr.Slots[0]
	}
	if len(slots) > 0 && slots[0].Hash == slotHash {
		_, content, _, err := rlp.Split(slots[0].Body)
		if err != nil {
			return out, fmt.Errorf("slot %x: bad value: %w", slot, err)
		}
		out.Value = (*hexutil.Big)(new(big.Int).SetBytes(content))
	}
	if len(sr.Proof) == 0 {
		// The peer returned the account's complete storage without a proof.
		// Rebuild that trie locally; it is accepted only if it hashes to the
		// verified storage root, then a regular proof is derived from it.
		nodes, err := proveFromCompleteStorage(slots, storageRoot, slotHash)
		if err != nil {
			return out, fmt.Errorf("slot %x: %w", slot, err)
		}
		out.Proof = toHex(nodes)
	}
	return out, nil
}

var errStorageRoot = errors.New("complete storage does not match storage root")

func proveFromCompleteStorage(slots []*snap.StorageData, storageRoot, key common.Hash) ([][]byte, error) {
	tr := trie.NewEmpty(triedb.NewDatabase(rawdb.NewMemoryDatabase(), nil))
	for _, s := range slots {
		if err := tr.Update(s.Hash.Bytes(), s.Body); err != nil {
			return nil, err
		}
	}
	if tr.Hash() != storageRoot {
		return nil, errStorageRoot
	}
	db := memorydb.New()
	if err := tr.Prove(key.Bytes(), db); err != nil {
		return nil, err
	}
	var nodes [][]byte
	it := db.NewIterator(nil, nil)
	for it.Next() {
		nodes = append(nodes, common.CopyBytes(it.Value()))
	}
	it.Release()
	return nodes, nil
}

// Codes implements verified.Backend.
func (b *Backend) Codes(ctx context.Context, _ verified.BlockRef, reqs []verified.CodeRequest) ([][]byte, error) {
	found := make(map[common.Hash][]byte)
	var want []common.Hash
	for _, r := range reqs {
		want = append(want, r.Hash)
	}
	for attempt := 0; attempt < 4 && len(want) > 0; attempt++ {
		var missing []common.Hash
		for i := 0; i < len(want); i += 64 {
			chunk := want[i:min(i+64, len(want))]
			codes, err := b.Node.ByteCodes(ctx, chunk)
			if err != nil {
				missing = append(missing, chunk...)
				continue
			}
			for _, c := range codes {
				found[crypto.Keccak256Hash(c)] = c
			}
			for _, h := range chunk {
				if _, ok := found[h]; !ok {
					missing = append(missing, h)
				}
			}
		}
		want = missing
	}
	out := make([][]byte, len(reqs))
	for i, r := range reqs {
		c, ok := found[r.Hash]
		if !ok {
			return nil, fmt.Errorf("code %x not available from peers", r.Hash)
		}
		out[i] = c
	}
	return out, nil
}

// TrieNode implements verified.PathNodeBackend via snap GetTrieNodes; the
// caller verifies the node against the hash referenced by its parent.
func (b *Backend) TrieNode(ctx context.Context, at verified.BlockRef, owner common.Hash, path []byte) ([]byte, error) {
	root, ok := b.RootOf(at)
	if !ok {
		return nil, fmt.Errorf("unknown state root for block %d", at.Number)
	}
	set := snap.TrieNodePathSet{compactPath(path)}
	if owner != (common.Hash{}) {
		set = snap.TrieNodePathSet{owner.Bytes(), compactPath(path)}
	}
	nodes, err := b.Node.TrieNodes(ctx, root, []snap.TrieNodePathSet{set})
	if err != nil {
		return nil, err
	}
	return nodes[0], nil
}

// TrieNodesBatch implements verified.BatchPathBackend. For every request it
// asks for the node at the missing path and, for each key routed through
// it, every deeper prefix of that key's path; the serving peer walks its own
// trie by path, so a whole missing branch arrives in one round trip. Paths
// that do not end on a node come back empty. Everything received is returned
// keyed by its keccak hash.
func (b *Backend) TrieNodesBatch(ctx context.Context, at verified.BlockRef, reqs []verified.NodeRequest) (map[common.Hash][]byte, error) {
	root, ok := b.RootOf(at)
	if !ok {
		return nil, fmt.Errorf("unknown state root for block %d", at.Number)
	}
	paths := map[common.Hash]map[string]struct{}{}
	add := func(owner common.Hash, nibbles []byte) {
		if paths[owner] == nil {
			paths[owner] = map[string]struct{}{}
		}
		paths[owner][string(nibbles)] = struct{}{}
	}
	for _, r := range reqs {
		add(r.Owner, r.Path)
		extra := 10
		if r.Owner != (common.Hash{}) {
			extra = 8
		}
		for _, k := range r.Keys {
			kn := keyNibbles(k)
			for l := len(r.Path) + 1; l <= min(len(kn), len(r.Path)+extra); l++ {
				add(r.Owner, kn[:l])
			}
		}
	}
	// Build path sets: one per account-trie path, storage paths grouped per
	// owner; then chunks of at most ~384 paths.
	var sets []snap.TrieNodePathSet
	for owner, ps := range paths {
		if owner == (common.Hash{}) {
			for p := range ps {
				sets = append(sets, snap.TrieNodePathSet{compactPath([]byte(p))})
			}
			continue
		}
		set := snap.TrieNodePathSet{owner.Bytes()}
		for p := range ps {
			set = append(set, compactPath([]byte(p)))
			if len(set) > 128 {
				sets = append(sets, set)
				set = snap.TrieNodePathSet{owner.Bytes()}
			}
		}
		if len(set) > 1 {
			sets = append(sets, set)
		}
	}
	var chunks [][]snap.TrieNodePathSet
	var cur []snap.TrieNodePathSet
	n := 0
	for _, s := range sets {
		cnt := max(len(s)-1, 1)
		if n+cnt > 384 && len(cur) > 0 {
			chunks = append(chunks, cur)
			cur, n = nil, 0
		}
		cur = append(cur, s)
		n += cnt
	}
	if len(cur) > 0 {
		chunks = append(chunks, cur)
	}
	var (
		mu  sync.Mutex
		out = map[common.Hash][]byte{}
		wg  sync.WaitGroup
		sem = make(chan struct{}, max(b.Parallel/4, 4))
	)
	for _, ch := range chunks {
		wg.Add(1)
		go func(ch []snap.TrieNodePathSet) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			nodes, err := b.Node.TrieNodes(ctx, root, ch)
			if err != nil {
				return
			}
			mu.Lock()
			for _, blob := range nodes {
				if len(blob) > 0 {
					out[crypto.Keccak256Hash(blob)] = blob
				}
			}
			mu.Unlock()
		}(ch)
	}
	wg.Wait()
	// Second pass for requested nodes that were not served (peers stop at
	// size limits): ask again for just those paths.
	var retry []verified.NodeRequest
	for _, r := range reqs {
		if _, ok := out[r.Hash]; !ok && len(r.Keys) > 0 {
			retry = append(retry, verified.NodeRequest{Owner: r.Owner, Path: r.Path, Hash: r.Hash})
		}
	}
	if len(retry) > 0 && len(retry) < len(reqs) {
		if more, err := b.TrieNodesBatch(ctx, at, retry); err == nil {
			for h, v := range more {
				out[h] = v
			}
		}
	}
	return out, nil
}

func keyNibbles(key []byte) []byte {
	out := make([]byte, len(key)*2)
	for i, c := range key {
		out[2*i], out[2*i+1] = c>>4, c&0x0f
	}
	return out
}

// compactPath hex-prefix encodes a nibble path (no terminator), the snap
// wire format for trie node paths.
func compactPath(nibbles []byte) []byte {
	flag := byte(0)
	if len(nibbles)%2 == 1 {
		flag = 1
	}
	var buf []byte
	if flag == 1 {
		buf = append(buf, 0x10|nibbles[0])
		nibbles = nibbles[1:]
	} else {
		buf = append(buf, 0)
	}
	for i := 0; i < len(nibbles); i += 2 {
		buf = append(buf, nibbles[i]<<4|nibbles[i+1])
	}
	return buf
}

// HeadersByHash implements verified.Backend (each header is self-verifying).
func (b *Backend) HeadersByHash(ctx context.Context, hashes []common.Hash) ([]*types.Header, error) {
	out := make([]*types.Header, len(hashes))
	errs := make([]error, len(hashes))
	var wg sync.WaitGroup
	for i, h := range hashes {
		wg.Add(1)
		go func(i int, h common.Hash) {
			defer wg.Done()
			out[i], errs[i] = b.Node.HeaderByHash(ctx, h)
		}(i, h)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// HeaderByHash fetches a header and checks it hashes to h.
func (n *Node) HeaderByHash(ctx context.Context, h common.Hash) (*types.Header, error) {
	hs, err := n.Headers(ctx, &eth.GetBlockHeadersRequest{Origin: eth.HashOrNumber{Hash: h}, Amount: 1}, func(hs []*types.Header) error {
		if hs[0].Hash() != h {
			return errors.New("header hash mismatch")
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("header %x: %w", h, err)
	}
	return hs[0], nil
}

// HeadersDescending fetches count headers ending at number from, newest
// first, checking that consecutive headers link by parent hash. The caller
// links the batch to a trusted header.
func (n *Node) HeadersDescending(ctx context.Context, from, count uint64) ([]*types.Header, error) {
	return n.Headers(ctx, &eth.GetBlockHeadersRequest{Origin: eth.HashOrNumber{Number: from}, Amount: count, Reverse: true}, func(hs []*types.Header) error {
		for i, h := range hs {
			if h.Number.Uint64() != from-uint64(i) {
				return errors.New("unexpected header number")
			}
			if i > 0 && hs[i-1].ParentHash != h.Hash() {
				return errors.New("headers do not link")
			}
		}
		return nil
	})
}

// Body fetches a block body and verifies it against header roots.
func (n *Node) Body(ctx context.Context, h *types.Header) (*types.Body, error) {
	var body *types.Body
	_, err := n.Bodies(ctx, []common.Hash{h.Hash()}, func(bs []*eth.BlockBody) error {
		b := bs[0]
		if types.DeriveSha(types.Transactions(b.Transactions), trie.NewStackTrie(nil)) != h.TxHash {
			return errors.New("transactions root mismatch")
		}
		if types.CalcUncleHash(b.Uncles) != h.UncleHash {
			return errors.New("uncle hash mismatch")
		}
		ws := b.Withdrawals
		if h.WithdrawalsHash != nil {
			if ws == nil {
				ws = []*types.Withdrawal{}
			}
			if types.DeriveSha(types.Withdrawals(ws), trie.NewStackTrie(nil)) != *h.WithdrawalsHash {
				return errors.New("withdrawals root mismatch")
			}
		}
		body = &types.Body{Transactions: b.Transactions, Uncles: b.Uncles, Withdrawals: ws}
		return nil
	})
	return body, err
}

// BlockReceipts fetches receipts for one block and verifies the root.
func (n *Node) BlockReceipts(ctx context.Context, h *types.Header) (types.Receipts, error) {
	var out types.Receipts
	_, err := n.Receipts(ctx, []common.Hash{h.Hash()}, func(rs []types.Receipts) error {
		if types.DeriveSha(rs[0], trie.NewStackTrie(nil)) != h.ReceiptHash {
			return errors.New("receipts root mismatch")
		}
		out = rs[0]
		return nil
	})
	return out, err
}

// ReceiptsBatch fetches receipts for several blocks, verifying every root.
// Peers cap responses (~2 MB), so partial answers are accepted as a verified
// prefix and the remainder is requested again.
func (n *Node) ReceiptsBatch(ctx context.Context, hs []*types.Header) ([]types.Receipts, error) {
	out := make([]types.Receipts, 0, len(hs))
	for attempt := 0; len(out) < len(hs); attempt++ {
		rest := hs[len(out):]
		hashes := make([]common.Hash, len(rest))
		for i, h := range rest {
			hashes[i] = h.Hash()
		}
		var got []types.Receipts
		_, err := n.Receipts(ctx, hashes, func(rs []types.Receipts) error {
			if len(rs) > len(rest) {
				return errors.New("too many receipt lists")
			}
			for i := range rs {
				if types.DeriveSha(rs[i], trie.NewStackTrie(nil)) != rest[i].ReceiptHash {
					return errors.New("receipts root mismatch")
				}
			}
			got = rs
			return nil
		})
		if err != nil {
			return nil, err
		}
		out = append(out, got...)
		if attempt > 4*len(hs) {
			return nil, errors.New("receipts batch not progressing")
		}
	}
	return out, nil
}

// BodiesBatch fetches bodies for several blocks, verifying every root and
// accepting partial (size-capped) responses as a verified prefix.
func (n *Node) BodiesBatch(ctx context.Context, hs []*types.Header) ([]*types.Body, error) {
	out := make([]*types.Body, 0, len(hs))
	for attempt := 0; len(out) < len(hs); attempt++ {
		rest := hs[len(out):]
		hashes := make([]common.Hash, len(rest))
		for i, h := range rest {
			hashes[i] = h.Hash()
		}
		var got []*types.Body
		_, err := n.Bodies(ctx, hashes, func(bs []*eth.BlockBody) error {
			if len(bs) > len(rest) {
				return errors.New("too many bodies")
			}
			got = make([]*types.Body, len(bs))
			for i, b := range bs {
				if types.DeriveSha(types.Transactions(b.Transactions), trie.NewStackTrie(nil)) != rest[i].TxHash {
					return errors.New("transactions root mismatch")
				}
				got[i] = &types.Body{Transactions: b.Transactions, Uncles: b.Uncles, Withdrawals: b.Withdrawals}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		out = append(out, got...)
		if attempt > 4*len(hs) {
			return nil, errors.New("bodies batch not progressing")
		}
	}
	return out, nil
}
