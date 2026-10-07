#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."

# Runs the gateway benchmark scenarios (docs/roadmap/m01-measured-advantage.md)
# against a disposable PostgreSQL and Valkey, and writes one result per scenario
# to .local/bench/<scenario>.json. See tests/bench/README.md.
usage() {
  cat <<'EOF' >&2
usage: scripts/bench.sh

  BENCH_SCENARIOS            comma-separated scenarios to run, from S1 to S6 and S1-shadow (default: all)
  BENCH_SERVICES             compose (default) starts disposable PostgreSQL and Valkey;
                             external uses the ones OLP_TEST_DATABASE_URL and OLP_TEST_VALKEY_URL
                             name, whose user may create databases

  OLP_BENCH_SCALE            multiplies every rate and concurrency; 1 is the roadmap's full rates (default 1)
  OLP_BENCH_DURATION         measured period of each run (default 60s)
  OLP_BENCH_WARMUP           warmup before it, at the same rate (default 10s)
  OLP_BENCH_GATEWAY_CPUS     CPUs to pin the gateway to, as taskset lists them (S1's target needs exactly two)
  OLP_BENCH_MOCK_CPUS        CPUs for the mock upstream
  OLP_BENCH_LOADGEN_CPUS     CPUs for the load generator
  OLP_BENCH_ENFORCE          1 fails the run on a missed target; needs full scale and pinned CPUs
  OLP_BENCH_OUT              result directory (default .local/bench)

  OLP_TEST_BINARY            benchmark this olp binary instead of building one from source
  OLP_BENCH_MOCK_BINARY      use this mock upstream instead of building one

Scenarios are the roadmap's, and the other OLP_BENCH_ variables are in tests/bench/README.md.
EOF
  exit "${1:-2}"
}
[[ $# -eq 0 ]] || { [[ $1 == -h || $1 == --help ]] && usage 0; usage; }

# Validate the scenario list before it becomes a regular expression.
scenarios=${BENCH_SCENARIOS:-S1,S1-shadow,S2,S3,S4,S5,S6}
selected=()
IFS=',' read -r -a requested <<< "$scenarios"
for scenario in "${requested[@]}"; do
  [[ $scenario =~ ^(S[1-6]|S1-shadow)$ ]] || { echo "BENCH_SCENARIOS must list scenarios from S1 to S6 or S1-shadow, got '$scenario'" >&2; exit 2; }
  # A scenario's test is named without the hyphen: S1-shadow runs TestScenarioS1Shadow.
  test=${scenario/-shadow/Shadow}
  [[ " ${selected[*]:-} " == *" $test "* ]] || selected+=("$test")
done
run_pattern="^TestScenario($(IFS='|'; echo "${selected[*]}"))\$"
services=${BENCH_SERVICES:-compose}
[[ $services == compose || $services == external ]] || { echo "BENCH_SERVICES must be compose or external" >&2; exit 2; }

# Seconds in a duration such as 90s, 1m30s or 2h, so the test timeout can
# follow what the scenarios will take.
seconds() {
  local total=0
  [[ $1 =~ ^([0-9]+h)?([0-9]+m)?([0-9]+s)?$ && -n $1 ]] || return 1
  if [[ -n ${BASH_REMATCH[1]} ]]; then total=$(( total + ${BASH_REMATCH[1]%h} * 3600 )); fi
  if [[ -n ${BASH_REMATCH[2]} ]]; then total=$(( total + ${BASH_REMATCH[2]%m} * 60 )); fi
  if [[ -n ${BASH_REMATCH[3]} ]]; then total=$(( total + ${BASH_REMATCH[3]%s} )); fi
  echo "$total"
}
duration=$(seconds "${OLP_BENCH_DURATION:-60s}") && warmup=$(seconds "${OLP_BENCH_WARMUP:-10s}") || {
  echo "OLP_BENCH_DURATION and OLP_BENCH_WARMUP must be durations in whole seconds, such as 90s, 1m30s or 2h" >&2
  exit 2
}
# Each scenario applies its load twice and then waits for metadata and shuts
# down; S3 also waits up to a minute for its budget to be installed, and S6
# holds its streams for a period as long as the run. The wait for metadata lasts
# as long as it keeps arriving, and one pipeline consumer persists it, a page at
# a time: S3's 3,000 requests a second for the whole run, at a pace as low as
# fifty events a second, is the longest it can take. The rest is margin.
drain=$(awk -v scale="${OLP_BENCH_SCALE:-1}" -v seconds=$(( warmup + duration )) 'BEGIN { printf "%d", 3000 * scale * seconds / 50 + 1 }')
per_scenario=$(( 4 * (warmup + duration) + 1800 + drain ))
timeout=$(( ${#selected[@]} * per_scenario ))s

# A descriptor limit of 1,024 would stop the benchmark long before S6's ten
# thousand streams do.
ulimit -n "$(ulimit -Hn)" 2>/dev/null || true

scratch=$(mktemp -d)
compose=()
cleanup() {
  status=$?
  trap - EXIT INT TERM
  if (( ${#compose[@]} > 0 )); then
    if (( status != 0 )); then "${compose[@]}" logs --no-color >&2 || true; fi
    "${compose[@]}" down -v --remove-orphans >&2 || true
  fi
  rm -rf -- "$scratch"
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

if [[ $services == compose ]]; then
  export OLP_POSTGRES_PORT=0 OLP_VALKEY_PORT=0
  project="olp-bench-$$-$RANDOM"
  compose=(docker compose -p "$project" -f deploy/compose.dev.yaml)
  # Registry pulls flake often enough to fail the run before it starts.
  for attempt in {1..3}; do
    if "${compose[@]}" pull --quiet; then break; fi
    if (( attempt == 3 )); then exit 1; fi
    sleep $((attempt * 10))
  done
  "${compose[@]}" up -d --wait --wait-timeout 90
  postgres=$("${compose[@]}" port postgres 5432)
  valkey=$("${compose[@]}" port valkey 6379)
  export OLP_TEST_DATABASE_URL="postgres://olp:olp-local@$postgres/olp?sslmode=disable"
  export OLP_TEST_VALKEY_URL="redis://:olp-local@$valkey/0"
else
  : "${OLP_TEST_DATABASE_URL:?BENCH_SERVICES=external needs OLP_TEST_DATABASE_URL}"
  : "${OLP_TEST_VALKEY_URL:?BENCH_SERVICES=external needs OLP_TEST_VALKEY_URL}"
fi

# The binary under test is the release build, with nothing linked into it. A
# caller that already has one, such as release qualification, passes it in. The
# tests run in their package's directory, so a relative path, which means the
# repository root to the caller, is made absolute here, as OLP_BENCH_OUT is.
if [[ -z ${OLP_TEST_BINARY:-} ]]; then
  make build-go
  OLP_TEST_BINARY="$PWD/.local/bin/olp"
fi
[[ $OLP_TEST_BINARY == /* ]] || OLP_TEST_BINARY="$PWD/$OLP_TEST_BINARY"
export OLP_TEST_BINARY
if [[ -z ${OLP_BENCH_MOCK_BINARY:-} ]]; then
  go build -mod=readonly -trimpath -tags bench -o .local/bin/mockupstream ./tests/bench/cmd/mockupstream
  OLP_BENCH_MOCK_BINARY="$PWD/.local/bin/mockupstream"
fi
[[ $OLP_BENCH_MOCK_BINARY == /* ]] || OLP_BENCH_MOCK_BINARY="$PWD/$OLP_BENCH_MOCK_BINARY"
export OLP_BENCH_MOCK_BINARY

# A scenario migrates a database of its own, so nothing is migrated here. The
# tests run in their package's directory, so the result directory is made
# absolute first.
mkdir -p -- "${OLP_BENCH_OUT:-.local/bench}"
OLP_BENCH_OUT=$(cd -- "${OLP_BENCH_OUT:-.local/bench}" && pwd)
export OLP_BENCH_OUT
# A result left by an earlier run must not stand in for this one: a scenario
# that fails before it writes its own would otherwise be summarized below by the
# numbers of the run before.
for scenario in "${selected[@]}"; do
  id=${scenario,,}
  rm -f -- "$OLP_BENCH_OUT/$id.json" "$OLP_BENCH_OUT/raw/$id-"*
done
status=0
go test -tags=bench -count=1 -timeout="$timeout" -v -run "$run_pattern" ./tests/bench || status=$?
if command -v node >/dev/null; then
  node scripts/bench-summary.mjs "$OLP_BENCH_OUT" "${selected[@]}" || true
fi
exit "$status"
