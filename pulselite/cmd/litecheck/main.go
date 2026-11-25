// Command litecheck verifies a running pulselite deployment:
//
//	litecheck compare -local URL -ref URL      functional RPC comparison vs a reference node
//	litecheck disk -dirs a,b -limit 25GB       disk measurement with a hard limit (exit 1)
//	litecheck load -rpc URL -n 2000            cache load generator
//	litecheck health -url URL                  print /health
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math/big"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/pulsechainlite/pulselite/internal/config"
	"github.com/pulsechainlite/pulselite/internal/disk"
)

var (
	wpls = common.HexToAddress("0xA1077a294dDE1B09bB078844df40758a5D0f9a27")
	hex  = common.HexToAddress("0x2b591e99afE9f32eAA6214f7B7629768c40Eeb39")
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: litecheck compare|disk|load|health [flags]")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "compare":
		err = compare(os.Args[2:])
	case "disk":
		err = diskCmd(os.Args[2:])
	case "load":
		err = load(os.Args[2:])
	case "health":
		err = health(os.Args[2:])
	default:
		err = errors.New("unknown subcommand " + os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "FAIL:", err)
		os.Exit(1)
	}
}

type check struct {
	name       string
	total, bad int
	latency    time.Duration
	samples    []string
}

type checker struct {
	local, ref *rpc.Client
	checks     map[string]*check
	order      []string
}

func (c *checker) get(name string) *check {
	if c.checks[name] == nil {
		c.checks[name] = &check{name: name}
		c.order = append(c.order, name)
	}
	return c.checks[name]
}

// both calls method on local and reference and compares with eq.
func (c *checker) both(ctx context.Context, name string, eq func(l, r json.RawMessage) bool, method string, args ...any) (json.RawMessage, json.RawMessage) {
	ch := c.get(name)
	var l, r json.RawMessage
	start := time.Now()
	lerr := c.local.CallContext(ctx, &l, method, args...)
	ch.latency += time.Since(start)
	rerr := c.ref.CallContext(ctx, &r, method, args...)
	ch.total++
	if rerr != nil {
		ch.total-- // reference failure: not a pulselite mismatch
		return l, r
	}
	if lerr != nil || !eq(l, r) {
		ch.bad++
		if len(ch.samples) < 3 {
			ch.samples = append(ch.samples, fmt.Sprintf("%s %v: local=%s err=%v ref=%s", method, args, trunc(l), lerr, trunc(r)))
		}
	}
	return l, r
}

func trunc(b []byte) string {
	if len(b) > 120 {
		return string(b[:120]) + "..."
	}
	return string(b)
}

func eqJSON(l, r json.RawMessage) bool {
	var a, b any
	json.Unmarshal(l, &a)
	json.Unmarshal(r, &b)
	return normalize(a) == normalize(b)
}

func normalize(v any) string {
	switch x := v.(type) {
	case string:
		if strings.HasPrefix(x, "0x") {
			if n, ok := new(big.Int).SetString(x[2:], 16); ok && len(x) <= 66 {
				return "0x" + n.Text(16)
			}
		}
		return strings.ToLower(x)
	default:
		b, _ := json.Marshal(v)
		return strings.ToLower(string(b))
	}
}

func eqCode(l, r json.RawMessage) bool {
	var a, b hexutil.Bytes
	json.Unmarshal(l, &a)
	json.Unmarshal(r, &b)
	return bytes.Equal(crypto.Keccak256(a), crypto.Keccak256(b))
}

func eqWithin(pct float64) func(l, r json.RawMessage) bool {
	return func(l, r json.RawMessage) bool {
		var a, b hexutil.Uint64
		if json.Unmarshal(l, &a) != nil || json.Unmarshal(r, &b) != nil {
			return false
		}
		d := float64(a) - float64(b)
		if d < 0 {
			d = -d
		}
		return d <= float64(b)*pct/100
	}
}

func compare(args []string) error {
	fs := flag.NewFlagSet("compare", flag.ExitOnError)
	localURL := fs.String("local", "http://127.0.0.1:8545", "pulselite RPC")
	refURL := fs.String("ref", "https://rpc.pulsechain.com", "reference full/archive node")
	maxAcc := fs.Int("accounts", 25, "accounts sampled from the block")
	lag := fs.Uint64("lag", 2, "compare at local head minus lag")
	fs.Parse(args)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	local, err := rpc.DialContext(ctx, *localURL)
	if err != nil {
		return err
	}
	ref, err := rpc.DialContext(ctx, *refURL)
	if err != nil {
		return err
	}
	c := &checker{local: local, ref: ref, checks: map[string]*check{}}

	var head hexutil.Uint64
	if err := local.CallContext(ctx, &head, "eth_blockNumber"); err != nil {
		return fmt.Errorf("local eth_blockNumber: %w", err)
	}
	var refHead hexutil.Uint64
	ref.CallContext(ctx, &refHead, "eth_blockNumber")
	n := uint64(head) - *lag
	tag := hexutil.EncodeUint64(n)
	fmt.Printf("local head %d, reference head %d, comparing at block %d\n", head, refHead, n)

	lb, _ := c.both(ctx, "eth_getBlockByNumber(hash)", func(l, r json.RawMessage) bool {
		var a, b struct {
			Hash common.Hash `json:"hash"`
		}
		json.Unmarshal(l, &a)
		json.Unmarshal(r, &b)
		return a.Hash == b.Hash && a.Hash != (common.Hash{})
	}, "eth_getBlockByNumber", tag, true)
	var block struct {
		Miner        common.Address `json:"miner"`
		Transactions []struct {
			Hash common.Hash     `json:"hash"`
			From common.Address  `json:"from"`
			To   *common.Address `json:"to"`
		} `json:"transactions"`
	}
	json.Unmarshal(lb, &block)

	seen := map[common.Address]bool{}
	accounts := []common.Address{wpls, hex, block.Miner, common.BytesToAddress(crypto.Keccak256([]byte("litecheck-absent")))}
	for _, a := range accounts {
		seen[a] = true
	}
	for _, tx := range block.Transactions {
		for _, a := range []*common.Address{&tx.From, tx.To} {
			if a != nil && !seen[*a] && len(accounts) < *maxAcc {
				seen[*a] = true
				accounts = append(accounts, *a)
			}
		}
	}
	fmt.Printf("sampling %d accounts from block %d (%d txs)\n", len(accounts), n, len(block.Transactions))
	for _, a := range accounts {
		c.both(ctx, "eth_getBalance", eqJSON, "eth_getBalance", a, tag)
		c.both(ctx, "eth_getTransactionCount", eqJSON, "eth_getTransactionCount", a, tag)
		c.both(ctx, "eth_getCode", eqCode, "eth_getCode", a, tag)
		bal := crypto.Keccak256Hash(common.LeftPadBytes(a.Bytes(), 32), common.LeftPadBytes([]byte{3}, 32))
		c.both(ctx, "eth_getStorageAt", eqJSON, "eth_getStorageAt", wpls, bal, tag)
		data := append(common.FromHex("0x70a08231"), common.LeftPadBytes(a.Bytes(), 32)...)
		c.both(ctx, "eth_call", eqJSON, "eth_call", map[string]any{"to": wpls, "data": hexutil.Bytes(data)}, tag)
		hexBal := append(common.FromHex("0x70a08231"), common.LeftPadBytes(a.Bytes(), 32)...)
		c.both(ctx, "eth_call", eqJSON, "eth_call", map[string]any{"to": hex, "data": hexutil.Bytes(hexBal)}, tag)
		transfer := append(append(common.FromHex("0xa9059cbb"), common.LeftPadBytes(a.Bytes(), 32)...), make([]byte, 32)...)
		c.both(ctx, "eth_estimateGas", eqWithin(3), "eth_estimateGas", map[string]any{"from": a, "to": wpls, "data": hexutil.Bytes(transfer)}, tag)
	}
	for _, s := range []int64{0, 1, 2} {
		c.both(ctx, "eth_getStorageAt", eqJSON, "eth_getStorageAt", wpls, hexutil.EncodeBig(big.NewInt(s)), tag)
		c.both(ctx, "eth_getStorageAt", eqJSON, "eth_getStorageAt", hex, hexutil.EncodeBig(big.NewInt(s)), tag)
	}
	for _, sel := range []string{"0x18160ddd", "0x06fdde03", "0x313ce567"} { // totalSupply, name, decimals
		c.both(ctx, "eth_call", eqJSON, "eth_call", map[string]any{"to": wpls, "data": sel}, tag)
		c.both(ctx, "eth_call", eqJSON, "eth_call", map[string]any{"to": hex, "data": sel}, tag)
	}
	c.both(ctx, "eth_getLogs", func(l, r json.RawMessage) bool {
		type lg struct {
			TxHash   common.Hash    `json:"transactionHash"`
			LogIndex hexutil.Uint   `json:"logIndex"`
			Address  common.Address `json:"address"`
		}
		var a, b []lg
		json.Unmarshal(l, &a)
		json.Unmarshal(r, &b)
		if len(a) != len(b) {
			return false
		}
		for i := range a {
			if a[i] != b[i] {
				return false
			}
		}
		return true
	}, "eth_getLogs", map[string]any{"fromBlock": tag, "toBlock": tag})
	for i, tx := range block.Transactions {
		if i >= 5 {
			break
		}
		c.both(ctx, "eth_getTransactionReceipt", func(l, r json.RawMessage) bool {
			type rc struct {
				Status  hexutil.Uint64 `json:"status"`
				GasUsed hexutil.Uint64 `json:"gasUsed"`
				Logs    []any          `json:"logs"`
				Block   common.Hash    `json:"blockHash"`
			}
			var a, b rc
			json.Unmarshal(l, &a)
			json.Unmarshal(r, &b)
			return a.Status == b.Status && a.GasUsed == b.GasUsed && len(a.Logs) == len(b.Logs) && a.Block == b.Block
		}, "eth_getTransactionReceipt", tx.Hash)
	}
	c.both(ctx, "eth_chainId", eqJSON, "eth_chainId")
	// Latest-state sanity (no comparison: heads may differ by a block).
	var latest hexutil.Big
	lc := c.get("eth_getBalance(latest, local only)")
	lc.total++
	if err := local.CallContext(ctx, &latest, "eth_getBalance", wpls, "latest"); err != nil {
		lc.bad++
		lc.samples = append(lc.samples, err.Error())
	}

	failed := 0
	fmt.Printf("\n%-36s %7s %9s %12s\n", "method", "checks", "mismatch", "avg latency")
	for _, name := range c.order {
		ch := c.checks[name]
		avg := time.Duration(0)
		if ch.total > 0 {
			avg = ch.latency / time.Duration(ch.total)
		}
		fmt.Printf("%-36s %7d %9d %12s\n", name, ch.total, ch.bad, avg.Round(time.Millisecond))
		for _, s := range ch.samples {
			fmt.Println("    ", s)
		}
		failed += ch.bad
	}
	if failed > 0 {
		return fmt.Errorf("%d mismatches", failed)
	}
	fmt.Println("\nPASS: all compared results match the reference node")
	return nil
}

func diskCmd(args []string) error {
	fs := flag.NewFlagSet("disk", flag.ExitOnError)
	dirs := fs.String("dirs", "/blockchain/consensus,/blockchain/execution", "directories to measure")
	limit := fs.String("limit", "25GB", "fail if the total exceeds this")
	target := fs.String("target", "20GB", "steady-state target (reported, not enforced)")
	csv := fs.String("csv", "", "append a CSV row to this file")
	fs.Parse(args)
	lim, err := config.ParseSize(*limit)
	if err != nil {
		return err
	}
	tgt, _ := config.ParseSize(*target)
	var total uint64
	var cols []string
	for _, d := range strings.Split(*dirs, ",") {
		u := disk.Usage(d)
		total += u
		cols = append(cols, fmt.Sprint(u))
		fmt.Printf("%-28s %10.3f GB\n", d, float64(u)/1e9)
	}
	var st syscall.Statfs_t
	free := uint64(0)
	if syscall.Statfs(strings.Split(*dirs, ",")[0], &st) == nil {
		free = st.Bavail * uint64(st.Bsize)
	}
	fmt.Printf("%-28s %10.3f GB   (target %.1f GB, hard limit %.1f GB, fs free %.1f GB)\n", "TOTAL", float64(total)/1e9, float64(tgt)/1e9, float64(lim)/1e9, float64(free)/1e9)
	if *csv != "" {
		os.MkdirAll(filepath.Dir(*csv), 0o755)
		newFile := false
		if _, err := os.Stat(*csv); errors.Is(err, os.ErrNotExist) {
			newFile = true
		}
		f, err := os.OpenFile(*csv, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err == nil {
			if newFile {
				fmt.Fprintf(f, "timestamp,%s,total\n", *dirs)
			}
			fmt.Fprintf(f, "%s,%s,%d\n", time.Now().UTC().Format(time.RFC3339), strings.Join(cols, ","), total)
			f.Close()
		}
	}
	if total > lim {
		return fmt.Errorf("disk usage %.3f GB exceeds hard limit %.1f GB", float64(total)/1e9, float64(lim)/1e9)
	}
	if total > tgt {
		fmt.Println("WARN: above steady-state target")
	} else {
		fmt.Println("OK: within target")
	}
	return nil
}

func load(args []string) error {
	fs := flag.NewFlagSet("load", flag.ExitOnError)
	url := fs.String("rpc", "http://127.0.0.1:8545", "pulselite RPC")
	n := fs.Int("n", 2000, "requests")
	conc := fs.Int("concurrency", 8, "parallel workers")
	seed := fs.Int64("seed", time.Now().UnixNano(), "random seed")
	fs.Parse(args)
	ctx := context.Background()
	c, err := rpc.DialContext(ctx, *url)
	if err != nil {
		return err
	}
	rng := rand.New(rand.NewSource(*seed))
	var mu sync.Mutex
	nextAddr := func() common.Address {
		mu.Lock()
		defer mu.Unlock()
		var a common.Address
		rng.Read(a[:])
		return a
	}
	var ok, fail atomic.Int64
	var lat []time.Duration
	var wg sync.WaitGroup
	jobs := make(chan int)
	start := time.Now()
	for w := 0; w < *conc; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				a := nextAddr()
				t0 := time.Now()
				var err error
				var out json.RawMessage
				switch i % 3 {
				case 0:
					err = c.CallContext(ctx, &out, "eth_getBalance", a, "latest")
				case 1:
					data := append(common.FromHex("0x70a08231"), common.LeftPadBytes(a.Bytes(), 32)...)
					err = c.CallContext(ctx, &out, "eth_call", map[string]any{"to": wpls, "data": hexutil.Bytes(data)}, "latest")
				case 2:
					err = c.CallContext(ctx, &out, "eth_getStorageAt", hex, crypto.Keccak256Hash(a.Bytes()), "latest")
				}
				d := time.Since(t0)
				mu.Lock()
				lat = append(lat, d)
				mu.Unlock()
				if err != nil {
					fail.Add(1)
				} else {
					ok.Add(1)
				}
			}
		}()
	}
	for i := 0; i < *n; i++ {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	p := func(q float64) time.Duration { return lat[int(float64(len(lat)-1)*q)] }
	fmt.Printf("load: %d ok, %d failed in %s (p50 %s, p95 %s)\n", ok.Load(), fail.Load(), time.Since(start).Round(time.Millisecond), p(0.5).Round(time.Millisecond), p(0.95).Round(time.Millisecond))
	if fail.Load() > int64(*n)/20 {
		return fmt.Errorf("too many failures: %d", fail.Load())
	}
	return nil
}

func health(args []string) error {
	fs := flag.NewFlagSet("health", flag.ExitOnError)
	url := fs.String("url", "http://127.0.0.1:6060/health", "health endpoint")
	fs.Parse(args)
	resp, err := http.Get(*url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var v map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return err
	}
	out, _ := json.Marshal(v)
	fmt.Println(string(out))
	return nil
}
