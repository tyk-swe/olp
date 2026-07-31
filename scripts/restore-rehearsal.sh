#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat >&2 <<'USAGE'
usage: OLP_DATABASE_URL=postgres://... OLP_RESTORE_DATABASE_URL=postgres://... \
  restore-rehearsal.sh BACKUP [--replace]

OLP_DATABASE_URL identifies the protected production database. The destination
must be an isolated rehearsal database. --replace is required when it contains
application objects and irreversibly cleans that destination.
USAGE
}

if [[ ${1:-} == "--help" || ${1:-} == "-h" ]]; then
  usage
  exit 0
fi
if (( $# < 1 || $# > 2 )); then
  usage
  exit 2
fi

: "${OLP_RESTORE_DATABASE_URL:?OLP_RESTORE_DATABASE_URL must identify an isolated destination}"
: "${OLP_DATABASE_URL:?OLP_DATABASE_URL must identify the protected production database}"
backup=$1
replace=${2:-}
[[ -f $backup ]] || { echo "backup does not exist: $backup" >&2; exit 1; }
[[ -z $replace || $replace == "--replace" ]] || { usage; exit 2; }

pg_restore_command=${OLP_PG_RESTORE:-pg_restore}
psql_command=${OLP_PSQL:-psql}
script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
manifest_tool="$script_dir/backup-manifest.sh"
[[ -x $manifest_tool ]] || {
  echo "required executable is unavailable: $manifest_tool" >&2
  exit 1
}
for command in "$pg_restore_command" "$psql_command"; do
  command -v "$command" >/dev/null || {
    echo "required command is unavailable: $command" >&2
    exit 1
  }
done

identity_sql="SELECT system_identifier::text || ':' || oid::text FROM pg_catalog.pg_control_system(), pg_catalog.pg_database WHERE datname = pg_catalog.current_database()"

database_identity() {
  local label=$1 url=$2 identity
  if ! identity=$("$psql_command" "$url" -X --no-psqlrc --tuples-only --no-align \
    --set=ON_ERROR_STOP=1 --command="$identity_sql"); then
    echo "failed to establish $label database identity with pg_control_system()" >&2
    return 1
  fi
  [[ $identity =~ ^[1-9][0-9]*:[1-9][0-9]*$ ]] || {
    echo "$label database returned an invalid PostgreSQL identity" >&2
    return 1
  }
  printf '%s\n' "$identity"
}

protected_identity=$(database_identity protected "$OLP_DATABASE_URL")
restore_identity=$(database_identity restore "$OLP_RESTORE_DATABASE_URL")
[[ $restore_identity != "$protected_identity" ]] || {
  echo "refusing to restore: OLP_DATABASE_URL and OLP_RESTORE_DATABASE_URL identify the same PostgreSQL database" >&2
  exit 1
}
restore_identity_guard="DO \$olp_restore_guard\$ BEGIN IF ($identity_sql) IS DISTINCT FROM '$restore_identity' THEN RAISE EXCEPTION 'restore database identity changed after safety check'; END IF; END \$olp_restore_guard\$;"

restore_expectations=$("$manifest_tool" validate "$backup")
IFS=$'\t' read -r expected_migrations expected_generation <<< "$restore_expectations"
"$pg_restore_command" --list "$backup" >/dev/null

user_objects=$("$psql_command" "$OLP_RESTORE_DATABASE_URL" -X --no-psqlrc --tuples-only --no-align \
  --command="SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname NOT IN ('pg_catalog','information_schema') AND n.nspname NOT LIKE 'pg_toast%'" \
  | tr -d '[:space:]')
restore_args=(--no-owner --no-privileges --file=-)
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
    --command="$restore_identity_guard" \
    --command='DROP SCHEMA public CASCADE' \
    --command='CREATE SCHEMA public AUTHORIZATION CURRENT_USER'
fi

started_at=$(date +%s)
{
  printf '%s\n' "$restore_identity_guard"
  "$pg_restore_command" "${restore_args[@]}" "$backup"
} | "$psql_command" "$OLP_RESTORE_DATABASE_URL" -X --no-psqlrc --set=ON_ERROR_STOP=1

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
