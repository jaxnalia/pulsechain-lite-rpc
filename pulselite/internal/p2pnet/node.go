// Package p2pnet is a minimal devp2p client for the PulseChain network. It
// speaks eth/68 (headers, bodies, receipts, transaction broadcast) and snap/1
// (account/storage range proofs, bytecodes, trie nodes) to ordinary go-pulse
// peers, so state can be retrieved without any RPC provider.
//
// The node is a well-behaved light participant: it serves block headers from
// its recent window and answers every other request with an empty response
// (the protocol's "data not available"). Nothing received is trusted; callers
// verify all data against consensus-provided roots and hashes.
package p2pnet

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/forkid"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/eth/protocols/eth"
	"github.com/ethereum/go-ethereum/eth/protocols/snap"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/p2p/enr"
	"github.com/ethereum/go-ethereum/p2p/nat"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/pulsechainlite/pulselite/internal/metrics"
)

// Config configures the node.
type Config struct {
	DataDir    string
	ListenPort int
	MaxPeers   int
	Bootnodes  []string
	NAT        string
	NetworkID  uint64
	Chain      *params.ChainConfig
	Genesis    *types.Block
	Name       string
	// Head returns the current head header (may be nil before sync).
	Head func() *types.Header
	// ServeHeaders answers remote GetBlockHeaders requests from the window.
	ServeHeaders func(req *eth.GetBlockHeadersRequest) []*types.Header
}

// ErrNoPeers is returned when no suitable peer is connected.
var ErrNoPeers = errors.New("no suitable p2p peers connected")

// errEmpty marks a response that carried no data (peer lacks it).
var errEmpty = errors.New("empty response")

// ethENR is the `eth` node record entry go-pulse advertises in discovery
// (go-ethereum's own type is unexported).
type ethENR struct {
	ForkID forkid.ID
	Rest   []rlp.RawValue `rlp:"tail"`
}

func (ethENR) ENRKey() string { return "eth" }

// A peer that was asked for state evictStrikes times while another peer
// answered first, and has not served state for evictIdle (or since it
// connected), is disconnected so its dial slot goes to a new peer. Peers
// without snap (erigon, reth, some go-pulse versions) are kept only up to
// maxEthOnly, for history and transaction broadcast. Dropped peers are not
// redialled or accepted again for evictBan.
const (
	evictStrikes = 3
	evictIdle    = 2 * time.Minute
	evictBan     = 30 * time.Minute
	maxEthOnly   = 4
)

type peer struct {
	id        string
	p         *p2p.Peer
	connected time.Time
	mu        sync.Mutex
	ethRW     p2p.MsgReadWriter
	snapRW    p2p.MsgReadWriter
	inflite   atomic.Int32
	latency   atomic.Int64 // EWMA, microseconds
	benched   atomic.Int64 // unix nano
	fails     atomic.Int32
	served    atomic.Int64 // unix nano of the last state (snap) response
	strikes   atomic.Int32 // state requests another peer answered first since then
}

func (p *peer) lastServed() time.Time {
	if t := p.served.Load(); t > 0 {
		return time.Unix(0, t)
	}
	return p.connected
}

func (p *peer) rw(snapProto bool) p2p.MsgReadWriter {
	p.mu.Lock()
	defer p.mu.Unlock()
	if snapProto {
		return p.snapRW
	}
	return p.ethRW
}

// Node is the devp2p client.
type Node struct {
	cfg    Config
	srv    *p2p.Server
	filter forkid.Filter

	mu    sync.RWMutex
	peers map[string]*peer

	dialMix *enode.FairMix
	emu     sync.Mutex
	evicted map[enode.ID]time.Time // ban expiry

	reqID   atomic.Uint64
	pmu     sync.Mutex
	pending map[uint64]chan any
}

// New creates the node (not started).
func New(cfg Config) (*Node, error) {
	key, err := loadNodeKey(filepath.Join(cfg.DataDir, "nodekey"))
	if err != nil {
		return nil, err
	}
	var boot []*enode.Node
	for _, url := range cfg.Bootnodes {
		n, err := enode.Parse(enode.ValidSchemes, url)
		if err != nil {
			return nil, fmt.Errorf("bootnode %q: %w", url, err)
		}
		boot = append(boot, n)
	}
	natm, err := nat.Parse(cfg.NAT)
	if err != nil {
		return nil, err
	}
	n := &Node{
		cfg:     cfg,
		filter:  forkid.NewStaticFilter(cfg.Chain, cfg.Genesis),
		peers:   make(map[string]*peer),
		dialMix: enode.NewFairMix(0),
		evicted: make(map[enode.ID]time.Time),
		pending: make(map[uint64]chan any),
	}
	n.srv = &p2p.Server{Config: p2p.Config{
		PrivateKey:     key,
		MaxPeers:       cfg.MaxPeers,
		Name:           cfg.Name,
		BootstrapNodes: boot,
		ListenAddr:     fmt.Sprintf(":%d", cfg.ListenPort),
		NAT:            natm,
		DiscoveryV4:    true,
		NodeDatabase:   filepath.Join(cfg.DataDir, "nodes"),
		Protocols: []p2p.Protocol{
			{Name: eth.ProtocolName, Version: eth.ETH68, Length: 17, Run: n.runEth,
				DialCandidates: n.dialMix, Attributes: []enr.Entry{&ethENR{ForkID: n.status().ForkID}}},
			{Name: snap.ProtocolName, Version: snap.SNAP1, Length: 8, Run: n.runSnap},
		},
		Logger: log.Root(),
	}}
	return n, nil
}

func loadNodeKey(path string) (*ecdsa.PrivateKey, error) {
	if key, err := crypto.LoadECDSA(path); err == nil {
		return key, nil
	}
	key, err := crypto.GenerateKey()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	return key, crypto.SaveECDSA(path, key)
}

// Start starts the p2p server.
func (n *Node) Start() error {
	if err := n.srv.Start(); err != nil {
		return err
	}
	// Dial only nodes whose record advertises a compatible PulseChain fork ID:
	// the discovery DHT is shared with other networks (~90% of the nodes it
	// returns), and checking a record costs a few UDP packets instead of a
	// TCP + RLPx + status handshake. The `snap` record entry is not required:
	// most go-pulse nodes serve snap without advertising it.
	if d := n.srv.DiscoveryV4(); d != nil {
		resolve := func(_ context.Context, nd *enode.Node) *enode.Node {
			if n.isEvicted(nd.ID()) {
				return nil
			}
			nn, err := d.RequestENR(nd)
			if err != nil {
				return nil
			}
			return nn
		}
		it := enode.AsyncFilter(d.RandomNodes(), resolve, 16)
		n.dialMix.AddSource(enode.NewBufferIter(enode.Filter(it, n.dialable), 4))
	}
	log.Info("P2P node started", "enode", n.srv.Self().URLv4(), "maxPeers", n.cfg.MaxPeers)
	go n.reportLoop()
	return nil
}

func (n *Node) dialable(nd *enode.Node) bool {
	var e ethENR
	return nd.Load(&e) == nil && n.filter(e.ForkID) == nil
}

func (n *Node) isEvicted(id enode.ID) bool {
	n.emu.Lock()
	defer n.emu.Unlock()
	return time.Now().Before(n.evicted[id])
}

// ban reports whether id was not banned already.
func (n *Node) ban(id enode.ID) bool {
	n.emu.Lock()
	defer n.emu.Unlock()
	already := time.Now().Before(n.evicted[id])
	n.evicted[id] = time.Now().Add(evictBan)
	if !already {
		metrics.P2PEvicted.Inc()
	}
	return !already
}

func hasSnap(p *p2p.Peer) bool { return p.RunningCap(snap.ProtocolName, []uint{snap.SNAP1}) }

func (n *Node) ethOnlyPeers() int {
	n.mu.RLock()
	defer n.mu.RUnlock()
	c := 0
	for _, pp := range n.peers {
		if !hasSnap(pp.p) {
			c++
		}
	}
	return c
}

// evict disconnects a peer that does not serve state and bans it for a while.
func (n *Node) evict(pp *peer) {
	if !n.ban(pp.p.ID()) {
		return
	}
	log.Debug("Dropping peer that does not serve state", "peer", pp.id[:8], "name", pp.p.Fullname(),
		"connected", time.Since(pp.connected).Round(time.Second))
	pp.p.Disconnect(p2p.DiscUselessPeer)
}

// strikeLosers records a strike against every peer that was asked for state
// before the winner but did not answer first.
func (n *Node) strikeLosers(order []*peer, winner *peer) {
	for _, pp := range order {
		if pp == winner {
			return
		}
		if pp.strikes.Add(1) >= evictStrikes && time.Since(pp.lastServed()) > evictIdle {
			n.evict(pp)
		}
	}
}

// Stop stops the server.
func (n *Node) Stop() { n.srv.Stop() }

// PeerCount returns the number of eth and snap capable peers.
func (n *Node) PeerCount() (ethPeers, snapPeers int) {
	n.mu.RLock()
	defer n.mu.RUnlock()
	for _, p := range n.peers {
		if p.rw(false) != nil {
			ethPeers++
		}
		if p.rw(true) != nil {
			snapPeers++
		}
	}
	return
}

func (n *Node) reportLoop() {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for range t.C {
		n.srv.LocalNode().Set(&ethENR{ForkID: n.status().ForkID}) // no-op unless a fork passed
		now := time.Now()
		n.emu.Lock()
		for id, until := range n.evicted {
			if now.After(until) {
				delete(n.evicted, id)
			}
		}
		n.emu.Unlock()
		e, s := n.PeerCount()
		metrics.P2PPeers.Set(float64(e))
		metrics.P2PSnapPeers.Set(float64(s))
		log.Info("P2P peers", "eth", e, "snap", s, "total", n.srv.PeerCount())
	}
}

func (n *Node) getPeer(p *p2p.Peer) *peer {
	id := p.ID().String()
	n.mu.Lock()
	defer n.mu.Unlock()
	pp := n.peers[id]
	if pp == nil {
		pp = &peer{id: id, p: p, connected: time.Now()}
		n.peers[id] = pp
	}
	return pp
}

func (n *Node) dropProto(pp *peer, snapProto bool) {
	pp.mu.Lock()
	if snapProto {
		pp.snapRW = nil
	} else {
		pp.ethRW = nil
	}
	empty := pp.snapRW == nil && pp.ethRW == nil
	pp.mu.Unlock()
	if empty {
		n.mu.Lock()
		delete(n.peers, pp.id)
		n.mu.Unlock()
	}
}

func (n *Node) status() *eth.StatusPacket68 {
	head := uint64(30_000_000)
	htime := uint64(time.Now().Unix())
	hash := n.cfg.Genesis.Hash()
	if h := n.cfg.Head(); h != nil {
		head, htime, hash = h.Number.Uint64(), h.Time, h.Hash()
	}
	return &eth.StatusPacket68{
		ProtocolVersion: eth.ETH68,
		NetworkID:       n.cfg.NetworkID,
		TD:              new(big.Int),
		Head:            hash,
		Genesis:         n.cfg.Genesis.Hash(),
		ForkID:          forkid.NewID(n.cfg.Chain, n.cfg.Genesis, head, htime),
	}
}

func (n *Node) handshake(rw p2p.MsgReadWriter) error {
	errc := make(chan error, 2)
	go func() { errc <- p2p.Send(rw, eth.StatusMsg, n.status()) }()
	go func() {
		msg, err := rw.ReadMsg()
		if err != nil {
			errc <- err
			return
		}
		defer msg.Discard()
		if msg.Code != eth.StatusMsg {
			errc <- fmt.Errorf("first message is not status (code %d)", msg.Code)
			return
		}
		var st eth.StatusPacket68
		if err := msg.Decode(&st); err != nil {
			errc <- err
			return
		}
		switch {
		case st.NetworkID != n.cfg.NetworkID:
			errc <- fmt.Errorf("network id %d", st.NetworkID)
		case st.Genesis != n.cfg.Genesis.Hash():
			errc <- errors.New("genesis mismatch")
		default:
			errc <- n.filter(st.ForkID)
		}
	}()
	timeout := time.After(5 * time.Second)
	for i := 0; i < 2; i++ {
		select {
		case err := <-errc:
			if err != nil {
				return err
			}
		case <-timeout:
			return errors.New("handshake timeout")
		}
	}
	return nil
}

func (n *Node) runEth(p *p2p.Peer, rw p2p.MsgReadWriter) error {
	if n.isEvicted(p.ID()) {
		return p2p.DiscUselessPeer
	}
	if err := n.handshake(rw); err != nil {
		log.Trace("eth handshake failed", "peer", p.ID(), "err", err)
		return err
	}
	if !hasSnap(p) && n.ethOnlyPeers() >= maxEthOnly {
		n.ban(p.ID())
		log.Debug("Dropping peer without snap", "peer", p.ID(), "name", p.Fullname())
		return p2p.DiscUselessPeer
	}
	pp := n.getPeer(p)
	pp.mu.Lock()
	pp.ethRW = rw
	pp.mu.Unlock()
	defer n.dropProto(pp, false)
	log.Debug("eth peer connected", "peer", p.ID(), "name", p.Fullname())
	for {
		msg, err := rw.ReadMsg()
		if err != nil {
			return err
		}
		if err := n.handleEth(rw, msg); err != nil {
			msg.Discard()
			return err
		}
		msg.Discard()
	}
}

func (n *Node) handleEth(rw p2p.MsgReadWriter, msg p2p.Msg) error {
	switch msg.Code {
	case eth.BlockHeadersMsg:
		var res eth.BlockHeadersPacket
		if err := msg.Decode(&res); err != nil {
			return err
		}
		n.deliver(res.RequestId, []*types.Header(res.BlockHeadersRequest))
	case eth.BlockBodiesMsg:
		var res eth.BlockBodiesPacket
		if err := msg.Decode(&res); err != nil {
			return err
		}
		n.deliver(res.RequestId, []*eth.BlockBody(res.BlockBodiesResponse))
	case eth.ReceiptsMsg:
		var res struct {
			RequestId uint64
			List      []types.Receipts
		}
		if err := msg.Decode(&res); err != nil {
			return err
		}
		n.deliver(res.RequestId, res.List)
	case eth.GetBlockHeadersMsg:
		var req eth.GetBlockHeadersPacket
		if err := msg.Decode(&req); err != nil {
			return err
		}
		var headers []*types.Header
		if n.cfg.ServeHeaders != nil && req.GetBlockHeadersRequest != nil {
			headers = n.cfg.ServeHeaders(req.GetBlockHeadersRequest)
		}
		return p2p.Send(rw, eth.BlockHeadersMsg, &eth.BlockHeadersPacket{RequestId: req.RequestId, BlockHeadersRequest: headers})
	case eth.GetBlockBodiesMsg:
		var req eth.GetBlockBodiesPacket
		if err := msg.Decode(&req); err != nil {
			return err
		}
		return p2p.Send(rw, eth.BlockBodiesMsg, &eth.BlockBodiesPacket{RequestId: req.RequestId})
	case eth.GetReceiptsMsg:
		var req eth.GetReceiptsPacket
		if err := msg.Decode(&req); err != nil {
			return err
		}
		return p2p.Send(rw, eth.ReceiptsMsg, &struct {
			RequestId uint64
			List      []types.Receipts
		}{RequestId: req.RequestId})
	case eth.GetPooledTransactionsMsg:
		var req eth.GetPooledTransactionsPacket
		if err := msg.Decode(&req); err != nil {
			return err
		}
		return p2p.Send(rw, eth.PooledTransactionsMsg, &eth.PooledTransactionsPacket{RequestId: req.RequestId})
	case eth.StatusMsg:
		return errors.New("unexpected status message")
	}
	// Announcements, broadcasts and anything else are ignored.
	return nil
}

func (n *Node) runSnap(p *p2p.Peer, rw p2p.MsgReadWriter) error {
	pp := n.getPeer(p)
	pp.mu.Lock()
	pp.snapRW = rw
	pp.mu.Unlock()
	defer n.dropProto(pp, true)
	for {
		msg, err := rw.ReadMsg()
		if err != nil {
			return err
		}
		if err := n.handleSnap(rw, msg); err != nil {
			msg.Discard()
			return err
		}
		msg.Discard()
	}
}

func (n *Node) handleSnap(rw p2p.MsgReadWriter, msg p2p.Msg) error {
	switch msg.Code {
	case snap.AccountRangeMsg:
		res := new(snap.AccountRangePacket)
		if err := msg.Decode(res); err != nil {
			return err
		}
		n.deliver(res.ID, res)
	case snap.StorageRangesMsg:
		res := new(snap.StorageRangesPacket)
		if err := msg.Decode(res); err != nil {
			return err
		}
		n.deliver(res.ID, res)
	case snap.ByteCodesMsg:
		res := new(snap.ByteCodesPacket)
		if err := msg.Decode(res); err != nil {
			return err
		}
		n.deliver(res.ID, res)
	case snap.TrieNodesMsg:
		res := new(snap.TrieNodesPacket)
		if err := msg.Decode(res); err != nil {
			return err
		}
		n.deliver(res.ID, res)
	case snap.GetAccountRangeMsg:
		var req snap.GetAccountRangePacket
		if err := msg.Decode(&req); err != nil {
			return err
		}
		return p2p.Send(rw, snap.AccountRangeMsg, &snap.AccountRangePacket{ID: req.ID})
	case snap.GetStorageRangesMsg:
		var req snap.GetStorageRangesPacket
		if err := msg.Decode(&req); err != nil {
			return err
		}
		return p2p.Send(rw, snap.StorageRangesMsg, &snap.StorageRangesPacket{ID: req.ID})
	case snap.GetByteCodesMsg:
		var req snap.GetByteCodesPacket
		if err := msg.Decode(&req); err != nil {
			return err
		}
		return p2p.Send(rw, snap.ByteCodesMsg, &snap.ByteCodesPacket{ID: req.ID})
	case snap.GetTrieNodesMsg:
		var req snap.GetTrieNodesPacket
		if err := msg.Decode(&req); err != nil {
			return err
		}
		return p2p.Send(rw, snap.TrieNodesMsg, &snap.TrieNodesPacket{ID: req.ID})
	}
	return nil
}

func (n *Node) deliver(id uint64, v any) {
	n.pmu.Lock()
	ch := n.pending[id]
	delete(n.pending, id)
	n.pmu.Unlock()
	if ch != nil {
		ch <- v
	}
}

// candidates returns usable peers ordered by load and latency.
func (n *Node) candidates(snapProto bool) []*peer {
	now := time.Now().UnixNano()
	n.mu.RLock()
	var list, benched []*peer
	for _, p := range n.peers {
		if p.rw(snapProto) == nil || p.rw(false) == nil {
			continue
		}
		if p.benched.Load() > now {
			benched = append(benched, p)
			continue
		}
		list = append(list, p)
	}
	n.mu.RUnlock()
	if len(list) == 0 {
		list = benched // better a slow peer than none
	}
	sort.Slice(list, func(i, j int) bool {
		li, lj := list[i].inflite.Load(), list[j].inflite.Load()
		if li != lj {
			return li < lj
		}
		return rankLatency(list[i]) < rankLatency(list[j])
	})
	return list
}

// unknownLatency ranks peers without a latency sample behind proven fast
// peers but ahead of slow ones (some fresh peers never answer snap requests).
const unknownLatency = int64(400 * time.Millisecond / time.Microsecond)

func rankLatency(p *peer) int64 {
	if l := p.latency.Load(); l > 0 {
		return l
	}
	return unknownLatency
}

var errAbandoned = errors.New("abandoned (another peer answered first)")

const (
	snapHedge  = 500 * time.Millisecond
	retryDelay = 750 * time.Millisecond
)

// attempt sends one request to one peer and waits for a checked response.
func (n *Node) attempt(ctx context.Context, kind string, pp *peer, snapProto bool, timeout time.Duration,
	send func(rw p2p.MsgReadWriter, id uint64) error, check func(any) error) (any, error) {
	rw := pp.rw(snapProto)
	if rw == nil {
		return nil, errors.New("peer disconnected")
	}
	id := n.reqID.Add(1)
	ch := make(chan any, 1)
	n.pmu.Lock()
	n.pending[id] = ch
	n.pmu.Unlock()
	pp.inflite.Add(1)
	start := time.Now()
	reason := ""
	err := send(rw, id)
	var res any
	if err != nil {
		reason = "send"
	} else {
		select {
		case res = <-ch:
		case <-time.After(timeout):
			err, reason = errors.New("request timeout"), "timeout"
		case <-ctx.Done():
			err = errAbandoned
		}
	}
	pp.inflite.Add(-1)
	if res == nil {
		n.pmu.Lock()
		delete(n.pending, id)
		n.pmu.Unlock()
	}
	if err == errAbandoned {
		return nil, err // not the peer's fault: no metrics, no penalty
	}
	if err == nil {
		if err = check(res); err != nil {
			reason = "invalid"
			if errors.Is(err, errEmpty) {
				reason = "empty"
			}
		}
	}
	if err == nil {
		d := time.Since(start)
		us := d.Microseconds()
		if old := pp.latency.Load(); old == 0 {
			pp.latency.Store(us)
		} else {
			pp.latency.Store((old*7 + us) / 8)
		}
		pp.fails.Store(0)
		if snapProto {
			pp.served.Store(time.Now().UnixNano())
			pp.strikes.Store(0)
		}
		metrics.P2PRequests("ok").Inc()
		metrics.P2PKind(kind, "ok").Inc()
		metrics.P2PLatency(kind).Observe(d.Seconds())
		return res, nil
	}
	metrics.P2PRequests("fail").Inc()
	metrics.P2PKind(kind, reason).Inc()
	// Penalise slow/empty peers in the ordering and bench repeat offenders.
	pp.latency.Add(int64(timeout / time.Microsecond / 4))
	if f := pp.fails.Add(1); f >= 5 {
		pp.benched.Store(time.Now().Add(time.Duration(min(f-4, 6)) * 2 * time.Second).UnixNano())
	}
	return nil, fmt.Errorf("peer %s: %w", pp.id[:8], err)
}

// request sends a request to up to attempts distinct peers until a response
// passes check. If a peer has not answered within hedge, the request is
// also sent to the next best peer (hedged request) and the first valid
// answer wins, so a single slow or state-less peer does not stall callers.
func (n *Node) request(parent context.Context, kind string, snapProto bool, timeout time.Duration, attempts int,
	send func(rw p2p.MsgReadWriter, id uint64) error, check func(any) error) (any, error) {
	// Losing hedged attempts are cancelled as soon as one attempt succeeds.
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	type result struct {
		pp  *peer
		v   any
		err error
	}
	results := make(chan result, attempts)
	tried := map[string]bool{}
	var order []*peer // launch order of the current round
	launched := 0
	launch := func() bool {
		for _, c := range n.candidates(snapProto) {
			if !tried[c.id] {
				tried[c.id] = true
				launched++
				order = append(order, c)
				go func(pp *peer) {
					v, err := n.attempt(ctx, kind, pp, snapProto, timeout, send, check)
					results <- result{pp, v, err}
				}(c)
				return true
			}
		}
		return false
	}
	for wait := 0; !launch(); wait++ {
		if wait >= 3 {
			return nil, ErrNoPeers
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	hedge := timeout / 4
	if snapProto {
		hedge = min(hedge, snapHedge) // small state requests: a healthy peer answers in ~150 ms
	}
	timer := time.NewTimer(hedge)
	defer timer.Stop()
	outstanding := 1
	retried := false
	var lastErr error = ErrNoPeers
	for outstanding > 0 {
		select {
		case r := <-results:
			outstanding--
			if r.err == nil {
				if snapProto {
					n.strikeLosers(order, r.pp)
				}
				return r.v, nil
			}
			lastErr = r.err
			if launched < attempts && launch() {
				outstanding++
			} else if outstanding == 0 && !retried {
				// Every candidate failed, typically because peers have not
				// imported the newest block yet: give them a moment.
				retried = true
				select {
				case <-time.After(retryDelay):
				case <-ctx.Done():
					return nil, ctx.Err()
				}
				clear(tried)
				order = order[:0]
				launched = 0
				if launch() {
					outstanding++
				}
			}
		case <-timer.C:
			if launched < attempts && launch() {
				outstanding++
			}
			timer.Reset(hedge)
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return nil, lastErr
}

// AccountRange requests a single-key account range proof.
func (n *Node) AccountRange(ctx context.Context, root, key common.Hash) (*snap.AccountRangePacket, error) {
	res, err := n.request(ctx, "account", true, 4*time.Second, 6, func(rw p2p.MsgReadWriter, id uint64) error {
		return p2p.Send(rw, snap.GetAccountRangeMsg, &snap.GetAccountRangePacket{ID: id, Root: root, Origin: key, Limit: key, Bytes: 1})
	}, func(v any) error {
		r := v.(*snap.AccountRangePacket)
		if len(r.Accounts) == 0 && len(r.Proof) == 0 {
			return errEmpty
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return res.(*snap.AccountRangePacket), nil
}

// StorageRange requests a single-slot storage range proof for one account.
func (n *Node) StorageRange(ctx context.Context, root, account, slot common.Hash) (*snap.StorageRangesPacket, error) {
	res, err := n.request(ctx, "storage", true, 4*time.Second, 6, func(rw p2p.MsgReadWriter, id uint64) error {
		return p2p.Send(rw, snap.GetStorageRangesMsg, &snap.GetStorageRangesPacket{
			ID: id, Root: root, Accounts: []common.Hash{account}, Origin: slot.Bytes(), Limit: slot.Bytes(), Bytes: 1,
		})
	}, func(v any) error {
		r := v.(*snap.StorageRangesPacket)
		if len(r.Slots) == 0 && len(r.Proof) == 0 {
			return errEmpty
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return res.(*snap.StorageRangesPacket), nil
}

// ByteCodes requests contract code by hash.
func (n *Node) ByteCodes(ctx context.Context, hashes []common.Hash) ([][]byte, error) {
	res, err := n.request(ctx, "codes", true, 5*time.Second, 6, func(rw p2p.MsgReadWriter, id uint64) error {
		return p2p.Send(rw, snap.GetByteCodesMsg, &snap.GetByteCodesPacket{ID: id, Hashes: hashes, Bytes: 4 << 20})
	}, func(v any) error {
		if len(v.(*snap.ByteCodesPacket).Codes) == 0 {
			return errEmpty
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return res.(*snap.ByteCodesPacket).Codes, nil
}

// TrieNodes requests trie nodes by path.
func (n *Node) TrieNodes(ctx context.Context, root common.Hash, paths []snap.TrieNodePathSet) ([][]byte, error) {
	res, err := n.request(ctx, "trienodes", true, 5*time.Second, 6, func(rw p2p.MsgReadWriter, id uint64) error {
		return p2p.Send(rw, snap.GetTrieNodesMsg, &snap.GetTrieNodesPacket{ID: id, Root: root, Paths: paths, Bytes: 2 << 20})
	}, func(v any) error {
		if len(v.(*snap.TrieNodesPacket).Nodes) == 0 {
			return errEmpty
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return res.(*snap.TrieNodesPacket).Nodes, nil
}

// Headers requests headers; check validates the (untrusted) result.
func (n *Node) Headers(ctx context.Context, req *eth.GetBlockHeadersRequest, check func([]*types.Header) error) ([]*types.Header, error) {
	res, err := n.request(ctx, "headers", false, 8*time.Second, 6, func(rw p2p.MsgReadWriter, id uint64) error {
		return p2p.Send(rw, eth.GetBlockHeadersMsg, &eth.GetBlockHeadersPacket{RequestId: id, GetBlockHeadersRequest: req})
	}, func(v any) error {
		hs := v.([]*types.Header)
		if len(hs) == 0 {
			return errEmpty
		}
		return check(hs)
	})
	if err != nil {
		return nil, err
	}
	return res.([]*types.Header), nil
}

// Bodies requests block bodies; check validates the result.
func (n *Node) Bodies(ctx context.Context, hashes []common.Hash, check func([]*eth.BlockBody) error) ([]*eth.BlockBody, error) {
	res, err := n.request(ctx, "bodies", false, 8*time.Second, 6, func(rw p2p.MsgReadWriter, id uint64) error {
		return p2p.Send(rw, eth.GetBlockBodiesMsg, &eth.GetBlockBodiesPacket{RequestId: id, GetBlockBodiesRequest: hashes})
	}, func(v any) error {
		bs := v.([]*eth.BlockBody)
		if len(bs) == 0 {
			return errEmpty
		}
		return check(bs)
	})
	if err != nil {
		return nil, err
	}
	return res.([]*eth.BlockBody), nil
}

// Receipts requests block receipts; check validates the result.
func (n *Node) Receipts(ctx context.Context, hashes []common.Hash, check func([]types.Receipts) error) ([]types.Receipts, error) {
	res, err := n.request(ctx, "receipts", false, 8*time.Second, 6, func(rw p2p.MsgReadWriter, id uint64) error {
		return p2p.Send(rw, eth.GetReceiptsMsg, &eth.GetReceiptsPacket{RequestId: id, GetReceiptsRequest: hashes})
	}, func(v any) error {
		rs := v.([]types.Receipts)
		if len(rs) == 0 {
			return errEmpty
		}
		return check(rs)
	})
	if err != nil {
		return nil, err
	}
	return res.([]types.Receipts), nil
}

// BroadcastTx sends a transaction to up to 8 peers. It returns the number of
// peers the transaction was sent to.
func (n *Node) BroadcastTx(tx *types.Transaction) int {
	sent := 0
	for _, p := range n.candidates(false) {
		if sent >= 8 {
			break
		}
		if rw := p.rw(false); rw != nil && p2p.Send(rw, eth.TransactionsMsg, types.Transactions{tx}) == nil {
			sent++
		}
	}
	return sent
}
