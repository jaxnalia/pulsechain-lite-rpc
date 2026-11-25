package rpcapi

import (
	"context"
	"encoding/json"
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/ethdb/memorydb"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/pulsechainlite/pulselite/internal/chain"
	"github.com/pulsechainlite/pulselite/internal/exec"
	"github.com/pulsechainlite/pulselite/internal/testutil"
	"github.com/pulsechainlite/pulselite/internal/verified"
)

func setup(t *testing.T) (*testutil.Chain, *rpc.Client) {
	t.Helper()
	tc := testutil.NewChain(6)
	ch, err := chain.Open("", tc.Config, chain.Windows{Headers: 8192, Logs: 1024, Reorg: 128})
	if err != nil {
		t.Fatal(err)
	}
	ch.AddHeader(tc.Genesis.Header())
	for i, b := range tc.Blocks {
		ch.AddBlock(b)
		ch.SetReceipts(b.Hash(), tc.Receipts[i])
	}
	head := tc.Blocks[len(tc.Blocks)-1]
	if _, err := ch.SetHead(head.Hash()); err != nil {
		t.Fatal(err)
	}
	ch.SetFinalized(tc.Blocks[1].Hash())
	src := verified.NewSource(memorydb.New(), &testutil.Backend{C: tc}, ch)
	ex := exec.New(tc.Config, src, ch, 40)
	srv := rpc.NewServer()
	srv.RegisterName("eth", NewEthAPI(&Backend{Chain: ch, Exec: ex, GasCap: 50_000_000, StateWindow: 120}))
	srv.RegisterName("net", NewNetAPI(369))
	srv.RegisterName("web3", Web3API{})
	t.Cleanup(srv.Stop)
	return tc, rpc.DialInProc(srv)
}

func TestStateQueriesMatchFullState(t *testing.T) {
	tc, c := setup(t)
	ctx := context.Background()
	for _, b := range []int{2, 4, 6} { // historical (in window) and head
		hdr := tc.Blocks[b-1].Header()
		full := tc.State(hdr.Root)
		tag := hexutil.EncodeUint64(uint64(b))
		for _, a := range []common.Address{tc.Sender, tc.Store, tc.Destruct, common.BigToAddress(big.NewInt(0xa000000 + 11)), common.HexToAddress("0x9999")} {
			var bal hexutil.Big
			if err := c.CallContext(ctx, &bal, "eth_getBalance", a, tag); err != nil {
				t.Fatal(err)
			}
			if bal.ToInt().Cmp(full.GetBalance(a).ToBig()) != 0 {
				t.Fatalf("block %d %s: balance %s, full state %s", b, a, bal.ToInt(), full.GetBalance(a))
			}
			var nonce hexutil.Uint64
			c.CallContext(ctx, &nonce, "eth_getTransactionCount", a, tag)
			if uint64(nonce) != full.GetNonce(a) {
				t.Fatalf("block %d %s: nonce mismatch", b, a)
			}
			var code hexutil.Bytes
			c.CallContext(ctx, &code, "eth_getCode", a, tag)
			if string(code) != string(full.GetCode(a)) {
				t.Fatalf("block %d %s: code mismatch", b, a)
			}
		}
		for _, slot := range []int64{1, 5, 37, 48, 299, 1000} {
			s := common.BigToHash(big.NewInt(slot))
			var v hexutil.Bytes
			if err := c.CallContext(ctx, &v, "eth_getStorageAt", tc.Store, hexutil.EncodeBig(big.NewInt(slot)), tag); err != nil {
				t.Fatal(err)
			}
			if common.BytesToHash(v) != full.GetState(tc.Store, s) {
				t.Fatalf("block %d slot %d: storage mismatch", b, slot)
			}
			var ret hexutil.Bytes
			call := map[string]any{"to": tc.Store, "data": hexutil.Bytes(s.Bytes())}
			if err := c.CallContext(ctx, &ret, "eth_call", call, tag); err != nil {
				t.Fatal(err)
			}
			if common.BytesToHash(ret) != full.GetState(tc.Store, s) {
				t.Fatalf("block %d slot %d: eth_call mismatch", b, slot)
			}
		}
	}
	var gas hexutil.Uint64
	call := map[string]any{"from": tc.Sender, "to": tc.Store, "data": hexutil.Bytes(testutil.StoreCall(common.BigToHash(big.NewInt(424242)), common.BigToHash(big.NewInt(1))))}
	if err := c.CallContext(ctx, &gas, "eth_estimateGas", call); err != nil {
		t.Fatal(err)
	}
	if gas < 41000 || gas > 60000 {
		t.Fatalf("implausible estimate %d", gas)
	}
}

func TestBlocksReceiptsAndWindow(t *testing.T) {
	tc, c := setup(t)
	ctx := context.Background()
	var num hexutil.Uint64
	c.CallContext(ctx, &num, "eth_blockNumber")
	if uint64(num) != 6 {
		t.Fatalf("blockNumber %d", num)
	}
	var chainID hexutil.Big
	c.CallContext(ctx, &chainID, "eth_chainId")
	if chainID.ToInt().Int64() != 369 {
		t.Fatal("wrong chain id")
	}
	var netVersion string
	c.CallContext(ctx, &netVersion, "net_version")
	if netVersion != "369" {
		t.Fatal("wrong net_version")
	}
	var blk map[string]any
	if err := c.CallContext(ctx, &blk, "eth_getBlockByNumber", "latest", true); err != nil {
		t.Fatal(err)
	}
	if blk["hash"] != tc.Blocks[5].Hash().Hex() {
		t.Fatalf("latest block hash %v", blk["hash"])
	}
	if txs := blk["transactions"].([]any); len(txs) != len(tc.Blocks[5].Transactions()) {
		t.Fatal("tx count mismatch")
	}
	c.CallContext(ctx, &blk, "eth_getBlockByNumber", "finalized", false)
	if blk["hash"] != tc.Blocks[1].Hash().Hex() {
		t.Fatal("finalized tag not served")
	}
	tx := tc.Blocks[3].Transactions()[2]
	var rcpt map[string]any
	if err := c.CallContext(ctx, &rcpt, "eth_getTransactionReceipt", tx.Hash()); err != nil {
		t.Fatal(err)
	}
	if rcpt["blockHash"] != tc.Blocks[3].Hash().Hex() || rcpt["status"] != "0x1" || rcpt["from"] != strings.ToLower(tc.Sender.Hex()) && rcpt["from"] != tc.Sender.Hex() {
		t.Fatalf("receipt mismatch: %v", rcpt)
	}
	var txj map[string]any
	c.CallContext(ctx, &txj, "eth_getTransactionByHash", tx.Hash())
	if txj["hash"] != tx.Hash().Hex() {
		t.Fatal("tx lookup failed")
	}
	// Contract creation receipt carries the new address.
	create := tc.Blocks[2].Transactions()[len(tc.Blocks[2].Transactions())-1]
	c.CallContext(ctx, &rcpt, "eth_getTransactionReceipt", create.Hash())
	if rcpt["contractAddress"] == nil {
		t.Fatal("contract address missing from creation receipt")
	}
	var logs []json.RawMessage
	if err := c.CallContext(ctx, &logs, "eth_getLogs", map[string]any{"fromBlock": "0x1", "toBlock": "latest"}); err != nil {
		t.Fatal(err)
	}
	// Outside the window must error clearly instead of returning wrong data.
	var bal hexutil.Big
	err := c.CallContext(ctx, &bal, "eth_getBalance", tc.Sender, "0x64")
	if err == nil || !strings.Contains(err.Error(), "not available") {
		t.Fatalf("out-of-window query: %v", err)
	}
	var fh map[string]any
	if err := c.CallContext(ctx, &fh, "eth_feeHistory", "0x4", "latest", []float64{25, 75}); err != nil {
		t.Fatal(err)
	}
	if len(fh["baseFeePerGas"].([]any)) != 5 {
		t.Fatal("feeHistory length")
	}
	var gp hexutil.Big
	if err := c.CallContext(ctx, &gp, "eth_gasPrice"); err != nil || gp.ToInt().Sign() <= 0 {
		t.Fatalf("gasPrice %v %v", gp.ToInt(), err)
	}
	var cv string
	c.CallContext(ctx, &cv, "web3_clientVersion")
	if !strings.HasPrefix(cv, "pulselite") {
		t.Fatal("client version")
	}
}
