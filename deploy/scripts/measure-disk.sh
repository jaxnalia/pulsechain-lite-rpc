#!/usr/bin/env bash
# Measures consensus + execution disk usage inside the stack, appends to
# data/measurements.csv and exits 1 if the total exceeds the hard limit.
set -euo pipefail
cd "$(dirname "$0")/.."
. scripts/require-docker.sh
require_docker
docker compose exec -T execution litecheck disk -dirs /blockchain/consensus,/blockchain/execution \
  -limit "${1:-25GB}" -target "${2:-20GB}" -csv /blockchain/measurements.csv
