package engineapi

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/beacon/engine"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethdb/memorydb"
	"github.com/ethereum/go-ethereum/node"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/pulsechainlite/pulselite/internal/chain"
	"github.com/pulsechainlite/pulselite/internal/config"
	"github.com/pulsechainlite/pulselite/internal/exec"
	"github.com/pulsechainlite/pulselite/internal/testutil"
	"github.com/pulsechainlite/pulselite/internal/verified"
)

func newAPI(t *testing.T, tc *testutil.Chain, mode config.Mode) (*API, *chain.Chain) {
	t.Helper()
	ch, err := chain.Open("", tc.Config, chain.Windows{Headers: 8192, Logs: 1024, Reorg: 128})
	if err != nil {
		t.Fatal(err)
	}
	be := &testutil.Backend{C: tc}
	src := verified.NewSource(memorydb.New(), be, ch)
	ex := exec.New(tc.Config, src, ch, 40)
	cfg := &config.Config{Mode: mode, ExecDeadline: 20 * time.Second, StateWindow: 120, MinerExtra: "pulselite"}
	return New(cfg, ex, ch, be, Hooks{}), ch
}

func payload(b *types.Block) engine.ExecutableData {
	return *engine.BlockToExecutableData(b, common.Big0, nil, nil).ExecutionPayload
}

func fcu(head, safe, fin common.Hash) engine.ForkchoiceStateV1 {
	return engine.ForkchoiceStateV1{HeadBlockHash: head, SafeBlockHash: safe, FinalizedBlockHash: fin}
}

func TestFullModeValidatesChain(t *testing.T) {
	tc := testutil.NewChain(6)
	api, ch := newAPI(t, tc, config.ModeFull)
	for _, b := range tc.Blocks {
		st, err := api.NewPayloadV2(payload(b))
		if err != nil {
			t.Fatal(err)
		}
		if st.Status != engine.VALID || st.LatestValidHash == nil || *st.LatestValidHash != b.Hash() {
			t.Fatalf("block %d: status %s", b.NumberU64(), st.Status)
		}
		resp, err := api.ForkchoiceUpdatedV2(fcu(b.Hash(), b.Hash(), tc.Genesis.Hash()), nil)
		if err != nil || resp.PayloadStatus.Status != engine.VALID {
			t.Fatalf("block %d: fcu %v %v", b.NumberU64(), resp.PayloadStatus.Status, err)
		}
		if !ch.HasReceipts(b.Hash()) {
			t.Fatalf("block %d: receipts not stored", b.NumberU64())
		}
	}
	if ch.Head().Hash() != tc.Blocks[5].Hash() {
		t.Fatal("head not updated")
	}
	// Re-sending a known payload is answered from the cache.
	if st, _ := api.NewPayloadV2(payload(tc.Blocks[2])); st.Status != engine.VALID {
		t.Fatal("known payload not VALID")
	}
}

func TestRPCModeIsOptimistic(t *testing.T) {
	tc := testutil.NewChain(2)
	api, ch := newAPI(t, tc, config.ModeRPC)
	st, _ := api.NewPayloadV2(payload(tc.Blocks[0]))
	if st.Status != engine.SYNCING {
		t.Fatalf("rpc mode newPayload: %s", st.Status)
	}
	resp, _ := api.ForkchoiceUpdatedV2(fcu(tc.Blocks[0].Hash(), common.Hash{}, common.Hash{}), &engine.PayloadAttributes{Timestamp: tc.Blocks[0].Time() + 10, Withdrawals: []*types.Withdrawal{}})
	if resp.PayloadStatus.Status != engine.SYNCING || resp.PayloadID != nil {
		t.Fatal("rpc mode must not validate or build")
	}
	if ch.Head().Hash() != tc.Blocks[0].Hash() {
		t.Fatal("rpc mode must still follow the head")
	}
}

func TestInvalidHashAndMismatchedRoot(t *testing.T) {
	tc := testutil.NewChain(2)
	api, _ := newAPI(t, tc, config.ModeFull)
	p := payload(tc.Blocks[0])
	p.BlockHash = common.HexToHash("0x1234")
	if st, _ := api.NewPayloadV2(p); st.Status != engine.INVALID {
		t.Fatalf("bad block hash: %s", st.Status)
	}
	// A self-consistent block with a wrong state root: never VALID, and the
	// service reports SYNCING rather than asserting INVALID on its own.
	h := types.CopyHeader(tc.Blocks[0].Header())
	h.Root = common.HexToHash("0xbad")
	bad := types.NewBlockWithHeader(h).WithBody(*tc.Blocks[0].Body())
	if st, _ := api.NewPayloadV2(payload(bad)); st.Status != engine.SYNCING {
		t.Fatalf("wrong-root block: %s", st.Status)
	}
	if st, _ := api.NewPayloadV1(payload(tc.Blocks[0])); st.Status == engine.VALID {
		t.Fatal("V1 accepted withdrawals")
	}
}

func TestBuildPayloadAndReimport(t *testing.T) {
	tc := testutil.NewChain(3)
	api, _ := newAPI(t, tc, config.ModeValidator)
	for _, b := range tc.Blocks {
		if st, _ := api.NewPayloadV2(payload(b)); st.Status != engine.VALID {
			t.Fatalf("block %d not VALID", b.NumberU64())
		}
	}
	head := tc.Blocks[2]
	attrs := &engine.PayloadAttributes{
		Timestamp:             head.Time() + 10,
		Random:                common.HexToHash("0x42"),
		SuggestedFeeRecipient: common.HexToAddress("0x00000000000000000000000000000000000fee01"),
		Withdrawals: []*types.Withdrawal{
			{Index: 9, Validator: 3, Address: tc.Sender, Amount: 777},
			{Index: 10, Validator: 4, Address: common.HexToAddress("0x0000000000000000000000000000000000abcdef"), Amount: 1},
		},
	}
	resp, err := api.ForkchoiceUpdatedV2(fcu(head.Hash(), head.Hash(), head.Hash()), attrs)
	if err != nil || resp.PayloadStatus.Status != engine.VALID || resp.PayloadID == nil {
		t.Fatalf("fcu with attributes: %+v %v", resp, err)
	}
	env, err := api.GetPayloadV2(*resp.PayloadID)
	if err != nil {
		t.Fatal(err)
	}
	ep := env.ExecutionPayload
	if ep.ParentHash != head.Hash() || len(ep.Transactions) != 0 || len(ep.Withdrawals) != 2 || ep.FeeRecipient != attrs.SuggestedFeeRecipient {
		t.Fatalf("unexpected payload %+v", ep)
	}
	// The consensus client would now gossip and re-import the block.
	st, _ := api.NewPayloadV2(*ep)
	if st.Status != engine.VALID {
		t.Fatalf("built payload re-import: %s", st.Status)
	}
	if _, err := api.GetPayloadV2(engine.PayloadID{9, 9}); err == nil {
		t.Fatal("unknown payload id accepted")
	}
}

func TestConsensusRestartDoesNotRewindHead(t *testing.T) {
	defer func(old uint64) { maxHeadRewind = old }(maxHeadRewind)
	maxHeadRewind = 2
	tc := testutil.NewChain(6)
	api, ch := newAPI(t, tc, config.ModeRPC)
	for _, b := range tc.Blocks {
		api.NewPayloadV2(payload(b))
	}
	api.ForkchoiceUpdatedV2(fcu(tc.Blocks[5].Hash(), common.Hash{}, common.Hash{}), nil)

	// Restarted consensus client points at an old checkpoint block (3 behind).
	resp, _ := api.ForkchoiceUpdatedV2(fcu(tc.Blocks[2].Hash(), common.Hash{}, common.Hash{}), nil)
	if resp.PayloadStatus.Status != engine.SYNCING {
		t.Fatalf("deep rewind answered %s, want SYNCING", resp.PayloadStatus.Status)
	}
	if ch.Head().Hash() != tc.Blocks[5].Hash() {
		t.Fatalf("head rewound to #%d", ch.Head().Number)
	}
	// A shallow move back (within the limit, e.g. an orphaned tip) still applies.
	api.ForkchoiceUpdatedV2(fcu(tc.Blocks[4].Hash(), common.Hash{}, common.Hash{}), nil)
	if ch.Head().Hash() != tc.Blocks[4].Hash() {
		t.Fatalf("shallow rewind ignored: head #%d", ch.Head().Number)
	}
}

func TestReorgViaForkchoice(t *testing.T) {
	tc := testutil.NewChain(5)
	api, ch := newAPI(t, tc, config.ModeFull)
	for _, b := range tc.Blocks {
		api.NewPayloadV2(payload(b))
	}
	api.ForkchoiceUpdatedV2(fcu(tc.Blocks[4].Hash(), common.Hash{}, common.Hash{}), nil)
	fork := tc.Fork(tc.Blocks[1], 4, 1) // diverges after block 2, longer by one
	for _, b := range fork {
		st, _ := api.NewPayloadV2(payload(b))
		if st.Status != engine.VALID {
			t.Fatalf("fork block %d: %s", b.NumberU64(), st.Status)
		}
	}
	resp, _ := api.ForkchoiceUpdatedV2(fcu(fork[3].Hash(), common.Hash{}, common.Hash{}), nil)
	if resp.PayloadStatus.Status != engine.VALID {
		t.Fatal("fork head not VALID")
	}
	for i, b := range fork {
		if h, _ := ch.CanonicalHash(b.NumberU64()); h != b.Hash() {
			t.Fatalf("fork block %d not canonical", i)
		}
	}
	if h, _ := ch.CanonicalHash(2); h != tc.Blocks[1].Hash() {
		t.Fatal("common ancestor not canonical")
	}
	oldTx := tc.Blocks[3].Transactions()[0].Hash()
	if _, ok := ch.TxLookup(oldTx); ok {
		t.Fatal("reorged-out tx still indexed")
	}
}

func TestJWTRequired(t *testing.T) {
	tc := testutil.NewChain(1)
	api, _ := newAPI(t, tc, config.ModeFull)
	srv := rpc.NewServer()
	srv.RegisterName("engine", api)
	var secret [32]byte
	copy(secret[:], []byte("0123456789abcdef0123456789abcdef"))
	ts := httptest.NewServer(node.NewHTTPHandlerStack(srv, nil, []string{"*"}, secret[:]))
	defer ts.Close()
	ctx := context.Background()

	noAuth, _ := rpc.DialHTTP(ts.URL)
	var caps []string
	if err := noAuth.CallContext(ctx, &caps, "engine_exchangeCapabilities", []string{}); err == nil {
		t.Fatal("engine API reachable without JWT")
	}
	authed, err := rpc.DialOptions(ctx, ts.URL, rpc.WithHTTPAuth(node.NewJWTAuth(secret)))
	if err != nil {
		t.Fatal(err)
	}
	if err := authed.CallContext(ctx, &caps, "engine_exchangeCapabilities", []string{}); err != nil || len(caps) == 0 {
		t.Fatalf("authenticated call failed: %v", err)
	}
	var st engine.PayloadStatusV1
	if err := authed.CallContext(ctx, &st, "engine_newPayloadV2", payload(tc.Blocks[0])); err != nil || st.Status != engine.VALID {
		t.Fatalf("newPayloadV2 over JWT HTTP: %v %v", st.Status, err)
	}
}
