// Package config parses pulselite flags, with PULSELITE_* environment
// variables as fallbacks for every flag.
package config

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Mode selects which duties the execution service performs.
type Mode string

const (
	ModeFull      Mode = "full"      // validator duties + public RPC
	ModeValidator Mode = "validator" // execute every block, build payloads, no public RPC
	ModeRPC       Mode = "rpc"       // public RPC only, consensus client runs optimistically
)

// The validating modes cannot yet validate blocks within a slot over p2p, so
// they are only accepted with --experimental.validator.
const experimentalHint = "validator and full modes are disabled in this release (blocks are not validated fast enough over p2p yet); use --mode=rpc, or set --experimental.validator / PULSELITE_EXPERIMENTAL_VALIDATOR=true to run them anyway"

// Executes reports whether blocks are executed and validated in this mode.
func (m Mode) Executes() bool { return m == ModeFull || m == ModeValidator }

// ServesRPC reports whether the full public JSON-RPC surface is enabled.
func (m Mode) ServesRPC() bool { return m == ModeFull || m == ModeRPC }

type diskSizes struct{ cache, exec, total string }

// sizeDefaults apply when the size flags are unset or empty. RPC mode only
// caches what queries touch and runs the consensus client with a bounded
// database (see deploy/consensus/lighthouse-run.sh), so it gets far smaller
// limits than the validating modes, which persist every block's post-state.
var sizeDefaults = map[Mode]diskSizes{
	ModeRPC:       {cache: "1GiB", exec: "3GiB", total: "8GB"},
	ModeValidator: {cache: "4GiB", exec: "10GiB", total: "25GB"},
	ModeFull:      {cache: "4GiB", exec: "10GiB", total: "25GB"},
}

// Config is the complete runtime configuration.
type Config struct {
	Mode                  Mode
	ExperimentalValidator bool
	DataDir               string

	RPCAddr   string
	RPCPort   int
	RPCCors   []string
	RPCVHosts []string
	AuthAddr  string
	AuthPort  int
	JWTPath   string
	BeaconURL string
	Upstreams []string
	// devp2p (default state/history source)
	P2PPort      int
	P2PMaxPeers  int
	P2PNAT       string
	P2PBootnodes []string
	MetricsAdr   string

	CacheMaxBytes  uint64
	BlocksWindow   uint64 // reorg window: side chains older than head-window are dropped
	LogsWindow     uint64 // bodies + receipts retained for eth_getLogs / receipts
	HeadersWindow  uint64 // headers retained (eth1 follow distance lookups for the beacon node)
	StateWindow    uint64 // how far behind head upstreams are expected to still serve state
	DiskLimit      uint64 // hard limit for the execution data directory
	DiskTotalLimit uint64 // hard limit for execution + watched directories (consensus)
	DiskWatch      []string
	DiskInterval   time.Duration
	DiskFailHard   bool

	ExecDeadline  time.Duration
	ExecMaxRounds int
	BruteForceMax int // max nibble depth for sibling-key search
	PrefetchPar   int // per-tx inline prefetch concurrency (-1 = auto)
	RPCGasCap     uint64
	RPCEVMTimeout time.Duration
	MinerGasLimit uint64
	MinerExtra    string
	LogLevel      string
}

// Parse builds a Config from command line arguments and the environment.
func Parse(args []string) (*Config, error) {
	fs := flag.NewFlagSet("pulselite", flag.ContinueOnError)
	var (
		mode        = fs.String("mode", env("MODE", "rpc"), "operating mode: rpc | validator | full (validating modes need --experimental.validator)")
		expValidate = fs.Bool("experimental.validator", envBool("EXPERIMENTAL_VALIDATOR", false), "allow the experimental validating modes (validator, full)")
		datadir     = fs.String("datadir", env("DATADIR", "/blockchain/execution"), "data directory")
		rpcAddr     = fs.String("rpc.addr", env("RPC_ADDR", "0.0.0.0"), "public JSON-RPC listen address")
		rpcPort     = fs.Int("rpc.port", envInt("RPC_PORT", 8545), "public JSON-RPC port")
		rpcCors     = fs.String("rpc.corsdomain", env("RPC_CORSDOMAIN", "*"), "comma separated CORS origins")
		rpcVHosts   = fs.String("rpc.vhosts", env("RPC_VHOSTS", "*"), "comma separated virtual hosts")
		authAddr    = fs.String("authrpc.addr", env("AUTHRPC_ADDR", "0.0.0.0"), "engine API listen address")
		authPort    = fs.Int("authrpc.port", envInt("AUTHRPC_PORT", 8551), "engine API port")
		jwt         = fs.String("authrpc.jwtsecret", env("JWT", "/blockchain/jwt.hex"), "path to the shared JWT secret (hex)")
		beacon      = fs.String("beacon", env("BEACON", "http://consensus:3500"), "beacon node REST API endpoint")
		upstreams   = fs.String("upstream", env("UPSTREAM", ""), "OPT-IN: comma separated RPC endpoints to use instead of the devp2p network for state (default: none, state comes from PulseChain p2p peers)")
		p2pPort     = fs.Int("p2p.port", envInt("P2P_PORT", 30303), "devp2p listen port (TCP+UDP)")
		p2pPeers    = fs.Int("p2p.maxpeers", envInt("P2P_MAXPEERS", 50), "maximum devp2p peers")
		p2pNAT      = fs.String("p2p.nat", env("P2P_NAT", "any"), "NAT port mapping: any | none | upnp | pmp | extip:<IP>")
		p2pBoot     = fs.String("p2p.bootnodes", env("P2P_BOOTNODES", ""), "comma separated enode URLs (default: PulseChain bootnodes)")
		metricsAddr = fs.String("metrics.addr", env("METRICS_ADDR", "0.0.0.0:6060"), "metrics/health listen address (empty to disable)")
		cacheMax    = fs.String("cache.max-bytes", env("CACHE_MAX_BYTES", ""), "verified trie-node/code cache size cap (default: 1GiB in rpc mode, 4GiB in validator/full)")
		blocksWin   = fs.Uint64("blocks.window", envUint("BLOCKS_WINDOW", 128), "reorg window in blocks")
		logsWin     = fs.Uint64("logs.window", envUint("LOGS_WINDOW", 1024), "blocks of bodies+receipts kept for logs/receipts")
		headersWin  = fs.Uint64("headers.window", envUint("HEADERS_WINDOW", 8192), "headers retained")
		stateWin    = fs.Uint64("state.window", envUint("STATE_WINDOW", 120), "blocks behind head for which upstream state is assumed available")
		diskLimit   = fs.String("disk.limit", env("DISK_LIMIT", ""), "hard limit for the execution data dir (default: 3GiB in rpc mode, 10GiB in validator/full)")
		diskTotal   = fs.String("disk.total-limit", env("DISK_TOTAL_LIMIT", ""), "hard limit for execution + watched dirs (default: 8GB in rpc mode, 25GB in validator/full)")
		diskWatch   = fs.String("disk.watch", env("DISK_WATCH", ""), "comma separated extra dirs to measure (e.g. consensus data)")
		diskIvl     = fs.Duration("disk.interval", envDur("DISK_INTERVAL", time.Minute), "disk measurement interval")
		diskFail    = fs.Bool("disk.fail-hard", envBool("DISK_FAIL_HARD", true), "exit non-zero when a hard disk limit is exceeded")
		execDl      = fs.Duration("exec.deadline", envDur("EXEC_DEADLINE", 7*time.Second), "max time newPayload waits for execution before answering SYNCING")
		execRounds  = fs.Int("exec.max-rounds", envInt("EXEC_MAX_ROUNDS", 40), "max speculative fetch/execute rounds per block or call")
		bruteMax    = fs.Int("exec.sibling-depth", envInt("EXEC_SIBLING_DEPTH", 6), "max trie depth (nibbles) for locating collapse siblings")
		prefetchPar = fs.Int("exec.prefetch-parallel", envInt("EXEC_PREFETCH_PARALLEL", -1), "concurrent per-transaction inline prefetch runs (-1 = auto: 0 for p2p, 64 for RPC upstreams)")
		gasCap      = fs.Uint64("rpc.gascap", envUint("RPC_GASCAP", 50_000_000), "gas cap for eth_call/eth_estimateGas")
		evmTimeout  = fs.Duration("rpc.evmtimeout", envDur("RPC_EVMTIMEOUT", 20*time.Second), "timeout for eth_call/eth_estimateGas")
		minerGas    = fs.Uint64("miner.gaslimit", envUint("MINER_GASLIMIT", 0), "target gas limit for built payloads (0 = keep parent)")
		minerExtra  = fs.String("miner.extradata", env("MINER_EXTRADATA", "pulselite"), "extra data for built payloads")
		logLevel    = fs.String("log.level", env("LOG_LEVEL", "info"), "log level: trace|debug|info|warn|error")
	)
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	cfg := &Config{
		Mode:                  Mode(strings.ToLower(*mode)),
		ExperimentalValidator: *expValidate,
		DataDir:               *datadir,
		RPCAddr:       *rpcAddr,
		RPCPort:       *rpcPort,
		RPCCors:       splitList(*rpcCors),
		RPCVHosts:     splitList(*rpcVHosts),
		AuthAddr:      *authAddr,
		AuthPort:      *authPort,
		JWTPath:       *jwt,
		BeaconURL:     strings.TrimRight(*beacon, "/"),
		Upstreams:     splitList(*upstreams),
		P2PPort:       *p2pPort,
		P2PMaxPeers:   *p2pPeers,
		P2PNAT:        *p2pNAT,
		P2PBootnodes:  splitList(*p2pBoot),
		MetricsAdr:    *metricsAddr,
		BlocksWindow:  *blocksWin,
		LogsWindow:    *logsWin,
		HeadersWindow: *headersWin,
		StateWindow:   *stateWin,
		DiskWatch:     splitList(*diskWatch),
		DiskInterval:  *diskIvl,
		DiskFailHard:  *diskFail,
		ExecDeadline:  *execDl,
		ExecMaxRounds: *execRounds,
		BruteForceMax: *bruteMax,
		PrefetchPar:   *prefetchPar,
		RPCGasCap:     *gasCap,
		RPCEVMTimeout: *evmTimeout,
		MinerGasLimit: *minerGas,
		MinerExtra:    *minerExtra,
		LogLevel:      *logLevel,
	}
	sizes := sizeDefaults[cfg.Mode]
	if sizes == (diskSizes{}) {
		sizes = sizeDefaults[ModeRPC]
	}
	orDefault := func(v, def string) string {
		if v == "" {
			return def
		}
		return v
	}
	var err error
	if cfg.CacheMaxBytes, err = ParseSize(orDefault(*cacheMax, sizes.cache)); err != nil {
		return nil, fmt.Errorf("cache.max-bytes: %w", err)
	}
	if cfg.DiskLimit, err = ParseSize(orDefault(*diskLimit, sizes.exec)); err != nil {
		return nil, fmt.Errorf("disk.limit: %w", err)
	}
	if cfg.DiskTotalLimit, err = ParseSize(orDefault(*diskTotal, sizes.total)); err != nil {
		return nil, fmt.Errorf("disk.total-limit: %w", err)
	}
	return cfg, cfg.Validate()
}

// Validate checks for inconsistent settings.
func (c *Config) Validate() error {
	switch c.Mode {
	case ModeFull, ModeValidator, ModeRPC:
	default:
		return fmt.Errorf("invalid mode %q (want rpc, validator or full)", c.Mode)
	}
	if c.Mode.Executes() && !c.ExperimentalValidator {
		return errors.New(experimentalHint)
	}
	if c.CacheMaxBytes < 16<<20 {
		return errors.New("cache.max-bytes must be at least 16MiB")
	}
	if c.CacheMaxBytes >= c.DiskLimit {
		return fmt.Errorf("cache.max-bytes (%d) must be below disk.limit (%d)", c.CacheMaxBytes, c.DiskLimit)
	}
	if c.LogsWindow < c.BlocksWindow {
		c.LogsWindow = c.BlocksWindow
	}
	if c.HeadersWindow < c.LogsWindow {
		c.HeadersWindow = c.LogsWindow
	}
	if c.HeadersWindow < 300 {
		return errors.New("headers.window must be at least 300 (BLOCKHASH needs 256 ancestors)")
	}
	if c.ExecMaxRounds < 2 {
		return errors.New("exec.max-rounds must be at least 2")
	}
	return nil
}

// ParseSize parses sizes like "4GiB", "500MB", "1073741824".
func ParseSize(s string) (uint64, error) {
	s = strings.TrimSpace(s)
	units := []struct {
		suffix string
		mult   float64
	}{
		{"TiB", 1 << 40}, {"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10},
		{"TB", 1e12}, {"GB", 1e9}, {"MB", 1e6}, {"KB", 1e3}, {"B", 1},
	}
	for _, u := range units {
		if strings.HasSuffix(strings.ToUpper(s), strings.ToUpper(u.suffix)) {
			num := strings.TrimSpace(s[:len(s)-len(u.suffix)])
			f, err := strconv.ParseFloat(num, 64)
			if err != nil || f < 0 {
				return 0, fmt.Errorf("invalid size %q", s)
			}
			return uint64(f * u.mult), nil
		}
	}
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	return v, nil
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func envKey(name string) string { return "PULSELITE_" + name }

// env returns the variable's value, or def when it is unset or empty (so
// compose files can pass through optional settings).
func env(name, def string) string {
	if v := os.Getenv(envKey(name)); v != "" {
		return v
	}
	return def
}

func envInt(name string, def int) int {
	if v, ok := os.LookupEnv(envKey(name)); ok {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envUint(name string, def uint64) uint64 {
	if v, ok := os.LookupEnv(envKey(name)); ok {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			return n
		}
	}
	return def
}

func envBool(name string, def bool) bool {
	if v, ok := os.LookupEnv(envKey(name)); ok {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}

func envDur(name string, def time.Duration) time.Duration {
	if v, ok := os.LookupEnv(envKey(name)); ok {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}
