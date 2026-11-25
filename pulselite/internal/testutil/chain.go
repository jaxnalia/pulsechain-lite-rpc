// Package testutil builds deterministic test chains with go-ethereum's chain
// maker and serves EIP-1186 proofs for any of their historical states, so
// the proof-based execution layer can be tested end to end offline.
package testutil

import (
	"context"
	"crypto/ecdsa"
	"fmt"
	"math/big"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/consensus/beacon"
	"github.com/ethereum/go-ethereum/consensus/ethash"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/ethdb/memorydb"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/trie"
	"github.com/ethereum/go-ethereum/triedb"
	"github.com/holiman/uint256"
	"github.com/pulsechainlite/pulselite/internal/proof"
	"github.com/pulsechainlite/pulselite/internal/verified"
)

// Config is a merged, Shanghai-only chain config (like PulseChain mainnet).
func Config() *params.ChainConfig {
	cfg := *params.MergedTestChainConfig
	cfg.ChainID = big.NewInt(369)
	cfg.CancunTime, cfg.PragueTime, cfg.OsakaTime = nil, nil, nil
	cfg.BlobScheduleConfig = nil
	return &cfg
}

// StoreCode is runtime code: with 64 bytes of calldata it stores
// calldata[32:64] at slot calldata[0:32]; otherwise it returns
// SLOAD(calldata[0:32]).
var StoreCode = []byte{
	0x36, 0x60, 0x40, 0x14, 0x60, 0x13, 0x57, // CALLDATASIZE PUSH1 64 EQ PUSH1 0x13 JUMPI
	0x60, 0x00, 0x35, 0x54, 0x60, 0x00, 0x52, // PUSH1 0 CALLDATALOAD SLOAD PUSH1 0 MSTORE
	0x60, 0x20, 0x60, 0x00, 0xf3, // PUSH1 32 PUSH1 0 RETURN
	0x5b, 0x60, 0x20, 0x35, 0x60, 0x00, 0x35, 0x55, 0x00, // JUMPDEST value slot SSTORE STOP
}

// DestructCode self-destructs to the caller.
var DestructCode = []byte{0x33, 0xff}

// InitCode wraps runtime code in a constructor that returns it.
func InitCode(runtime []byte) []byte {
	l := byte(len(runtime))
	return append([]byte{0x60, l, 0x60, 0x0c, 0x60, 0x00, 0x39, 0x60, l, 0x60, 0x00, 0xf3}, runtime...)
}

// StoreCall encodes a store(slot, value) call.
func StoreCall(slot, value common.Hash) []byte { return append(slot.Bytes(), value.Bytes()...) }

// Chain is a generated test chain with full historical state.
type Chain struct {
	Config   *params.ChainConfig
	Genesis  *types.Block
	Blocks   []*types.Block
	Receipts []types.Receipts
	DB       ethdb.Database
	TrieDB   *triedb.Database

	Key      *ecdsa.PrivateKey
	Sender   common.Address
	Store    common.Address // storage contract with many genesis slots
	Destruct common.Address // self-destructing contract with storage

	mu     sync.Mutex
	byHash map[common.Hash]*types.Header
}

// GenesisSlots is the number of pre-populated storage slots of Store.
const GenesisSlots = 300

// NewChain generates n blocks exercising transfers, account creation,
// storage writes and deletions (trie collapses), contract creation,
// self-destruct and withdrawals.
func NewChain(n int) *Chain {
	key, _ := crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
	c := &Chain{
		Config:   Config(),
		Key:      key,
		Sender:   crypto.PubkeyToAddress(key.PublicKey),
		Store:    common.HexToAddress("0x5700000000000000000000000000000000000001"),
		Destruct: common.HexToAddress("0xde00000000000000000000000000000000000002"),
		byHash:   map[common.Hash]*types.Header{},
	}
	storage := map[common.Hash]common.Hash{}
	for i := 1; i <= GenesisSlots; i++ {
		storage[common.BigToHash(big.NewInt(int64(i)))] = common.BigToHash(big.NewInt(int64(i * 1000)))
	}
	destructStorage := map[common.Hash]common.Hash{}
	for i := 1; i <= 20; i++ {
		destructStorage[common.BigToHash(big.NewInt(int64(i)))] = common.BigToHash(big.NewInt(int64(i)))
	}
	alloc := types.GenesisAlloc{
		c.Sender:   {Balance: new(big.Int).Mul(big.NewInt(1e9), big.NewInt(params.Ether))},
		c.Store:    {Code: StoreCode, Storage: storage, Balance: big.NewInt(1)},
		c.Destruct: {Code: DestructCode, Storage: destructStorage, Balance: big.NewInt(12345)},
	}
	// Plenty of unrelated accounts so the account trie has real depth.
	for i := 0; i < 400; i++ {
		alloc[common.BigToAddress(big.NewInt(int64(0x10000+i*977)))] = types.Account{Balance: big.NewInt(int64(i + 1))}
	}
	genesis := &core.Genesis{
		Config:     c.Config,
		Alloc:      alloc,
		Difficulty: common.Big0,
		BaseFee:    big.NewInt(params.InitialBaseFee),
		GasLimit:   30_000_000,
	}
	signer := types.LatestSigner(c.Config)
	engine := beacon.New(ethash.NewFaker())
	db, blocks, receipts := core.GenerateChainWithGenesis(genesis, engine, n, func(i int, b *core.BlockGen) {
		tx := func(to *common.Address, value *big.Int, gas uint64, data []byte) {
			t, err := types.SignNewTx(key, signer, &types.DynamicFeeTx{
				ChainID: c.Config.ChainID, Nonce: b.TxNonce(c.Sender), GasTipCap: big.NewInt(params.GWei),
				GasFeeCap: new(big.Int).Add(new(big.Int).Mul(b.BaseFee(), big.NewInt(2)), big.NewInt(params.GWei)), Gas: gas, To: to, Value: value, Data: data,
			})
			if err != nil {
				panic(err)
			}
			b.AddTx(t)
		}
		// New accounts via value transfers.
		for j := 0; j < 3; j++ {
			to := common.BigToAddress(big.NewInt(int64(0xa000000 + i*10 + j)))
			tx(&to, big.NewInt(int64(1e15+j)), 21000, nil)
		}
		// Storage writes, updates and deletions (zeroing slots collapses branches).
		for j := 0; j < 6; j++ {
			slot := common.BigToHash(big.NewInt(int64(1 + (i*37+j*11)%GenesisSlots)))
			val := common.BigToHash(big.NewInt(int64(i*100 + j + 1)))
			if j%2 == 1 {
				val = common.Hash{}
			}
			tx(&c.Store, nil, 100000, StoreCall(slot, val))
		}
		if i == 2 {
			tx(nil, nil, 200000, InitCode(StoreCode)) // contract creation
		}
		if i == 3 {
			tx(&c.Destruct, nil, 100000, nil) // pre-Cancun SELFDESTRUCT with storage
		}
		if i%2 == 0 {
			b.AddWithdrawal(&types.Withdrawal{Validator: uint64(i), Address: common.BigToAddress(big.NewInt(int64(0xb00000 + i))), Amount: 1000})
			b.AddWithdrawal(&types.Withdrawal{Validator: uint64(i + 1), Address: c.Sender, Amount: 5})
		}
	})
	c.DB = db
	c.TrieDB = triedb.NewDatabase(db, triedb.HashDefaults)
	c.Genesis = genesis.ToBlock()
	c.Blocks = blocks
	c.Receipts = receipts
	c.byHash[c.Genesis.Hash()] = c.Genesis.Header()
	for _, b := range blocks {
		c.byHash[b.Hash()] = b.Header()
	}
	return c
}

// Fork generates n blocks on top of parent with a different coinbase so the
// blocks differ from the canonical ones.
func (c *Chain) Fork(parent *types.Block, n int, salt int64) []*types.Block {
	signer := types.LatestSigner(c.Config)
	blocks, _ := core.GenerateChain(c.Config, parent, beacon.New(ethash.NewFaker()), c.DB, n, func(i int, b *core.BlockGen) {
		b.SetCoinbase(common.BigToAddress(big.NewInt(0xf0000 + salt)))
		to := common.BigToAddress(big.NewInt(0xc000000 + salt*100 + int64(i)))
		t, _ := types.SignNewTx(c.Key, signer, &types.DynamicFeeTx{
			ChainID: c.Config.ChainID, Nonce: b.TxNonce(c.Sender), GasTipCap: big.NewInt(params.GWei),
			GasFeeCap: new(big.Int).Add(new(big.Int).Mul(b.BaseFee(), big.NewInt(2)), big.NewInt(params.GWei)), Gas: 21000, To: &to, Value: big.NewInt(7),
		})
		b.AddTx(t)
	})
	c.mu.Lock()
	for _, b := range blocks {
		c.byHash[b.Hash()] = b.Header()
	}
	c.mu.Unlock()
	return blocks
}

// Header returns any generated header by hash.
func (c *Chain) Header(h common.Hash) *types.Header {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.byHash[h]
}

// State opens the full state at root.
func (c *Chain) State(root common.Hash) *state.StateDB {
	s, err := state.New(root, state.NewDatabase(c.TrieDB, nil))
	if err != nil {
		panic(err)
	}
	return s
}

// Backend serves verified.Backend from the chain's full historical state.
type Backend struct {
	C        *Chain
	mu       sync.Mutex
	Requests int
	// Corrupt, if set, tampers with proofs to simulate a malicious upstream.
	Corrupt bool
}

func (b *Backend) rootAt(at verified.BlockRef) (common.Hash, error) {
	h := b.C.Header(at.Hash)
	if h == nil {
		return common.Hash{}, fmt.Errorf("unknown block %x", at.Hash)
	}
	return h.Root, nil
}

func prove(tr *trie.StateTrie, key []byte) []hexutil.Bytes {
	db := memorydb.New()
	if err := tr.Prove(key, db); err != nil {
		panic(err)
	}
	var out []hexutil.Bytes
	it := db.NewIterator(nil, nil)
	for it.Next() {
		out = append(out, common.CopyBytes(it.Value()))
	}
	it.Release()
	return out
}

// Proofs implements verified.Backend.
func (b *Backend) Proofs(_ context.Context, at verified.BlockRef, reqs []verified.ProofRequest) ([]*proof.AccountResult, error) {
	b.mu.Lock()
	b.Requests += len(reqs)
	b.mu.Unlock()
	root, err := b.rootAt(at)
	if err != nil {
		return nil, err
	}
	out := make([]*proof.AccountResult, len(reqs))
	for i, req := range reqs {
		tr, err := trie.NewStateTrie(trie.StateTrieID(root), b.C.TrieDB)
		if err != nil {
			return nil, err
		}
		acc, err := tr.GetAccount(req.Address)
		if err != nil {
			return nil, err
		}
		res := &proof.AccountResult{
			Address:      req.Address,
			AccountProof: prove(tr, crypto.Keccak256(req.Address.Bytes())),
			Balance:      (*hexutil.Big)(new(big.Int)),
			CodeHash:     types.EmptyCodeHash,
			StorageHash:  types.EmptyRootHash,
		}
		var st *trie.StateTrie
		if acc != nil {
			res.Balance = (*hexutil.Big)(acc.Balance.ToBig())
			res.Nonce = hexutil.Uint64(acc.Nonce)
			res.CodeHash = common.BytesToHash(acc.CodeHash)
			res.StorageHash = acc.Root
			if acc.Root != types.EmptyRootHash {
				st, err = trie.NewStateTrie(trie.StorageTrieID(root, crypto.Keccak256Hash(req.Address.Bytes()), acc.Root), b.C.TrieDB)
				if err != nil {
					return nil, err
				}
			}
		}
		for _, slot := range req.Slots {
			sp := proof.StorageResult{Key: slot.Hex(), Value: (*hexutil.Big)(new(big.Int))}
			if st != nil {
				v, err := st.GetStorage(req.Address, slot.Bytes())
				if err != nil {
					return nil, err
				}
				sp.Value = (*hexutil.Big)(new(big.Int).SetBytes(v))
				sp.Proof = prove(st, crypto.Keccak256(slot.Bytes()))
			}
			res.StorageProof = append(res.StorageProof, sp)
		}
		if b.Corrupt {
			res.Balance = (*hexutil.Big)(new(big.Int).Add(res.Balance.ToInt(), big.NewInt(1)))
		}
		out[i] = res
	}
	return out, nil
}

// Codes implements verified.Backend.
func (b *Backend) Codes(_ context.Context, at verified.BlockRef, reqs []verified.CodeRequest) ([][]byte, error) {
	root, err := b.rootAt(at)
	if err != nil {
		return nil, err
	}
	s := b.C.State(root)
	out := make([][]byte, len(reqs))
	for i, r := range reqs {
		out[i] = s.GetCode(r.Address)
	}
	return out, nil
}

// HeadersByHash implements verified.Backend.
func (b *Backend) HeadersByHash(_ context.Context, hashes []common.Hash) ([]*types.Header, error) {
	out := make([]*types.Header, len(hashes))
	for i, h := range hashes {
		hdr := b.C.Header(h)
		if hdr == nil {
			return nil, fmt.Errorf("unknown header %x", h)
		}
		out[i] = hdr
	}
	return out, nil
}

// Balance is a helper reading a balance from full state.
func (c *Chain) Balance(root common.Hash, a common.Address) *uint256.Int {
	return c.State(root).GetBalance(a)
}
