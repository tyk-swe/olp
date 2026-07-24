#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat >&2 <<'EOF'
usage: OLP_QUALIFICATION_DATABASE_URL=postgres://... \
       OLP_QUALIFICATION_RESTORE_DATABASE_URL=postgres://... \
       OLP_VALKEY_URL=redis://... tests/qualification/n-minus-one.sh
EOF
}
if [[ ${1:-} == --help || ${1:-} == -h ]]; then usage; exit 0; fi
[[ $# -eq 0 ]] || { usage; exit 2; }
: "${OLP_QUALIFICATION_DATABASE_URL:?OLP_QUALIFICATION_DATABASE_URL is required}"
: "${OLP_QUALIFICATION_RESTORE_DATABASE_URL:?OLP_QUALIFICATION_RESTORE_DATABASE_URL is required}"
: "${OLP_VALKEY_URL:?OLP_VALKEY_URL is required}"
for command in cargo curl docker jq openssl psql valkey-cli; do
  command -v "$command" >/dev/null || { echo "required command is unavailable: $command" >&2; exit 1; }
done

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
"$root/scripts/check-release-version.sh" >/dev/null
declare -A metadata=()
while IFS= read -r line || [[ -n $line ]]; do
  [[ $line =~ ^[[:space:]]*($|#) ]] && continue
  [[ $line =~ ^([A-Z0-9_]+)=(.*)$ ]] || { echo "invalid release metadata line" >&2; exit 1; }
  metadata["${BASH_REMATCH[1]}"]=${BASH_REMATCH[2]}
done <"$root/release-metadata.env"
mode=${metadata[OLP_PREVIOUS_RELEASE_MODE]}
previous_version=${metadata[OLP_PREVIOUS_RELEASED_VERSION]}
previous_image=${metadata[OLP_PREVIOUS_RELEASED_IMAGE]}
previous_migration=${metadata[OLP_PREVIOUS_RELEASED_SCHEMA_MIGRATION]}
candidate="$root/target/debug/olp"
[[ -x $candidate ]] || cargo build --locked -p olp

work=$(mktemp -d)
previous_container=
candidate_pid=
cleanup() {
  local status=$?
  trap - EXIT INT TERM
  [[ -z $candidate_pid ]] || kill "$candidate_pid" 2>/dev/null || true
  [[ -z $candidate_pid ]] || wait "$candidate_pid" 2>/dev/null || true
  [[ -z $previous_container ]] || docker rm -f "$previous_container" >/dev/null 2>&1 || true
  if (( status != 0 )); then
    tail -n 240 "$work/previous.log" >&2 || true
    tail -n 240 "$work/candidate.log" >&2 || true
  fi
  rm -rf "$work"
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

mkdir "$work/console" "$work/media"
printf '<!doctype html><html></html>\n' >"$work/console/index.html"
openssl rand -base64 32 >"$work/master-key"
openssl rand -base64 32 >"$work/auth-hmac-key"
openssl rand -base64 32 >"$work/bootstrap-token"
chmod 600 "$work/master-key" "$work/auth-hmac-key" "$work/bootstrap-token"
expected_generation=0
cookies=
api_key=

if [[ $mode == bootstrap ]]; then
  [[ $previous_version == none && $previous_image == none && $previous_migration == 0021 ]]
  OLP_DATABASE_URL=$OLP_QUALIFICATION_DATABASE_URL \
    OLP_ALLOW_PARTIAL_MIGRATIONS_FOR_TESTS=test-only \
    "$candidate" migrate --through-version "$((10#$previous_migration))"
  psql "$OLP_QUALIFICATION_DATABASE_URL" -X --no-psqlrc -v ON_ERROR_STOP=1 \
    -c "INSERT INTO usage_consumer_health (singleton,pending_events,lag_events,checked_at)
        VALUES (true,0,0,now()) ON CONFLICT (singleton) DO UPDATE
        SET pending_events=0,lag_events=0,checked_at=now()"
else
  command -v cosign >/dev/null || { echo "cosign is required in released mode" >&2; exit 1; }
  cosign verify \
    --certificate-identity "https://github.com/tyk-swe/olp/.github/workflows/release.yml@refs/tags/v${previous_version}" \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com \
    "$previous_image" >"$work/cosign-verify.json"
  docker pull "$previous_image"
  docker run --rm --network host \
    -e "OLP_DATABASE_URL=$OLP_QUALIFICATION_DATABASE_URL" \
    -e "OLP_VALKEY_URL=$OLP_VALKEY_URL" "$previous_image" migrate
  previous_container="olp-n-minus-one-$$"
  docker run -d --name "$previous_container" --network host \
    --user "$(id -u):$(id -g)" \
    -e "OLP_DATABASE_URL=$OLP_QUALIFICATION_DATABASE_URL" \
    -e "OLP_VALKEY_URL=$OLP_VALKEY_URL" \
    -e OLP_LISTEN_ADDR=127.0.0.1:28200 \
    -e OLP_OBSERVABILITY_LISTEN_ADDR=127.0.0.1:29200 \
    -e OLP_PUBLIC_ORIGIN=http://127.0.0.1:28200 \
    -e OLP_MASTER_KEY_FILE=/qualification/master-key \
    -e OLP_AUTH_HMAC_KEY_FILE=/qualification/auth-hmac-key \
    -e OLP_BOOTSTRAP_TOKEN_FILE=/qualification/bootstrap-token \
    -e OLP_CONSOLE_DIR=/qualification/console \
    -e OLP_MEDIA_SPOOL_DIR=/qualification/media \
    -v "$work:/qualification" "$previous_image" all >/dev/null
  for _ in $(seq 1 300); do
    curl -fsS http://127.0.0.1:29200/health/live >/dev/null 2>&1 && break
    sleep 0.1
  done
  status=$(curl -sS -D "$work/setup.headers" -o "$work/setup.json" -w '%{http_code}' \
    -X POST http://127.0.0.1:28200/api/v1/setup \
    -H 'Origin: http://127.0.0.1:28200' \
    -H "X-OLP-Setup-Token: $(<"$work/bootstrap-token")" -H 'Content-Type: application/json' \
    --data '{"email":"owner@n-minus-one.test","password":"correct horse battery staple","display_name":"Owner","installation_name":"N-1 qualification"}')
  [[ $status == 201 ]] || { cat "$work/setup.json" >&2; exit 1; }
  csrf=$(jq -er .csrf_token "$work/setup.json")
  session=$(awk 'BEGIN{IGNORECASE=1} /^set-cookie: __Host-olp_session=/{sub(/^set-cookie: /,""); sub(/;.*/,""); gsub("\r",""); print}' "$work/setup.headers")
  csrf_cookie=$(awk 'BEGIN{IGNORECASE=1} /^set-cookie: __Host-olp_csrf=/{sub(/^set-cookie: /,""); sub(/;.*/,""); gsub("\r",""); print}' "$work/setup.headers")
  cookies="$session; $csrf_cookie"
  status=$(curl -sS -o "$work/provider.json" -w '%{http_code}' -X POST \
    http://127.0.0.1:28200/api/v1/providers -H 'Origin: http://127.0.0.1:28200' \
    -H "Cookie: $cookies" -H "x-csrf-token: $csrf" \
    -H 'Idempotency-Key: n-minus-one-draft-0001' -H 'Content-Type: application/json' \
    --data '{"name":"N-1 draft","kind":"openai","credential":"fixture","model":"future-model"}')
  [[ $status == 201 ]] || { cat "$work/provider.json" >&2; exit 1; }
  status=$(curl -sS -o "$work/key.json" -w '%{http_code}' -X POST \
    http://127.0.0.1:28200/api/v1/api-keys -H 'Origin: http://127.0.0.1:28200' \
    -H "Cookie: $cookies" -H "x-csrf-token: $csrf" \
    -H 'Idempotency-Key: n-minus-one-key-0001' -H 'Content-Type: application/json' \
    --data '{"name":"N-1 key","scopes":["models_read"]}')
  [[ $status == 201 ]] || { cat "$work/key.json" >&2; exit 1; }
  api_key=$(jq -er .secret "$work/key.json")
  expected_generation=$(jq -er .runtime_generation.sequence "$work/key.json")
  for _ in $(seq 1 120); do
    checkpoint=$(psql "$OLP_QUALIFICATION_DATABASE_URL" -X --no-psqlrc -Atc \
      "SELECT pending_events || ':' || lag_events FROM request_metadata_consumer_health WHERE singleton" 2>/dev/null || true)
    [[ $checkpoint == 0:0 ]] && break
    sleep 0.25
  done
  [[ $checkpoint == 0:0 ]] || { echo "previous release did not reach a zero-backlog checkpoint" >&2; exit 1; }
  docker stop "$previous_container" >"$work/previous.log"
  docker rm "$previous_container" >/dev/null
  previous_container=
fi

previous_backup=$(OLP_DATABASE_URL=$OLP_QUALIFICATION_DATABASE_URL \
  OLP_BACKUP_TRAFFIC_QUIESCED=true "$root/scripts/backup.sh" "$work/backups")
export OLP_MASTER_KEY_FILE="$work/master-key"
export OLP_AUTH_HMAC_KEY_FILE="$work/auth-hmac-key"
export OLP_CONSOLE_DIR="$work/console"
export OLP_MEDIA_SPOOL_DIR="$work/media"
OLP_REHEARSAL_DATABASE_URL=$OLP_QUALIFICATION_RESTORE_DATABASE_URL \
  OLP_REHEARSAL_CONFIRM=destroy-target \
  OLP_REHEARSAL_PREVIOUS_RELEASED_SCHEMA_MIGRATION="$previous_migration" \
  OLP_REHEARSAL_RUN_DOCTOR=true OLP_BIN="$candidate" \
  "$root/scripts/upgrade-rehearsal.sh" "$previous_backup"

if [[ $mode == released ]]; then
  valkey-cli -u "$OLP_VALKEY_URL" FLUSHALL >/dev/null
  OLP_DATABASE_URL=$OLP_QUALIFICATION_RESTORE_DATABASE_URL \
  OLP_LISTEN_ADDR=127.0.0.1:28200 OLP_OBSERVABILITY_LISTEN_ADDR=127.0.0.1:29200 \
  OLP_PUBLIC_ORIGIN=http://127.0.0.1:28200 \
    "$candidate" all >"$work/candidate.log" 2>&1 &
  candidate_pid=$!
  for _ in $(seq 1 300); do
    ready=$(curl -fsS http://127.0.0.1:29200/health/ready 2>/dev/null || true)
    [[ $(jq -r '.generation // 0' <<<"${ready:-null}") == "$expected_generation" ]] && break
    sleep 0.1
  done
  [[ $(jq -r '.generation // 0' <<<"${ready:-null}") == "$expected_generation" ]]
  curl -fsS http://127.0.0.1:28200/api/v1/sessions/current \
    -H 'Origin: http://127.0.0.1:28200' -H "Cookie: $cookies" | jq -e '.user.id' >/dev/null
  curl -fsS http://127.0.0.1:28200/openai/v1/models \
    -H "Authorization: Bearer $api_key" | jq -e '.object == "list"' >/dev/null
fi
echo "N-1 qualification passed: mode=$mode previous=$previous_version migration=$previous_migration"
