#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
: "${OLP_CONSOLE_E2E_IMAGE:?set the immutable candidate image}"
[[ $OLP_CONSOLE_E2E_IMAGE == *@sha256:* ]] || { echo 'Candidate must be selected by digest.' >&2; exit 2; }
docker pull --platform "${OLP_IMAGE_PLATFORM:?set the native image platform}" "$OLP_CONSOLE_E2E_IMAGE"
./scripts/smoke-image-modes.sh "$OLP_CONSOLE_E2E_IMAGE"
./scripts/scan-image.sh "$OLP_CONSOLE_E2E_IMAGE" "${RUNNER_TEMP:-/tmp}/olp-candidate-scan"
./scripts/smoke-image-services.sh "$OLP_CONSOLE_E2E_IMAGE" "${OLP_IMAGE_PLATFORM#linux/}"
project="olp-candidate-$$-$RANDOM"
export OLP_POSTGRES_PORT=0 OLP_VALKEY_PORT=0
compose=(docker compose -p "$project" -f deploy/compose.dev.yaml)
scratch=$(mktemp -d)
cleanup() {
  status=$?
  trap - EXIT INT TERM
  if (( status != 0 )); then "${compose[@]}" logs --no-color >&2 || true; fi
  "${compose[@]}" down -v --remove-orphans >&2 || true
  rm -rf -- "$scratch"
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
"${compose[@]}" up -d --wait --wait-timeout 90
postgres=$("${compose[@]}" port postgres 5432)
valkey=$("${compose[@]}" port valkey 6379)
export OLP_TEST_DATABASE_ADMIN_URL="postgres://olp:olp-local@$postgres/postgres"
export OLP_TEST_DATABASE_URL_PREFIX="postgres://olp:olp-local@$postgres"
export OLP_VALKEY_URL="redis://:olp-local@$valkey/0"
export OLP_LOCAL_DIR="$scratch"
source scripts/secrets.sh "$scratch/secrets"
export OLP_TEST_RUN_TOKEN="$(openssl rand -hex 5)"
export OLP_CONSOLE_E2E_PACKAGED=true OLP_CONSOLE_E2E_CANDIDATE=true
./scripts/browser-integration.sh
