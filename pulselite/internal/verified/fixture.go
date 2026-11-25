package verified

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/pulsechainlite/pulselite/internal/proof"
)

// Fixture is a recorded set of witness material, stored at the granularity
// of single accounts and single slots so it can be replayed regardless of how
// requests are grouped.
type Fixture struct {
	Accounts map[string]*proof.AccountResult `json:"accounts"` // "<block>:<addr>" (storageProof empty)
	Slots    map[string]proof.StorageResult  `json:"slots"`    // "<block>:<addr>:<slot>"
	Codes    map[string]hexutil.Bytes        `json:"codes"`    // "<block>:<addr>"
	Headers  map[common.Hash]*types.Header   `json:"headers"`
	Hints    map[string]types.AccessList     `json:"hints,omitempty"` // "<block>:<keccak(call json)>"
}

func hintKey(at BlockRef, c CallHint) string {
	enc, _ := json.Marshal(c)
	return fmt.Sprintf("%d:%x", at.Number, crypto.Keccak256(enc))
}

// AccessLists records hints.
func (r *Recorder) AccessLists(ctx context.Context, at BlockRef, calls []CallHint) ([]types.AccessList, error) {
	hb, ok := r.Inner.(HintBackend)
	if !ok {
		return nil, errors.New("inner backend has no hints")
	}
	res, err := hb.AccessLists(ctx, at, calls)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, l := range res {
		if l != nil {
			r.F.Hints[hintKey(at, calls[i])] = l
		}
	}
	return res, nil
}

// AccessLists replays recorded hints (missing ones are nil).
func (r *Replay) AccessLists(_ context.Context, at BlockRef, calls []CallHint) ([]types.AccessList, error) {
	if len(r.F.Hints) == 0 {
		return nil, errors.New("fixture has no hints")
	}
	out := make([]types.AccessList, len(calls))
	for i, c := range calls {
		out[i] = r.F.Hints[hintKey(at, c)]
	}
	return out, nil
}

// NewFixture returns an empty fixture.
func NewFixture() *Fixture {
	return &Fixture{
		Accounts: map[string]*proof.AccountResult{},
		Slots:    map[string]proof.StorageResult{},
		Codes:    map[string]hexutil.Bytes{},
		Headers:  map[common.Hash]*types.Header{},
		Hints:    map[string]types.AccessList{},
	}
}

func accKey(at BlockRef, a common.Address) string { return fmt.Sprintf("%d:%s", at.Number, a.Hex()) }
func slotKey(at BlockRef, a common.Address, s common.Hash) string {
	return fmt.Sprintf("%d:%s:%s", at.Number, a.Hex(), s.Hex())
}

// Save writes the fixture as JSON.
func (f *Fixture) Save(path string) error {
	data, err := json.MarshalIndent(f, "", " ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// LoadFixture reads a fixture file.
func LoadFixture(path string) (*Fixture, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	f := NewFixture()
	return f, json.Unmarshal(data, f)
}

// Recorder wraps a Backend and records every response into a Fixture.
type Recorder struct {
	Inner Backend
	mu    sync.Mutex
	F     *Fixture
}

// NewRecorder creates a recording backend.
func NewRecorder(inner Backend) *Recorder { return &Recorder{Inner: inner, F: NewFixture()} }

// Proofs implements Backend.
func (r *Recorder) Proofs(ctx context.Context, at BlockRef, reqs []ProofRequest) ([]*proof.AccountResult, error) {
	res, err := r.Inner.Proofs(ctx, at, reqs)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, ar := range res {
		acc := *ar
		acc.StorageProof = nil
		r.F.Accounts[accKey(at, reqs[i].Address)] = &acc
		for j, sp := range ar.StorageProof {
			if j < len(reqs[i].Slots) {
				r.F.Slots[slotKey(at, reqs[i].Address, reqs[i].Slots[j])] = sp
			}
		}
	}
	return res, nil
}

// Codes implements Backend.
func (r *Recorder) Codes(ctx context.Context, at BlockRef, reqs []CodeRequest) ([][]byte, error) {
	res, err := r.Inner.Codes(ctx, at, reqs)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, c := range res {
		r.F.Codes[accKey(at, reqs[i].Address)] = c
	}
	return res, nil
}

// HeadersByHash implements Backend.
func (r *Recorder) HeadersByHash(ctx context.Context, hashes []common.Hash) ([]*types.Header, error) {
	res, err := r.Inner.HeadersByHash(ctx, hashes)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, h := range res {
		r.F.Headers[h.Hash()] = h
	}
	return res, nil
}

// Replay serves a Fixture as a Backend.
type Replay struct {
	F        *Fixture
	mu       sync.Mutex
	Requests int
}

// Proofs implements Backend.
func (r *Replay) Proofs(_ context.Context, at BlockRef, reqs []ProofRequest) ([]*proof.AccountResult, error) {
	r.mu.Lock()
	r.Requests += len(reqs)
	r.mu.Unlock()
	out := make([]*proof.AccountResult, len(reqs))
	for i, req := range reqs {
		acc, ok := r.F.Accounts[accKey(at, req.Address)]
		if !ok {
			return nil, fmt.Errorf("fixture: no account proof for %s at #%d", req.Address, at.Number)
		}
		res := *acc
		res.StorageProof = nil
		for _, s := range req.Slots {
			sp, ok := r.F.Slots[slotKey(at, req.Address, s)]
			if !ok {
				return nil, fmt.Errorf("fixture: no storage proof for %s slot %s at #%d", req.Address, s, at.Number)
			}
			res.StorageProof = append(res.StorageProof, sp)
		}
		out[i] = &res
	}
	return out, nil
}

// Codes implements Backend.
func (r *Replay) Codes(_ context.Context, at BlockRef, reqs []CodeRequest) ([][]byte, error) {
	out := make([][]byte, len(reqs))
	for i, req := range reqs {
		c, ok := r.F.Codes[accKey(at, req.Address)]
		if !ok {
			return nil, fmt.Errorf("fixture: no code for %s at #%d", req.Address, at.Number)
		}
		out[i] = c
	}
	return out, nil
}

// HeadersByHash implements Backend.
func (r *Replay) HeadersByHash(_ context.Context, hashes []common.Hash) ([]*types.Header, error) {
	out := make([]*types.Header, len(hashes))
	for i, h := range hashes {
		hdr, ok := r.F.Headers[h]
		if !ok {
			return nil, fmt.Errorf("fixture: no header %x", h)
		}
		out[i] = hdr
	}
	return out, nil
}
