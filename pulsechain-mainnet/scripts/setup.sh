#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DATA_DIR="${ROOT_DIR}/data"
JWT_FILE="${DATA_DIR}/jwt.hex"

mkdir -p "${DATA_DIR}"

if [[ -s "${JWT_FILE}" ]]; then
  echo "JWT already exists at ${JWT_FILE}"
else
  if ! command -v openssl >/dev/null 2>&1; then
    echo "openssl is required to generate the JWT secret." >&2
    echo "Install openssl, then run this script again." >&2
    exit 1
  fi
  # Strip the trailing newline openssl adds
  printf '%s' "$(openssl rand -hex 32)" > "${JWT_FILE}"
  echo "Created ${JWT_FILE}"
fi

echo "Setup complete. Next: docker compose up -d"
