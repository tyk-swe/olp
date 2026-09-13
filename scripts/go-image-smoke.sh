#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
image=${1:?usage: go-image-smoke.sh image [amd64|arm64]}
expected_arch=${2:-$(docker info --format '{{.Architecture}}')}
case "$expected_arch" in x86_64) expected_arch=amd64 ;; aarch64) expected_arch=arm64 ;; esac
actual_arch=$(docker image inspect --format '{{.Architecture}}' "$image")
[[ $actual_arch == "$expected_arch" ]] || { echo "image architecture mismatch: $actual_arch" >&2; exit 1; }
host_arch=$(uname -m)
case "$expected_arch:$host_arch" in amd64:x86_64|arm64:aarch64|arm64:arm64) ;; *) echo 'Native architecture qualification requires a matching host; emulation does not qualify' >&2; exit 1 ;; esac
project="olp-go-image-$$-$RANDOM"
export OLP_GO_POSTGRES_PORT=0 OLP_GO_VALKEY_PORT=0
compose=(docker compose -p "$project" -f deploy/compose.go.yaml)
containers=()
scratch=$(mktemp -d)
chmod 755 "$scratch"
source scripts/go-secrets.sh "$scratch/secrets"
chmod 755 "$scratch/secrets"
cleanup() {
  status=$?
  trap - EXIT INT TERM
  for container in "${containers[@]}"; do
    if (( status != 0 )); then docker logs "$container" >&2 || true; fi
    docker rm -f "$container" >/dev/null 2>&1 || true
  done
  "${compose[@]}" down -v --remove-orphans >&2 || true
  rm -rf -- "$scratch"
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
"${compose[@]}" up -d --wait --wait-timeout 90
[[ $(docker image inspect --format '{{.Config.User}}' "$image") == '65532:65532' ]]
# Use the already pulled PostgreSQL image only to assign disposable volume ownership.
docker run --rm --user 0 -v "$scratch/secrets:/secrets" postgres:18 chown 65532:65532 /secrets/auth.key /secrets/master.json /secrets/bootstrap.token
secret_args=(-v "$scratch/secrets:/secrets:ro"
  -e OLP_AUTH_HMAC_KEY_FILE=/secrets/auth.key
  -e OLP_MASTER_KEY_FILE=/secrets/master.json
  -e OLP_BOOTSTRAP_TOKEN_FILE=/secrets/bootstrap.token)
docker run --rm --network "${project}_default" --read-only --cap-drop=ALL --security-opt=no-new-privileges \
  -e OLP_DATABASE_URL='postgres://olp_go:olp-go-local@postgres/olp_go?sslmode=disable' "$image" migrate
for mode in all gateway control worker; do
  container="$project-$mode"
  containers+=("$container")
  docker run -d --name "$container" --network "${project}_default" --read-only --cap-drop=ALL --security-opt=no-new-privileges \
    -p 127.0.0.1::8080 -p 127.0.0.1::9090 \
    -e OLP_DATABASE_URL='postgres://olp_go:olp-go-local@postgres/olp_go?sslmode=disable' \
    "${secret_args[@]}" -e OLP_VALKEY_URL='redis://:olp-go-local@valkey/0' "$image" "$mode" >/dev/null
  ready=false
  for _ in {1..100}; do
    if docker exec "$container" /usr/local/bin/olp health-probe >/dev/null 2>&1; then ready=true; break; fi
    [[ $(docker inspect --format '{{.State.Running}}' "$container") == true ]] || break
    sleep 0.1
  done
  [[ $ready == true ]] || { echo "$mode did not become ready" >&2; exit 1; }
  public=$(docker port "$container" 8080/tcp)
  private=$(docker port "$container" 9090/tcp)
  curl --fail --silent "http://$private/health/live" >/dev/null
  if [[ $mode == all || $mode == control ]]; then
    curl --fail --silent "http://$public/api/v3/openapi.json" | cmp - openapi/management.json
    curl --fail --silent "http://$public/login" | rg -q 'svelte-root'
  fi
  if [[ $mode != worker ]]; then
    [[ $(curl --silent --output /dev/null --write-out '%{http_code}' "http://$public/health/ready") == 404 ]]
  else
    if curl --silent --max-time 1 "http://$public/" >/dev/null; then echo 'worker exposed a public listener' >&2; exit 1; fi
  fi
  docker stop --time 8 "$container" >/dev/null
  [[ $(docker inspect --format '{{.State.ExitCode}}' "$container") == 0 ]]
  echo "qualified $expected_arch $mode: dependencies, private health, listener ownership, shutdown"
done
docker image inspect --format '{{json .}}' "$image"
