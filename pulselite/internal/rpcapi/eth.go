// Package rpcapi implements the eth/net/web3 JSON-RPC namespaces on top of
// proof-verified state and the recent block window.
package rpcapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strings"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/common/math"
	"github.com/ethereum/go-ethereum/consensus/misc/eip1559"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/pulsechainlite/pulselite/internal/chain"
	"github.com/pulsechainlite/pulselite/internal/deposits"
	"github.com/pulsechainlite/pulselite/internal/exec"
	"github.com/pulsechainlite/pulselite/internal/metrics"
	"github.com/pulsechainlite/pulselite/internal/proof"
)

// ClientVersion is reported by web3_clientVersion.
var ClientVersion = "pulselite/v0.1.0"

// Backend bundles what the RPC handlers need.
type Backend struct {
	Chain         *chain.Chain
	Exec          *exec.Executor
	FetchReceipts func(ctx context.Context, hash common.Hash) (types.Receipts, error)
	// FetchBody downloads and verifies (tx/withdrawals roots) a block body.
	FetchBody   func(ctx context.Context, hash common.Hash) error
	GasCap      uint64
	StateWindow uint64
	// NetworkHead estimates the network head block number (0 = unknown).
	NetworkHead func() uint64
	// SendTx propagates a signed transaction (devp2p broadcast by default).
	SendTx func(ctx context.Context, tx *types.Transaction) error
	// History serves headers outside the window (verified by hash/linkage).
	History HeaderSource
	// Deposits serves the deposit-contract log history to the consensus
	// client (engine endpoint only).
	Deposits *deposits.Scanner
	// Engine marks the authenticated engine endpoint, where the consensus
	// client may query history outside the window (old headers, deposits).
	Engine bool
}

// HeaderSource fetches headers from the network.
type HeaderSource interface {
	HeaderByHash(ctx context.Context, h common.Hash) (*types.Header, error)
	HeadersDescending(ctx context.Context, from, count uint64) ([]*types.Header, error)
}

// ErrNotInWindow is returned for data outside the lite node's window.
var ErrNotInWindow = errors.New("not available in lite node: outside the recent block window")

// EthAPI implements the eth namespace.
type EthAPI struct{ b *Backend }

// NewEthAPI creates the eth namespace handler.
func NewEthAPI(b *Backend) *EthAPI { return &EthAPI{b: b} }

func (api *EthAPI) cfgChainID() *big.Int { return api.b.Exec.Config().ChainID }

// ChainId implements eth_chainId.
func (api *EthAPI) ChainId() *hexutil.Big { return (*hexutil.Big)(api.cfgChainID()) }

// BlockNumber implements eth_blockNumber.
func (api *EthAPI) BlockNumber() (hexutil.Uint64, error) {
	h := api.b.Chain.Latest()
	if h == nil {
		return 0, errNoHead
	}
	return hexutil.Uint64(h.Number.Uint64()), nil
}

var errNoHead = errors.New("no head yet: waiting for the consensus client")

// Syncing implements eth_syncing.
func (api *EthAPI) Syncing() (any, error) {
	head := api.b.Chain.Latest()
	if head == nil {
		return map[string]hexutil.Uint64{"startingBlock": 0, "currentBlock": 0, "highestBlock": 0}, nil
	}
	n := head.Number.Uint64()
	if api.b.NetworkHead != nil {
		if nh := api.b.NetworkHead(); nh > n+syncingLag {
			return map[string]hexutil.Uint64{"startingBlock": hexutil.Uint64(n), "currentBlock": hexutil.Uint64(n), "highestBlock": hexutil.Uint64(nh)}, nil
		}
	}
	return false, nil
}

// syncingLag is how many blocks behind the network head eth_syncing still
// reports false.
const syncingLag = 3

// Accounts implements eth_accounts.
func (api *EthAPI) Accounts() []common.Address { return []common.Address{} }

// Mining implements eth_mining.
func (api *EthAPI) Mining() bool { return false }

// Hashrate implements eth_hashrate.
func (api *EthAPI) Hashrate() hexutil.Uint64 { return 0 }

func (api *EthAPI) headerByNumber(n rpc.BlockNumber) (*types.Header, error) {
	c := api.b.Chain
	var h *types.Header
	switch n {
	case rpc.LatestBlockNumber, rpc.PendingBlockNumber:
		h = c.Latest()
		if h == nil {
			return nil, errNoHead
		}
	case rpc.SafeBlockNumber:
		h = c.Safe()
		if h == nil {
			return nil, errors.New("safe block not known yet")
		}
	case rpc.FinalizedBlockNumber:
		h = c.Finalized()
		if h == nil {
			return nil, errors.New("finalized block not known yet")
		}
	case rpc.EarliestBlockNumber:
		h = c.HeaderByNumber(0)
	default:
		h = c.HeaderByNumber(uint64(n.Int64()))
	}
	return h, nil
}

// resolve maps a block selector to a header in the window.
func (api *EthAPI) resolve(bnh rpc.BlockNumberOrHash) (*types.Header, error) {
	if hash, ok := bnh.Hash(); ok {
		h := api.b.Chain.Header(hash)
		if h == nil {
			return nil, fmt.Errorf("block %x: %w", hash, ErrNotInWindow)
		}
		if bnh.RequireCanonical && !api.b.Chain.IsCanonical(hash) {
			return nil, fmt.Errorf("hash %x is not currently canonical", hash)
		}
		return h, nil
	}
	n, _ := bnh.Number()
	h, err := api.headerByNumber(n)
	if err != nil {
		return nil, err
	}
	if h == nil {
		return nil, fmt.Errorf("block #%d: %w", n.Int64(), ErrNotInWindow)
	}
	return h, nil
}

func latest() rpc.BlockNumberOrHash { return rpc.BlockNumberOrHashWithNumber(rpc.LatestBlockNumber) }

func (api *EthAPI) stateErr(h *types.Header, err error) error {
	if err == nil {
		return nil
	}
	head := api.b.Chain.Head()
	if head != nil && head.Number.Uint64() > h.Number.Uint64()+api.b.StateWindow {
		return fmt.Errorf("state for block #%d not available: the lite node serves state for the most recent ~%d blocks (%v)", h.Number, api.b.StateWindow, err)
	}
	return err
}

// stateAvailable fails fast for blocks whose state peers no longer serve
// (older than the network head by more than the state window) unless the
// state root is already in the local cache.
func (api *EthAPI) stateAvailable(h *types.Header) error {
	if api.b.NetworkHead == nil {
		return nil
	}
	nh := api.b.NetworkHead()
	if nh > h.Number.Uint64()+api.b.StateWindow && !api.b.Exec.Source().HasNode(h.Root) {
		return fmt.Errorf("state for block #%d not available yet: the consensus client is %d blocks behind the network head (state is served for the most recent ~%d blocks)",
			h.Number, nh-h.Number.Uint64(), api.b.StateWindow)
	}
	return nil
}

func (api *EthAPI) readState(ctx context.Context, bnh rpc.BlockNumberOrHash, fn func(*state.StateDB) error) error {
	h, err := api.resolve(bnh)
	if err != nil {
		return err
	}
	if err := api.stateAvailable(h); err != nil {
		return err
	}
	return api.stateErr(h, api.b.Exec.ReadState(ctx, h, fn))
}

// GetBalance implements eth_getBalance.
func (api *EthAPI) GetBalance(ctx context.Context, addr common.Address, bnh rpc.BlockNumberOrHash) (*hexutil.Big, error) {
	var bal *big.Int
	err := api.readState(ctx, bnh, func(s *state.StateDB) error {
		bal = s.GetBalance(addr).ToBig()
		return nil
	})
	return (*hexutil.Big)(bal), err
}

// GetTransactionCount implements eth_getTransactionCount.
func (api *EthAPI) GetTransactionCount(ctx context.Context, addr common.Address, bnh rpc.BlockNumberOrHash) (*hexutil.Uint64, error) {
	var nonce uint64
	err := api.readState(ctx, bnh, func(s *state.StateDB) error {
		nonce = s.GetNonce(addr)
		return nil
	})
	if err != nil {
		return nil, err
	}
	n := hexutil.Uint64(nonce)
	return &n, nil
}

// GetCode implements eth_getCode.
func (api *EthAPI) GetCode(ctx context.Context, addr common.Address, bnh rpc.BlockNumberOrHash) (hexutil.Bytes, error) {
	var code []byte
	err := api.readState(ctx, bnh, func(s *state.StateDB) error {
		code = s.GetCode(addr)
		return nil
	})
	return code, err
}

// GetStorageAt implements eth_getStorageAt.
func (api *EthAPI) GetStorageAt(ctx context.Context, addr common.Address, key string, bnh rpc.BlockNumberOrHash) (hexutil.Bytes, error) {
	slot, err := proof.ParseStorageKey(key)
	if err != nil {
		return nil, err
	}
	var val common.Hash
	err = api.readState(ctx, bnh, func(s *state.StateDB) error {
		val = s.GetState(addr, slot)
		return nil
	})
	return val[:], err
}

// TransactionArgs are eth_call / eth_estimateGas arguments.
type TransactionArgs struct {
	From                 *common.Address   `json:"from"`
	To                   *common.Address   `json:"to"`
	Gas                  *hexutil.Uint64   `json:"gas"`
	GasPrice             *hexutil.Big      `json:"gasPrice"`
	MaxFeePerGas         *hexutil.Big      `json:"maxFeePerGas"`
	MaxPriorityFeePerGas *hexutil.Big      `json:"maxPriorityFeePerGas"`
	Value                *hexutil.Big      `json:"value"`
	Nonce                *hexutil.Uint64   `json:"nonce"`
	Data                 *hexutil.Bytes    `json:"data"`
	Input                *hexutil.Bytes    `json:"input"`
	AccessList           *types.AccessList `json:"accessList,omitempty"`
	ChainID              *hexutil.Big      `json:"chainId,omitempty"`
}

func (a *TransactionArgs) data() []byte {
	if a.Input != nil {
		return *a.Input
	}
	if a.Data != nil {
		return *a.Data
	}
	return nil
}

func (a *TransactionArgs) toMessage(baseFee *big.Int, gasCap uint64, defaultGas uint64) (*core.Message, error) {
	if a.GasPrice != nil && (a.MaxFeePerGas != nil || a.MaxPriorityFeePerGas != nil) {
		return nil, errors.New("both gasPrice and (maxFeePerGas or maxPriorityFeePerGas) specified")
	}
	var from common.Address
	if a.From != nil {
		from = *a.From
	}
	gas := defaultGas
	if a.Gas != nil {
		gas = uint64(*a.Gas)
	}
	if gasCap != 0 && gas > gasCap {
		gas = gasCap
	}
	var gasPrice, feeCap, tipCap *big.Int
	if baseFee == nil {
		gasPrice = new(big.Int)
		if a.GasPrice != nil {
			gasPrice = a.GasPrice.ToInt()
		}
		feeCap, tipCap = gasPrice, gasPrice
	} else if a.GasPrice != nil {
		gasPrice = a.GasPrice.ToInt()
		feeCap, tipCap = gasPrice, gasPrice
	} else {
		feeCap, tipCap = new(big.Int), new(big.Int)
		if a.MaxFeePerGas != nil {
			feeCap = a.MaxFeePerGas.ToInt()
		}
		if a.MaxPriorityFeePerGas != nil {
			tipCap = a.MaxPriorityFeePerGas.ToInt()
		}
		gasPrice = new(big.Int)
		if feeCap.Sign() != 0 || tipCap.Sign() != 0 {
			gasPrice = new(big.Int).Add(tipCap, baseFee)
			if gasPrice.Cmp(feeCap) > 0 {
				gasPrice = feeCap
			}
		}
	}
	value := new(big.Int)
	if a.Value != nil {
		value = a.Value.ToInt()
	}
	var al types.AccessList
	if a.AccessList != nil {
		al = *a.AccessList
	}
	return &core.Message{
		From:             from,
		To:               a.To,
		Value:            value,
		GasLimit:         gas,
		GasPrice:         gasPrice,
		GasFeeCap:        feeCap,
		GasTipCap:        tipCap,
		Data:             a.data(),
		AccessList:       al,
		SkipNonceChecks:  true,
		SkipFromEOACheck: true,
	}, nil
}

type revertError struct {
	error
	reason string
}

func (e *revertError) ErrorCode() int { return 3 }
func (e *revertError) ErrorData() any { return e.reason }
func newRevertError(ret []byte) *revertError {
	err := vm.ErrExecutionReverted
	if reason, uerr := abi.UnpackRevert(ret); uerr == nil {
		err = fmt.Errorf("%w: %v", vm.ErrExecutionReverted, reason)
	}
	return &revertError{error: err, reason: hexutil.Encode(ret)}
}

func noOverrides(raw *json.RawMessage) error {
	if raw != nil && len(*raw) > 0 && string(*raw) != "null" && string(*raw) != "{}" {
		return errors.New("state/block overrides are not supported by the lite node")
	}
	return nil
}

// Call implements eth_call.
func (api *EthAPI) Call(ctx context.Context, args TransactionArgs, bnh *rpc.BlockNumberOrHash, overrides *json.RawMessage, blockOverrides *json.RawMessage) (hexutil.Bytes, error) {
	if err := noOverrides(overrides); err != nil {
		return nil, err
	}
	if err := noOverrides(blockOverrides); err != nil {
		return nil, err
	}
	sel := latest()
	if bnh != nil {
		sel = *bnh
	}
	h, err := api.resolve(sel)
	if err != nil {
		return nil, err
	}
	if err := api.stateAvailable(h); err != nil {
		return nil, err
	}
	msg, err := args.toMessage(h.BaseFee, api.b.GasCap, api.b.GasCap)
	if err != nil {
		return nil, err
	}
	res, err := api.b.Exec.Call(ctx, h, msg)
	if err != nil {
		return nil, api.stateErr(h, err)
	}
	if len(res.Revert()) > 0 {
		return nil, newRevertError(res.Revert())
	}
	return res.Return(), res.Err
}

// EstimateGas implements eth_estimateGas.
func (api *EthAPI) EstimateGas(ctx context.Context, args TransactionArgs, bnh *rpc.BlockNumberOrHash, overrides *json.RawMessage) (hexutil.Uint64, error) {
	if err := noOverrides(overrides); err != nil {
		return 0, err
	}
	sel := latest()
	if bnh != nil {
		sel = *bnh
	}
	h, err := api.resolve(sel)
	if err != nil {
		return 0, err
	}
	if err := api.stateAvailable(h); err != nil {
		return 0, err
	}
	msg, err := args.toMessage(h.BaseFee, api.b.GasCap, 0)
	if err != nil {
		return 0, err
	}
	gas, revert, err := api.b.Exec.EstimateGas(ctx, h, msg, api.b.GasCap)
	if err != nil {
		if len(revert) > 0 {
			return 0, newRevertError(revert)
		}
		return 0, api.stateErr(h, err)
	}
	return hexutil.Uint64(gas), nil
}

// GetBlockByNumber implements eth_getBlockByNumber.
func (api *EthAPI) GetBlockByNumber(ctx context.Context, number rpc.BlockNumber, fullTx bool) (any, error) {
	h, err := api.headerByNumber(number)
	if err != nil {
		return nil, err
	}
	if h == nil {
		if api.b.Engine && api.b.History != nil && number >= 0 {
			// Old header for the consensus client (eth1 data lookups). By
			// number its canonicality cannot be proven locally; counted.
			metrics.RPCUnverified.Inc()
			hs, err := api.b.History.HeadersDescending(ctx, uint64(number.Int64()), 1)
			if err != nil {
				return nil, err
			}
			return marshalBlock(hs[0], nil, false, api.b.Exec.Config()), nil
		}
		return nil, nil
	}
	if fullTx && api.b.Chain.Body(h.Hash()) == nil && api.b.FetchBody != nil {
		if err := api.b.FetchBody(ctx, h.Hash()); err != nil {
			return nil, err
		}
	}
	return api.blockJSON(h, fullTx), nil
}

// GetBlockByHash implements eth_getBlockByHash.
func (api *EthAPI) GetBlockByHash(ctx context.Context, hash common.Hash, fullTx bool) (any, error) {
	h := api.b.Chain.Header(hash)
	if h == nil && api.b.Engine && api.b.History != nil {
		// Self-verifying lookup by hash, then marshal locally so the consensus
		// client can reconstruct payloads byte-for-byte.
		hdr, err := api.b.History.HeaderByHash(ctx, hash)
		if err != nil {
			return nil, nil
		}
		api.b.Chain.AddHeader(hdr)
		h = hdr
	}
	if h == nil {
		return nil, nil
	}
	if fullTx && api.b.Chain.Body(hash) == nil && api.b.FetchBody != nil {
		if err := api.b.FetchBody(ctx, hash); err != nil {
			return nil, err
		}
	}
	return api.blockJSON(h, fullTx), nil
}

func (api *EthAPI) blockJSON(h *types.Header, fullTx bool) map[string]any {
	return marshalBlock(h, api.b.Chain.Body(h.Hash()), fullTx, api.b.Exec.Config())
}

// GetBlockTransactionCountByNumber implements eth_getBlockTransactionCountByNumber.
func (api *EthAPI) GetBlockTransactionCountByNumber(number rpc.BlockNumber) (*hexutil.Uint, error) {
	h, err := api.headerByNumber(number)
	if err != nil || h == nil {
		return nil, err
	}
	return api.txCount(h.Hash()), nil
}

// GetBlockTransactionCountByHash implements eth_getBlockTransactionCountByHash.
func (api *EthAPI) GetBlockTransactionCountByHash(hash common.Hash) *hexutil.Uint {
	return api.txCount(hash)
}

func (api *EthAPI) txCount(hash common.Hash) *hexutil.Uint {
	body := api.b.Chain.Body(hash)
	if body == nil {
		return nil
	}
	n := hexutil.Uint(len(body.Transactions))
	return &n
}

// GetUncleCountByBlockNumber implements eth_getUncleCountByBlockNumber.
func (api *EthAPI) GetUncleCountByBlockNumber(rpc.BlockNumber) hexutil.Uint { return 0 }

// GetUncleCountByBlockHash implements eth_getUncleCountByBlockHash.
func (api *EthAPI) GetUncleCountByBlockHash(common.Hash) hexutil.Uint { return 0 }

func (api *EthAPI) txAt(hash common.Hash, index int) *RPCTransaction {
	h := api.b.Chain.Header(hash)
	body := api.b.Chain.Body(hash)
	if h == nil || body == nil || index < 0 || index >= len(body.Transactions) {
		return nil
	}
	return newRPCTransaction(body.Transactions[index], hash, h.Number.Uint64(), h.Time, uint64(index), h.BaseFee, api.b.Exec.Config())
}

// GetTransactionByHash implements eth_getTransactionByHash.
func (api *EthAPI) GetTransactionByHash(hash common.Hash) *RPCTransaction {
	loc, ok := api.b.Chain.TxLookup(hash)
	if !ok {
		return nil
	}
	return api.txAt(loc.BlockHash, loc.Index)
}

// GetTransactionByBlockHashAndIndex implements eth_getTransactionByBlockHashAndIndex.
func (api *EthAPI) GetTransactionByBlockHashAndIndex(hash common.Hash, index hexutil.Uint) *RPCTransaction {
	return api.txAt(hash, int(index))
}

// GetTransactionByBlockNumberAndIndex implements eth_getTransactionByBlockNumberAndIndex.
func (api *EthAPI) GetTransactionByBlockNumberAndIndex(number rpc.BlockNumber, index hexutil.Uint) (*RPCTransaction, error) {
	h, err := api.headerByNumber(number)
	if err != nil || h == nil {
		return nil, err
	}
	return api.txAt(h.Hash(), int(index)), nil
}

func (api *EthAPI) receipts(ctx context.Context, hash common.Hash) (types.Receipts, error) {
	if r := api.b.Chain.Receipts(hash); r != nil {
		return r, nil
	}
	if api.b.FetchReceipts == nil {
		return nil, errors.New("receipts not available")
	}
	return api.b.FetchReceipts(ctx, hash)
}

// GetTransactionReceipt implements eth_getTransactionReceipt.
func (api *EthAPI) GetTransactionReceipt(ctx context.Context, hash common.Hash) (map[string]any, error) {
	loc, ok := api.b.Chain.TxLookup(hash)
	if !ok {
		return nil, nil
	}
	receipts, err := api.receipts(ctx, loc.BlockHash)
	if err != nil {
		return nil, err
	}
	body := api.b.Chain.Body(loc.BlockHash)
	h := api.b.Chain.Header(loc.BlockHash)
	if body == nil || h == nil || loc.Index >= len(receipts) {
		return nil, nil
	}
	signer := types.MakeSigner(api.b.Exec.Config(), h.Number, h.Time)
	return marshalReceipt(receipts[loc.Index], body.Transactions[loc.Index], signer), nil
}

// GetBlockReceipts implements eth_getBlockReceipts.
func (api *EthAPI) GetBlockReceipts(ctx context.Context, bnh rpc.BlockNumberOrHash) ([]map[string]any, error) {
	h, err := api.resolve(bnh)
	if err != nil {
		return nil, err
	}
	body := api.b.Chain.Body(h.Hash())
	if body == nil {
		return nil, fmt.Errorf("block body #%d: %w", h.Number, ErrNotInWindow)
	}
	receipts, err := api.receipts(ctx, h.Hash())
	if err != nil {
		return nil, err
	}
	signer := types.MakeSigner(api.b.Exec.Config(), h.Number, h.Time)
	out := make([]map[string]any, len(receipts))
	for i, r := range receipts {
		out[i] = marshalReceipt(r, body.Transactions[i], signer)
	}
	return out, nil
}

// SendRawTransaction implements eth_sendRawTransaction: the transaction is
// checked against verified head state and broadcast to devp2p peers (the
// lite node keeps no mempool of its own).
func (api *EthAPI) SendRawTransaction(ctx context.Context, input hexutil.Bytes) (common.Hash, error) {
	tx := new(types.Transaction)
	if err := tx.UnmarshalBinary(input); err != nil {
		return common.Hash{}, err
	}
	if tx.Protected() && tx.ChainId().Cmp(api.cfgChainID()) != 0 {
		return common.Hash{}, fmt.Errorf("invalid chain id %d (want %d)", tx.ChainId(), api.cfgChainID())
	}
	head := api.b.Chain.Latest()
	if head == nil {
		return common.Hash{}, errNoHead
	}
	from, err := types.Sender(types.LatestSigner(api.b.Exec.Config()), tx)
	if err != nil {
		return common.Hash{}, err
	}
	var nonce uint64
	var bal *big.Int
	if err := api.b.Exec.ReadState(ctx, head, func(s *state.StateDB) error {
		nonce, bal = s.GetNonce(from), s.GetBalance(from).ToBig()
		return nil
	}); err == nil {
		if tx.Nonce() < nonce {
			return common.Hash{}, fmt.Errorf("nonce too low: next nonce %d, tx nonce %d", nonce, tx.Nonce())
		}
		if bal.Cmp(tx.Cost()) < 0 {
			return common.Hash{}, fmt.Errorf("insufficient funds for gas * price + value: balance %s, tx cost %s", bal, tx.Cost())
		}
	}
	if api.b.SendTx == nil {
		return common.Hash{}, errors.New("transaction propagation not available")
	}
	if err := api.b.SendTx(ctx, tx); err != nil {
		return common.Hash{}, err
	}
	log.Info("Broadcast transaction", "hash", tx.Hash(), "from", from, "nonce", tx.Nonce())
	return tx.Hash(), nil
}

// GasPrice implements eth_gasPrice (tip suggestion + head base fee).
func (api *EthAPI) GasPrice(ctx context.Context) (*hexutil.Big, error) {
	head := api.b.Chain.Head()
	if head == nil {
		return nil, errNoHead
	}
	tip := api.suggestTip()
	if head.BaseFee != nil {
		tip.Add(tip, head.BaseFee)
	}
	return (*hexutil.Big)(tip), nil
}

// MaxPriorityFeePerGas implements eth_maxPriorityFeePerGas.
func (api *EthAPI) MaxPriorityFeePerGas(ctx context.Context) (*hexutil.Big, error) {
	return (*hexutil.Big)(api.suggestTip()), nil
}

// suggestTip follows geth's oracle: the 60th percentile of the lowest
// effective tips of recent blocks.
func (api *EthAPI) suggestTip() *big.Int {
	head := api.b.Chain.Head()
	if head == nil {
		return new(big.Int)
	}
	var tips []*big.Int
	for i := uint64(0); i < 20 && i <= head.Number.Uint64(); i++ {
		h := api.b.Chain.HeaderByNumber(head.Number.Uint64() - i)
		if h == nil {
			break
		}
		body := api.b.Chain.Body(h.Hash())
		if body == nil {
			continue
		}
		var blockTips []*big.Int
		for _, tx := range body.Transactions {
			t, err := tx.EffectiveGasTip(h.BaseFee)
			if err == nil {
				blockTips = append(blockTips, t)
			}
		}
		sort.Slice(blockTips, func(a, b int) bool { return blockTips[a].Cmp(blockTips[b]) < 0 })
		if len(blockTips) > 3 {
			blockTips = blockTips[:3]
		}
		tips = append(tips, blockTips...)
	}
	if len(tips) == 0 {
		return big.NewInt(params.GWei)
	}
	sort.Slice(tips, func(a, b int) bool { return tips[a].Cmp(tips[b]) < 0 })
	return new(big.Int).Set(tips[(len(tips)-1)*60/100])
}

type feeHistoryResult struct {
	OldestBlock  *hexutil.Big     `json:"oldestBlock"`
	Reward       [][]*hexutil.Big `json:"reward,omitempty"`
	BaseFee      []*hexutil.Big   `json:"baseFeePerGas,omitempty"`
	GasUsedRatio []float64        `json:"gasUsedRatio"`
}

// FeeHistory implements eth_feeHistory over the block window.
func (api *EthAPI) FeeHistory(ctx context.Context, blockCount math.HexOrDecimal64, lastBlock rpc.BlockNumber, percentiles []float64) (*feeHistoryResult, error) {
	last, err := api.headerByNumber(lastBlock)
	if err != nil {
		return nil, err
	}
	if last == nil {
		return nil, fmt.Errorf("block #%d: %w", lastBlock, ErrNotInWindow)
	}
	count := uint64(blockCount)
	if count == 0 {
		return &feeHistoryResult{OldestBlock: (*hexutil.Big)(last.Number), GasUsedRatio: []float64{}}, nil
	}
	if count > 1024 {
		count = 1024
	}
	for i, p := range percentiles {
		if p < 0 || p > 100 || (i > 0 && p < percentiles[i-1]) {
			return nil, fmt.Errorf("invalid reward percentile %f", p)
		}
	}
	var headers []*types.Header
	for n := last.Number.Uint64(); uint64(len(headers)) < count; n-- {
		h := api.b.Chain.HeaderByNumber(n)
		if h == nil {
			break
		}
		headers = append([]*types.Header{h}, headers...)
		if n == 0 {
			break
		}
	}
	res := &feeHistoryResult{OldestBlock: (*hexutil.Big)(headers[0].Number)}
	for _, h := range headers {
		bf := h.BaseFee
		if bf == nil {
			bf = new(big.Int)
		}
		res.BaseFee = append(res.BaseFee, (*hexutil.Big)(bf))
		ratio := 0.0
		if h.GasLimit > 0 {
			ratio = float64(h.GasUsed) / float64(h.GasLimit)
		}
		res.GasUsedRatio = append(res.GasUsedRatio, ratio)
		if len(percentiles) > 0 {
			res.Reward = append(res.Reward, api.rewards(ctx, h, percentiles))
		}
	}
	res.BaseFee = append(res.BaseFee, (*hexutil.Big)(eip1559.CalcBaseFee(api.b.Exec.Config(), last)))
	return res, nil
}

func (api *EthAPI) rewards(ctx context.Context, h *types.Header, percentiles []float64) []*hexutil.Big {
	out := make([]*hexutil.Big, len(percentiles))
	for i := range out {
		out[i] = (*hexutil.Big)(new(big.Int))
	}
	body := api.b.Chain.Body(h.Hash())
	receipts := api.b.Chain.Receipts(h.Hash())
	if body == nil || len(body.Transactions) == 0 || len(receipts) != len(body.Transactions) {
		return out
	}
	type txGas struct {
		gas uint64
		tip *big.Int
	}
	list := make([]txGas, len(body.Transactions))
	for i, tx := range body.Transactions {
		tip, _ := tx.EffectiveGasTip(h.BaseFee)
		list[i] = txGas{gas: receipts[i].GasUsed, tip: tip}
	}
	sort.Slice(list, func(a, b int) bool { return list[a].tip.Cmp(list[b].tip) < 0 })
	var idx int
	sum := list[0].gas
	for i, p := range percentiles {
		threshold := uint64(float64(h.GasUsed) * p / 100)
		for sum < threshold && idx < len(list)-1 {
			idx++
			sum += list[idx].gas
		}
		out[i] = (*hexutil.Big)(list[idx].tip)
	}
	return out
}

// FilterQuery is the eth_getLogs filter.
type FilterQuery struct {
	BlockHash *common.Hash      `json:"blockHash"`
	FromBlock *rpc.BlockNumber  `json:"fromBlock"`
	ToBlock   *rpc.BlockNumber  `json:"toBlock"`
	Address   json.RawMessage   `json:"address"`
	Topics    []json.RawMessage `json:"topics"`
}

func parseAddresses(raw json.RawMessage) ([]common.Address, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var one common.Address
	if err := json.Unmarshal(raw, &one); err == nil {
		return []common.Address{one}, nil
	}
	var many []common.Address
	if err := json.Unmarshal(raw, &many); err != nil {
		return nil, fmt.Errorf("invalid address filter: %w", err)
	}
	return many, nil
}

func parseTopics(raws []json.RawMessage) ([][]common.Hash, error) {
	out := make([][]common.Hash, len(raws))
	for i, raw := range raws {
		if len(raw) == 0 || string(raw) == "null" {
			continue
		}
		var one common.Hash
		if err := json.Unmarshal(raw, &one); err == nil {
			out[i] = []common.Hash{one}
			continue
		}
		var many []*common.Hash
		if err := json.Unmarshal(raw, &many); err != nil {
			return nil, fmt.Errorf("invalid topic filter: %w", err)
		}
		for _, h := range many {
			if h == nil {
				out[i] = nil // wildcard
				break
			}
			out[i] = append(out[i], *h)
		}
	}
	return out, nil
}

func matchLog(l *types.Log, addrs []common.Address, topics [][]common.Hash) bool {
	if len(addrs) > 0 {
		ok := false
		for _, a := range addrs {
			if l.Address == a {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	if len(topics) > len(l.Topics) {
		return false
	}
	for i, alts := range topics {
		if len(alts) == 0 {
			continue
		}
		ok := false
		for _, t := range alts {
			if l.Topics[i] == t {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

func bloomMatch(b types.Bloom, addrs []common.Address, topics [][]common.Hash) bool {
	if len(addrs) > 0 {
		ok := false
		for _, a := range addrs {
			if types.BloomLookup(b, a) {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	for _, alts := range topics {
		if len(alts) == 0 {
			continue
		}
		ok := false
		for _, t := range alts {
			if types.BloomLookup(b, t) {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

const maxLogs = 10000

// GetLogs implements eth_getLogs for blocks inside the logs window.
func (api *EthAPI) GetLogs(ctx context.Context, q FilterQuery) (any, error) {
	addrs, err := parseAddresses(q.Address)
	if err != nil {
		return nil, err
	}
	topics, err := parseTopics(q.Topics)
	if err != nil {
		return nil, err
	}
	var hashes []common.Hash
	if q.BlockHash != nil {
		if api.b.Chain.Header(*q.BlockHash) == nil {
			return nil, fmt.Errorf("block %x: %w", *q.BlockHash, ErrNotInWindow)
		}
		hashes = []common.Hash{*q.BlockHash}
	} else {
		head := api.b.Chain.Head()
		if head == nil {
			return nil, errNoHead
		}
		resolveNum := func(p *rpc.BlockNumber) (uint64, error) {
			if p == nil {
				return head.Number.Uint64(), nil
			}
			if *p < 0 {
				h, err := api.headerByNumber(*p)
				if err != nil {
					return 0, err
				}
				if h == nil {
					return 0, errors.New("unknown block tag")
				}
				return h.Number.Uint64(), nil
			}
			return uint64(p.Int64()), nil
		}
		from, err := resolveNum(q.FromBlock)
		if err != nil {
			return nil, err
		}
		to, err := resolveNum(q.ToBlock)
		if err != nil {
			return nil, err
		}
		if to > head.Number.Uint64() {
			to = head.Number.Uint64()
		}
		if from > to {
			return []*types.Log{}, nil
		}
		// The consensus client's deposit-contract queries are served from the
		// locally rebuilt deposit history.
		if api.b.Engine && api.b.Deposits != nil && len(addrs) == 1 && addrs[0] == deposits.Contract {
			if api.b.Deposits.Covers(from, to) {
				return api.b.Deposits.Query(from, to, topics)
			}
			if from < api.b.Chain.LowestBodies() {
				return api.b.Deposits.Query(from, to, topics) // returns ErrIncomplete with coverage
			}
		}
		low := api.b.Chain.LowestBodies()
		if from < low {
			return nil, fmt.Errorf("fromBlock #%d: %w (logs available from #%d)", from, ErrNotInWindow, low)
		}
		for n := from; n <= to; n++ {
			h, ok := api.b.Chain.CanonicalHash(n)
			if !ok {
				return nil, fmt.Errorf("block #%d: %w", n, ErrNotInWindow)
			}
			hashes = append(hashes, h)
		}
	}
	logs := []*types.Log{}
	for _, hash := range hashes {
		h := api.b.Chain.Header(hash)
		if h == nil || !bloomMatch(h.Bloom, addrs, topics) {
			continue
		}
		receipts, err := api.receipts(ctx, hash)
		if err != nil {
			return nil, fmt.Errorf("receipts for #%d: %w", h.Number, err)
		}
		for _, r := range receipts {
			for _, l := range r.Logs {
				if matchLog(l, addrs, topics) {
					logs = append(logs, l)
					if len(logs) > maxLogs {
						return nil, fmt.Errorf("query returned more than %d results", maxLogs)
					}
				}
			}
		}
	}
	return logs, nil
}

// MinimalEthAPI is exposed on the public port in validator mode.
type MinimalEthAPI struct{ full *EthAPI }

// NewMinimalEthAPI wraps the full API exposing only status methods.
func NewMinimalEthAPI(full *EthAPI) *MinimalEthAPI { return &MinimalEthAPI{full: full} }

func (m *MinimalEthAPI) ChainId() *hexutil.Big                { return m.full.ChainId() }
func (m *MinimalEthAPI) BlockNumber() (hexutil.Uint64, error) { return m.full.BlockNumber() }
func (m *MinimalEthAPI) Syncing() (any, error)                { return m.full.Syncing() }

// NetAPI implements the net namespace.
type NetAPI struct{ chainID uint64 }

// NewNetAPI creates the net namespace handler.
func NewNetAPI(chainID uint64) *NetAPI { return &NetAPI{chainID: chainID} }

func (n *NetAPI) Version() string         { return fmt.Sprint(n.chainID) }
func (n *NetAPI) Listening() bool         { return true }
func (n *NetAPI) PeerCount() hexutil.Uint { return 0 }

// Web3API implements the web3 namespace.
type Web3API struct{}

func (Web3API) ClientVersion() string                  { return ClientVersion }
func (Web3API) Sha3(input hexutil.Bytes) hexutil.Bytes { return crypto.Keccak256(input) }

// IsNotInWindow reports whether err is a window error.
func IsNotInWindow(err error) bool {
	return err != nil && (errors.Is(err, ErrNotInWindow) || strings.Contains(err.Error(), "not available"))
}
