#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
usage() {
  echo 'usage: integration.sh [all|shell|code|LANE...], where LANE is codecli, clients, a-f, g-m, n-r, s-z, smoke or browser' >&2
  exit 2
}
lanes=("$@")
case "${lanes[*]:-all}" in
  all) lanes=(codecli clients a-f g-m n-r s-z smoke browser) ;;
  shell) lanes=(codecli clients a-f g-m n-r s-z smoke) ;;
  code) ;;
  *)
    for lane in "${lanes[@]}"; do
      case "$lane" in qualification|codecli|clients|a-f|g-m|n-z|n-r|s-z|smoke|browser) ;; *) usage ;; esac
    done
    ;;
esac
expanded=()
for lane in "${lanes[@]}"; do
  case "$lane" in
    qualification) expanded+=(codecli clients) ;;
    n-z) expanded+=(n-r s-z) ;;
    *) expanded+=("$lane") ;;
  esac
done
lanes=("${expanded[@]}")
need_replica=false
need_regional=false
for lane in "${lanes[@]}"; do
  case "$lane" in
    g-m) need_replica=true ;;
    n-r) need_regional=true ;;
  esac
done
scratch=$(mktemp -d)
export OLP_TEST_TLS_DIR="$scratch/tls"
mkdir -p "$OLP_TEST_TLS_DIR"
chmod 755 "$scratch" "$OLP_TEST_TLS_DIR"
export OLP_POSTGRES_PORT=0 OLP_VALKEY_PORT=0 OLP_CACHE_VALKEY_PORT=0
project="olp-test-$$-$RANDOM"
compose=(docker compose -p "$project" -f deploy/compose.dev.yaml -f deploy/compose.integration.yaml -f deploy/compose.replica.yaml)
cleanup() {
  status=$?
  trap - EXIT INT TERM
  if (( status != 0 )); then "${compose[@]}" --profile '*' logs --no-color >&2 || true; fi
  if [[ -n ${restore_valkey:-} ]]; then docker rm -f "$restore_valkey" >/dev/null 2>&1 || true; fi
  "${compose[@]}" --profile '*' down -v --remove-orphans >&2 || true
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
cache_valkey=$("${compose[@]}" port cache 6379)
export OLP_TEST_CACHE_VALKEY_URL="redis://:olp-local@$cache_valkey/0"
export OLP_TEST_DATABASE_URL="postgres://olp:olp-local@$postgres/olp?sslmode=disable"
export OLP_TEST_DATABASE_ADMIN_URL="postgres://olp:olp-local@$postgres/postgres?sslmode=disable"
export OLP_TEST_DATABASE_TLS_URL="postgres://olp:olp-local@$postgres/olp?sslmode=verify-full&sslrootcert=$OLP_TEST_TLS_DIR/ca.crt"
export OLP_TEST_VALKEY_URL="redis://:olp-local@$valkey/0"
export OLP_TEST_VALKEY_TLS_URL="rediss://:olp-local@$valkey_tls/0"
export OLP_TEST_CA_FILE="$OLP_TEST_TLS_DIR/ca.crt"
export OLP_TEST_BINARY="$PWD/.local/bin/olp"
if [[ ${OLP_TEST_SKIP_BUILD:-0} == 1 ]]; then
  test -x "$OLP_TEST_BINARY"
  test -d console/build
else
  make build
fi
source scripts/secrets.sh "$scratch/secrets"
OLP_DATABASE_URL="$OLP_TEST_DATABASE_URL" "$OLP_TEST_BINARY" migrate
if [[ $need_replica == true ]]; then
  # The physical standby uses the disposable installation's authenticated TLS
  # connection. Explicitly qualify lag instead of treating successful reads as
  # evidence that revocations have reached a regional gateway.
  "${compose[@]}" exec -T -u postgres postgres bash -ec 'printf "hostssl replication olp all scram-sha-256\n" >> "$PGDATA/pg_hba.conf"; psql -U olp -d olp -c "SELECT pg_reload_conf()" >/dev/null'
  "${compose[@]}" up -d --wait --wait-timeout 90 postgres-replica
  replica=$("${compose[@]}" port postgres-replica 5432)
  export OLP_TEST_DATABASE_READ_URL="postgres://olp:olp-local@$replica/olp?sslmode=disable"
fi
if [[ $need_regional == true ]]; then
  "${compose[@]}" up -d --wait --wait-timeout 90 valkey-region
  regional=$("${compose[@]}" port valkey-region 6380)
  export OLP_TEST_VALKEY_REGIONAL_URL="rediss://:olp-local@$regional/0"
fi
if [[ -z ${OLP_CODECLI_ENV:-} ]]; then
  OLP_CODECLI_ENV=$(./scripts/code-mode-qualification.sh resolve)
fi
source "$OLP_CODECLI_ENV"
if [[ ${lanes[*]} == code ]]; then
  go test -mod=readonly -race -tags=integration,codecli -count=1 -timeout=25m -run '^TestCode' ./tests/integration
  ./scripts/code-mode-qualification.sh cli
  exit
fi
if [[ $need_regional == true ]]; then
  # Recovery tests use a separately provisioned Valkey, never a logical database
  # within the original service.
  restore_valkey="${project}-restore-valkey"
  docker run --detach --rm --name "$restore_valkey" -p 127.0.0.1::6379 valkey/valkey:9-alpine valkey-server --requirepass olp-local >/dev/null
  OLP_TEST_RESTORE_VALKEY_URL="redis://:olp-local@$(docker port "$restore_valkey" 6379/tcp)/0"
  export OLP_TEST_RESTORE_VALKEY_URL
fi
# Each selection keeps the deliberate recovery/enrollment waits inside the
# package deadline. The selections of ./tests/integration across the codecli,
# a-f, g-m, n-r and s-z lanes are disjoint and exhaustive.
process_tests() {
  go test -race -tags=integration,oidctest,pythonsdk,codecli -count=1 -timeout=30m "$@" ./tests/integration
}
for lane in "${lanes[@]}"; do
  case "$lane" in
    codecli)
      process_tests -run '^TestCode'
      ./scripts/code-mode-qualification.sh cli
      ;;
    clients)
      # Latest coding agents and pinned agent SDKs/frameworks against a scripted
      # upstream. It starts its own gateway and uses none of the services above.
      ./tests/clients/run.sh
      ;;
    a-f) process_tests -run '^Test[A-F]' -skip '^TestCode' ;;
    g-m) process_tests -run '^Test[G-M]' ;;
    n-r) process_tests -run '^Test[N-R]' ;;
    s-z)
      process_tests -run '^Test($|[^A-R])'
      go test -race -tags=integration,oidctest -count=1 -timeout=30m -run '^TestIntegration' ./internal/database ./internal/export ./internal/gateway ./internal/providers ./internal/media ./internal/usage ./internal/process ./internal/runtime ./internal/cache
      # Test-only trusted registry additions run in their own process, so dynamic
      # fixture profiles cannot change the normal suite's fixed catalogue inventory.
      go test -race -tags=integration,extension -count=1 -timeout=5m -run '^TestRegisteredExtensionsPublic$' ./tests/integration
      ;;
    smoke) OLP_SDK_SMOKE_SURFACES=openai,anthropic,gemini ./tests/sdk-smoke/run.sh ;;
    browser) (
      export OLP_DATABASE_URL="$OLP_TEST_DATABASE_URL" OLP_VALKEY_URL="$OLP_TEST_VALKEY_URL"
      # Browser OIDC uses a separate, explicitly test-only binary. Release builds
      # never allow loopback identity issuers.
      make build-go GO_BUILD_OUTPUT=.local/bin/olp-identity-test GO_BUILD_TAGS=oidctest
      for database in olp_packaged olp_vite olp_fleet_first olp_fleet_second; do
        "${compose[@]}" exec -T postgres createdb -U olp "$database"
      done
      export OLP_CONSOLE_E2E_BIN="$PWD/.local/bin/olp-identity-test"
      export OLP_CONSOLE_E2E_FLEET_SECRET_DIR="$scratch/fleet-secrets"
      pnpm --dir console exec playwright test --config playwright.config.ts

      export OLP_TEST_DATABASE_URL_PREFIX="postgres://olp:olp-local@$postgres"
      export OLP_LOCAL_DIR="$scratch"
      OLP_TEST_RUN_TOKEN="$(openssl rand -hex 5)"
      export OLP_TEST_RUN_TOKEN
      OLP_CONSOLE_E2E_PACKAGED=true ./scripts/browser-integration.sh
    ) ;;
  esac
done
