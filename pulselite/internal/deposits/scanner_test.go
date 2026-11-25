package deposits

import (
	"context"
	"errors"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/beacon"
	"github.com/ethereum/go-ethereum/consensus/ethash"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb/memorydb"
	"github.com/ethereum/go-ethereum/params"
	"github.com/pulsechainlite/pulselite/internal/chain"
)

type fakeSource struct {
	byNum    map[uint64]*types.Block
	receipts map[common.Hash]types.Receipts
	forge    uint64 // if non-zero, corrupt the header at this number
}

func (f *fakeSource) HeadersDescending(_ context.Context, from, count uint64) ([]*types.Header, error) {
	var out []*types.Header
	for i := uint64(0); i < count; i++ {
		b := f.byNum[from-i]
		if b == nil {
			return nil, errors.New("missing")
		}
		h := b.Header()
		if from-i == f.forge {
			h.Extra = []byte("forged")
		}
		out = append(out, h)
	}
	return out, nil
}

func (f *fakeSource) ReceiptsBatch(_ context.Context, hs []*types.Header) ([]types.Receipts, error) {
	out := make([]types.Receipts, len(hs))
	for i, h := range hs {
		out[i] = f.receipts[h.Hash()]
	}
	return out, nil
}

func (f *fakeSource) BodiesBatch(_ context.Context, hs []*types.Header) ([]*types.Body, error) {
	out := make([]*types.Body, len(hs))
	for i, h := range hs {
		out[i] = f.byNum[h.Number.Uint64()].Body()
	}
	return out, nil
}

func setup(t *testing.T) (*fakeSource, *chain.Chain, []*types.Block, map[uint64]bool) {
	key, _ := crypto.GenerateKey()
	sender := crypto.PubkeyToAddress(key.PublicKey)
	cfg := *params.MergedTestChainConfig
	cfg.CancunTime, cfg.PragueTime, cfg.OsakaTime, cfg.BlobScheduleConfig = nil, nil, nil, nil
	// LOG1(topic) with empty data, emitted by a contract at the deposit address.
	code := append(append([]byte{0x7f}, Topic.Bytes()...), 0x60, 0x00, 0x60, 0x00, 0xa1, 0x00)
	genesis := &core.Genesis{
		Config: &cfg, Difficulty: common.Big0, BaseFee: big.NewInt(params.InitialBaseFee), GasLimit: 30_000_000,
		Alloc: types.GenesisAlloc{
			sender:   {Balance: big.NewInt(1e18)},
			Contract: {Code: code},
		},
	}
	signer := types.LatestSigner(&cfg)
	expect := map[uint64]bool{}
	_, blocks, receipts := core.GenerateChainWithGenesis(genesis, beacon.New(ethash.NewFaker()), 40, func(i int, b *core.BlockGen) {
		// A plain transfer first so deposit logs are not at index 0.
		to := common.HexToAddress("0x1234")
		mk := func(to common.Address) {
			tx, _ := types.SignNewTx(key, signer, &types.DynamicFeeTx{ChainID: cfg.ChainID, Nonce: b.TxNonce(sender),
				GasTipCap: big.NewInt(1), GasFeeCap: new(big.Int).Add(b.BaseFee(), big.NewInt(1e9)), Gas: 60000, To: &to})
			b.AddTx(tx)
		}
		mk(to)
		if i%5 == 0 {
			mk(Contract)
			expect[uint64(i+1)] = true
		}
	})
	src := &fakeSource{byNum: map[uint64]*types.Block{}, receipts: map[common.Hash]types.Receipts{}}
	for i, b := range blocks {
		src.byNum[b.NumberU64()] = b
		src.receipts[b.Hash()] = receipts[i]
	}
	ch, err := chain.Open("", &cfg, chain.Windows{Headers: 8192, Logs: 1024, Reorg: 128})
	if err != nil {
		t.Fatal(err)
	}
	last := blocks[len(blocks)-1]
	ch.AddHeader(last.Header())
	ch.SetHead(last.Hash())
	ch.SetFinalized(last.Hash())
	return src, ch, blocks, expect
}

func TestScanFindsAllDeposits(t *testing.T) {
	DeployBlock = 1
	src, ch, blocks, expect := setup(t)
	s := New(src, ch, memorydb.New())
	s.Batch, s.Parallel = 7, 3 // force several linked batches per round
	if s.Covers(1, 40) {
		t.Fatal("covers before scanning")
	}
	if err := s.step(context.Background(), ch.Finalized()); err != nil {
		t.Fatal(err)
	}
	if err := s.extend(context.Background(), ch.Finalized()); err != nil {
		t.Fatal(err)
	}
	logs, err := s.Query(1, 40, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != len(expect) {
		t.Fatalf("found %d deposit logs, want %d", len(logs), len(expect))
	}
	for _, l := range logs {
		if !expect[l.BlockNumber] {
			t.Fatalf("unexpected deposit log in block %d", l.BlockNumber)
		}
		b := blocks[l.BlockNumber-1]
		if l.BlockHash != b.Hash() || l.TxHash != b.Transactions()[1].Hash() || l.TxIndex != 1 || l.Address != Contract {
			t.Fatalf("log metadata wrong: %+v", l)
		}
	}
	sub, _ := s.Query(10, 20, nil)
	for _, l := range sub {
		if l.BlockNumber < 10 || l.BlockNumber > 20 {
			t.Fatal("range filter broken")
		}
	}
	if _, err := s.Query(1, 41, nil); !errors.Is(err, ErrIncomplete) {
		t.Fatal("query beyond coverage not rejected")
	}
}

func TestScanRejectsForgedHeaders(t *testing.T) {
	DeployBlock = 1
	src, ch, _, _ := setup(t)
	src.forge = 20
	s := New(src, ch, memorydb.New())
	s.Batch, s.Parallel = 7, 3
	if err := s.step(context.Background(), ch.Finalized()); err == nil {
		t.Fatal("forged header chain accepted")
	}
	if s.Covers(1, 40) {
		t.Fatal("coverage extended past a forged header")
	}
	low, _, _, _ := s.Status()
	if low <= 20 {
		t.Fatalf("scan progressed below the forged header (low=%d)", low)
	}
}
