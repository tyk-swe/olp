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

restore_expectations=$("$manifest_tool" validate "$backup")
IFS=$'\t' read -r expected_migrations expected_generation <<< "$restore_expectations"
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
