// Package engineapi implements the Engine API consumed by the consensus
// client. In "full" and "validator" modes every payload is executed on
// proof-verified state and reported VALID only if the locally computed state
// root, receipt root, gas used and bloom match. Anything that cannot be
// validated (old blocks outside the upstream state window, unresolvable
// witnesses, timeouts) is reported SYNCING so the consensus client imports it
// optimistically; the service never reports INVALID for a block it merely
// failed to process.
package engineapi

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/beacon/engine"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/log"
	"github.com/pulsechainlite/pulselite/internal/chain"
	"github.com/pulsechainlite/pulselite/internal/config"
	"github.com/pulsechainlite/pulselite/internal/exec"
	"github.com/pulsechainlite/pulselite/internal/metrics"
	"github.com/pulsechainlite/pulselite/internal/verified"
)

// Version is reported via engine_getClientVersionV1 and web3_clientVersion.
const Version = "0.1.0"

// Hooks lets the node react to engine events.
type Hooks struct {
	// OnNewBlock is called for every block received (e.g. to fetch receipts
	// in modes that do not execute).
	OnNewBlock func(b *types.Block)
	// OnHead is called after the canonical head changes.
	OnHead func(h *types.Header)
	// FetchBody downloads and verifies a block body into the window.
	FetchBody func(ctx context.Context, hash common.Hash) error
}

// API is the "engine" RPC namespace.
type API struct {
	mode        config.Mode
	exec        *exec.Executor
	chain       *chain.Chain
	backend     verified.Backend
	deadline    time.Duration
	stateWindow uint64
	gasLimit    uint64
	extra       []byte
	hooks       Hooks

	networkHead atomic.Uint64 // latest execution block number known from the beacon node
	busy        atomic.Int32  // validations running or queued

	mu       sync.Mutex
	jobs     map[common.Hash]*execJob
	pending  map[common.Hash]*execJob
	wake     chan struct{}
	payloads map[engine.PayloadID]*buildJob
	order    []engine.PayloadID
}

type execJob struct {
	block *types.Block
	done  chan struct{}
	valid bool
	err   error
}

type buildJob struct {
	done  chan struct{}
	block *types.Block
	err   error
}

// New creates the engine API.
func New(cfg *config.Config, ex *exec.Executor, ch *chain.Chain, backend verified.Backend, hooks Hooks) *API {
	api := &API{
		mode:        cfg.Mode,
		exec:        ex,
		chain:       ch,
		backend:     backend,
		deadline:    cfg.ExecDeadline,
		stateWindow: cfg.StateWindow,
		gasLimit:    cfg.MinerGasLimit,
		extra:       []byte(cfg.MinerExtra),
		hooks:       hooks,
		jobs:        make(map[common.Hash]*execJob),
		pending:     make(map[common.Hash]*execJob),
		wake:        make(chan struct{}, 1),
		payloads:    make(map[engine.PayloadID]*buildJob),
	}
	go api.worker()
	return api
}

// SetNetworkHead records the newest execution block number seen by the
// consensus client, used to skip blocks whose parent state upstreams no
// longer serve.
func (api *API) SetNetworkHead(n uint64) {
	for {
		old := api.networkHead.Load()
		if n <= old || api.networkHead.CompareAndSwap(old, n) {
			return
		}
	}
}

// Busy reports whether a payload validation is running or queued.
func (api *API) Busy() bool { return api.busy.Load() > 0 }

// NetworkHead returns the estimated network head block number.
func (api *API) NetworkHead() uint64 { return api.networkHead.Load() }

// SetNetworkHeadEstimate replaces the network head estimate (it may go
// down, e.g. when a slot-based estimate overshot because of missed slots).
func (api *API) SetNetworkHeadEstimate(n uint64) { api.networkHead.Store(n) }

var capabilities = []string{
	"engine_exchangeCapabilities",
	"engine_exchangeTransitionConfigurationV1",
	"engine_getClientVersionV1",
	"engine_newPayloadV1",
	"engine_newPayloadV2",
	"engine_forkchoiceUpdatedV1",
	"engine_forkchoiceUpdatedV2",
	"engine_getPayloadV1",
	"engine_getPayloadV2",
	"engine_getPayloadBodiesByHashV1",
	"engine_getPayloadBodiesByRangeV1",
}

// ExchangeCapabilities implements engine_exchangeCapabilities.
func (api *API) ExchangeCapabilities([]string) []string { return capabilities }

// GetClientVersionV1 implements engine_getClientVersionV1.
func (api *API) GetClientVersionV1(engine.ClientVersionV1) []engine.ClientVersionV1 {
	return []engine.ClientVersionV1{{Code: "PL", Name: "pulselite", Version: "v" + Version, Commit: "0x00000000"}}
}

// ExchangeTransitionConfigurationV1 implements the (deprecated) method.
func (api *API) ExchangeTransitionConfigurationV1(c engine.TransitionConfigurationV1) (*engine.TransitionConfigurationV1, error) {
	return &c, nil
}

func strPtr(s string) *string { return &s }

func syncing() engine.PayloadStatusV1 { return engine.PayloadStatusV1{Status: engine.SYNCING} }

// NewPayloadV1 implements engine_newPayloadV1 (pre-Shanghai payloads).
func (api *API) NewPayloadV1(params engine.ExecutableData) (engine.PayloadStatusV1, error) {
	if params.Withdrawals != nil {
		return engine.PayloadStatusV1{Status: engine.INVALID}, engine.InvalidParams.With(errors.New("withdrawals not supported in V1"))
	}
	return api.newPayload(params)
}

// NewPayloadV2 implements engine_newPayloadV2 (Shanghai payloads).
func (api *API) NewPayloadV2(params engine.ExecutableData) (engine.PayloadStatusV1, error) {
	cfg := api.exec.Config()
	shanghai := cfg.IsShanghai(new(bigInt).SetUint64(params.Number), params.Timestamp)
	if shanghai && params.Withdrawals == nil {
		return engine.PayloadStatusV1{Status: engine.INVALID}, engine.InvalidParams.With(errors.New("nil withdrawals post-shanghai"))
	}
	if !shanghai && params.Withdrawals != nil {
		return engine.PayloadStatusV1{Status: engine.INVALID}, engine.InvalidParams.With(errors.New("non-nil withdrawals pre-shanghai"))
	}
	if params.ExcessBlobGas != nil || params.BlobGasUsed != nil {
		return engine.PayloadStatusV1{Status: engine.INVALID}, engine.InvalidParams.With(errors.New("blob fields not supported in V2"))
	}
	return api.newPayload(params)
}

// NewPayloadV3 is not supported: PulseChain has not activated Cancun.
func (api *API) NewPayloadV3(engine.ExecutableData, []common.Hash, *common.Hash) (engine.PayloadStatusV1, error) {
	return engine.PayloadStatusV1{Status: engine.INVALID}, engine.UnsupportedFork
}

func (api *API) newPayload(params engine.ExecutableData) (engine.PayloadStatusV1, error) {
	block, err := engine.ExecutableDataToBlock(params, nil, nil, nil)
	if err != nil {
		metrics.PayloadStatus(engine.INVALID).Inc()
		log.Warn("Invalid payload (block hash / encoding)", "number", params.Number, "hash", params.BlockHash, "err", err)
		return engine.PayloadStatusV1{Status: engine.INVALID, ValidationError: strPtr(err.Error())}, nil
	}
	hash := block.Hash()
	api.chain.AddBlock(block)
	api.SetNetworkHead(block.NumberU64())
	if api.hooks.OnNewBlock != nil {
		api.hooks.OnNewBlock(block)
	}
	if api.chain.IsValid(hash) {
		return api.valid(hash), nil
	}
	if !api.mode.Executes() {
		metrics.PayloadStatus(engine.SYNCING).Inc()
		return syncing(), nil
	}
	if nh := api.networkHead.Load(); nh > block.NumberU64()+api.stateWindow {
		// Parent state is older than what upstreams retain: import optimistically.
		metrics.PayloadStatus(engine.SYNCING).Inc()
		log.Debug("Payload outside state window, importing optimistically", "number", block.NumberU64(), "networkHead", nh)
		return syncing(), nil
	}
	job := api.startExec(block)
	if nh := api.networkHead.Load(); nh > block.NumberU64()+catchUpLag {
		// Consensus client is catching up: don't hold it back. The newest
		// payload is validated in the background and VALID descendants
		// validate these ancestors for the consensus client.
		metrics.PayloadStatus(engine.SYNCING).Inc()
		return syncing(), nil
	}
	select {
	case <-job.done:
	case <-time.After(api.deadline):
		metrics.PayloadStatus("TIMEOUT").Inc()
		log.Warn("Payload validation exceeded deadline, answering SYNCING", "number", block.NumberU64(), "hash", hash)
		return syncing(), nil
	}
	if job.valid {
		return api.valid(hash), nil
	}
	return syncing(), nil
}

func (api *API) valid(hash common.Hash) engine.PayloadStatusV1 {
	metrics.PayloadStatus(engine.VALID).Inc()
	h := hash
	return engine.PayloadStatusV1{Status: engine.VALID, LatestValidHash: &h}
}

func (api *API) parentHeader(ctx context.Context, block *types.Block) (*types.Header, error) {
	if p := api.chain.Header(block.ParentHash()); p != nil {
		return p, nil
	}
	hs, err := api.backend.HeadersByHash(ctx, []common.Hash{block.ParentHash()})
	if err != nil {
		return nil, err
	}
	if hs[0].Hash() != block.ParentHash() {
		metrics.HeaderFail.Inc()
		return nil, errors.New("upstream returned wrong parent header")
	}
	metrics.HeaderOK.Inc()
	api.chain.AddHeader(hs[0])
	return hs[0], nil
}

var errSkipped = errors.New("superseded by a newer payload")

// catchUpLag is how far behind the network head a payload may be before
// newPayload stops waiting for its validation.
const catchUpLag = 3

// maxHeadRewind is the deepest canonical ancestor forkchoiceUpdated may move
// the head back to (two epochs: deeper blocks are finalized).
var maxHeadRewind uint64 = 64

// startExec queues (or joins) validation of block. A single worker runs
// validations one at a time so consecutive blocks reuse the warm post-state
// of their parent instead of competing for upstream bandwidth.
func (api *API) startExec(block *types.Block) *execJob {
	hash := block.Hash()
	api.mu.Lock()
	defer api.mu.Unlock()
	if job, ok := api.jobs[hash]; ok {
		return job
	}
	job := &execJob{block: block, done: make(chan struct{})}
	api.jobs[hash] = job
	api.pending[hash] = job
	select {
	case api.wake <- struct{}{}:
	default:
	}
	return job
}

func (api *API) finish(job *execJob, err error) {
	job.err = err
	job.valid = err == nil
	close(job.done)
	hash := job.block.Hash()
	// Keep finished jobs briefly so a late forkchoiceUpdated can join.
	time.AfterFunc(2*time.Minute, func() {
		api.mu.Lock()
		delete(api.jobs, hash)
		api.mu.Unlock()
	})
}

// worker validates the newest pending payload first. Older pending payloads
// are dropped: a VALID descendant implies their validity to the consensus
// client, which then marks them valid as well.
func (api *API) worker() {
	for range api.wake {
		for {
			api.mu.Lock()
			var pick *execJob
			for _, j := range api.pending {
				if pick == nil || j.block.NumberU64() > pick.block.NumberU64() {
					pick = j
				}
			}
			if pick == nil {
				api.mu.Unlock()
				break
			}
			delete(api.pending, pick.block.Hash())
			var skipped []*execJob
			for h, j := range api.pending {
				if j.block.NumberU64() < pick.block.NumberU64() {
					delete(api.pending, h)
					skipped = append(skipped, j)
				}
			}
			api.mu.Unlock()
			for _, j := range skipped {
				metrics.PayloadStatus("SKIPPED").Inc()
				api.finish(j, errSkipped)
			}
			api.busy.Add(1)
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			err := api.validate(ctx, pick.block)
			cancel()
			api.busy.Add(-1)
			api.finish(pick, err)
		}
	}
}

func (api *API) validate(ctx context.Context, block *types.Block) error {
	number, hash := block.NumberU64(), block.Hash()
	parent, err := api.parentHeader(ctx, block)
	if err != nil {
		metrics.PayloadStatus("ERROR").Inc()
		log.Warn("Cannot validate payload: parent unavailable", "number", number, "err", err)
		return err
	}
	res, err := api.exec.ExecuteBlock(ctx, block, parent)
	if err == nil {
		err = exec.Validate(block, res)
		if err != nil {
			err = &exec.InvalidBlockError{Err: err}
		}
	}
	if err != nil {
		var inv *exec.InvalidBlockError
		if errors.As(err, &inv) {
			metrics.PayloadStatus("MISMATCH").Inc()
			log.Error("Local execution disagrees with payload; reporting SYNCING", "number", number, "hash", hash, "err", err)
		} else {
			metrics.PayloadStatus("ERROR").Inc()
			log.Warn("Payload validation failed; reporting SYNCING", "number", number, "hash", hash, "err", err)
		}
		return err
	}
	if err := api.chain.SetReceipts(hash, res.Receipts); err != nil {
		log.Warn("Failed to store receipts", "number", number, "err", err)
	}
	api.chain.MarkValid(hash)
	if err := api.exec.Source().Persist(res.State, number, api.exec.Config().IsEIP158(block.Number())); err != nil {
		log.Debug("Post-state not cached (will be re-fetched on demand)", "number", number, "err", err)
	}
	log.Info("Validated block", "number", number, "hash", hash, "txs", len(block.Transactions()),
		"gas", block.GasUsed(), "rounds", res.Stats.Rounds, "fetched", res.Stats.Fetched, "elapsed", res.Duration.Round(time.Millisecond))
	return nil
}

// ForkchoiceUpdatedV1 implements engine_forkchoiceUpdatedV1.
func (api *API) ForkchoiceUpdatedV1(state engine.ForkchoiceStateV1, attrs *engine.PayloadAttributes) (engine.ForkChoiceResponse, error) {
	if attrs != nil && attrs.Withdrawals != nil {
		return engine.STATUS_INVALID, engine.InvalidPayloadAttributes.With(errors.New("withdrawals not supported in V1"))
	}
	return api.forkchoiceUpdated(state, attrs, engine.PayloadV1)
}

// ForkchoiceUpdatedV2 implements engine_forkchoiceUpdatedV2.
func (api *API) ForkchoiceUpdatedV2(state engine.ForkchoiceStateV1, attrs *engine.PayloadAttributes) (engine.ForkChoiceResponse, error) {
	if attrs != nil && attrs.BeaconRoot != nil {
		return engine.STATUS_INVALID, engine.InvalidPayloadAttributes.With(errors.New("parentBeaconBlockRoot not supported in V2"))
	}
	return api.forkchoiceUpdated(state, attrs, engine.PayloadV2)
}

// ForkchoiceUpdatedV3 is not supported: PulseChain has not activated Cancun.
func (api *API) ForkchoiceUpdatedV3(engine.ForkchoiceStateV1, *engine.PayloadAttributes) (engine.ForkChoiceResponse, error) {
	return engine.STATUS_INVALID, engine.UnsupportedFork
}

func (api *API) knownHeader(hash common.Hash) *types.Header {
	if h := api.chain.Header(hash); h != nil {
		return h
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	hs, err := api.backend.HeadersByHash(ctx, []common.Hash{hash})
	if err != nil || hs[0].Hash() != hash {
		return nil
	}
	api.chain.AddHeader(hs[0])
	return hs[0]
}

func (api *API) forkchoiceUpdated(fc engine.ForkchoiceStateV1, attrs *engine.PayloadAttributes, version engine.PayloadVersion) (engine.ForkChoiceResponse, error) {
	if fc.HeadBlockHash == (common.Hash{}) {
		return engine.STATUS_INVALID, nil
	}
	head := api.knownHeader(fc.HeadBlockHash)
	if head == nil {
		return engine.STATUS_SYNCING, nil
	}
	if cur := api.chain.Head(); cur != nil && head.Number.Uint64()+maxHeadRewind < cur.Number.Uint64() && api.chain.IsCanonical(fc.HeadBlockHash) {
		// The consensus client restarted from an older (finalized) checkpoint
		// and is catching up through blocks we already have. Reorgs never go
		// this deep, so keep serving the newer head instead of rewinding.
		log.Debug("Consensus client is behind the current head; not rewinding", "fcuHead", head.Number, "head", cur.Number)
		return engine.STATUS_SYNCING, nil
	}
	api.SetNetworkHead(head.Number.Uint64())
	if _, err := api.chain.SetHead(fc.HeadBlockHash); err != nil {
		return engine.STATUS_SYNCING, nil
	}
	// Safe/finalized pointers never delay the response: unknown headers are
	// fetched (and verified by hash) in the background.
	for _, p := range []struct {
		hash common.Hash
		set  func(common.Hash)
	}{{fc.SafeBlockHash, api.chain.SetSafe}, {fc.FinalizedBlockHash, api.chain.SetFinalized}} {
		if p.hash == (common.Hash{}) {
			continue
		}
		if api.chain.Header(p.hash) != nil {
			p.set(p.hash)
			continue
		}
		go func(hash common.Hash, set func(common.Hash)) {
			if api.knownHeader(hash) != nil {
				set(hash)
			}
		}(p.hash, p.set)
	}
	if api.hooks.OnHead != nil {
		api.hooks.OnHead(head)
	}
	status := engine.SYNCING
	if api.mode.Executes() {
		if !api.chain.IsValid(fc.HeadBlockHash) {
			// Join an in-flight validation for a short while.
			api.mu.Lock()
			job := api.jobs[fc.HeadBlockHash]
			api.mu.Unlock()
			if job != nil {
				select {
				case <-job.done:
				case <-time.After(2 * time.Second):
				}
			}
		}
		if api.chain.IsValid(fc.HeadBlockHash) {
			status = engine.VALID
		}
	}
	resp := engine.ForkChoiceResponse{PayloadStatus: engine.PayloadStatusV1{Status: status}}
	if status == engine.VALID {
		h := fc.HeadBlockHash
		resp.PayloadStatus.LatestValidHash = &h
	}
	if attrs == nil {
		return resp, nil
	}
	if status != engine.VALID {
		// Payloads are only built on validated heads.
		return resp, nil
	}
	if attrs.Timestamp <= head.Time {
		return engine.STATUS_INVALID, engine.InvalidPayloadAttributes.With(errors.New("timestamp not after parent"))
	}
	cfg := api.exec.Config()
	shanghai := cfg.IsShanghai(new(bigInt).Add(head.Number, bigOne), attrs.Timestamp)
	if version == engine.PayloadV2 && shanghai && attrs.Withdrawals == nil {
		return engine.STATUS_INVALID, engine.InvalidPayloadAttributes.With(errors.New("missing withdrawals"))
	}
	id := payloadID(fc.HeadBlockHash, attrs, version)
	api.startBuild(id, head, attrs)
	resp.PayloadID = &id
	return resp, nil
}

func payloadID(parent common.Hash, attrs *engine.PayloadAttributes, version engine.PayloadVersion) engine.PayloadID {
	h := sha256.New()
	h.Write(parent.Bytes())
	binary.Write(h, binary.BigEndian, attrs.Timestamp)
	h.Write(attrs.Random.Bytes())
	h.Write(attrs.SuggestedFeeRecipient.Bytes())
	for _, w := range attrs.Withdrawals {
		binary.Write(h, binary.BigEndian, w.Index)
		binary.Write(h, binary.BigEndian, w.Validator)
		h.Write(w.Address.Bytes())
		binary.Write(h, binary.BigEndian, w.Amount)
	}
	var id engine.PayloadID
	copy(id[:], h.Sum(nil)[:8])
	id[0] = byte(version)
	return id
}

func (api *API) startBuild(id engine.PayloadID, parent *types.Header, attrs *engine.PayloadAttributes) {
	api.mu.Lock()
	if _, ok := api.payloads[id]; ok {
		api.mu.Unlock()
		return
	}
	job := &buildJob{done: make(chan struct{})}
	api.payloads[id] = job
	api.order = append(api.order, id)
	for len(api.order) > 16 {
		delete(api.payloads, api.order[0])
		api.order = api.order[1:]
	}
	api.mu.Unlock()

	go func() {
		defer close(job.done)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		start := time.Now()
		job.block, job.err = api.exec.BuildEmpty(ctx, exec.BuildArgs{
			Parent:       parent,
			Timestamp:    attrs.Timestamp,
			Random:       attrs.Random,
			FeeRecipient: attrs.SuggestedFeeRecipient,
			Withdrawals:  attrs.Withdrawals,
			GasLimit:     api.gasLimit,
			Extra:        api.extra,
		})
		if job.err != nil {
			log.Error("Payload build failed", "id", id, "parent", parent.Number, "err", job.err)
			return
		}
		log.Info("Built payload", "id", id, "number", job.block.NumberU64(), "hash", job.block.Hash(),
			"withdrawals", len(attrs.Withdrawals), "elapsed", time.Since(start).Round(time.Millisecond))
	}()
}

func (api *API) getPayload(id engine.PayloadID) (*types.Block, error) {
	api.mu.Lock()
	job := api.payloads[id]
	api.mu.Unlock()
	if job == nil {
		return nil, engine.UnknownPayload
	}
	select {
	case <-job.done:
	case <-time.After(8 * time.Second):
		return nil, engine.GenericServerError.With(errors.New("payload still building"))
	}
	if job.err != nil {
		return nil, engine.GenericServerError.With(job.err)
	}
	return job.block, nil
}

// GetPayloadV1 implements engine_getPayloadV1.
func (api *API) GetPayloadV1(id engine.PayloadID) (*engine.ExecutableData, error) {
	if !id.Is(engine.PayloadV1) {
		return nil, engine.UnsupportedFork
	}
	b, err := api.getPayload(id)
	if err != nil {
		return nil, err
	}
	return engine.BlockToExecutableData(b, new(bigInt), nil, nil).ExecutionPayload, nil
}

// GetPayloadV2 implements engine_getPayloadV2.
func (api *API) GetPayloadV2(id engine.PayloadID) (*engine.ExecutionPayloadEnvelope, error) {
	if !id.Is(engine.PayloadV1, engine.PayloadV2) {
		return nil, engine.UnsupportedFork
	}
	b, err := api.getPayload(id)
	if err != nil {
		return nil, err
	}
	return engine.BlockToExecutableData(b, new(bigInt), nil, nil), nil
}

// GetPayloadBodiesByHashV1 implements engine_getPayloadBodiesByHashV1.
func (api *API) GetPayloadBodiesByHashV1(hashes []common.Hash) []*engine.ExecutionPayloadBody {
	out := make([]*engine.ExecutionPayloadBody, len(hashes))
	for i, h := range hashes {
		out[i] = toBody(api.body(h))
	}
	return out
}

// body returns a stored body, fetching (and verifying against the header's
// transactions/withdrawals roots) bodies of blocks the consensus client knows
// but never sent us, e.g. the checkpoint-sync anchor block.
func (api *API) body(hash common.Hash) *types.Body {
	if b := api.chain.Body(hash); b != nil {
		return b
	}
	if api.hooks.FetchBody == nil || api.knownHeader(hash) == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := api.hooks.FetchBody(ctx, hash); err != nil {
		log.Debug("Body fetch failed", "hash", hash, "err", err)
		return nil
	}
	return api.chain.Body(hash)
}

// GetPayloadBodiesByRangeV1 implements engine_getPayloadBodiesByRangeV1.
func (api *API) GetPayloadBodiesByRangeV1(start, count hexutil.Uint64) ([]*engine.ExecutionPayloadBody, error) {
	if start == 0 || count == 0 {
		return nil, engine.InvalidParams.With(fmt.Errorf("invalid start or count: %d %d", start, count))
	}
	if count > 1024 {
		return nil, engine.TooLargeRequest.With(fmt.Errorf("requested count too large: %d", count))
	}
	var out []*engine.ExecutionPayloadBody
	for n := uint64(start); n < uint64(start+count); n++ {
		h, ok := api.chain.CanonicalHash(n)
		if !ok {
			out = append(out, nil)
			continue
		}
		out = append(out, toBody(api.body(h)))
	}
	return out, nil
}

func toBody(b *types.Body) *engine.ExecutionPayloadBody {
	if b == nil {
		return nil
	}
	txs := make([]hexutil.Bytes, len(b.Transactions))
	for i, tx := range b.Transactions {
		txs[i], _ = tx.MarshalBinary()
	}
	ws := b.Withdrawals
	return &engine.ExecutionPayloadBody{TransactionData: txs, Withdrawals: ws}
}
