// Package metrics is a minimal Prometheus text-format registry for counters,
// gauges and summaries (sum + count).
package metrics

import (
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type kind int

const (
	kindCounter kind = iota
	kindGauge
	kindSummary
)

// Counter is a monotonically increasing value.
type Counter struct{ v atomic.Uint64 }

func (c *Counter) Inc()          { c.v.Add(1) }
func (c *Counter) Add(n uint64)  { c.v.Add(n) }
func (c *Counter) Value() uint64 { return c.v.Load() }

// Gauge is a value that can go up and down.
type Gauge struct{ v atomic.Uint64 }

func (g *Gauge) Set(f float64)  { g.v.Store(math.Float64bits(f)) }
func (g *Gauge) Value() float64 { return math.Float64frombits(g.v.Load()) }

// Summary tracks the sum and count of observations.
type Summary struct {
	mu    sync.Mutex
	sum   float64
	count uint64
}

func (s *Summary) Observe(f float64) {
	s.mu.Lock()
	s.sum += f
	s.count++
	s.mu.Unlock()
}

// ObserveSince records the seconds elapsed since start.
func (s *Summary) ObserveSince(start time.Time) { s.Observe(time.Since(start).Seconds()) }

// Values returns the sum and count of observations.
func (s *Summary) Values() (float64, uint64) { return s.values() }

func (s *Summary) values() (float64, uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sum, s.count
}

type entry struct {
	name   string
	labels string
	help   string
	kind   kind
	c      *Counter
	g      *Gauge
	s      *Summary
}

var (
	mu      sync.Mutex
	entries = map[string]*entry{}
)

func key(name string, labels []string) (string, string) {
	if len(labels)%2 != 0 {
		panic("metrics: odd label list")
	}
	var parts []string
	for i := 0; i < len(labels); i += 2 {
		parts = append(parts, fmt.Sprintf("%s=%q", labels[i], labels[i+1]))
	}
	l := strings.Join(parts, ",")
	return name + "{" + l + "}", l
}

func get(name, help string, k kind, labels []string) *entry {
	id, l := key(name, labels)
	mu.Lock()
	defer mu.Unlock()
	if e, ok := entries[id]; ok {
		return e
	}
	e := &entry{name: name, labels: l, help: help, kind: k}
	switch k {
	case kindCounter:
		e.c = new(Counter)
	case kindGauge:
		e.g = new(Gauge)
	case kindSummary:
		e.s = new(Summary)
	}
	entries[id] = e
	return e
}

// NewCounter returns (creating if needed) the counter with the given labels.
func NewCounter(name, help string, labels ...string) *Counter {
	return get(name, help, kindCounter, labels).c
}

// NewGauge returns (creating if needed) the gauge with the given labels.
func NewGauge(name, help string, labels ...string) *Gauge {
	return get(name, help, kindGauge, labels).g
}

// NewSummary returns (creating if needed) the summary with the given labels.
func NewSummary(name, help string, labels ...string) *Summary {
	return get(name, help, kindSummary, labels).s
}

// Write renders all metrics in Prometheus text exposition format.
func Write(w io.Writer) {
	mu.Lock()
	list := make([]*entry, 0, len(entries))
	for _, e := range entries {
		list = append(list, e)
	}
	mu.Unlock()
	sort.Slice(list, func(i, j int) bool {
		if list[i].name != list[j].name {
			return list[i].name < list[j].name
		}
		return list[i].labels < list[j].labels
	})
	seen := map[string]bool{}
	for _, e := range list {
		if !seen[e.name] {
			seen[e.name] = true
			typ := map[kind]string{kindCounter: "counter", kindGauge: "gauge", kindSummary: "summary"}[e.kind]
			fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", e.name, e.help, e.name, typ)
		}
		lbl := ""
		if e.labels != "" {
			lbl = "{" + e.labels + "}"
		}
		switch e.kind {
		case kindCounter:
			fmt.Fprintf(w, "%s%s %d\n", e.name, lbl, e.c.Value())
		case kindGauge:
			fmt.Fprintf(w, "%s%s %g\n", e.name, lbl, e.g.Value())
		case kindSummary:
			sum, count := e.s.values()
			fmt.Fprintf(w, "%s_sum%s %g\n%s_count%s %d\n", e.name, lbl, sum, e.name, lbl, count)
		}
	}
}

// Handler serves the registry over HTTP.
func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		Write(w)
	})
}

// Shared metrics used across packages.
var (
	CacheHitCur     = NewCounter("pulselite_cache_hits_total", "Verified cache lookups served locally", "gen", "current")
	CacheHitPrev    = NewCounter("pulselite_cache_hits_total", "Verified cache lookups served locally", "gen", "previous")
	CacheMiss       = NewCounter("pulselite_cache_misses_total", "Verified cache lookups that missed")
	CacheBytes      = NewGauge("pulselite_cache_bytes", "Logical bytes written to the cache generations")
	CacheRotations  = NewCounter("pulselite_cache_rotations_total", "Cache generation rotations (evictions)")
	ProofOK         = NewCounter("pulselite_proof_verifications_total", "Merkle proof verifications", "result", "ok")
	ProofFail       = NewCounter("pulselite_proof_verifications_total", "Merkle proof verifications", "result", "fail")
	CodeOK          = NewCounter("pulselite_code_verifications_total", "Contract code hash verifications", "result", "ok")
	CodeFail        = NewCounter("pulselite_code_verifications_total", "Contract code hash verifications", "result", "fail")
	HeaderOK        = NewCounter("pulselite_header_verifications_total", "Header hash/linkage verifications", "result", "ok")
	HeaderFail      = NewCounter("pulselite_header_verifications_total", "Header hash/linkage verifications", "result", "fail")
	ReceiptOK       = NewCounter("pulselite_receipt_verifications_total", "Receipt root verifications", "result", "ok")
	ReceiptFail     = NewCounter("pulselite_receipt_verifications_total", "Receipt root verifications", "result", "fail")
	ExecRounds      = NewSummary("pulselite_exec_rounds", "Speculative fetch/execute rounds per block")
	ExecSeconds     = NewSummary("pulselite_exec_seconds", "Wall time to validate a block")
	ExecFetches     = NewSummary("pulselite_exec_fetched_items", "Items fetched from upstream per validated block")
	SiblingSearches = NewCounter("pulselite_sibling_searches_total", "Collapse-sibling key searches", "result", "ok")
	SiblingFailed   = NewCounter("pulselite_sibling_searches_total", "Collapse-sibling key searches", "result", "fail")
	HeadNumber      = NewGauge("pulselite_head_block", "Current head block number")
	ValidatedNumber = NewGauge("pulselite_validated_block", "Highest block validated by local execution")
	Reorgs          = NewCounter("pulselite_reorgs_total", "Canonical chain reorganisations")
	RPCUnverified   = NewCounter("pulselite_unverified_forwards_total", "Engine-endpoint history queries forwarded upstream without verification")
	DiskExecution   = NewGauge("pulselite_disk_bytes", "Measured disk usage", "component", "execution")
	DiskWatched     = NewGauge("pulselite_disk_bytes", "Measured disk usage", "component", "watched")
	DiskTotal       = NewGauge("pulselite_disk_bytes", "Measured disk usage", "component", "total")
)

var (
	P2PPeers     = NewGauge("pulselite_p2p_peers", "Connected eth peers")
	P2PSnapPeers = NewGauge("pulselite_p2p_snap_peers", "Connected peers serving snap")
	P2PEvicted   = NewCounter("pulselite_p2p_evicted_total", "Peers disconnected for not serving state")
	DepositScan  = NewGauge("pulselite_deposit_scan_progress", "Deposit history scan progress (0..1)")
	DepositLogs  = NewGauge("pulselite_deposit_logs", "Deposit contract logs held locally")
	RPCRequests  = NewCounter("pulselite_rpc_http_requests_total", "Public JSON-RPC HTTP requests (a batch counts once)")
	RPCSeconds   = NewSummary("pulselite_rpc_http_seconds", "Public JSON-RPC HTTP request latency")
)

// P2PKind counts p2p requests by request kind and outcome
// (ok, timeout, empty, invalid, send).
func P2PKind(kind, result string) *Counter {
	return NewCounter("pulselite_p2p_requests_by_kind_total", "devp2p requests by kind and outcome", "kind", kind, "result", result)
}

// P2PLatency tracks successful p2p request latency by kind.
func P2PLatency(kind string) *Summary {
	return NewSummary("pulselite_p2p_request_seconds", "Successful devp2p request latency", "kind", kind)
}

// P2PRequests counts p2p requests by outcome.
func P2PRequests(result string) *Counter {
	return NewCounter("pulselite_p2p_requests_total", "devp2p requests", "result", result)
}

// PayloadStatus counts newPayload results by status.
func PayloadStatus(status string) *Counter {
	return NewCounter("pulselite_newpayload_total", "engine_newPayload results", "status", status)
}

// Upstream counts upstream requests by endpoint and outcome.
func Upstream(endpoint, result string) *Counter {
	return NewCounter("pulselite_upstream_requests_total", "Upstream RPC requests", "endpoint", endpoint, "result", result)
}
