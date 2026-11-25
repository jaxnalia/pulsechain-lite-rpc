#!/usr/bin/env bash
# One-command startup of the PulseChain Lite RPC:
#   ./scripts/setup.sh [--lan] [--proxy] [lighthouse|prysm] [--no-wait]
# Experimental validator modes (blocks not yet validated within the slot):
#   ./scripts/setup.sh validator|full --experimental-validator [--validator]
set -euo pipefail
cd "$(dirname "$0")/.."

MODE=rpc
CL=lighthouse
PROXY=0
VALIDATOR=0
EXPERIMENTAL=0
LAN=0
WAIT=1
WAIT_MINUTES=20
for arg in "$@"; do
  case "$arg" in
    rpc|validator|full) MODE="$arg" ;;
    lighthouse|prysm) CL="$arg" ;;
    --lan) LAN=1 ;;
    --proxy) PROXY=1 ;;
    --validator) VALIDATOR=1 ;;
    --experimental-validator) EXPERIMENTAL=1 ;;
    --no-wait) WAIT=0 ;;
    *) echo "unknown argument: $arg" >&2; exit 2 ;;
  esac
done
NEEDS_EXP=0
{ [ "$MODE" != rpc ] || [ "$VALIDATOR" = 1 ]; } && NEEDS_EXP=1
if [ "$NEEDS_EXP" = 1 ] && [ "$EXPERIMENTAL" = 0 ]; then
  echo "Validator modes are disabled in this release (blocks are not validated fast enough over p2p yet)." >&2
  echo "Use the default rpc mode, or add --experimental-validator to run them anyway." >&2
  exit 2
fi
if [ "$VALIDATOR" = 1 ] && [ "$MODE" = rpc ]; then
  echo "--validator needs mode validator or full" >&2; exit 2
fi
. scripts/require-docker.sh
require_docker

PROFILES="$CL"
[ "$PROXY" = 1 ] && PROFILES="$PROFILES,proxy"
if [ "$VALIDATOR" = 1 ]; then
  if [ "$CL" = prysm ]; then PROFILES="$PROFILES,validator-prysm"; else PROFILES="$PROFILES,validator"; fi
fi

set_env() {
  if grep -q "^$1=" .env; then
    sed -i.bak "s|^$1=.*|$1=$2|" .env && rm -f .env.bak
  else
    printf '\n%s=%s\n' "$1" "$2" >> .env
  fi
}
set_env PULSELITE_MODE "$MODE"
if [ "$NEEDS_EXP" = 1 ]; then
  set_env PULSELITE_EXPERIMENTAL_VALIDATOR true
  set_env LIGHTHOUSE_EXTRA_FLAGS ""
  set_env LH_FRESH_START false
  set_env LH_DB_BUDGET_MB 0
else
  set_env PULSELITE_EXPERIMENTAL_VALIDATOR false
  set_env LIGHTHOUSE_EXTRA_FLAGS "--disable-deposit-contract-sync"
  set_env LH_FRESH_START true
  set_env LH_DB_BUDGET_MB 2000
fi
set_env COMPOSE_PROFILES "$PROFILES"
if [ "$LAN" = 1 ]; then set_env RPC_BIND 0.0.0.0; else set_env RPC_BIND 127.0.0.1; fi

# A host-path DATA_DIR gets its JWT here; a named volume gets it from pulselite.
DATA_DIR=$(grep '^DATA_DIR=' .env | cut -d= -f2- | tr -d '\r')
case "$DATA_DIR" in
  .*|/*)
    mkdir -p "$DATA_DIR"
    if [ ! -s "$DATA_DIR/jwt.hex" ]; then
      openssl rand -hex 32 | tr -d '\n' > "$DATA_DIR/jwt.hex"
      echo "Created shared JWT secret $DATA_DIR/jwt.hex"
    fi ;;
esac

echo "Starting PulseChain Lite: mode=${MODE} consensus=${CL} profiles=${PROFILES}"
docker compose up -d --build ||
  docker_fail "Starting the containers failed (see the messages above). Fix the problem and run setup again."

PORT=$(grep '^RPC_PORT=' .env | cut -d= -f2 | tr -d '\r')
URL="http://127.0.0.1:${PORT:-8545}"
echo
echo "Dashboard + RPC: $URL  (open it in a browser)"
[ "$LAN" = 1 ] && echo "From other devices: http://<this-computer's-LAN-IP>:${PORT:-8545}"
[ "$WAIT" = 0 ] && exit 0

echo "Waiting for the node to become ready (Ctrl+C to stop waiting; the node keeps running)..."
END=$(( $(date +%s) + WAIT_MINUTES * 60 ))
LAST=""
while [ "$(date +%s)" -lt "$END" ]; do
  BODY=$(curl -s --max-time 5 "$URL/ready" || true)
  if echo "$BODY" | grep -q '"ready":true'; then
    echo
    echo "READY: serving verified state and accepting transactions."
    echo "Wallet settings: RPC URL $URL | Chain ID 369 | Symbol PLS | Explorer https://scan.pulsechain.com"
    exit 0
  fi
  MSG=$(echo "$BODY" | sed -n 's/.*"summary":"\([^"]*\)".*/\1/p')
  [ -z "$MSG" ] && MSG="starting containers..."
  if [ "$MSG" != "$LAST" ]; then echo "  [$(date +%H:%M:%S)] $MSG"; LAST="$MSG"; fi
  sleep 5
done
echo "Not ready after $WAIT_MINUTES minutes; it may still be syncing. Check $URL or 'docker compose logs -f'."
