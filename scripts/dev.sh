#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
for tool in go node python3 docker; do
  command -v "$tool" >/dev/null || { printf 'Install %s, then run make setup\n' "$tool" >&2; exit 1; }
done
read -r cc _ <<<"$(go env CC)"
command -v "$cc" >/dev/null || { echo 'Install a C compiler and glibc development headers, then run make setup' >&2; exit 1; }
docker compose version >/dev/null || { echo 'Install Docker Compose, then rerun make dev' >&2; exit 1; }
for executable in .local/bin/air console/node_modules/.bin/vite; do
  test -x "$executable" || { echo 'Development dependencies are missing. Run make setup' >&2; exit 1; }
done
for generated in console/src/lib/management/schema.d.ts console/src/lib/management/requirements.ts; do
  test -f "$generated" || { echo 'Console types are missing. Run make setup' >&2; exit 1; }
done
compose=(docker compose -f deploy/compose.dev.yaml)
"${compose[@]}" up -d --wait
postgres_addr=$("${compose[@]}" port postgres 5432 | head -n 1)
valkey_addr=$("${compose[@]}" port valkey 6379 | head -n 1)
cache_addr=$("${compose[@]}" port cache 6379 | head -n 1)
export OLP_DATABASE_URL="postgres://olp:olp-local@$postgres_addr/olp?sslmode=disable"
export OLP_VALKEY_URL="redis://:olp-local@$valkey_addr/0"
export OLP_CACHE_VALKEY_URL="redis://:olp-local@$cache_addr/0"
export OLP_LISTEN_ADDR=127.0.0.1:8082
export OLP_OBSERVABILITY_LISTEN_ADDR=127.0.0.1:9092
export OLP_PUBLIC_ORIGIN=http://127.0.0.1:5173
export OLP_DEV_API_ORIGIN=http://127.0.0.1:8082
export OLP_SHUTDOWN_TIMEOUT=2s
# shellcheck source=scripts/secrets.sh
source scripts/secrets.sh "$PWD/.local/secrets"
echo 'Development console at http://127.0.0.1:5173'
echo "Bootstrap token file: $OLP_BOOTSTRAP_TOKEN_FILE"
pids=()
cleanup() {
  local status=$?
  trap - EXIT INT TERM
  if [[ ${#pids[@]} -gt 0 ]]; then
    kill -TERM "${pids[@]}" 2>/dev/null || true
    wait "${pids[@]}" 2>/dev/null || true
  fi
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
.local/bin/air -c .air.toml &
watcher_pid=$!
pids+=("$watcher_pid")
(
  cd console
  exec node_modules/.bin/vite --host 127.0.0.1 --port 5173 --strictPort
) &
console_pid=$!
pids+=("$console_pid")
wait -n "$watcher_pid" "$console_pid"
