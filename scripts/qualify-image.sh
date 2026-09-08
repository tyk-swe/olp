#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
: "${OLP_CONSOLE_E2E_IMAGE:?set the immutable candidate image}"
[[ $OLP_CONSOLE_E2E_IMAGE == *@sha256:* ]] || { echo 'Candidate must be selected by digest.' >&2; exit 2; }
docker pull --platform "${OLP_IMAGE_PLATFORM:?set the native image platform}" "$OLP_CONSOLE_E2E_IMAGE"
./scripts/smoke-image-modes.sh "$OLP_CONSOLE_E2E_IMAGE"
./scripts/scan-image.sh "$OLP_CONSOLE_E2E_IMAGE" "${RUNNER_TEMP:-/tmp}/olp-candidate-scan"
docker compose -f deploy/compose.dev.yaml up -d --wait
export OLP_LOCAL_DIR="$PWD/.local/candidate"
source scripts/local-env.sh
export OLP_TEST_DATABASE_ADMIN_URL=postgres://olp:olp-local@127.0.0.1:54320/postgres
export OLP_TEST_DATABASE_URL_PREFIX=postgres://olp:olp-local@127.0.0.1:54320
export OLP_TEST_RUN_TOKEN="$(openssl rand -hex 5)"
./scripts/smoke-image-services.sh "$OLP_CONSOLE_E2E_IMAGE"
export OLP_CONSOLE_E2E_PACKAGED=true
export OLP_CONSOLE_E2E_CANDIDATE=true
./scripts/browser-integration.sh
