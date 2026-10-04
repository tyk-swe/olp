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
bench_container=
cleanup() {
  status=$?
  trap - EXIT INT TERM
  if (( status != 0 )); then "${compose[@]}" logs --no-color >&2 || true; fi
  if [[ -n $bench_container ]]; then docker rm -f "$bench_container" >/dev/null 2>&1 || true; fi
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
OLP_TEST_RUN_TOKEN="$(openssl rand -hex 5)"
export OLP_TEST_RUN_TOKEN
export OLP_CONSOLE_E2E_PACKAGED=true OLP_CONSOLE_E2E_CANDIDATE=true
./scripts/browser-integration.sh
# Full-scale gateway scenarios (docs/roadmap/m01-measured-advantage.md) against
# the binary this candidate ships, not one built from source. They are a
# release-qualification suite like the ones above, but opt-in: the numbers mean
# something only on reference hardware, with the gateway, the mock upstream and
# the load generator pinned to CPUs of their own (OLP_BENCH_*_CPUS), and a run
# of the six takes far longer than the 45 minutes the hosted runners allow.
# With OLP_BENCH_ENFORCE=1 a missed target fails qualification.
if [[ ${OLP_QUALIFY_BENCH:-0} == 1 ]]; then
  bench_container=$(docker create --platform "$OLP_IMAGE_PLATFORM" "$OLP_CONSOLE_E2E_IMAGE" all --help)
  docker cp "$bench_container:/usr/local/bin/olp" "$scratch/olp"
  docker rm "$bench_container" >/dev/null
  bench_container=
  # The image's binary links the image's glibc, which an older host may lack.
  "$scratch/olp" version >/dev/null || {
    echo 'The candidate binary does not run on this host: benchmark on a host with the image'"'"'s glibc or newer.' >&2
    exit 1
  }
  OLP_TEST_BINARY="$scratch/olp" ./scripts/bench.sh
fi
