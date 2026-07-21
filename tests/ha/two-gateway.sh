#!/usr/bin/env bash
set -euo pipefail

: "${OLP_HA_DATABASE_ADMIN_URL:?set the PostgreSQL 18 maintenance database URL}"
: "${OLP_HA_DATABASE_URL_PREFIX:?set the PostgreSQL 18 URL prefix without a database}"

root_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
olp_bin=${OLP_BIN:-"$root_dir/target/debug/olp"}
database_name=${OLP_HA_DATABASE_NAME:-olp_ha_test}
database_owner=${OLP_HA_DATABASE_OWNER:-olp}
database_app_url_prefix=${OLP_HA_DATABASE_APP_URL_PREFIX:-$OLP_HA_DATABASE_URL_PREFIX}
toxiproxy_api=${OLP_HA_TOXIPROXY_API:-}
toxiproxy_name=${OLP_HA_TOXIPROXY_NAME:-olp-postgresql}
control_origin=${OLP_HA_CONTROL_ORIGIN:-http://127.0.0.1:18090}
gateway_two=${OLP_HA_GATEWAY_TWO:-http://127.0.0.1:18091}
control_observability=${OLP_HA_CONTROL_OBSERVABILITY:-http://127.0.0.1:19090}
gateway_two_observability=${OLP_HA_GATEWAY_TWO_OBSERVABILITY:-http://127.0.0.1:19091}
valkey_port=${OLP_HA_VALKEY_PORT:-56379}
valkey_url="redis://127.0.0.1:${valkey_port}"
psql_command=${OLP_PSQL:-psql}
createdb_command=${OLP_CREATEDB:-createdb}
dropdb_command=${OLP_DROPDB:-dropdb}

for command in "$olp_bin" "$psql_command" "$createdb_command" "$dropdb_command" curl jq openssl; do
  command -v "$command" >/dev/null || { echo "required command is unavailable: $command" >&2; exit 1; }
done
[[ $database_name =~ ^[a-z0-9_]+$ ]] || { echo "unsafe database name" >&2; exit 1; }

work=$(mktemp -d)
all_pid=
gateway_pid=
valkey_pid=
valkey_container=

stop_valkey() {
  if [[ -n $valkey_pid ]]; then
    kill "$valkey_pid" 2>/dev/null || true
    wait "$valkey_pid" 2>/dev/null || true
    valkey_pid=
  fi
  if [[ -n $valkey_container ]]; then
    docker stop "$valkey_container" >/dev/null 2>&1 || true
    docker rm -f "$valkey_container" >/dev/null 2>&1 || true
    valkey_container=
  fi
}

start_valkey() {
  if [[ -n ${VALKEY_SERVER:-} ]]; then
    "$VALKEY_SERVER" --bind 127.0.0.1 --port "$valkey_port" --save '' --appendonly no \
      >"$work/valkey.log" 2>&1 &
    valkey_pid=$!
  elif command -v docker >/dev/null && docker info >/dev/null 2>&1; then
    valkey_container="olp-ha-valkey-$$"
    docker run -d --name "$valkey_container" -p "127.0.0.1:${valkey_port}:6379" \
      valkey/valkey:9.1@sha256:4963247afc4cd33c7d3b2d2816b9f7f8eeebab148d29056c2ca4d7cbc966f2d9 \
      --save '' --appendonly no >/dev/null
  else
    echo "set VALKEY_SERVER or provide a working Docker daemon" >&2
    exit 1
  fi
  for _ in $(seq 1 100); do
    if (exec 3<>"/dev/tcp/127.0.0.1/${valkey_port}") 2>/dev/null; then
      exec 3>&-
      return
    fi
    sleep 0.05
  done
  echo "Valkey did not become ready" >&2
  exit 1
}

cleanup() {
  local status=$?
  trap - EXIT INT TERM
  set +e
  if (( status != 0 )); then
    echo 'two-gateway HA proof failed; retained process logs follow' >&2
    for log in all.log gateway.log valkey.log; do
      if [[ -f $work/$log ]]; then
        echo "===== $log =====" >&2
        sed -n '1,400p' "$work/$log" >&2
      fi
    done
    if [[ -n $valkey_container ]]; then
      echo '===== valkey container =====' >&2
      docker logs "$valkey_container" >&2 || true
    fi
  fi
  [[ -z $gateway_pid ]] || kill "$gateway_pid" 2>/dev/null || true
  [[ -z $all_pid ]] || kill "$all_pid" 2>/dev/null || true
  [[ -z $gateway_pid ]] || wait "$gateway_pid" 2>/dev/null || true
  [[ -z $all_pid ]] || wait "$all_pid" 2>/dev/null || true
  stop_valkey
  if ! "$dropdb_command" --maintenance-db="$OLP_HA_DATABASE_ADMIN_URL" \
    --if-exists --force "$database_name"; then
    echo "failed to remove HA test database: $database_name" >&2
    (( status == 0 )) && status=1
  fi
  rm -rf "$work"
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

"$psql_command" "$OLP_HA_DATABASE_ADMIN_URL" -X --no-psqlrc -v ON_ERROR_STOP=1 \
  --command="DROP DATABASE IF EXISTS ${database_name} WITH (FORCE)"
"$createdb_command" --maintenance-db="$OLP_HA_DATABASE_ADMIN_URL" \
  --owner="$database_owner" "$database_name"
database_url="${OLP_HA_DATABASE_URL_PREFIX%/}/${database_name}"
app_database_url="${database_app_url_prefix%/}/${database_name}"

openssl rand -base64 32 >"$work/master-key"
openssl rand -base64 32 >"$work/auth-hmac-key"
openssl rand -base64 32 >"$work/bootstrap-token"
chmod 600 "$work/master-key" "$work/auth-hmac-key" "$work/bootstrap-token"

start_valkey
OLP_DATABASE_URL=$database_url OLP_VALKEY_URL=$valkey_url "$olp_bin" migrate

OLP_DATABASE_URL=$app_database_url OLP_VALKEY_URL=$valkey_url \
OLP_LISTEN_ADDR=127.0.0.1:18090 OLP_PUBLIC_ORIGIN=$control_origin \
OLP_OBSERVABILITY_LISTEN_ADDR=127.0.0.1:19090 \
OLP_MASTER_KEY_FILE="$work/master-key" OLP_AUTH_HMAC_KEY_FILE="$work/auth-hmac-key" \
OLP_BOOTSTRAP_TOKEN_FILE="$work/bootstrap-token" \
OLP_CONSOLE_DIR="$work/console" RUST_LOG=olp=debug \
  "$olp_bin" all >"$work/all.log" 2>&1 &
all_pid=$!
OLP_DATABASE_URL=$app_database_url OLP_VALKEY_URL=$valkey_url \
OLP_LISTEN_ADDR=127.0.0.1:18091 OLP_PUBLIC_ORIGIN=$gateway_two \
OLP_OBSERVABILITY_LISTEN_ADDR=127.0.0.1:19091 \
OLP_MASTER_KEY_FILE="$work/master-key" OLP_AUTH_HMAC_KEY_FILE="$work/auth-hmac-key" \
OLP_CONSOLE_DIR="$work/console" RUST_LOG=olp=debug \
  "$olp_bin" gateway >"$work/gateway.log" 2>&1 &
gateway_pid=$!

for endpoint in "$control_observability/health/live" "$gateway_two_observability/health/live"; do
  for _ in $(seq 1 200); do
    curl -fsS "$endpoint" >/dev/null 2>&1 && break
    sleep 0.05
  done
  curl -fsS "$endpoint" >/dev/null
done
for endpoint in \
  "$control_origin/health/live" "$control_origin/metrics" \
  "$gateway_two/health/live" "$gateway_two/metrics"; do
  status=$(curl -sS -o /dev/null -w '%{http_code}' "$endpoint")
  [[ $status == 404 ]] || { echo "public observability endpoint leaked: $endpoint ($status)" >&2; exit 1; }
done

setup_headers="$work/setup.headers"
setup_body="$work/setup.json"
setup_status=$(curl -sS -D "$setup_headers" -o "$setup_body" -w '%{http_code}' \
  -X POST "$control_origin/api/v1/setup" \
  -H "Origin: $control_origin" -H "X-OLP-Setup-Token: $(<"$work/bootstrap-token")" -H 'Content-Type: application/json' \
  --data '{"email":"owner@example.test","password":"correct horse battery staple","display_name":"Owner","installation_name":"HA integration"}')
[[ $setup_status == 201 ]] || { cat "$setup_body" >&2; exit 1; }
csrf=$(jq -er .csrf_token "$setup_body")
owner_id=$(jq -er .user.id "$setup_body")
[[ $owner_id =~ ^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$ ]] || {
  echo "setup returned an invalid owner UUID" >&2
  exit 1
}
session_cookie=$(awk 'BEGIN{IGNORECASE=1} /^set-cookie: __Host-olp_session=/{sub(/^set-cookie: /,""); sub(/;.*/,""); gsub("\r",""); print}' "$setup_headers")
csrf_cookie=$(awk 'BEGIN{IGNORECASE=1} /^set-cookie: __Host-olp_csrf=/{sub(/^set-cookie: /,""); sub(/;.*/,""); gsub("\r",""); print}' "$setup_headers")
cookie_header="$session_cookie; $csrf_cookie"

LAST_BODY=
LAST_ETAG=
mutate() {
  local method=$1 path=$2 payload=$3 expected=$4 idempotency=${5:-} if_match=${6:-}
  local headers="$work/mutation.headers" body="$work/mutation.json"
  local args=(-sS -D "$headers" -o "$body" -w '%{http_code}' -X "$method"
    "$control_origin$path" -H "Origin: $control_origin" -H "Cookie: $cookie_header"
    -H "x-csrf-token: $csrf" -H 'Content-Type: application/json')
  [[ -z $idempotency ]] || args+=(-H "Idempotency-Key: $idempotency")
  [[ -z $if_match ]] || args+=(-H "If-Match: $if_match")
  [[ -z $payload ]] || args+=(--data "$payload")
  local status
  status=$(curl "${args[@]}")
  [[ $status == "$expected" ]] || { echo "$method $path returned $status" >&2; cat "$body" >&2; exit 1; }
  LAST_BODY=$(<"$body")
  LAST_ETAG=$(awk 'BEGIN{IGNORECASE=1} /^etag:/{gsub("\r",""); print $2}' "$headers")
}

# HA exercises runtime convergence, not connector liveness. Native OpenAI uses
# its fixed official endpoint, and activation relies only on the declared model
# capability evidence without sending a provider request.
provider_payload=$(jq -cn '{name:"ha-provider",kind:"openai",credential:"dummy-upstream-key",model:"mock-model"}')
mutate POST /api/v1/providers "$provider_payload" 201 provider-ha-0001
provider_id=$(jq -er .id <<<"$LAST_BODY")
provider_etag=$LAST_ETAG
mutate GET "/api/v1/providers/$provider_id" '' 200
provider_etag=$LAST_ETAG
mutate GET "/api/v1/providers/$provider_id/models?limit=100" '' 200
provider_model_id=$(
  jq -er '.items[] | select(.upstream_model == "mock-model") | .id' <<<"$LAST_BODY"
)
model_review_payload=$(jq -cn '{enabled:true,capabilities:[{operation:"generation",surface:"openai",mode:"unary"}]}')
mutate PATCH "/api/v1/providers/$provider_id/models/$provider_model_id" "$model_review_payload" 200 '' "$provider_etag"
provider_etag=$LAST_ETAG
# Connector probes/certification have their own HTTP integration suite. This
# HA fixture marks the locally declared tuple as certified in PostgreSQL so the
# proof remains deterministic and never calls an external provider.
"$psql_command" "$database_url" -X --no-psqlrc -v ON_ERROR_STOP=1 \
  --command="UPDATE providers SET last_probe_status = 'succeeded', last_probe_at = clock_timestamp() WHERE id = '${provider_id}'::uuid" \
  --command="UPDATE model_capabilities SET source = 'certified', certified_at = clock_timestamp() WHERE provider_model_id = '${provider_model_id}'::uuid"
mutate POST "/api/v1/providers/$provider_id/activate" '' 200 provider-ha-activate-0001 "$provider_etag"

route_payload=$(jq -cn --arg provider "$provider_id" '{slug:"default",operations:["generation"],overall_timeout_ms:2000,max_attempts:1,targets:[{provider_id:$provider,provider_model:"mock-model",priority:0,weight:1,timeout_ms:1000}]}')
mutate POST /api/v1/route-drafts "$route_payload" 201 route-ha-0001
draft_id=$(jq -er .id <<<"$LAST_BODY")
draft_etag=$LAST_ETAG
mutate POST "/api/v1/route-drafts/$draft_id/validate" '' 200 '' "$draft_etag"
draft_etag=$LAST_ETAG
mutate POST "/api/v1/route-drafts/$draft_id/activate" '' 200 route-activate-ha-0001 "$draft_etag"

hard_key_payload='{"name":"HA hard limit","scopes":["inference","models_read"],"allowed_routes":["default"],"requests_per_minute":4}'
mutate POST /api/v1/api-keys "$hard_key_payload" 201 key-ha-hard-0001
hard_key=$(jq -er .secret <<<"$LAST_BODY")
hard_generation=$(jq -er .runtime_generation.sequence <<<"$LAST_BODY")

observability_origin() {
  case "$1" in
    "$control_origin") printf '%s\n' "$control_observability" ;;
    "$gateway_two") printf '%s\n' "$gateway_two_observability" ;;
    *)
      echo "unknown HA public origin: $1" >&2
      return 1
      ;;
  esac
}

wait_for_key() {
  local base=$1 key=$2 expected_generation=$3 label=$4
  local started_ms now_ms status
  started_ms=$(date +%s%3N)
  while :; do
    status=$(curl -sS -o /dev/null -w '%{http_code}' \
      -H "Authorization: Bearer $key" "$base/openai/v1/models" || true)
    [[ $status == 200 ]] && break
    now_ms=$(date +%s%3N)
    if (( now_ms - started_ms >= 5000 )); then
      echo "$base did not converge on $label generation $expected_generation within 5000ms (last status: $status)" >&2
      return 1
    fi
    sleep 0.05
  done
  now_ms=$(date +%s%3N)
  echo "$label convergence: base=$base generation=$expected_generation elapsed_ms=$((now_ms - started_ms))"
}

for base in "$control_origin" "$gateway_two"; do
  wait_for_key "$base" "$hard_key" "$hard_generation" "hard-key activation"
done

# Prove healthy-cluster key revocation converges within the advertised bound.
fast_key_payload='{"name":"HA convergence probe","scopes":["models_read"],"allowed_routes":["default"]}'
mutate POST /api/v1/api-keys "$fast_key_payload" 201 key-ha-fast-0001
fast_key_id=$(jq -er .id <<<"$LAST_BODY")
fast_key=$(jq -er .secret <<<"$LAST_BODY")
fast_key_generation=$(jq -er .runtime_generation.sequence <<<"$LAST_BODY")
fast_key_etag=$LAST_ETAG
for base in "$control_origin" "$gateway_two"; do
  wait_for_key "$base" "$fast_key" "$fast_key_generation" "revocation-key activation"
done
started_ms=$(date +%s%3N)
mutate POST "/api/v1/api-keys/$fast_key_id/revoke" '' 200 key-revoke-ha-fast-0001 "$fast_key_etag"
for base in "$control_origin" "$gateway_two"; do
  while :; do
    status=$(curl -sS -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $fast_key" "$base/openai/v1/models")
    [[ $status == 401 ]] && break
    elapsed_ms=$(( $(date +%s%3N) - started_ms ))
    [[ $elapsed_ms -lt 5000 ]] || { echo "$base healthy revocation exceeded 5000ms" >&2; exit 1; }
    sleep 0.05
  done
done
elapsed_ms=$(( $(date +%s%3N) - started_ms ))
[[ $elapsed_ms -le 5000 ]] || { echo "healthy revocation convergence took ${elapsed_ms}ms" >&2; exit 1; }

chat='{"model":"default","messages":[{"role":"user","content":"HA limiter probe"}],"max_tokens":1}'
for base in "$control_origin" "$gateway_two"; do
  status=$(curl --max-time 5 -sS -o "$work/chat.json" -w '%{http_code}' -X POST \
    "$base/openai/v1/chat/completions" -H "Authorization: Bearer $hard_key" \
    -H 'Content-Type: application/json' --data "$chat" || true)
  [[ $status != 429 ]] || { echo "shared RPM denied too early" >&2; exit 1; }
done
status=$(curl --max-time 5 -sS -o "$work/chat.json" -w '%{http_code}' -X POST \
  "$control_origin/openai/v1/chat/completions" -H "Authorization: Bearer $hard_key" \
  -H 'Content-Type: application/json' --data "$chat" || true)
[[ $status == 429 ]] || { echo "shared RPM was not atomic across gateways: $status" >&2; exit 1; }

# Keep an unlimited key in the last-known-good release so the corruption proof
# is independent of the deliberately exhausted hard-limit key above.
lkg_key_payload='{"name":"HA last-known-good probe","scopes":["models_read"],"allowed_routes":["default"]}'
mutate POST /api/v1/api-keys "$lkg_key_payload" 201 key-ha-lkg-0001
lkg_key=$(jq -er .secret <<<"$LAST_BODY")
pre_corrupt_generation=$(jq -er .runtime_generation.sequence <<<"$LAST_BODY")
for base in "$control_origin" "$gateway_two"; do
  wait_for_key "$base" "$lkg_key" "$pre_corrupt_generation" "last-known-good probe activation"
done

# Publish a corrupt higher sequence directly. Both gateways must retain the
# pinned last-known-good generation and remain ready.
corrupt_sequence=$("$psql_command" "$database_url" -X --no-psqlrc -v ON_ERROR_STOP=1 \
  --quiet --tuples-only --no-align \
  --command="INSERT INTO runtime_generations (id, compiled_release, release_sha256, created_by) VALUES (uuidv7(), decode('00','hex'), decode(repeat('00',32),'hex'), '${owner_id}'::uuid) RETURNING sequence")
[[ $corrupt_sequence -gt $pre_corrupt_generation ]]
sleep 5.2
for base in "$control_origin" "$gateway_two"; do
  status=$(curl -sS -o /dev/null -w '%{http_code}' \
    -H "Authorization: Bearer $lkg_key" "$base/openai/v1/models" || true)
  [[ $status == 200 ]] || { echo "$base did not retain its last-known-good generation after corrupt release (status: $status)" >&2; exit 1; }
  curl -fsS "$(observability_origin "$base")/health/ready" >/dev/null
done

soft_key_payload='{"name":"HA no hard limits","scopes":["inference","models_read"],"allowed_routes":["default"]}'
mutate POST /api/v1/api-keys "$soft_key_payload" 201 key-ha-soft-0001
soft_key_id=$(jq -er .id <<<"$LAST_BODY")
soft_key=$(jq -er .secret <<<"$LAST_BODY")
soft_generation=$(jq -er .runtime_generation.sequence <<<"$LAST_BODY")
soft_key_etag=$LAST_ETAG
for base in "$control_origin" "$gateway_two"; do
  wait_for_key "$base" "$soft_key" "$soft_generation" "corrupt-release recovery"
done

if [[ -n $toxiproxy_api ]]; then
  set_database_proxy() {
    local enabled=$1
    local toxic_name=olp-postgresql-reset
    if [[ $enabled == true ]]; then
      curl -fsS -X DELETE \
        "$toxiproxy_api/proxies/$toxiproxy_name/toxics/$toxic_name" >/dev/null
    else
      curl -fsS -X POST "$toxiproxy_api/proxies/$toxiproxy_name/toxics" \
        -H 'Content-Type: application/json' \
        --data "{\"name\":\"$toxic_name\",\"type\":\"reset_peer\",\"stream\":\"downstream\",\"toxicity\":1,\"attributes\":{\"timeout\":0}}" >/dev/null
    fi
  }

  set_database_proxy false
  for base in "$control_origin" "$gateway_two"; do
    # The private readiness endpoint is deliberately cache-backed. Allow one
    # failed four-second collection and a subsequent five-second refresh to
    # observe that the SQLx pool has discarded its broken connections.
    for _ in $(seq 1 250); do
      readiness=$(curl --max-time 5 -fsS "$(observability_origin "$base")/health/ready" 2>/dev/null || true)
      [[ $(jq -r '.database // empty' <<<"${readiness:-null}") == unavailable_lkg ]] && break
      sleep 0.1
    done
    [[ $(jq -r '.database // empty' <<<"${readiness:-null}") == unavailable_lkg ]] || {
      echo "$base did not expose PostgreSQL last-known-good degradation" >&2
      exit 1
    }
    status=$(curl -sS -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $soft_key" "$base/openai/v1/models")
    [[ $status == 200 ]] || { echo "$base stopped LKG traffic during PostgreSQL outage: $status" >&2; exit 1; }
  done
  set_database_proxy true
  for base in "$control_origin" "$gateway_two"; do
    for _ in $(seq 1 150); do
      readiness=$(curl --max-time 5 -fsS "$(observability_origin "$base")/health/ready" 2>/dev/null || true)
      [[ $(jq -r '.database // empty' <<<"${readiness:-null}") == ok ]] && break
      sleep 0.1
    done
    [[ $(jq -r '.database // empty' <<<"${readiness:-null}") == ok ]] || {
      echo "$base did not recover PostgreSQL readiness" >&2
      exit 1
    }
  done
fi

stop_valkey
for base in "$control_origin" "$gateway_two"; do
  for _ in $(seq 1 120); do
    readiness=$(curl --max-time 3 -fsS "$(observability_origin "$base")/health/ready" 2>/dev/null || true)
    if [[ $(jq -r '.status // empty' <<<"${readiness:-null}") == degraded && \
      $(jq -r '.limits // empty' <<<"${readiness:-null}") == unavailable ]]; then
      break
    fi
    sleep 0.1
  done
  [[ $(jq -r '.status // empty' <<<"${readiness:-null}") == degraded ]] || {
    echo "$base did not remain traffic-ready with degraded distributed limits" >&2
    exit 1
  }
done
hard_status=$(curl --max-time 3 -sS -o /dev/null -w '%{http_code}' -X POST \
  "$control_origin/openai/v1/chat/completions" -H "Authorization: Bearer $hard_key" \
  -H 'Content-Type: application/json' --data "$chat" || true)
[[ $hard_status == 503 ]] || { echo "hard-limited key did not fail closed: $hard_status" >&2; exit 1; }
soft_status=$(curl --max-time 5 -sS -o /dev/null -w '%{http_code}' -X POST \
  "$control_origin/openai/v1/chat/completions" -H "Authorization: Bearer $soft_key" \
  -H 'Content-Type: application/json' --data "$chat" || true)
[[ $soft_status != 503 ]] || { echo "unlimited key incorrectly failed on Valkey outage" >&2; exit 1; }

started_ms=$(date +%s%3N)
mutate POST "/api/v1/api-keys/$soft_key_id/revoke" '' 200 key-revoke-ha-0001 "$soft_key_etag"
for base in "$control_origin" "$gateway_two"; do
  for _ in $(seq 1 120); do
    status=$(curl -sS -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $soft_key" "$base/openai/v1/models")
    [[ $status == 401 ]] && break
    sleep 0.05
  done
  [[ $status == 401 ]] || { echo "$base did not converge on revocation" >&2; exit 1; }
done
elapsed_ms=$(( $(date +%s%3N) - started_ms ))
[[ $elapsed_ms -le 5500 ]] || { echo "missed-hint revocation convergence took ${elapsed_ms}ms" >&2; exit 1; }

echo "two-gateway HA proof passed: generation=${soft_generation} revocation_ms=${elapsed_ms}"
