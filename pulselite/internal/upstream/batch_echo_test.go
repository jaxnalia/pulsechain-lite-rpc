package upstream_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/ethereum/go-ethereum/rpc"
)

// A per-element error in a batch reply goes through the same redaction as a
// single call.
func TestBatchElementErrorRedacted(t *testing.T) {
	const key = "BATCHKEY0123456789"
	srv := echoServer(t, func(r *http.Request) string {
		return "rate limited: http://" + r.Host + r.URL.Path + " key " + key
	})
	p := newPool(t, srv.URL+"/v2/"+key)
	var a, b any
	elems := []rpc.BatchElem{
		{Method: "eth_blockNumber", Result: &a},
		{Method: "eth_chainId", Result: &b},
	}
	err := p.Batch(context.Background(), false, elems)
	assertRedacted(t, "Batch", err, key)
	for i := range elems {
		assertRedacted(t, "Batch element", elems[i].Error, key)
	}
}
