#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
docker compose -f deploy/compose.dev.yaml --profile integration up -d --wait
export OLP_LOCAL_DIR="$PWD/.local/integration"
source scripts/local-env.sh
export OLP_TEST_DATABASE_ADMIN_URL=postgres://olp:olp-local@127.0.0.1:54320/postgres
export OLP_TEST_DATABASE_URL_PREFIX=postgres://olp:olp-local@127.0.0.1:54320
export OLP_TEST_DATABASE_OWNER=olp
export OLP_TEST_RUN_TOKEN="$(openssl rand -hex 5)"
export OLP_TEST_VALKEY_URL="$OLP_VALKEY_URL"
export OLP_E2E_DATABASE_ADMIN_URL="$OLP_TEST_DATABASE_ADMIN_URL"
export OLP_E2E_DATABASE_APP_ADMIN_URL=postgres://olp:olp-local@127.0.0.1:55432/postgres
export OLP_E2E_VALKEY_URL=redis://127.0.0.1:56379/0
export OLP_E2E_TOXIPROXY_API=http://127.0.0.1:58474
export OLP_E2E_DATABASE_PROXY_NAME=postgres
export OLP_E2E_VALKEY_PROXY_NAME=valkey
curl --fail --silent --show-error --request POST "$OLP_E2E_TOXIPROXY_API/reset" >/dev/null
export OLP_E2E_RUN_TOKEN="$OLP_TEST_RUN_TOKEN"
cleanup() {
  local status=$?
  trap - EXIT INT TERM
  curl --silent --request POST "$OLP_E2E_TOXIPROXY_API/reset" >/dev/null || true
  for pid_file in /tmp/olp-e2e-"$OLP_E2E_RUN_TOKEN"-*/*.pid; do
    [[ -f $pid_file ]] || continue
    local process_id
    read -r process_id < "$pid_file"
    [[ $process_id =~ ^[0-9]+$ ]] && kill "$process_id" 2>/dev/null || true
  done
  psql "$OLP_TEST_DATABASE_ADMIN_URL" -XAt -v ON_ERROR_STOP=1 <<SQL || true
SELECT format('DROP DATABASE %I WITH (FORCE)', datname)
FROM pg_database
WHERE starts_with(datname, 'olp_test_${OLP_TEST_RUN_TOKEN}_')
   OR starts_with(datname, 'olp_e2e_${OLP_TEST_RUN_TOKEN}_')
   OR datname IN ('olp_browser_${OLP_TEST_RUN_TOKEN}', 'olp_restore_${OLP_TEST_RUN_TOKEN}')
\gexec
SQL
  rm -rf -- /tmp/olp-e2e-"$OLP_E2E_RUN_TOKEN"-*
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

cargo build --locked --features test-util --bin olp --example sdk_smoke_fixture
export OLP_E2E_BIN="$PWD/target/debug/olp"
cargo test --locked --all-features --lib --test persistence --test system -- --ignored --skip live_provider --test-threads=4
cargo test --locked --all-features --test contract --test ha -- --ignored --test-threads=1
./tests/sdk-smoke/run.sh

browser_db="olp_browser_${OLP_TEST_RUN_TOKEN}"
psql "$OLP_TEST_DATABASE_ADMIN_URL" -v ON_ERROR_STOP=1 -c "CREATE DATABASE $browser_db"
export OLP_CONSOLE_E2E_DATABASE_URL="$OLP_TEST_DATABASE_URL_PREFIX/$browser_db"
export OLP_CONSOLE_E2E_MASTER_KEY_FILE="$OLP_MASTER_KEY_FILE"
export OLP_CONSOLE_E2E_AUTH_HMAC_KEY_FILE="$OLP_AUTH_HMAC_KEY_FILE"
export OLP_CONSOLE_E2E_BOOTSTRAP_TOKEN_FILE="$OLP_BOOTSTRAP_TOKEN_FILE"
export OLP_CONSOLE_E2E_BIN="$OLP_E2E_BIN"
pnpm --dir console exec playwright install chromium
pnpm --dir console test:e2e

restore_db="olp_restore_${OLP_TEST_RUN_TOKEN}"
psql "$OLP_TEST_DATABASE_ADMIN_URL" -X -v ON_ERROR_STOP=1 -c "CREATE DATABASE $restore_db"
backup=$(OLP_DATABASE_URL="$OLP_CONSOLE_E2E_DATABASE_URL" OLP_BACKUP_TRAFFIC_QUIESCED=true \
  ./scripts/backup.sh "$OLP_LOCAL_DIR/backups/$OLP_TEST_RUN_TOKEN")
OLP_RESTORE_DATABASE_URL="$OLP_TEST_DATABASE_URL_PREFIX/$restore_db" ./scripts/restore.sh "$backup"
OLP_DATABASE_URL="$OLP_TEST_DATABASE_URL_PREFIX/$restore_db" "$OLP_E2E_BIN" master-key status
