package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
	"github.com/pulsechainlite/pulselite/internal/follow"
	"github.com/pulsechainlite/pulselite/internal/p2pnet"
	"github.com/pulsechainlite/pulselite/internal/proof"
	"github.com/pulsechainlite/pulselite/internal/verified"
)

// p2pSmoke fetches and verifies state for a trusted head purely over devp2p.
func p2pSmoke(ctx context.Context, beaconURL string) error {
	ref, err := follow.NewBeaconClient(beaconURL).ExecutionPayload(ctx, "head")
	if err != nil {
		return fmt.Errorf("beacon head: %w", err)
	}
	fmt.Printf("trusted head from beacon node: block %d hash %x state root %x\n", ref.BlockNumber, ref.BlockHash, ref.StateRoot)
	genesis := core.DefaultPulseChainGenesisBlock().ToBlock()
	fmt.Printf("genesis %x\n", genesis.Hash())
	dir, _ := os.MkdirTemp("", "p2psmoke")
	node, err := p2pnet.New(p2pnet.Config{
		DataDir: dir, ListenPort: 30399, MaxPeers: 50, Bootnodes: params.PulseChainBootnodes, NAT: "any",
		NetworkID: 369, Chain: params.PulseChainConfig, Genesis: genesis, Name: "pulselite-smoke/v0.1.0",
		Head: func() *types.Header { return nil },
	})
	if err != nil {
		return err
	}
	if err := node.Start(); err != nil {
		return err
	}
	defer node.Stop()
	start := time.Now()
	for {
		e, s := node.PeerCount()
		if s >= 3 || time.Since(start) > 3*time.Minute {
			fmt.Printf("peers after %s: eth=%d snap=%d\n", time.Since(start).Round(time.Second), e, s)
			break
		}
		time.Sleep(2 * time.Second)
	}
	{
		h, err := node.HeaderByHash(ctx, ref.BlockHash)
		if err != nil {
			return err
		}
		fmt.Printf("header %d by hash verified\n", h.Number)
		body, err := node.Body(ctx, h)
		if err != nil {
			return fmt.Errorf("body: %w", err)
		}
		fmt.Printf("body: %d txs verified against tx root\n", len(body.Transactions))
		hs, err := node.HeadersDescending(ctx, h.Number.Uint64(), 16)
		if err != nil {
			return err
		}
		for _, n := range []int{1, 4, 16} {
			t0 := time.Now()
			rs, err := node.ReceiptsBatch(ctx, hs[:n])
			fmt.Printf("receipts for %d blocks: got %d lists err=%v (%s)\n", n, len(rs), err, time.Since(t0).Round(time.Millisecond))
		}
	}
	be := &p2pnet.Backend{Node: node, Parallel: 16, RootOf: func(verified.BlockRef) (common.Hash, bool) { return ref.StateRoot, true }}
	at := verified.BlockRef{Hash: ref.BlockHash, Number: ref.BlockNumber}
	wpls := common.HexToAddress("0xA1077a294dDE1B09bB078844df40758a5D0f9a27")
	bal := crypto.Keccak256Hash(common.LeftPadBytes(wpls.Bytes(), 32), common.LeftPadBytes([]byte{3}, 32))
	reqs := []verified.ProofRequest{
		{Address: wpls, Slots: []common.Hash{{}, common.BigToHash(common.Big1), bal}},
		{Address: common.BytesToAddress(crypto.Keccak256([]byte("pulselite-absent-account")))},
	}
	t0 := time.Now()
	res, err := be.Proofs(ctx, at, reqs)
	if err != nil {
		return fmt.Errorf("p2p proofs: %w", err)
	}
	fmt.Printf("snap proofs fetched in %s\n", time.Since(t0).Round(time.Millisecond))
	for i, r := range res {
		v, err := proof.VerifyAccount(ref.StateRoot, reqs[i].Address, r)
		if err != nil {
			return fmt.Errorf("verification of %s failed: %w", reqs[i].Address, err)
		}
		if v.Account == nil {
			fmt.Printf("  %s: proven ABSENT (%d nodes)\n", reqs[i].Address, len(v.Nodes))
			continue
		}
		fmt.Printf("  %s: proven balance %s nonce %d, %d storage slots verified (%d nodes)\n", reqs[i].Address, v.Account.Balance, v.Account.Nonce, len(v.Storage), len(v.Nodes))
		for k, val := range v.Storage {
			fmt.Printf("     slot %x = %x\n", k[:6], val)
		}
		t1 := time.Now()
		codes, err := be.Codes(ctx, at, []verified.CodeRequest{{Address: reqs[i].Address, Hash: common.BytesToHash(v.Account.CodeHash)}})
		if err != nil {
			return err
		}
		if err := proof.VerifyCode(common.BytesToHash(v.Account.CodeHash), codes[0]); err != nil {
			return err
		}
		fmt.Printf("  code %d bytes verified against code hash in %s\n", len(codes[0]), time.Since(t1).Round(time.Millisecond))
	}
	t2 := time.Now()
	h, err := node.HeaderByHash(ctx, ref.BlockHash)
	if err != nil {
		return err
	}
	fmt.Printf("header %d fetched by hash and verified in %s (state root matches beacon: %v)\n", h.Number, time.Since(t2).Round(time.Millisecond), h.Root == ref.StateRoot)
	hs, err := node.HeadersDescending(ctx, h.Number.Uint64()-1, 256)
	if err != nil {
		return err
	}
	fmt.Printf("256 ancestor headers fetched, linked to trusted head: %v\n", hs[0].Hash() == h.ParentHash)
	body, err := node.Body(ctx, h)
	if err != nil {
		return err
	}
	rcpts, err := node.BlockReceipts(ctx, h)
	if err != nil {
		return err
	}
	fmt.Printf("body (%d txs) and receipts (%d) verified against header roots\n", len(body.Transactions), len(rcpts))
	fmt.Println("PASS: state, code, headers, bodies and receipts retrieved from PulseChain p2p peers and verified")
	return nil
}
