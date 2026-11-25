package upstream_test

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

	"github.com/pulsechainlite/pulselite/internal/upstream"
)

// echoServer is an upstream that answers every JSON-RPC call, single or
// batched, with an error whose message comes from msg. Providers do this:
// "invalid project id <key>", or the request URL in a rate-limit error.
func echoServer(t *testing.T, msg func(r *http.Request) string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		type req struct {
			ID json.RawMessage `json:"id"`
		}
		reply := func(q req) map[string]any {
			return map[string]any{"jsonrpc": "2.0", "id": q.ID,
				"error": map[string]any{"code": -32000, "message": msg(r)}}
		}
		w.Header().Set("Content-Type", "application/json")
		if b := bytes.TrimSpace(body); len(b) > 0 && b[0] == '[' {
			var qs []req
			json.Unmarshal(b, &qs)
			out := make([]map[string]any, len(qs))
			for i, q := range qs {
				out[i] = reply(q)
			}
			json.NewEncoder(w).Encode(out)
			return
		}
		var q req
		json.Unmarshal(body, &q)
		json.NewEncoder(w).Encode(reply(q))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newPool(t *testing.T, url string) *upstream.Pool {
	t.Helper()
	p, err := upstream.NewPool([]string{url})
	if err != nil {
		t.Fatal(err)
	}
	p.Timeout = 5 * time.Second
	return p
}

func assertRedacted(t *testing.T, what string, err error, secrets ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: no error", what)
	}
	msg := err.Error()
	for _, s := range secrets {
		if strings.Contains(msg, s) {
			t.Fatalf("%s: error %q leaks %q", what, msg, s)
		}
	}
	if !strings.Contains(msg, "<redacted>") {
		t.Fatalf("%s: error %q does not mark the redaction", what, msg)
	}
}

// The upstream echoes only the key, one path segment, not the whole path.
func TestCallErrorEchoingKeySegment(t *testing.T) {
	const key = "SEGMENTKEY0123456789"
	srv := echoServer(t, func(*http.Request) string { return "invalid project id " + key })
	p := newPool(t, srv.URL+"/v2/"+key)
	var res any
	err := p.Call(context.Background(), &res, false, "eth_blockNumber")
	assertRedacted(t, "Call", err, key)
}

// The upstream echoes the key in decoded form, from a path segment and from
// a query value.
func TestCallErrorEchoingDecodedKey(t *testing.T) {
	cases := []struct {
		name, suffix, escaped, decoded string
	}{
		{"path", "/v2/DECODED%3AKEY0123456789", "DECODED%3AKEY0123456789", "DECODED:KEY0123456789"},
		{"query", "/?network=pulse&apikey=QUERY%40KEY0123456789", "QUERY%40KEY0123456789", "QUERY@KEY0123456789"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := echoServer(t, func(*http.Request) string { return "unknown api key " + c.decoded })
			p := newPool(t, srv.URL+c.suffix)
			var res any
			err := p.Call(context.Background(), &res, false, "eth_blockNumber")
			assertRedacted(t, "Call", err, c.escaped, c.decoded)
		})
	}
}
