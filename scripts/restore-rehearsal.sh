#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat >&2 <<'USAGE'
usage: OLP_RESTORE_DATABASE_URL=postgres://... restore-rehearsal.sh BACKUP [--replace]

The destination must be an isolated rehearsal database. --replace is required
when it contains application objects and irreversibly cleans that destination.
USAGE
}

if [[ $# -lt 1 || ${1:-} == "--help" || ${1:-} == "-h" ]]; then
  usage
  [[ $# -ge 1 ]] && exit 0 || exit 2
fi

: "${OLP_RESTORE_DATABASE_URL:?OLP_RESTORE_DATABASE_URL must identify an isolated destination}"
backup=$1
replace=${2:-}
[[ -f $backup ]] || { echo "backup does not exist: $backup" >&2; exit 1; }
[[ -z ${OLP_DATABASE_URL:-} || $OLP_RESTORE_DATABASE_URL != "$OLP_DATABASE_URL" ]] || {
  echo "refusing to restore over OLP_DATABASE_URL" >&2
  exit 1
}
[[ -z $replace || $replace == "--replace" ]] || { usage; exit 2; }

pg_restore_command=${OLP_PG_RESTORE:-pg_restore}
psql_command=${OLP_PSQL:-psql}
for command in "$pg_restore_command" "$psql_command" sha256sum jq; do
  command -v "$command" >/dev/null || {
    echo "required command is unavailable: $command" >&2
    exit 1
  }
done

checksum_file="${backup}.sha256"
manifest_file="${backup}.manifest.json"
[[ -f $checksum_file ]] || { echo "backup checksum sidecar is required: $checksum_file" >&2; exit 1; }
[[ -f $manifest_file ]] || { echo "backup manifest is required: $manifest_file" >&2; exit 1; }
expected=$(awk 'NR == 1 {print $1}' "$checksum_file")
actual=$(sha256sum "$backup" | awk '{print $1}')
[[ $expected =~ ^[a-f0-9]{64}$ && $actual == "$expected" ]] || {
  echo "backup checksum mismatch" >&2
  exit 1
}
backup_name=$(basename "$backup")
jq -e --arg checksum "$actual" --arg backup_name "$backup_name" '
  (.format == "olp-v2-postgresql-custom-v1" or
   .format == "olp-v2-postgresql-custom-v2") and
  .backup_file == $backup_name and
  .sha256 == $checksum and
  .plaintext_secrets_included == false and
  .encrypted_sensitive_records_included == true and
  .mounted_key_files_included == false and
  (.traffic_quiesced | type == "boolean") and
  (if .format == "olp-v2-postgresql-custom-v1" then
     (.usage_stream_drained | type == "boolean") and
     (if .usage_stream_drained then
        .traffic_quiesced == true and
        (.usage_consumer_checked_at | type == "string" and
          test("^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$"))
      else .usage_consumer_checked_at == null end)
   else
     (.request_metadata_stream_drained | type == "boolean") and
     (if .request_metadata_stream_drained then
        .traffic_quiesced == true and
        (.request_metadata_consumer_checked_at | type == "string" and
          test("^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$"))
      else .request_metadata_consumer_checked_at == null end)
   end) and
  (.database_server_version | type == "string" and startswith("18.")) and
  (.successful_migrations | type == "number") and
  (.runtime_generation_ordinal | type == "number")
' "$manifest_file" >/dev/null || { echo "backup manifest is invalid or inconsistent" >&2; exit 1; }
"$pg_restore_command" --list "$backup" >/dev/null

user_objects=$("$psql_command" "$OLP_RESTORE_DATABASE_URL" -X --no-psqlrc --tuples-only --no-align \
  --command="SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname NOT IN ('pg_catalog','information_schema') AND n.nspname NOT LIKE 'pg_toast%'" \
  | tr -d '[:space:]')
restore_args=(--dbname="$OLP_RESTORE_DATABASE_URL" --no-owner --no-privileges --exit-on-error)
if [[ $user_objects != 0 ]]; then
  [[ $replace == "--replace" ]] || {
    echo "destination is not empty; pass --replace only for an isolated rehearsal database" >&2
    exit 1
  }
  non_public_schemas=$("$psql_command" "$OLP_RESTORE_DATABASE_URL" -X --no-psqlrc \
    --tuples-only --no-align --command="SELECT count(*) FROM pg_namespace WHERE nspname NOT IN ('public','pg_catalog','information_schema') AND nspname NOT LIKE 'pg_toast%' AND nspname NOT LIKE 'pg_temp_%'" \
    | tr -d '[:space:]')
  [[ $non_public_schemas == 0 ]] || {
    echo "destination contains non-public application schemas; refusing replacement" >&2
    exit 1
  }
  "$psql_command" "$OLP_RESTORE_DATABASE_URL" -X --no-psqlrc -v ON_ERROR_STOP=1 \
    --command='DROP SCHEMA public CASCADE' \
    --command='CREATE SCHEMA public AUTHORIZATION CURRENT_USER'
fi

started_at=$(date +%s)
"$pg_restore_command" "${restore_args[@]}" "$backup"

migration_count=$("$psql_command" "$OLP_RESTORE_DATABASE_URL" -X --no-psqlrc --tuples-only --no-align \
  --command='SELECT count(*) FROM _sqlx_migrations WHERE success' | tr -d '[:space:]')
failed_migrations=$("$psql_command" "$OLP_RESTORE_DATABASE_URL" -X --no-psqlrc --tuples-only --no-align \
  --command='SELECT count(*) FROM _sqlx_migrations WHERE NOT success' | tr -d '[:space:]')
generation_count=$("$psql_command" "$OLP_RESTORE_DATABASE_URL" -X --no-psqlrc --tuples-only --no-align \
  --command='SELECT count(*) FROM runtime_generations' | tr -d '[:space:]')
installation_count=$("$psql_command" "$OLP_RESTORE_DATABASE_URL" -X --no-psqlrc --tuples-only --no-align \
  --command='SELECT count(*) FROM installation' | tr -d '[:space:]')
latest_generation=$("$psql_command" "$OLP_RESTORE_DATABASE_URL" -X --no-psqlrc --tuples-only --no-align \
  --command='SELECT COALESCE(max(sequence), 0) FROM runtime_generations' | tr -d '[:space:]')
expected_migrations=$(jq -r '.successful_migrations' "$manifest_file")
expected_generation=$(jq -r '.runtime_generation_ordinal' "$manifest_file")
[[ $failed_migrations == 0 ]] || { echo "restored database contains failed migrations" >&2; exit 1; }
[[ $migration_count == "$expected_migrations" ]] || {
  echo "restored migration count differs from backup manifest" >&2
  exit 1
}
[[ $latest_generation == "$expected_generation" ]] || {
  echo "restored runtime generation differs from backup manifest" >&2
  exit 1
}

elapsed_seconds=$(( $(date +%s) - started_at ))
printf 'restore verified: migrations=%s generations=%s installations=%s elapsed_seconds=%s\n' \
  "$migration_count" "$generation_count" "$installation_count" "$elapsed_seconds"
