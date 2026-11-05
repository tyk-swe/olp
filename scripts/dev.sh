#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
for tool in go node python3 docker; do
  command -v "$tool" >/dev/null || { printf 'Install %s, then run make setup\n' "$tool" >&2; exit 1; }
done
command -v "$(go env CC)" >/dev/null || { echo 'Install a C compiler and glibc development headers, then run make setup' >&2; exit 1; }
docker compose version >/dev/null || { echo 'Install Docker Compose, then rerun make dev' >&2; exit 1; }
for executable in .local/bin/air console/node_modules/.bin/vite; do
  test -x "$executable" || { echo 'Development dependencies are missing. Run make setup' >&2; exit 1; }
done
for generated in console/src/lib/management/schema.d.ts console/src/lib/management/requirements.ts; do
  test -f "$generated" || { echo 'Console types are missing. Run make setup' >&2; exit 1; }
done
docker compose -f deploy/compose.dev.yaml up -d --wait
export OLP_DATABASE_URL="postgres://olp:olp-local@127.0.0.1:${OLP_POSTGRES_PORT:-54321}/olp?sslmode=disable"
export OLP_CACHE_VALKEY_URL="redis://:olp-local@127.0.0.1:${OLP_CACHE_VALKEY_PORT:-63792}/0"
export OLP_VALKEY_URL="redis://:olp-local@127.0.0.1:${OLP_VALKEY_PORT:-63791}/0"
export OLP_LISTEN_ADDR=127.0.0.1:8082
export OLP_OBSERVABILITY_LISTEN_ADDR=127.0.0.1:9092
export OLP_PUBLIC_ORIGIN=http://127.0.0.1:5173
export OLP_DEV_API_ORIGIN=http://127.0.0.1:8082
export OLP_SHUTDOWN_TIMEOUT=2s
source scripts/secrets.sh "$PWD/.local/secrets"
echo 'Development console at http://127.0.0.1:5173'
echo "Bootstrap token file: $OLP_BOOTSTRAP_TOKEN_FILE"
cleanup() {
  trap - EXIT INT TERM
  kill -TERM "${watcher_pid:-}" "${console_pid:-}" 2>/dev/null || true
  wait "${watcher_pid:-}" "${console_pid:-}" 2>/dev/null || true
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
.local/bin/air -c .air.toml &
watcher_pid=$!
(
  cd console
  exec node_modules/.bin/vite --host 127.0.0.1 --port 5173 --strictPort
) &
console_pid=$!
wait -n "$watcher_pid" "$console_pid"
