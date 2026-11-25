// Package deposits rebuilds the deposit-contract log history the consensus
// client needs (eth1 data voting / deposit inclusion in proposals) without
// any RPC provider and without storing event logs in general.
//
// Headers are walked from the consensus-finalized block down to the deposit
// contract deployment block; each batch must link by parent hash to the
// already verified header above it, so every header is authenticated back to
// a consensus-trusted anchor. Only blocks whose logs bloom matches the deposit
// contract and event topic have their receipts and bodies fetched (verified
// against the header roots), and only the deposit logs are kept.
package deposits

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/log"
	"github.com/pulsechainlite/pulselite/internal/chain"
	"github.com/pulsechainlite/pulselite/internal/metrics"
)

// PulseChain deposit contract (prysm-pulse config) and its deployment block.
var (
	Contract    = common.HexToAddress("0x3693693693693693693693693693693693693693")
	Topic       = crypto.Keccak256Hash([]byte("DepositEvent(bytes,bytes,bytes,bytes,bytes)"))
	DeployBlock = uint64(17_233_000)
)

// Source retrieves (and verifies) block data.
type Source interface {
	HeadersDescending(ctx context.Context, from, count uint64) ([]*types.Header, error)
	ReceiptsBatch(ctx context.Context, hs []*types.Header) ([]types.Receipts, error)
	BodiesBatch(ctx context.Context, hs []*types.Header) ([]*types.Body, error)
}

// ErrIncomplete is returned for ranges not scanned yet.
var ErrIncomplete = errors.New("deposit history scan in progress")

type meta struct {
	Top       uint64      // highest block covered (finalized at the time)
	TopHash   common.Hash // its hash
	Low       uint64      // lowest block covered
	LowParent common.Hash // parent hash of Low (next header to link)
	Done      bool        // reached the deployment block
	Logs      uint64
	Matched   uint64 // bloom-matching blocks processed
}

var keyMeta = []byte("meta")

// Scanner maintains the deposit log history.
type Scanner struct {
	src      Source
	chain    *chain.Chain
	db       ethdb.KeyValueStore
	Batch    uint64
	Parallel int
	// Busy, if set, pauses the historical scan while it returns true (block
	// validation has priority on the shared peers).
	Busy func() bool

	mu   sync.RWMutex
	meta meta
}

// New creates a scanner over db.
func New(src Source, ch *chain.Chain, db ethdb.KeyValueStore) *Scanner {
	s := &Scanner{src: src, chain: ch, db: db, Batch: 512, Parallel: 3}
	if v, err := db.Get(keyMeta); err == nil {
		json.Unmarshal(v, &s.meta)
	}
	metrics.DepositLogs.Set(float64(s.meta.Logs))
	return s
}

func (s *Scanner) saveMeta() {
	s.mu.RLock()
	v, _ := json.Marshal(s.meta)
	s.mu.RUnlock()
	s.db.Put(keyMeta, v)
}

func logKey(number uint64, index uint) []byte {
	k := make([]byte, 13)
	k[0] = 'l'
	binary.BigEndian.PutUint64(k[1:9], number)
	binary.BigEndian.PutUint32(k[9:13], uint32(index))
	return k
}

// Status reports coverage.
func (s *Scanner) Status() (low, top uint64, done bool, logs uint64) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.meta.Low, s.meta.Top, s.meta.Done, s.meta.Logs
}

func (s *Scanner) progress() {
	s.mu.RLock()
	m := s.meta
	s.mu.RUnlock()
	if m.Top > DeployBlock {
		metrics.DepositScan.Set(float64(m.Top-max(m.Low, DeployBlock)) / float64(m.Top-DeployBlock))
	}
	if m.Done {
		metrics.DepositScan.Set(1)
	}
	metrics.DepositLogs.Set(float64(m.Logs))
}

// Run scans backwards to the deployment block while a separate loop keeps
// extending the covered range up to the finalized block, so recent ranges
// are served long before the historical scan completes.
func (s *Scanner) Run(ctx context.Context) {
	go s.loop(ctx, true)
	s.loop(ctx, false)
}

func (s *Scanner) loop(ctx context.Context, forward bool) {
	for {
		if fin := s.chain.Finalized(); fin != nil {
			var err error
			if forward {
				err = s.extend(ctx, fin)
			} else {
				err = s.step(ctx, fin)
			}
			if err != nil && ctx.Err() == nil {
				log.Warn("Deposit scan step failed (will retry)", "forward", forward, "err", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(15 * time.Second):
		}
	}
}

func (s *Scanner) init(fin *types.Header) meta {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.meta.Top == 0 {
		s.meta = meta{Top: fin.Number.Uint64(), TopHash: fin.Hash(), Low: fin.Number.Uint64() + 1, LowParent: fin.Hash()}
		log.Info("Starting deposit history scan", "from", fin.Number, "to", DeployBlock)
	}
	return s.meta
}

// extend covers blocks between the previous top and the new finalized block.
func (s *Scanner) extend(ctx context.Context, fin *types.Header) error {
	m := s.init(fin)
	if fin.Number.Uint64() <= m.Top {
		return nil
	}
	if err := s.walk(ctx, fin.Number.Uint64(), fin.Hash(), m.Top+1, nil); err != nil {
		return err
	}
	s.mu.Lock()
	if fin.Number.Uint64() > s.meta.Top {
		s.meta.Top, s.meta.TopHash = fin.Number.Uint64(), fin.Hash()
	}
	s.mu.Unlock()
	s.saveMeta()
	return nil
}

// step continues the historical scan downwards.
func (s *Scanner) step(ctx context.Context, fin *types.Header) error {
	m := s.init(fin)
	if !m.Done && m.Low > DeployBlock {
		start := time.Now()
		err := s.walk(ctx, m.Low-1, m.LowParent, DeployBlock, func(low uint64, lowParent common.Hash) {
			s.mu.Lock()
			s.meta.Low, s.meta.LowParent = low, lowParent
			s.mu.Unlock()
			s.saveMeta()
			s.progress()
		})
		if err != nil {
			return err
		}
		s.mu.Lock()
		s.meta.Done = true
		s.meta.Low = DeployBlock
		s.mu.Unlock()
		s.saveMeta()
		log.Info("Deposit history scan complete", "logs", s.meta.Logs, "matchedBlocks", s.meta.Matched, "elapsed", time.Since(start).Round(time.Second))
	}
	s.progress()
	return nil
}

type batchResult struct {
	hs  []*types.Header
	err error
}

// walk fetches headers from `from` (whose hash must be fromHash) down to
// `stop`, verifying linkage, processing deposit candidates, and reporting
// the verified low watermark after each batch.
func (s *Scanner) walk(ctx context.Context, from uint64, fromHash common.Hash, stop uint64, onBatch func(low uint64, lowParent common.Hash)) error {
	expect := fromHash
	next := from
	lastLog := time.Now()
	for next >= stop && next > 0 {
		for onBatch != nil && s.Busy != nil && s.Busy() {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(250 * time.Millisecond):
			}
		}
		// Fetch up to Parallel consecutive batches concurrently.
		var starts []uint64
		for i, at := 0, next; i < s.Parallel && at >= stop && at > 0; i++ {
			starts = append(starts, at)
			cnt := min(s.Batch, at-stop+1)
			if at < cnt {
				break
			}
			at -= cnt
		}
		results := make([]batchResult, len(starts))
		var wg sync.WaitGroup
		for i, st := range starts {
			wg.Add(1)
			go func(i int, st uint64) {
				defer wg.Done()
				cnt := min(s.Batch, st-stop+1)
				for attempt := 0; attempt < 4; attempt++ {
					hs, err := s.src.HeadersDescending(ctx, st, cnt)
					if err == nil && uint64(len(hs)) == cnt {
						results[i] = batchResult{hs: hs}
						return
					}
					if err == nil {
						err = errors.New("short header batch")
					}
					results[i] = batchResult{err: err}
				}
			}(i, st)
		}
		wg.Wait()
		for _, r := range results {
			if r.err != nil {
				return r.err
			}
			// Every header must link to the verified one above it; the source
			// is not trusted to have checked this.
			for i, h := range r.hs {
				want := expect
				if i > 0 {
					want = r.hs[i-1].ParentHash
					if h.Number.Uint64()+1 != r.hs[i-1].Number.Uint64() {
						return fmt.Errorf("header #%d out of sequence", h.Number)
					}
				}
				if h.Hash() != want {
					metrics.HeaderFail.Inc()
					return fmt.Errorf("header #%d does not link to verified child", h.Number)
				}
			}
			metrics.HeaderOK.Add(uint64(len(r.hs)))
			if err := s.process(ctx, r.hs); err != nil {
				return err
			}
			last := r.hs[len(r.hs)-1]
			expect = last.ParentHash
			next = last.Number.Uint64() - 1
			if onBatch != nil {
				onBatch(last.Number.Uint64(), last.ParentHash)
			}
			if last.Number.Uint64() <= stop {
				return nil
			}
		}
		if time.Since(lastLog) > 30*time.Second {
			lastLog = time.Now()
			low, top, _, logs := s.Status()
			log.Info("Deposit history scan", "at", next, "low", low, "top", top, "logs", logs)
		}
	}
	return nil
}

// process fetches receipts/bodies for bloom candidates and stores their
// deposit logs.
func (s *Scanner) process(ctx context.Context, hs []*types.Header) error {
	var cands []*types.Header
	for _, h := range hs {
		if types.BloomLookup(h.Bloom, Contract) && types.BloomLookup(h.Bloom, Topic) {
			cands = append(cands, h)
		}
	}
	// PulseChain receipts are large; fetch one block per request, spread over
	// peers in parallel.
	for i := 0; i < len(cands); i += 16 {
		chunk := cands[i:min(i+16, len(cands))]
		receipts := make([]types.Receipts, len(chunk))
		bodies := make([]*types.Body, len(chunk))
		errs := make([]error, len(chunk))
		var wg sync.WaitGroup
		for j, h := range chunk {
			wg.Add(1)
			go func(j int, h *types.Header) {
				defer wg.Done()
				for attempt := 0; attempt < 3; attempt++ {
					rs, err := s.src.ReceiptsBatch(ctx, []*types.Header{h})
					if err == nil {
						var bs []*types.Body
						if bs, err = s.src.BodiesBatch(ctx, []*types.Header{h}); err == nil {
							receipts[j], bodies[j], errs[j] = rs[0], bs[0], nil
							return
						}
					}
					errs[j] = err
				}
			}(j, h)
		}
		wg.Wait()
		for _, err := range errs {
			if err != nil {
				return fmt.Errorf("deposit receipts/bodies: %w", err)
			}
		}
		batch := s.db.NewBatch()
		found := uint64(0)
		for b, h := range chunk {
			index := uint(0)
			for t, r := range receipts[b] {
				for _, l := range r.Logs {
					if l.Address == Contract && len(l.Topics) > 0 && l.Topics[0] == Topic {
						full := *l
						full.BlockNumber = h.Number.Uint64()
						full.BlockHash = h.Hash()
						full.TxIndex = uint(t)
						full.Index = index
						if t < len(bodies[b].Transactions) {
							full.TxHash = bodies[b].Transactions[t].Hash()
						}
						enc, _ := json.Marshal(&full)
						batch.Put(logKey(full.BlockNumber, index), enc)
						found++
					}
					index++
				}
			}
		}
		if err := batch.Write(); err != nil {
			return err
		}
		s.mu.Lock()
		s.meta.Logs += found
		s.meta.Matched += uint64(len(chunk))
		s.mu.Unlock()
	}
	return nil
}

// Covers reports whether [from, to] lies inside the scanned range.
func (s *Scanner) Covers(from, to uint64) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if from < DeployBlock {
		from = DeployBlock
	}
	low := s.meta.Low
	if s.meta.Done {
		low = DeployBlock
	}
	return s.meta.Top != 0 && from >= low && to <= s.meta.Top
}

// Query returns deposit logs in [from, to] matching topics.
func (s *Scanner) Query(from, to uint64, topics [][]common.Hash) ([]*types.Log, error) {
	if !s.Covers(from, to) {
		low, top, done, _ := s.Status()
		return nil, fmt.Errorf("%w: have blocks %d..%d (done=%v), requested %d..%d", ErrIncomplete, low, top, done, from, to)
	}
	it := s.db.NewIterator([]byte("l"), logKey(from, 0)[1:])
	defer it.Release()
	out := []*types.Log{}
	for it.Next() {
		n := binary.BigEndian.Uint64(it.Key()[1:9])
		if n > to {
			break
		}
		l := new(types.Log)
		if err := json.Unmarshal(it.Value(), l); err != nil {
			continue
		}
		if matchTopics(l, topics) {
			out = append(out, l)
		}
	}
	return out, nil
}

func matchTopics(l *types.Log, topics [][]common.Hash) bool {
	if len(topics) > len(l.Topics) {
		return false
	}
	for i, alts := range topics {
		if len(alts) == 0 {
			continue
		}
		ok := false
		for _, t := range alts {
			if l.Topics[i] == t {
				ok = true
			}
		}
		if !ok {
			return false
		}
	}
	return true
}
