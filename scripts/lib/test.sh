# shellcheck shell=bash

report_budget() {
  local name=$1 budget=$2 ran=$3 wall=$4
  printf '%s: ran %ss of a %ss budget (%ss wall with builds)\n' "$name" "$ran" "$budget" "$wall" |
    tee -a "${GITHUB_STEP_SUMMARY:-/dev/null}"
  if awk -v ran="$ran" -v budget="$budget" 'BEGIN { exit !(ran > budget) }'; then
    printf '%s: exceeded its %ss run budget\n' "$name" "$budget" >&2
    return 1
  fi
}

take_test_slot() {
  local name=$1
  exec 9>"/tmp/olp-test-slot-$(id -u)-${name// /-}.lock"
  flock --nonblock 9 && return
  printf '%s: waiting for another checkout to finish this suite\n' "$name" >&2
  flock 9
}

release_test_slot() {
  exec 9>&-
}

go_suite() {
  local name=$1 budget=$2
  shift 2
  take_test_slot "$name"
  local log status=0 began=$SECONDS
  log=$(mktemp)
  test_temp_files+=("$log")
  go test -mod=readonly -count=1 -shuffle=on "$@" 2>&1 9>&- |
    awk '!/\[no test files\]$/ { print; fflush() }' 9>&- | tee "$log" 9>&- || status=$?
  release_test_slot
  local package_seconds
  package_seconds=$(awk '($1 == "ok" || $1 == "FAIL") && $3 ~ /^[0-9.]+s$/ { sum += $3 } END { printf "%.1f", sum }' "$log")
  rm -f -- "$log"
  [ "$status" -eq 0 ] || return "$status"
  report_budget "$name" "$budget" "$package_seconds" "$((SECONDS - began))"
}

timed_suite() {
  local name=$1 budget=$2
  shift 2
  take_test_slot "$name"
  local status=0 began=$SECONDS
  "$@" 9>&- || status=$?
  release_test_slot
  [ "$status" -eq 0 ] || return "$status"
  local ran=$((SECONDS - began))
  report_budget "$name" "$budget" "$ran" "$ran"
}

test_temp_files=()

cleanup_temp_files() {
  local status=$?
  trap - EXIT INT TERM
  if [[ ${#test_temp_files[@]} -gt 0 ]]; then
    rm -f -- "${test_temp_files[@]}"
  fi
  exit "$status"
}

trap cleanup_temp_files EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

start_services() {
  test_scratch=$(mktemp -d)
  test_processes=()
  test_compose=(docker compose -p "olp-test-$$" -f deploy/compose.dev.yaml -f deploy/compose.test.yaml)
  export OLP_POSTGRES_PORT=0 OLP_VALKEY_PORT=0
  trap stop_services EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM
  "${test_compose[@]}" up -d --wait --wait-timeout 90 postgres valkey
  local postgres valkey
  postgres=$("${test_compose[@]}" port postgres 5432 | head -n 1)
  valkey=$("${test_compose[@]}" port valkey 6379 | head -n 1)
  export OLP_TEST_DATABASE_URL="postgres://olp:olp-local@$postgres/olp?sslmode=disable"
  export OLP_TEST_VALKEY_URL="redis://:olp-local@$valkey/0"
}

build_binary() {
  local version
  version=$(jq -er .version package.json)
  CGO_ENABLED=1 go build -mod=readonly -trimpath -ldflags="-X main.Version=$version" -o "$test_scratch/olp" ./cmd/olp
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
  local public private vendor olp_pid vendor_pid
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
  olp_pid=$!
  test_processes+=("$olp_pid")
  "$test_scratch/vendor" -listen "$vendor" >"$test_scratch/vendor.log" 2>&1 &
  vendor_pid=$!
  test_processes+=("$vendor_pid")
  local ready=false
  for _ in {1..120}; do
    if ! kill -0 "$olp_pid" 2>/dev/null; then
      echo 'olp exited before becoming live' >&2
      return 1
    fi
    if ! kill -0 "$vendor_pid" 2>/dev/null; then
      echo 'fixture vendor exited before becoming ready' >&2
      return 1
    fi
    if curl --silent --fail --output /dev/null "http://$private/health/live"; then
      ready=true
      break
    fi
    sleep 0.5
  done
  [[ $ready == true ]] || { echo 'olp did not become live in time' >&2; return 1; }
  export OLP_E2E_ORIGIN=http://$public OLP_E2E_VENDOR=http://$vendor
  OLP_E2E_SETUP_TOKEN=$(<"$OLP_BOOTSTRAP_TOKEN_FILE")
  export OLP_E2E_SETUP_TOKEN
}

stop_services() {
  local status=$?
  trap - EXIT INT TERM
  if [[ $status -ne 0 ]]; then
    [[ ! -s ${test_scratch:-}/olp.log ]] || tail -n 40 "$test_scratch/olp.log" >&2
    [[ ! -s ${test_scratch:-}/vendor.log ]] || tail -n 40 "$test_scratch/vendor.log" >&2
  fi
  if [[ ${#test_processes[@]} -gt 0 ]]; then
    kill "${test_processes[@]}" 2>/dev/null || true
    wait "${test_processes[@]}" 2>/dev/null || true
  fi
  if [[ ${#test_temp_files[@]} -gt 0 ]]; then
    rm -f -- "${test_temp_files[@]}"
  fi
  "${test_compose[@]}" down --volumes --remove-orphans >/dev/null 2>&1 || true
  rm -rf -- "${test_scratch:-}"
  exit "$status"
}
