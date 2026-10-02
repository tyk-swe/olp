#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
mode=${1:-all}
case "$mode" in all|shell|code) ;; *) echo 'usage: integration.sh [all|shell|code]' >&2; exit 2 ;; esac
scratch=$(mktemp -d)
export OLP_TEST_TLS_DIR="$scratch/tls"
mkdir -p "$OLP_TEST_TLS_DIR"
chmod 755 "$scratch" "$OLP_TEST_TLS_DIR"
export OLP_POSTGRES_PORT=0 OLP_VALKEY_PORT=0
project="olp-test-$$-$RANDOM"
compose=(docker compose -p "$project" -f deploy/compose.dev.yaml -f deploy/compose.integration.yaml)
cleanup() {
  status=$?
  trap - EXIT INT TERM
  if (( status != 0 )); then "${compose[@]}" logs --no-color >&2 || true; fi
  if [[ -n ${restore_valkey:-} ]]; then docker rm -f "$restore_valkey" >/dev/null 2>&1 || true; fi
  "${compose[@]}" down -v --remove-orphans >&2 || true
  rm -rf -- "$scratch"
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
openssl req -x509 -newkey rsa:2048 -nodes -keyout "$OLP_TEST_TLS_DIR/ca.key" -out "$OLP_TEST_TLS_DIR/ca.crt" -days 1 -subj /CN=olp-test-ca >/dev/null 2>&1
openssl req -newkey rsa:2048 -nodes -keyout "$OLP_TEST_TLS_DIR/server.key" -out "$scratch/server.csr" -subj /CN=localhost >/dev/null 2>&1
printf 'subjectAltName=DNS:localhost,DNS:postgres,DNS:valkey,IP:127.0.0.1\nextendedKeyUsage=serverAuth\n' > "$scratch/extensions"
openssl x509 -req -in "$scratch/server.csr" -CA "$OLP_TEST_TLS_DIR/ca.crt" -CAkey "$OLP_TEST_TLS_DIR/ca.key" -CAcreateserial -out "$OLP_TEST_TLS_DIR/server.crt" -days 1 -extfile "$scratch/extensions" >/dev/null 2>&1
# Disposable test key only; the Valkey image runs as its own unprivileged UID.
chmod 644 "$OLP_TEST_TLS_DIR/server.key"
# Registry pulls flake often enough to fail the suite before it starts; retry
# them, including the standalone restore Valkey image reused below.
for attempt in {1..3}; do
  if "${compose[@]}" pull --quiet --policy "${OLP_TEST_PULL_POLICY:-always}" &&
    { [[ ${OLP_TEST_PULL_POLICY:-always} == missing ]] && docker image inspect valkey/valkey:9-alpine >/dev/null 2>&1 || docker pull --quiet valkey/valkey:9-alpine >/dev/null; }; then break; fi
  if (( attempt == 3 )); then exit 1; fi
  sleep $((attempt * 10))
done
"${compose[@]}" up -d --wait --wait-timeout 90
postgres=$("${compose[@]}" port postgres 5432)
valkey=$("${compose[@]}" port valkey 6379)
valkey_tls=$("${compose[@]}" port valkey 6380)
export OLP_TEST_DATABASE_URL="postgres://olp:olp-local@$postgres/olp?sslmode=disable"
export OLP_TEST_DATABASE_ADMIN_URL="postgres://olp:olp-local@$postgres/postgres?sslmode=disable"
export OLP_TEST_DATABASE_TLS_URL="postgres://olp:olp-local@$postgres/olp?sslmode=verify-full&sslrootcert=$OLP_TEST_TLS_DIR/ca.crt"
export OLP_TEST_VALKEY_URL="redis://:olp-local@$valkey/0"
export OLP_TEST_VALKEY_TLS_URL="rediss://:olp-local@$valkey_tls/0"
export OLP_TEST_CA_FILE="$OLP_TEST_TLS_DIR/ca.crt"
export OLP_TEST_BINARY="$PWD/.local/bin/olp"
make build
source scripts/secrets.sh "$scratch/secrets"
OLP_DATABASE_URL="$OLP_TEST_DATABASE_URL" "$OLP_TEST_BINARY" migrate
export OLP_CODEX_BINARY
OLP_CODEX_BINARY=$(./scripts/code-mode-qualification.sh install)
if [[ "$mode" == code ]]; then
  go test -mod=readonly -race -tags=integration,codecli -count=1 -timeout=15m -v -run '^TestCode' ./tests/integration
  ./scripts/code-mode-qualification.sh cli
  exit
fi
# Recovery tests use a separately provisioned Valkey, never a logical database
# within the original service.
restore_valkey="${project}-restore-valkey"
docker run --detach --rm --name "$restore_valkey" -p 127.0.0.1::6379 valkey/valkey:9-alpine valkey-server --requirepass olp-local >/dev/null
OLP_TEST_RESTORE_VALKEY_URL="redis://:olp-local@$(docker port "$restore_valkey" 6379/tcp)/0"
export OLP_TEST_RESTORE_VALKEY_URL
go test -race -tags=integration,oidctest,pythonsdk,codecli -count=1 -timeout=30m -v ./tests/integration
./scripts/code-mode-qualification.sh cli
go test -race -tags=integration,oidctest -count=1 -timeout=30m -v -run '^TestIntegration' ./internal/database ./internal/gateway ./internal/providers ./internal/media ./internal/usage
# Test-only trusted registry additions run in their own process, so dynamic
# fixture profiles cannot change the normal suite's fixed catalogue inventory.
go test -race -tags=integration,extension -count=1 -timeout=5m -v -run '^TestRegisteredExtensionsPublic$' ./tests/integration
OLP_SDK_SMOKE_SURFACES=openai,anthropic,gemini ./tests/sdk-smoke/run.sh
if [[ "$mode" == shell ]]; then exit; fi
export OLP_DATABASE_URL="$OLP_TEST_DATABASE_URL" OLP_VALKEY_URL="$OLP_TEST_VALKEY_URL"
# Browser OIDC uses a separate, explicitly test-only binary. Release builds
# never allow loopback identity issuers.
make build-go GO_BUILD_OUTPUT=.local/bin/olp-identity-test GO_BUILD_TAGS=oidctest
for database in olp_packaged olp_vite; do
  "${compose[@]}" exec -T postgres createdb -U olp "$database"
done
export OLP_CONSOLE_E2E_BIN="$PWD/.local/bin/olp-identity-test"
pnpm --dir console exec playwright test --config playwright.config.ts

export OLP_TEST_DATABASE_URL_PREFIX="postgres://olp:olp-local@$postgres"
export OLP_LOCAL_DIR="$scratch"
OLP_TEST_RUN_TOKEN="$(openssl rand -hex 5)"
export OLP_TEST_RUN_TOKEN
OLP_CONSOLE_E2E_PACKAGED=true ./scripts/browser-integration.sh
