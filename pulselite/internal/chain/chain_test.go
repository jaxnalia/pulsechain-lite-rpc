package chain

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/trie"
)

func mkChain(parent *types.Header, n int, salt byte, withTx bool) []*types.Block {
	var out []*types.Block
	for i := 0; i < n; i++ {
		h := &types.Header{
			ParentHash: parent.Hash(),
			Number:     new(big.Int).Add(parent.Number, common.Big1),
			Difficulty: new(big.Int),
			GasLimit:   30_000_000,
			Time:       parent.Time + 10,
			Extra:      []byte{salt},
			BaseFee:    big.NewInt(1),
		}
		var txs []*types.Transaction
		if withTx {
			txs = append(txs, types.NewTx(&types.LegacyTx{Nonce: uint64(i), Gas: 21000, GasPrice: big.NewInt(1), Data: []byte{salt, byte(i)}}))
		}
		b := types.NewBlock(h, &types.Body{Transactions: txs}, nil, trie.NewStackTrie(nil))
		out = append(out, b)
		parent = b.Header()
	}
	return out
}

func genesis() *types.Header {
	return &types.Header{Number: big.NewInt(0), Difficulty: new(big.Int), GasLimit: 30_000_000, BaseFee: big.NewInt(1)}
}

func open(t *testing.T, dir string) *Chain {
	c, err := Open(dir, params.MergedTestChainConfig, Windows{Headers: 300, Logs: 50, Reorg: 20})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestReorgRewind(t *testing.T) {
	c := open(t, "")
	g := genesis()
	c.AddHeader(g)
	main := mkChain(g, 10, 'a', true)
	for _, b := range main {
		c.AddBlock(b)
	}
	if _, err := c.SetHead(main[9].Hash()); err != nil {
		t.Fatal(err)
	}
	if c.Head().Number.Uint64() != 10 {
		t.Fatal("wrong head")
	}
	oldTx := main[7].Transactions()[0].Hash()
	if loc, ok := c.TxLookup(oldTx); !ok || loc.BlockHash != main[7].Hash() {
		t.Fatal("tx not indexed on main chain")
	}
	// Fork at block 6 (common ancestor 6), 3 blocks long -> shorter chain.
	fork := mkChain(main[5].Header(), 3, 'b', true)
	for _, b := range fork {
		c.AddBlock(b)
	}
	res, err := c.SetHead(fork[2].Hash())
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsReorg || res.Ancestor != 6 || res.Dropped != 4 || res.Added != 3 {
		t.Fatalf("unexpected reorg result %+v", res)
	}
	if h, _ := c.CanonicalHash(10); h != (common.Hash{}) {
		t.Fatal("old canonical block above new head still indexed")
	}
	for n := uint64(7); n <= 9; n++ {
		if h, _ := c.CanonicalHash(n); h != fork[n-7].Hash() {
			t.Fatalf("height %d not rewritten to fork", n)
		}
	}
	if h, _ := c.CanonicalHash(6); h != main[5].Hash() {
		t.Fatal("common ancestor changed")
	}
	if _, ok := c.TxLookup(oldTx); ok {
		t.Fatal("tx of reorged-out block still indexed")
	}
	if loc, ok := c.TxLookup(fork[1].Transactions()[0].Hash()); !ok || loc.BlockHash != fork[1].Hash() {
		t.Fatal("fork tx not indexed")
	}
	// Reorg back to the original chain.
	res, _ = c.SetHead(main[9].Hash())
	if !res.IsReorg || res.Ancestor != 6 {
		t.Fatalf("reorg back: %+v", res)
	}
	if loc, ok := c.TxLookup(oldTx); !ok || loc.BlockHash != main[7].Hash() {
		t.Fatal("tx not re-indexed after reorg back")
	}
	// Plain extension is not a reorg.
	ext := mkChain(main[9].Header(), 1, 'a', false)
	c.AddBlock(ext[0])
	if res, _ := c.SetHead(ext[0].Hash()); res.IsReorg {
		t.Fatal("extension reported as reorg")
	}
}

func TestPruneAndPersistence(t *testing.T) {
	dir := t.TempDir()
	c := open(t, dir)
	g := genesis()
	c.AddHeader(g)
	blocks := mkChain(g, 400, 'a', true)
	side := mkChain(blocks[349].Header(), 2, 's', false)
	for i, b := range blocks {
		c.AddBlock(b)
		if i == 350 {
			for _, s := range side {
				c.AddBlock(s)
			}
		}
		c.SetHead(b.Hash())
	}
	headers, canonical, txs := c.Stats()
	if headers > 301+2 || canonical > 301 {
		t.Fatalf("header window not enforced: headers=%d canonical=%d", headers, canonical)
	}
	if txs > 51 {
		t.Fatalf("tx index not pruned to logs window: %d", txs)
	}
	if c.Body(blocks[300].Hash()) != nil {
		t.Fatal("body outside logs window retained")
	}
	if c.Body(blocks[399].Hash()) == nil {
		t.Fatal("recent body missing")
	}
	if c.Header(side[0].Hash()) != nil {
		t.Fatal("stale side-chain header outside reorg window retained")
	}
	c.MarkValid(blocks[399].Hash())
	c.SetFinalized(blocks[390].Hash())
	c.Close()

	c2 := open(t, dir)
	defer c2.Close()
	if c2.Head() == nil || c2.Head().Hash() != blocks[399].Hash() {
		t.Fatal("head not restored")
	}
	if !c2.IsValid(blocks[399].Hash()) || c2.Finalized().Hash() != blocks[390].Hash() {
		t.Fatal("validity/finality not restored")
	}
	if h, ok := c2.CanonicalHash(200); !ok || h != blocks[199].Hash() {
		t.Fatal("canonical index not rebuilt")
	}
	if _, ok := c2.TxLookup(blocks[398].Transactions()[0].Hash()); !ok {
		t.Fatal("tx index not rebuilt")
	}
}

func TestReceipts(t *testing.T) {
	c := open(t, "")
	g := genesis()
	c.AddHeader(g)
	b := mkChain(g, 1, 'a', true)[0]
	c.AddBlock(b)
	c.SetHead(b.Hash())
	r := &types.Receipt{Status: 1, CumulativeGasUsed: 21000, GasUsed: 21000, Logs: []*types.Log{{Address: common.HexToAddress("0x1"), Topics: []common.Hash{{1}}, Data: []byte{2}}}}
	r.Bloom = types.CreateBloom(r)
	if err := c.SetReceipts(b.Hash(), types.Receipts{r}); err != nil {
		t.Fatal(err)
	}
	got := c.Receipts(b.Hash())
	if len(got) != 1 || got[0].TxHash != b.Transactions()[0].Hash() || got[0].BlockHash != b.Hash() || len(got[0].Logs) != 1 || got[0].Logs[0].BlockNumber != 1 {
		t.Fatalf("derived receipt fields wrong: %+v", got)
	}
}
