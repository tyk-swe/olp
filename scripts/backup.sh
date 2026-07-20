#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat >&2 <<'EOF'
usage: OLP_DATABASE_URL=postgres://... scripts/backup.sh [output-directory]

For a production-consistent backup, stop new inference first and set
OLP_BACKUP_TRAFFIC_QUIESCED=true. The command then fails unless the durable
request metadata consumer checkpoint is fresh and has zero pending/lag events.
EOF
}

if [[ ${1:-} == "--help" || ${1:-} == "-h" ]]; then
  usage
  exit 0
fi

: "${OLP_DATABASE_URL:?OLP_DATABASE_URL must identify the PostgreSQL authority to back up}"
pg_dump_command=${OLP_PG_DUMP:-pg_dump}
psql_command=${OLP_PSQL:-psql}
for command in "$pg_dump_command" "$psql_command" sha256sum; do
  command -v "$command" >/dev/null || {
    echo "required command is unavailable: $command" >&2
    exit 1
  }
done

output_dir=${1:-./backups}
timestamp=$(date -u +%Y%m%dT%H%M%SZ)
created_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
backup_name="olp-${timestamp}.dump"
backup_path="${output_dir}/${backup_name}"
temporary_path="${backup_path}.partial"

umask 077
mkdir -p "$output_dir"
if [[ -e $backup_path || -e ${backup_path}.sha256 || -e ${backup_path}.manifest.json ]]; then
  echo "refusing to overwrite an existing backup: $backup_path" >&2
  exit 1
fi
trap 'rm -f "$temporary_path"' EXIT

server_version=$("$psql_command" "$OLP_DATABASE_URL" -X --no-psqlrc --tuples-only --no-align \
  --command='SHOW server_version' | tr -d '[:space:]')
migration_count=$("$psql_command" "$OLP_DATABASE_URL" -X --no-psqlrc --tuples-only --no-align \
  --command='SELECT count(*) FROM _sqlx_migrations WHERE success' | tr -d '[:space:]')
latest_generation=$("$psql_command" "$OLP_DATABASE_URL" -X --no-psqlrc --tuples-only --no-align \
  --command='SELECT COALESCE(max(sequence), 0) FROM runtime_generations' | tr -d '[:space:]')
request_metadata_schema=$("$psql_command" "$OLP_DATABASE_URL" -X --no-psqlrc \
  --tuples-only --no-align --command="
    SELECT CASE
      WHEN to_regclass('public.request_metadata_consumer_health') IS NOT NULL THEN 'current'
      WHEN to_regclass('public.usage_consumer_health') IS NOT NULL THEN 'legacy'
      ELSE 'missing'
    END
  " | tr -d '[:space:]')
[[ $request_metadata_schema == current || $request_metadata_schema == legacy ]] || {
  echo "request metadata consumer health schema is unavailable" >&2
  exit 1
}

traffic_quiesced=${OLP_BACKUP_TRAFFIC_QUIESCED:-false}
case "$traffic_quiesced" in
  true | false) ;;
  *) echo "OLP_BACKUP_TRAFFIC_QUIESCED must be true or false" >&2; exit 2 ;;
esac
request_metadata_stream_drained=false
request_metadata_consumer_checked_at_json=null
if [[ $traffic_quiesced == true ]]; then
  max_age=${OLP_BACKUP_REQUEST_METADATA_CHECKPOINT_MAX_AGE_SECONDS:-30}
  [[ $max_age =~ ^[1-9][0-9]*$ ]] || {
    echo "OLP_BACKUP_REQUEST_METADATA_CHECKPOINT_MAX_AGE_SECONDS must be a positive integer" >&2
    exit 2
  }
  if [[ $request_metadata_schema == current ]]; then
    consumer_health_table=request_metadata_consumer_health
  else
    consumer_health_table=usage_consumer_health
  fi
  checkpoint=$("$psql_command" "$OLP_DATABASE_URL" -X --no-psqlrc \
    --tuples-only --no-align --field-separator='|' --command="
      SELECT pending_events,
             lag_events,
             to_char(checked_at AT TIME ZONE 'UTC', 'YYYY-MM-DD\"T\"HH24:MI:SS\"Z\"'),
             greatest(0, floor(extract(epoch FROM now() - checked_at)))::bigint
        FROM ${consumer_health_table}
       WHERE singleton
    " | tr -d '[:space:]')
  [[ -n $checkpoint ]] || {
    echo "no durable request metadata consumer checkpoint exists; start the worker and wait for a checkpoint" >&2
    exit 1
  }
  IFS='|' read -r pending lag checked_at checkpoint_age <<<"$checkpoint"
  [[ $pending =~ ^[0-9]+$ && $lag =~ ^[0-9]+$ && $checkpoint_age =~ ^[0-9]+$ ]] || {
    echo "request metadata consumer checkpoint is malformed" >&2
    exit 1
  }
  [[ $pending == 0 && $lag == 0 ]] || {
    echo "request metadata stream is not drained: pending=$pending lag=$lag" >&2
    exit 1
  }
  (( checkpoint_age <= max_age )) || {
    echo "request metadata consumer checkpoint is stale: age=${checkpoint_age}s maximum=${max_age}s" >&2
    exit 1
  }
  [[ $checked_at =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$ ]] || {
    echo "request metadata consumer checkpoint timestamp is malformed" >&2
    exit 1
  }
  request_metadata_stream_drained=true
  request_metadata_consumer_checked_at_json="\"$checked_at\""
else
  echo "warning: backup was not declared traffic-quiesced; manifest will mark request_metadata_stream_drained=false" >&2
fi

"$pg_dump_command" "$OLP_DATABASE_URL" \
  --format=custom \
  --compress=9 \
  --no-owner \
  --no-privileges \
  --serializable-deferrable \
  --file="$temporary_path"

mv "$temporary_path" "$backup_path"
checksum=$(sha256sum "$backup_path" | awk '{print $1}')
printf '%s  %s\n' "$checksum" "$backup_name" > "${backup_path}.sha256"
if [[ $request_metadata_schema == current ]]; then
  printf '{\n  "format": "olp-v2-postgresql-custom-v2",\n  "created_at": "%s",\n  "database_server_version": "%s",\n  "successful_migrations": %s,\n  "runtime_generation_ordinal": %s,\n  "backup_file": "%s",\n  "sha256": "%s",\n  "traffic_quiesced": %s,\n  "request_metadata_stream_drained": %s,\n  "request_metadata_consumer_checked_at": %s,\n  "plaintext_secrets_included": false,\n  "encrypted_sensitive_records_included": true,\n  "mounted_key_files_included": false\n}\n' \
    "$created_at" "$server_version" "$migration_count" "$latest_generation" \
    "$backup_name" "$checksum" "$traffic_quiesced" "$request_metadata_stream_drained" \
    "$request_metadata_consumer_checked_at_json" > "${backup_path}.manifest.json"
else
  printf '{\n  "format": "olp-v2-postgresql-custom-v1",\n  "created_at": "%s",\n  "database_server_version": "%s",\n  "successful_migrations": %s,\n  "runtime_generation_ordinal": %s,\n  "backup_file": "%s",\n  "sha256": "%s",\n  "traffic_quiesced": %s,\n  "usage_stream_drained": %s,\n  "usage_consumer_checked_at": %s,\n  "plaintext_secrets_included": false,\n  "encrypted_sensitive_records_included": true,\n  "mounted_key_files_included": false\n}\n' \
    "$created_at" "$server_version" "$migration_count" "$latest_generation" \
    "$backup_name" "$checksum" "$traffic_quiesced" "$request_metadata_stream_drained" \
    "$request_metadata_consumer_checked_at_json" > "${backup_path}.manifest.json"
fi

echo "$backup_path"
