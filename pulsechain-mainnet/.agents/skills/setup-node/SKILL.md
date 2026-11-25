---
name: setup-node
description: >-
  First-time setup and bootstrap of a local PulseChain Docker Compose full
  node (prereqs, JWT/data folder, compose up). Use when the user asks to set
  up, install, bootstrap, or run first-time setup, prepare the data/JWT
  folder, or get a local RPC running for the first time.
---

# Set up a PulseChain Docker Compose node

Run from the project directory that contains `docker-compose.yml`.
Covers the **easiest** Docker Compose path only (not advanced/archive or validator staking). Prefer the shell matching the host OS.

Human-oriented steps and advanced options live in `docs/running-a-node.md`. This skill adds agent gates: enforce prereqs, avoid destructive re-setup, verify start, and do not claim the RPC is wallet-ready.

Do **not** start the node until prerequisites pass (or the user explicitly overrides a disk-space warning). After start, hand off to `sync-status` — first sync can take days.

## Prerequisites

Enforce the Compose requirements from `docs/running-a-node.md`:

- **4+ CPU cores**, **16 GB+ RAM**, **2 TB+ free SSD** (chain data under `./data`)
- Docker with Compose ([install](https://docs.docker.com/get-docker/))
- Mac / Linux: `openssl` if `data/jwt.hex` must be created
- Windows: `scripts/setup.bat` (launches `setup.ps1`; no openssl)

## 1. Preflight checks

### A) Confirm project layout

Verify in the project root:

- `docker-compose.yml`
- `.env` (pins `GO_PULSE_VERSION` and `PRYSM_PULSE_VERSION`)
- `scripts/setup.sh` (Mac / Linux) and/or `scripts/setup.bat` (Windows)

If `.env` is missing, stop; do not invent tags (use `upgrade-clients` / GitLab releases).

### B) Docker available

```shell
docker version
docker compose version
```

If either fails, stop and tell the user to install Docker, then re-run. Do not attempt a full OS-level Docker install unless the user explicitly asks.

### C) Disk space

Mac / Linux:

```shell
df -h .
```

Windows (PowerShell):

```powershell
Get-PSDrive -Name (Get-Location).Drive.Name
```

- **≥ 2 TB free** → proceed
- **< 2 TB free** → warn; **do not** start unless the user explicitly confirms

Warn if CPU/RAM are clearly below 4 cores / 16 GB when easy to check; disk is the hard gate.

### D) Already running?

```shell
docker compose ps
```

If `execution` / `consensus` are already up, report that and ask whether to leave them alone, inspect with `sync-status`, or restart — do not treat setup as a destructive reset.

## 2. Prepare data folder and JWT

Mac / Linux:

```shell
./scripts/setup.sh
```

Windows (Command Prompt, PowerShell, or double-click) — use the `.bat` entrypoint (it calls `setup.ps1`):

```bat
.\scripts\setup.bat
```

Expect `data/` and a non-empty `data/jwt.hex`. If the script fails (e.g. missing `openssl`), fix the dependency and re-run. Do not hand-write a weak JWT.

## 3. Start the node

Only after prerequisites pass (or explicit disk override):

```shell
docker compose up -d
```

First image pull may take a while.

## 4. Verify containers started

```shell
docker compose ps
```

Both `execution` and `consensus` should be running (or starting). If either exits immediately:

```shell
docker compose logs --since 5m
```

Common failures: empty/missing JWT, port binds (`8545`/`8546`/`30303`/`13000`/`12000`), image pull errors, disk full. Do not claim success if services are not up.

## 5. Respond to the user

Short status covering:

1. **Prerequisites:** Docker OK; free disk (and any CPU/RAM warnings)
2. **Setup:** JWT/data prepared or already present
3. **Node:** both services up, or what failed
4. **RPC:** `http://127.0.0.1:8545` will exist but is **not** ready for MetaMask/apps until sync finishes (see wallet settings in `docs/running-a-node.md`)
5. **Next:** leave Docker running; use `sync-status` to monitor (first sync can take days)

Useful follow-ups: `docker compose logs -f`; `docker compose down` to stop.
