package upstream_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/rpc"
)

// assertFailClosed checks that err names no secret in any letter case, is
// the generic "request failed" error, keeps the JSON-RPC code and carries
// no error data that names a secret.
func assertFailClosed(t *testing.T, what string, err error, code int, secrets ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: no error", what)
	}
	msg := err.Error()
	for _, s := range secrets {
		if strings.Contains(strings.ToLower(msg), strings.ToLower(s)) {
			t.Fatalf("%s: error %q leaks %q", what, msg, s)
		}
	}
	var de rpc.DataError
	if errors.As(err, &de) {
		data := fmt.Sprint(de.ErrorData())
		for _, s := range secrets {
			if strings.Contains(strings.ToLower(data), strings.ToLower(s)) {
				t.Fatalf("%s: error data %q leaks %q", what, data, s)
			}
		}
	}
	if !strings.HasPrefix(msg, "upstream 127.0.0.1:") || !strings.HasSuffix(msg, ": request failed") {
		t.Fatalf("%s: error %q is not the generic upstream error", what, msg)
	}
	var re rpc.Error
	if !errors.As(err, &re) || re.ErrorCode() != code {
		t.Fatalf("%s: error %q lost JSON-RPC code %d", what, msg, code)
	}
}

// Scrubbing is best-effort. Where it misses a form of the key, the error must
// fail closed to the generic message.
func TestRedactionFailsClosed(t *testing.T) {
	cases := []struct {
		name, suffix, echo string
		secrets            []string
	}{
		{"short key", "/v2/k3Y9z", "invalid key k3Y9z", []string{"k3Y9z"}},
		{"key in parameter name", "/?NAMEKEY0123456789=1", "unknown parameter NAMEKEY0123456789", []string{"NAMEKEY0123456789"}},
		{"percent-encoding in other case", "/v2/MIX%3AKEY0123456789", "bad key mix%3akey0123456789",
			[]string{"MIX%3AKEY0123456789", "MIX:KEY0123456789"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := echoServer(t, func(*http.Request) string { return c.echo })
			p := newPool(t, srv.URL+c.suffix)
			var res any
			err := p.Call(context.Background(), &res, false, "eth_blockNumber")
			assertFailClosed(t, "Call", err, -32000, c.secrets...)
		})
	}
}

// JSON-RPC error data is not scrubbed. If it names the key, the error fails
// closed and the data is dropped.
func TestRedactionFailsClosedOnErrorData(t *testing.T) {
	const key = "DATAKEY0123456789"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID json.RawMessage `json:"id"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID,
			"error": map[string]any{"code": 3, "message": "execution reverted", "data": "denied for key " + key}})
	}))
	defer srv.Close()
	p := newPool(t, srv.URL+"/v2/"+key)
	var res any
	err := p.Call(context.Background(), &res, false, "eth_call")
	assertFailClosed(t, "Call", err, 3, key)
}
