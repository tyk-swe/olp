#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
export OLP_IMAGE=${1:?usage: smoke-image-compose.sh image} OLP_HOST_PORT=0 OLP_PUBLIC_ORIGIN=http://olp.test
compose=(docker compose -p "olp-compose-$$-$RANDOM" -f deploy/compose.yaml)
cleanup() {
  local status=$?
  trap - EXIT INT TERM
  if (( status != 0 )); then "${compose[@]}" logs >&2 || true; fi
  "${compose[@]}" down -v --remove-orphans >&2 || true
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
published_addr() {
  local addr
  addr=$("${compose[@]}" port olp 8080 | head -n 1)
  printf '%s\n' "${addr/#0.0.0.0:/127.0.0.1:}"
}
setup_required() {
  local body addr
  for _ in {1..120}; do
    addr=$(published_addr 2>/dev/null || true)
    if [[ -n $addr ]] && body=$(curl --fail --silent "http://$addr/api/v1/setup/status"); then
      jq -r '.setup_required' <<<"$body"
      return
    fi
    sleep 0.5
  done
  echo 'control API did not answer' >&2
  return 1
}
"${compose[@]}" up -d
[[ $(setup_required) == true ]]
token=$("${compose[@]}" exec -T olp olp setup-token | tail -n 1)
addr=$(published_addr)
status=$(curl --silent --output /dev/null --write-out '%{http_code}' -X POST \
  "http://$addr/api/v1/setup" \
  -H 'Content-Type: application/json' -H "Origin: $OLP_PUBLIC_ORIGIN" -H "X-OLP-Setup-Token: $token" \
  -d '{"email":"owner@example.test","display_name":"Owner","password":"a long compose smoke password","installation_name":"Compose smoke"}' || echo 000)
[[ $status == 201 ]] || { echo "setup returned $status" >&2; exit 1; }
OLP_DATABASE_URL='postgres://olp@postgres:5432/olp?sslmode=disable' "${compose[@]}" up -d --force-recreate
[[ $(setup_required) == false ]]
echo 'Compose quickstart created an owner and kept it across a restart with options in the bundled database URL'
