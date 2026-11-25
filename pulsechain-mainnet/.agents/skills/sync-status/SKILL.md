---
name: sync-status
description: >-
  Check whether the local PulseChain Docker Compose node is fully synced,
  healthy, and how far sync has progressed. Use when the user asks about
  sync status, eth_syncing, whether the RPC is ready, node health, or
  recent compose/container logs/errors.
---

# PulseChain node sync status

Run these checks from the project directory that contains `docker-compose.yml`.
Prefer the shell matching the host OS.

## 1. Query `eth_syncing`

Mac / Linux:

```bash
curl -s -X POST http://127.0.0.1:8545 -H 'content-type: application/json' --data '{"jsonrpc":"2.0","method":"eth_syncing","params":[],"id":1}'
```

Windows (PowerShell):

```powershell
Invoke-RestMethod -Method Post -Uri http://127.0.0.1:8545 -ContentType 'application/json' -Body '{"jsonrpc":"2.0","method":"eth_syncing","params":[],"id":1}' | Format-List
```

Interpretation:

- `result` is `false` / `False` → **fully synced**
- `result` is an object → **still syncing**; note `currentBlock` and `highestBlock` (hex). Convert to decimal for reporting.

If the RPC call fails (connection refused, timeout), say the node is not reachable on `http://127.0.0.1:8545` and still inspect logs below.

## 2. Inspect recent Compose logs

Fetch roughly the last few minutes of logs for both services:

```shell
docker compose logs --since 5m
```

Also useful if containers look down:

```shell
docker compose ps
```

Look for:

- Repeated `error` / `crit` / `fatal` / panic / segfault lines
- Execution↔consensus JWT / auth RPC failures
- Disk full / database corruption messages
- Normal progress: peer counts, imported/synced blocks, checkpoint sync activity

If logs are empty, widen the window (e.g. `--since 30m`) or note that containers may not be running.

## 3. Estimate sync percentage (when possible)

When `eth_syncing` returns an object with both `currentBlock` and `highestBlock`:

`percentage = (currentBlock / highestBlock) * 100`

(Use decimal values of the hex fields.) Round to a sensible precision (e.g. one decimal place).

If `highestBlock` is missing or zero (common in practice), try to derive a network head from recent execution/consensus log lines (imported block / head slot / similar) and compute:

`percentage = (currentBlock from eth_syncing / network head from logs) * 100`

Label this as an **estimate** when the head came from logs rather than `highestBlock`. Only report a percentage when both numbers look trustworthy; otherwise say progress could not be estimated and quote the raw `eth_syncing` fields / relevant log lines.

When fully synced (`result` is false), report **100%** / fully synced.

**Caveat:** block-height percentage is **not** a good proxy for time remaining. Later blocks (and post-merge / PulseChain-era state) are much heavier than early-chain blocks, so the last portion of sync often takes a disproportionate share of wall-clock time. Say so when reporting progress.

## 4. Respond to the user

Give a short, clear status covering all of:

1. **Sync:** fully synced **or** not fully synced
2. **Health:** errors found in the last ~5 minutes of logs, **or** things appear to be running well (mention any notable warnings)
3. **Progress:** percentage when calculable; otherwise state that it could not be determined

Do not claim the node is ready for wallets/apps unless `eth_syncing` is false and logs do not show active failure modes.
