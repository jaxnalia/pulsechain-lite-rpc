package verified

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/pulsechainlite/pulselite/internal/proof"
	"github.com/pulsechainlite/pulselite/internal/upstream"
)

// BlockRef identifies the block whose post-state is being read.
type BlockRef struct {
	Hash   common.Hash
	Number uint64
}

// ProofRequest asks for an account proof plus storage proofs for slots.
type ProofRequest struct {
	Address common.Address
	Slots   []common.Hash
}

// Backend fetches unverified witness material. Implementations: RPCBackend
// (eth_getProof over untrusted upstreams), the fixture replay backend used in
// tests, and (future) a devp2p snap backend. Every result is verified by the
// Source before use.
type Backend interface {
	Proofs(ctx context.Context, at BlockRef, reqs []ProofRequest) ([]*proof.AccountResult, error)
	Codes(ctx context.Context, at BlockRef, reqs []CodeRequest) ([][]byte, error)
	HeadersByHash(ctx context.Context, hashes []common.Hash) ([]*types.Header, error)
}

// CodeRequest asks for the code of an account with a known code hash.
type CodeRequest struct {
	Address common.Address
	Hash    common.Hash
}

// NodeRequest asks for the trie node at Path (expected hash Hash) and, for
// each hashed key in Keys, every deeper node on that key's path, so a whole
// missing branch arrives in one round trip.
type NodeRequest struct {
	Owner common.Hash
	Path  []byte
	Hash  common.Hash
	Keys  [][]byte // hashed keys (32 bytes) whose paths run through Path
}

// BatchPathBackend optionally fetches many trie nodes by path in bulk. It
// returns every node received, keyed by its keccak hash. Storing nodes by
// their own hash is safe by construction: a node is only ever reached via a
// hash reference from an already verified parent.
type BatchPathBackend interface {
	TrieNodesBatch(ctx context.Context, at BlockRef, reqs []NodeRequest) (map[common.Hash][]byte, error)
}

// PathNodeBackend optionally fetches a single trie node by its path (snap
// GetTrieNodes). owner is zero for the account trie, otherwise the hashed
// account address of a storage trie. The result is verified by hash.
type PathNodeBackend interface {
	TrieNode(ctx context.Context, at BlockRef, owner common.Hash, path []byte) ([]byte, error)
}

// CallHint describes a transaction for access-list hinting.
type CallHint struct {
	From       common.Address    `json:"from"`
	To         *common.Address   `json:"to,omitempty"`
	Gas        hexutil.Uint64    `json:"gas"`
	GasPrice   *hexutil.Big      `json:"gasPrice,omitempty"`
	FeeCap     *hexutil.Big      `json:"maxFeePerGas,omitempty"`
	TipCap     *hexutil.Big      `json:"maxPriorityFeePerGas,omitempty"`
	Value      *hexutil.Big      `json:"value,omitempty"`
	Data       hexutil.Bytes     `json:"data,omitempty"`
	AccessList *types.AccessList `json:"accessList,omitempty"`
}

// HintBackend optionally provides access lists (eth_createAccessList). Hints
// are untrusted and only decide which proofs to fetch; a wrong or missing
// hint costs performance, never correctness. Failed hints are returned as nil.
type HintBackend interface {
	AccessLists(ctx context.Context, at BlockRef, calls []CallHint) ([]types.AccessList, error)
}

// AccessLists implements HintBackend.
func (b *RPCBackend) AccessLists(ctx context.Context, at BlockRef, calls []CallHint) ([]types.AccessList, error) {
	type result struct {
		AccessList types.AccessList `json:"accessList"`
	}
	res := make([]result, len(calls))
	elems := make([]rpc.BatchElem, len(calls))
	for i, c := range calls {
		elems[i] = rpc.BatchElem{Method: "eth_createAccessList", Args: []any{c, blockArg(at)}, Result: &res[i]}
	}
	err := b.Pool.Batch(ctx, true, elems)
	out := make([]types.AccessList, len(calls))
	for i := range elems {
		if elems[i].Error == nil {
			out[i] = res[i].AccessList
		}
	}
	if err != nil && ctx.Err() != nil {
		return nil, err
	}
	return out, nil
}

// RPCBackend implements Backend over an upstream pool.
type RPCBackend struct {
	Pool *upstream.Pool
}

func blockArg(at BlockRef) string { return hexutil.EncodeUint64(at.Number) }

// Proofs implements Backend.
func (b *RPCBackend) Proofs(ctx context.Context, at BlockRef, reqs []ProofRequest) ([]*proof.AccountResult, error) {
	out := make([]*proof.AccountResult, len(reqs))
	elems := make([]rpc.BatchElem, len(reqs))
	for i, r := range reqs {
		slots := make([]string, len(r.Slots))
		for j, s := range r.Slots {
			slots[j] = s.Hex() // always 32-byte padded (erigon rejects short keys)
		}
		out[i] = new(proof.AccountResult)
		elems[i] = rpc.BatchElem{Method: "eth_getProof", Args: []any{r.Address, slots, blockArg(at)}, Result: out[i]}
	}
	if err := b.Pool.Batch(ctx, true, elems); err != nil {
		return nil, err
	}
	return out, nil
}

// Codes implements Backend.
func (b *RPCBackend) Codes(ctx context.Context, at BlockRef, reqs []CodeRequest) ([][]byte, error) {
	res := make([]hexutil.Bytes, len(reqs))
	elems := make([]rpc.BatchElem, len(reqs))
	for i, r := range reqs {
		elems[i] = rpc.BatchElem{Method: "eth_getCode", Args: []any{r.Address, blockArg(at)}, Result: &res[i]}
	}
	if err := b.Pool.Batch(ctx, true, elems); err != nil {
		return nil, err
	}
	out := make([][]byte, len(reqs))
	for i := range res {
		out[i] = res[i]
	}
	return out, nil
}

// HeadersByHash implements Backend.
func (b *RPCBackend) HeadersByHash(ctx context.Context, hashes []common.Hash) ([]*types.Header, error) {
	raws := make([]json.RawMessage, len(hashes))
	elems := make([]rpc.BatchElem, len(hashes))
	for i, h := range hashes {
		elems[i] = rpc.BatchElem{Method: "eth_getBlockByHash", Args: []any{h, false}, Result: &raws[i]}
	}
	if err := b.Pool.Batch(ctx, false, elems); err != nil {
		return nil, err
	}
	out := make([]*types.Header, len(hashes))
	for i, raw := range raws {
		if len(raw) == 0 || string(raw) == "null" {
			return nil, fmt.Errorf("header %x not found upstream", hashes[i])
		}
		h := new(types.Header)
		if err := json.Unmarshal(raw, h); err != nil {
			return nil, fmt.Errorf("decode header %x: %w", hashes[i], err)
		}
		out[i] = h
	}
	return out, nil
}
