#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."

# Runs the gateway benchmark scenarios against OLP and against the LiteLLM
# release pinned in deploy/compose.bench.yaml, on the same CPUs, the same mock
# upstream and the same load, and compares them against the roadmap's targets
# (docs/roadmap/m01-measured-advantage.md). See docs/performance.md.
#
# For each scenario, OLP runs first, through scripts/bench.sh, which writes
# .local/bench/<scenario>.json. LiteLLM then runs the load that result records,
# through the same cmd/loadgen binary and a direct-to-mock baseline of its own
# (scripts/bench-litellm.mjs), and writes .local/bench/litellm/<scenario>.json.
# Only one of the two gateways runs at a time. Finally scripts/bench-compare.mjs
# prints the comparison and writes .local/bench/compare.json and compare.md.
usage() {
  cat <<'EOF' >&2
usage: scripts/bench-compare.sh

  BENCH_SCENARIOS            comma-separated scenarios to compare, from S1 to S5 (default: all five).
                             S6 has no LiteLLM counterpart: no target compares it, and its socket
                             buffers at 10,000 held streams need more TCP memory than most hosts have.
  BENCH_SERVICES             compose (default) starts disposable PostgreSQL and Valkey;
                             external uses the ones OLP_TEST_DATABASE_URL and OLP_TEST_VALKEY_URL
                             name, whose user may create databases. LiteLLM uses both, with a
                             database of its own (litellm_bench), which is dropped and made anew.

  OLP_BENCH_SCALE            multiplies every rate and concurrency; 1 is the roadmap's full rates (default 1)
  OLP_BENCH_DURATION         measured period of each run (default 60s)
  OLP_BENCH_WARMUP           warmup before it, at the same rate (default 10s)
  OLP_BENCH_GATEWAY_CPUS     CPUs for the gateway, as taskset lists them: OLP is pinned to them and
                             every LiteLLM container is given them as its cpuset, with a worker
                             for each. Unset, both may use every CPU this shell may.
  OLP_BENCH_MOCK_CPUS        CPUs for the mock upstream, which it must not share with the gateway
  OLP_BENCH_LOADGEN_CPUS     CPUs for the load generator, which it must not share either
  OLP_BENCH_ENFORCE          1 exits non-zero on a missed or unchecked target; needs full scale and
                             all three pins, and passes through to OLP's own scenario runs
  OLP_BENCH_OUT              result directory (default .local/bench)

  OLP_TEST_BINARY            compare this olp binary instead of building one from source
  OLP_BENCH_MOCK_BINARY      use this mock upstream instead of building one
  OLP_BENCH_LOADGEN_BINARY   use this load generator for LiteLLM instead of building one

S1's target is stated for a gateway of two vCPUs, so a reference comparison is two runs:
BENCH_SCENARIOS=S1 with OLP_BENCH_GATEWAY_CPUS naming two CPUs, and the other scenarios with the
CPUs the comparison is made on. The other OLP_BENCH_ variables are in tests/bench/README.md.
EOF
  exit "${1:-2}"
}
[[ $# -eq 0 ]] || { [[ $1 == -h || $1 == --help ]] && usage 0; usage; }

# Validate the scenario list.
scenarios=${BENCH_SCENARIOS:-S1,S2,S3,S4,S5}
selected=()
IFS=',' read -r -a requested <<< "$scenarios"
for scenario in "${requested[@]}"; do
  if [[ $scenario == S6 ]]; then
    echo "S6 has no LiteLLM counterpart: no roadmap target compares it, and its socket buffers at 10,000 held streams need more TCP memory than most hosts have. Run it with make bench." >&2
    exit 2
  fi
  [[ $scenario =~ ^S[1-5]$ ]] || { echo "BENCH_SCENARIOS must list scenarios from S1 to S5, got '$scenario'" >&2; exit 2; }
  [[ " ${selected[*]:-} " == *" $scenario "* ]] || selected+=("$scenario")
done
services=${BENCH_SERVICES:-compose}
[[ $services == compose || $services == external ]] || { echo "BENCH_SERVICES must be compose or external" >&2; exit 2; }
enforce=${OLP_BENCH_ENFORCE:-}
case ${enforce,,} in
  ''|0|false) enforce=0 ;;
  1|true) enforce=1 ;;
  *) echo "OLP_BENCH_ENFORCE must be 0 or 1" >&2; exit 2 ;;
esac
# Seconds in a duration such as 90s, 1m30s or 2h.
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
# What one LiteLLM scenario may take: making a key and trying it (two minutes),
# the baseline and the run, each for the warmup, the measured period and the
# slowest request a scenario allows (thirty seconds), the wait for a budget key's
# spend (150 seconds) and a last request. The rest is margin. A LiteLLM that
# hangs is stopped at this, not waited for. (The runner is started with
# timeout --foreground, so that a terminal's interrupt still reaches it and the
# load generator it starts.)
runner_limit=$(( 2 * (warmup + duration + 31) + 120 + 150 + 60 + 120 ))

# The CPUs of a taskset list such as 0-3,6, one to a line and in order. A list
# that is not one is checked below, and gives nothing useful here.
expand_cpus() {
  local part parts
  IFS=',' read -r -a parts <<< "$1"
  for part in "${parts[@]}"; do
    if [[ $part =~ ^([0-9]+)-([0-9]+)$ ]]; then seq "${BASH_REMATCH[1]}" "${BASH_REMATCH[2]}"; else echo "$part"; fi
  done | LC_ALL=C sort -u
}

# An enforcing run judges targets stated at full rates for a gateway, a mock and
# a load generator that each have CPUs of their own. Refuse before anything is
# pulled or started, rather than after, as the scenario suite would.
if (( enforce )); then
  awk -v scale="${OLP_BENCH_SCALE:-1}" 'BEGIN { exit !(scale == 1) }' || {
    echo "OLP_BENCH_ENFORCE=1 judges targets stated at full rates: unset OLP_BENCH_SCALE, or unset OLP_BENCH_ENFORCE for a smoke run" >&2
    exit 2
  }
  for pin in OLP_BENCH_GATEWAY_CPUS OLP_BENCH_MOCK_CPUS OLP_BENCH_LOADGEN_CPUS; do
    [[ -n ${!pin:-} ]] || { echo "OLP_BENCH_ENFORCE=1 needs $pin: a reference run gives each process CPUs of its own" >&2; exit 2; }
  done
  # Of CPUs of their own: a mock or a generator that competes with the gateway
  # for a core bends exactly what is measured.
  shared=0
  share_check() { # name, CPU list, name, CPU list
    local both
    both=$(LC_ALL=C comm -12 <(expand_cpus "$2") <(expand_cpus "$4") | sort -n | paste -sd, -)
    if [[ -n $both ]]; then echo "$1 and $3 both run on CPU $both" >&2; shared=1; fi
  }
  share_check "the gateway" "$OLP_BENCH_GATEWAY_CPUS" "the mock upstream" "$OLP_BENCH_MOCK_CPUS"
  share_check "the gateway" "$OLP_BENCH_GATEWAY_CPUS" "the load generator" "$OLP_BENCH_LOADGEN_CPUS"
  share_check "the mock upstream" "$OLP_BENCH_MOCK_CPUS" "the load generator" "$OLP_BENCH_LOADGEN_CPUS"
  if (( shared )); then
    echo "OLP_BENCH_ENFORCE=1 judges targets stated for a gateway, a mock and a load generator that each have CPUs of their own" >&2
    exit 2
  fi
fi
for tool in docker node curl psql; do
  command -v "$tool" >/dev/null || { echo "$tool is required" >&2; exit 2; }
done
for pin in OLP_BENCH_GATEWAY_CPUS OLP_BENCH_MOCK_CPUS OLP_BENCH_LOADGEN_CPUS; do
  if [[ -n ${!pin:-} ]]; then command -v taskset >/dev/null || { echo "$pin needs taskset" >&2; exit 2; }; fi
done

# CPUs in a taskset list such as 0-3,6.
count_cpus() {
  local total=0 part parts
  IFS=',' read -r -a parts <<< "$1"
  for part in "${parts[@]}"; do
    if [[ $part =~ ^([0-9]+)-([0-9]+)$ && ${BASH_REMATCH[2]} -ge ${BASH_REMATCH[1]} ]]; then
      total=$(( total + BASH_REMATCH[2] - BASH_REMATCH[1] + 1 ))
    elif [[ $part =~ ^[0-9]+$ ]]; then
      total=$(( total + 1 ))
    else
      echo "'$1' is not a CPU list such as 0-3,6" >&2
      return 1
    fi
  done
  echo "$total"
}

# LiteLLM gets the CPUs the OLP gateway may use, and a worker for each, which is
# its guide's rule for a machine with nothing scaling it. Unpinned, OLP may use
# every CPU this shell may, and so may LiteLLM.
if [[ -n ${OLP_BENCH_GATEWAY_CPUS:-} ]]; then
  cpuset=$OLP_BENCH_GATEWAY_CPUS
else
  cpuset=$(awk '/^Cpus_allowed_list:/ {print $2}' /proc/self/status)
fi
workers=$(count_cpus "$cpuset") || exit 2
[[ -z ${OLP_BENCH_MOCK_CPUS:-} ]] || count_cpus "$OLP_BENCH_MOCK_CPUS" >/dev/null || exit 2
[[ -z ${OLP_BENCH_LOADGEN_CPUS:-} ]] || count_cpus "$OLP_BENCH_LOADGEN_CPUS" >/dev/null || exit 2

out=${OLP_BENCH_OUT:-.local/bench}
mkdir -p -- "$out/litellm"
out=$(cd -- "$out" && pwd)
export OLP_BENCH_OUT=$out

# Descriptor limits stop a gateway long before its capacity does.
ulimit -n "$(ulimit -Hn)" 2>/dev/null || true

scratch=$(mktemp -d)
compose=()
bench=()
mock_pid=
litellm_services=(litellm litellm-ht litellm-ht-collector litellm-ht-metrics)
cleanup() {
  status=$?
  trap - EXIT INT TERM
  if [[ -n $mock_pid ]]; then kill "$mock_pid" 2>/dev/null || true; fi
  if (( ${#bench[@]} > 0 )); then
    if (( status != 0 )); then "${bench[@]}" logs --no-color --tail 100 "${litellm_services[@]}" >&2 || true; fi
    "${bench[@]}" down -v --remove-orphans >&2 || true
    # The database in services of the caller's, which compose does not own.
    if [[ ${services:-} == external && -n ${OLP_TEST_DATABASE_URL:-} ]]; then
      psql "$OLP_TEST_DATABASE_URL" -Xq -c 'DROP DATABASE IF EXISTS litellm_bench WITH (FORCE)' >&2 || true
    fi
    # Whatever compose could not remove, by the label it puts on everything.
    leftovers=$(docker ps -aq --filter "label=com.docker.compose.project=$project")
    # shellcheck disable=SC2086 # ids and names do not contain spaces
    [[ -z $leftovers ]] || docker rm -f -v $leftovers >&2 || true
    leftovers=$(docker volume ls -q --filter "label=com.docker.compose.project=$project")
    # shellcheck disable=SC2086
    [[ -z $leftovers ]] || docker volume rm $leftovers >&2 || true
    leftovers=$(docker network ls -q --filter "label=com.docker.compose.project=$project")
    # shellcheck disable=SC2086
    [[ -z $leftovers ]] || docker network rm $leftovers >&2 || true
  fi
  rm -rf -- "$scratch"
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# Registry pulls flake often enough to fail the run before it starts.
pull() {
  local attempt
  for attempt in {1..3}; do
    if "$@" pull --quiet "${services_to_pull[@]}"; then return 0; fi
    if (( attempt == 3 )); then return 1; fi
    sleep $((attempt * 10))
  done
}

# Compose reads every variable of deploy/compose.bench.yaml for every command,
# down included, so each has a value from here on; the real ones replace these
# below. The keys are made for this run.
export LITELLM_CPUSET=$cpuset LITELLM_WORKERS=$workers LITELLM_MEMORY_LIMIT="$(( workers * 4 ))g"
LITELLM_MASTER_KEY="sk-bench-$(openssl rand -hex 16)"
LITELLM_SALT_KEY="sk-bench-$(openssl rand -hex 16)"
export LITELLM_MASTER_KEY LITELLM_SALT_KEY
export LITELLM_DATABASE_URL=unset REDIS_HOST=unset REDIS_PORT=0 REDIS_PASSWORD=unset BENCH_MOCK_API_BASE=unset
export LITELLM_PORT=0 LITELLM_METRICS_PORT=0 LITELLM_PGBOUNCER_PORT=0

# Every compose command names the profiles it needs through its services, and
# --profile '*' lets down remove them all.
project="olp-bench-$$-$RANDOM"
if [[ $services == compose ]]; then
  export OLP_POSTGRES_PORT=0 OLP_VALKEY_PORT=0
  compose=(docker compose -p "$project" -f deploy/compose.dev.yaml)
  bench=(docker compose -p "$project" --profile '*' -f deploy/compose.dev.yaml -f deploy/compose.bench.yaml)
  services_to_pull=(postgres valkey)
  pull "${compose[@]}"
  "${compose[@]}" up -d --wait --wait-timeout 90
  postgres=$("${compose[@]}" port postgres 5432)
  valkey=$("${compose[@]}" port valkey 6379)
  export OLP_TEST_DATABASE_URL="postgres://olp:olp-local@$postgres/olp?sslmode=disable"
  export OLP_TEST_VALKEY_URL="redis://:olp-local@$valkey/0"
else
  : "${OLP_TEST_DATABASE_URL:?BENCH_SERVICES=external needs OLP_TEST_DATABASE_URL}"
  : "${OLP_TEST_VALKEY_URL:?BENCH_SERVICES=external needs OLP_TEST_VALKEY_URL}"
  bench=(docker compose -p "$project" --profile '*' -f deploy/compose.bench.yaml)
fi

# LiteLLM's own database in the PostgreSQL, and the Valkey it shares Redis
# protocol state through, from the URLs OLP is given.
litellm_database_url=$(node -e 'const u = new URL(process.argv[1]); u.pathname = "/litellm_bench"; console.log(u.href)' "$OLP_TEST_DATABASE_URL")
psql "$OLP_TEST_DATABASE_URL" -Xq -v ON_ERROR_STOP=1 -c 'DROP DATABASE IF EXISTS litellm_bench WITH (FORCE)' -c 'CREATE DATABASE litellm_bench'
{
  read -r REDIS_HOST
  read -r REDIS_PORT
  read -r REDIS_PASSWORD
} < <(node -e 'const u = new URL(process.argv[1]); console.log(u.hostname); console.log(u.port || 6379); console.log(decodeURIComponent(u.password))' "$OLP_TEST_VALKEY_URL")

# Three loopback ports that are free now, held at once so they differ.
free_ports() {
  node -e '
    const net = require("net");
    const servers = Array.from({ length: 3 }, () => net.createServer());
    Promise.all(servers.map((s) => new Promise((ok) => s.listen(0, "127.0.0.1", ok)))).then(() => {
      for (const s of servers) console.log(s.address().port);
      for (const s of servers) s.close();
    });
  '
}
# What deploy/compose.bench.yaml reads.
export LITELLM_DATABASE_URL=$litellm_database_url REDIS_HOST REDIS_PORT REDIS_PASSWORD
{
  read -r LITELLM_PORT
  read -r LITELLM_METRICS_PORT
  read -r LITELLM_PGBOUNCER_PORT
} < <(free_ports)
export LITELLM_PORT LITELLM_METRICS_PORT LITELLM_PGBOUNCER_PORT
litellm_url="http://127.0.0.1:$LITELLM_PORT"

# The pinned LiteLLM image, by digest.
services_to_pull=(litellm)
pull "${bench[@]}"

# The binaries: the release build of OLP, and the mock upstream and the load
# generator as static programs, so the same ones serve both gateways.
if [[ -z ${OLP_TEST_BINARY:-} ]]; then
  make build-go
  OLP_TEST_BINARY="$PWD/.local/bin/olp"
fi
export OLP_TEST_BINARY
if [[ -z ${OLP_BENCH_MOCK_BINARY:-} ]]; then
  CGO_ENABLED=0 go build -mod=readonly -trimpath -tags bench -o .local/bin/mockupstream ./tests/bench/cmd/mockupstream
  OLP_BENCH_MOCK_BINARY="$PWD/.local/bin/mockupstream"
fi
export OLP_BENCH_MOCK_BINARY
loadgen_binary=${OLP_BENCH_LOADGEN_BINARY:-}
if [[ -z $loadgen_binary ]]; then
  CGO_ENABLED=0 go build -mod=readonly -trimpath -tags bench -o .local/bin/loadgen ./tests/bench/cmd/loadgen
  loadgen_binary="$PWD/.local/bin/loadgen"
fi

# The mock upstream LiteLLM calls, on CPUs of its own. It stays up for the run,
# and each scenario sets its behavior and resets its counters. OLP's scenarios
# start a mock of their own.
mock_command=("$OLP_BENCH_MOCK_BINARY" -addr 127.0.0.1:0)
if [[ -n ${OLP_BENCH_MOCK_CPUS:-} ]]; then mock_command=(taskset -c "$OLP_BENCH_MOCK_CPUS" "${mock_command[@]}"); fi
"${mock_command[@]}" >"$scratch/mock.log" 2>&1 &
mock_pid=$!
mock_address=
for _ in {1..100}; do
  mock_address=$(sed -n 's/.*"address":"\([^"]*\)".*/\1/p' "$scratch/mock.log")
  [[ -z $mock_address ]] || break
  kill -0 "$mock_pid" 2>/dev/null || { cat "$scratch/mock.log" >&2; echo "the mock upstream exited" >&2; exit 1; }
  sleep 0.1
done
[[ -n $mock_address ]] || { echo "the mock upstream did not say where it listens" >&2; exit 1; }
export BENCH_MOCK_API_BASE="http://$mock_address/v1"

# The profile each scenario runs LiteLLM in: S3 is LiteLLM's high-throughput
# benchmark, and the rest run on its documented production settings.
profile_of() { [[ $1 == S3 ]] && echo high-throughput || echo production; }

wait_for() { # description, deadline in seconds, command...
  local what=$1 deadline=$(( SECONDS + $2 ))
  shift 2
  until "$@" >/dev/null 2>&1; do
    if (( SECONDS > deadline )); then echo "timed out waiting for $what" >&2; return 1; fi
    sleep 1
  done
}

start_litellm() {
  local profile=$1 gateway=litellm
  [[ $profile == high-throughput ]] && gateway=litellm-ht
  "${bench[@]}" up -d "$gateway"
  # A gateway of the high-throughput profile starts its own PgBouncer, which the
  # collector sidecar connects through, so the sidecars start once it serves.
  wait_for "LiteLLM to serve" 300 curl -fsS --max-time 3 "$litellm_url/health/readiness" || return 1
  if [[ $profile == high-throughput ]]; then
    "${bench[@]}" up -d litellm-ht-collector litellm-ht-metrics
    collector=$("${bench[@]}" ps -q litellm-ht-collector)
    # Through sh, whose test is built in: the image's PATH starts with a Python
    # script that is also called test.
    wait_for "the collector sidecar to listen" 300 docker exec "$collector" sh -c 'test -S /var/run/litellm/collector.sock' || return 1
    containers=$("${bench[@]}" ps -q litellm-ht litellm-ht-collector litellm-ht-metrics | paste -sd, -)
  else
    containers=$("${bench[@]}" ps -q litellm)
  fi
  litellm_image=$(docker inspect -f '{{.Config.Image}}' "${containers%%,*}")
}

stop_litellm() {
  "${bench[@]}" rm -sf "${litellm_services[@]}" >/dev/null 2>&1 || true
  docker volume rm "${project}_collector-socket" "${project}_prometheus-multiproc" >/dev/null 2>&1 || true
}

status=0
for scenario in "${selected[@]}"; do
  id=${scenario,,}
  # A result left by an earlier run must not stand in for this one.
  rm -f -- "$out/$id.json" "$out/litellm/$id.json" "$out/raw/$id-"* "$out/litellm/raw/$id-"*
  echo "=== $scenario: OLP" >&2
  if ! BENCH_SCENARIOS=$scenario BENCH_SERVICES=external ./scripts/bench.sh; then
    echo "$scenario failed against OLP" >&2
    status=1
  fi
  if [[ ! -f $out/$id.json ]]; then
    echo "$scenario left no OLP result, so there is no workload to run against LiteLLM" >&2
    status=1
    continue
  fi
  profile=$(profile_of "$scenario")
  echo "=== $scenario: LiteLLM ($profile profile, $workers workers on CPUs $cpuset)" >&2
  if start_litellm "$profile"; then
    pins=()
    [[ -z ${OLP_BENCH_GATEWAY_CPUS:-} ]] || pins+=(--gateway-cpus "$OLP_BENCH_GATEWAY_CPUS")
    [[ -z ${OLP_BENCH_MOCK_CPUS:-} ]] || pins+=(--mock-cpus "$OLP_BENCH_MOCK_CPUS")
    [[ -z ${OLP_BENCH_LOADGEN_CPUS:-} ]] || pins+=(--loadgen-cpus "$OLP_BENCH_LOADGEN_CPUS")
    timeout --foreground --kill-after=15 "${runner_limit}s" node scripts/bench-litellm.mjs "$scenario" \
      --olp-result "$out/$id.json" --out "$out/litellm" --litellm-url "$litellm_url" \
      --profile "$profile" --containers "$containers" --workers "$workers" --image "$litellm_image" \
      --mock-url "http://$mock_address" --mock-pid "$mock_pid" --loadgen-binary "$loadgen_binary" \
      --late-after "${OLP_BENCH_LATE_AFTER:-5ms}" "${pins[@]}" || {
      echo "$scenario failed against LiteLLM" >&2
      status=1
    }
    # What LiteLLM's containers said, kept beside the result: at ERROR level a
    # healthy run says almost nothing, so anything here is worth reading.
    mkdir -p -- "$out/litellm/raw"
    "${bench[@]}" logs --no-color --tail 200 "${litellm_services[@]}" >"$out/litellm/raw/$id-containers.log" 2>&1 || true
  else
    echo "LiteLLM did not start for $scenario" >&2
    "${bench[@]}" logs --no-color --tail 100 "${litellm_services[@]}" >&2 || true
    status=1
  fi
  stop_litellm
done

report=(node scripts/bench-compare.mjs --olp "$out" --litellm "$out/litellm" --out "$out" --scenarios "$(IFS=,; echo "${selected[*]}")")
if (( enforce )); then report+=(--enforce); fi
"${report[@]}" || status=1
exit "$status"
