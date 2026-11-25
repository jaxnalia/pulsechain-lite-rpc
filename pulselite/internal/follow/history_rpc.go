package follow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/ethereum/go-ethereum/trie"
	"github.com/pulsechainlite/pulselite/internal/upstream"
)

// RPCHistory implements History over opt-in RPC upstreams, with the same
// verification rules as the devp2p implementation.
type RPCHistory struct{ Pool *upstream.Pool }

// HeaderByHash implements History.
func (r *RPCHistory) HeaderByHash(ctx context.Context, hash common.Hash) (*types.Header, error) {
	var raw json.RawMessage
	if err := r.Pool.Call(ctx, &raw, false, "eth_getBlockByHash", hash, false); err != nil {
		return nil, err
	}
	h := new(types.Header)
	if err := json.Unmarshal(raw, h); err != nil {
		return nil, err
	}
	if h.Hash() != hash {
		return nil, errors.New("header hash mismatch")
	}
	return h, nil
}

// HeadersDescending implements History.
func (r *RPCHistory) HeadersDescending(ctx context.Context, from, count uint64) ([]*types.Header, error) {
	raws := make([]json.RawMessage, count)
	elems := make([]rpc.BatchElem, count)
	for i := uint64(0); i < count; i++ {
		elems[i] = rpc.BatchElem{Method: "eth_getBlockByNumber", Args: []any{hexutil.EncodeUint64(from - i), false}, Result: &raws[i]}
	}
	if err := r.Pool.Batch(ctx, false, elems); err != nil {
		return nil, err
	}
	out := make([]*types.Header, count)
	for i := range raws {
		h := new(types.Header)
		if err := json.Unmarshal(raws[i], h); err != nil {
			return nil, fmt.Errorf("decode header #%d: %w", from-uint64(i), err)
		}
		if i > 0 && out[i-1].ParentHash != h.Hash() {
			return nil, errors.New("headers do not link")
		}
		out[i] = h
	}
	return out, nil
}

// Body implements History.
func (r *RPCHistory) Body(ctx context.Context, h *types.Header) (*types.Body, error) {
	var raw struct {
		Transactions []*types.Transaction `json:"transactions"`
		Withdrawals  []*types.Withdrawal  `json:"withdrawals"`
	}
	if err := r.Pool.Call(ctx, &raw, true, "eth_getBlockByHash", h.Hash(), true); err != nil {
		return nil, err
	}
	if types.DeriveSha(types.Transactions(raw.Transactions), trie.NewStackTrie(nil)) != h.TxHash {
		return nil, errors.New("transactions root mismatch")
	}
	body := &types.Body{Transactions: raw.Transactions}
	if h.WithdrawalsHash != nil {
		if raw.Withdrawals == nil {
			raw.Withdrawals = []*types.Withdrawal{}
		}
		if types.DeriveSha(types.Withdrawals(raw.Withdrawals), trie.NewStackTrie(nil)) != *h.WithdrawalsHash {
			return nil, errors.New("withdrawals root mismatch")
		}
		body.Withdrawals = raw.Withdrawals
	}
	return body, nil
}

// ReceiptsBatch implements deposits.Source.
func (r *RPCHistory) ReceiptsBatch(ctx context.Context, hs []*types.Header) ([]types.Receipts, error) {
	out := make([]types.Receipts, len(hs))
	for i, h := range hs {
		rs, err := r.BlockReceipts(ctx, h)
		if err != nil {
			return nil, err
		}
		out[i] = rs
	}
	return out, nil
}

// BodiesBatch implements deposits.Source.
func (r *RPCHistory) BodiesBatch(ctx context.Context, hs []*types.Header) ([]*types.Body, error) {
	out := make([]*types.Body, len(hs))
	for i, h := range hs {
		b, err := r.Body(ctx, h)
		if err != nil {
			return nil, err
		}
		out[i] = b
	}
	return out, nil
}

// BlockReceipts implements History.
func (r *RPCHistory) BlockReceipts(ctx context.Context, h *types.Header) (types.Receipts, error) {
	var receipts []*types.Receipt
	if err := r.Pool.Call(ctx, &receipts, true, "eth_getBlockReceipts", h.Hash()); err != nil {
		return nil, err
	}
	if types.DeriveSha(types.Receipts(receipts), trie.NewStackTrie(nil)) != h.ReceiptHash {
		return nil, fmt.Errorf("receipts root mismatch for #%d", h.Number)
	}
	return receipts, nil
}
