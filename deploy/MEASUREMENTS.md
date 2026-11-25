# Measured results (PulseChain mainnet, Nov 19–21 2026)

Host: Windows 10, Docker Desktop (WSL2), 8 CPUs, 16 GB RAM, home internet.
Data on the Docker named volume `pulselite-data` (bind mounts on Docker
Desktop are too slow for databases; see README).

Two configurations were measured:

* **p2p (default, final)**: lighthouse-pulse v2.5.1 + pulselite with all
  state/history from PulseChain devp2p peers, no RPC provider.
* **opt-in RPC (earlier build)**: prysm-pulse v2.3.0 + pulselite with
  `--upstream` public RPCs and access-list hints. Kept for comparison.

## Disk usage

`litecheck disk` samples from `measurements.csv`; consensus = lighthouse (or
prysm) datadir, execution = pulselite datadir (cache + block window +
deposit store + p2p node db).

| Time (UTC) | Consensus | Execution | Total | Notes |
|---|---|---|---|---|
| 2026-11-19 20:09 | 0.072 GB | 0.071 GB | 0.142 GB | RPC build, fresh checkpoint sync (prysm) |
| 2026-11-19 21:10 | 0.120 GB | 0.445 GB | 0.565 GB | RPC build, +1 h |
| *(host powered off ~26 h)* | | | | soak resumed automatically |
| 2026-11-20 23:43 | 0.120 GB | 0.440 GB | 0.559 GB | p2p build |
| 2026-11-20 23:53 | 0.233 GB | 0.468 GB | 0.702 GB | lighthouse checkpoint sync started |
| 2026-11-21 00:34 | 0.413 GB | 0.526 GB | 0.939 GB | lighthouse backfilling history |
| 2026-11-21 01:15 | 0.572 GB | 0.620 GB | 1.191 GB | |
| 2026-11-21 01:56 | 0.603 GB | 0.769 GB | **1.373 GB** | soak complete |

Breakdown at 01:56: lighthouse 465 MB, leftover prysm data 114 MB, pulselite
cache 681 MB (cap 4 GiB), block window 59 MB, deposit store 14 MB.

**Soak result**: `PASS: soak completed: 4.09 h operating time, 26 samples,
1 interruption (1,541 min downtime, host powered off; resumed automatically),
within 25GB`.

**Hard limit check**: every sample is far below the 25 GB fail threshold
(the soak exits non-zero above it); `pulselite --disk.limit` (10 GiB) and
`--disk.total-limit` (25 GB) are enforced in-process as well.

**Steady-state projection** (not yet observed end-to-end): lighthouse is
backfilling 1,028,640 slots (17 weeks, the weak-subjectivity period) at
~9.6 slots/s (~30 h), using ≈ 8 KB/slot with payloads pruned → ≈ 8–9 GB.
pulselite is bounded by its cache cap (4 GiB logical, ≤ ~1.5x on disk with
pebble overhead, see bloat test) plus ~0.1 GB of windows. Projected total
**≈ 13–14 GB**, within the 20 GB target. prysm-pulse does not backfill and
stayed at ~0.12 GB.

## Restart / bloat test (`bloat-test.ps1`, 64 MiB cache cap to force eviction)

| Cycle | Cache (logical) | Execution datadir | Load |
|---|---|---|---|
| 1 | 46.3 MB | 138 MB | 3000 ok, 0 failed |
| 2 | 41.3 MB | 103 MB | 3000 ok, 0 failed |
| 3 | 42.2 MB | 109 MB | 3000 ok, 0 failed |
| 4 | 51.2 MB | 117 MB | 3000 ok, 0 failed |
| 5 | 34.1 MB | 85 MB | 3000 ok, 0 failed |
| 6 | 50.2 MB | 103 MB | 3000 ok, 0 failed |

PASS: cache stayed under its 64 MiB cap across 6 load+restart cycles, the
datadir did not grow, and the persisted cache was served after every restart.

## Functional comparison (`compare-rpc`)

Against `https://rpc.pulsechain.com` (erigon archive, used purely as a test
oracle), at the same block, accounts sampled from that block's transactions:

| Build | Block | Checks | Mismatches |
|---|---|---|---|
| RPC build | 28,099,604 | 196 | 0 |
| p2p build (no RPC provider) | 28,110,110 | 161 | 0 |

p2p build breakdown: `eth_getBalance` 20, `eth_getTransactionCount` 20,
`eth_getCode` 20, `eth_getStorageAt` 26 (incl. WPLS `balanceOf` mapping
slots), `eth_call` 46 (WPLS/HEX `balanceOf`, `totalSupply`, `name`,
`decimals`), `eth_estimateGas` 20, `eth_getLogs` 1, receipts 5, block hash 1,
chain id 1 — all equal. Average latency: 2–155 ms (cold proofs over p2p
~130–155 ms, cached reads single-digit ms).

## RPC mode (default; validation off), Nov 21 2026

Cold restart of the execution container with existing data, then 5 minutes
of wallet-style calls every ~3 s (`eth_call` HEX `totalSupply`, WPLS
`balanceOf`, `eth_getTransactionCount`, `eth_estimateGas`, `eth_getBalance`
of a random address):

| Metric | Result |
|---|---|
| Ready (`/ready` = 200) after restart | 6 s |
| Readiness samples after first ready | 78 / 78 ready (no flapping) |
| Calls | 390 ok, **0 errors** |
| Latency | p50 17 ms, p90 488 ms, p99 986 ms, max 1,003 ms |
| p2p state requests | 294 ok, 0 failed (account 94, storage 35, trie nodes 165) |
| Readiness state probe (WPLS balance at each new head) | ~150–165 ms |

Wallet method spot check (all answered): `eth_chainId`, `net_version`,
`eth_syncing`, `eth_blockNumber`, `eth_gasPrice`, `eth_maxPriorityFeePerGas`,
`eth_feeHistory`, `eth_getBalance`, `eth_getTransactionCount`, `eth_getCode`,
`eth_call`, `eth_estimateGas`, `eth_getBlockByNumber`,
`eth_getTransactionReceipt`, `eth_getLogs`.

Before tuning, freshly connected peers without a latency sample were tried
first and some never answer snap requests; with a 1 s hedge that made the
first calls after a restart take ~1.4 s, and one readiness probe failed.
Fixed by ranking unknown peers behind proven ones, hedging snap requests at
500 ms and retrying once after all peers fail (the newest block may not be
imported by peers yet).

### RPC-mode disk (fresh checkpoint per start, Nov 21 2026)

| Measurement | Result |
|---|---|
| lighthouse database before (resumed DB, backfill at 16%) | 895 MB (chain_db 869 MB, freezer 14 MB) + 51 MB of its own log files |
| lighthouse database after a fresh checkpoint start | **142 MB** |
| `docker compose up` → `/ready` = 200 (with purge + checkpoint sync) | **42 s** (checkpoint state download 2.6 s) |
| Finalized BeaconState (SSZ) | 18.0 MB |
| Backfill speed / growth | 736 slots/min, ≈ 4.8 KB per slot ≈ 210 MB/hour |
| Over-budget restart (test budget 150 MB, 30 s checks) | triggered at 168 MB; RPC not ready for 41 s, then ready |
| Docker images needed | lighthouse-pulse 398 MB, pulselite 198 MB; build-only: golang 1.27 GB, debian 116 MB |

Bounds with the defaults: consensus ≤ ~2.0 GB (`LH_DB_BUDGET_MB=2000`,
checked every 5 min), execution ≤ 3 GiB hard (cache cap 1 GiB logical,
~1.1–1.6 GB on disk, + ~0.1 GB windows); chain data ~0.25 GB at start and
~3.5–4 GB at most.

## Block validation (experimental modes)

| Build | Median | p90 | Notes |
|---|---|---|---|
| RPC build (hints + public RPC) | ~5 s | ~14 s | `is_optimistic: false` most of the time |
| p2p build, first 10 min (cold) | 8.2 s | 60 s (cap) | 19 blocks in 9 min; average 15.6 discovery rounds |
| p2p build, last 100 min of soak | 9.8 s | 37.2 s | 263 blocks validated; p25 5.1 s, p75 17.9 s; 47% within the 9 s deadline |

Correctness: every validated block's computed state root, receipts root, gas
used and bloom matched the payload (0 mismatches). p2p build over the soak:
102,044 proofs/trie nodes verified, 0 rejected; 159 collapse siblings
resolved by path, 0 unresolvable. RPC build: 20,816 proofs verified, 684
collapse siblings resolved, 0 failures.
`is_optimistic: false` was reached with both builds, but the p2p build does
not yet validate every block within the slot (see README *Limitations*).

## Notable findings during testing

* prysm-pulse v2.3.0 stopped importing blocks on 2026-11-20: every peer
  response failed with `saw digest=0x845ee500`; the expected Capella digest
  is `0x845ee52a` (computed from the genesis validators root). Its
  `readContextFromStream` uses a single `stream.Read` for the 4-byte digest.
  lighthouse-pulse v2.5.1 synced to head within a minute from the same host.
* Docker Desktop bind mounts made pebble compaction stall; named volumes fix it.
* The PulseChain deposit history scan finds ≈ 4 deposit logs per 1000 blocks
  (11,827 logs in the first 1.54M blocks scanned).
