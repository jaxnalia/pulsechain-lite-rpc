#!/usr/bin/env bash
# Compares pulselite answers against an independent reference node at the
# same block (balances, nonces, code, storage, eth_call, estimateGas, logs,
# receipts). Exits 1 on any mismatch.
set -euo pipefail
cd "$(dirname "$0")/.."
. scripts/require-docker.sh
require_docker
docker compose exec -T execution litecheck compare -local http://127.0.0.1:8545 \
  -ref "${1:-https://rpc.pulsechain.com}" -accounts "${2:-25}"
