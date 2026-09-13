#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
scratch=$(mktemp -d)
export OLP_GO_TEST_TLS_DIR="$scratch/tls"
mkdir -p "$OLP_GO_TEST_TLS_DIR"
chmod 755 "$scratch" "$OLP_GO_TEST_TLS_DIR"
export OLP_GO_POSTGRES_PORT=0 OLP_GO_VALKEY_PORT=0
project="olp-go-test-$$-$RANDOM"
compose=(docker compose -p "$project" -f deploy/compose.go.yaml -f deploy/compose.go-integration.yaml)
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
openssl req -x509 -newkey rsa:2048 -nodes -keyout "$OLP_GO_TEST_TLS_DIR/ca.key" -out "$OLP_GO_TEST_TLS_DIR/ca.crt" -days 1 -subj /CN=olp-go-test-ca >/dev/null 2>&1
openssl req -newkey rsa:2048 -nodes -keyout "$OLP_GO_TEST_TLS_DIR/server.key" -out "$scratch/server.csr" -subj /CN=localhost >/dev/null 2>&1
printf 'subjectAltName=DNS:localhost,DNS:postgres,DNS:valkey,IP:127.0.0.1\nextendedKeyUsage=serverAuth\n' > "$scratch/extensions"
openssl x509 -req -in "$scratch/server.csr" -CA "$OLP_GO_TEST_TLS_DIR/ca.crt" -CAkey "$OLP_GO_TEST_TLS_DIR/ca.key" -CAcreateserial -out "$OLP_GO_TEST_TLS_DIR/server.crt" -days 1 -extfile "$scratch/extensions" >/dev/null 2>&1
# Disposable test key only; the Valkey image runs as its own unprivileged UID.
chmod 644 "$OLP_GO_TEST_TLS_DIR/server.key"
"${compose[@]}" up -d --wait --wait-timeout 90
postgres=$("${compose[@]}" port postgres 5432)
valkey=$("${compose[@]}" port valkey 6379)
valkey_tls=$("${compose[@]}" port valkey 6380)
export OLP_TEST_DATABASE_URL="postgres://olp_go:olp-go-local@$postgres/olp_go?sslmode=disable"
export OLP_TEST_DATABASE_TLS_URL="postgres://olp_go:olp-go-local@$postgres/olp_go?sslmode=verify-full&sslrootcert=$OLP_GO_TEST_TLS_DIR/ca.crt"
export OLP_TEST_VALKEY_URL="redis://:olp-go-local@$valkey/0"
export OLP_TEST_VALKEY_TLS_URL="rediss://:olp-go-local@$valkey_tls/0"
export OLP_TEST_CA_FILE="$OLP_GO_TEST_TLS_DIR/ca.crt"
export OLP_TEST_BINARY="$PWD/.local/bin/olp"
make go-build
go test -race -tags=integration -count=1 -timeout=2m -v ./tests/integration
OLP_SDK_SMOKE_BACKEND=go ./tests/sdk-smoke/run.sh node tests/sdk-smoke/smoke.mjs --check-metadata
export OLP_DATABASE_URL="$OLP_TEST_DATABASE_URL" OLP_VALKEY_URL="$OLP_TEST_VALKEY_URL"
export OLP_CONSOLE_E2E_BACKEND=go OLP_CONSOLE_E2E_BIN="$OLP_TEST_BINARY"
pnpm --dir console exec playwright test --config playwright.go.config.ts
