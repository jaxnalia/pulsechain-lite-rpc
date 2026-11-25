package rpcapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/ethdb/memorydb"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/pulsechainlite/pulselite/internal/chain"
	"github.com/pulsechainlite/pulselite/internal/exec"
	"github.com/pulsechainlite/pulselite/internal/testutil"
	"github.com/pulsechainlite/pulselite/internal/upstream"
	"github.com/pulsechainlite/pulselite/internal/verified"
)

// A state read through a failing upstream must not show the upstream URL,
// with its key in the path, to the public JSON-RPC caller.
func TestUpstreamKeyNotInRPCError(t *testing.T) {
	const key = "SECRETKEY0123456789"
	dead := httptest.NewServer(http.NotFoundHandler())
	u := dead.URL + "/v2/" + key
	dead.Close()
	pool, err := upstream.NewPool([]string{u})
	if err != nil {
		t.Fatal(err)
	}
	pool.Timeout = 2 * time.Second

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
	if _, err := ch.SetHead(tc.Blocks[len(tc.Blocks)-1].Hash()); err != nil {
		t.Fatal(err)
	}
	src := verified.NewSource(memorydb.New(), &verified.RPCBackend{Pool: pool}, ch)
	ex := exec.New(tc.Config, src, ch, 40)
	srv := rpc.NewServer()
	srv.RegisterName("eth", NewEthAPI(&Backend{Chain: ch, Exec: ex, GasCap: 50_000_000, StateWindow: 120}))
	defer srv.Stop()
	c := rpc.DialInProc(srv)
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var bal hexutil.Big
	err = c.CallContext(ctx, &bal, "eth_getBalance", tc.Sender, "latest")
	checkNoKey(t, "eth_getBalance", err, key)
	var out hexutil.Bytes
	err = c.CallContext(ctx, &out, "eth_call", map[string]any{"to": tc.Store}, "latest")
	checkNoKey(t, "eth_call", err, key)
}

func checkNoKey(t *testing.T, method string, err error, key string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: no error from a dead upstream", method)
	}
	if strings.Contains(err.Error(), key) {
		t.Fatalf("%s: error %q leaks the key", method, err)
	}
	if !strings.Contains(err.Error(), "upstream 127.0.0.1:") {
		t.Fatalf("%s: error %q does not name the upstream host", method, err)
	}
}
