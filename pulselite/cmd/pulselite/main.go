// Command pulselite is a proof-based PulseChain execution layer: it answers
// the Engine API for an unmodified consensus client and serves current-state
// JSON-RPC from Merkle proofs verified against consensus-provided state roots,
// keeping only a bounded cache instead of the full state trie.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/eth/protocols/eth"
	"github.com/ethereum/go-ethereum/ethdb/pebble"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/node"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/pulsechainlite/pulselite/internal/chain"
	"github.com/pulsechainlite/pulselite/internal/config"
	"github.com/pulsechainlite/pulselite/internal/deposits"
	"github.com/pulsechainlite/pulselite/internal/disk"
	"github.com/pulsechainlite/pulselite/internal/engineapi"
	"github.com/pulsechainlite/pulselite/internal/exec"
	"github.com/pulsechainlite/pulselite/internal/follow"
	"github.com/pulsechainlite/pulselite/internal/metrics"
	"github.com/pulsechainlite/pulselite/internal/p2pnet"
	"github.com/pulsechainlite/pulselite/internal/rpcapi"
	"github.com/pulsechainlite/pulselite/internal/status"
	"github.com/pulsechainlite/pulselite/internal/store"
	"github.com/pulsechainlite/pulselite/internal/upstream"
	"github.com/pulsechainlite/pulselite/internal/verified"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

func setupLogging(level string) {
	lvl := slog.LevelInfo
	switch strings.ToLower(level) {
	case "trace":
		lvl = log.LevelTrace
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	}
	log.SetDefault(log.NewLogger(log.NewTerminalHandlerWithLevel(os.Stderr, lvl, false)))
}

func readJWT(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		secret := make([]byte, 32)
		rand.Read(secret)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(path, []byte(hex.EncodeToString(secret)), 0o600); err != nil {
			return nil, err
		}
		log.Warn("Generated new JWT secret", "path", path)
		return secret, nil
	}
	if err != nil {
		return nil, err
	}
	s := strings.TrimPrefix(strings.TrimSpace(string(data)), "0x")
	secret, err := hex.DecodeString(s)
	if err != nil || len(secret) != 32 {
		return nil, fmt.Errorf("invalid JWT secret in %s (want 32 hex-encoded bytes)", path)
	}
	return secret, nil
}

func serve(name, addr string, h http.Handler) *http.Server {
	srv := &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 10 * time.Second}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Crit("Listen failed", "server", name, "addr", addr, "err", err)
	}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("HTTP server stopped", "server", name, "err", err)
		}
	}()
	log.Info("HTTP server started", "server", name, "addr", ln.Addr())
	return srv
}

// serveHeaders answers a remote GetBlockHeaders request from the window.
func serveHeaders(ch *chain.Chain, req *eth.GetBlockHeadersRequest) []*types.Header {
	var (
		out   []*types.Header
		start *types.Header
	)
	if req.Origin.Hash != (common.Hash{}) {
		start = ch.Header(req.Origin.Hash)
	} else {
		start = ch.HeaderByNumber(req.Origin.Number)
	}
	if start == nil {
		return nil
	}
	n := start.Number.Uint64()
	for i := uint64(0); i < min(req.Amount, 1024); i++ {
		h := ch.HeaderByNumber(n)
		if h == nil {
			break
		}
		out = append(out, h)
		step := req.Skip + 1
		if req.Reverse {
			if n < step {
				break
			}
			n -= step
		} else {
			n += step
		}
	}
	return out
}

func run() error {
	cfg, err := config.Parse(os.Args[1:])
	if err != nil {
		return err
	}
	setupLogging(cfg.LogLevel)
	if !cfg.Mode.Executes() {
		log.Info("RPC mode: block validation is off; the consensus client follows the chain optimistically")
	} else {
		log.Warn("Experimental validating mode enabled", "mode", cfg.Mode)
	}
	chainCfg := params.PulseChainConfig
	log.Info("Starting pulselite", "version", engineapi.Version, "mode", cfg.Mode, "chainId", chainCfg.ChainID,
		"cacheMax", cfg.CacheMaxBytes, "diskLimit", cfg.DiskLimit, "upstreams", upstream.RedactAll(cfg.Upstreams))

	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return err
	}
	kv, err := store.OpenGenStore(filepath.Join(cfg.DataDir, "cache"), cfg.CacheMaxBytes, 64)
	if err != nil {
		return fmt.Errorf("open cache: %w", err)
	}
	defer kv.Close()
	ch, err := chain.Open(filepath.Join(cfg.DataDir, "chain"), chainCfg, chain.Windows{
		Headers: cfg.HeadersWindow, Logs: cfg.LogsWindow, Reorg: cfg.BlocksWindow,
	})
	if err != nil {
		return fmt.Errorf("open block window: %w", err)
	}
	defer ch.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Data sources. Default: the PulseChain devp2p network (snap + eth from
	// ordinary go-pulse peers). RPC upstreams are used only if explicitly
	// configured with --upstream.
	var (
		backend verified.Backend
		history interface {
			follow.History
			deposits.Source
		}
		sendTx  func(ctx context.Context, tx *types.Transaction) error
		p2pNode *p2pnet.Node
	)
	if len(cfg.Upstreams) > 0 {
		pool, err := upstream.NewPool(cfg.Upstreams)
		if err != nil {
			return err
		}
		go pool.Probe(ctx)
		log.Warn("Using opt-in RPC upstreams instead of the p2p network", "upstreams", upstream.RedactAll(cfg.Upstreams))
		backend = &verified.RPCBackend{Pool: pool}
		history = &follow.RPCHistory{Pool: pool}
		sendTx = func(ctx context.Context, tx *types.Transaction) error {
			raw, _ := tx.MarshalBinary()
			var h common.Hash
			return pool.Broadcast(ctx, &h, "eth_sendRawTransaction", hexutil.Bytes(raw))
		}
	} else {
		bootnodes := cfg.P2PBootnodes
		if len(bootnodes) == 0 {
			bootnodes = params.PulseChainBootnodes
		}
		genesis := core.DefaultPulseChainGenesisBlock().ToBlock()
		p2pNode, err = p2pnet.New(p2pnet.Config{
			DataDir:    filepath.Join(cfg.DataDir, "p2p"),
			ListenPort: cfg.P2PPort,
			MaxPeers:   cfg.P2PMaxPeers,
			Bootnodes:  bootnodes,
			NAT:        cfg.P2PNAT,
			NetworkID:  chainCfg.ChainID.Uint64(),
			Chain:      chainCfg,
			Genesis:    genesis,
			Name:       "pulselite/v" + engineapi.Version,
			Head:       ch.Latest,
			ServeHeaders: func(req *eth.GetBlockHeadersRequest) []*types.Header {
				return serveHeaders(ch, req)
			},
		})
		if err != nil {
			return err
		}
		if err := p2pNode.Start(); err != nil {
			return fmt.Errorf("start p2p: %w", err)
		}
		defer p2pNode.Stop()
		backend = &p2pnet.Backend{Node: p2pNode, Parallel: 64, RootOf: func(at verified.BlockRef) (common.Hash, bool) {
			if h := ch.Header(at.Hash); h != nil {
				return h.Root, true
			}
			return common.Hash{}, false
		}}
		history = p2pNode
		sendTx = func(ctx context.Context, tx *types.Transaction) error {
			if n := p2pNode.BroadcastTx(tx); n == 0 {
				return errors.New("no p2p peers to broadcast to")
			}
			return nil
		}
	}

	src := verified.NewSource(kv, backend, ch)
	src.SiblingDepth = cfg.BruteForceMax
	ex := exec.New(chainCfg, src, ch, cfg.ExecMaxRounds)
	switch {
	case cfg.PrefetchPar >= 0:
		ex.PrefetchParallel = cfg.PrefetchPar
	case p2pNode != nil:
		ex.PrefetchParallel = 0 // p2p: bulk node batches in discovery rounds beat per-key requests
	}

	follower := &follow.Follower{
		Beacon:        follow.NewBeaconClient(cfg.BeaconURL),
		Chain:         ch,
		History:       history,
		HeadersWindow: cfg.HeadersWindow,
		LogsWindow:    cfg.LogsWindow,
		Interval:      10 * time.Second,
	}
	// The deposit-contract history is only needed by a consensus client that
	// serves validators; in rpc mode it runs with --disable-deposit-contract-sync.
	var depScanner *deposits.Scanner
	if cfg.Mode.Executes() {
		depDB, err := pebble.New(filepath.Join(cfg.DataDir, "deposits"), 16, 16, "", false)
		if err != nil {
			return fmt.Errorf("open deposit store: %w", err)
		}
		defer depDB.Close()
		depScanner = deposits.New(history, ch, depDB)
	}
	engAPI := engineapi.New(cfg, ex, ch, backend, engineapi.Hooks{
		OnNewBlock: func(b *types.Block) {
			if !cfg.Mode.Executes() {
				follower.FetchReceiptsAsync(b.Hash())
			}
		},
		FetchBody: follower.FetchBody,
	})
	follower.OnNetworkHead = engAPI.SetNetworkHeadEstimate
	if depScanner != nil {
		depScanner.Busy = engAPI.Busy
	}

	fetchReceipts := func(ctx context.Context, hash common.Hash) (types.Receipts, error) {
		return follower.FetchReceipts(ctx, hash)
	}
	mkBackend := func(engine bool) *rpcapi.Backend {
		b := &rpcapi.Backend{
			Chain: ch, Exec: ex, FetchReceipts: fetchReceipts, FetchBody: follower.FetchBody,
			GasCap: cfg.RPCGasCap, StateWindow: cfg.StateWindow, SendTx: sendTx, Engine: engine,
			NetworkHead: engAPI.NetworkHead,
		}
		if engine {
			b.History = history
			b.Deposits = depScanner
		}
		return b
	}
	timeoutWrap := func(h http.Handler) http.Handler {
		return http.TimeoutHandler(h, cfg.RPCEVMTimeout+5*time.Second, `{"jsonrpc":"2.0","error":{"code":-32002,"message":"request timed out"}}`)
	}

	// Engine endpoint (JWT): engine + eth/net/web3 for the consensus client.
	jwt, err := readJWT(cfg.JWTPath)
	if err != nil {
		return err
	}
	esrv := rpc.NewServer()
	if err := esrv.RegisterName("engine", engAPI); err != nil {
		return err
	}
	esrv.RegisterName("eth", rpcapi.NewEthAPI(mkBackend(true)))
	esrv.RegisterName("net", rpcapi.NewNetAPI(chainCfg.ChainID.Uint64()))
	esrv.RegisterName("web3", rpcapi.Web3API{})
	engineHTTP := serve("engine", fmt.Sprintf("%s:%d", cfg.AuthAddr, cfg.AuthPort),
		node.NewHTTPHandlerStack(esrv, nil, []string{"*"}, jwt))

	// Public endpoint.
	psrv := rpc.NewServer()
	psrv.SetBatchLimits(100, 25*1024*1024)
	full := rpcapi.NewEthAPI(mkBackend(false))
	if cfg.Mode.ServesRPC() {
		psrv.RegisterName("eth", full)
	} else {
		psrv.RegisterName("eth", rpcapi.NewMinimalEthAPI(full))
	}
	psrv.RegisterName("net", rpcapi.NewNetAPI(chainCfg.ChainID.Uint64()))
	psrv.RegisterName("web3", rpcapi.Web3API{})

	st := &status.Service{
		Version: engineapi.Version, Mode: string(cfg.Mode), ChainID: chainCfg.ChainID.Uint64(),
		Chain: ch, Beacon: follower.Beacon, Exec: ex, Cache: kv, NetworkHead: engAPI.NetworkHead,
		DiskTotalLimit: cfg.DiskTotalLimit,
		GasPrice: func(ctx context.Context) (*big.Int, error) {
			p, err := full.GasPrice(ctx)
			return (*big.Int)(p), err
		},
	}
	if p2pNode != nil {
		st.Peers = p2pNode.PeerCount
	}
	publicHTTP := serve("rpc", fmt.Sprintf("%s:%d", cfg.RPCAddr, cfg.RPCPort),
		st.Wrap(timeoutWrap(node.NewHTTPHandlerStack(psrv, cfg.RPCCors, cfg.RPCVHosts, nil))))

	// Metrics + health.
	var metricsHTTP *http.Server
	if cfg.MetricsAdr != "" {
		mux := http.NewServeMux()
		mux.Handle("/metrics", metrics.Handler())
		mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
			out := map[string]any{
				"mode":        cfg.Mode,
				"cacheBytes":  kv.LogicalBytes(),
				"cacheCap":    kv.Cap(),
				"diskBytes":   metrics.DiskExecution.Value(),
				"watchedDisk": metrics.DiskWatched.Value(),
			}
			if h := ch.Head(); h != nil {
				out["head"] = h.Number.Uint64()
				out["headHash"] = h.Hash()
				out["headValidated"] = ch.IsValid(h.Hash())
			}
			out["validated"] = metrics.ValidatedNumber.Value()
			hdrs, canon, txs := ch.Stats()
			out["window"] = map[string]int{"headers": hdrs, "canonical": canon, "txs": txs}
			if depScanner != nil {
				low, top, done, logs := depScanner.Status()
				out["deposits"] = map[string]any{"low": low, "top": top, "done": done, "logs": logs, "progress": metrics.DepositScan.Value()}
			}
			if p2pNode != nil {
				e, s := p2pNode.PeerCount()
				out["p2p"] = map[string]int{"eth": e, "snap": s}
			} else {
				out["source"] = "rpc-upstreams"
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(out)
		})
		mux.HandleFunc("/ready", st.ServeReady)
		mux.HandleFunc("/api/status", st.ServeStatus)
		metricsHTTP = serve("metrics", cfg.MetricsAdr, mux)
	}

	go follower.Run(ctx)
	go st.Run(ctx)
	if depScanner != nil {
		go depScanner.Run(ctx)
	}

	hardLimit := make(chan string, 1)
	mon := &disk.Monitor{
		ExecDir:     cfg.DataDir,
		Watch:       cfg.DiskWatch,
		Interval:    cfg.DiskInterval,
		ExecLimit:   cfg.DiskLimit,
		TotalLimit:  cfg.DiskTotalLimit,
		FailHard:    cfg.DiskFailHard,
		OnSoftLimit: kv.Rotate,
		OnHardLimit: func(reason string) {
			select {
			case hardLimit <- reason:
			default:
			}
		},
	}
	go mon.Run(ctx)

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	var exitErr error
	select {
	case s := <-sigs:
		log.Info("Shutting down", "signal", s)
	case reason := <-hardLimit:
		exitErr = fmt.Errorf("disk hard limit exceeded: %s", reason)
	}
	cancel()
	sctx, scancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer scancel()
	engineHTTP.Shutdown(sctx)
	publicHTTP.Shutdown(sctx)
	if metricsHTTP != nil {
		metricsHTTP.Shutdown(sctx)
	}
	esrv.Stop()
	psrv.Stop()
	if exitErr != nil {
		log.Error("Exiting", "err", exitErr)
		ch.Close()
		kv.Close()
		os.Exit(2)
	}
	return nil
}
