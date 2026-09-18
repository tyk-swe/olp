#!/usr/bin/env bash
set -euo pipefail
: "${OLP_RESTORE_DATABASE_URL:?set OLP_RESTORE_DATABASE_URL to an empty destination database}"
: "${OLP_RESTORE_VALKEY_ISOLATED:?use a separate Valkey service and set OLP_RESTORE_VALKEY_ISOLATED=true}"
[[ $OLP_RESTORE_VALKEY_ISOLATED == true ]] || exit 2
repo_dir=$(cd "$(dirname "$0")/.." && pwd)
backup=${1:?usage: scripts/restore.sh BACKUP}
manifest="$backup.manifest.json"
[[ -f $backup && -f $manifest ]] || { echo 'Backup and manifest are required.' >&2; exit 1; }
checksum=$(sha256sum < "$backup")
checksum=${checksum%% *}
jq -e --arg checksum "$checksum" '
  .format == "olp-go-v1" and .schema == "olp_go" and .checksum == $checksum and
  .accounting_drained == true and (.installation | type == "string") and
  (.migration_history | type == "array" and length > 0) and
  .migrations == (.migration_history | length)' "$manifest" >/dev/null
while IFS=$'\t' read -r version checksum; do
  [[ $version =~ ^[0-9]{4}_[a-z0-9_]+\.sql$ && $checksum =~ ^[0-9a-f]{64}$ ]] || exit 1
  file="$repo_dir/internal/database/migrations/$version"
  [[ -f $file ]] || { echo 'Backup migration is not supported by this checkout.' >&2; exit 1; }
  actual_checksum=$(sha256sum < "$file")
  [[ ${actual_checksum%% *} == "$checksum" ]] || { echo 'Backup migration checksum differs from this checkout.' >&2; exit 1; }
done < <(jq -r '.migration_history[] | [.version, .checksum] | @tsv' "$manifest")

umask 077
scratch=$(mktemp -d)
staging="olp_restore_verify_$(openssl rand -hex 12)"
created=false
cleanup() {
  if [[ $created == true ]]; then dropdb --maintenance-db="$OLP_RESTORE_DATABASE_URL" --if-exists --force "$staging"; fi
  rm -rf -- "$scratch"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
cat > "$scratch/empty.sql" <<'SQL'
SELECT pg_advisory_xact_lock(726419823071);
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM pg_namespace WHERE nspname NOT IN ('public','pg_catalog','information_schema') AND nspname NOT LIKE 'pg_toast%' AND nspname NOT LIKE 'pg_temp_%')
     OR EXISTS (SELECT 1 FROM pg_class WHERE relnamespace='public'::regnamespace)
     OR EXISTS (SELECT 1 FROM pg_proc WHERE pronamespace='public'::regnamespace)
     OR EXISTS (SELECT 1 FROM pg_type WHERE typnamespace='public'::regnamespace)
  THEN RAISE EXCEPTION 'The restore destination must be empty'; END IF;
END $$;
SQL
psql "$OLP_RESTORE_DATABASE_URL" -Xq -v ON_ERROR_STOP=1 --single-transaction -f "$scratch/empty.sql" >/dev/null
# Validate the complete dump and mounted keys before mutating the destination.
# CREATEDB is required for this isolated scratch database, which is always removed.
createdb --maintenance-db="$OLP_RESTORE_DATABASE_URL" --template=template0 "$staging"
created=true
staging_url=$(OLP_STAGING_NAME="$staging" python3 - <<'PY'
import os, urllib.parse
url = urllib.parse.urlsplit(os.environ['OLP_RESTORE_DATABASE_URL'])
print(urllib.parse.urlunsplit(url._replace(path='/'+os.environ['OLP_STAGING_NAME'])))
PY
)
pg_restore --dbname="$staging_url" --single-transaction --exit-on-error --no-owner --no-privileges "$backup"
actual=$(psql "$staging_url" -XAt -v ON_ERROR_STOP=1 -c \
  "SELECT json_build_object('installation', (SELECT id FROM olp_go.installation WHERE singleton), 'migrations', (SELECT count(*) FROM olp_go.migrations), 'generation', (SELECT COALESCE(max(sequence), 0) FROM olp_go.runtime_releases), 'migration_history', (SELECT json_agg(json_build_object('version', version, 'checksum', encode(checksum, 'hex')) ORDER BY version) FROM olp_go.migrations))")
jq -e --argjson actual "$actual" \
  '.installation == $actual.installation and .migrations == $actual.migrations and .generation == $actual.generation and .migration_history == $actual.migration_history' "$manifest" >/dev/null
export OLP_DATABASE_URL="$staging_url"
unset OLP_DATABASE_URL_FILE
if [[ -n ${OLP_CONSOLE_E2E_IMAGE:-} ]]; then
  source "$repo_dir/scripts/lib/image-container.sh"
  olp_image_container_args
  maintenance=(docker run --rm "${olp_image_args[@]}" --env OLP_DATABASE_URL --env OLP_VALKEY_URL
    --env OLP_MASTER_KEY_FILE --env OLP_AUTH_HMAC_KEY_FILE --env OLP_BOOTSTRAP_TOKEN_FILE "$OLP_CONSOLE_E2E_IMAGE")
else
  maintenance=("${OLP_MAINTENANCE_BIN:-$repo_dir/.local/bin/olp}")
fi
"${maintenance[@]}" migrate >/dev/null
"${maintenance[@]}" doctor >/dev/null
pg_dump "$staging_url" --schema=olp_go --no-owner --no-privileges --file="$scratch/restore.sql"
# Recheck emptiness under the same lock as migrate, and restore atomically.
psql "$OLP_RESTORE_DATABASE_URL" -Xq -v ON_ERROR_STOP=1 --single-transaction \
  -f "$scratch/empty.sql" -f "$scratch/restore.sql" >/dev/null
printf 'Restored and verified the Go installation, keys, migration history and runtime identity.\n'
