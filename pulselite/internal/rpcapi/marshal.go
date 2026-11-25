package rpcapi

import (
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
)

// marshalHeader mirrors go-ethereum's RPCMarshalHeader.
func marshalHeader(h *types.Header) map[string]any {
	out := map[string]any{
		"number":           (*hexutil.Big)(h.Number),
		"hash":             h.Hash(),
		"parentHash":       h.ParentHash,
		"nonce":            h.Nonce,
		"mixHash":          h.MixDigest,
		"sha3Uncles":       h.UncleHash,
		"logsBloom":        h.Bloom,
		"stateRoot":        h.Root,
		"miner":            h.Coinbase,
		"difficulty":       (*hexutil.Big)(h.Difficulty),
		"extraData":        hexutil.Bytes(h.Extra),
		"gasLimit":         hexutil.Uint64(h.GasLimit),
		"gasUsed":          hexutil.Uint64(h.GasUsed),
		"timestamp":        hexutil.Uint64(h.Time),
		"transactionsRoot": h.TxHash,
		"receiptsRoot":     h.ReceiptHash,
	}
	if h.BaseFee != nil {
		out["baseFeePerGas"] = (*hexutil.Big)(h.BaseFee)
	}
	if h.WithdrawalsHash != nil {
		out["withdrawalsRoot"] = h.WithdrawalsHash
	}
	if h.BlobGasUsed != nil {
		out["blobGasUsed"] = hexutil.Uint64(*h.BlobGasUsed)
	}
	if h.ExcessBlobGas != nil {
		out["excessBlobGas"] = hexutil.Uint64(*h.ExcessBlobGas)
	}
	if h.ParentBeaconRoot != nil {
		out["parentBeaconBlockRoot"] = h.ParentBeaconRoot
	}
	if h.RequestsHash != nil {
		out["requestsHash"] = h.RequestsHash
	}
	return out
}

// marshalBlock mirrors RPCMarshalBlock. body may be nil (header only).
func marshalBlock(h *types.Header, body *types.Body, fullTx bool, cfg *params.ChainConfig) map[string]any {
	out := marshalHeader(h)
	out["uncles"] = []common.Hash{}
	if body == nil {
		return out
	}
	block := types.NewBlockWithHeader(h).WithBody(*body)
	out["size"] = hexutil.Uint64(block.Size())
	txs := block.Transactions()
	if fullTx {
		list := make([]*RPCTransaction, len(txs))
		for i, tx := range txs {
			list[i] = newRPCTransaction(tx, h.Hash(), h.Number.Uint64(), h.Time, uint64(i), h.BaseFee, cfg)
		}
		out["transactions"] = list
	} else {
		list := make([]common.Hash, len(txs))
		for i, tx := range txs {
			list[i] = tx.Hash()
		}
		out["transactions"] = list
	}
	if body.Withdrawals != nil {
		out["withdrawals"] = body.Withdrawals
	}
	return out
}

// RPCTransaction is the JSON representation of a transaction.
type RPCTransaction struct {
	BlockHash           *common.Hash      `json:"blockHash"`
	BlockNumber         *hexutil.Big      `json:"blockNumber"`
	From                common.Address    `json:"from"`
	Gas                 hexutil.Uint64    `json:"gas"`
	GasPrice            *hexutil.Big      `json:"gasPrice"`
	GasFeeCap           *hexutil.Big      `json:"maxFeePerGas,omitempty"`
	GasTipCap           *hexutil.Big      `json:"maxPriorityFeePerGas,omitempty"`
	Hash                common.Hash       `json:"hash"`
	Input               hexutil.Bytes     `json:"input"`
	Nonce               hexutil.Uint64    `json:"nonce"`
	To                  *common.Address   `json:"to"`
	TransactionIndex    *hexutil.Uint64   `json:"transactionIndex"`
	Value               *hexutil.Big      `json:"value"`
	Type                hexutil.Uint64    `json:"type"`
	Accesses            *types.AccessList `json:"accessList,omitempty"`
	ChainID             *hexutil.Big      `json:"chainId,omitempty"`
	V                   *hexutil.Big      `json:"v"`
	R                   *hexutil.Big      `json:"r"`
	S                   *hexutil.Big      `json:"s"`
	YParity             *hexutil.Uint64   `json:"yParity,omitempty"`
	BlobVersionedHashes []common.Hash     `json:"blobVersionedHashes,omitempty"`
}

func newRPCTransaction(tx *types.Transaction, blockHash common.Hash, number, time, index uint64, baseFee *big.Int, cfg *params.ChainConfig) *RPCTransaction {
	signer := types.MakeSigner(cfg, new(big.Int).SetUint64(number), time)
	from, _ := types.Sender(signer, tx)
	v, r, s := tx.RawSignatureValues()
	result := &RPCTransaction{
		Type:     hexutil.Uint64(tx.Type()),
		From:     from,
		Gas:      hexutil.Uint64(tx.Gas()),
		GasPrice: (*hexutil.Big)(tx.GasPrice()),
		Hash:     tx.Hash(),
		Input:    hexutil.Bytes(tx.Data()),
		Nonce:    hexutil.Uint64(tx.Nonce()),
		To:       tx.To(),
		Value:    (*hexutil.Big)(tx.Value()),
		V:        (*hexutil.Big)(v),
		R:        (*hexutil.Big)(r),
		S:        (*hexutil.Big)(s),
	}
	if blockHash != (common.Hash{}) {
		bh := blockHash
		result.BlockHash = &bh
		result.BlockNumber = (*hexutil.Big)(new(big.Int).SetUint64(number))
		idx := hexutil.Uint64(index)
		result.TransactionIndex = &idx
	}
	switch tx.Type() {
	case types.LegacyTxType:
		if id := tx.ChainId(); id.Sign() != 0 {
			result.ChainID = (*hexutil.Big)(id)
		}
	default:
		al := tx.AccessList()
		yparity := hexutil.Uint64(v.Sign())
		result.Accesses = &al
		result.ChainID = (*hexutil.Big)(tx.ChainId())
		result.YParity = &yparity
		if tx.Type() != types.AccessListTxType {
			result.GasFeeCap = (*hexutil.Big)(tx.GasFeeCap())
			result.GasTipCap = (*hexutil.Big)(tx.GasTipCap())
			if baseFee != nil && blockHash != (common.Hash{}) {
				price := new(big.Int).Add(tx.GasTipCap(), baseFee)
				if price.Cmp(tx.GasFeeCap()) > 0 {
					price = tx.GasFeeCap()
				}
				result.GasPrice = (*hexutil.Big)(price)
			} else {
				result.GasPrice = (*hexutil.Big)(tx.GasFeeCap())
			}
		}
		if tx.Type() == types.BlobTxType {
			result.BlobVersionedHashes = tx.BlobHashes()
		}
	}
	return result
}

// marshalReceipt mirrors go-ethereum's MarshalReceipt.
func marshalReceipt(r *types.Receipt, tx *types.Transaction, signer types.Signer) map[string]any {
	from, _ := types.Sender(signer, tx)
	out := map[string]any{
		"blockHash":         r.BlockHash,
		"blockNumber":       hexutil.Uint64(r.BlockNumber.Uint64()),
		"transactionHash":   r.TxHash,
		"transactionIndex":  hexutil.Uint64(r.TransactionIndex),
		"from":              from,
		"to":                tx.To(),
		"gasUsed":           hexutil.Uint64(r.GasUsed),
		"cumulativeGasUsed": hexutil.Uint64(r.CumulativeGasUsed),
		"contractAddress":   nil,
		"logs":              r.Logs,
		"logsBloom":         r.Bloom,
		"type":              hexutil.Uint(tx.Type()),
		"effectiveGasPrice": (*hexutil.Big)(r.EffectiveGasPrice),
	}
	if len(r.PostState) > 0 {
		out["root"] = hexutil.Bytes(r.PostState)
	} else {
		out["status"] = hexutil.Uint(r.Status)
	}
	if r.Logs == nil {
		out["logs"] = []*types.Log{}
	}
	if r.ContractAddress != (common.Address{}) {
		out["contractAddress"] = r.ContractAddress
	}
	return out
}
