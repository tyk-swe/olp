#!/usr/bin/env bash
set -euo pipefail
: "${OLP_RESTORE_DATABASE_URL:?set OLP_RESTORE_DATABASE_URL to an empty destination database}"
backup=${1:?usage: scripts/restore.sh BACKUP}
manifest="$backup.manifest.json"
[[ -f $backup && -f $manifest ]] || { echo 'Backup and manifest are required.' >&2; exit 1; }
checksum=$(sha256sum < "$backup")
checksum=${checksum%% *}
jq -e --arg checksum "$checksum" \
  '.format == "olp3" and .schema == "olp_v3" and .checksum == $checksum and .accounting_drained == true' \
  "$manifest" >/dev/null
if jq -e '.migration_history != null' "$manifest" >/dev/null; then
  while IFS=$'\t' read -r version checksum; do
    [[ $version =~ ^[0-9]+$ && $checksum =~ ^[0-9a-f]{96}$ ]] || exit 1
    printf -v migration_prefix '%04d' "$version"
    files=("$(dirname "$0")/../migrations/${migration_prefix}_"*.sql)
    [[ ${#files[@]} == 1 && -f ${files[0]} ]] || { echo 'Backup migration is not supported by this checkout.' >&2; exit 1; }
    actual_checksum=$(sha384sum < "${files[0]}")
    [[ ${actual_checksum%% *} == "$checksum" ]] || { echo 'Backup migration checksum differs from this checkout.' >&2; exit 1; }
  done < <(jq -r '.migration_history[] | [.version, .checksum] | @tsv' "$manifest")
fi
objects=$(psql "$OLP_RESTORE_DATABASE_URL" -XAt -v ON_ERROR_STOP=1 -c \
  "SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname NOT IN ('pg_catalog', 'information_schema') AND n.nspname NOT LIKE 'pg_toast%' AND n.nspname NOT LIKE 'pg_temp_%'")
[[ $objects == 0 ]] || { echo 'The restore destination must be empty.' >&2; exit 1; }
pg_restore --dbname="$OLP_RESTORE_DATABASE_URL" --single-transaction --exit-on-error \
  --no-owner --no-privileges "$backup"
actual=$(psql "$OLP_RESTORE_DATABASE_URL" -XAt -v ON_ERROR_STOP=1 -c \
  "SELECT json_build_object('installation', (SELECT id FROM olp_v3.installation_identity WHERE singleton), 'migrations', (SELECT count(*) FROM olp_v3._sqlx_migrations WHERE success), 'generation', (SELECT COALESCE(max(sequence), 0) FROM olp_v3.runtime_generations), 'migration_history', (SELECT json_agg(json_build_object('version', version, 'description', description, 'checksum', encode(checksum, 'hex')) ORDER BY version) FROM olp_v3._sqlx_migrations WHERE success))")
jq -e --argjson actual "$actual" \
  '.installation == $actual.installation and .migrations == $actual.migrations and .generation == $actual.generation and (.migration_history == null or .migration_history == $actual.migration_history)' "$manifest" >/dev/null
printf 'Restored and verified the 3.0 installation.\n'
