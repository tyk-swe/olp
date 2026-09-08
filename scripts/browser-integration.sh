#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
# shellcheck source=scripts/lib/disposable-database.sh
source scripts/lib/disposable-database.sh
browser_db="olp_browser_${OLP_TEST_RUN_TOKEN}"
restore_db="olp_restore_${OLP_TEST_RUN_TOKEN}"
cleanup() {
  local status=$?
  drop_disposable_database "$browser_db"
  drop_disposable_database "$restore_db"
  exit "$status"
}
trap cleanup EXIT
create_disposable_database "$browser_db"
export OLP_CONSOLE_E2E_DATABASE_URL="$OLP_TEST_DATABASE_URL_PREFIX/$browser_db"
export OLP_CONSOLE_E2E_MASTER_KEY_FILE="$OLP_MASTER_KEY_FILE"
export OLP_CONSOLE_E2E_AUTH_HMAC_KEY_FILE="$OLP_AUTH_HMAC_KEY_FILE"
export OLP_CONSOLE_E2E_BOOTSTRAP_TOKEN_FILE="$OLP_BOOTSTRAP_TOKEN_FILE"
pnpm --dir console exec playwright install chromium
pnpm --dir console test:e2e
create_disposable_database "$restore_db"
backup=$(OLP_DATABASE_URL="$OLP_CONSOLE_E2E_DATABASE_URL" OLP_BACKUP_TRAFFIC_QUIESCED=true \
  ./scripts/backup.sh "$OLP_LOCAL_DIR/backups/$OLP_TEST_RUN_TOKEN")
OLP_RESTORE_DATABASE_URL="$OLP_TEST_DATABASE_URL_PREFIX/$restore_db" ./scripts/restore.sh "$backup"
export OLP_CONSOLE_E2E_DATABASE_URL="$OLP_TEST_DATABASE_URL_PREFIX/$restore_db"
export OLP_CONSOLE_E2E_RESTORED=true
auth_wait=$(psql "$OLP_CONSOLE_E2E_DATABASE_URL" -XAt -v ON_ERROR_STOP=1 -c \
  "SELECT GREATEST(0, COALESCE(ceil(extract(epoch FROM max(window_started_at) + interval '1 minute' - clock_timestamp())), 0))::int FROM olp_v3.public_auth_rate_limits WHERE action = 'local_login' AND scope = 'source_target' AND attempts >= 5")
[[ $auth_wait =~ ^[0-9]+$ && $auth_wait -le 60 ]] || { echo 'Restored authentication window is outside the expected clock bound.' >&2; exit 1; }
if (( auth_wait > 0 )); then
  echo "Waiting $auth_wait seconds for the preserved authentication rate limit."
  sleep "$auth_wait"
fi
before=$(psql "$OLP_CONSOLE_E2E_DATABASE_URL" -XAt -v ON_ERROR_STOP=1 -c 'SELECT count(*) FROM olp_v3.requests')
pnpm --dir console test:e2e --grep 'restored installation'
after=$(psql "$OLP_CONSOLE_E2E_DATABASE_URL" -XAt -v ON_ERROR_STOP=1 -c 'SELECT count(*) FROM olp_v3.requests')
(( before > 0 && after > before )) || { echo 'Restored gateway did not preserve and extend request accounting.' >&2; exit 1; }
echo "Restore drill passed: historical requests=$before, requests after inference=$after"
