package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/rpc"
)

const testKey = "SECRETKEY0123456789"

func TestRedact(t *testing.T) {
	for in, want := range map[string]string{
		"https://user:pass@rpc.example.com:8443/v2/" + testKey + "?apikey=x#f": "https://rpc.example.com:8443",
		"wss://rpc.example.com/ws/" + testKey:                                  "wss://rpc.example.com",
		"http://127.0.0.1:8545":                                                "http://127.0.0.1:8545",
		"rpc.example.com:8545":                                                 "<redacted>",
		"%zz":                                                                  "<redacted>",
	} {
		if got := Redact(in); got != want {
			t.Errorf("Redact(%q) = %q, want %q", in, got, want)
		}
	}
	got := RedactAll([]string{"https://a.example/" + testKey, "https://b.example/?key=" + testKey})
	if got != "https://a.example,https://b.example" {
		t.Errorf("RedactAll = %q", got)
	}
}

func assertNoKey(t *testing.T, what string, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: no error", what)
	}
	if msg := err.Error(); strings.Contains(msg, testKey) || !strings.HasPrefix(msg, "upstream 127.0.0.1:") {
		t.Fatalf("%s: error %q leaks the key or lacks the host", what, msg)
	}
}

// deadPool returns a pool whose only endpoint refuses connections and has the
// key in its path.
func deadPool(t *testing.T) *Pool {
	t.Helper()
	srv := httptest.NewServer(http.NotFoundHandler())
	u := srv.URL + "/v2/" + testKey
	srv.Close()
	p, err := NewPool([]string{u})
	if err != nil {
		t.Fatal(err)
	}
	p.Timeout = 2 * time.Second
	return p
}

func TestTransportErrorHidesKey(t *testing.T) {
	p := deadPool(t)
	ctx := context.Background()
	var res any
	err := p.Call(ctx, &res, false, "eth_blockNumber")
	assertNoKey(t, "Call", err)
	var te *TransportError
	if !errors.As(err, &te) {
		t.Fatalf("Call: %T is not a TransportError", err)
	}
	assertNoKey(t, "Broadcast", p.Broadcast(ctx, &res, "eth_sendRawTransaction", "0x00"))

	elems := []rpc.BatchElem{{Method: "eth_blockNumber", Result: &res}}
	err = p.Batch(ctx, false, elems)
	if err == nil || strings.Contains(err.Error(), testKey) {
		t.Fatalf("Batch: error %v leaks the key", err)
	}
	assertNoKey(t, "Batch element", elems[0].Error)
}

func TestHTTPErrorBodyNotShown(t *testing.T) {
	// A provider that echoes the request path in its error page.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no route for "+r.URL.Path, http.StatusInternalServerError)
	}))
	defer srv.Close()
	p, err := NewPool([]string{srv.URL + "/v2/" + testKey})
	if err != nil {
		t.Fatal(err)
	}
	var res any
	err = p.Call(context.Background(), &res, false, "eth_blockNumber")
	assertNoKey(t, "Call", err)
	if !strings.Contains(err.Error(), "500") {
		t.Fatalf("error %q does not name the status", err)
	}
}

func TestJSONRPCErrorKeepsType(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID json.RawMessage `json:"id"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0", "id": req.ID,
			"error": map[string]any{"code": 3, "message": "execution reverted"},
		})
	}))
	defer srv.Close()
	p, err := NewPool([]string{srv.URL + "/v2/" + testKey})
	if err != nil {
		t.Fatal(err)
	}
	var res any
	err = p.Call(context.Background(), &res, false, "eth_call")
	assertNoKey(t, "Call", err)
	var rpcErr rpc.Error
	if !errors.As(err, &rpcErr) || !strings.Contains(err.Error(), "execution reverted") {
		t.Fatalf("error %q lost the JSON-RPC error", err)
	}
}

func TestSanitizeDropsUnknownErrorWithKey(t *testing.T) {
	raw := "https://rpc.example.com/v2/" + testKey
	err := sanitize(raw, "rpc.example.com", errors.New("dial "+raw+": boom"))
	if strings.Contains(err.Error(), testKey) {
		t.Fatalf("error %q leaks the key", err)
	}
	err = sanitize("https://rpc.example.com/?apikey="+testKey, "rpc.example.com", errors.New("bad apikey="+testKey))
	if strings.Contains(err.Error(), testKey) {
		t.Fatalf("error %q leaks the key", err)
	}
}

func TestNewPoolErrorHidesKey(t *testing.T) {
	_, err := NewPool([]string{"ftp://rpc.example.com/v2/" + testKey})
	if err == nil {
		t.Fatal("no error for an unknown scheme")
	}
	if strings.Contains(err.Error(), testKey) {
		t.Fatalf("error %q leaks the key", err)
	}
}
