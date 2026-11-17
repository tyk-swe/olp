#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
export OLP_IMAGE=${1:?usage: smoke-image-compose.sh image} OLP_HOST_PORT=0 OLP_PUBLIC_ORIGIN=http://olp.test
compose=(docker compose -p "olp-compose-$$-$RANDOM" -f deploy/compose.yaml)
cleanup() {
  status=$?
  trap - EXIT INT TERM
  if (( status != 0 )); then "${compose[@]}" logs >&2 || true; fi
  "${compose[@]}" down -v --remove-orphans >&2 || true
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
setup_required() {
  for _ in {1..120}; do
    if body=$(curl --fail --silent "http://$("${compose[@]}" port olp 8080)/api/v1/setup/status"); then
      python3 -c 'import json, sys; print(json.loads(sys.argv[1])["setup_required"])' "$body"
      return
    fi
    sleep 0.5
  done
  echo 'control API did not answer' >&2
  return 1
}
"${compose[@]}" up -d
[[ $(setup_required) == True ]]
token=$("${compose[@]}" exec -T olp olp setup-token | tail -n 1)
status=$(curl --silent --output /dev/null --write-out '%{http_code}' -X POST \
  "http://$("${compose[@]}" port olp 8080)/api/v1/setup" \
  -H 'Content-Type: application/json' -H "Origin: $OLP_PUBLIC_ORIGIN" -H "X-OLP-Setup-Token: $token" \
  -d '{"email":"owner@example.test","display_name":"Owner","password":"a long compose smoke password","installation_name":"Compose smoke"}')
[[ $status == 201 ]] || { echo "setup returned $status" >&2; exit 1; }
"${compose[@]}" up -d --force-recreate
[[ $(setup_required) == False ]]
echo 'Compose quickstart created an owner and kept it across a restart'
