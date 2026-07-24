#!/usr/bin/env bash
set -euo pipefail

if [[ ${1:-} == --help || ${1:-} == -h ]]; then
  echo "usage: tests/qualification/compose-clean-install.sh" >&2
  exit 0
fi
[[ $# -eq 0 ]] || exit 2
for command in docker curl jq openssl; do
  command -v "$command" >/dev/null || { echo "required command is unavailable: $command" >&2; exit 1; }
done
docker compose version >/dev/null
docker info >/dev/null

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
work=$(mktemp -d)
project="olp-qualification-$$"
port=${OLP_QUALIFICATION_COMPOSE_PORT:-28080}
image=${OLP_QUALIFICATION_IMAGE:-openllmproxy:qualification}
compose=(docker compose --project-name "$project" --file "$root/deploy/compose.yaml")
cleanup() {
  local status=$?
  trap - EXIT INT TERM
  if (( status != 0 )); then
    "${compose[@]}" --file "$work/runtime.yaml" ps >&2 || true
    "${compose[@]}" --file "$work/runtime.yaml" logs --no-color --tail 100 >&2 || true
  fi
  "${compose[@]}" --file "$work/runtime.yaml" down --volumes --remove-orphans \
    >"$work/teardown.log" 2>&1 || {
      cat "$work/teardown.log" >&2
      (( status == 0 )) && status=1
    }
  rm -rf "$work"
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

mkdir "$work/secrets"
chmod 700 "$work/secrets"
for name in master auth bootstrap; do
  openssl rand -base64 32 >"$work/secrets/$name"
  chmod 600 "$work/secrets/$name"
done
cat >"$work/runtime.yaml" <<EOF
services:
  migrate:
    image: $image
    build: !reset null
  olp:
    image: $image
    build: !reset null
    environment:
      OLP_PUBLIC_ORIGIN: http://127.0.0.1:$port
secrets:
  olp_master_key:
    file: $work/secrets/master
  olp_auth_hmac_key:
    file: $work/secrets/auth
EOF
cat >"$work/bootstrap-secret.yaml" <<EOF
secrets:
  olp_bootstrap_token:
    file: $work/secrets/bootstrap
EOF

if [[ -z ${OLP_QUALIFICATION_IMAGE:-} ]]; then
  docker build --file "$root/deploy/Dockerfile" --tag "$image" "$root"
fi
OLP_UID=$(id -u)
OLP_GID=$(id -g)
export OLP_HOST_PORT=$port OLP_UID OLP_GID
compose_config="$work/compose.config.json"
"${compose[@]}" --file "$root/deploy/compose.bootstrap.yaml" \
  --file "$work/runtime.yaml" --file "$work/bootstrap-secret.yaml" \
  config --format json >"$compose_config"
jq -e --arg image "$image" '
  def qualified_olp_service($name):
    (.services[$name].image == $image)
    and ((.services[$name].build // null) == null);
  qualified_olp_service("migrate") and qualified_olp_service("olp")
' "$compose_config" >/dev/null || {
  echo "Compose clean install must use the qualified image without build settings for migrate and olp" >&2
  jq '.services | {migrate, olp}' "$compose_config" >&2
  exit 1
}
"${compose[@]}" --file "$root/deploy/compose.bootstrap.yaml" \
  --file "$work/runtime.yaml" --file "$work/bootstrap-secret.yaml" up \
  --no-build --detach --wait --wait-timeout 180
"${compose[@]}" --file "$work/runtime.yaml" run --rm migrate

origin="http://127.0.0.1:$port"
for _ in {1..60}; do
  curl -fsS "$origin/readyz" >/dev/null 2>&1 && break
  sleep 1
done
curl -fsS "$origin/readyz" >/dev/null
setup_headers="$work/setup.headers"
setup_body="$work/setup.json"
status=$(curl -sS -D "$setup_headers" -o "$setup_body" -w '%{http_code}' \
  -X POST "$origin/api/v1/setup" -H "Origin: $origin" \
  -H "X-OLP-Setup-Token: $(<"$work/secrets/bootstrap")" \
  -H 'Content-Type: application/json' \
  --data '{"email":"owner@qualification.test","password":"correct horse battery staple","display_name":"Qualification Owner","installation_name":"Compose qualification"}')
[[ $status == 201 ]] || { cat "$setup_body" >&2; exit 1; }
csrf=$(jq -er .csrf_token "$setup_body")
session_cookie=$(awk 'BEGIN{IGNORECASE=1} /^set-cookie: __Host-olp_session=/{sub(/^set-cookie: /,""); sub(/;.*/,""); gsub("\r",""); print}' "$setup_headers")
csrf_cookie=$(awk 'BEGIN{IGNORECASE=1} /^set-cookie: __Host-olp_csrf=/{sub(/^set-cookie: /,""); sub(/;.*/,""); gsub("\r",""); print}' "$setup_headers")
cookies="$session_cookie; $csrf_cookie"

"${compose[@]}" --file "$work/runtime.yaml" up --no-build --detach --force-recreate --wait --wait-timeout 120 olp
rm -f "$work/secrets/bootstrap"
[[ ! -e $work/secrets/bootstrap ]]
for _ in {1..30}; do
  curl -fsS "$origin/readyz" >/dev/null 2>&1 && break
  sleep 1
done
curl -fsS "$origin/readyz" >/dev/null

status=$(curl -sS -o "$work/session.json" -w '%{http_code}' \
  "$origin/api/v1/sessions/current" -H "Origin: $origin" -H "Cookie: $cookies")
[[ $status == 200 ]] || { cat "$work/session.json" >&2; exit 1; }
jq -e '.user.email == "owner@qualification.test"' "$work/session.json" >/dev/null

status=$(curl -sS -o "$work/key.json" -w '%{http_code}' -X POST \
  "$origin/api/v1/api-keys" -H "Origin: $origin" -H "Cookie: $cookies" \
  -H "x-csrf-token: $csrf" -H 'Idempotency-Key: compose-qualification-key-0001' \
  -H 'Content-Type: application/json' \
  --data '{"name":"Compose qualification","scopes":["models_read"]}')
[[ $status == 201 ]] || { cat "$work/key.json" >&2; exit 1; }
api_key=$(jq -er .secret "$work/key.json")
generation=$(jq -er .runtime_generation.sequence "$work/key.json")
status=000
for _ in {1..50}; do
  status=$(curl -sS -o "$work/models.json" -w '%{http_code}' \
    "$origin/openai/v1/models" -H "Authorization: Bearer $api_key")
  [[ $status == 200 ]] && break
  sleep 0.1
done
[[ $status == 200 ]] || { cat "$work/models.json" >&2; exit 1; }

echo "Compose clean install passed: bootstrap retired, session and API key valid, generation=$generation"
