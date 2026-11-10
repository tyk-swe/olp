# shellcheck shell=bash

report_budget() {
  local name=$1 budget=$2 ran=$3 wall=$4
  printf '%s: ran %ss of a %ss budget (%ss wall with builds)\n' "$name" "$ran" "$budget" "$wall"
  if awk -v ran="$ran" -v budget="$budget" 'BEGIN { exit !(ran > budget) }'; then
    printf '%s: exceeded its %ss run budget\n' "$name" "$budget" >&2
    return 1
  fi
}

go_suite() {
  local name=$1 budget=$2
  shift 2
  local log status=0 began=$SECONDS
  log=$(mktemp)
  go test -mod=readonly -count=1 -shuffle=on "$@" 2>&1 |
    grep --line-buffered -v '\[no test files\]$' | tee "$log" || status=$?
  local package_seconds
  package_seconds=$(awk '($1 == "ok" || $1 == "FAIL") && $3 ~ /^[0-9.]+s$/ { sum += $3 } END { printf "%.1f", sum }' "$log")
  rm -f -- "$log"
  [ "$status" -eq 0 ] || return "$status"
  report_budget "$name" "$budget" "$package_seconds" "$((SECONDS - began))"
}

timed_suite() {
  local name=$1 budget=$2
  shift 2
  local began=$SECONDS
  "$@" || return
  local ran=$((SECONDS - began))
  report_budget "$name" "$budget" "$ran" "$ran"
}

start_services() {
  test_scratch=$(mktemp -d)
  test_processes=()
  test_compose=(docker compose -p "olp-test-$$" -f deploy/compose.dev.yaml)
  export OLP_POSTGRES_PORT=0 OLP_VALKEY_PORT=0 OLP_CACHE_VALKEY_PORT=0 OLP_POSTGRES_MAX_CONNECTIONS=400
  trap stop_services EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM
  "${test_compose[@]}" up -d --wait --wait-timeout 90 postgres valkey
  local postgres valkey
  postgres=$("${test_compose[@]}" port postgres 5432)
  valkey=$("${test_compose[@]}" port valkey 6379)
  export OLP_TEST_DATABASE_URL="postgres://olp:olp-local@$postgres/olp?sslmode=disable"
  export OLP_TEST_VALKEY_URL="redis://:olp-local@$valkey/0"
}

build_binary() {
  CGO_ENABLED=1 go build -mod=readonly -trimpath -o "$test_scratch/olp" ./cmd/olp
  export OLP_TEST_BINARY="$test_scratch/olp"
}

free_port() {
  python3 -c 'import socket; s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1])'
}

start_console_stack() {
  go build -mod=readonly -trimpath -o "$test_scratch/vendor" ./tests/harness/cmd/vendor
  pnpm --dir console build >"$test_scratch/console-build.log" 2>&1 || {
    cat "$test_scratch/console-build.log" >&2
    return 1
  }
  # shellcheck source=scripts/secrets.sh
  source scripts/secrets.sh "$test_scratch/secrets"
  local public private vendor
  public=127.0.0.1:$(free_port)
  private=127.0.0.1:$(free_port)
  vendor=127.0.0.1:$(free_port)
  export OLP_DATABASE_URL=$OLP_TEST_DATABASE_URL OLP_VALKEY_URL=$OLP_TEST_VALKEY_URL
  export OLP_LISTEN_ADDR=$public OLP_OBSERVABILITY_LISTEN_ADDR=$private OLP_PUBLIC_ORIGIN=http://$public
  export OLP_CONSOLE_DIR=console/build OLP_MEDIA_SPOOL_DIR=$test_scratch/spool
  export OLP_PROVIDER_EGRESS_ALLOW_CIDRS=127.0.0.0/8 OLP_PROVIDER_EGRESS_ALLOW_HTTP_HOSTS=127.0.0.1
  export OLP_SHUTDOWN_TIMEOUT=2s OLP_LOG_LEVEL=warn
  "$OLP_TEST_BINARY" migrate >/dev/null
  "$OLP_TEST_BINARY" all >"$test_scratch/olp.log" 2>&1 &
  test_processes+=($!)
  "$test_scratch/vendor" -listen "$vendor" &
  test_processes+=($!)
  curl --silent --fail --output /dev/null --retry 60 --retry-delay 1 --retry-connrefused "http://$private/health/live"
  export OLP_E2E_ORIGIN=http://$public OLP_E2E_VENDOR=http://$vendor
  OLP_E2E_SETUP_TOKEN=$(<"$OLP_BOOTSTRAP_TOKEN_FILE")
  export OLP_E2E_SETUP_TOKEN
}

stop_services() {
  local status=$?
  trap - EXIT INT TERM
  [ "$status" -eq 0 ] || [ ! -s "$test_scratch/olp.log" ] || tail -n 40 "$test_scratch/olp.log" >&2
  [ "${#test_processes[@]}" -eq 0 ] || kill "${test_processes[@]}" 2>/dev/null || true
  "${test_compose[@]}" down --volumes --remove-orphans >/dev/null 2>&1 || true
  rm -rf -- "$test_scratch"
  exit "$status"
}
