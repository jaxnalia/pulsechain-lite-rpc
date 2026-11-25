package p2pnet

import (
	"context"
	"errors"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/eth/protocols/snap"
	"github.com/ethereum/go-ethereum/ethdb/memorydb"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/trie"
	"github.com/pulsechainlite/pulselite/internal/chain"
	"github.com/pulsechainlite/pulselite/internal/exec"
	"github.com/pulsechainlite/pulselite/internal/testutil"
	"github.com/pulsechainlite/pulselite/internal/verified"
)

// fakePeer serves snap responses from a generated chain the way go-pulse's
// snap server builds them: the first leaf at or after the origin plus
// boundary proofs for the origin and the last returned key.
type fakePeer struct {
	c *testutil.Chain
	// completeStorage returns whole storage tries without proofs (as a peer
	// may when the entire range fits in the response).
	completeStorage bool
	requests        int
	pathRequests    int
}

func (f *fakePeer) prove(tr *trie.Trie, keys ...[]byte) [][]byte {
	db := memorydb.New()
	for _, k := range keys {
		tr.Prove(k, db)
	}
	var out [][]byte
	it := db.NewIterator(nil, nil)
	for it.Next() {
		out = append(out, common.CopyBytes(it.Value()))
	}
	it.Release()
	return out
}

func firstLeaf(tr *trie.Trie, origin []byte) ([]byte, []byte) {
	nit, err := tr.NodeIterator(origin)
	if err != nil {
		return nil, nil
	}
	it := trie.NewIterator(nit)
	if it.Next() {
		return common.CopyBytes(it.Key), common.CopyBytes(it.Value)
	}
	return nil, nil
}

func (f *fakePeer) AccountRange(_ context.Context, root, key common.Hash) (*snap.AccountRangePacket, error) {
	f.requests++
	tr, err := trie.New(trie.TrieID(root), f.c.TrieDB)
	if err != nil {
		return nil, err
	}
	res := &snap.AccountRangePacket{}
	k, v := firstLeaf(tr, key.Bytes())
	keys := [][]byte{key.Bytes()}
	if k != nil {
		acc := new(types.StateAccount)
		if err := rlp.DecodeBytes(v, acc); err != nil {
			return nil, err
		}
		res.Accounts = []*snap.AccountData{{Hash: common.BytesToHash(k), Body: types.SlimAccountRLP(*acc)}}
		keys = append(keys, k)
	}
	res.Proof = f.prove(tr, keys...)
	return res, nil
}

func (f *fakePeer) StorageRange(_ context.Context, root, account, slot common.Hash) (*snap.StorageRangesPacket, error) {
	f.requests++
	at, err := trie.NewStateTrie(trie.StateTrieID(root), f.c.TrieDB)
	if err != nil {
		return nil, err
	}
	acc, err := at.GetAccountByHash(account)
	if err != nil || acc == nil {
		return &snap.StorageRangesPacket{}, err
	}
	st, err := trie.New(trie.StorageTrieID(root, account, acc.Root), f.c.TrieDB)
	if err != nil {
		return nil, err
	}
	res := &snap.StorageRangesPacket{}
	if f.completeStorage {
		var all []*snap.StorageData
		it := trie.NewIterator(st.MustNodeIterator(nil))
		for it.Next() {
			all = append(all, &snap.StorageData{Hash: common.BytesToHash(it.Key), Body: common.CopyBytes(it.Value)})
		}
		// Rotate so the requested slot (or its successor) comes first, as
		// the backend expects the origin first; no proof is attached.
		for i, s := range all {
			if s.Hash.Cmp(slot) >= 0 {
				all = append(all[i:], all[:i]...)
				break
			}
		}
		res.Slots = [][]*snap.StorageData{all}
		return res, nil
	}
	k, v := firstLeaf(st, slot.Bytes())
	keys := [][]byte{slot.Bytes()}
	if k != nil {
		res.Slots = [][]*snap.StorageData{{{Hash: common.BytesToHash(k), Body: v}}}
		keys = append(keys, k)
	}
	res.Proof = f.prove(st, keys...)
	return res, nil
}

func (f *fakePeer) ByteCodes(_ context.Context, hashes []common.Hash) ([][]byte, error) {
	var out [][]byte
	for _, h := range hashes {
		if c := rawdb.ReadCode(f.c.DB, h); len(c) > 0 {
			out = append(out, c)
		}
	}
	return out, nil
}

func (f *fakePeer) TrieNodes(_ context.Context, root common.Hash, paths []snap.TrieNodePathSet) ([][]byte, error) {
	f.pathRequests++
	// Like go-pulse: nodes are returned flattened in request order, and
	// serving stops at the first node that cannot be resolved.
	var out [][]byte
	for _, set := range paths {
		at, err := trie.NewStateTrie(trie.StateTrieID(root), f.c.TrieDB)
		if err != nil {
			return out, nil
		}
		if len(set) == 1 {
			blob, _, err := at.GetNode(set[0])
			if err != nil {
				return out, nil
			}
			out = append(out, blob)
			continue
		}
		acc, err := at.GetAccountByHash(common.BytesToHash(set[0]))
		if err != nil || acc == nil {
			return out, nil
		}
		st, err := trie.NewStateTrie(trie.StorageTrieID(root, common.BytesToHash(set[0]), acc.Root), f.c.TrieDB)
		if err != nil {
			return out, nil
		}
		for _, p := range set[1:] {
			blob, _, err := st.GetNode(p)
			if err != nil {
				return out, nil
			}
			out = append(out, blob)
		}
	}
	return out, nil
}

func (f *fakePeer) HeaderByHash(_ context.Context, h common.Hash) (*types.Header, error) {
	if hdr := f.c.Header(h); hdr != nil {
		return hdr, nil
	}
	return nil, errors.New("unknown header")
}

func run(t *testing.T, tc *testutil.Chain, peer *fakePeer, siblingDepth int) {
	t.Helper()
	parent := tc.Genesis.Header()
	for _, b := range tc.Blocks {
		ch, err := chain.Open("", tc.Config, chain.Windows{Headers: 8192, Logs: 1024, Reorg: 128})
		if err != nil {
			t.Fatal(err)
		}
		ch.AddHeader(parent)
		ch.AddHeader(b.Header())
		be := &Backend{Node: peer, Parallel: 8, RootOf: func(at verified.BlockRef) (common.Hash, bool) {
			if h := ch.Header(at.Hash); h != nil {
				return h.Root, true
			}
			return common.Hash{}, false
		}}
		src := verified.NewSource(memorydb.New(), be, ch)
		src.SiblingDepth = siblingDepth
		ex := exec.New(tc.Config, src, ch, 40)
		res, err := ex.ExecuteBlock(context.Background(), b, parent)
		if err != nil {
			t.Fatalf("block %d: %v", b.NumberU64(), err)
		}
		if err := exec.Validate(b, res); err != nil {
			t.Fatalf("block %d: %v", b.NumberU64(), err)
		}
		parent = b.Header()
	}
}

// TestSnapBackendValidatesChain executes a generated chain (account
// creation, storage writes/deletes, CREATE, SELFDESTRUCT, withdrawals) with
// all state served as snap range proofs.
func TestSnapBackendValidatesChain(t *testing.T) {
	tc := testutil.NewChain(8)
	peer := &fakePeer{c: tc}
	run(t, tc, peer, 0) // sibling search disabled: collapses must use GetTrieNodes
	if peer.requests == 0 || peer.pathRequests == 0 {
		t.Fatalf("expected range and path requests, got %d / %d", peer.requests, peer.pathRequests)
	}
	t.Logf("snap range requests %d, trie-node path requests %d", peer.requests, peer.pathRequests)
}

// TestSnapBackendWarmNodeFetch validates consecutive blocks on a shared,
// persisted cache, where deep misses are fetched as trie nodes by path.
func TestSnapBackendWarmNodeFetch(t *testing.T) {
	tc := testutil.NewChain(8)
	peer := &fakePeer{c: tc}
	ch, err := chain.Open("", tc.Config, chain.Windows{Headers: 8192, Logs: 1024, Reorg: 128})
	if err != nil {
		t.Fatal(err)
	}
	ch.AddHeader(tc.Genesis.Header())
	be := &Backend{Node: peer, Parallel: 8, RootOf: func(at verified.BlockRef) (common.Hash, bool) {
		if h := ch.Header(at.Hash); h != nil {
			return h.Root, true
		}
		return common.Hash{}, false
	}}
	src := verified.NewSource(memorydb.New(), be, ch)
	src.NodeFetchDepth = 1
	ex := exec.New(tc.Config, src, ch, 40)
	ex.PrefetchParallel = 0 // exercise discovery rounds (node fetches), not inline key proofs
	parent := tc.Genesis.Header()
	for _, b := range tc.Blocks {
		ch.AddHeader(b.Header())
		res, err := ex.ExecuteBlock(context.Background(), b, parent)
		if err != nil {
			t.Fatalf("block %d: %v", b.NumberU64(), err)
		}
		if err := exec.Validate(b, res); err != nil {
			t.Fatalf("block %d: %v", b.NumberU64(), err)
		}
		src.Persist(res.State, b.NumberU64(), true)
		parent = b.Header()
	}
	if peer.pathRequests == 0 {
		t.Fatal("no node-by-path requests issued")
	}
	t.Logf("range requests %d, trie-node batch requests %d", peer.requests, peer.pathRequests)
}

// TestSnapBackendCompleteStorage covers responses carrying whole storage
// tries without proofs (rebuilt and checked against the storage root).
func TestSnapBackendCompleteStorage(t *testing.T) {
	tc := testutil.NewChain(3)
	run(t, tc, &fakePeer{c: tc, completeStorage: true}, 6)
}

// TestSnapBackendRejectsForgedStorage ensures a complete-storage response
// that does not hash to the verified storage root is rejected.
func TestSnapBackendRejectsForgedStorage(t *testing.T) {
	slots := []*snap.StorageData{{Hash: common.HexToHash("0x01"), Body: []byte{0x05}}}
	if _, err := proveFromCompleteStorage(slots, common.HexToHash("0xbeef"), common.HexToHash("0x01")); !errors.Is(err, errStorageRoot) {
		t.Fatalf("forged storage accepted: %v", err)
	}
}

func TestCompactPath(t *testing.T) {
	for _, tc := range []struct {
		in   []byte
		want []byte
	}{
		{nil, []byte{0x00}},
		{[]byte{1}, []byte{0x11}},
		{[]byte{1, 2}, []byte{0x00, 0x12}},
		{[]byte{0xa, 0xb, 0xc}, []byte{0x1a, 0xbc}},
	} {
		if got := compactPath(tc.in); string(got) != string(tc.want) {
			t.Fatalf("compactPath(%x) = %x want %x", tc.in, got, tc.want)
		}
	}
}
