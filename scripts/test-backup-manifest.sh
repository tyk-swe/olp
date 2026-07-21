#!/usr/bin/env bash
set -euo pipefail

root_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
manifest_tool="$root_dir/scripts/backup-manifest.sh"
test_dir=$(mktemp -d)
trap 'rm -rf "$test_dir"' EXIT

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

echo "backup manifest contract tests passed"
