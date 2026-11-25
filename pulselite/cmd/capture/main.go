// Command capture records PulseChain mainnet data into test fixtures:
//
//	capture proofs -out testdata/mainnet_proofs.json
//	capture block  -number N -out testdata/mainnet_block.json
//
// "proofs" stores a header and raw eth_getProof responses for a few
// well-known accounts. "block" executes a recent block with the real
// executor against live upstreams while recording every witness item, so the
// block can later be re-validated offline.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb/memorydb"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/trie"
	"github.com/pulsechainlite/pulselite/internal/chain"
	"github.com/pulsechainlite/pulselite/internal/exec"
	"github.com/pulsechainlite/pulselite/internal/proof"
	"github.com/pulsechainlite/pulselite/internal/upstream"
	"github.com/pulsechainlite/pulselite/internal/verified"
)

// ProofFixture is the format of testdata/mainnet_proofs.json.
type ProofFixture struct {
	Header  *types.Header       `json:"header"`
	Entries []ProofFixtureEntry `json:"entries"`
}

// ProofFixtureEntry is one eth_getProof call.
type ProofFixtureEntry struct {
	Address common.Address       `json:"address"`
	Slots   []common.Hash        `json:"slots"`
	Result  *proof.AccountResult `json:"result"`
}

// BlockFixture is the format of testdata/mainnet_block.json.
type BlockFixture struct {
	Block   hexutil.Bytes     `json:"block"` // RLP
	Parent  *types.Header     `json:"parent"`
	Witness *verified.Fixture `json:"witness"`
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: capture proofs|block [flags]")
		os.Exit(2)
	}
	fs := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	up := fs.String("upstream", "https://pulsechain-rpc.publicnode.com,https://rpc.pulsechainrpc.com", "upstreams")
	out := fs.String("out", "", "output file")
	number := fs.Int64("number", -1, "block number (default head-4)")
	count := fs.Int("count", 1, "block: validate this many consecutive blocks with a shared warm cache (benchmark)")
	beaconURL := fs.String("beacon", "http://host.docker.internal:3500", "p2p: beacon API used for the trusted head state root")
	fs.Parse(os.Args[2:])
	benchCount = *count
	log.SetDefault(log.NewLogger(log.NewTerminalHandlerWithLevel(os.Stderr, slog.LevelInfo, false)))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if os.Args[1] == "p2p" {
		if err := p2pSmoke(ctx, *beaconURL); err != nil {
			fail(err)
		}
		return
	}
	pool, err := upstream.NewPool(strings.Split(*up, ","))
	if err != nil {
		fail(err)
	}
	pool.Probe(context.Background())
	switch os.Args[1] {
	case "proofs":
		err = captureProofs(ctx, pool, *out)
	case "block":
		err = captureBlock(ctx, pool, *number, *out)
	default:
		err = errors.New("unknown subcommand")
	}
	if err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}

func head(ctx context.Context, pool *upstream.Pool) (uint64, error) {
	var n hexutil.Uint64
	err := pool.Call(ctx, &n, true, "eth_blockNumber")
	return uint64(n), err
}

func headerByNumber(ctx context.Context, pool *upstream.Pool, n uint64) (*types.Header, error) {
	var raw json.RawMessage
	if err := pool.Call(ctx, &raw, true, "eth_getBlockByNumber", hexutil.EncodeUint64(n), false); err != nil {
		return nil, err
	}
	h := new(types.Header)
	return h, json.Unmarshal(raw, h)
}

func captureProofs(ctx context.Context, pool *upstream.Pool, out string) error {
	hn, err := head(ctx, pool)
	if err != nil {
		return err
	}
	h, err := headerByNumber(ctx, pool, hn-16)
	if err != nil {
		return err
	}
	wpls := common.HexToAddress("0xA1077a294dDE1B09bB078844df40758a5D0f9a27")
	hex := common.HexToAddress("0x2b591e99afE9f32eAA6214f7B7629768c40Eeb39")
	// balanceOf mapping (slot 3 in WETH9-style contracts) for the WPLS contract itself.
	balSlot := crypto.Keccak256Hash(common.LeftPadBytes(wpls.Bytes(), 32), common.LeftPadBytes([]byte{3}, 32))
	entries := []ProofFixtureEntry{
		{Address: wpls, Slots: []common.Hash{{}, common.BigToHash(common.Big1), common.BigToHash(common.Big2), balSlot}},
		{Address: hex, Slots: []common.Hash{{}, common.BigToHash(common.Big1)}},
		{Address: h.Coinbase},
		{Address: common.BytesToAddress(crypto.Keccak256([]byte("pulselite-absent-account")))},
		{Address: common.HexToAddress("0x1234567890abcdef1234567890abcdef12345678"), Slots: []common.Hash{{}}},
	}
	be := &verified.RPCBackend{Pool: pool}
	at := verified.BlockRef{Hash: h.Hash(), Number: h.Number.Uint64()}
	for i := range entries {
		res, err := be.Proofs(ctx, at, []verified.ProofRequest{{Address: entries[i].Address, Slots: entries[i].Slots}})
		if err != nil {
			return err
		}
		if _, err := proof.VerifyAccount(h.Root, entries[i].Address, res[0]); err != nil {
			return fmt.Errorf("live proof for %s failed verification: %w", entries[i].Address, err)
		}
		entries[i].Result = res[0]
	}
	data, _ := json.MarshalIndent(ProofFixture{Header: h, Entries: entries}, "", " ")
	fmt.Printf("captured proofs at block %d root %x\n", h.Number, h.Root)
	return os.WriteFile(out, data, 0o644)
}

func fetchBlock(ctx context.Context, pool *upstream.Pool, n uint64) (*types.Block, error) {
	var raw json.RawMessage
	if err := pool.Call(ctx, &raw, true, "eth_getBlockByNumber", hexutil.EncodeUint64(n), true); err != nil {
		return nil, err
	}
	h := new(types.Header)
	if err := json.Unmarshal(raw, h); err != nil {
		return nil, err
	}
	var body struct {
		Transactions []*types.Transaction `json:"transactions"`
		Withdrawals  []*types.Withdrawal  `json:"withdrawals"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, err
	}
	if types.DeriveSha(types.Transactions(body.Transactions), trie.NewStackTrie(nil)) != h.TxHash {
		return nil, errors.New("tx root mismatch")
	}
	if body.Withdrawals == nil && h.WithdrawalsHash != nil {
		body.Withdrawals = []*types.Withdrawal{}
	}
	return types.NewBlockWithHeader(h).WithBody(types.Body{Transactions: body.Transactions, Withdrawals: body.Withdrawals}), nil
}

func captureBlock(ctx context.Context, pool *upstream.Pool, number int64, out string) error {
	n := uint64(number)
	if number < 0 {
		hn, err := head(ctx, pool)
		if err != nil {
			return err
		}
		n = hn - 4
	}
	block, err := fetchBlock(ctx, pool, n)
	if err != nil {
		return err
	}
	parent, err := headerByNumber(ctx, pool, n-1)
	if err != nil {
		return err
	}
	if parent.Hash() != block.ParentHash() {
		return errors.New("parent hash mismatch")
	}
	ch, err := chain.Open("", params.PulseChainConfig, chain.Windows{Headers: 8192, Logs: 1024, Reorg: 128})
	if err != nil {
		return err
	}
	ch.AddHeader(parent)
	rec := verified.NewRecorder(&verified.RPCBackend{Pool: pool})
	src := verified.NewSource(memorydb.New(), rec, ch)
	ex := exec.New(params.PulseChainConfig, src, ch, 32)
	start := time.Now()
	res, err := ex.ExecuteBlock(ctx, block, parent)
	if err != nil {
		return err
	}
	if err := exec.Validate(block, res); err != nil {
		return fmt.Errorf("live execution of #%d does not match header: %w", n, err)
	}
	fmt.Printf("block %d: %d txs, gas %d, rounds %d, fetched %d, %s -> VALID root %x\n",
		n, len(block.Transactions()), block.GasUsed(), res.Stats.Rounds, res.Stats.Fetched, time.Since(start).Round(time.Millisecond), res.Root)
	enc, err := rlp.EncodeToBytes(block)
	if err != nil {
		return err
	}
	data, _ := json.Marshal(BlockFixture{Block: enc, Parent: parent, Witness: rec.F})
	if err := os.WriteFile(out, data, 0o644); err != nil {
		return err
	}
	// Benchmark: keep validating the following blocks on the warm cache,
	// persisting each post-state like the node does in full mode.
	if err := src.Persist(res.State, n, true); err != nil {
		fmt.Println("persist:", err)
	}
	var total time.Duration
	for i := 1; i < benchCount; i++ {
		next := n + uint64(i)
		for {
			hn, err := head(ctx, pool)
			if err == nil && hn >= next+1 {
				break
			}
			time.Sleep(2 * time.Second)
		}
		b, err := fetchBlock(ctx, pool, next)
		if err != nil {
			return err
		}
		ch.AddBlock(block)
		p := block.Header()
		start := time.Now()
		r, err := ex.ExecuteBlock(ctx, b, p)
		if err == nil {
			err = exec.Validate(b, r)
		}
		if err != nil {
			return fmt.Errorf("warm block %d: %w", next, err)
		}
		el := time.Since(start)
		total += el
		fmt.Printf("warm block %d: %d txs, gas %d, rounds %d, fetched %d, %s -> VALID\n",
			next, len(b.Transactions()), b.GasUsed(), r.Stats.Rounds, r.Stats.Fetched, el.Round(time.Millisecond))
		if err := src.Persist(r.State, next, true); err != nil {
			fmt.Println("persist:", err)
		}
		block = b
	}
	if benchCount > 1 {
		fmt.Printf("warm average: %s over %d blocks\n", (total / time.Duration(benchCount-1)).Round(time.Millisecond), benchCount-1)
	}
	return nil
}

var benchCount = 1
