#!/usr/bin/env bash
# Resumable multi-hour soak: measures disk every INTERVAL minutes (hard limit
# => exit 1), records health/metrics and runs the RPC comparison periodically.
# Progress is kept in data/soak-state; re-running continues where it stopped
# (only time with the stack up counts). Pass --fresh to start over.
# Auto-resume after reboot, e.g.:  @reboot cd /path/deploy && ./scripts/soak.sh >>data/soak-run.log 2>&1
set -uo pipefail
cd "$(dirname "$0")/.."
mkdir -p data
HOURS="${1:-4}"; INTERVAL="${2:-10}"; LIMIT="${3:-25GB}"
STATE=data/soak-state
LOG=data/soak.log
[ "${4:-}" = "--fresh" ] && rm -f "$STATE"

acc=0; samples=0; interruptions=0; last=0
if [ -f "$STATE" ]; then
  # shellcheck disable=SC1090
  . "$STATE"
  [ "${complete:-0}" = 1 ] && { echo "soak already complete; pass --fresh to restart"; exit 0; }
  echo "=== resuming soak: ${acc}s of $((HOURS*3600))s done, $samples samples" | tee -a "$LOG"
fi
save() { printf 'acc=%s\nsamples=%s\ninterruptions=%s\nlast=%s\ncomplete=%s\n' "$acc" "$samples" "$interruptions" "$last" "${1:-0}" > "$STATE.tmp" && mv "$STATE.tmp" "$STATE"; }
maxgap=$((INTERVAL * 120 + 120))

until docker info >/dev/null 2>&1; do sleep 5; done
while :; do
  docker compose ps --status running --services | grep -q execution || docker compose up -d >/dev/null
  now=$(date +%s)
  if [ "$last" -gt 0 ]; then
    gap=$((now - last))
    if [ "$gap" -le "$maxgap" ]; then acc=$((acc + gap)); else interruptions=$((interruptions + 1)); echo "=== interruption #$interruptions: ${gap}s not counted" | tee -a "$LOG"; fi
  fi
  samples=$((samples + 1)); last=$now
  echo "=== sample $samples at $(date -u +%FT%TZ) (${acc}s operating)" | tee -a "$LOG"
  out=$(docker compose exec -T execution litecheck disk -dirs /blockchain/consensus,/blockchain/execution \
    -limit "$LIMIT" -csv /blockchain/measurements.csv 2>&1); echo "$out" | tee -a "$LOG"
  echo "$out" | grep -q "exceeds hard limit" && { save; echo "FAIL: disk hard limit exceeded" | tee -a "$LOG"; exit 1; }
  docker compose exec -T execution litecheck health 2>&1 | tee -a "$LOG"
  curl -s "http://127.0.0.1:${METRICS_PORT:-6060}/metrics" | grep -E '^pulselite_(newpayload_total|exec_seconds|cache_rotations|proof_verifications|sibling)' 2>&1 | tee -a "$LOG"
  curl -s "http://127.0.0.1:${BEACON_API_PORT:-3500}/eth/v1/node/syncing" 2>&1 | tee -a "$LOG"; echo | tee -a "$LOG"
  if [ $((samples % 3)) -eq 1 ]; then
    docker compose exec -T execution litecheck compare -accounts 15 2>&1 | tail -16 | tee -a "$LOG"
  fi
  save
  [ "$acc" -ge $((HOURS * 3600)) ] && break
  sleep $((INTERVAL * 60))
done
save 1
echo "PASS: soak completed (${acc}s operating, $samples samples, $interruptions interruptions) within $LIMIT" | tee -a "$LOG"
