#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
scratch=$(mktemp -d)
export OLP_GO_TEST_TLS_DIR="$scratch/tls"
mkdir -p "$OLP_GO_TEST_TLS_DIR"
chmod 755 "$scratch" "$OLP_GO_TEST_TLS_DIR"
export OLP_GO_POSTGRES_PORT=0 OLP_GO_VALKEY_PORT=0
project="olp-go-test-$$-$RANDOM"
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
export OLP_TEST_DATABASE_ADMIN_URL="postgres://olp_go:olp-go-local@$postgres/postgres?sslmode=disable"
export OLP_TEST_DATABASE_TLS_URL="postgres://olp_go:olp-go-local@$postgres/olp_go?sslmode=verify-full&sslrootcert=$OLP_GO_TEST_TLS_DIR/ca.crt"
export OLP_TEST_VALKEY_URL="redis://:olp-go-local@$valkey/0"
export OLP_TEST_VALKEY_TLS_URL="rediss://:olp-go-local@$valkey_tls/0"
export OLP_TEST_CA_FILE="$OLP_GO_TEST_TLS_DIR/ca.crt"
export OLP_TEST_BINARY="$PWD/.local/bin/olp"
make build
source scripts/secrets.sh "$scratch/secrets"
OLP_DATABASE_URL="$OLP_TEST_DATABASE_URL" "$OLP_TEST_BINARY" migrate
# Recovery tests use a separately provisioned Valkey, never a logical database
# within the original service.
restore_valkey="${project}-restore-valkey"
docker run --detach --rm --name "$restore_valkey" -p 127.0.0.1::6379 valkey/valkey:9-alpine valkey-server --requirepass olp-go-local >/dev/null
export OLP_TEST_RESTORE_VALKEY_URL="redis://:olp-go-local@$(docker port "$restore_valkey" 6379/tcp)/0"
go test -race -tags=integration,oidctest -count=1 -timeout=30m -v ./tests/integration ./internal/gateway ./internal/providers ./internal/media
# Test-only trusted registry additions run in their own process, so dynamic
# fixture profiles cannot change the normal suite's fixed catalogue inventory.
go test -race -tags=integration,extension -count=1 -timeout=5m -v -run '^TestRegisteredExtensionsPublic$' ./tests/integration
OLP_SDK_SMOKE_SURFACES=openai,anthropic,gemini ./tests/sdk-smoke/run.sh
export OLP_DATABASE_URL="$OLP_TEST_DATABASE_URL" OLP_VALKEY_URL="$OLP_TEST_VALKEY_URL"
# Browser OIDC uses a separate, explicitly test-only binary. Release builds
# never allow loopback identity issuers.
go build -tags=oidctest -ldflags "-X github.com/tyk-swe/olp/internal/process.Version=$(node -p 'require("./package.json").version')" -o .local/bin/olp-identity-test ./cmd/olp
for database in olp_go_packaged olp_go_vite; do
  "${compose[@]}" exec -T postgres createdb -U olp_go "$database"
done
export OLP_CONSOLE_E2E_BIN="$PWD/.local/bin/olp-identity-test"
pnpm --dir console exec playwright test --config playwright.config.ts

export OLP_TEST_DATABASE_URL_PREFIX="postgres://olp_go:olp-go-local@$postgres"
export OLP_LOCAL_DIR="$scratch"
for origin in true false; do
  export OLP_TEST_RUN_TOKEN="$(openssl rand -hex 5)"
  OLP_CONSOLE_E2E_PACKAGED="$origin" ./scripts/browser-integration.sh
done
