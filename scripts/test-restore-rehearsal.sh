#!/usr/bin/env bash
set -euo pipefail

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
touch "$work/backup.dump" "$work/psql.log" "$work/pg_restore.log"

cat > "$work/psql" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "$OLP_TEST_PSQL_LOG"
if [[ $* == *pg_db_role_setting* ]]; then
  printf '%s\n' "$OLP_TEST_DATABASE_SENTINEL"
elif [[ $* == *pg_control_system* ]]; then
  printf '["cluster-1", "16384", "olp"]\n'
else
  echo "unexpected psql call" >&2
  exit 1
fi
EOF
cat > "$work/pg_restore" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "$OLP_TEST_PG_RESTORE_LOG"
EOF
chmod +x "$work/psql" "$work/pg_restore"

if OLP_DATABASE_URL=postgres://production.example/olp \
  OLP_RESTORE_DATABASE_URL=postgres://alias.example/olp \
  OLP_PSQL="$work/psql" \
  OLP_PG_RESTORE="$work/pg_restore" \
  OLP_TEST_PSQL_LOG="$work/psql.log" \
  OLP_TEST_PG_RESTORE_LOG="$work/pg_restore.log" \
  OLP_TEST_DATABASE_SENTINEL=f \
  ./scripts/restore-rehearsal.sh "$work/backup.dump" --replace \
  >"$work/output" 2>&1; then
  echo "restore accepted a destination without the isolation sentinel" >&2
  exit 1
fi
grep -Fq 'refusing to restore without the destination database isolation sentinel' "$work/output"
if grep -Fq 'pg_control_system' "$work/psql.log"; then
  echo "restore queried identities before validating the isolation sentinel" >&2
  exit 1
fi

: >"$work/psql.log"
if PGOPTIONS='-c olp.restore_rehearsal=isolated-destination' \
  OLP_DATABASE_URL=postgres://production.example/olp \
  OLP_RESTORE_DATABASE_URL=postgres://isolated.example/olp \
  OLP_PSQL="$work/psql" \
  OLP_PG_RESTORE="$work/pg_restore" \
  OLP_TEST_PSQL_LOG="$work/psql.log" \
  OLP_TEST_PG_RESTORE_LOG="$work/pg_restore.log" \
  OLP_TEST_DATABASE_SENTINEL=f \
  ./scripts/restore-rehearsal.sh "$work/backup.dump" --replace \
  >"$work/output" 2>&1; then
  echo "restore accepted a session-only isolation sentinel" >&2
  exit 1
fi
grep -Fq 'refusing to restore without the destination database isolation sentinel' "$work/output"
if grep -Fq 'pg_control_system' "$work/psql.log"; then
  echo "restore queried identities after a session-only sentinel spoof" >&2
  exit 1
fi

: >"$work/psql.log"
if OLP_DATABASE_URL=postgres://production.example/olp \
  OLP_RESTORE_DATABASE_URL=postgres://alias.example/olp \
  OLP_PSQL="$work/psql" \
  OLP_PG_RESTORE="$work/pg_restore" \
  OLP_TEST_PSQL_LOG="$work/psql.log" \
  OLP_TEST_PG_RESTORE_LOG="$work/pg_restore.log" \
  OLP_TEST_DATABASE_SENTINEL=t \
  ./scripts/restore-rehearsal.sh "$work/backup.dump" --replace \
  >"$work/output" 2>&1; then
  echo "restore accepted aliased URLs for the connected production database" >&2
  exit 1
fi

grep -Fq 'refusing to restore over the connected production database' "$work/output"
if grep -Fq 'DROP SCHEMA' "$work/psql.log" || [[ -s $work/pg_restore.log ]]; then
  echo "restore safety failure reached destructive work" >&2
  exit 1
fi

echo "restore rehearsal safety tests passed"
