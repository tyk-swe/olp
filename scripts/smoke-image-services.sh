#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
image=${1:?set the candidate image}
# shellcheck source=scripts/lib/disposable-database.sh
source scripts/lib/disposable-database.sh
# shellcheck source=scripts/lib/image-container.sh
source scripts/lib/image-container.sh
database="olp_image_${OLP_TEST_RUN_TOKEN}"
container=
cleanup() {
  local status=$?
  [[ -z $container ]] || docker rm -f "$container" >/dev/null 2>&1 || true
  drop_disposable_database "$database"
  exit "$status"
}
trap cleanup EXIT
create_disposable_database "$database"
export OLP_DATABASE_URL="$OLP_TEST_DATABASE_URL_PREFIX/$database"
olp_image_container_args
port=4185
observability_port=4187
args=("${olp_image_args[@]}"
  --env OLP_DATABASE_URL --env OLP_VALKEY_URL
  --env OLP_MASTER_KEY_FILE --env OLP_AUTH_HMAC_KEY_FILE --env OLP_BOOTSTRAP_TOKEN_FILE
  --env OLP_LISTEN_ADDR=127.0.0.1:$port --env OLP_OBSERVABILITY_LISTEN_ADDR=127.0.0.1:$observability_port
  --env OLP_PUBLIC_ORIGIN=http://localhost:$port)
docker run --rm "${args[@]}" "$image" migrate
docker run --rm "${args[@]}" "$image" doctor | jq -e '.ok == true' >/dev/null
for mode in all gateway control worker; do
  psql "$OLP_DATABASE_URL" -Xq -v ON_ERROR_STOP=1 -c 'DELETE FROM olp_v3.worker_task_health' >/dev/null
  container=$(docker run -d "${args[@]}" "$image" "$mode")
  ready=false
  for _ in {1..30}; do
    [[ $(docker inspect "$container" --format '{{.State.Running}}') == true ]] || break
    if [[ $mode == worker ]]; then
      checkpoint=$(psql "$OLP_DATABASE_URL" -XAt -v ON_ERROR_STOP=1 -c 'SELECT EXISTS (SELECT 1 FROM olp_v3.worker_task_health)')
      [[ $checkpoint != t ]] || { ready=true; break; }
    elif curl --fail --silent --max-time 1 http://127.0.0.1:$observability_port/health/live >/dev/null; then
      ready=true
      break
    fi
    sleep 1
  done
  [[ $ready == true ]] || { echo "Candidate $mode did not become live." >&2; exit 1; }
  case "$mode" in
    all|control)
      curl --fail --silent http://127.0.0.1:$port/ | grep -qi '<html\|<!doctype html'
      [[ $(curl --silent -o /dev/null -w '%{http_code}' http://127.0.0.1:$port/_app/missing.js) == 404 ]]
      ;;
    gateway)
      [[ $(curl --silent -o /dev/null -w '%{http_code}' http://127.0.0.1:$port/api/v3/setup) == 404 ]]
      [[ $(curl --silent -o /dev/null -w '%{http_code}' http://127.0.0.1:$observability_port/health/ready) == 503 ]]
      ;;
    worker)
      if curl --fail --silent --max-time 1 http://127.0.0.1:$observability_port/health/live >/dev/null; then
        echo 'Worker unexpectedly exposed an HTTP listener.' >&2
        exit 1
      fi
      ;;
  esac
  docker stop --time 30 "$container" >/dev/null
  [[ $(docker inspect "$container" --format '{{.State.ExitCode}}') == 0 ]] || { echo "Candidate $mode shutdown failed." >&2; exit 1; }
  docker rm "$container" >/dev/null
  container=
  echo "Candidate service mode passed: $mode"
done
