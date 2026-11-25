package p2pnet

import (
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/forkid"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/p2p/enode"
	"github.com/ethereum/go-ethereum/p2p/enr"
	"github.com/ethereum/go-ethereum/params"
)

func testNode() *Node {
	genesis := core.DefaultPulseChainGenesisBlock().ToBlock()
	return &Node{
		cfg:     Config{Chain: params.PulseChainConfig, Genesis: genesis, Head: func() *types.Header { return nil }},
		filter:  forkid.NewStaticFilter(params.PulseChainConfig, genesis),
		peers:   make(map[string]*peer),
		evicted: make(map[enode.ID]time.Time),
	}
}

func record(id byte, entries ...enr.Entry) *enode.Node {
	var r enr.Record
	for _, e := range entries {
		r.Set(e)
	}
	return enode.SignNull(&r, enode.ID{id})
}

func TestDialable(t *testing.T) {
	n := testNode()
	pulse := &ethENR{ForkID: n.status().ForkID}
	mainnet := &ethENR{ForkID: forkid.NewID(params.MainnetChainConfig, n.cfg.Genesis, 22_000_000, uint64(time.Now().Unix()))}
	cases := []struct {
		name string
		node *enode.Node
		want bool
	}{
		{"pulsechain", record(1, pulse), true},
		{"ethereum mainnet", record(2, mainnet), false},
		{"no eth entry", record(3), false},
	}
	for _, c := range cases {
		if got := n.dialable(c.node); got != c.want {
			t.Errorf("%s: dialable = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestEvictNonServingPeers(t *testing.T) {
	n := testNode()
	mk := func(b byte, connected time.Time) *peer {
		p := p2p.NewPeer(enode.ID{b}, "test", nil)
		return &peer{id: p.ID().String(), p: p, connected: connected}
	}
	old := time.Now().Add(-time.Hour)
	idle, fresh, recent, winner, late := mk(1, old), mk(2, time.Now()), mk(3, old), mk(4, old), mk(5, old)
	recent.served.Store(time.Now().UnixNano())
	order := []*peer{idle, fresh, recent, winner, late}

	for i := 0; i < evictStrikes-1; i++ {
		n.strikeLosers(order, winner)
	}
	if n.isEvicted(idle.p.ID()) {
		t.Fatal("peer evicted before reaching the strike limit")
	}
	n.strikeLosers(order, winner)
	if !n.isEvicted(idle.p.ID()) {
		t.Error("idle peer that never served state was not evicted")
	}
	if n.isEvicted(fresh.p.ID()) {
		t.Error("newly connected peer evicted within its grace period")
	}
	if n.isEvicted(recent.p.ID()) {
		t.Error("peer that served state recently was evicted")
	}
	if n.isEvicted(winner.p.ID()) || late.strikes.Load() != 0 {
		t.Error("winner or peers asked after it were struck")
	}
}
