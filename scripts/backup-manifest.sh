#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat >&2 <<'USAGE'
usage:
  backup-manifest.sh create-v2 BACKUP CREATED_AT SERVER_VERSION MIGRATIONS GENERATION QUIESCED DRAINED CHECKED_AT_OR_NULL
  backup-manifest.sh validate BACKUP [v1|v2]
  backup-manifest.sh convert-v2-to-v1 BACKUP

create-v2 writes BACKUP.sha256 and BACKUP.manifest.json. validate checks the
manifest, checksum sidecar, and backup as one contract and prints the expected
migration count and runtime-generation ordinal separated by a tab.
convert-v2-to-v1 rewrites a valid v2 manifest as the legacy v1 CI fixture.
USAGE
}

die() {
  echo "$1" >&2
  exit 1
}

for command in jq sha256sum; do
  command -v "$command" >/dev/null || die "required command is unavailable: $command"
done
umask 077

temporary_files=()
cleanup() {
  if (( ${#temporary_files[@]} > 0 )); then
    rm -f -- "${temporary_files[@]}"
  fi
}
trap cleanup EXIT

backup_checksum() {
  local backup=$1 checksum
  [[ -f $backup ]] || die "backup does not exist: $backup"
  checksum=$(sha256sum < "$backup")
  checksum=${checksum%% *}
  [[ $checksum =~ ^[a-f0-9]{64}$ ]] || die "could not calculate the backup checksum"
  printf '%s\n' "$checksum"
}

validate_manifest() {
  local backup=$1 checksum_file=$2 manifest_file=$3 expected_format=${4:-}
  local actual_checksum=${5:-}
  local backup_name checksum_pattern sidecar_checksum sidecar_name result
  local -a checksum_lines

  [[ -f $backup ]] || die "backup does not exist: $backup"
  [[ -f $checksum_file ]] || die "backup checksum sidecar is required: $checksum_file"
  [[ -f $manifest_file ]] || die "backup manifest is required: $manifest_file"

  if [[ -z $actual_checksum ]]; then
    actual_checksum=$(backup_checksum "$backup")
  fi

  mapfile -t checksum_lines < "$checksum_file"
  (( ${#checksum_lines[@]} == 1 )) || die "backup checksum sidecar is malformed"
  checksum_pattern='^([a-f0-9]{64})  (.+)$'
  [[ ${checksum_lines[0]} =~ $checksum_pattern ]] || die "backup checksum sidecar is malformed"
  sidecar_checksum=${BASH_REMATCH[1]}
  sidecar_name=${BASH_REMATCH[2]}
  backup_name=${backup##*/}
  [[ $sidecar_name == "$backup_name" ]] || die "backup checksum sidecar names a different file"
  [[ $sidecar_checksum == "$actual_checksum" ]] || die "backup checksum mismatch"

  if ! result=$(jq -er \
    --arg checksum "$actual_checksum" \
    --arg backup_name "$backup_name" \
    --arg expected_format "$expected_format" '
      def utc_timestamp:
        type == "string" and
        test("^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$") and
        ((try fromdateiso8601 catch null) != null);
      def nonnegative_integer:
        type == "number" and . >= 0 and floor == .;
      def exact_keys($expected):
        (keys | sort) == ($expected | sort);
      def common_contract:
        (.created_at | utc_timestamp) and
        (.database_server_version | type == "string" and startswith("18.")) and
        (.successful_migrations | nonnegative_integer) and
        (.runtime_generation_ordinal | nonnegative_integer) and
        .backup_file == $backup_name and
        .sha256 == $checksum and
        (.traffic_quiesced | type == "boolean") and
        .plaintext_secrets_included == false and
        .encrypted_sensitive_records_included == true and
        .mounted_key_files_included == false;
      def checkpoint_contract($drained; $checked_at; $enforce_chronology):
        ($drained | type == "boolean") and
        .traffic_quiesced == $drained and
        (if $drained then
           ($checked_at | utc_timestamp) and
           (if $enforce_chronology then
              ($checked_at | fromdateiso8601) <= (.created_at | fromdateiso8601)
            else
              true
            end)
         else
           $checked_at == null
         end);
      def v1_contract:
        exact_keys([
          "format", "created_at", "database_server_version",
          "successful_migrations", "runtime_generation_ordinal", "backup_file",
          "sha256", "traffic_quiesced", "usage_stream_drained",
          "usage_consumer_checked_at", "plaintext_secrets_included",
          "encrypted_sensitive_records_included", "mounted_key_files_included"
        ]) and
        checkpoint_contract(
          .usage_stream_drained;
          .usage_consumer_checked_at;
          false
        );
      def v2_contract:
        exact_keys([
          "format", "created_at", "database_server_version",
          "successful_migrations", "runtime_generation_ordinal", "backup_file",
          "sha256", "traffic_quiesced", "request_metadata_stream_drained",
          "request_metadata_consumer_checked_at", "plaintext_secrets_included",
          "encrypted_sensitive_records_included", "mounted_key_files_included"
        ]) and
        checkpoint_contract(
          .request_metadata_stream_drained;
          .request_metadata_consumer_checked_at;
          true
        );
      def manifest_contract:
        common_contract and
        (if .format == "olp-v2-postgresql-custom-v1" then
           v1_contract
         elif .format == "olp-v2-postgresql-custom-v2" then
           v2_contract
         else
           false
         end) and
        ($expected_format == "" or
         .format == "olp-v2-postgresql-custom-" + $expected_format);
      if manifest_contract then
        [.successful_migrations, .runtime_generation_ordinal] | @tsv
      else
        error("manifest contract violation")
      end
    ' "$manifest_file" 2>/dev/null); then
    die "backup manifest is invalid or inconsistent"
  fi
  printf '%s\n' "$result"
}

operation=${1:-}
case "$operation" in
  create-v2)
    [[ $# -eq 9 ]] || { usage; exit 2; }
    backup=$2
    created_at=$3
    server_version=$4
    migrations=$5
    generation=$6
    quiesced=$7
    drained=$8
    checked_at=$9
    checksum_file="${backup}.sha256"
    manifest_file="${backup}.manifest.json"
    [[ -f $backup ]] || die "backup does not exist: $backup"
    [[ ! -e $checksum_file ]] || die "refusing to overwrite an existing checksum: $checksum_file"
    [[ ! -e $manifest_file ]] || die "refusing to overwrite an existing manifest: $manifest_file"
    [[ $migrations =~ ^[0-9]+$ ]] || die "successful migration count must be a non-negative integer"
    [[ $generation =~ ^[0-9]+$ ]] || die "runtime generation ordinal must be a non-negative integer"
    [[ $quiesced == true || $quiesced == false ]] || die "quiesced must be true or false"
    [[ $drained == true || $drained == false ]] || die "drained must be true or false"

    checksum=$(backup_checksum "$backup")
    backup_name=${backup##*/}
    checksum_temporary="${checksum_file}.partial.$$"
    manifest_temporary="${manifest_file}.partial.$$"
    temporary_files+=("$checksum_temporary" "$manifest_temporary")
    printf '%s  %s\n' "$checksum" "$backup_name" > "$checksum_temporary"
    jq -n \
      --arg created_at "$created_at" \
      --arg server_version "$server_version" \
      --argjson migrations "$migrations" \
      --argjson generation "$generation" \
      --arg backup_name "$backup_name" \
      --arg checksum "$checksum" \
      --argjson quiesced "$quiesced" \
      --argjson drained "$drained" \
      --arg checked_at "$checked_at" '
        {
          format: "olp-v2-postgresql-custom-v2",
          created_at: $created_at,
          database_server_version: $server_version,
          successful_migrations: $migrations,
          runtime_generation_ordinal: $generation,
          backup_file: $backup_name,
          sha256: $checksum,
          traffic_quiesced: $quiesced,
          request_metadata_stream_drained: $drained,
          request_metadata_consumer_checked_at:
            (if $checked_at == "null" then null else $checked_at end),
          plaintext_secrets_included: false,
          encrypted_sensitive_records_included: true,
          mounted_key_files_included: false
        }
      ' > "$manifest_temporary"
    validate_manifest "$backup" "$checksum_temporary" "$manifest_temporary" v2 "$checksum" >/dev/null
    mv -- "$checksum_temporary" "$checksum_file"
    mv -- "$manifest_temporary" "$manifest_file"
    ;;
  validate)
    [[ $# -eq 2 || $# -eq 3 ]] || { usage; exit 2; }
    expected_format=${3:-}
    [[ -z $expected_format || $expected_format == v1 || $expected_format == v2 ]] || {
      die "expected format must be v1 or v2"
    }
    validate_manifest "$2" "${2}.sha256" "${2}.manifest.json" "$expected_format"
    ;;
  convert-v2-to-v1)
    [[ $# -eq 2 ]] || { usage; exit 2; }
    backup=$2
    checksum_file="${backup}.sha256"
    manifest_file="${backup}.manifest.json"
    checksum=$(backup_checksum "$backup")
    validate_manifest "$backup" "$checksum_file" "$manifest_file" v2 "$checksum" >/dev/null
    manifest_temporary="${manifest_file}.partial.$$"
    temporary_files+=("$manifest_temporary")
    jq -e '
      .format = "olp-v2-postgresql-custom-v1"
      | .usage_stream_drained = .request_metadata_stream_drained
      | .usage_consumer_checked_at = .request_metadata_consumer_checked_at
      | del(.request_metadata_stream_drained, .request_metadata_consumer_checked_at)
    ' "$manifest_file" > "$manifest_temporary"
    validate_manifest "$backup" "$checksum_file" "$manifest_temporary" v1 "$checksum" >/dev/null
    mv -- "$manifest_temporary" "$manifest_file"
    ;;
  --help|-h)
    usage
    ;;
  *)
    usage
    exit 2
    ;;
esac
