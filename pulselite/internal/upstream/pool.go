// Package upstream talks to untrusted JSON-RPC endpoints. Nothing returned by
// an upstream is trusted: callers verify every proof, code blob, header and
// receipt against consensus-provided roots before use.
package upstream

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/pulsechainlite/pulselite/internal/metrics"
)

// Endpoint is one upstream RPC server.
type Endpoint struct {
	URL      string
	name     string
	client   *rpc.Client
	proofs   atomic.Bool  // serves historical eth_getProof (geth-style)
	failures atomic.Int32 // consecutive failures
	latency  atomic.Int64 // EWMA in microseconds
	disabled atomic.Int64 // unix nano until which the endpoint is benched
}

// Name is a short label for metrics/logs.
func (e *Endpoint) Name() string { return e.name }

func (e *Endpoint) healthy() bool { return time.Now().UnixNano() >= e.disabled.Load() }

func (e *Endpoint) record(start time.Time, err error) {
	if err != nil {
		metrics.Upstream(e.name, "error").Inc()
		n := e.failures.Add(1)
		if n >= 3 {
			backoff := time.Duration(min(int64(n), 10)) * 3 * time.Second
			e.disabled.Store(time.Now().Add(backoff).UnixNano())
		}
		return
	}
	metrics.Upstream(e.name, "ok").Inc()
	e.failures.Store(0)
	us := time.Since(start).Microseconds()
	old := e.latency.Load()
	if old == 0 {
		e.latency.Store(us)
	} else {
		e.latency.Store((old*7 + us) / 8)
	}
}

// Pool distributes requests over upstream endpoints with failover.
type Pool struct {
	eps       []*Endpoint
	rr        atomic.Uint64
	BatchSize int
	Parallel  int
	Timeout   time.Duration
}

// NewPool dials all endpoints (dialing HTTP is lazy, so this does not block).
func NewPool(urls []string) (*Pool, error) {
	p := &Pool{BatchSize: 20, Parallel: 16, Timeout: 8 * time.Second}
	for i, u := range urls {
		name := hostOf(u)
		if name == "" {
			name = fmt.Sprintf("upstream-%d", i)
		}
		c, err := rpc.DialOptions(context.Background(), u)
		if err != nil {
			return nil, sanitize(u, name, err)
		}
		e := &Endpoint{URL: u, name: name, client: c}
		e.proofs.Store(true) // optimistic until Probe says otherwise
		p.eps = append(p.eps, e)
	}
	if len(p.eps) == 0 {
		return nil, errors.New("no upstreams configured")
	}
	return p, nil
}

// NewPoolFromClients builds a pool over pre-made clients (tests).
func NewPoolFromClients(clients map[string]*rpc.Client) *Pool {
	p := &Pool{BatchSize: 20, Parallel: 8, Timeout: 20 * time.Second}
	names := make([]string, 0, len(clients))
	for n := range clients {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		e := &Endpoint{URL: n, name: n, client: clients[n]}
		e.proofs.Store(true)
		p.eps = append(p.eps, e)
	}
	return p
}

// Endpoints returns the configured endpoints.
func (p *Pool) Endpoints() []*Endpoint { return p.eps }

// Probe checks which endpoints serve historical eth_getProof. Erigon 2 style
// endpoints only answer for "latest" and are kept for non-proof traffic.
func (p *Pool) Probe(ctx context.Context) {
	var wg sync.WaitGroup
	for _, e := range p.eps {
		wg.Add(1)
		go func(e *Endpoint) {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			var head hexutil.Uint64
			if err := e.client.CallContext(cctx, &head, "eth_blockNumber"); err != nil {
				log.Warn("Upstream unreachable", "endpoint", e.name, "err", e.sanitize(err))
				e.record(time.Now(), err)
				return
			}
			var version string
			e.client.CallContext(cctx, &version, "web3_clientVersion")
			// Erigon 2 only serves eth_getProof reliably at "latest" and stalls
			// on older blocks, so it is kept for non-proof traffic only.
			var err error
			if strings.Contains(strings.ToLower(version), "erigon") {
				err = errors.New("erigon: no historical proofs")
			} else {
				var res map[string]any
				block := hexutil.Uint64(uint64(head) - 64).String()
				pctx, pcancel := context.WithTimeout(ctx, 5*time.Second)
				err = e.client.CallContext(pctx, &res, "eth_getProof", "0x0000000000000000000000000000000000000000",
					[]string{"0x0000000000000000000000000000000000000000000000000000000000000000"}, block)
				pcancel()
			}
			e.proofs.Store(err == nil)
			log.Info("Upstream probed", "endpoint", e.name, "client", version, "head", uint64(head), "historicalProofs", err == nil)
		}(e)
	}
	wg.Wait()
	any := false
	for _, e := range p.eps {
		any = any || e.proofs.Load()
	}
	if !any {
		log.Error("No upstream serves historical eth_getProof; state queries will fail")
	}
}

// ordered returns endpoints in preference order (healthy first, then by latency).
func (p *Pool) ordered(needProofs bool) []*Endpoint {
	var list []*Endpoint
	for _, e := range p.eps {
		if needProofs && !e.proofs.Load() {
			continue
		}
		list = append(list, e)
	}
	start := int(p.rr.Add(1))
	n := len(list)
	if n == 0 {
		return nil
	}
	rot := make([]*Endpoint, n)
	for i := range list {
		rot[i] = list[(start+i)%n]
	}
	sort.SliceStable(rot, func(i, j int) bool {
		hi, hj := rot[i].healthy(), rot[j].healthy()
		if hi != hj {
			return hi
		}
		return false
	})
	return rot
}

// Call performs a single request with failover across endpoints.
func (p *Pool) Call(ctx context.Context, result any, needProofs bool, method string, args ...any) error {
	eps := p.ordered(needProofs)
	if len(eps) == 0 {
		return errors.New("no suitable upstream endpoint")
	}
	var lastErr error
	for _, e := range eps {
		cctx, cancel := context.WithTimeout(ctx, p.Timeout)
		start := time.Now()
		err := e.client.CallContext(cctx, result, method, args...)
		cancel()
		e.record(start, transportErr(err))
		if err == nil {
			return nil
		}
		lastErr = e.sanitize(err)
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	return lastErr
}

// Broadcast sends the request to every endpoint and returns the first success.
func (p *Pool) Broadcast(ctx context.Context, result any, method string, args ...any) error {
	var lastErr error
	ok := false
	for _, e := range p.eps {
		cctx, cancel := context.WithTimeout(ctx, p.Timeout)
		start := time.Now()
		err := e.client.CallContext(cctx, result, method, args...)
		cancel()
		e.record(start, transportErr(err))
		if err == nil {
			ok = true
		} else {
			lastErr = e.sanitize(err)
		}
	}
	if ok {
		return nil
	}
	return lastErr
}

// Batch executes the elements in parallel chunks. Elements that fail (either
// transport errors or per-element errors such as "missing trie node" on a
// lagging endpoint) are retried on the remaining endpoints.
func (p *Pool) Batch(ctx context.Context, needProofs bool, elems []rpc.BatchElem) error {
	if len(elems) == 0 {
		return nil
	}
	eps := p.ordered(needProofs)
	if len(eps) == 0 {
		return errors.New("no suitable upstream endpoint")
	}
	pending := make([]int, len(elems))
	for i := range pending {
		pending[i] = i
	}
	for attempt := 0; attempt < len(eps) && len(pending) > 0; attempt++ {
		var (
			mu     sync.Mutex
			failed []int
			wg     sync.WaitGroup
			sem    = make(chan struct{}, p.Parallel)
		)
		for c := 0; c*p.BatchSize < len(pending); c++ {
			lo, hi := c*p.BatchSize, min((c+1)*p.BatchSize, len(pending))
			chunk := pending[lo:hi]
			e := eps[(attempt+c)%len(eps)]
			wg.Add(1)
			sem <- struct{}{}
			go func(e *Endpoint, chunk []int) {
				defer wg.Done()
				defer func() { <-sem }()
				batch := make([]rpc.BatchElem, len(chunk))
				for i, idx := range chunk {
					batch[i] = elems[idx]
					batch[i].Error = nil
				}
				cctx, cancel := context.WithTimeout(ctx, p.Timeout)
				start := time.Now()
				err := e.client.BatchCallContext(cctx, batch)
				cancel()
				e.record(start, err)
				err = e.sanitize(err)
				mu.Lock()
				defer mu.Unlock()
				for i, idx := range chunk {
					elems[idx].Error = e.sanitize(batch[i].Error)
					if err != nil {
						elems[idx].Error = err
					}
					if elems[idx].Error != nil {
						failed = append(failed, idx)
					}
				}
			}(e, chunk)
		}
		wg.Wait()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		sort.Ints(failed)
		pending = failed
	}
	if len(pending) > 0 {
		return fmt.Errorf("%d/%d upstream requests failed, first: %v", len(pending), len(elems), elems[pending[0]].Error)
	}
	return nil
}

// transportErr filters out JSON-RPC application errors (which say nothing
// about endpoint health) so that only connectivity problems bench endpoints.
func transportErr(err error) error {
	if err == nil {
		return nil
	}
	var rpcErr rpc.Error
	if errors.As(err, &rpcErr) {
		return nil
	}
	if strings.Contains(err.Error(), "not found") {
		return nil
	}
	return err
}
