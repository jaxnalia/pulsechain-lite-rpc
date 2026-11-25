// Package exec executes blocks and calls on proof-verified partial state.
package exec

import (
	"context"
	"errors"
	"fmt"
	gomath "math"
	"math/big"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/consensus/beacon"
	"github.com/ethereum/go-ethereum/consensus/ethash"
	"github.com/ethereum/go-ethereum/consensus/misc/eip1559"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/trie"
	"github.com/holiman/uint256"
	"github.com/pulsechainlite/pulselite/internal/chain"
	"github.com/pulsechainlite/pulselite/internal/metrics"
	"github.com/pulsechainlite/pulselite/internal/verified"
)

// Executor runs blocks, payload builds and calls on verified state.
type Executor struct {
	cfg       *params.ChainConfig
	engine    consensus.Engine
	src       *verified.Source
	chain     *chain.Chain
	MaxRounds int
	// PrefetchParallel bounds concurrent per-transaction prefetch runs
	// (0 disables the prefetch phase).
	PrefetchParallel int
}

// New creates an executor.
func New(cfg *params.ChainConfig, src *verified.Source, ch *chain.Chain, maxRounds int) *Executor {
	return &Executor{cfg: cfg, engine: beacon.New(ethash.NewFaker()), src: src, chain: ch, MaxRounds: maxRounds, PrefetchParallel: 64}
}

// Config returns the chain config.
func (e *Executor) Config() *params.ChainConfig { return e.cfg }

// Source returns the verified state source.
func (e *Executor) Source() *verified.Source { return e.src }

// ChainContext implements core.ChainContext and consensus.ChainHeaderReader
// over the block window, recording missing ancestor headers as misses.
type ChainContext struct {
	e *Executor
	r *verified.Reader
}

// NewChainContext returns a chain context bound to reader r (may be nil).
func (e *Executor) NewChainContext(r *verified.Reader) *ChainContext {
	return &ChainContext{e: e, r: r}
}

func (c *ChainContext) Engine() consensus.Engine    { return c.e.engine }
func (c *ChainContext) Config() *params.ChainConfig { return c.e.cfg }
func (c *ChainContext) CurrentHeader() *types.Header {
	return c.e.chain.Head()
}
func (c *ChainContext) GetHeaderByNumber(n uint64) *types.Header {
	return c.e.chain.HeaderByNumber(n)
}
func (c *ChainContext) GetHeaderByHash(h common.Hash) *types.Header {
	return c.e.chain.Header(h)
}
func (c *ChainContext) GetHeader(h common.Hash, n uint64) *types.Header {
	if hdr := c.e.chain.Header(h); hdr != nil {
		return hdr
	}
	if c.r != nil {
		return c.r.ResolveHeader(h, n)
	}
	return nil
}

// Result is the outcome of executing a block.
type Result struct {
	Root        common.Hash
	ReceiptRoot common.Hash
	Receipts    types.Receipts
	GasUsed     uint64
	Bloom       types.Bloom
	State       *state.StateDB
	Stats       verified.RunStats
	Duration    time.Duration
}

// InvalidBlockError marks a definitive execution failure on complete data.
type InvalidBlockError struct{ Err error }

func (e *InvalidBlockError) Error() string { return "invalid block: " + e.Err.Error() }
func (e *InvalidBlockError) Unwrap() error { return e.Err }

var discoveryBalance = new(uint256.Int).Lsh(uint256.NewInt(1), 200)

// process mirrors core.StateProcessor.Process using only exported pieces so
// that missing state can be detected per round. In discovery mode a failing
// transaction is skipped (instead of aborting) when misses are pending, so
// a single round discovers as many accesses as possible.
// checkpoint is the execution state right before transaction tx, captured
// in a round that had no misses up to that point (so it is built purely from
// verified data and can seed later rounds).
type checkpoint struct {
	sdb      *state.StateDB
	tx       int
	receipts types.Receipts
	logs     []*types.Log
	usedGas  uint64
	gas      uint64
}

const checkpointEvery = 8

func (e *Executor) process(block *types.Block, sdb *state.StateDB, cc *ChainContext, r *verified.Reader, cp *checkpoint, save func(*checkpoint)) (*state.StateDB, types.Receipts, uint64, error) {
	var (
		header   = block.Header()
		gp       = new(core.GasPool).AddGas(block.GasLimit())
		usedGas  = new(uint64)
		receipts types.Receipts
		logs     []*types.Log
		signer   = types.MakeSigner(e.cfg, header.Number, header.Time)
		start    = 0
	)
	if cp != nil {
		sdb = cp.sdb.Copy()
		start = cp.tx
		receipts = append(types.Receipts(nil), cp.receipts...)
		logs = append([]*types.Log(nil), cp.logs...)
		*usedGas = cp.usedGas
		gp = new(core.GasPool).AddGas(cp.gas)
	}
	blockCtx := core.NewEVMBlockContext(header, cc, nil)
	evm := vm.NewEVM(blockCtx, sdb, e.cfg, vm.Config{})
	if cp == nil {
		if br := block.BeaconRoot(); br != nil {
			core.ProcessBeaconBlockRoot(*br, evm)
		}
		if e.cfg.IsPrague(block.Number(), block.Time()) {
			core.ProcessParentBlockHash(block.ParentHash(), evm)
		}
	}
	txs := block.Transactions()
	for i := start; i < len(txs); i++ {
		tx := txs[i]
		if save != nil && i > start && i%checkpointEvery == 0 && r.Misses().Empty() {
			save(&checkpoint{
				sdb: sdb.Copy(), tx: i, usedGas: *usedGas, gas: gp.Gas(),
				receipts: append(types.Receipts(nil), receipts...), logs: append([]*types.Log(nil), logs...),
			})
		}
		msg, err := core.TransactionToMessage(tx, signer, header.BaseFee)
		if err != nil {
			return sdb, nil, 0, &InvalidBlockError{fmt.Errorf("tx %d: %w", i, err)}
		}
		sdb.SetTxContext(tx.Hash(), i)
		// Touch the sender first so a missing account taints the round.
		sdb.GetBalance(msg.From)
		if !r.Misses().Empty() {
			// Tainted round: its result is discarded, so relax pre-checks to
			// let the transaction run deep and expose its state accesses.
			msg.SkipNonceChecks = true
			msg.SkipFromEOACheck = true
			sdb.AddBalance(msg.From, discoveryBalance, tracing.BalanceChangeUnspecified)
			gp.AddGas(tx.Gas())
		}
		receipt, err := core.ApplyTransactionWithEVM(msg, gp, sdb, header.Number, block.Hash(), header.Time, tx, usedGas, evm)
		if err != nil {
			if !r.Misses().Empty() {
				continue
			}
			return sdb, nil, 0, &InvalidBlockError{fmt.Errorf("tx %d [%x]: %w", i, tx.Hash(), err)}
		}
		receipts = append(receipts, receipt)
		logs = append(logs, receipt.Logs...)
	}
	if e.cfg.IsPrague(block.Number(), block.Time()) {
		requests := [][]byte{}
		if err := core.ParseDepositLogs(&requests, logs, e.cfg); err != nil {
			return sdb, nil, 0, &InvalidBlockError{err}
		}
		if err := core.ProcessWithdrawalQueue(&requests, evm); err != nil {
			return sdb, nil, 0, &InvalidBlockError{err}
		}
		if err := core.ProcessConsolidationQueue(&requests, evm); err != nil {
			return sdb, nil, 0, &InvalidBlockError{err}
		}
	}
	e.engine.Finalize(cc, header, sdb, block.Body())
	return sdb, receipts, *usedGas, nil
}

// prefetch warms the cache for block: one batch for the obviously touched
// accounts, then every transaction is run concurrently against the
// pre-state with inline fetching so the sequential discovery chains of
// different transactions overlap.
func (e *Executor) prefetch(ctx context.Context, block *types.Block, parent *types.Header, at verified.BlockRef) int {
	header := block.Header()
	signer := types.MakeSigner(e.cfg, header.Number, header.Time)
	seen := map[common.Address]bool{header.Coinbase: true}
	addrs := []common.Address{header.Coinbase}
	add := func(a common.Address) {
		if !seen[a] {
			seen[a] = true
			addrs = append(addrs, a)
		}
	}
	msgs := make([]*core.Message, 0, len(block.Transactions()))
	for _, tx := range block.Transactions() {
		msg, err := core.TransactionToMessage(tx, signer, header.BaseFee)
		if err != nil {
			continue
		}
		add(msg.From)
		if msg.To != nil {
			add(*msg.To)
		}
		msgs = append(msgs, msg)
	}
	for _, w := range block.Withdrawals() {
		add(w.Address)
	}
	// Preferred path: one batch of untrusted access-list hints, then one
	// verified bulk proof fetch.
	calls := make([]verified.CallHint, 0, len(block.Transactions()))
	for _, tx := range block.Transactions() {
		from, err := types.Sender(signer, tx)
		if err != nil {
			continue
		}
		c := verified.CallHint{From: from, To: tx.To(), Gas: hexutil.Uint64(tx.Gas()), Value: (*hexutil.Big)(tx.Value()), Data: tx.Data()}
		if tx.Type() == types.LegacyTxType || tx.Type() == types.AccessListTxType {
			c.GasPrice = (*hexutil.Big)(tx.GasPrice())
		} else {
			c.FeeCap, c.TipCap = (*hexutil.Big)(tx.GasFeeCap()), (*hexutil.Big)(tx.GasTipCap())
		}
		if al := tx.AccessList(); len(al) > 0 {
			c.AccessList = &al
		}
		calls = append(calls, c)
	}
	// Hints at the parent capture each transaction in isolation; hints at
	// the block's own (claimed) post-state additionally surface keys that
	// depend on earlier transactions of the block. Both are only hints.
	var (
		postLists []types.AccessList
		postWG    sync.WaitGroup
	)
	postWG.Add(1)
	go func() {
		defer postWG.Done()
		postLists, _ = e.src.Hints(ctx, verified.BlockRef{Hash: block.Hash(), Number: block.NumberU64()}, calls)
	}()
	lists, ok := e.src.Hints(ctx, at, calls)
	postWG.Wait()
	if ok {
		lists = append(lists, postLists...)
	}
	if ok {
		hinted := 0
		for _, l := range lists {
			if l != nil {
				hinted++
			}
		}
		n, err := e.src.PrefetchHinted(ctx, parent.Root, at, addrs, lists)
		if err == nil && hinted*2 >= len(calls) {
			return n
		}
		log.Debug("Hinted prefetch incomplete, falling back to parallel prefetch", "block", block.NumberU64(), "hinted", hinted, "txs", len(calls), "err", err)
	}
	if err := e.src.PrefetchAccounts(ctx, parent.Root, at, addrs); err != nil {
		log.Debug("Account prefetch failed", "block", block.NumberU64(), "err", err)
	}
	var (
		wg      sync.WaitGroup
		sem     = make(chan struct{}, e.PrefetchParallel)
		fetched atomic.Int64
	)
	for _, msg := range msgs {
		wg.Add(1)
		sem <- struct{}{}
		go func(msg *core.Message) {
			defer wg.Done()
			defer func() { <-sem }()
			n := e.src.Prefetch(ctx, parent.Root, at, func(r *verified.Reader) {
				sdb, err := state.NewWithReader(parent.Root, e.src.StateDatabase(), r)
				if err != nil {
					return
				}
				m := *msg
				m.SkipNonceChecks, m.SkipFromEOACheck = true, true
				sdb.AddBalance(m.From, discoveryBalance, tracing.BalanceChangeUnspecified)
				evm := vm.NewEVM(core.NewEVMBlockContext(header, e.NewChainContext(r), nil), sdb, e.cfg, vm.Config{NoBaseFee: true})
				core.ApplyMessage(evm, &m, new(core.GasPool).AddGas(gomath.MaxUint64))
			})
			fetched.Add(int64(n))
		}(msg)
	}
	wg.Wait()
	return int(fetched.Load()) + len(addrs)
}

// ExecuteBlock executes block on top of parent's verified state.
func (e *Executor) ExecuteBlock(ctx context.Context, block *types.Block, parent *types.Header) (*Result, error) {
	start := time.Now()
	var res *Result
	at := verified.BlockRef{Hash: parent.Hash(), Number: parent.Number.Uint64()}
	prefetched := 0
	if e.PrefetchParallel > 0 && len(block.Transactions()) > 0 {
		prefetched = e.prefetch(ctx, block, parent, at)
		log.Debug("Prefetched block witness", "block", block.NumberU64(), "items", prefetched, "elapsed", time.Since(start).Round(time.Millisecond))
	}
	post := &verified.PostState{Root: block.Root(), At: verified.BlockRef{Hash: block.Hash(), Number: block.NumberU64()}}
	// Later rounds resume from the latest checkpoint captured before the
	// first miss instead of replaying already-verified transactions.
	var cp *checkpoint
	stats, err := e.src.RunWithPost(ctx, parent.Root, at, post, e.MaxRounds, func(r *verified.Reader) error {
		res = nil
		cc := e.NewChainContext(r)
		sdb, err := state.NewWithReader(parent.Root, e.src.StateDatabase(), r)
		if err != nil {
			return err
		}
		sdb, receipts, gasUsed, perr := e.process(block, sdb, cc, r, cp, func(c *checkpoint) { cp = c })
		if !r.Misses().Empty() {
			return nil
		}
		if perr != nil {
			return perr
		}
		if serr := sdb.Error(); serr != nil {
			if r.RecordStateError(serr) {
				return nil
			}
			return serr
		}
		root := sdb.IntermediateRoot(e.cfg.IsEIP158(block.Number()))
		if serr := sdb.Error(); serr != nil {
			if r.RecordStateError(serr) {
				return nil
			}
			return serr
		}
		res = &Result{
			Root:        root,
			ReceiptRoot: types.DeriveSha(receipts, trie.NewStackTrie(nil)),
			Receipts:    receipts,
			GasUsed:     gasUsed,
			Bloom:       types.MergeBloom(receipts),
			State:       sdb,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if res == nil {
		return nil, errors.New("execution produced no result")
	}
	stats.Fetched += prefetched
	res.Stats = stats
	res.Duration = time.Since(start)
	metrics.ExecRounds.Observe(float64(stats.Rounds))
	metrics.ExecFetches.Observe(float64(stats.Fetched))
	metrics.ExecSeconds.ObserveSince(start)
	return res, nil
}

// Validate compares an execution result against the block header.
func Validate(block *types.Block, res *Result) error {
	h := block.Header()
	switch {
	case res.GasUsed != h.GasUsed:
		return fmt.Errorf("gas used mismatch: remote %d local %d", h.GasUsed, res.GasUsed)
	case res.Bloom != h.Bloom:
		return errors.New("logs bloom mismatch")
	case res.ReceiptRoot != h.ReceiptHash:
		return fmt.Errorf("receipt root mismatch: remote %x local %x", h.ReceiptHash, res.ReceiptRoot)
	case res.Root != h.Root:
		return fmt.Errorf("state root mismatch: remote %x local %x", h.Root, res.Root)
	}
	return nil
}

// BuildArgs describes a payload to build.
type BuildArgs struct {
	Parent       *types.Header
	Timestamp    uint64
	Random       common.Hash
	FeeRecipient common.Address
	Withdrawals  []*types.Withdrawal
	GasLimit     uint64 // 0 keeps the parent's gas limit
	Extra        []byte
}

// BuildEmpty builds a block with no transactions, applying only withdrawals.
// The mempool is not tracked by the lite node, so proposals carry no
// transactions (and no fee revenue), but they are valid and keep the
// validator from missing its slot.
func (e *Executor) BuildEmpty(ctx context.Context, args BuildArgs) (*types.Block, error) {
	parent := args.Parent
	number := new(big.Int).Add(parent.Number, common.Big1)
	if e.cfg.IsCancun(number, args.Timestamp) {
		return nil, errors.New("payload building is only implemented up to Shanghai")
	}
	gasLimit := parent.GasLimit
	if args.GasLimit != 0 {
		gasLimit = core.CalcGasLimit(parent.GasLimit, args.GasLimit)
	}
	withdrawals := args.Withdrawals
	if withdrawals == nil {
		withdrawals = []*types.Withdrawal{}
	}
	header := &types.Header{
		ParentHash: parent.Hash(),
		UncleHash:  types.EmptyUncleHash,
		Coinbase:   args.FeeRecipient,
		Difficulty: new(big.Int),
		Number:     number,
		GasLimit:   gasLimit,
		Time:       args.Timestamp,
		Extra:      args.Extra,
		MixDigest:  args.Random,
		BaseFee:    eip1559.CalcBaseFee(e.cfg, parent),
	}
	body := &types.Body{Withdrawals: withdrawals}
	at := verified.BlockRef{Hash: parent.Hash(), Number: parent.Number.Uint64()}
	var root common.Hash
	_, err := e.src.Run(ctx, parent.Root, at, e.MaxRounds, func(r *verified.Reader) error {
		cc := e.NewChainContext(r)
		sdb, err := state.NewWithReader(parent.Root, e.src.StateDatabase(), r)
		if err != nil {
			return err
		}
		e.engine.Finalize(cc, header, sdb, body)
		if !r.Misses().Empty() {
			return nil
		}
		root = sdb.IntermediateRoot(e.cfg.IsEIP158(number))
		if serr := sdb.Error(); serr != nil {
			if r.RecordStateError(serr) {
				return nil
			}
			return serr
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	header.Root = root
	return types.NewBlock(header, body, nil, trie.NewStackTrie(nil)), nil
}
