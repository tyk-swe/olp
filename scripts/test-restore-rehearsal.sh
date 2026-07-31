#!/usr/bin/env bash
set -euo pipefail

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
touch "$work/backup.dump" "$work/psql.log" "$work/pg_restore.log"

assert_output() {
  local expected=$1
  if ! grep -Fq -- "$expected" "$work/output"; then
    printf 'expected output containing: %s\nactual output:\n' "$expected" >&2
    sed 's/^/  /' "$work/output" >&2
    exit 1
  fi
}

cat > "$work/psql" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "$OLP_TEST_PSQL_LOG"
if [[ $* == *pg_db_role_setting* ]]; then
  printf '%s\n' "$OLP_TEST_DATABASE_SENTINEL"
elif [[ $* == *pg_control_system* ]]; then
  if [[ $1 == *isolated.example* ]]; then
    printf '["cluster-2", "32768", "olp_rehearsal"]\n'
  else
    printf '["cluster-1", "16384", "olp"]\n'
  fi
elif [[ $* == *'FROM pg_class'* ]]; then
  printf '1\n'
elif [[ $* == *'FROM pg_namespace'* ]]; then
  printf '0\n'
elif [[ $* == *'WHERE NOT success'* ]]; then
  printf '0\n'
elif [[ $* == *'FROM _sqlx_migrations WHERE success'* ]]; then
  printf '34\n'
elif [[ $* == *'count(*) FROM runtime_generations'* ]]; then
  printf '1\n'
elif [[ $* == *'count(*) FROM installation'* ]]; then
  printf '1\n'
elif [[ $* == *'max(sequence)'* ]]; then
  printf '1\n'
elif [[ $* == *'DROP SCHEMA public CASCADE'* ]]; then
  :
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
./scripts/backup-manifest.sh create-v2 "$work/backup.dump" \
  2026-01-01T00:00:00Z 18.1 34 1 true true 2026-01-01T00:00:00Z

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
assert_output 'refusing to restore without the destination database isolation sentinel'
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
assert_output 'refusing to restore without the destination database isolation sentinel'
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

assert_output 'refusing to restore over the connected production database'
if grep -Fq 'DROP SCHEMA' "$work/psql.log" || [[ -s $work/pg_restore.log ]]; then
  echo "restore safety failure reached destructive work" >&2
  exit 1
fi

: >"$work/psql.log"
: >"$work/pg_restore.log"
if ! OLP_DATABASE_URL=postgres://production.example/olp \
  OLP_RESTORE_DATABASE_URL=postgres://isolated.example/olp_rehearsal \
  OLP_PSQL="$work/psql" \
  OLP_PG_RESTORE="$work/pg_restore" \
  OLP_TEST_PSQL_LOG="$work/psql.log" \
  OLP_TEST_PG_RESTORE_LOG="$work/pg_restore.log" \
  OLP_TEST_DATABASE_SENTINEL=t \
  ./scripts/restore-rehearsal.sh "$work/backup.dump" --replace \
  >"$work/output" 2>&1; then
  echo "restore rejected a valid isolated destination" >&2
  sed 's/^/  /' "$work/output" >&2
  exit 1
fi
grep -Fq 'DROP SCHEMA public CASCADE' "$work/psql.log" || {
  echo "successful replacement did not drop the destination schema" >&2
  exit 1
}
[[ -s $work/pg_restore.log ]] || {
  echo "successful replacement did not invoke pg_restore" >&2
  exit 1
}

echo "restore rehearsal safety tests passed"
