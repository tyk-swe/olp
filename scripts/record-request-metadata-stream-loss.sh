#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat >&2 <<'EOF'
usage: OLP_DATABASE_URL=postgres://... OLP_CONFIRM_REQUEST_METADATA_STREAM_LOSS=record-explicit-gap \
  scripts/record-request-metadata-stream-loss.sh INCIDENT_ID EVENT_COUNT exact|lower-bound FIRST_AT LAST_AT

Records a content-free PostgreSQL request metadata gap for an unrecoverable
Valkey Stream loss. Timestamps must be whole-second RFC 3339 UTC.
EOF
}

if [[ ${1:-} == --help || ${1:-} == -h ]]; then usage; exit 0; fi
[[ $# == 5 ]] || { usage; exit 2; }
: "${OLP_DATABASE_URL:?OLP_DATABASE_URL is required}"
: "${OLP_CONFIRM_REQUEST_METADATA_STREAM_LOSS:?set OLP_CONFIRM_REQUEST_METADATA_STREAM_LOSS=record-explicit-gap}"
[[ $OLP_CONFIRM_REQUEST_METADATA_STREAM_LOSS == record-explicit-gap ]] || {
  echo "request metadata stream loss confirmation did not match" >&2
  exit 2
}

incident_id=$1
event_count=$2
precision=$3
first_at=$4
last_at=$5
[[ $incident_id =~ ^[a-z0-9][a-z0-9._-]{0,79}$ ]] || {
  echo "INCIDENT_ID must be a lowercase content-free identifier" >&2
  exit 2
}
[[ $event_count =~ ^[1-9][0-9]*$ ]] || {
  echo "EVENT_COUNT must be a positive integer" >&2
  exit 2
}
[[ $precision == exact || $precision == lower-bound ]] || {
  echo "precision must be exact or lower-bound" >&2
  exit 2
}
timestamp='^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$'
[[ $first_at =~ $timestamp && $last_at =~ $timestamp ]] || {
  echo "FIRST_AT and LAST_AT must be whole-second RFC 3339 UTC" >&2
  exit 2
}

psql_command=${OLP_PSQL:-psql}
command -v "$psql_command" >/dev/null || { echo "psql is required" >&2; exit 1; }
reason="request_metadata_stream_loss_${precision}:${incident_id}"
certainty=${precision//-/_}
result=$("$psql_command" "$OLP_DATABASE_URL" -X --no-psqlrc --set=ON_ERROR_STOP=1 \
  --set=incident_id="$incident_id" --set=event_count="$event_count" \
  --set=first_at="$first_at" --set=last_at="$last_at" --set=reason="$reason" \
  --set=certainty="$certainty" \
  --tuples-only --no-align <<'SQL'
BEGIN;
SELECT pg_advisory_xact_lock(hashtextextended(:'reason', 0));
WITH inserted AS (
  INSERT INTO request_metadata_ingestion_gaps
    (id, gateway_instance, event_count, reason, certainty,
     first_observed_at, last_observed_at)
  SELECT uuidv7(), 'disaster-recovery', :'event_count'::bigint,
         :'reason', :'certainty'::request_metadata_gap_certainty,
         :'first_at'::timestamptz, :'last_at'::timestamptz
  WHERE NOT EXISTS (SELECT 1 FROM request_metadata_ingestion_gaps WHERE reason = :'reason')
  RETURNING id
)
INSERT INTO audit_events
  (id, actor_user_id, action, resource_type, resource_id, outcome, occurred_at)
SELECT uuidv7(), NULL, 'request_metadata.stream_loss_recorded', 'incident',
       :'incident_id', 'success', now()
FROM inserted;
DELETE FROM request_metadata_consumer_health WHERE singleton;
SELECT event_count::text || ':' || certainty::text
FROM request_metadata_ingestion_gaps WHERE reason = :'reason';
COMMIT;
SQL
)
recorded=$(printf '%s\n' "$result" | awk '/^[1-9][0-9]*:(exact|lower_bound)$/ { value=$0 } END { print value }')
[[ $recorded == "$event_count:$certainty" ]] || {
  echo "existing incident record has a different count/certainty or was not recorded" >&2
  exit 1
}
echo "request metadata stream loss recorded: incident=$incident_id count=$event_count precision=$precision"
