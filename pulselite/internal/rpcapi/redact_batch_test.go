package rpcapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
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

// State reads go to the upstream as JSON-RPC batches. An upstream that
// answers each batch element with an error naming its own URL and key must
// not show that key to the public JSON-RPC caller.
func TestUpstreamKeyNotInBatchRPCError(t *testing.T) {
	const key = "BATCHRPCKEY0123456789"
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var reqs []struct {
			ID json.RawMessage `json:"id"`
		}
		if b := bytes.TrimSpace(body); len(b) == 0 || b[0] != '[' {
			http.Error(w, "batch only", http.StatusBadRequest)
			return
		}
		json.Unmarshal(body, &reqs)
		out := make([]map[string]any, len(reqs))
		for i, q := range reqs {
			out[i] = map[string]any{"jsonrpc": "2.0", "id": q.ID, "error": map[string]any{
				"code": -32005, "message": "rate limited: http://" + r.Host + r.URL.Path + " (key " + key + ")"}}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(out)
	}))
	defer up.Close()
	pool, err := upstream.NewPool([]string{up.URL + "/v2/" + key})
	if err != nil {
		t.Fatal(err)
	}
	pool.Timeout = 5 * time.Second

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
	if err == nil {
		t.Fatal("eth_getBalance: no error from a failing upstream")
	}
	if strings.Contains(err.Error(), key) {
		t.Fatalf("eth_getBalance: error %q leaks the key", err)
	}
	if !strings.Contains(err.Error(), "rate limited") {
		t.Fatalf("eth_getBalance: error %q lost the upstream reason", err)
	}
}
