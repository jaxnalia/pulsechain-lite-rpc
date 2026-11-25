// Package chain keeps the small recent-block window: headers (for BLOCKHASH,
// eth1 follow-distance lookups and RPC), bodies and receipts (for
// eth_getLogs / receipts), the canonical index and forkchoice pointers.
//
// State is never stored per block: trie nodes live in the content-addressed
// cache, so a reorg only needs to rewind the canonical index and transaction
// lookups kept here.
package chain

import (
	"errors"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/ethdb/memorydb"
	"github.com/ethereum/go-ethereum/ethdb/pebble"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/pulsechainlite/pulselite/internal/metrics"
)

var (
	prefixHeader   = []byte("h")
	prefixBody     = []byte("b")
	prefixReceipts = []byte("r")
	prefixValid    = []byte("v")
	keyHead        = []byte("H")
	keySafe        = []byte("S")
	keyFinal       = []byte("F")
)

// TxLoc locates a canonical transaction.
type TxLoc struct {
	BlockHash common.Hash
	Number    uint64
	Index     int
}

// Windows configures retention.
type Windows struct {
	Headers uint64 // headers retained behind head
	Logs    uint64 // bodies + receipts retained behind head
	Reorg   uint64 // non-canonical headers retained behind head
}

// Chain is the recent block window.
type Chain struct {
	cfg *params.ChainConfig
	db  ethdb.KeyValueStore
	win Windows

	mu      sync.RWMutex
	headers map[common.Hash]*types.Header
	byNum   map[uint64]map[common.Hash]struct{}
	canon   map[uint64]common.Hash
	valid   map[common.Hash]bool
	txs     map[common.Hash]TxLoc
	head    common.Hash
	safe    common.Hash
	final   common.Hash
	lowest  uint64 // lowest canonical number in the index

	bodyPruned uint64 // bodies/receipts below this number have been deleted
}

// Open opens the chain window at dir. Pass dir == "" for an in-memory store.
func Open(dir string, cfg *params.ChainConfig, win Windows) (*Chain, error) {
	var db ethdb.KeyValueStore
	if dir == "" {
		db = memorydb.New()
	} else {
		pdb, err := pebble.New(dir, 64, 64, "", false)
		if err != nil {
			return nil, err
		}
		db = pdb
	}
	c := &Chain{
		cfg:     cfg,
		db:      db,
		win:     win,
		headers: make(map[common.Hash]*types.Header),
		byNum:   make(map[uint64]map[common.Hash]struct{}),
		canon:   make(map[uint64]common.Hash),
		valid:   make(map[common.Hash]bool),
		txs:     make(map[common.Hash]TxLoc),
	}
	if err := c.load(); err != nil {
		db.Close()
		return nil, err
	}
	return c, nil
}

func (c *Chain) load() error {
	it := c.db.NewIterator(prefixHeader, nil)
	for it.Next() {
		h := new(types.Header)
		if err := rlp.DecodeBytes(it.Value(), h); err != nil {
			continue
		}
		c.insertHeaderLocked(h)
	}
	it.Release()
	it = c.db.NewIterator(prefixValid, nil)
	for it.Next() {
		c.valid[common.BytesToHash(it.Key()[1:])] = true
	}
	it.Release()
	if v, err := c.db.Get(keySafe); err == nil {
		c.safe = common.BytesToHash(v)
	}
	if v, err := c.db.Get(keyFinal); err == nil {
		c.final = common.BytesToHash(v)
	}
	if v, err := c.db.Get(keyHead); err == nil {
		if h := c.headers[common.BytesToHash(v)]; h != nil {
			c.setHeadLocked(h)
		}
	}
	log.Info("Loaded block window", "headers", len(c.headers), "head", c.headNumberLocked())
	return nil
}

// Close closes the underlying store.
func (c *Chain) Close() error { return c.db.Close() }

// Config returns the chain configuration.
func (c *Chain) Config() *params.ChainConfig { return c.cfg }

func numKey(prefix []byte, hash common.Hash) []byte {
	return append(append([]byte{}, prefix...), hash.Bytes()...)
}

func (c *Chain) insertHeaderLocked(h *types.Header) {
	hash := h.Hash()
	c.headers[hash] = h
	n := h.Number.Uint64()
	if c.byNum[n] == nil {
		c.byNum[n] = make(map[common.Hash]struct{})
	}
	c.byNum[n][hash] = struct{}{}
}

// AddHeader stores a header. Callers must have authenticated it (hash link to
// a consensus-provided block or requested-by-hash).
func (c *Chain) AddHeader(h *types.Header) {
	hash := h.Hash()
	c.mu.Lock()
	if _, ok := c.headers[hash]; ok {
		c.mu.Unlock()
		return
	}
	c.insertHeaderLocked(h)
	c.mu.Unlock()
	if enc, err := rlp.EncodeToBytes(h); err == nil {
		c.db.Put(numKey(prefixHeader, hash), enc)
	}
}

// AddBlock stores a header and its body.
func (c *Chain) AddBlock(b *types.Block) {
	c.AddHeader(b.Header())
	body := b.Body()
	if enc, err := rlp.EncodeToBytes(body); err == nil {
		c.db.Put(numKey(prefixBody, b.Hash()), enc)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.canon[b.NumberU64()] == b.Hash() {
		c.indexTxsLocked(b.Hash(), b.NumberU64(), body)
	}
}

// Header returns a header by hash.
func (c *Chain) Header(hash common.Hash) *types.Header {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.headers[hash]
}

// HeaderByNumber returns the canonical header at number.
func (c *Chain) HeaderByNumber(n uint64) *types.Header {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if h, ok := c.canon[n]; ok {
		return c.headers[h]
	}
	return nil
}

// CanonicalHash returns the canonical hash at number.
func (c *Chain) CanonicalHash(n uint64) (common.Hash, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	h, ok := c.canon[n]
	return h, ok
}

// IsCanonical reports whether hash is on the canonical chain.
func (c *Chain) IsCanonical(hash common.Hash) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	h := c.headers[hash]
	return h != nil && c.canon[h.Number.Uint64()] == hash
}

// Body returns a stored block body.
func (c *Chain) Body(hash common.Hash) *types.Body {
	enc, err := c.db.Get(numKey(prefixBody, hash))
	if err != nil || len(enc) == 0 {
		return nil
	}
	body := new(types.Body)
	if err := rlp.DecodeBytes(enc, body); err != nil {
		return nil
	}
	return body
}

// Block returns header+body if both are present.
func (c *Chain) Block(hash common.Hash) *types.Block {
	h := c.Header(hash)
	if h == nil {
		return nil
	}
	body := c.Body(hash)
	if body == nil {
		return nil
	}
	return types.NewBlockWithHeader(h).WithBody(*body)
}

// SetReceipts stores receipts for a block (already verified against the
// header's receipt root by the caller).
func (c *Chain) SetReceipts(hash common.Hash, receipts types.Receipts) error {
	stored := make([]*types.ReceiptForStorage, len(receipts))
	for i, r := range receipts {
		stored[i] = (*types.ReceiptForStorage)(r)
	}
	enc, err := rlp.EncodeToBytes(stored)
	if err != nil {
		return err
	}
	return c.db.Put(numKey(prefixReceipts, hash), enc)
}

// HasReceipts reports whether receipts are stored for hash.
func (c *Chain) HasReceipts(hash common.Hash) bool {
	ok, _ := c.db.Has(numKey(prefixReceipts, hash))
	return ok
}

// Receipts returns receipts with all derived fields populated.
func (c *Chain) Receipts(hash common.Hash) types.Receipts {
	enc, err := c.db.Get(numKey(prefixReceipts, hash))
	if err != nil || len(enc) == 0 {
		return nil
	}
	var stored []*types.ReceiptForStorage
	if err := rlp.DecodeBytes(enc, &stored); err != nil {
		return nil
	}
	h := c.Header(hash)
	body := c.Body(hash)
	if h == nil || body == nil {
		return nil
	}
	receipts := make(types.Receipts, len(stored))
	for i, r := range stored {
		receipts[i] = (*types.Receipt)(r)
	}
	if err := receipts.DeriveFields(c.cfg, hash, h.Number.Uint64(), h.Time, h.BaseFee, nil, body.Transactions); err != nil {
		return nil
	}
	return receipts
}

// MarkValid records that a block was validated by local execution.
func (c *Chain) MarkValid(hash common.Hash) {
	c.mu.Lock()
	c.valid[hash] = true
	c.mu.Unlock()
	c.db.Put(numKey(prefixValid, hash), []byte{1})
	if h := c.Header(hash); h != nil && float64(h.Number.Uint64()) > metrics.ValidatedNumber.Value() {
		metrics.ValidatedNumber.Set(float64(h.Number.Uint64()))
	}
}

// IsValid reports whether the block was validated locally. A block is also
// considered valid if a validated descendant exists (ancestors of a valid
// block are valid by construction).
func (c *Chain) IsValid(hash common.Hash) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.valid[hash]
}

// Head returns the canonical head header (or nil).
func (c *Chain) Head() *types.Header {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.headers[c.head]
}

// Latest returns the canonical head, or before any forkchoice update the
// highest-numbered header received from the consensus client.
func (c *Chain) Latest() *types.Header {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if h := c.headers[c.head]; h != nil {
		return h
	}
	var best *types.Header
	for _, h := range c.headers {
		if best == nil || h.Number.Cmp(best.Number) > 0 {
			best = h
		}
	}
	return best
}

func (c *Chain) headNumberLocked() uint64 {
	if h := c.headers[c.head]; h != nil {
		return h.Number.Uint64()
	}
	return 0
}

// Safe returns the safe header (or nil).
func (c *Chain) Safe() *types.Header {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.headers[c.safe]
}

// Finalized returns the finalized header (or nil).
func (c *Chain) Finalized() *types.Header {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.headers[c.final]
}

// SetSafe updates the safe pointer.
func (c *Chain) SetSafe(hash common.Hash) {
	if hash == (common.Hash{}) {
		return
	}
	c.mu.Lock()
	c.safe = hash
	c.mu.Unlock()
	c.db.Put(keySafe, hash.Bytes())
}

// SetFinalized updates the finalized pointer.
func (c *Chain) SetFinalized(hash common.Hash) {
	if hash == (common.Hash{}) {
		return
	}
	c.mu.Lock()
	c.final = hash
	c.mu.Unlock()
	c.db.Put(keyFinal, hash.Bytes())
}

// ErrUnknownHead is returned when the requested head header is not known.
var ErrUnknownHead = errors.New("unknown head header")

// ReorgResult describes the effect of a head update.
type ReorgResult struct {
	Dropped   int // canonical blocks removed from the old chain
	Added     int // canonical blocks added from the new chain
	Ancestor  uint64
	IsReorg   bool
	OldHead   uint64
	NewHead   uint64
	GapBottom uint64 // lowest number linked; headers below are missing (0 if fully linked)
}

// SetHead makes hash the canonical head, rewinding the canonical index and
// transaction lookups on reorgs.
// Database writes never happen under c.mu: a stalled write (e.g. during
// compaction) must not block readers such as RPC handlers.
func (c *Chain) SetHead(hash common.Hash) (ReorgResult, error) {
	c.mu.Lock()
	h := c.headers[hash]
	if h == nil {
		c.mu.Unlock()
		return ReorgResult{}, ErrUnknownHead
	}
	res := c.setHeadLocked(h)
	dels := c.pruneLocked()
	c.mu.Unlock()

	batch := c.db.NewBatch()
	batch.Put(keyHead, hash.Bytes())
	for _, k := range dels {
		batch.Delete(k)
	}
	if err := batch.Write(); err != nil {
		log.Warn("Block window write failed", "err", err)
	}
	if res.IsReorg {
		metrics.Reorgs.Inc()
		log.Warn("Chain reorg", "oldHead", res.OldHead, "newHead", res.NewHead, "ancestor", res.Ancestor, "dropped", res.Dropped, "added", res.Added)
	}
	metrics.HeadNumber.Set(float64(res.NewHead))
	return res, nil
}

func (c *Chain) setHeadLocked(h *types.Header) ReorgResult {
	oldHead := c.headNumberLocked()
	res := ReorgResult{OldHead: oldHead, NewHead: h.Number.Uint64()}

	// Walk back from the new head until we meet the canonical chain or a gap.
	var path []*types.Header
	cur := h
	for {
		n := cur.Number.Uint64()
		if ch, ok := c.canon[n]; ok && ch == cur.Hash() {
			res.Ancestor = n
			break
		}
		path = append(path, cur)
		parent := c.headers[cur.ParentHash]
		if parent == nil {
			if n > 0 {
				res.GapBottom = n
				res.Ancestor = n - 1
			}
			break
		}
		cur = parent
	}
	// Truncate the old canonical chain above the ancestor.
	if c.head != (common.Hash{}) {
		for n := res.Ancestor + 1; n <= oldHead; n++ {
			if old, ok := c.canon[n]; ok {
				if n > res.NewHead || c.headerOnPath(path, n) != old {
					res.Dropped++
				}
				c.unindexTxsLocked(old)
				delete(c.canon, n)
			}
		}
	}
	// If we hit a gap, anything canonical below the gap is no longer linked
	// to the head; drop it so stale entries are never served as canonical.
	if res.GapBottom > 0 {
		for n := range c.canon {
			if n < res.GapBottom {
				c.unindexTxsLocked(c.canon[n])
				delete(c.canon, n)
			}
		}
	}
	for i := len(path) - 1; i >= 0; i-- {
		hh := path[i]
		hash := hh.Hash()
		c.canon[hh.Number.Uint64()] = hash
		res.Added++
		if body := c.bodyLocked(hash); body != nil {
			c.indexTxsLocked(hash, hh.Number.Uint64(), body)
		}
	}
	res.IsReorg = res.Dropped > 0
	c.head = h.Hash()
	c.recomputeLowestLocked()
	c.extendDownLocked()
	return res
}

func (c *Chain) headerOnPath(path []*types.Header, n uint64) common.Hash {
	for _, h := range path {
		if h.Number.Uint64() == n {
			return h.Hash()
		}
	}
	return common.Hash{}
}

func (c *Chain) bodyLocked(hash common.Hash) *types.Body {
	enc, err := c.db.Get(numKey(prefixBody, hash))
	if err != nil || len(enc) == 0 {
		return nil
	}
	body := new(types.Body)
	if rlp.DecodeBytes(enc, body) != nil {
		return nil
	}
	return body
}

func (c *Chain) indexTxsLocked(hash common.Hash, number uint64, body *types.Body) {
	for i, tx := range body.Transactions {
		c.txs[tx.Hash()] = TxLoc{BlockHash: hash, Number: number, Index: i}
	}
}

func (c *Chain) unindexTxsLocked(hash common.Hash) {
	body := c.bodyLocked(hash)
	if body == nil {
		return
	}
	for _, tx := range body.Transactions {
		if loc, ok := c.txs[tx.Hash()]; ok && loc.BlockHash == hash {
			delete(c.txs, tx.Hash())
		}
	}
}

// TxLookup returns the canonical location of a transaction.
func (c *Chain) TxLookup(txHash common.Hash) (TxLoc, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	loc, ok := c.txs[txHash]
	return loc, ok
}

// LowestCanonical returns the lowest canonical number linked to head.
func (c *Chain) LowestCanonical() uint64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.lowest
}

// LowestBodies returns the lowest number for which bodies/receipts are kept.
func (c *Chain) LowestBodies() uint64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	head := c.headNumberLocked()
	low := uint64(0)
	if head > c.win.Logs {
		low = head - c.win.Logs
	}
	if low < c.lowest {
		low = c.lowest
	}
	return low
}

// pruneLocked updates the in-memory window and returns the keys to delete;
// the caller deletes them after releasing the lock. Bodies and receipts are
// pruned once per block using a watermark so already-pruned blocks never
// generate repeated tombstones.
func (c *Chain) pruneLocked() [][]byte {
	head := c.headNumberLocked()
	var dels [][]byte
	bodyLimit := uint64(0)
	if head > c.win.Logs {
		bodyLimit = head - c.win.Logs
	}
	for n, set := range c.byNum {
		age := head - min(n, head)
		for hash := range set {
			canonical := c.canon[n] == hash
			switch {
			case age > c.win.Headers || (!canonical && age > c.win.Reorg && n < head):
				dels = append(dels, c.deleteBlockLocked(hash)...)
				delete(set, hash)
				if canonical {
					delete(c.canon, n)
				}
			case n < bodyLimit && n >= c.bodyPruned:
				dels = append(dels, numKey(prefixBody, hash), numKey(prefixReceipts, hash))
			}
		}
		if len(set) == 0 {
			delete(c.byNum, n)
		}
	}
	if bodyLimit > c.bodyPruned {
		c.bodyPruned = bodyLimit
	}
	// Drop tx lookups for blocks whose bodies are gone.
	for txh, loc := range c.txs {
		if head-min(loc.Number, head) > c.win.Logs {
			delete(c.txs, txh)
		}
	}
	c.recomputeLowestLocked()
	return dels
}

// extendDownLocked links stored ancestors below the lowest canonical header
// (e.g. after a header backfill) into the canonical index.
func (c *Chain) extendDownLocked() {
	cur := c.headers[c.canon[c.lowest]]
	for cur != nil && cur.Number.Uint64() > 0 {
		parent := c.headers[cur.ParentHash]
		if parent == nil {
			break
		}
		n := parent.Number.Uint64()
		if c.canon[n] != parent.Hash() {
			if old, ok := c.canon[n]; ok {
				c.unindexTxsLocked(old)
			}
			c.canon[n] = parent.Hash()
			if body := c.bodyLocked(parent.Hash()); body != nil {
				c.indexTxsLocked(parent.Hash(), n, body)
			}
		}
		c.lowest = n
		cur = parent
	}
}

func (c *Chain) recomputeLowestLocked() {
	c.lowest = c.headNumberLocked()
	for n := range c.canon {
		if n < c.lowest {
			c.lowest = n
		}
	}
}

func (c *Chain) deleteBlockLocked(hash common.Hash) [][]byte {
	delete(c.headers, hash)
	keys := [][]byte{numKey(prefixHeader, hash)}
	if c.valid[hash] {
		keys = append(keys, numKey(prefixValid, hash))
	}
	delete(c.valid, hash)
	return keys
}

// Stats returns sizes for logging/metrics.
func (c *Chain) Stats() (headers, canonical, txs int) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.headers), len(c.canon), len(c.txs)
}
