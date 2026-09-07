#!/usr/bin/env bash
set -euo pipefail
: "${OLP_DATABASE_URL:?set OLP_DATABASE_URL}"
: "${OLP_BACKUP_TRAFFIC_QUIESCED:?stop new inference, drain accounting, and set OLP_BACKUP_TRAFFIC_QUIESCED=true}"
[[ $OLP_BACKUP_TRAFFIC_QUIESCED == true ]] || exit 2
umask 077
output=${1:-backups}
mkdir -p "$output"
partial=$(mktemp -d "$output/.olp3-backup-XXXXXX")
backup="$partial/olp3-$(date -u +%Y%m%dT%H%M%SZ).dump"
snapshot_pid=
cleanup() {
  [[ -z $snapshot_pid ]] || kill "$snapshot_pid" 2>/dev/null || true
  rm -rf -- "$partial"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

coproc SNAPSHOT { psql "$OLP_DATABASE_URL" -X -qAt -F '|' -v ON_ERROR_STOP=1; }
snapshot_pid=$SNAPSHOT_PID
read_fd=${SNAPSHOT[0]}
write_fd=${SNAPSHOT[1]}
printf '%s\n' \
  'BEGIN ISOLATION LEVEL SERIALIZABLE READ ONLY DEFERRABLE;' \
  "SELECT pg_export_snapshot(), current_setting('server_version'), (SELECT count(*) FROM olp_v3._sqlx_migrations WHERE success), (SELECT COALESCE(max(sequence), 0) FROM olp_v3.runtime_generations), (SELECT id FROM olp_v3.installation_identity WHERE singleton);" \
  "SELECT COALESCE((SELECT pending_events = 0 AND lag_events = 0 AND checked_at >= clock_timestamp() - interval '30 seconds' FROM olp_v3.request_metadata_consumer_health WHERE singleton), false);" \
  >&"$write_fd"
IFS='|' read -r -u "$read_fd" snapshot server migrations generation installation
IFS= read -r -u "$read_fd" drained
[[ $drained == t ]] || { echo 'A fresh, drained accounting checkpoint is required.' >&2; exit 1; }
pg_dump "$OLP_DATABASE_URL" --format=custom --compress=zstd --schema=olp_v3 \
  --no-owner --no-privileges --snapshot="$snapshot" --file="$backup"
printf '%s\n' 'COMMIT;' >&"$write_fd"
exec {write_fd}>&-
wait "$snapshot_pid"
snapshot_pid=
checksum=$(sha256sum < "$backup")
checksum=${checksum%% *}
jq -n --arg checksum "$checksum" --arg server "$server" --arg installation "$installation" \
  --argjson migrations "$migrations" --argjson generation "$generation" \
  '{format: "olp3", schema: "olp_v3", checksum: $checksum, postgres: $server, installation: $installation, migrations: $migrations, generation: $generation, accounting_drained: true}' \
  > "$backup.manifest.json"
final="$output/$(basename "$backup")"
[[ ! -e $final && ! -e $final.manifest.json ]] || { echo 'Backup destination already exists.' >&2; exit 1; }
mv -- "$backup" "$backup.manifest.json" "$output/"
printf '%s\n' "$final"
