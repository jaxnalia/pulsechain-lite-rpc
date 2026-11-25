package proof

import (
	"encoding/json"
	"errors"
	"math/big"
	"os"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb/memorydb"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/trie"
	"github.com/ethereum/go-ethereum/triedb"
	"github.com/holiman/uint256"
)

type fixture struct {
	Header  *types.Header `json:"header"`
	Entries []struct {
		Address common.Address `json:"address"`
		Slots   []common.Hash  `json:"slots"`
		Result  *AccountResult `json:"result"`
	} `json:"entries"`
}

func loadFixture(t *testing.T) *fixture {
	t.Helper()
	data, err := os.ReadFile("../../testdata/mainnet_proofs.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	f := new(fixture)
	if err := json.Unmarshal(data, f); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	if len(f.Entries) == 0 {
		t.Fatal("empty fixture")
	}
	return f
}

// cloneResult deep-copies a result so tests can tamper with it.
func cloneResult(r *AccountResult) *AccountResult {
	data, _ := json.Marshal(r)
	out := new(AccountResult)
	json.Unmarshal(data, out)
	return out
}

func TestMainnetProofsVerify(t *testing.T) {
	f := loadFixture(t)
	root := f.Header.Root
	for _, e := range f.Entries {
		v, err := VerifyAccount(root, e.Address, e.Result)
		if err != nil {
			t.Fatalf("%s: %v", e.Address, err)
		}
		if len(v.Nodes) == 0 {
			t.Fatalf("%s: no nodes extracted", e.Address)
		}
		if len(v.Storage) != len(e.Slots) {
			t.Fatalf("%s: got %d storage values, want %d", e.Address, len(v.Storage), len(e.Slots))
		}
		// Every extracted node must be one of the supplied proof nodes and
		// hash to its key.
		for h, n := range v.Nodes {
			if crypto.Keccak256Hash(n) != h {
				t.Fatalf("%s: node %x does not hash to its key", e.Address, h)
			}
		}
	}
	// Known shapes: WPLS is a contract, the coinbase exists, the
	// keccak-derived address (entry 3) has never been touched.
	wpls, _ := VerifyAccount(root, f.Entries[0].Address, f.Entries[0].Result)
	if wpls.Account == nil || common.BytesToHash(wpls.Account.CodeHash) == types.EmptyCodeHash {
		t.Fatal("WPLS should be a contract")
	}
	if wpls.Storage[f.Entries[0].Slots[3]] == (common.Hash{}) {
		t.Fatal("WPLS self-balance slot unexpectedly empty")
	}
	coinbase, _ := VerifyAccount(root, f.Entries[2].Address, f.Entries[2].Result)
	if coinbase.Account == nil {
		t.Fatal("coinbase account should exist")
	}
	absent, _ := VerifyAccount(root, f.Entries[3].Address, f.Entries[3].Result)
	if absent.Account != nil {
		t.Fatal("keccak-derived address should be absent")
	}
}

func TestTamperedAccountFields(t *testing.T) {
	f := loadFixture(t)
	root := f.Header.Root
	base := f.Entries[0]
	cases := map[string]func(r *AccountResult){
		"balance": func(r *AccountResult) {
			r.Balance = (*hexutil.Big)(new(big.Int).Add(r.Balance.ToInt(), common.Big1))
		},
		"nonce":       func(r *AccountResult) { r.Nonce++ },
		"codeHash":    func(r *AccountResult) { r.CodeHash = common.HexToHash("0x1234") },
		"storageHash": func(r *AccountResult) { r.StorageHash = common.HexToHash("0x5678") },
	}
	for name, mutate := range cases {
		r := cloneResult(base.Result)
		mutate(r)
		if _, err := VerifyAccount(root, base.Address, r); !errors.Is(err, ErrAccountMismatch) {
			t.Fatalf("%s tamper: want ErrAccountMismatch, got %v", name, err)
		}
	}
}

func TestTamperedStorage(t *testing.T) {
	f := loadFixture(t)
	root := f.Header.Root
	base := f.Entries[0]

	r := cloneResult(base.Result)
	r.StorageProof[3].Value = (*hexutil.Big)(new(big.Int).Add(r.StorageProof[3].Value.ToInt(), common.Big1))
	if _, err := VerifyAccount(root, base.Address, r); !errors.Is(err, ErrStorageMismatch) {
		t.Fatalf("value tamper: want ErrStorageMismatch, got %v", err)
	}
	// A valid proof for one slot presented as another slot must fail.
	r = cloneResult(base.Result)
	r.StorageProof[3].Key = r.StorageProof[0].Key
	if _, err := VerifyAccount(root, base.Address, r); err == nil {
		t.Fatal("swapped storage key accepted")
	}
}

func TestWrongRootAndAddress(t *testing.T) {
	f := loadFixture(t)
	e := f.Entries[0]
	if _, err := VerifyAccount(common.HexToHash("0xdead"), e.Address, e.Result); err == nil {
		t.Fatal("proof accepted against wrong root")
	}
	if _, err := VerifyAccount(f.Header.Root, f.Entries[1].Address, e.Result); !errors.Is(err, ErrAddressMismatch) {
		t.Fatalf("want ErrAddressMismatch, got %v", err)
	}
	// A proof for account A must not prove account B even if relabelled.
	r := cloneResult(e.Result)
	r.Address = f.Entries[1].Address
	if _, err := VerifyAccount(f.Header.Root, f.Entries[1].Address, r); err == nil {
		t.Fatal("relabelled proof accepted")
	}
}

func TestTruncatedAndCorruptedProof(t *testing.T) {
	f := loadFixture(t)
	e := f.Entries[0]
	r := cloneResult(e.Result)
	r.AccountProof = r.AccountProof[:len(r.AccountProof)-1]
	if _, err := VerifyAccount(f.Header.Root, e.Address, r); err == nil {
		t.Fatal("truncated proof accepted")
	}
	r = cloneResult(e.Result)
	mid := len(r.AccountProof) / 2
	r.AccountProof[mid][len(r.AccountProof[mid])/2] ^= 0xff
	if _, err := VerifyAccount(f.Header.Root, e.Address, r); err == nil {
		t.Fatal("corrupted proof accepted")
	}
	r = cloneResult(e.Result)
	r.StorageProof[3].Proof = r.StorageProof[3].Proof[:1]
	if _, err := VerifyAccount(f.Header.Root, e.Address, r); err == nil {
		t.Fatal("truncated storage proof accepted")
	}
}

func TestAbsentAccountClaims(t *testing.T) {
	f := loadFixture(t)
	e := f.Entries[3] // absent account
	r := cloneResult(e.Result)
	r.Balance = (*hexutil.Big)(big.NewInt(1))
	if _, err := VerifyAccount(f.Header.Root, e.Address, r); !errors.Is(err, ErrAccountMismatch) {
		t.Fatalf("non-zero balance for absent account accepted: %v", err)
	}
}

func TestJunkNodesNotExtracted(t *testing.T) {
	f := loadFixture(t)
	e := f.Entries[2]
	r := cloneResult(e.Result)
	junk := hexutil.Bytes([]byte("not a trie node, just junk bytes!!"))
	r.AccountProof = append(r.AccountProof, junk)
	v, err := VerifyAccount(f.Header.Root, e.Address, r)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := v.Nodes[crypto.Keccak256Hash(junk)]; ok {
		t.Fatal("junk node not on the proven path was extracted")
	}
}

// buildTrie creates a local state trie with n accounts and returns proofs.
func TestSyntheticTrie(t *testing.T) {
	tdb := triedb.NewDatabase(rawdb.NewMemoryDatabase(), nil)
	tr := trie.NewEmpty(tdb)
	accounts := map[common.Address]*types.StateAccount{}
	for i := 0; i < 500; i++ {
		a := common.BigToAddress(big.NewInt(int64(i*7919 + 1)))
		acc := &types.StateAccount{Nonce: uint64(i), Balance: uint256.NewInt(uint64(i) * 1e9), Root: types.EmptyRootHash, CodeHash: types.EmptyCodeHash.Bytes()}
		enc, _ := rlp.EncodeToBytes(acc)
		tr.MustUpdate(crypto.Keccak256(a.Bytes()), enc)
		accounts[a] = acc
	}
	root := tr.Hash()
	prove := func(a common.Address) []hexutil.Bytes {
		db := memorydb.New()
		if err := tr.Prove(crypto.Keccak256(a.Bytes()), db); err != nil {
			t.Fatal(err)
		}
		var out []hexutil.Bytes
		it := db.NewIterator(nil, nil)
		for it.Next() {
			out = append(out, common.CopyBytes(it.Value()))
		}
		it.Release()
		return out
	}
	for a, acc := range accounts {
		res := &AccountResult{Address: a, AccountProof: prove(a), Balance: (*hexutil.Big)(acc.Balance.ToBig()),
			Nonce: hexutil.Uint64(acc.Nonce), CodeHash: types.EmptyCodeHash, StorageHash: types.EmptyRootHash}
		v, err := VerifyAccount(root, a, res)
		if err != nil {
			t.Fatalf("%s: %v", a, err)
		}
		if v.Account.Nonce != acc.Nonce || v.Account.Balance.Cmp(acc.Balance) != 0 {
			t.Fatalf("%s: wrong proven account", a)
		}
	}
	// Proof of absence.
	missing := common.HexToAddress("0xffffffffffffffffffffffffffffffffffffffff")
	res := &AccountResult{Address: missing, AccountProof: prove(missing), Balance: (*hexutil.Big)(new(big.Int))}
	v, err := VerifyAccount(root, missing, res)
	if err != nil || v.Account != nil {
		t.Fatalf("absence proof: account=%v err=%v", v, err)
	}
}

func TestParseStorageKey(t *testing.T) {
	for in, want := range map[string]common.Hash{
		"0x0":  {},
		"0x1":  common.BigToHash(common.Big1),
		"0x01": common.BigToHash(common.Big1),
		"0x" + "00000000000000000000000000000000000000000000000000000000000000ff": common.BigToHash(big.NewInt(255)),
	} {
		got, err := ParseStorageKey(in)
		if err != nil || got != want {
			t.Fatalf("%s: got %x err %v", in, got, err)
		}
	}
	if _, err := ParseStorageKey("0x" + string(make([]byte, 70))); err == nil {
		t.Fatal("overlong key accepted")
	}
}
