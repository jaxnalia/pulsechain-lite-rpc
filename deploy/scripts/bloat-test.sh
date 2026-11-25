#!/usr/bin/env bash
# Restart/bloat integration test: N cycles of cache-filling query load
# followed by a restart of the execution service. Fails if the cache's
# logical size exceeds its cap or the execution datadir grows unboundedly.
set -euo pipefail
cd "$(dirname "$0")/.."
. scripts/require-docker.sh
require_docker
CYCLES="${1:-5}"
REQUESTS="${2:-3000}"

health() {
  for _ in $(seq 60); do
    if out=$(docker compose exec -T execution litecheck health 2>/dev/null); then echo "$out"; return; fi
    sleep 2
  done
  echo "execution not healthy" >&2; exit 1
}
field() { echo "$1" | sed -E "s/.*\"$2\":([0-9.e+]+).*/\1/"; }

cap=$(field "$(health)" cacheCap)
for c in $(seq "$CYCLES"); do
  docker compose exec -T execution litecheck load -n "$REQUESTS" -concurrency 8
  h=$(health)
  cache=$(field "$h" cacheBytes)
  exec_bytes=$(docker compose exec -T execution du -sb /blockchain/execution | cut -f1)
  echo "cycle $c: cache=$cache cap=$cap execution_datadir=$exec_bytes"
  awk -v a="$cache" -v b="$cap" 'BEGIN{exit !(a <= b*1.01)}' || { echo "FAIL: cache exceeds cap"; exit 1; }
  awk -v a="$exec_bytes" -v b="$cap" 'BEGIN{exit !(a <= b*1.6 + 536870912)}' || { echo "FAIL: datadir bloated"; exit 1; }
  docker compose restart execution >/dev/null
  sleep 5
  health >/dev/null
done
health >/dev/null
echo "PASS: cache bounded across $CYCLES restart cycles"
