#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat >&2 <<'EOF'
usage: OLP_DATABASE_URL=postgres://... OLP_VALKEY_URL=redis://... \
  OLP_CANARY_OPENAI_API_KEY=... OLP_CANARY_OPENAI_MODEL=... \
  OLP_CANARY_ANTHROPIC_API_KEY=... OLP_CANARY_ANTHROPIC_MODEL=... \
  OLP_CANARY_GEMINI_API_KEY=... OLP_CANARY_GEMINI_MODEL=... \
  tests/qualification/canary.sh
EOF
}
if [[ ${1:-} == --help || ${1:-} == -h ]]; then usage; exit 0; fi
[[ $# -eq 0 ]] || { usage; exit 2; }
: "${OLP_DATABASE_URL:?OLP_DATABASE_URL is required}"
: "${OLP_VALKEY_URL:?OLP_VALKEY_URL is required}"
for provider in OPENAI ANTHROPIC GEMINI; do
  key_name="OLP_CANARY_${provider}_API_KEY"
  model_name="OLP_CANARY_${provider}_MODEL"
  [[ -n ${!key_name:-} ]] || { echo "$key_name is required" >&2; exit 1; }
  [[ -n ${!model_name:-} ]] || { echo "$model_name is required" >&2; exit 1; }
done
for command in cargo curl jq openssl; do
  command -v "$command" >/dev/null || { echo "required command is unavailable: $command" >&2; exit 1; }
done

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
olp_bin=${OLP_BIN:-"$root/target/debug/olp"}
work=$(mktemp -d)
server_pid=
cleanup() {
  local status=$?
  trap - EXIT INT TERM
  [[ -z $server_pid ]] || kill "$server_pid" 2>/dev/null || true
  [[ -z $server_pid ]] || wait "$server_pid" 2>/dev/null || true
  if (( status != 0 )); then tail -n 300 "$work/server.log" >&2 || true; fi
  rm -rf "$work"
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

[[ -x $olp_bin ]] || cargo build --locked -p olp
mkdir "$work/console" "$work/media"
printf '<!doctype html><html></html>\n' >"$work/console/index.html"
for key in master auth bootstrap; do openssl rand -base64 32 >"$work/$key"; chmod 600 "$work/$key"; done
export OLP_MASTER_KEY_FILE="$work/master"
export OLP_AUTH_HMAC_KEY_FILE="$work/auth"
export OLP_BOOTSTRAP_TOKEN_FILE="$work/bootstrap"
export OLP_CONSOLE_DIR="$work/console"
export OLP_MEDIA_SPOOL_DIR="$work/media"
export OLP_LISTEN_ADDR=127.0.0.1:28300
export OLP_OBSERVABILITY_LISTEN_ADDR=127.0.0.1:29300
export OLP_PUBLIC_ORIGIN=http://127.0.0.1:28300
"$olp_bin" migrate
"$olp_bin" all >"$work/server.log" 2>&1 &
server_pid=$!
for _ in $(seq 1 300); do
  curl -fsS http://127.0.0.1:29300/health/live >/dev/null 2>&1 && break
  kill -0 "$server_pid" 2>/dev/null || { echo "canary server exited during startup" >&2; exit 1; }
  sleep 0.1
done
origin=$OLP_PUBLIC_ORIGIN
status=$(curl -sS -D "$work/setup.headers" -o "$work/setup.json" -w '%{http_code}' \
  -X POST "$origin/api/v1/setup" -H "Origin: $origin" \
  -H "X-OLP-Setup-Token: $(<"$work/bootstrap")" -H 'Content-Type: application/json' \
  --data '{"email":"owner@canary.test","password":"correct horse battery staple","display_name":"Canary","installation_name":"Live connector canary"}')
[[ $status == 201 ]] || { cat "$work/setup.json" >&2; exit 1; }
csrf=$(jq -er .csrf_token "$work/setup.json")
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
  [[ $response_status == "$expected" ]] || {
    echo "$method $path returned $response_status, expected $expected" >&2
    cat "$work/mutation.json" >&2
    exit 1
  }
  LAST_BODY=$(<"$work/mutation.json")
  LAST_ETAG=$(awk 'BEGIN{IGNORECASE=1} /^etag:/{gsub("\r",""); print $2}' "$work/mutation.headers")
}

configure_provider() {
  local provider=$1 kind=$2 surface=$3 model=$4 credential=$5
  local route="canary-$provider"
  local payload
  payload=$(jq -cn --arg name "$provider-canary" --arg kind "$kind" --arg credential "$credential" --arg model "$model" \
    '{name:$name,kind:$kind,credential:$credential,model:$model}')
  mutate POST /api/v1/providers "$payload" 201 "canary-$provider-provider-0001"
  local provider_id
  provider_id=$(jq -er .id <<<"$LAST_BODY")
  mutate GET "/api/v1/providers/$provider_id" '' 200
  local etag=$LAST_ETAG
  # This is credentialed discovery through the production connector.
  mutate POST "/api/v1/providers/$provider_id/probe" '' 200 '' "$etag"
  mutate GET "/api/v1/providers/$provider_id/models?limit=100" '' 200
  local model_id
  model_id=$(jq -er --arg model "$model" '.items[] | select(.upstream_model == $model) | .id' <<<"$LAST_BODY")
  local capabilities
  capabilities=$(jq -cn --arg surface "$surface" \
    '{enabled:true,capabilities:[
      {operation:"generation",surface:$surface,mode:"unary"},
      {operation:"generation",surface:$surface,mode:"streaming"},
      {operation:"token_count",surface:$surface,mode:"unary"}]}')
  mutate PATCH "/api/v1/providers/$provider_id/models/$model_id" "$capabilities" 200 '' "$etag"
  etag=$LAST_ETAG
  mutate POST "/api/v1/providers/$provider_id/models/$model_id/certify" '' 200 '' "$etag"
  jq -e '.status == "succeeded" and .certified_count == 3' <<<"$LAST_BODY" >/dev/null
  etag=$LAST_ETAG
  mutate POST "/api/v1/providers/$provider_id/activate" '' 200 "canary-$provider-activate-0001" "$etag"
  local route_payload
  route_payload=$(jq -cn --arg route "$route" --arg provider "$provider_id" --arg model "$model" \
    '{slug:$route,operations:["generation","token_count"],overall_timeout_ms:30000,max_attempts:1,
      targets:[{provider_id:$provider,provider_model:$model,priority:0,weight:1,timeout_ms:25000}]}')
  mutate POST /api/v1/route-drafts "$route_payload" 201 "canary-$provider-route-0001"
  local draft_id draft_etag
  draft_id=$(jq -er .id <<<"$LAST_BODY")
  draft_etag=$LAST_ETAG
  mutate POST "/api/v1/route-drafts/$draft_id/validate" '' 200 '' "$draft_etag"
  mutate POST "/api/v1/route-drafts/$draft_id/activate" '' 200 "canary-$provider-route-activate-0001" "$LAST_ETAG"
  printf '%s\n' "$route"
}

openai_route=$(configure_provider openai openai openai "$OLP_CANARY_OPENAI_MODEL" "$OLP_CANARY_OPENAI_API_KEY")
anthropic_route=$(configure_provider anthropic anthropic anthropic "$OLP_CANARY_ANTHROPIC_MODEL" "$OLP_CANARY_ANTHROPIC_API_KEY")
gemini_route=$(configure_provider gemini gemini gemini "$OLP_CANARY_GEMINI_MODEL" "$OLP_CANARY_GEMINI_API_KEY")
allowed=$(jq -cn --arg a "$openai_route" --arg b "$anthropic_route" --arg c "$gemini_route" \
  '{name:"Live canary",scopes:["inference","models_read"],allowed_routes:[$a,$b,$c]}')
mutate POST /api/v1/api-keys "$allowed" 201 canary-key-0001
api_key=$(jq -er .secret <<<"$LAST_BODY")
expected_generation=$(jq -er .runtime_generation.sequence <<<"$LAST_BODY")
authorization="Authorization: Bearer $api_key"
ready=
for _ in $(seq 1 120); do
  ready=$(curl -fsS http://127.0.0.1:29300/health/ready 2>/dev/null || true)
  [[ $(jq -r '.generation // 0' <<<"${ready:-null}") == "$expected_generation" ]] && break
  sleep 0.1
done
[[ $(jq -r '.generation // 0' <<<"${ready:-null}") == "$expected_generation" ]]

curl -fsS "$origin/openai/v1/models" -H "$authorization" \
  | jq -e --arg route "$openai_route" '.data | any(.id == $route)' >/dev/null
curl -fsS -X POST "$origin/openai/v1/chat/completions" -H "$authorization" -H 'Content-Type: application/json' \
  --data "$(jq -cn --arg model "$openai_route" '{model:$model,max_tokens:1,messages:[{role:"user",content:"Reply with one token."}]}')" \
  | jq -e '.choices[0].finish_reason != null' >/dev/null
curl -fsS -X POST "$origin/openai/v1/chat/completions" -H "$authorization" -H 'Content-Type: application/json' \
  --data "$(jq -cn --arg model "$openai_route" '{model:$model,max_tokens:1,stream:true,messages:[{role:"user",content:"Reply with one token."}]}')" \
  >"$work/openai.sse"
grep -F 'data: [DONE]' "$work/openai.sse" >/dev/null
curl -fsS -X POST "$origin/openai/v1/responses/input_tokens" -H "$authorization" -H 'Content-Type: application/json' \
  --data "$(jq -cn --arg model "$openai_route" '{model:$model,input:"count me"}')" \
  | jq -e '.input_tokens > 0' >/dev/null

curl -fsS "$origin/anthropic/v1/models" -H "$authorization" \
  | jq -e --arg route "$anthropic_route" '.data | any(.id == $route)' >/dev/null
curl -fsS -X POST "$origin/anthropic/v1/messages" -H "$authorization" -H 'anthropic-version: 2023-06-01' \
  -H 'Content-Type: application/json' \
  --data "$(jq -cn --arg model "$anthropic_route" '{model:$model,max_tokens:1,messages:[{role:"user",content:"Reply with one token."}]}')" \
  | jq -e '.stop_reason != null' >/dev/null
curl -fsS -X POST "$origin/anthropic/v1/messages" -H "$authorization" -H 'anthropic-version: 2023-06-01' \
  -H 'Content-Type: application/json' \
  --data "$(jq -cn --arg model "$anthropic_route" '{model:$model,max_tokens:1,stream:true,messages:[{role:"user",content:"Reply with one token."}]}')" \
  >"$work/anthropic.sse"
grep -F 'event: message_stop' "$work/anthropic.sse" >/dev/null
curl -fsS -X POST "$origin/anthropic/v1/messages/count_tokens" -H "$authorization" \
  -H 'anthropic-version: 2023-06-01' -H 'Content-Type: application/json' \
  --data "$(jq -cn --arg model "$anthropic_route" '{model:$model,messages:[{role:"user",content:"count me"}]}')" \
  | jq -e '.input_tokens > 0' >/dev/null

curl -fsS "$origin/gemini/v1beta/models" -H "$authorization" \
  | jq -e --arg route "models/$gemini_route" '.models | any(.name == $route)' >/dev/null
curl -fsS -X POST "$origin/gemini/v1beta/models/$gemini_route:generateContent" -H "$authorization" \
  -H 'Content-Type: application/json' --data '{"contents":[{"role":"user","parts":[{"text":"Reply with one token."}]}],"generationConfig":{"maxOutputTokens":1}}' \
  | jq -e '.candidates[0].finishReason != null' >/dev/null
curl -fsS -X POST "$origin/gemini/v1beta/models/$gemini_route:streamGenerateContent?alt=sse" -H "$authorization" \
  -H 'Content-Type: application/json' --data '{"contents":[{"role":"user","parts":[{"text":"Reply with one token."}]}],"generationConfig":{"maxOutputTokens":1}}' \
  >"$work/gemini.sse"
grep -F 'finishReason' "$work/gemini.sse" >/dev/null
curl -fsS -X POST "$origin/gemini/v1beta/models/$gemini_route:countTokens" -H "$authorization" \
  -H 'Content-Type: application/json' --data '{"contents":[{"role":"user","parts":[{"text":"count me"}]}]}' \
  | jq -e '.totalTokens > 0' >/dev/null

echo "OpenAI, Anthropic, and Gemini live production-connector canaries passed"
