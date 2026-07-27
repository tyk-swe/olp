#!/usr/bin/env bash
set -euo pipefail

# Backup / restore / upgrade rehearsal driven by CI's upgrade-rehearsal job
# and reproducible locally against a disposable PostgreSQL 18 + Valkey.
# The caller has already built the binary and migrated the current-release
# database (OLP_DATABASE_URL).
#
# Required environment:
#   OLP_REHEARSAL_SERVER_URL  server URL without a database name, e.g.
#                             postgres://olp:olp@localhost:5432
#   OLP_DATABASE_URL          migrated current-release database
#   OLP_VALKEY_URL            Valkey for the doctor smoke
#   OLP_BIN                   path to the olp binary
# Optional:
#   OLP_REHEARSAL_SCRATCH_DIR scratch directory (default: mktemp -d)
#   OLP_EXPECTED_PG_BINDIR    assert pg_dump/pg_restore resolve from here
#                             (CI uses it to prove the PGDG install won)

: "${OLP_REHEARSAL_SERVER_URL:?set OLP_REHEARSAL_SERVER_URL without a trailing database name}"
: "${OLP_DATABASE_URL:?set OLP_DATABASE_URL to the migrated current-release database}"
: "${OLP_VALKEY_URL:?set OLP_VALKEY_URL for the doctor smoke}"
: "${OLP_BIN:?set OLP_BIN to the olp binary}"

scratch_dir=${OLP_REHEARSAL_SCRATCH_DIR:-$(mktemp -d)}

if [[ -n ${OLP_EXPECTED_PG_BINDIR:-} ]]; then
  [[ $(command -v pg_dump) == "$OLP_EXPECTED_PG_BINDIR/pg_dump" ]]
  [[ $(command -v pg_restore) == "$OLP_EXPECTED_PG_BINDIR/pg_restore" ]]
fi
pg_dump --version | grep -Eq '^pg_dump \(PostgreSQL\) 18([.]|$)'
pg_restore --version | grep -Eq '^pg_restore \(PostgreSQL\) 18([.]|$)'

psql "$OLP_REHEARSAL_SERVER_URL/postgres" -v ON_ERROR_STOP=1 \
  -c 'CREATE DATABASE olp_restore' \
  -c 'CREATE DATABASE olp_legacy_restore' \
  -c 'CREATE DATABASE olp_upgrade' \
  -c 'CREATE DATABASE olp_previous'

backup=$(./scripts/backup.sh "$scratch_dir/olp-backups")
./scripts/backup-manifest.sh validate "$backup" v2 >/dev/null
OLP_RESTORE_DATABASE_URL="$OLP_REHEARSAL_SERVER_URL/olp_restore" \
  ./scripts/restore-rehearsal.sh "$backup"
./scripts/backup-manifest.sh convert-v2-to-v1 "$backup"
./scripts/backup-manifest.sh validate "$backup" v1 >/dev/null
OLP_RESTORE_DATABASE_URL="$OLP_REHEARSAL_SERVER_URL/olp_legacy_restore" \
  ./scripts/restore-rehearsal.sh "$backup"

metadata_file=release-metadata.env
[[ -f $metadata_file ]] || {
  echo "required release metadata file is missing: $metadata_file" >&2
  exit 1
}
previous_migration=''
metadata_assignments=0
while IFS= read -r line || [[ -n $line ]]; do
  [[ $line =~ ^[[:space:]]*($|#) ]] && continue
  if [[ $line =~ ^OLP_PREVIOUS_RELEASED_SCHEMA_MIGRATION=([0-9]{4})$ ]]; then
    previous_migration=${BASH_REMATCH[1]}
    ((metadata_assignments += 1))
    continue
  fi
  echo 'release metadata contains an unsupported line' >&2
  exit 1
done <"$metadata_file"
((metadata_assignments == 1)) || {
  echo 'release metadata must contain exactly one OLP_PREVIOUS_RELEASED_SCHEMA_MIGRATION=NNNN assignment' >&2
  exit 1
}
previous_version=$((10#$previous_migration))

OLP_DATABASE_URL="$OLP_REHEARSAL_SERVER_URL/olp_previous" \
  OLP_ALLOW_PARTIAL_MIGRATIONS_FOR_TESTS=test-only \
  "$OLP_BIN" migrate --through-version "$previous_version"
psql "$OLP_REHEARSAL_SERVER_URL/olp_previous" -v ON_ERROR_STOP=1 \
  -c "INSERT INTO usage_consumer_health \
        (singleton, pending_events, lag_events, checked_at) \
      VALUES (true, 0, 0, now()) \
      ON CONFLICT (singleton) DO UPDATE SET \
        pending_events = 0, lag_events = 0, checked_at = now()"
previous_backup=$(OLP_DATABASE_URL="$OLP_REHEARSAL_SERVER_URL/olp_previous" \
  OLP_BACKUP_TRAFFIC_QUIESCED=true \
  ./scripts/backup.sh "$scratch_dir/olp-previous-backups")
./scripts/backup-manifest.sh validate "$previous_backup" v1 >/dev/null

doctor_root=$(mktemp -d)
mkdir -p "$doctor_root/console" "$doctor_root/media"
printf '<!doctype html><html></html>\n' >"$doctor_root/console/index.html"
openssl rand -base64 32 >"$doctor_root/master-key"
openssl rand -base64 32 >"$doctor_root/auth-hmac-key"
chmod 600 "$doctor_root/master-key" "$doctor_root/auth-hmac-key"
OLP_REHEARSAL_DATABASE_URL="$OLP_REHEARSAL_SERVER_URL/olp_upgrade" \
  OLP_REHEARSAL_CONFIRM=destroy-target \
  OLP_REHEARSAL_PREVIOUS_RELEASED_SCHEMA_MIGRATION="$previous_migration" \
  OLP_REHEARSAL_RUN_DOCTOR=true \
  OLP_MASTER_KEY_FILE="$doctor_root/master-key" \
  OLP_AUTH_HMAC_KEY_FILE="$doctor_root/auth-hmac-key" \
  OLP_CONSOLE_DIR="$doctor_root/console" \
  OLP_MEDIA_SPOOL_DIR="$doctor_root/media" \
  ./scripts/upgrade-rehearsal.sh "$previous_backup"
