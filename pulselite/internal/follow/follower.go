package follow

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
	"github.com/pulsechainlite/pulselite/internal/chain"
	"github.com/pulsechainlite/pulselite/internal/metrics"
)

// History retrieves block data from the network. Implementations verify
// what they return: HeaderByHash by hash, HeadersDescending by parent
// linkage within the batch, Body and BlockReceipts against header roots.
// The devp2p node is the default implementation.
type History interface {
	HeaderByHash(ctx context.Context, h common.Hash) (*types.Header, error)
	HeadersDescending(ctx context.Context, from, count uint64) ([]*types.Header, error)
	Body(ctx context.Context, h *types.Header) (*types.Body, error)
	BlockReceipts(ctx context.Context, h *types.Header) (types.Receipts, error)
}

// Follower keeps the window populated.
type Follower struct {
	Beacon        *BeaconClient
	Chain         *chain.Chain
	History       History
	HeadersWindow uint64
	LogsWindow    uint64
	Interval      time.Duration
	// OnNetworkHead receives the estimated network head block number.
	OnNetworkHead func(n uint64)

	inflight sync.Map // block hash -> struct{} (receipt fetches)
}

// Run loops until ctx is done.
func (f *Follower) Run(ctx context.Context) {
	t := time.NewTicker(f.Interval)
	defer t.Stop()
	for {
		f.tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (f *Follower) tick(ctx context.Context) {
	cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if f.Beacon != nil {
		ref, err := f.Beacon.ExecutionPayload(cctx, "head")
		if err == nil {
			// The network head is estimated from the beacon node's own view:
			// its head plus the distance it reports to the wall-clock slot.
			// Slots can be empty, so slot distance overestimates block
			// distance; when synced the beacon head block is exact.
			if st, err := f.Beacon.Syncing(cctx); err == nil && f.OnNetworkHead != nil {
				if st.SyncDistance <= 2 {
					f.OnNetworkHead(ref.BlockNumber)
				} else {
					f.OnNetworkHead(ref.BlockNumber + st.SyncDistance)
				}
			}
			if f.Chain.Head() == nil {
				if err := f.adopt(cctx, ref.BlockHash); err == nil {
					f.Chain.SetHead(ref.BlockHash)
					log.Info("Bootstrapped head from beacon node", "number", ref.BlockNumber, "hash", ref.BlockHash)
				} else {
					log.Warn("Cannot bootstrap head", "err", err)
				}
			}
		} else {
			log.Debug("Beacon head unavailable", "err", err)
		}
		if ref, err := f.Beacon.ExecutionPayload(cctx, "finalized"); err == nil {
			if fin := f.Chain.Finalized(); fin == nil || fin.Number.Uint64() < ref.BlockNumber {
				if f.adopt(cctx, ref.BlockHash) == nil {
					f.Chain.SetFinalized(ref.BlockHash)
				}
			}
			if h, ok := f.Chain.CanonicalHash(ref.BlockNumber); ok && h != ref.BlockHash {
				log.Error("Canonical chain disagrees with beacon finalized block", "number", ref.BlockNumber, "local", h, "beacon", ref.BlockHash)
			}
		}
	}
	if err := f.backfillHeaders(cctx); err != nil {
		log.Debug("Header backfill incomplete", "err", err)
	}
	f.fillBodies(cctx, 64)
}

// adopt makes sure the header for hash is in the window (self-verified).
func (f *Follower) adopt(ctx context.Context, hash common.Hash) error {
	if f.Chain.Header(hash) != nil {
		return nil
	}
	h, err := f.History.HeaderByHash(ctx, hash)
	if err != nil {
		return err
	}
	if h.Hash() != hash {
		metrics.HeaderFail.Inc()
		return errors.New("header hash mismatch")
	}
	metrics.HeaderOK.Inc()
	f.Chain.AddHeader(h)
	return nil
}

// backfillHeaders extends the header chain below the lowest linked canonical
// header, accepting only batches that link to the already trusted child.
func (f *Follower) backfillHeaders(ctx context.Context) error {
	head := f.Chain.Head()
	if head == nil {
		return nil
	}
	target := uint64(0)
	if head.Number.Uint64() > f.HeadersWindow {
		target = head.Number.Uint64() - f.HeadersWindow
	}
	for rounds := 0; rounds < 20; rounds++ {
		low := f.Chain.LowestCanonical()
		child := f.Chain.HeaderByNumber(low)
		if child == nil || low <= target || low == 0 {
			return nil
		}
		from := low - 1
		count := min(uint64(512), from+1-target)
		hs, err := f.History.HeadersDescending(ctx, from, count)
		if err != nil {
			return err
		}
		if hs[0].Hash() != child.ParentHash {
			metrics.HeaderFail.Inc()
			if err := f.adopt(ctx, child.ParentHash); err != nil {
				return err
			}
			f.Chain.SetHead(head.Hash())
			continue
		}
		metrics.HeaderOK.Add(uint64(len(hs)))
		for _, h := range hs {
			f.Chain.AddHeader(h)
		}
		f.Chain.SetHead(head.Hash())
	}
	return nil
}

// fillBodies fetches missing bodies and receipts for the newest canonical
// blocks inside the logs window.
func (f *Follower) fillBodies(ctx context.Context, max int) {
	head := f.Chain.Head()
	if head == nil {
		return
	}
	low := f.Chain.LowestBodies()
	done := 0
	for n := head.Number.Uint64(); n >= low && done < max; n-- {
		hash, ok := f.Chain.CanonicalHash(n)
		if !ok {
			break
		}
		if f.Chain.Body(hash) == nil {
			if err := f.FetchBody(ctx, hash); err != nil {
				log.Debug("Body backfill failed", "number", n, "err", err)
				return
			}
			done++
		}
		if !f.Chain.HasReceipts(hash) {
			if _, err := f.FetchReceipts(ctx, hash); err != nil {
				log.Debug("Receipt backfill failed", "number", n, "err", err)
				return
			}
			done++
		}
		if n == 0 {
			break
		}
	}
}

// FetchBody downloads a block body (verified against the header's
// transactions and withdrawals roots) into the window.
func (f *Follower) FetchBody(ctx context.Context, hash common.Hash) error {
	h := f.Chain.Header(hash)
	if h == nil {
		return errors.New("unknown header")
	}
	body, err := f.History.Body(ctx, h)
	if err != nil {
		metrics.HeaderFail.Inc()
		return err
	}
	f.Chain.AddBlock(types.NewBlockWithHeader(h).WithBody(*body))
	return nil
}

// FetchReceipts downloads and verifies the receipts of block hash.
func (f *Follower) FetchReceipts(ctx context.Context, hash common.Hash) (types.Receipts, error) {
	h := f.Chain.Header(hash)
	if h == nil {
		return nil, errors.New("unknown header")
	}
	if f.Chain.Body(hash) == nil {
		if err := f.FetchBody(ctx, hash); err != nil {
			return nil, err
		}
	}
	receipts, err := f.History.BlockReceipts(ctx, h)
	if err != nil {
		metrics.ReceiptFail.Inc()
		return nil, err
	}
	metrics.ReceiptOK.Inc()
	if err := f.Chain.SetReceipts(hash, receipts); err != nil {
		return nil, err
	}
	return f.Chain.Receipts(hash), nil
}

// FetchReceiptsAsync fetches receipts for a new block with retries (peers
// may not have imported it yet).
func (f *Follower) FetchReceiptsAsync(hash common.Hash) {
	if _, loaded := f.inflight.LoadOrStore(hash, struct{}{}); loaded {
		return
	}
	go func() {
		defer f.inflight.Delete(hash)
		for attempt := 0; attempt < 8; attempt++ {
			time.Sleep(time.Duration(2+attempt*2) * time.Second)
			if f.Chain.HasReceipts(hash) {
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			_, err := f.FetchReceipts(ctx, hash)
			cancel()
			if err == nil {
				return
			}
		}
	}()
}
