#!/usr/bin/env bash
set -euo pipefail

root_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
manifest_tool="$root_dir/scripts/backup-manifest.sh"
test_dir=$(mktemp -d)
active_backup_pid=
cleanup() {
  if [[ -n $active_backup_pid ]]; then
    kill "$active_backup_pid" 2>/dev/null || true
    wait "$active_backup_pid" 2>/dev/null || true
  fi
  rm -rf "$test_dir"
}
trap cleanup EXIT
shopt -s nullglob

backup="$test_dir/olp-fixture.dump"
manifest="${backup}.manifest.json"
valid_manifest="$test_dir/valid-v2.json"
printf 'deterministic backup fixture\n' > "$backup"

expect_invalid() {
  local label=$1
  if "$manifest_tool" validate "$backup" >/dev/null 2>&1; then
    echo "expected invalid backup manifest: $label" >&2
    exit 1
  fi
}

mutate_manifest() {
  local label=$1 filter=$2
  jq "$filter" "$valid_manifest" > "$manifest"
  expect_invalid "$label"
}

prepare_backup_commands() {
  local fake_bin="$test_dir/fake-bin"
  mkdir -p "$fake_bin"
  cat > "$fake_bin/psql" <<'FAKE_PSQL'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$$" > "$FAKE_CASE_DIR/psql.pid"
while IFS= read -r sql; do
  case "$sql" in
    *pg_export_snapshot*)
      printf '00000001-00000002-1|%s|42|7|%s\n' "${FAKE_SERVER_VERSION:-18.1}" "${FAKE_SCHEMA:-current}"
      ;;
    *'consumer_health'*)
      printf '%s\n' '0|0|2026-07-21T11:59:59Z|1'
      ;;
    'COMMIT;')
      : > "$FAKE_CASE_DIR/committed"
      exit 0
      ;;
  esac
done
FAKE_PSQL
  cat > "$fake_bin/pg_dump" <<'FAKE_PG_DUMP'
#!/usr/bin/env bash
set -euo pipefail
snapshot=
output=
for argument in "$@"; do
  case "$argument" in
    --snapshot=*) snapshot=${argument#*=} ;;
    --file=*) output=${argument#*=} ;;
    --serializable-deferrable) exit 9 ;;
  esac
done
[[ $snapshot == 00000001-00000002-1 && -n $output ]]
snapshot_pid=$(<"$FAKE_CASE_DIR/psql.pid")
kill -0 "$snapshot_pid"
[[ ! -e $FAKE_CASE_DIR/committed ]]
printf '%s\n' "$$" > "$FAKE_CASE_DIR/dump.pid"
printf 'dump from exported snapshot\n' > "$output"
case "${FAKE_DUMP_MODE:-success}" in
  fail) exit 7 ;;
  hold)
    printf 'ready\n' > "$FAKE_CASE_DIR/ready"
    read -r release < "$FAKE_CASE_DIR/release"
    [[ $release == continue ]]
    ;;
esac
kill -0 "$snapshot_pid"
[[ ! -e $FAKE_CASE_DIR/committed ]]
FAKE_PG_DUMP
  cat > "$fake_bin/date" <<'FAKE_DATE'
#!/usr/bin/env bash
case "$*" in
  '-u +%Y%m%dT%H%M%SZ') printf '20260721T120000Z\n' ;;
  '-u +%Y-%m-%dT%H:%M:%SZ') printf '2026-07-21T12:00:00Z\n' ;;
  *) exit 9 ;;
esac
FAKE_DATE
  cat > "$fake_bin/mv" <<'FAKE_MV'
#!/usr/bin/env bash
set -euo pipefail
if [[ ${FAKE_PUBLISH_FAILURE:-false} == true && $# == 5 ]]; then
  "$FAKE_REAL_MV" -- "$2" "${!#}"
  exit 8
fi
exec "$FAKE_REAL_MV" "$@"
FAKE_MV
  chmod +x "$fake_bin/psql" "$fake_bin/pg_dump" "$fake_bin/date" "$fake_bin/mv"
  backup_command=(env PATH="$fake_bin:$PATH" FAKE_REAL_MV="$(command -v mv)"
    OLP_DATABASE_URL=postgres://fixture OLP_PSQL=psql OLP_PG_DUMP=pg_dump
    OLP_BACKUP_TRAFFIC_QUIESCED=true "$root_dir/scripts/backup.sh")
}

assert_backup_bundle() {
  local generated_backup=$1 version=${2:-v2} output
  [[ $("$manifest_tool" validate "$generated_backup" "$version") == $'42\t7' ]]
  for output in "$generated_backup" "${generated_backup}.sha256" "${generated_backup}.manifest.json"; do
    [[ $(stat -c '%a' "$output") == 600 ]]
  done
  [[ ! -e ${generated_backup}.partial ]]
}

assert_no_backup_outputs() {
  local outputs=("$1"/*)
  [[ ${#outputs[@]} == 0 ]]
}

test_backup_uses_exported_snapshot() {
  local schema fixture_dir backup_dir generated_backup version
  for schema in current legacy; do
    fixture_dir="$test_dir/snapshot-$schema"
    backup_dir="$fixture_dir/output"
    mkdir -p "$fixture_dir"
    generated_backup=$(FAKE_CASE_DIR="$fixture_dir" FAKE_SCHEMA="$schema" \
      "${backup_command[@]}" "$backup_dir")
    [[ -f $fixture_dir/committed ]]
    [[ $generated_backup == "$backup_dir/olp-20260721T120000Z.dump" ]]
    version=v2
    if [[ $schema == legacy ]]; then version=v1; fi
    assert_backup_bundle "$generated_backup" "$version"
  done
}

start_held_backup() {
  local fixture_dir=$1 backup_dir=$2
  mkdir -p "$fixture_dir"
  mkfifo "$fixture_dir/ready" "$fixture_dir/release"
  exec {ready_fd}<>"$fixture_dir/ready"
  exec {release_fd}<>"$fixture_dir/release"
  FAKE_CASE_DIR="$fixture_dir" FAKE_DUMP_MODE=hold \
    "${backup_command[@]}" "$backup_dir" > "$fixture_dir/result" 2> "$fixture_dir/error" &
  active_backup_pid=$!
  local ready
  read -r -t 10 -u "$ready_fd" ready
  [[ $ready == ready ]]
}

test_concurrent_backups_keep_the_first_reservation() {
  local fixture_dir="$test_dir/concurrent" backup_dir="$test_dir/concurrent/output"
  local first_backup="$backup_dir/olp-20260721T120000Z.dump"
  start_held_backup "$fixture_dir" "$backup_dir"
  local snapshot_pid
  snapshot_pid=$(<"$fixture_dir/psql.pid")
  [[ $(stat -c '%a' "${first_backup}.partial") == 700 ]]
  cp "${first_backup}.partial/${first_backup##*/}" "$fixture_dir/expected-dump"
  mkdir "$fixture_dir/second"
  if FAKE_CASE_DIR="$fixture_dir/second" "${backup_command[@]}" "$backup_dir" \
    > "$fixture_dir/second/result" 2> "$fixture_dir/second/error"; then
    echo "overlapping backup reused a reserved namespace" >&2
    exit 1
  fi
  [[ ! -e $fixture_dir/second/psql.pid && ! -e $fixture_dir/second/dump.pid ]]
  cmp "$fixture_dir/expected-dump" "${first_backup}.partial/${first_backup##*/}"
  kill -0 "$snapshot_pid"
  printf 'continue\n' >&"$release_fd"
  wait "$active_backup_pid"
  active_backup_pid=
  exec {ready_fd}>&-
  exec {release_fd}>&-
  [[ $(<"$fixture_dir/result") == "$first_backup" && -f $fixture_dir/committed ]]
  assert_backup_bundle "$first_backup"
}

test_failed_backups_remove_owned_outputs() {
  local failure fixture_dir backup_dir
  for failure in dump manifest publication; do
    fixture_dir="$test_dir/failure-$failure"
    backup_dir="$fixture_dir/output"
    mkdir -p "$fixture_dir"
    local dump_mode=success server_version=18.1 publish_failure=false
    case "$failure" in
      dump) dump_mode=fail ;;
      manifest) server_version=17.0 ;;
      publication) publish_failure=true ;;
    esac
    if FAKE_CASE_DIR="$fixture_dir" FAKE_DUMP_MODE="$dump_mode" \
      FAKE_SERVER_VERSION="$server_version" FAKE_PUBLISH_FAILURE="$publish_failure" \
      "${backup_command[@]}" "$backup_dir" > "$fixture_dir/result" 2> "$fixture_dir/error"; then
      echo "expected $failure backup failure" >&2
      exit 1
    fi
    [[ -f $fixture_dir/dump.pid && ! -s $fixture_dir/result ]]
    assert_no_backup_outputs "$backup_dir"
    if kill -0 "$(<"$fixture_dir/psql.pid")" 2>/dev/null; then
      echo "$failure backup left its snapshot session running" >&2
      exit 1
    fi
  done
}

test_interrupted_backup_removes_owned_outputs() {
  local fixture_dir="$test_dir/interrupted" backup_dir="$test_dir/interrupted/output"
  start_held_backup "$fixture_dir" "$backup_dir"
  local dump_pid snapshot_pid status=0
  dump_pid=$(<"$fixture_dir/dump.pid")
  snapshot_pid=$(<"$fixture_dir/psql.pid")
  kill -TERM "$active_backup_pid"
  wait "$active_backup_pid" || status=$?
  active_backup_pid=
  exec {ready_fd}>&-
  exec {release_fd}>&-
  [[ $status == 143 && ! -e $fixture_dir/committed && ! -s $fixture_dir/result ]]
  assert_no_backup_outputs "$backup_dir"
  if kill -0 "$dump_pid" 2>/dev/null || kill -0 "$snapshot_pid" 2>/dev/null; then
    echo "interrupted backup left a child running" >&2
    exit 1
  fi
}

test_existing_backup_paths_are_untouched() {
  local suffix kind fixture_dir backup_dir existing index=0
  for suffix in '' .sha256 .manifest.json .partial; do
    for kind in file symlink dangling; do
      fixture_dir="$test_dir/existing-$index"
      backup_dir="$fixture_dir/output"
      existing="$backup_dir/olp-20260721T120000Z.dump$suffix"
      mkdir -p "$backup_dir"
      printf 'existing backup data\n' > "$fixture_dir/sentinel"
      case "$kind" in
        file) cp "$fixture_dir/sentinel" "$existing" ;;
        symlink) ln -s "$fixture_dir/sentinel" "$existing" ;;
        dangling) ln -s "$fixture_dir/missing" "$existing" ;;
      esac
      if FAKE_CASE_DIR="$fixture_dir" "${backup_command[@]}" "$backup_dir" \
        > "$fixture_dir/result" 2> "$fixture_dir/error"; then
        echo "backup overwrote existing $kind $suffix" >&2
        exit 1
      fi
      [[ ! -e $fixture_dir/psql.pid && ! -e $fixture_dir/dump.pid ]]
      if [[ $kind == dangling ]]; then
        [[ -L $existing && ! -e $fixture_dir/missing ]]
        [[ $(readlink "$existing") == "$fixture_dir/missing" ]]
      else
        cmp "$fixture_dir/sentinel" "$existing"
        [[ $(<"$fixture_dir/sentinel") == 'existing backup data' ]]
        if [[ $kind == symlink ]]; then [[ -L $existing ]]; fi
      fi
      local outputs=("$backup_dir"/*)
      [[ ${#outputs[@]} == 1 ]]
      index=$((index + 1))
    done
  done
}

"$manifest_tool" create-v2 "$backup" \
  2026-07-21T12:00:00Z 18.1 42 7 true true 2026-07-21T11:59:59Z
[[ $("$manifest_tool" validate "$backup" v2) == $'42\t7' ]]
cp "$manifest" "$valid_manifest"

mutate_manifest "manifest checksum" '.sha256 = ("0" * 64)'
mutate_manifest "backup filename" '.backup_file = "another.dump"'
mutate_manifest "extra field" '.unexpected = true'
mutate_manifest "fractional migration count" '.successful_migrations = 42.5'
mutate_manifest "unsupported format" '.format = "olp-v2-postgresql-custom-v3"'
mutate_manifest "drained without quiescence" '.traffic_quiesced = false'
mutate_manifest "missing drained timestamp" '.request_metadata_consumer_checked_at = null'
mutate_manifest "timestamp without drain" \
  '.traffic_quiesced = false | .request_metadata_stream_drained = false'
mutate_manifest "checkpoint after creation" \
  '.request_metadata_consumer_checked_at = "2026-07-21T12:00:01Z"'

jq '
  .traffic_quiesced = false
  | .request_metadata_stream_drained = false
  | .request_metadata_consumer_checked_at = null
' "$valid_manifest" > "$manifest"
[[ $("$manifest_tool" validate "$backup" v2) == $'42\t7' ]]

cp "$valid_manifest" "$manifest"
printf '0%.0s' {1..64} > "${backup}.sha256"
printf '  %s\n' "${backup##*/}" >> "${backup}.sha256"
expect_invalid "checksum sidecar checksum"

checksum=$(sha256sum "$backup")
checksum=${checksum%% *}
printf '%s  %s\n' "$checksum" another.dump > "${backup}.sha256"
expect_invalid "checksum sidecar filename"

printf '%s  %s\n' "$checksum" "${backup##*/}" > "${backup}.sha256"
printf 'tampered\n' >> "$backup"
expect_invalid "backup contents"

printf 'deterministic backup fixture\n' > "$backup"
"$manifest_tool" convert-v2-to-v1 "$backup"
[[ $("$manifest_tool" validate "$backup" v1) == $'42\t7' ]]
if "$manifest_tool" validate "$backup" v2 >/dev/null 2>&1; then
  echo "legacy fixture unexpectedly validated as v2" >&2
  exit 1
fi
cp "$manifest" "$test_dir/valid-v1.json"
jq '.usage_stream_drained = false' "$test_dir/valid-v1.json" > "$manifest"
expect_invalid "legacy drained/quiesced relationship"

jq '.usage_consumer_checked_at = "2026-07-21T12:00:01Z"' \
  "$test_dir/valid-v1.json" > "$test_dir/historical-v1.json"
cp "$test_dir/historical-v1.json" "$manifest"
[[ $("$manifest_tool" validate "$backup" v1) == $'42\t7' ]]
jq '.usage_consumer_checked_at = "not-a-timestamp"' \
  "$test_dir/historical-v1.json" > "$manifest"
expect_invalid "legacy malformed checkpoint timestamp"

prepare_backup_commands
test_backup_uses_exported_snapshot
test_concurrent_backups_keep_the_first_reservation
test_failed_backups_remove_owned_outputs
test_interrupted_backup_removes_owned_outputs
test_existing_backup_paths_are_untouched

echo "backup manifest contract tests passed"
