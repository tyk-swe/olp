#!/usr/bin/env bash
set -euo pipefail
: "${OLP_DATABASE_URL:?set OLP_DATABASE_URL}"
: "${OLP_BACKUP_TRAFFIC_QUIESCED:?stop new inference, drain accounting, and set OLP_BACKUP_TRAFFIC_QUIESCED=true}"
[[ $OLP_BACKUP_TRAFFIC_QUIESCED == true ]] || exit 2
deadline=${OLP_BACKUP_TIMEOUT_SECONDS:-600}
[[ $deadline =~ ^[1-9][0-9]*$ ]] || { echo 'Backup timeout must be a positive number of seconds.' >&2; exit 2; }
umask 077
output=${1:-backups}
mkdir -p "$output"
partial=$(mktemp -d "$output/.olp3-backup-XXXXXX")
backup="$partial/olp3-$(date -u +%Y%m%dT%H%M%SZ).dump"
snapshot_pid=
cleanup() {
  if [[ -n $snapshot_pid ]]; then
    kill "$snapshot_pid" 2>/dev/null || true
    wait "$snapshot_pid" 2>/dev/null || true
  fi
  rm -rf -- "$partial"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

coproc SNAPSHOT { PGOPTIONS="${PGOPTIONS:-} -c statement_timeout=${deadline}s -c idle_in_transaction_session_timeout=${deadline}s" exec psql "$OLP_DATABASE_URL" -X -qAt -F '|' -v ON_ERROR_STOP=1; }
snapshot_pid=$SNAPSHOT_PID
read_fd=${SNAPSHOT[0]}
write_fd=${SNAPSHOT[1]}
printf '%s\n' \
  'BEGIN ISOLATION LEVEL SERIALIZABLE READ ONLY DEFERRABLE;' \
  "SELECT pg_export_snapshot(), current_setting('server_version'), (SELECT count(*) FROM olp_v3._sqlx_migrations WHERE success), (SELECT COALESCE(max(sequence), 0) FROM olp_v3.runtime_generations), (SELECT id FROM olp_v3.installation_identity WHERE singleton);" \
  "SELECT COALESCE((SELECT pending_events = 0 AND lag_events = 0 AND checked_at >= clock_timestamp() - interval '30 seconds' FROM olp_v3.request_metadata_consumer_health WHERE singleton), false);" \
  "SELECT json_agg(json_build_object('version', version, 'description', description, 'checksum', encode(checksum, 'hex')) ORDER BY version) FROM olp_v3._sqlx_migrations WHERE success;" \
  >&"$write_fd"
IFS='|' read -r -t "$deadline" -u "$read_fd" snapshot server migrations generation installation
IFS= read -r -t "$deadline" -u "$read_fd" drained
IFS= read -r -t "$deadline" -u "$read_fd" migration_history
[[ $drained == t ]] || { echo 'A fresh, drained accounting checkpoint is required.' >&2; exit 1; }
timeout --kill-after=5s "${deadline}s" pg_dump "$OLP_DATABASE_URL" --format=custom --compress=zstd --schema=olp_v3 \
  --no-owner --no-privileges --snapshot="$snapshot" --file="$backup"
printf '%s\n' 'COMMIT;' >&"$write_fd"
exec {write_fd}>&-
wait "$snapshot_pid"
snapshot_pid=
checksum=$(sha256sum < "$backup")
checksum=${checksum%% *}
jq -n --arg checksum "$checksum" --arg server "$server" --arg installation "$installation" \
  --argjson migrations "$migrations" --argjson generation "$generation" \
  --argjson migration_history "$migration_history" \
  --arg application_version "${OLP_BACKUP_APP_VERSION:-$(sed -n 's/^version = "\([^"]*\)"/\1/p' "$(dirname "$0")/../Cargo.toml" | head -1)}" \
  --arg image_digest "${OLP_BACKUP_IMAGE_DIGEST:-}" \
  '{format: "olp3", schema: "olp_v3", checksum: $checksum, postgres: $server, installation: $installation, migrations: $migrations, generation: $generation, accounting_drained: true, migration_history: $migration_history, application_version: $application_version, image_digest: $image_digest}' \
  > "$backup.manifest.json"
final_dir="$output/$(basename "$backup" .dump)-${partial##*-}"
[[ ! -e $final_dir ]] || { echo 'Backup destination already exists.' >&2; exit 1; }
sync -f "$backup"
sync -f "$backup.manifest.json"
mv -T -- "$partial" "$final_dir"
sync -f "$output"
printf '%s\n' "$final_dir/$(basename "$backup")"
