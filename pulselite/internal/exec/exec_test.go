package exec

import (
	"context"
	"encoding/json"
	"math/big"
	"os"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethdb/memorydb"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/holiman/uint256"
	"github.com/pulsechainlite/pulselite/internal/chain"
	"github.com/pulsechainlite/pulselite/internal/testutil"
	"github.com/pulsechainlite/pulselite/internal/verified"
)

func newExecutor(t *testing.T, cfg *params.ChainConfig, be verified.Backend) (*Executor, *chain.Chain) {
	t.Helper()
	ch, err := chain.Open("", cfg, chain.Windows{Headers: 8192, Logs: 1024, Reorg: 128})
	if err != nil {
		t.Fatal(err)
	}
	src := verified.NewSource(memorydb.New(), be, ch)
	return New(cfg, src, ch, 40), ch
}

// TestGeneratedChainCold validates every block of a generated chain starting
// from an empty cache each time (all state fetched via proofs).
func TestGeneratedChainCold(t *testing.T) {
	tc := testutil.NewChain(8)
	be := &testutil.Backend{C: tc}
	parent := tc.Genesis.Header()
	for _, b := range tc.Blocks {
		ex, ch := newExecutor(t, tc.Config, be)
		ch.AddHeader(parent)
		res, err := ex.ExecuteBlock(context.Background(), b, parent)
		if err != nil {
			t.Fatalf("block %d: %v", b.NumberU64(), err)
		}
		if err := Validate(b, res); err != nil {
			t.Fatalf("block %d: %v", b.NumberU64(), err)
		}
		parent = b.Header()
	}
}

// TestGeneratedChainWarm validates sequentially with a persisted cache, as
// in full mode, and checks that the warm cache reduces fetching.
func TestGeneratedChainWarm(t *testing.T) {
	tc := testutil.NewChain(8)
	be := &testutil.Backend{C: tc}
	ex, ch := newExecutor(t, tc.Config, be)
	parent := tc.Genesis.Header()
	ch.AddHeader(parent)
	var first, later int
	for i, b := range tc.Blocks {
		before := be.Requests
		res, err := ex.ExecuteBlock(context.Background(), b, parent)
		if err != nil {
			t.Fatalf("block %d: %v", b.NumberU64(), err)
		}
		if err := Validate(b, res); err != nil {
			t.Fatalf("block %d: %v", b.NumberU64(), err)
		}
		if err := ex.Source().Persist(res.State, b.NumberU64(), true); err != nil {
			t.Logf("block %d: post-state not persisted: %v", b.NumberU64(), err)
		}
		ch.AddBlock(b)
		if i == 0 {
			first = be.Requests - before
		} else {
			later += be.Requests - before
		}
		parent = b.Header()
	}
	avg := later / (len(tc.Blocks) - 1)
	t.Logf("proof requests: first block %d, later blocks avg %d", first, avg)
	if avg >= first {
		t.Fatalf("warm cache did not reduce fetching (first %d, avg %d)", first, avg)
	}
}

// TestCollapseSiblingsFromPostState disables the brute-force key search so
// every branch collapse (storage slot zeroing, self-destruct) must be
// resolved by reconstructing the sibling from post-state proofs.
func TestCollapseSiblingsFromPostState(t *testing.T) {
	tc := testutil.NewChain(8)
	be := &testutil.Backend{C: tc}
	parent := tc.Genesis.Header()
	for _, b := range tc.Blocks {
		ex, ch := newExecutor(t, tc.Config, be)
		ex.Source().SiblingDepth = 0
		ch.AddHeader(parent)
		res, err := ex.ExecuteBlock(context.Background(), b, parent)
		if err != nil {
			t.Fatalf("block %d: %v", b.NumberU64(), err)
		}
		if err := Validate(b, res); err != nil {
			t.Fatalf("block %d: %v", b.NumberU64(), err)
		}
		parent = b.Header()
	}
}

// TestMaliciousUpstreamRejected ensures tampered proofs never validate.
func TestMaliciousUpstreamRejected(t *testing.T) {
	tc := testutil.NewChain(2)
	be := &testutil.Backend{C: tc, Corrupt: true}
	ex, ch := newExecutor(t, tc.Config, be)
	ch.AddHeader(tc.Genesis.Header())
	_, err := ex.ExecuteBlock(context.Background(), tc.Blocks[0], tc.Genesis.Header())
	if err == nil {
		t.Fatal("execution succeeded with corrupted proofs")
	}
}

// TestWrongBlockDetected checks that a block whose header claims a wrong
// state root is not validated.
func TestWrongBlockDetected(t *testing.T) {
	tc := testutil.NewChain(2)
	be := &testutil.Backend{C: tc}
	ex, ch := newExecutor(t, tc.Config, be)
	ch.AddHeader(tc.Genesis.Header())
	h := types.CopyHeader(tc.Blocks[0].Header())
	h.Root = common.HexToHash("0xbad")
	bad := types.NewBlockWithHeader(h).WithBody(*tc.Blocks[0].Body())
	res, err := ex.ExecuteBlock(context.Background(), bad, tc.Genesis.Header())
	if err != nil {
		t.Fatal(err)
	}
	if err := Validate(bad, res); err == nil {
		t.Fatal("wrong state root validated")
	}
}

// TestBuildEmpty builds a withdrawals-only payload on the head and checks
// its root against full-state execution and by re-validating it.
func TestBuildEmpty(t *testing.T) {
	tc := testutil.NewChain(3)
	be := &testutil.Backend{C: tc}
	ex, ch := newExecutor(t, tc.Config, be)
	head := tc.Blocks[len(tc.Blocks)-1].Header()
	ch.AddHeader(head)
	ws := []*types.Withdrawal{
		{Index: 100, Validator: 7, Address: tc.Sender, Amount: 1_000_000},
		{Index: 101, Validator: 8, Address: common.HexToAddress("0x0000000000000000000000000000000000c0ffee"), Amount: 42},
	}
	b, err := ex.BuildEmpty(context.Background(), BuildArgs{
		Parent: head, Timestamp: head.Time + 10, Random: common.HexToHash("0x01"),
		FeeRecipient: common.HexToAddress("0x0000000000000000000000000000000000fee000"), Withdrawals: ws, Extra: []byte("test"),
	})
	if err != nil {
		t.Fatal(err)
	}
	// Reference root: apply the withdrawals on the full state.
	full := tc.State(head.Root)
	for _, w := range ws {
		full.AddBalance(w.Address, new(uint256Int).Mul(uint256NewInt(w.Amount), uint256NewInt(params.GWei)), 0)
	}
	if want := full.IntermediateRoot(true); want != b.Root() {
		t.Fatalf("built root %x, full-state root %x", b.Root(), want)
	}
	if b.Transactions().Len() != 0 || b.GasUsed() != 0 || b.Header().WithdrawalsHash == nil {
		t.Fatal("unexpected built block shape")
	}
	res, err := ex.ExecuteBlock(context.Background(), b, head)
	if err != nil {
		t.Fatal(err)
	}
	if err := Validate(b, res); err != nil {
		t.Fatalf("built block does not re-validate: %v", err)
	}
}

// TestCallAndEstimate compares eth_call-style execution with full state.
func TestCallAndEstimate(t *testing.T) {
	tc := testutil.NewChain(4)
	be := &testutil.Backend{C: tc}
	ex, ch := newExecutor(t, tc.Config, be)
	head := tc.Blocks[len(tc.Blocks)-1].Header()
	ch.AddHeader(head)
	full := tc.State(head.Root)
	for _, slot := range []int64{1, 2, 50, 299, 5000} {
		s := common.BigToHash(bigInt(slot))
		msg := callMsg(tc.Sender, tc.Store, s.Bytes())
		res, err := ex.Call(context.Background(), head, msg)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := common.BytesToHash(res.Return()), full.GetState(tc.Store, s); got != want {
			t.Fatalf("slot %d: call returned %x, full state %x", slot, got, want)
		}
	}
	gas, _, err := ex.EstimateGas(context.Background(), head, callMsg(tc.Sender, tc.Store, testutil.StoreCall(common.BigToHash(bigInt(77777)), common.BigToHash(bigInt(1)))), 30_000_000)
	if err != nil {
		t.Fatal(err)
	}
	if gas < 21000+20000 || gas > 60000 {
		t.Fatalf("implausible gas estimate %d for a fresh SSTORE", gas)
	}
}

type blockFixture struct {
	Block   hexutil.Bytes     `json:"block"`
	Parent  *types.Header     `json:"parent"`
	Witness *verified.Fixture `json:"witness"`
}

// TestMainnetBlockReplay re-validates a captured PulseChain mainnet block
// offline: every proof, code blob and header comes from the recorded
// (untrusted) upstream responses and is verified again here.
func TestMainnetBlockReplay(t *testing.T) {
	data, err := os.ReadFile("../../testdata/mainnet_block.json")
	if err != nil {
		t.Skip("no mainnet block fixture:", err)
	}
	var f blockFixture
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatal(err)
	}
	block := new(types.Block)
	if err := rlp.DecodeBytes(f.Block, block); err != nil {
		t.Fatal(err)
	}
	replay := &verified.Replay{F: f.Witness}
	ex, ch := newExecutor(t, params.PulseChainConfig, replay)
	ch.AddHeader(f.Parent)
	start := time.Now()
	res, err := ex.ExecuteBlock(context.Background(), block, f.Parent)
	if err != nil {
		t.Fatal(err)
	}
	if err := Validate(block, res); err != nil {
		t.Fatalf("mainnet block %d: %v", block.NumberU64(), err)
	}
	t.Logf("mainnet block %d (%d txs, %d gas) validated offline in %s, root %x",
		block.NumberU64(), len(block.Transactions()), block.GasUsed(), time.Since(start).Round(time.Millisecond), res.Root)

	// Tampering with recorded (untrusted) storage values must break validation.
	for k, sp := range f.Witness.Slots {
		if sp.Value != nil && sp.Value.ToInt().Sign() > 0 {
			sp.Value = (*hexutil.Big)(bigInt(0).Add(sp.Value.ToInt(), bigInt(1)))
			f.Witness.Slots[k] = sp
		}
	}
	ex2, ch2 := newExecutor(t, params.PulseChainConfig, &verified.Replay{F: f.Witness})
	ch2.AddHeader(f.Parent)
	res2, err := ex2.ExecuteBlock(context.Background(), block, f.Parent)
	if err == nil {
		err = Validate(block, res2)
	}
	if err == nil {
		t.Fatal("tampered witness still validated")
	}
	t.Logf("tampered witness rejected: %v", err)
}

type uint256Int = uint256.Int

var uint256NewInt = uint256.NewInt

func bigInt(v int64) *big.Int { return big.NewInt(v) }

func callMsg(from, to common.Address, data []byte) *core.Message {
	return &core.Message{
		From: from, To: &to, Value: new(big.Int), GasLimit: 1_000_000,
		GasPrice: new(big.Int), GasFeeCap: new(big.Int), GasTipCap: new(big.Int),
		Data: data, SkipNonceChecks: true, SkipFromEOACheck: true,
	}
}
