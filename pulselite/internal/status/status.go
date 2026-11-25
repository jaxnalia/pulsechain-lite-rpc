// Package status computes node readiness ("is this RPC safe to point a
// wallet at?") and serves it as JSON, a readiness probe and a small
// dashboard.
package status

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/pulsechainlite/pulselite/internal/chain"
	"github.com/pulsechainlite/pulselite/internal/exec"
	"github.com/pulsechainlite/pulselite/internal/follow"
	"github.com/pulsechainlite/pulselite/internal/metrics"
)

//go:embed dashboard.html
var dashboardHTML []byte

// WPLS is read at every new head to check that verified state is being
// served end to end (it also keeps the top of the account trie warm).
var WPLS = common.HexToAddress("0xA1077a294dDE1B09bB078844df40758a5D0f9a27")

const (
	maxHeadAge  = 90 * time.Second // PulseChain targets 10 s blocks; tolerates a few missed slots
	maxHeadLag  = 3                // blocks behind the network head
	probeMaxLag = 6                // state probe must be this recent (blocks)
	recentCount = 12

	// Until these elapse, a missing condition is reported as pending
	// (still working) rather than failed.
	startupGrace   = 5 * time.Minute // consensus client starting, peers connecting
	headStallFail     = 5 * time.Minute // no new block although the consensus client is synced
	probeFailAfter    = 2 * time.Minute // state reads failing since the last success
	consensusDownFail = 3 * time.Minute // consensus client unreachable (a restart takes ~40 s)
	syncStallFail     = 5 * time.Minute // consensus client syncing without progress
)

// Cache reports the verified-state cache size.
type Cache interface {
	LogicalBytes() uint64
	Cap() uint64
}

// Service tracks readiness.
type Service struct {
	Version        string
	Mode           string
	ChainID        uint64
	Chain          *chain.Chain
	Beacon         *follow.BeaconClient
	Exec           *exec.Executor
	Cache          Cache
	NetworkHead    func() uint64
	Peers          func() (eth, snap int) // nil when RPC upstreams are configured
	GasPrice       func(ctx context.Context) (*big.Int, error)
	DiskTotalLimit uint64

	start   time.Time
	probing atomic.Bool

	mu         sync.Mutex
	beacon     *follow.SyncStatus
	beaconErr  error
	clPeers    uint64
	downSince  time.Time // consensus client unreachable since (zero = reachable)
	lastSlot   uint64    // consensus head slot at lastSlotAt
	lastSlotAt time.Time // when the consensus head slot last changed
	probe      Probe
}

// Probe is the result of the latest end-to-end verified state read.
type Probe struct {
	Block     uint64  `json:"block"`
	OK        bool    `json:"ok"`
	LatencyMs float64 `json:"latencyMs"`
	Error     string  `json:"error,omitempty"`
	At        int64   `json:"at"`
	LastOK    int64   `json:"lastOk,omitempty"` // unix time of the last successful read
}

// Check states: a pending check is still working towards OK (starting up,
// syncing, retrying); fail means something stopped working.
const (
	CheckOK      = "ok"
	CheckPending = "pending"
	CheckFail    = "fail"
)

// Check is one readiness condition.
type Check struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	OK     bool   `json:"ok"`
	State  string `json:"state"` // ok | pending | fail
	Detail string `json:"detail"`
}

// Block summarises a recent block.
type Block struct {
	Number      uint64  `json:"number"`
	Hash        string  `json:"hash"`
	Time        uint64  `json:"time"`
	AgeSec      int64   `json:"ageSec"`
	Txs         int     `json:"txs"`
	GasUsed     uint64  `json:"gasUsed"`
	GasLimit    uint64  `json:"gasLimit"`
	BaseFeeGwei float64 `json:"baseFeeGwei"`
}

// Snapshot is the /api/status document.
type Snapshot struct {
	Ready       bool    `json:"ready"`
	State       string  `json:"state"` // starting | syncing | degraded | ready
	Summary     string  `json:"summary"`
	Checks      []Check `json:"checks"`
	Version     string  `json:"version"`
	Mode        string  `json:"mode"`
	Validation  bool    `json:"validation"`
	ChainID     uint64  `json:"chainId"`
	UptimeSec   int64   `json:"uptimeSec"`
	Head        *Block  `json:"head,omitempty"`
	NetworkHead uint64  `json:"networkHead"`
	Finalized   uint64  `json:"finalized,omitempty"`
	Recent      []Block `json:"recentBlocks"`
	Gas         struct {
		BaseFeeGwei  float64 `json:"baseFeeGwei"`
		GasPriceGwei float64 `json:"gasPriceGwei"`
	} `json:"gas"`
	P2P *struct {
		Eth  int `json:"eth"`
		Snap int `json:"snap"`
	} `json:"p2p,omitempty"`
	Consensus struct {
		Reachable    bool   `json:"reachable"`
		Error        string `json:"error,omitempty"`
		HeadSlot     uint64 `json:"headSlot"`
		SyncDistance uint64 `json:"syncDistance"`
		IsSyncing    bool   `json:"isSyncing"`
		IsOptimistic bool   `json:"isOptimistic"`
		Peers        uint64 `json:"peers"`
		DownSec      int64  `json:"downSec,omitempty"`    // unreachable for this long
		StalledSec   int64  `json:"stalledSec,omitempty"` // syncing without head-slot progress for this long
	} `json:"consensus"`
	Probe Probe `json:"stateProbe"`
	Cache struct {
		Bytes uint64 `json:"bytes"`
		Cap   uint64 `json:"cap"`
	} `json:"cache"`
	Disk struct {
		Execution uint64 `json:"execution"`
		Consensus uint64 `json:"consensus"`
		Total     uint64 `json:"total"`
		Limit     uint64 `json:"limit"`
	} `json:"disk"`
	Window struct {
		Headers   int    `json:"headers"`
		Txs       int    `json:"txs"`
		LogsFrom  uint64 `json:"logsFrom"`
		StateFrom uint64 `json:"stateFrom"`
	} `json:"window"`
	Verified struct {
		Proofs        uint64 `json:"proofs"`
		ProofsFailed  uint64 `json:"proofsFailed"`
		Headers       uint64 `json:"headers"`
		Receipts      uint64 `json:"receipts"`
		Codes         uint64 `json:"codes"`
		CacheHitRatio float64 `json:"cacheHitRatio"`
	} `json:"verified"`
	RPC struct {
		Requests     uint64  `json:"requests"`
		AvgLatencyMs float64 `json:"avgLatencyMs"`
	} `json:"rpc"`
}

// Run polls the consensus client and probes state at every new head.
func (s *Service) Run(ctx context.Context) {
	s.start = time.Now()
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	var probed uint64
	for {
		s.pollBeacon(ctx)
		if h := s.Chain.Latest(); h != nil && h.Number.Uint64() != probed && !s.probing.Load() {
			probed = h.Number.Uint64()
			s.probing.Store(true)
			go s.runProbe(ctx, h)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Service) pollBeacon(ctx context.Context) {
	if s.Beacon == nil {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	st, err := s.Beacon.Syncing(cctx)
	var peers uint64
	if err == nil {
		peers, _ = s.Beacon.PeerCount(cctx)
	}
	now := time.Now()
	s.mu.Lock()
	s.beacon, s.beaconErr = st, err
	if err != nil {
		if s.downSince.IsZero() {
			s.downSince = now
		}
	} else {
		s.downSince = time.Time{}
		s.clPeers = peers
		if st.HeadSlot != s.lastSlot || s.lastSlotAt.IsZero() {
			s.lastSlot, s.lastSlotAt = st.HeadSlot, now
		}
	}
	s.mu.Unlock()
}

func (s *Service) runProbe(ctx context.Context, h *types.Header) {
	defer s.probing.Store(false)
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	start := time.Now()
	read := func() error {
		return s.Exec.ReadState(cctx, h, func(sdb *state.StateDB) error {
			sdb.GetBalance(WPLS)
			return nil
		})
	}
	err := read()
	if err != nil && cctx.Err() == nil {
		err = read() // one retry: a single unlucky peer should not flip readiness
	}
	p := Probe{Block: h.Number.Uint64(), OK: err == nil, LatencyMs: float64(time.Since(start).Microseconds()) / 1000, At: time.Now().Unix()}
	if err != nil {
		p.Error = err.Error()
	}
	s.mu.Lock()
	p.LastOK = s.probe.LastOK
	if p.OK {
		p.LastOK = p.At
	}
	s.probe = p
	s.mu.Unlock()
}

func gwei(v *big.Int) float64 {
	if v == nil {
		return 0
	}
	f, _ := new(big.Float).Quo(new(big.Float).SetInt(v), big.NewFloat(1e9)).Float64()
	return f
}

func (s *Service) block(h *types.Header, now time.Time) Block {
	b := Block{
		Number: h.Number.Uint64(), Hash: h.Hash().Hex(), Time: h.Time,
		AgeSec: now.Unix() - int64(h.Time), GasUsed: h.GasUsed, GasLimit: h.GasLimit, BaseFeeGwei: gwei(h.BaseFee),
		Txs: -1,
	}
	if body := s.Chain.Body(h.Hash()); body != nil {
		b.Txs = len(body.Transactions)
	}
	return b
}

// Snapshot computes the current status.
func (s *Service) Snapshot(ctx context.Context) *Snapshot {
	now := time.Now()
	out := &Snapshot{
		Version: s.Version, Mode: s.Mode, Validation: s.Mode != "rpc", ChainID: s.ChainID,
		UptimeSec: int64(now.Sub(s.start).Seconds()), Recent: []Block{},
	}
	s.mu.Lock()
	beacon, beaconErr, clPeers, probe := s.beacon, s.beaconErr, s.clPeers, s.probe
	downSince, lastSlotAt := s.downSince, s.lastSlotAt
	s.mu.Unlock()
	if !downSince.IsZero() {
		out.Consensus.DownSec = int64(now.Sub(downSince).Seconds())
	}
	if beacon != nil && beacon.IsSyncing && !lastSlotAt.IsZero() {
		out.Consensus.StalledSec = int64(now.Sub(lastSlotAt).Seconds())
	}

	head := s.Chain.Latest()
	if s.NetworkHead != nil {
		out.NetworkHead = s.NetworkHead()
	}
	if head != nil {
		b := s.block(head, now)
		out.Head = &b
		out.Gas.BaseFeeGwei = b.BaseFeeGwei
		if out.NetworkHead < b.Number {
			out.NetworkHead = b.Number
		}
		for n := head.Number.Uint64(); len(out.Recent) < recentCount; n-- {
			h := s.Chain.HeaderByNumber(n)
			if h == nil {
				break
			}
			out.Recent = append(out.Recent, s.block(h, now))
			if n == 0 {
				break
			}
		}
		if s.GasPrice != nil {
			if p, err := s.GasPrice(ctx); err == nil {
				out.Gas.GasPriceGwei = gwei(p)
			}
		}
	}
	if f := s.Chain.Finalized(); f != nil {
		out.Finalized = f.Number.Uint64()
	}
	eth, snap := -1, -1
	if s.Peers != nil {
		eth, snap = s.Peers()
		out.P2P = &struct {
			Eth  int `json:"eth"`
			Snap int `json:"snap"`
		}{eth, snap}
	}
	out.Consensus.Reachable = beaconErr == nil && beacon != nil
	if beaconErr != nil {
		out.Consensus.Error = beaconErr.Error()
	}
	if beacon != nil {
		out.Consensus.HeadSlot = beacon.HeadSlot
		out.Consensus.SyncDistance = beacon.SyncDistance
		out.Consensus.IsSyncing = beacon.IsSyncing
		out.Consensus.IsOptimistic = beacon.IsOptimistic
		out.Consensus.Peers = clPeers
	}
	out.Probe = probe
	if s.Cache != nil {
		out.Cache.Bytes, out.Cache.Cap = s.Cache.LogicalBytes(), s.Cache.Cap()
	}
	out.Disk.Execution = uint64(metrics.DiskExecution.Value())
	out.Disk.Consensus = uint64(metrics.DiskWatched.Value())
	out.Disk.Total = out.Disk.Execution + out.Disk.Consensus
	out.Disk.Limit = s.DiskTotalLimit
	hdrs, _, txs := s.Chain.Stats()
	out.Window.Headers, out.Window.Txs, out.Window.LogsFrom = hdrs, txs, s.Chain.LowestBodies()
	if head != nil && head.Number.Uint64() > 120 {
		out.Window.StateFrom = head.Number.Uint64() - 120
	}
	out.Verified.Proofs = metrics.ProofOK.Value()
	out.Verified.ProofsFailed = metrics.ProofFail.Value()
	out.Verified.Headers = metrics.HeaderOK.Value()
	out.Verified.Receipts = metrics.ReceiptOK.Value()
	out.Verified.Codes = metrics.CodeOK.Value()
	hits := metrics.CacheHitCur.Value() + metrics.CacheHitPrev.Value()
	if total := hits + metrics.CacheMiss.Value(); total > 0 {
		out.Verified.CacheHitRatio = float64(hits) / float64(total)
	}
	out.RPC.Requests = metrics.RPCRequests.Value()
	if sum, n := metrics.RPCSeconds.Values(); n > 0 {
		out.RPC.AvgLatencyMs = sum / float64(n) * 1000
	}

	s.evaluate(out, now)
	return out
}

func (s *Service) evaluate(out *Snapshot, now time.Time) {
	add := func(id, label, state, detail string) {
		out.Checks = append(out.Checks, Check{ID: id, Label: label, OK: state == CheckOK, State: state, Detail: detail})
	}
	inGrace := time.Duration(out.UptimeSec)*time.Second < startupGrace
	pendingOr := func(stillWorking bool) string {
		if stillWorking {
			return CheckPending
		}
		return CheckFail
	}

	c := out.Consensus
	clSynced := c.Reachable && !c.IsSyncing && c.SyncDistance <= 2
	down := time.Duration(c.DownSec) * time.Second
	switch {
	case !c.Reachable && (inGrace || down < consensusDownFail):
		add("consensus", "Consensus client synced", CheckPending, "consensus client starting (fresh checkpoint sync)")
	case !c.Reachable:
		add("consensus", "Consensus client synced", CheckFail, fmt.Sprintf("not reachable for %s: %s", down.Round(time.Second), c.Error))
	case !clSynced && time.Duration(c.StalledSec)*time.Second >= syncStallFail:
		add("consensus", "Consensus client synced", CheckFail, fmt.Sprintf("sync stalled at slot %d for %s (%d slots behind)",
			c.HeadSlot, (time.Duration(c.StalledSec) * time.Second).Round(time.Second), c.SyncDistance))
	case !clSynced:
		add("consensus", "Consensus client synced", CheckPending, fmt.Sprintf("syncing: %d slots behind (head slot %d)", c.SyncDistance, c.HeadSlot))
	default:
		add("consensus", "Consensus client synced", CheckOK, fmt.Sprintf("head slot %d, %d peers", c.HeadSlot, c.Peers))
	}

	headOK := false
	switch {
	case out.Head == nil:
		add("head", "Following the chain head", pendingOr(!clSynced || inGrace), "waiting for the first block from the consensus client")
	default:
		age := time.Duration(out.Head.AgeSec) * time.Second
		lag := out.NetworkHead - out.Head.Number
		headOK = age <= maxHeadAge && lag <= maxHeadLag
		detail := fmt.Sprintf("block #%d, %s old", out.Head.Number, age.Round(time.Second))
		if lag > 0 {
			detail += fmt.Sprintf(", %d behind network", lag)
		}
		state := CheckOK
		if !headOK {
			state = pendingOr(!clSynced || age <= headStallFail)
			if state == CheckPending {
				detail = "catching up: " + detail
			} else {
				detail = "no new blocks: " + detail
			}
		}
		add("head", "Following the chain head", state, detail)
	}

	peersOK := true
	if out.P2P != nil {
		peersOK = out.P2P.Snap > 0 && out.P2P.Eth > 0
		detail := fmt.Sprintf("%d peers, %d serving state (snap)", out.P2P.Eth, out.P2P.Snap)
		state := CheckOK
		if !peersOK {
			state = pendingOr(inGrace)
			if state == CheckPending {
				detail = "connecting: " + detail
			}
		}
		add("peers", "Connected to PulseChain peers", state, detail)
	}

	p := out.Probe
	probeOK := p.OK && out.Head != nil && p.Block+probeMaxLag >= out.Head.Number
	// Reads keep retrying at every new block; they only count as failed
	// once they have not succeeded for a while.
	readsRecent := (p.LastOK == 0 && inGrace) || (p.LastOK > 0 && now.Unix()-p.LastOK < int64(probeFailAfter.Seconds()))
	switch {
	case p.At == 0:
		add("state", "Verified state reads working", pendingOr(inGrace || !headOK), "first state read in progress")
	case !p.OK:
		add("state", "Verified state reads working", pendingOr(readsRecent || !headOK), fmt.Sprintf("retrying: read at #%d failed: %s", p.Block, p.Error))
	case !probeOK:
		add("state", "Verified state reads working", CheckPending, fmt.Sprintf("waiting for a read at the new head (last verified at #%d)", p.Block))
	default:
		add("state", "Verified state reads working", CheckOK, fmt.Sprintf("read at #%d verified in %.0f ms", p.Block, p.LatencyMs))
	}

	out.Ready = clSynced && headOK && peersOK && probeOK
	labels := func(state string) string {
		var l []string
		for _, ch := range out.Checks {
			if ch.State == state {
				l = append(l, checkPhrases[ch.ID][state])
			}
		}
		return strings.Join(l, ", ")
	}
	switch {
	case out.Ready:
		out.State, out.Summary = "ready", "Ready: serving verified state and accepting transactions."
	case labels(CheckFail) != "":
		out.State, out.Summary = "degraded", "Not working: "+labels(CheckFail)+"."
	case out.Head == nil || !c.Reachable:
		out.State, out.Summary = "starting", "Starting: waiting for the consensus client."
	case !clSynced || !headOK:
		out.State, out.Summary = "syncing", "Syncing: catching up with the chain head."
	default:
		out.State, out.Summary = "syncing", "Almost ready: "+labels(CheckPending)+"."
	}
}

// checkPhrases describe a check that is not OK, for the summary line.
var checkPhrases = map[string]map[string]string{
	"consensus": {CheckPending: "consensus client syncing", CheckFail: "consensus client not running, unreachable or stalled"},
	"head":      {CheckPending: "catching up with the chain head", CheckFail: "no new blocks from the consensus client"},
	"peers":     {CheckPending: "connecting to PulseChain peers", CheckFail: "no PulseChain peers serving state"},
	"state":     {CheckPending: "waiting for a verified state read", CheckFail: "verified state reads failing"},
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

// Ready reports whether the node is ready (for the readiness probe).
func (s *Service) Ready(ctx context.Context) bool { return s.Snapshot(ctx).Ready }

// ServeStatus serves /api/status.
func (s *Service) ServeStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.Snapshot(r.Context()))
}

// ServeReady serves /ready: 200 when ready, 503 otherwise.
func (s *Service) ServeReady(w http.ResponseWriter, r *http.Request) {
	snap := s.Snapshot(r.Context())
	code := http.StatusOK
	if !snap.Ready {
		code = http.StatusServiceUnavailable
	}
	writeJSON(w, code, map[string]any{"ready": snap.Ready, "state": snap.State, "summary": snap.Summary, "checks": snap.Checks})
}

// ServeDashboard serves the dashboard page.
func ServeDashboard(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(dashboardHTML)
}

// Wrap serves the dashboard, /api/status and /ready on GET requests and
// passes everything else (JSON-RPC POSTs) to rpc. GET / shows the dashboard
// only to browsers, so plain GET health checks still reach the RPC handler.
func (s *Service) Wrap(rpc http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			switch r.URL.Path {
			case "/api/status":
				s.ServeStatus(w, r)
				return
			case "/ready":
				s.ServeReady(w, r)
				return
			case "/dashboard":
				ServeDashboard(w, r)
				return
			case "/":
				if strings.Contains(r.Header.Get("Accept"), "text/html") {
					ServeDashboard(w, r)
					return
				}
			}
		}
		if r.Method == http.MethodPost {
			start := time.Now()
			defer func() {
				metrics.RPCRequests.Inc()
				metrics.RPCSeconds.ObserveSince(start)
			}()
		}
		rpc.ServeHTTP(w, r)
	})
}
