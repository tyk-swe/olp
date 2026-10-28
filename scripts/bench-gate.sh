#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."

# Compares the hot-path benchmarks of this tree with its merge base and fails
# on a significant regression in time or allocations per operation. Base and
# head run alternately on the same machine, so drift hits both equally.
#
# A regression the first comparison finds is measured again, only the
# benchmarks that showed it and with twice the samples, and fails the gate only
# if it shows again: one comparison in twenty finds a change in a benchmark
# that did not change, and the gate compares many. The gate also fails when it
# compared nothing, or when a benchmark of the base is absent from the head,
# since a renamed or deleted benchmark is one whose regressions it cannot see.
usage() {
  cat <<'EOF' >&2
usage: scripts/bench-gate.sh [BASE_REF]

BASE_REF defaults to $BENCH_BASE, then origin/main, then main. The head is
this working tree, including uncommitted changes. Needs full git history.

  BENCH_COUNT      samples per benchmark and side, at least 6 (default 10)
  BENCH_TIME       -benchtime for every run (default: the go test default)
  BENCH_THRESHOLD  regression percentage that fails the gate (default 10)
  BENCH_FILTER     regexp selecting benchmark packages by import path
  BENCH_SHARD      INDEX/TOTAL compares only one of TOTAL disjoint shards of the
                   packages, balanced by their number of benchmarks, so that
                   TOTAL machines can share the gate (default 1/1)
  BENCH_RUN        -bench regexp (default .)
  BENCH_SKIP       -skip regexp for benchmarks of the encoder itself, whose input
                   is chosen to be slow rather than to be what a request pays
                   (default: the list in tests/README.md#microbenchmarks; set it
                   empty to run them)
  BENCH_OUT        output directory (default .local/bench/gate)
  BENCH_ALLOW_REMOVED
                   1 lets benchmarks of the base be absent from the head, when
                   removing or renaming them is the change (default 0)
EOF
  exit "${1:-2}"
}
[[ $# -le 1 ]] || usage
[[ ${1:-} != -h && ${1:-} != --help ]] || usage 0
[[ ${1:-} != -* ]] || usage

# benchstat has no tagged releases, so the pin is a pseudo-version. Tools are
# pinned independently of runtime dependencies, as in check-dependencies.sh.
benchstat=golang.org/x/perf/cmd/benchstat@v0.0.0-20260929162123-406019bb8b68
count=${BENCH_COUNT:-10}
threshold=${BENCH_THRESHOLD:-10}
bench=${BENCH_RUN:-.}
skip=${BENCH_SKIP-^(BenchmarkLoad|BenchmarkCount|BenchmarkUnbrokenPieces|BenchmarkMeterWorstCase)\$}
out=${BENCH_OUT:-.local/bench/gate}
mkdir -p -- "$out"
out=$(cd -- "$out" && pwd)
# benchstat cannot report any change from fewer than 6 samples per side.
if ! [[ $count =~ ^[0-9]+$ ]] || (( count < 6 )); then
  echo "BENCH_COUNT must be an integer of at least 6" >&2
  exit 2
fi
[[ $threshold =~ ^[0-9]+(\.[0-9]+)?$ ]] || { echo "BENCH_THRESHOLD must be a percentage" >&2; exit 2; }
allow_removed=${BENCH_ALLOW_REMOVED:-0}
[[ $allow_removed == 0 || $allow_removed == 1 ]] || { echo "BENCH_ALLOW_REMOVED must be 0 or 1" >&2; exit 2; }
shard=${BENCH_SHARD:-1/1}
[[ $shard =~ ^([1-9][0-9]*)/([1-9][0-9]*)$ ]] && (( BASH_REMATCH[1] <= BASH_REMATCH[2] )) ||
  { echo "BENCH_SHARD must be INDEX/TOTAL with 1 <= INDEX <= TOTAL" >&2; exit 2; }
shard_index=${BASH_REMATCH[1]}
shard_total=${BASH_REMATCH[2]}

# What the gate has to say goes to the job summary as well as the log, so that
# a check that compared nothing is not a green mark with no explanation.
note() {
  echo "bench-gate: $1" >&2
  if [[ -n ${GITHUB_STEP_SUMMARY:-} ]]; then printf '%s\nbench-gate: %s\n%s\n' '```' "$1" '```' >>"$GITHUB_STEP_SUMMARY"; fi
}

base_ref=${1:-${BENCH_BASE:-}}
if [[ -z $base_ref ]]; then
  for candidate in origin/main main; do
    if git rev-parse --verify --quiet "$candidate^{commit}" >/dev/null; then base_ref=$candidate; break; fi
  done
fi
[[ -n $base_ref ]] || { echo "No base ref: pass one, set BENCH_BASE, or fetch origin/main" >&2; exit 2; }
git rev-parse --verify --quiet "$base_ref^{commit}" >/dev/null || { echo "Unknown base ref $base_ref" >&2; exit 2; }
base=$(git merge-base "$base_ref" HEAD) || {
  echo "No merge base between $base_ref and HEAD; a shallow clone needs git fetch --unshallow" >&2
  exit 2
}

scratch=$(mktemp -d)
cleanup() {
  status=$?
  trap - EXIT INT TERM
  git worktree remove --force "$scratch/base" >/dev/null 2>&1 || true
  git worktree prune
  rm -rf -- "$scratch"
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
git worktree add --detach --quiet "$scratch/base" "$base"
echo "bench-gate: base ${base:0:12} ($base_ref), head $(git rev-parse --short=12 HEAD) working tree, $count samples" >&2

# Import paths of the internal packages that declare benchmarks, each with the
# number it declares, one tab-separated pair per line. tests/bench holds
# scenarios rather than microbenchmarks and is not under internal/, so it is
# never listed.
benchmark_packages() {
  local package file listing benchmarks
  declare -A declared=()
  # A failing go list must fail the gate rather than look like no benchmarks.
  listing=$(cd -- "$1" && go list -f '{{range .TestGoFiles}}{{$.ImportPath}}{{"\t"}}{{$.Dir}}/{{.}}{{"\n"}}{{end}}{{range .XTestGoFiles}}{{$.ImportPath}}{{"\t"}}{{$.Dir}}/{{.}}{{"\n"}}{{end}}' ./internal/...)
  while IFS=$'\t' read -r package file; do
    [[ -n $package ]] || continue
    benchmarks=$(grep -cE '^func Benchmark([^a-z]|\()' -- "$file" || true)
    if (( benchmarks > 0 )); then declared[$package]=$(( ${declared[$package]:-0} + benchmarks )); fi
  done <<<"$listing"
  for package in "${!declared[@]}"; do
    if [[ $package =~ ${BENCH_FILTER:-.} ]]; then printf '%s\t%s\n' "$package" "${declared[$package]}"; fi
  done | LC_ALL=C sort
}
benchmark_packages "$scratch/base" | cut -f1 >"$scratch/base.packages"
benchmark_packages . >"$scratch/head.benchmarks"
cut -f1 "$scratch/head.benchmarks" >"$scratch/head.packages"
mapfile -t packages < <(LC_ALL=C comm -12 "$scratch/base.packages" "$scratch/head.packages")
mapfile -t removed < <(LC_ALL=C comm -23 "$scratch/base.packages" "$scratch/head.packages")
mapfile -t added < <(LC_ALL=C comm -13 "$scratch/base.packages" "$scratch/head.packages")
# A package whose benchmarks the base declares and the head does not is one the
# gate can no longer watch.
if (( ${#removed[@]} > 0 && ! allow_removed )); then
  note "the base declares benchmarks in ${removed[*]#github.com/*/*/} that the head does not, so the gate cannot compare them: restore them, or set BENCH_ALLOW_REMOVED=1 if removing them is the change"
  exit 1
fi
if (( ${#added[@]} > 0 )); then
  note "benchmarks in ${added[*]#github.com/*/*/} are new in the head and have no base to be compared with; the gate compares them from the next change on"
fi
if (( ${#packages[@]} == 0 )); then
  # With benchmarks at the base this is a removal that was allowed. Without, the
  # change introduces the first benchmarks, and there is nothing to regress from.
  note "no package declares benchmarks at both the base and head, so nothing was compared"
  exit 0
fi

# The packages of this shard. Every shard computes the same assignment, giving
# each package, most benchmarks first, to the shard with the fewest so far.
shard_packages() { # packages...
  local package benchmarks lightest i
  local -a load=()
  for ((i = 0; i < shard_total; i++)); do load[i]=0; done
  while IFS=$'\t' read -r package benchmarks; do
    lightest=0
    for ((i = 1; i < shard_total; i++)); do
      if (( load[i] < load[lightest] )); then lightest=$i; fi
    done
    load[lightest]=$(( load[lightest] + benchmarks ))
    if (( lightest + 1 == shard_index )); then echo "$package"; fi
  done < <(awk -F'\t' 'NR == FNR { compared[$0]; next } $1 in compared' <(printf '%s\n' "$@") "$scratch/head.benchmarks" |
    LC_ALL=C sort -t $'\t' -k2,2nr -k1,1)
}
mapfile -t packages < <(shard_packages "${packages[@]}")
if (( ${#packages[@]} == 0 )); then
  note "shard $shard has no benchmark package to compare"
  exit 0
fi
echo "bench-gate: shard $shard, ${#packages[@]} benchmark packages: ${packages[*]#github.com/*/*/}" >&2

# Build each side's test binaries once, so that no sample pays to link them.
declare -A test_dir=()
build() { # base|head, tree
  local side=$1 tree=$2 listing package dir
  listing=$(cd -- "$tree" && go list -f '{{.ImportPath}}{{"\t"}}{{.Dir}}' "${packages[@]}")
  while IFS=$'\t' read -r package dir; do
    # Align functions to a page on both sides. A benchmark of a code layout
    # the linker happens to emit is noise the comparison should not gate on;
    # with entries 4096-byte aligned, an instruction's address modulo a page
    # depends only on its offset inside its function, so identical code lands
    # on identical fetch lines, branch-predictor entries and uop-cache sets —
    # inner loops included — whatever text the change adds before it.
    (cd -- "$tree" && go test -c -vet=off -ldflags=-funcalign=4096 -o "$scratch/bin/$side/${package//\//_}.test" "$package")
    test_dir[$side/$package]=$dir
  done <<<"$listing"
}
build base "$scratch/base"
build head .

# One sample of the benchmarks that -bench selects in the packages, at a side,
# appended to a file. Packages run one at a time, so that benchmarks never
# compete for CPUs.
measure() { # base|head, output, -bench pattern, packages...
  local side=$1 output=$2 pattern=$3 package
  shift 3
  local flags=(-test.run '^$' -test.bench "$pattern" -test.benchmem -test.count=1 -test.timeout=10m)
  [[ -z ${BENCH_TIME:-} ]] || flags+=(-test.benchtime "$BENCH_TIME")
  [[ -z $skip ]] || flags+=(-test.skip "$skip")
  for package; do
    if ! (cd -- "${test_dir[$side/$package]}" && "$scratch/bin/$side/${package//\//_}.test" "${flags[@]}") >"$scratch/stdout" 2>"$scratch/stderr"; then
      cat "$scratch/stdout" "$scratch/stderr" >&2
      echo "bench-gate: benchmarks of $package failed at the $side" >&2
      exit 1
    fi
    # Keep only what benchstat reads, so test logging cannot become configuration.
    { grep -E '^(goos|goarch|pkg|cpu): |^Benchmark' "$scratch/stdout" || true; } >>"$output"
  done
}

: >"$out/old.txt"
: >"$out/new.txt"
for ((sample = 1; sample <= count; sample++)); do
  # Alternate which side goes first, so neither always benefits from a cooler or
  # warmer machine.
  if (( sample % 2 )); then
    measure base "$out/old.txt" "$bench" "${packages[@]}"
    measure head "$out/new.txt" "$bench" "${packages[@]}"
  else
    measure head "$out/new.txt" "$bench" "${packages[@]}"
    measure base "$out/old.txt" "$bench" "${packages[@]}"
  fi
  echo "bench-gate: sample $sample/$count" >&2
done
for side in old new; do
  grep -q '^Benchmark' "$out/$side.txt" || { note "no benchmark ran at the $([[ $side == old ]] && echo base || echo head); check BENCH_RUN=$bench"; exit 1; }
done

go run "$benchstat" base="$out/old.txt" head="$out/new.txt" >"$out/benchstat.txt"
cat "$out/benchstat.txt"
echo
check=(node scripts/check-benchstat.mjs --threshold "$threshold")
if (( allow_removed )); then check+=(--allow-removed); fi
status=0
"${check[@]}" --flagged "$out/flagged.tsv" "$out/benchstat.txt" || status=$?
# Anything but regressions alone is a verdict. Regressions alone are measured
# again, and only those that reproduce fail.
(( status == 3 )) || exit "$status"

mapfile -t flagged <"$out/flagged.tsv"
confirm=$(( count * 2 ))
echo >&2
echo "bench-gate: ${#flagged[@]} packages regressed; measuring only the benchmarks that did, $confirm samples per side, to see whether it reproduces" >&2
: >"$out/old-again.txt"
: >"$out/new-again.txt"
for ((sample = 1; sample <= confirm; sample++)); do
  for line in "${flagged[@]}"; do
    IFS=$'\t' read -r package pattern <<<"$line"
    if (( sample % 2 )); then
      measure base "$out/old-again.txt" "$pattern" "$package"
      measure head "$out/new-again.txt" "$pattern" "$package"
    else
      measure head "$out/new-again.txt" "$pattern" "$package"
      measure base "$out/old-again.txt" "$pattern" "$package"
    fi
  done
  echo "bench-gate: sample $sample/$confirm of the second measurement" >&2
done
go run "$benchstat" base="$out/old-again.txt" head="$out/new-again.txt" >"$out/benchstat-again.txt"
cat "$out/benchstat-again.txt"
echo
"${check[@]}" --confirm "$out/benchstat-again.txt" "$out/benchstat.txt"
