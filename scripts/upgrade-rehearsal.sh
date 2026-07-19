#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat >&2 <<'USAGE'
usage: OLP_REHEARSAL_DATABASE_URL=postgres://... upgrade-rehearsal.sh BACKUP

OLP_REHEARSAL_CONFIRM=destroy-target must be set. The script restores the
backup into that isolated database, runs the current migrator twice, and proves
that the second run is idempotent. The target migration set is derived from the
tracked SQL migrations. Set OLP_REHEARSAL_EXPECTED_NEW_MIGRATIONS to require
an exact number of newly applied migrations, or
OLP_REHEARSAL_PREVIOUS_RELEASED_SCHEMA_MIGRATION to derive it from a released
migration marker.
USAGE
}

if [[ $# -ne 1 || ${1:-} == "--help" || ${1:-} == "-h" ]]; then
  usage
  [[ ${1:-} == "--help" || ${1:-} == "-h" ]] && exit 0 || exit 2
fi

if [[ -v OLP_REHEARSAL_MIN_NEW_MIGRATIONS ]]; then
  echo "OLP_REHEARSAL_MIN_NEW_MIGRATIONS is no longer supported; set OLP_REHEARSAL_EXPECTED_NEW_MIGRATIONS to the exact migration count" >&2
  exit 2
fi

: "${OLP_REHEARSAL_DATABASE_URL:?OLP_REHEARSAL_DATABASE_URL is required}"
expected_new_migrations=${OLP_REHEARSAL_EXPECTED_NEW_MIGRATIONS:-}
if [[ -n $expected_new_migrations ]]; then
  [[ $expected_new_migrations =~ ^[0-9]+$ ]] || {
    echo "OLP_REHEARSAL_EXPECTED_NEW_MIGRATIONS must be a non-negative integer" >&2
    exit 1
  }
  expected_new_migrations=$((10#$expected_new_migrations))
fi
[[ ${OLP_REHEARSAL_CONFIRM:-} == "destroy-target" ]] || {
  echo "set OLP_REHEARSAL_CONFIRM=destroy-target for the isolated rehearsal database" >&2
  exit 1
}

root_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
olp_bin=${OLP_BIN:-"$root_dir/target/release/olp"}
psql_command=${OLP_PSQL:-psql}
migration_directory="$root_dir/crates/storage/migrations"
shopt -s nullglob
migration_files=("$migration_directory"/[0-9][0-9][0-9][0-9]_*.sql)
shopt -u nullglob
(( ${#migration_files[@]} > 0 )) || {
  echo "no tracked migrations found in $migration_directory" >&2
  exit 1
}
mapfile -t migration_files < <(printf '%s\n' "${migration_files[@]}" | LC_ALL=C sort)
tracked_migration_versions=()
last_tracked_version=0
for migration_file in "${migration_files[@]}"; do
  migration_name=${migration_file##*/}
  migration_version=${migration_name%%_*}
  [[ $migration_version =~ ^[0-9]{4}$ ]] || {
    echo "invalid tracked migration filename: $migration_name" >&2
    exit 1
  }
  migration_version=$((10#$migration_version))
  (( migration_version > last_tracked_version )) || {
    echo "tracked migration versions are not strictly increasing: $migration_name" >&2
    exit 1
  }
  tracked_migration_versions+=("$migration_version")
  last_tracked_version=$migration_version
done
previous_released_migration=${OLP_REHEARSAL_PREVIOUS_RELEASED_SCHEMA_MIGRATION:-}
if [[ -n $previous_released_migration ]]; then
  [[ -z $expected_new_migrations ]] || {
    echo "set either OLP_REHEARSAL_EXPECTED_NEW_MIGRATIONS or OLP_REHEARSAL_PREVIOUS_RELEASED_SCHEMA_MIGRATION, not both" >&2
    exit 2
  }
  [[ $previous_released_migration =~ ^[0-9]{4}$ ]] || {
    echo "OLP_REHEARSAL_PREVIOUS_RELEASED_SCHEMA_MIGRATION must be a four-digit migration version" >&2
    exit 1
  }
  previous_released_version=$((10#$previous_released_migration))
  previous_released_index=-1
  for migration_index in "${!tracked_migration_versions[@]}"; do
    if (( tracked_migration_versions[migration_index] == previous_released_version )); then
      (( previous_released_index == -1 )) || {
        echo "released migration $previous_released_migration matches more than one tracked migration" >&2
        exit 1
      }
      previous_released_index=$migration_index
    fi
  done
  (( previous_released_index >= 0 )) || {
    echo "released migration $previous_released_migration does not match a tracked migration filename" >&2
    exit 1
  }
  expected_new_migrations=$((${#tracked_migration_versions[@]} - previous_released_index - 1))
fi
if [[ ! -x $olp_bin ]]; then
  cargo build --locked --release --manifest-path "$root_dir/Cargo.toml" -p olp
fi

OLP_RESTORE_DATABASE_URL=$OLP_REHEARSAL_DATABASE_URL \
  "$root_dir/scripts/restore-rehearsal.sh" "$1" --replace

migration_versions() {
  "$psql_command" "$OLP_REHEARSAL_DATABASE_URL" -X --no-psqlrc --tuples-only --no-align \
    --command='SELECT version FROM _sqlx_migrations WHERE success ORDER BY version'
}

validate_migration_versions() {
  local label=$1 raw_version normalized_version index
  shift
  local -a observed_versions=("$@")
  (( ${#observed_versions[@]} <= ${#tracked_migration_versions[@]} )) || {
    echo "$label has more successful migrations than the tracked source" >&2
    exit 1
  }
  for ((index = 0; index < ${#observed_versions[@]}; index++)); do
    raw_version=${observed_versions[$index]}
    [[ $raw_version =~ ^[0-9]+$ ]] || {
      echo "$label contains an invalid migration version: $raw_version" >&2
      exit 1
    }
    normalized_version=$((10#$raw_version))
    (( normalized_version == tracked_migration_versions[index] )) || {
      echo "$label does not match the tracked migration order at version $raw_version" >&2
      exit 1
    }
  done
}

before_output=$(migration_versions)
before_versions=()
if [[ -n $before_output ]]; then
  mapfile -t before_versions <<<"$before_output"
fi
validate_migration_versions "restored backup" "${before_versions[@]}"
OLP_DATABASE_URL=$OLP_REHEARSAL_DATABASE_URL "$olp_bin" migrate
after_first_output=$(migration_versions)
after_first_versions=()
if [[ -n $after_first_output ]]; then
  mapfile -t after_first_versions <<<"$after_first_output"
fi
validate_migration_versions "first migration run" "${after_first_versions[@]}"
(( ${#after_first_versions[@]} == ${#tracked_migration_versions[@]} )) || {
  echo "migration runner did not apply every tracked migration: applied=${#after_first_versions[@]} tracked=${#tracked_migration_versions[@]}" >&2
  exit 1
}
OLP_DATABASE_URL=$OLP_REHEARSAL_DATABASE_URL "$olp_bin" migrate
after_second_output=$(migration_versions)
after_second_versions=()
if [[ -n $after_second_output ]]; then
  mapfile -t after_second_versions <<<"$after_second_output"
fi
validate_migration_versions "second migration run" "${after_second_versions[@]}"

[[ ${after_first_versions[*]} == "${after_second_versions[*]}" ]] || {
  echo "migration runner is not idempotent" >&2
  exit 1
}
new_migrations=$((${#after_first_versions[@]} - ${#before_versions[@]}))
if [[ -n $expected_new_migrations ]]; then
  (( new_migrations == expected_new_migrations )) || {
    echo "upgrade applied $new_migrations migrations; expected exactly $expected_new_migrations" >&2
    exit 1
  }
fi

if [[ ${OLP_REHEARSAL_RUN_DOCTOR:-false} == true ]]; then
  : "${OLP_VALKEY_URL:?OLP_VALKEY_URL is required for upgrade doctor smoke}"
  : "${OLP_MASTER_KEY_FILE:?OLP_MASTER_KEY_FILE is required for upgrade doctor smoke}"
  : "${OLP_KEY_HASH_KEY_FILE:?OLP_KEY_HASH_KEY_FILE is required for upgrade doctor smoke}"
  : "${OLP_CONSOLE_DIR:?OLP_CONSOLE_DIR is required for upgrade doctor smoke}"
  OLP_DATABASE_URL=$OLP_REHEARSAL_DATABASE_URL "$olp_bin" doctor >/dev/null
fi

echo "upgrade rehearsal passed: migrations ${#before_versions[@]} -> ${#after_first_versions[@]} (tracked=${#tracked_migration_versions[@]}); new=${new_migrations}; second run unchanged; doctor=${OLP_REHEARSAL_RUN_DOCTOR:-false}"
