// Package proof verifies eth_getProof (EIP-1186) responses against a trusted
// state root using go-ethereum's trie.VerifyProof, and extracts the trie nodes
// that were actually on the verified paths so they can be cached.
package proof

import (
	"bytes"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb/memorydb"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/trie"
	"github.com/holiman/uint256"
)

// StorageResult is one entry of an eth_getProof storageProof list.
type StorageResult struct {
	Key   string          `json:"key"`
	Value *hexutil.Big    `json:"value"`
	Proof []hexutil.Bytes `json:"proof"`
}

// AccountResult is an eth_getProof response.
type AccountResult struct {
	Address      common.Address  `json:"address"`
	AccountProof []hexutil.Bytes `json:"accountProof"`
	Balance      *hexutil.Big    `json:"balance"`
	CodeHash     common.Hash     `json:"codeHash"`
	Nonce        hexutil.Uint64  `json:"nonce"`
	StorageHash  common.Hash     `json:"storageHash"`
	StorageProof []StorageResult `json:"storageProof"`
}

// Verified is the outcome of a successful verification.
type Verified struct {
	Address common.Address
	Account *types.StateAccount // nil if the account does not exist
	Storage map[common.Hash]common.Hash
	// Nodes are the trie nodes (keyed by hash) that lay on the verified
	// paths. They are self-authenticating: each is reachable from the
	// trusted root via hash links.
	Nodes map[common.Hash][]byte
}

// recordingDB is a proof database that remembers which nodes were read.
type recordingDB struct {
	db   *memorydb.Database
	used map[common.Hash][]byte
}

func newRecordingDB(nodes []hexutil.Bytes) *recordingDB {
	r := &recordingDB{db: memorydb.New(), used: make(map[common.Hash][]byte)}
	for _, n := range nodes {
		r.db.Put(crypto.Keccak256(n), n)
	}
	return r
}

func (r *recordingDB) Has(key []byte) (bool, error) { return r.db.Has(key) }

func (r *recordingDB) Get(key []byte) ([]byte, error) {
	v, err := r.db.Get(key)
	if err == nil {
		r.used[common.BytesToHash(key)] = common.CopyBytes(v)
	}
	return v, err
}

// ParseStorageKey normalises a storage key as returned by upstreams (which
// may echo short or unpadded hex) into a 32-byte slot.
func ParseStorageKey(s string) (common.Hash, error) {
	s = strings.TrimPrefix(strings.TrimPrefix(s, "0x"), "0X")
	if len(s) > 64 {
		return common.Hash{}, fmt.Errorf("storage key too long: %q", s)
	}
	if len(s)%2 == 1 {
		s = "0" + s
	}
	b, err := hexutil.Decode("0x" + s)
	if err != nil && s != "" {
		return common.Hash{}, fmt.Errorf("invalid storage key %q: %w", s, err)
	}
	return common.BytesToHash(b), nil
}

var (
	ErrAddressMismatch = errors.New("proof is for a different address")
	ErrAccountMismatch = errors.New("claimed account fields do not match proven account")
	ErrStorageMismatch = errors.New("claimed storage value does not match proven value")
)

// VerifyAccount verifies an eth_getProof response against root. The claimed
// balance/nonce/codeHash/storageHash and storage values must match the values
// proven by the Merkle paths, otherwise the response is rejected.
func VerifyAccount(root common.Hash, want common.Address, res *AccountResult) (*Verified, error) {
	if res == nil {
		return nil, errors.New("nil proof result")
	}
	if res.Address != want {
		return nil, ErrAddressMismatch
	}
	adb := newRecordingDB(res.AccountProof)
	val, err := trie.VerifyProof(root, crypto.Keccak256(want.Bytes()), adb)
	if err != nil {
		return nil, fmt.Errorf("account proof: %w", err)
	}
	out := &Verified{Address: want, Storage: make(map[common.Hash]common.Hash), Nodes: adb.used}

	storageRoot := types.EmptyRootHash
	if val == nil {
		if !claimedEmpty(res) {
			return nil, fmt.Errorf("%w: account absent but non-empty fields claimed", ErrAccountMismatch)
		}
	} else {
		acc := new(types.StateAccount)
		if err := rlp.DecodeBytes(val, acc); err != nil {
			return nil, fmt.Errorf("decode account: %w", err)
		}
		if err := matchAccount(acc, res); err != nil {
			return nil, err
		}
		out.Account = acc
		storageRoot = acc.Root
	}
	for _, sp := range res.StorageProof {
		slot, err := ParseStorageKey(sp.Key)
		if err != nil {
			return nil, err
		}
		claimed := common.Hash{}
		if sp.Value != nil {
			if (*big.Int)(sp.Value).Sign() < 0 || (*big.Int)(sp.Value).BitLen() > 256 {
				return nil, fmt.Errorf("storage value out of range for slot %x", slot)
			}
			claimed = common.BigToHash((*big.Int)(sp.Value))
		}
		if storageRoot == types.EmptyRootHash {
			if claimed != (common.Hash{}) {
				return nil, fmt.Errorf("%w: slot %x non-zero in empty storage", ErrStorageMismatch, slot)
			}
			out.Storage[slot] = common.Hash{}
			continue
		}
		sdb := newRecordingDB(sp.Proof)
		sval, err := trie.VerifyProof(storageRoot, crypto.Keccak256(slot.Bytes()), sdb)
		if err != nil {
			return nil, fmt.Errorf("storage proof %x: %w", slot, err)
		}
		proven := common.Hash{}
		if sval != nil {
			_, content, _, err := rlp.Split(sval)
			if err != nil {
				return nil, fmt.Errorf("decode slot %x: %w", slot, err)
			}
			proven = common.BytesToHash(content)
		}
		if proven != claimed {
			return nil, fmt.Errorf("%w: slot %x proven %x claimed %x", ErrStorageMismatch, slot, proven, claimed)
		}
		out.Storage[slot] = proven
		for h, n := range sdb.used {
			out.Nodes[h] = n
		}
	}
	return out, nil
}

func claimedEmpty(res *AccountResult) bool {
	if res.Balance != nil && (*big.Int)(res.Balance).Sign() != 0 {
		return false
	}
	if res.Nonce != 0 {
		return false
	}
	if res.CodeHash != (common.Hash{}) && res.CodeHash != types.EmptyCodeHash {
		return false
	}
	if res.StorageHash != (common.Hash{}) && res.StorageHash != types.EmptyRootHash {
		return false
	}
	return true
}

func matchAccount(acc *types.StateAccount, res *AccountResult) error {
	bal := new(big.Int)
	if res.Balance != nil {
		bal = (*big.Int)(res.Balance)
	}
	claimedBal, overflow := uint256.FromBig(bal)
	switch {
	case overflow || claimedBal.Cmp(acc.Balance) != 0:
		return fmt.Errorf("%w: balance proven %s claimed %s", ErrAccountMismatch, acc.Balance, bal)
	case uint64(res.Nonce) != acc.Nonce:
		return fmt.Errorf("%w: nonce proven %d claimed %d", ErrAccountMismatch, acc.Nonce, res.Nonce)
	case !bytes.Equal(res.CodeHash.Bytes(), acc.CodeHash):
		return fmt.Errorf("%w: codeHash proven %x claimed %x", ErrAccountMismatch, acc.CodeHash, res.CodeHash)
	case res.StorageHash != acc.Root:
		return fmt.Errorf("%w: storageHash proven %x claimed %x", ErrAccountMismatch, acc.Root, res.StorageHash)
	}
	return nil
}

// VerifyCode checks that code hashes to the expected code hash.
func VerifyCode(codeHash common.Hash, code []byte) error {
	if got := crypto.Keccak256Hash(code); got != codeHash {
		return fmt.Errorf("code hash mismatch: have %x want %x", got, codeHash)
	}
	return nil
}
