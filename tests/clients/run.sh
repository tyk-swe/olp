#!/usr/bin/env bash
# Runs the client qualification suites: real, pinned client releases against an
# OLP gateway whose upstream is the scripted fixture of tests/clientfixture.
# See tests/clients/README.md for the harness contract.
#
#   tests/clients/run.sh                       every suite in the table
#   CLIENTS=claude-code,codex tests/clients/run.sh   only these
#   tests/clients/run.sh --list                the suite names
set -euo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
repo_dir=$(cd -- "$script_dir/../.." && pwd)
ready_timeout_seconds=${OLP_CLIENTS_READY_TIMEOUT_SECONDS:-60}
suite_timeout_seconds=${OLP_CLIENTS_SUITE_TIMEOUT_SECONDS:-900}
test_timeout_seconds=${OLP_CLIENTS_TEST_TIMEOUT_SECONDS:-180}

# One row per suite: name|runner|target|skip reason.
#
# The runner is `node` (node:test files in the target directory, run one at a
# time because the recording is shared) or `go` (a package directory of the Go
# module in this directory). A row with a skip reason is an explicit, visible
# open item that the summary prints; it is never skipped silently, and a suite
# without a reason that cannot run fails.
suites=(
  'harness|node|suites/harness|'
  'ai-sdk|node|suites/ai-sdk|'
  'go-sdks|go|gosdk|'
  'claude-code|node|suites/claude-code|'
  'codex|node|suites/codex|'
  'gemini-cli|node|suites/gemini-cli|'
  'openai-agents|node|suites/openai-agents|'
  'langchain|node|suites/langchain|'
  'llamaindex|node|suites/llamaindex|'
)

names=()
for row in "${suites[@]}"; do names+=("${row%%|*}"); done

if [[ ${1:-} == --list ]]; then
  printf '%s\n' "${names[@]}"
  exit 0
elif (( $# > 0 )); then
  echo "usage: [CLIENTS=name,name] tests/clients/run.sh [--list]" >&2
  exit 64
fi

for setting in \
  "OLP_CLIENTS_READY_TIMEOUT_SECONDS:$ready_timeout_seconds" \
  "OLP_CLIENTS_SUITE_TIMEOUT_SECONDS:$suite_timeout_seconds" \
  "OLP_CLIENTS_TEST_TIMEOUT_SECONDS:$test_timeout_seconds"; do
  if [[ ! ${setting#*:} =~ ^[1-9][0-9]*$ ]]; then
    echo "${setting%%:*} must be a positive integer" >&2
    exit 64
  fi
done

# The suites to run, in table order.
selected=("${names[@]}")
if [[ -n ${CLIENTS:-} ]]; then
  selected=()
  IFS=, read -r -a requested <<<"$CLIENTS"
  for want in "${requested[@]}"; do
    found=false
    for name in "${names[@]}"; do [[ $name == "$want" ]] && found=true; done
    if [[ $found != true ]]; then
      echo "unknown client '$want'; the suites are: ${names[*]}" >&2
      exit 64
    fi
  done
  for name in "${names[@]}"; do
    for want in "${requested[@]}"; do
      if [[ $name == "$want" ]]; then
        selected+=("$name")
        break
      fi
    done
  done
fi

row_field() { # name index
  local row
  for row in "${suites[@]}"; do
    if [[ ${row%%|*} == "$1" ]]; then
      IFS='|' read -r -a fields <<<"$row"
      printf '%s' "${fields[$2]:-}"
      return
    fi
  done
}

# The suites that run are the unskipped ones. Only they need a toolchain, a
# scratch directory and a gateway, and a missing toolchain is a failure, never a
# quiet skip.
runnable=()
need_node=false
need_go=false
for name in "${selected[@]}"; do
  [[ -n $(row_field "$name" 3) ]] && continue
  runnable+=("$name")
  case $(row_field "$name" 1) in
    node) need_node=true ;;
    go) need_go=true ;;
  esac
done
scratch=
if (( ${#runnable[@]} > 0 )); then
  commands=(go timeout jq curl mktemp)
  [[ $need_node == true ]] && commands+=(node)
  for command in "${commands[@]}"; do
    command -v "$command" >/dev/null 2>&1 || {
      echo "required command is unavailable: $command" >&2
      exit 1
    }
  done
  if [[ $need_node == true && ! -d $script_dir/node_modules ]]; then
    echo "client packages are missing; run 'pnpm install --frozen-lockfile' in the repository" >&2
    exit 1
  fi
  scratch=$(mktemp -d)
fi
metadata=$scratch/harness.json
fixture_log=$scratch/clientfixture.log
fixture_pid=
suite_pid=

# stop_suite ends the suite that is running. A suite runs in the background
# under timeout, which passes a TERM on to its command and kills it after its
# grace, so a signal to this script ends the suite as its deadline does, instead
# of after the suite has finished.
stop_suite() {
  [[ -n $suite_pid ]] || return 0
  if kill -0 "$suite_pid" 2>/dev/null; then
    kill "$suite_pid" 2>/dev/null || true
    for _ in {1..200}; do
      kill -0 "$suite_pid" 2>/dev/null || break
      sleep 0.1
    done
    if kill -0 "$suite_pid" 2>/dev/null; then
      kill -KILL "$suite_pid" 2>/dev/null || true
    fi
  fi
  wait "$suite_pid" 2>/dev/null || true
  suite_pid=
}

stop_fixture() {
  [[ -n $fixture_pid ]] || return 0
  if kill -0 "$fixture_pid" 2>/dev/null; then
    kill "$fixture_pid" 2>/dev/null || true
    for _ in {1..50}; do
      kill -0 "$fixture_pid" 2>/dev/null || break
      sleep 0.1
    done
    if kill -0 "$fixture_pid" 2>/dev/null; then
      kill -KILL "$fixture_pid" 2>/dev/null || true
    fi
  fi
  wait "$fixture_pid" 2>/dev/null || true
  fixture_pid=
}

cleanup() {
  local status=$?
  trap - EXIT INT TERM
  stop_suite
  stop_fixture
  if (( status != 0 )) && [[ -s $fixture_log ]]; then
    echo "--- client fixture log (last 120 lines) ---" >&2
    tail -n 120 "$fixture_log" >&2
  fi
  [[ -z $scratch ]] || rm -rf -- "$scratch"
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

harness_env=()
go_env=()
results=()
record() { results+=("$(printf '%-16s %-8s %s' "$1" "$2" "${3:-}")"); }

if (( ${#runnable[@]} > 0 )); then
  fixture_bin=$repo_dir/.local/bin/clientfixture
  (cd -- "$repo_dir" && mkdir -p .local/bin && go build -o "$fixture_bin" ./tests/clientfixture)
  OLP_CLIENTS_METADATA=$metadata "$fixture_bin" >"$fixture_log" 2>&1 &
  fixture_pid=$!

  deadline=$((SECONDS + ready_timeout_seconds))
  until [[ -s $metadata ]]; do
    if ! kill -0 "$fixture_pid" 2>/dev/null; then
      echo "the client fixture exited before becoming ready" >&2
      exit 1
    fi
    if (( SECONDS >= deadline )); then
      echo "the client fixture was not ready within ${ready_timeout_seconds} seconds" >&2
      exit 1
    fi
    sleep 0.1
  done

  # The harness contract, as environment variables: each metadata member is
  # one, and the allowlist below is all else a suite inherits.
  while IFS= read -r line; do harness_env+=("$line"); done < <(jq -r 'to_entries[] | "\(.key)=\(.value)"' "$metadata")
  origin=$(jq -r .OLP_CLIENTS_ORIGIN "$metadata")
  api_key=$(jq -r .OLP_CLIENTS_API_KEY "$metadata")
  curl -fsS --noproxy '*' -o /dev/null -H "Authorization: Bearer $api_key" "$origin/v1/models" || {
    echo "the gateway does not serve $origin" >&2
    exit 1
  }

  if [[ $need_go == true ]]; then
    # Modules are fetched here, outside the isolation. Inside it the proxy refuses
    # the network and GOPROXY=off, so a suite can only use what was fetched.
    (cd -- "$script_dir" && go mod download)
    # The developer's `go env -w` settings live under the real HOME, which a
    # suite does not see, so they are carried over explicitly.
    for name in GOCACHE GOMODCACHE GOPATH GOTOOLCHAIN; do
      go_env+=("$name=$(cd -- "$script_dir" && go env "$name")")
    done
    go_env+=("GOPROXY=off")
  fi
fi

# await_suite waits for the suite started in the background, and returns its
# status. A signal ends the wait at once and runs the trap of this script, which
# stops the suite.
await_suite() {
  local status=0
  wait "$suite_pid" || status=$?
  suite_pid=
  return "$status"
}

# run_suite runs one suite from a clean environment: a private home and XDG
# tree so no client reads or writes the developer's configuration, telemetry and
# update checks off, no inherited credentials or endpoints, and a proxy that
# refuses everything but loopback, so a client that bypasses OLP fails instead
# of reaching the network.
run_suite() { # name runner target
  local name=$1 runner=$2 target=$3 dir=$scratch/suite-$1 ran skipped tee_pid
  mkdir -p "$dir"/{home,config,data,cache,state,tmp}
  local -a env_vars=(
    "PATH=$PATH" "LANG=C.UTF-8" "TERM=dumb" "NO_COLOR=1"
    "HOME=$dir/home" "XDG_CONFIG_HOME=$dir/config" "XDG_DATA_HOME=$dir/data"
    "XDG_CACHE_HOME=$dir/cache" "XDG_STATE_HOME=$dir/state" "TMPDIR=$dir/tmp"
    "OLP_CLIENTS_SCRATCH=$dir"
    "DO_NOT_TRACK=1" "NO_UPDATE_NOTIFIER=1" "DISABLE_AUTOUPDATER=1" "DISABLE_TELEMETRY=1"
    "HTTP_PROXY=http://127.0.0.1:9" "HTTPS_PROXY=http://127.0.0.1:9" "ALL_PROXY=http://127.0.0.1:9"
    "NO_PROXY=127.0.0.1,localhost,::1" "NODE_USE_ENV_PROXY=1"
    "GOFLAGS=-mod=readonly"
    "${go_env[@]}" "${harness_env[@]}"
  )
  case $runner in
    node)
      if ! compgen -G "$script_dir/$target/*.test.mjs" >/dev/null; then
        echo "suite $name has no test files in $target" >&2
        return 1
      fi
      # The reporter of lib/leaf-tests.mjs lists the tests that ran.
      (cd -- "$script_dir" && exec env -i "${env_vars[@]}" \
        timeout --kill-after=15s "${suite_timeout_seconds}s" \
        node --test --test-concurrency=1 --test-timeout=$((test_timeout_seconds * 1000)) \
          --test-reporter=spec --test-reporter-destination=stdout \
          --test-reporter="$script_dir/lib/leaf-tests.mjs" --test-reporter-destination="$dir/tests.jsonl" \
          "$target/*.test.mjs") &
      suite_pid=$!
      await_suite || return 1
      # A suite that ran nothing, a test file that defines nothing, or a skipped
      # test or group has not qualified anything. An open item belongs in the
      # table with its reason, never in a test.
      "$script_dir/lib/check-ran.sh" "$name" "$dir/tests.jsonl" "$script_dir/$target"/*.test.mjs || return 1
      ;;
    go)
      # The root go vet ./... does not reach this module, so vet it here. The
      # tests carry the integration tag, as every suite that needs a running
      # harness does, and fail without one.
      (cd -- "$script_dir" && env -i "${env_vars[@]}" go vet -tags=integration "./$target/...") || return 1
      # The toolchain bounds a package, not a test, so the suite deadline bounds
      # the package; each SDK call has a deadline of its own in the tests.
      mkfifo "$dir/go-test.fifo"
      tee "$dir/go-test.txt" <"$dir/go-test.fifo" &
      tee_pid=$!
      (cd -- "$script_dir" && exec env -i "${env_vars[@]}" \
        timeout --kill-after=15s "${suite_timeout_seconds}s" \
        go test -tags=integration -v -count=1 -timeout "${suite_timeout_seconds}s" "./$target/...") >"$dir/go-test.fifo" 2>&1 &
      suite_pid=$!
      await_suite || {
        wait "$tee_pid" 2>/dev/null || true
        return 1
      }
      wait "$tee_pid" 2>/dev/null || true
      ran=$(grep -c '^--- PASS' "$dir/go-test.txt" || true)
      skipped=$(grep -c -- '--- SKIP' "$dir/go-test.txt" || true)
      if (( ran == 0 )); then
        echo "suite $name ran no tests" >&2
        return 1
      elif (( skipped > 0 )); then
        echo "suite $name skipped $skipped test(s); record the open item in the table of run.sh instead" >&2
        return 1
      fi
      ;;
    *)
      echo "suite $name has unknown runner $runner" >&2
      return 2
      ;;
  esac
}

failed=0
for name in "${selected[@]}"; do
  reason=$(row_field "$name" 3)
  if [[ -n $reason ]]; then
    echo "=== $name: skipped ($reason)"
    record "$name" skipped "$reason"
    continue
  fi
  echo "=== $name"
  started=$SECONDS
  if run_suite "$name" "$(row_field "$name" 1)" "$(row_field "$name" 2)"; then
    status=passed
  else
    status=failed
    failed=1
  fi
  record "$name" "$status" "($((SECONDS - started))s)"
done

echo
echo "client qualification"
printf '  %s\n' "${results[@]}"
exit "$failed"
