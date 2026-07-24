#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat >&2 <<'EOF'
usage: OLP_QUALIFICATION_DATABASE_URL=postgres://... \
       OLP_QUALIFICATION_RESTORE_DATABASE_URL=postgres://... \
       OLP_VALKEY_URL=redis://... tests/qualification/backup-restore.sh
EOF
}
if [[ ${1:-} == --help || ${1:-} == -h ]]; then usage; exit 0; fi
[[ $# -eq 0 ]] || { usage; exit 2; }
: "${OLP_QUALIFICATION_DATABASE_URL:?OLP_QUALIFICATION_DATABASE_URL is required}"
: "${OLP_QUALIFICATION_RESTORE_DATABASE_URL:?OLP_QUALIFICATION_RESTORE_DATABASE_URL is required}"
: "${OLP_VALKEY_URL:?OLP_VALKEY_URL is required}"
for command in cargo curl jq openssl psql pg_dump pg_restore valkey-cli; do
  command -v "$command" >/dev/null || { echo "required command is unavailable: $command" >&2; exit 1; }
done

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
olp_bin=${OLP_BIN:-"$root/target/debug/olp"}
work=$(mktemp -d)
server_pid=
cleanup() {
  local status=$?
  trap - EXIT INT TERM
  if [[ -n $server_pid ]]; then
    kill "$server_pid" 2>/dev/null || true
    wait "$server_pid" 2>/dev/null || true
  fi
  if (( status != 0 )); then
    tail -n 300 "$work/server.log" >&2 || true
  fi
  rm -rf "$work"
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

[[ -x $olp_bin ]] || cargo build --locked -p olp
mkdir "$work/console" "$work/media"
printf '<!doctype html><html></html>\n' >"$work/console/index.html"
openssl rand -base64 32 >"$work/master-key"
openssl rand -base64 32 >"$work/auth-hmac-key"
openssl rand -base64 32 >"$work/bootstrap-token"
chmod 600 "$work/master-key" "$work/auth-hmac-key" "$work/bootstrap-token"
export OLP_MASTER_KEY_FILE="$work/master-key"
export OLP_AUTH_HMAC_KEY_FILE="$work/auth-hmac-key"
export OLP_CONSOLE_DIR="$work/console"
export OLP_MEDIA_SPOOL_DIR="$work/media"

OLP_DATABASE_URL=$OLP_QUALIFICATION_DATABASE_URL "$olp_bin" migrate
start_server() {
  local database_url=$1 bootstrap_file=${2:-}
  local -a environment=(
    "OLP_DATABASE_URL=$database_url"
    "OLP_VALKEY_URL=$OLP_VALKEY_URL"
    "OLP_LISTEN_ADDR=127.0.0.1:28100"
    "OLP_OBSERVABILITY_LISTEN_ADDR=127.0.0.1:29100"
    "OLP_PUBLIC_ORIGIN=http://127.0.0.1:28100"
    "OLP_MASTER_KEY_FILE=$OLP_MASTER_KEY_FILE"
    "OLP_AUTH_HMAC_KEY_FILE=$OLP_AUTH_HMAC_KEY_FILE"
    "OLP_CONSOLE_DIR=$OLP_CONSOLE_DIR"
    "OLP_MEDIA_SPOOL_DIR=$OLP_MEDIA_SPOOL_DIR"
  )
  [[ -z $bootstrap_file ]] || environment+=("OLP_BOOTSTRAP_TOKEN_FILE=$bootstrap_file")
  env "${environment[@]}" "$olp_bin" all >"$work/server.log" 2>&1 &
  server_pid=$!
  for _ in $(seq 1 300); do
    curl -fsS http://127.0.0.1:29100/health/live >/dev/null 2>&1 && return
    kill -0 "$server_pid" 2>/dev/null || { echo "server exited during startup" >&2; exit 1; }
    sleep 0.1
  done
  echo "server did not become live" >&2
  exit 1
}
start_server "$OLP_QUALIFICATION_DATABASE_URL" "$work/bootstrap-token"
origin=http://127.0.0.1:28100
status=$(curl -sS -D "$work/setup.headers" -o "$work/setup.json" -w '%{http_code}' \
  -X POST "$origin/api/v1/setup" -H "Origin: $origin" \
  -H "X-OLP-Setup-Token: $(<"$work/bootstrap-token")" -H 'Content-Type: application/json' \
  --data '{"email":"owner@restore.qualification.test","password":"correct horse battery staple","display_name":"Owner","installation_name":"Restore qualification"}')
[[ $status == 201 ]] || { cat "$work/setup.json" >&2; exit 1; }
csrf=$(jq -er .csrf_token "$work/setup.json")
owner_id=$(jq -er .user.id "$work/setup.json")
session=$(awk 'BEGIN{IGNORECASE=1} /^set-cookie: __Host-olp_session=/{sub(/^set-cookie: /,""); sub(/;.*/,""); gsub("\r",""); print}' "$work/setup.headers")
csrf_cookie=$(awk 'BEGIN{IGNORECASE=1} /^set-cookie: __Host-olp_csrf=/{sub(/^set-cookie: /,""); sub(/;.*/,""); gsub("\r",""); print}' "$work/setup.headers")
cookies="$session; $csrf_cookie"

LAST_BODY=
LAST_ETAG=
mutate() {
  local method=$1 path=$2 payload=$3 expected=$4 idempotency=${5:-} if_match=${6:-}
  local -a args=(-sS -D "$work/mutation.headers" -o "$work/mutation.json" -w '%{http_code}'
    -X "$method" "$origin$path" -H "Origin: $origin" -H "Cookie: $cookies"
    -H "x-csrf-token: $csrf" -H 'Content-Type: application/json')
  [[ -z $payload ]] || args+=(--data "$payload")
  [[ -z $idempotency ]] || args+=(-H "Idempotency-Key: $idempotency")
  [[ -z $if_match ]] || args+=(-H "If-Match: $if_match")
  local response_status
  response_status=$(curl "${args[@]}")
  [[ $response_status == "$expected" ]] || { cat "$work/mutation.json" >&2; exit 1; }
  LAST_BODY=$(<"$work/mutation.json")
  LAST_ETAG=$(awk 'BEGIN{IGNORECASE=1} /^etag:/{gsub("\r",""); print $2}' "$work/mutation.headers")
}

provider_payload='{"name":"restore-provider","kind":"openai","credential":"fixture","model":"restore-model"}'
mutate POST /api/v1/providers "$provider_payload" 201 restore-provider-0001
provider_id=$(jq -er .id <<<"$LAST_BODY")
mutate GET "/api/v1/providers/$provider_id" '' 200
provider_etag=$LAST_ETAG
mutate GET "/api/v1/providers/$provider_id/models?limit=100" '' 200
model_id=$(jq -er '.items[] | select(.upstream_model == "restore-model") | .id' <<<"$LAST_BODY")
mutate PATCH "/api/v1/providers/$provider_id/models/$model_id" \
  '{"enabled":true,"capabilities":[{"operation":"generation","surface":"openai","mode":"unary"}]}' \
  200 '' "$provider_etag"
provider_etag=$LAST_ETAG
psql "$OLP_QUALIFICATION_DATABASE_URL" -X --no-psqlrc -v ON_ERROR_STOP=1 \
  -c "UPDATE providers SET last_probe_status='succeeded', last_probe_at=clock_timestamp() WHERE id='$provider_id'::uuid" \
  -c "UPDATE model_capabilities SET source='certified', certified_at=clock_timestamp() WHERE provider_model_id='$model_id'::uuid"
mutate POST "/api/v1/providers/$provider_id/activate" '' 200 restore-provider-activate-0001 "$provider_etag"
route_payload=$(jq -cn --arg provider "$provider_id" \
  '{slug:"restore-route",operations:["generation"],overall_timeout_ms:2000,max_attempts:1,targets:[{provider_id:$provider,provider_model:"restore-model",priority:0,weight:1,timeout_ms:1000}]}')
mutate POST /api/v1/route-drafts "$route_payload" 201 restore-route-0001
draft_id=$(jq -er .id <<<"$LAST_BODY")
draft_etag=$LAST_ETAG
mutate POST "/api/v1/route-drafts/$draft_id/validate" '' 200 '' "$draft_etag"
mutate POST "/api/v1/route-drafts/$draft_id/activate" '' 200 restore-route-activate-0001 "$LAST_ETAG"
# Retain a separate draft to prove non-active configuration survives.
draft_payload=$(jq -cn --arg provider "$provider_id" \
  '{slug:"future-route",operations:["generation"],overall_timeout_ms:2000,max_attempts:1,targets:[{provider_id:$provider,provider_model:"restore-model",priority:0,weight:1,timeout_ms:1000}]}')
mutate POST /api/v1/route-drafts "$draft_payload" 201 restore-future-draft-0001
future_draft_id=$(jq -er .id <<<"$LAST_BODY")
mutate POST /api/v1/api-keys \
  '{"name":"Restore qualification","scopes":["models_read"],"allowed_routes":["restore-route"]}' \
  201 restore-key-0001
api_key=$(jq -er .secret <<<"$LAST_BODY")
expected_generation=$(jq -er .runtime_generation.sequence <<<"$LAST_BODY")

for _ in $(seq 1 120); do
  checkpoint=$(psql "$OLP_QUALIFICATION_DATABASE_URL" -X --no-psqlrc -Atc \
    "SELECT pending_events || ':' || lag_events FROM request_metadata_consumer_health WHERE singleton" 2>/dev/null || true)
  [[ $checkpoint == 0:0 ]] && break
  sleep 0.25
done
[[ $checkpoint == 0:0 ]] || { echo "worker did not publish a zero-backlog checkpoint" >&2; exit 1; }
kill "$server_pid"
wait "$server_pid"
server_pid=

backup=$(OLP_DATABASE_URL=$OLP_QUALIFICATION_DATABASE_URL \
  OLP_BACKUP_TRAFFIC_QUIESCED=true "$root/scripts/backup.sh" "$work/backups")
"$root/scripts/backup-manifest.sh" validate "$backup" v2 >/dev/null
OLP_RESTORE_DATABASE_URL=$OLP_QUALIFICATION_RESTORE_DATABASE_URL \
  "$root/scripts/restore-rehearsal.sh" "$backup" --replace
OLP_DATABASE_URL=$OLP_QUALIFICATION_RESTORE_DATABASE_URL "$olp_bin" doctor >/dev/null
valkey-cli -u "$OLP_VALKEY_URL" FLUSHALL >/dev/null
start_server "$OLP_QUALIFICATION_RESTORE_DATABASE_URL"

ready=
for _ in $(seq 1 120); do
  ready=$(curl -fsS http://127.0.0.1:29100/health/ready 2>/dev/null || true)
  [[ $(jq -r '.generation // 0' <<<"${ready:-null}") == "$expected_generation" ]] && break
  sleep 0.1
done
[[ $(jq -r '.generation // 0' <<<"${ready:-null}") == "$expected_generation" ]]
status=$(curl -sS -o "$work/restored-session.json" -w '%{http_code}' \
  "$origin/api/v1/sessions/current" -H "Origin: $origin" -H "Cookie: $cookies")
[[ $status == 200 ]] || { cat "$work/restored-session.json" >&2; exit 1; }
jq -e --arg owner "$owner_id" '.user.id == $owner' "$work/restored-session.json" >/dev/null
status=$(curl -sS -o "$work/restored-models.json" -w '%{http_code}' \
  "$origin/openai/v1/models" -H "Authorization: Bearer $api_key")
[[ $status == 200 ]] || { cat "$work/restored-models.json" >&2; exit 1; }
jq -e '.data | any(.id == "restore-route")' "$work/restored-models.json" >/dev/null
psql "$OLP_QUALIFICATION_RESTORE_DATABASE_URL" -X --no-psqlrc -Atc \
  "SELECT id FROM route_drafts WHERE id='$future_draft_id'::uuid" | grep -Fx "$future_draft_id" >/dev/null
echo "backup/restore qualification passed: owner=$owner_id generation=$expected_generation draft=$future_draft_id"
