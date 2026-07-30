#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
helper="$script_dir/lib/repository-validation.sh"

for required_executable in bash rg grep mktemp mkdir chmod cargo cp; do
  command -v "$required_executable" >/dev/null || {
    echo "test-repository-validation.sh: $required_executable is required" >&2
    exit 1
  }
done

real_bash=$(command -v bash)
real_cargo=$(command -v cargo)
real_grep=$(command -v grep)
original_path=$PATH
test_root=$(mktemp -d)
trap 'rm -rf "$test_root"' EXIT

# shellcheck source=scripts/lib/repository-validation.sh
source "$helper"
VALIDATION_SCRIPT_NAME=test-repository-validation.sh

tests_run=0

run_test() {
  local name=$1
  shift

  tests_run=$((tests_run + 1))
  if "$@"; then
    printf 'ok %d - %s\n' "$tests_run" "$name"
  else
    local status=$?
    printf 'not ok %d - %s (exit %d)\n' "$tests_run" "$name" "$status" >&2
    return "$status"
  fi
}

assert_contains() {
  local file=$1
  local expected=$2
  "$real_grep" -Fq "$expected" "$file"
}

write_fake_rg() {
  local directory=$1
  local body=$2

  mkdir -p "$directory"
  {
    printf '#!%s\n' "$real_bash"
    printf '%s\n' "$body"
  } > "$directory/rg"
  chmod +x "$directory/rg"
}

test_matching_scan() {
  local scan_dir="$test_root/matching"
  local output=
  local matched=
  mkdir -p "$scan_dir"
  printf 'needle\nother\n' > "$scan_dir/input.txt"

  checked_rg_capture output matched "find fixture needle" "$scan_dir" \
    --no-heading -n needle "$scan_dir" || return
  [[ $matched == 1 ]] || return 1
  [[ $output == "$scan_dir/input.txt:1:needle" ]]
}

test_valid_no_match_scan() {
  local scan_dir="$test_root/no-match"
  local output=sentinel
  local matched=sentinel
  mkdir -p "$scan_dir"
  printf 'haystack\n' > "$scan_dir/input.txt"

  checked_rg_capture output matched "find absent fixture needle" "$scan_dir" \
    --no-heading -n needle "$scan_dir" || return
  [[ $matched == 0 && -z $output ]]
}

test_missing_required_directory() {
  local missing="$test_root/missing-required"
  local diagnostic="$test_root/missing-required.log"
  local status

  if validation_require_directory "$missing" 2>"$diagnostic"; then
    return 1
  else
    status=$?
  fi
  [[ $status == 2 ]] || return 1
  assert_contains "$diagnostic" "required directory is missing: $missing"
}

test_invalid_directory_path() {
  local invalid="$test_root/not-a-directory"
  local diagnostic="$test_root/invalid-directory.log"
  local status
  printf 'file\n' > "$invalid"

  if validation_require_directory "$invalid" 2>"$diagnostic"; then
    return 1
  else
    status=$?
  fi
  [[ $status == 2 ]] || return 1
  assert_contains "$diagnostic" "required directory path is invalid: $invalid"
}

test_invalid_scan_path() {
  local invalid="$test_root/invalid-scan-target"
  local diagnostic="$test_root/invalid-scan.log"
  local output=unchanged
  local matched=unchanged
  local status

  if checked_rg_capture output matched "scan invalid target" "$invalid" \
    needle "$invalid" 2>"$diagnostic"; then
    return 1
  else
    status=$?
  fi
  (( status > 1 )) || return 1
  [[ $output == unchanged && $matched == unchanged ]] || return 1
  assert_contains "$diagnostic" \
    "ripgrep scan failed: operation=scan invalid target path=$invalid exit=$status"
}

test_simulated_rg_failure() {
  local fake_bin="$test_root/fake-rg-failure/bin"
  local diagnostic="$test_root/fake-rg-failure.log"
  local output=unchanged
  local matched=unchanged
  local status
  write_fake_rg "$fake_bin" 'exit 23'

  if PATH="$fake_bin:$original_path" \
    checked_rg_capture output matched "simulate rg failure" "/fixture/path" \
      needle /fixture/path 2>"$diagnostic"; then
    return 1
  else
    status=$?
  fi
  [[ $status == 23 ]] || return 1
  [[ $output == unchanged && $matched == unchanged ]] || return 1
  assert_contains "$diagnostic" \
    "ripgrep scan failed: operation=simulate rg failure path=/fixture/path exit=23"
}

test_missing_rg_executable() {
  local empty_bin="$test_root/no-rg/bin"
  local diagnostic="$test_root/no-rg.log"
  local output=unchanged
  local matched=unchanged
  local status
  mkdir -p "$empty_bin"

  if PATH="$empty_bin" \
    checked_rg_capture output matched "scan without rg" "/fixture/path" \
      needle /fixture/path 2>"$diagnostic"; then
    return 1
  else
    status=$?
  fi
  [[ $status == 127 ]] || return 1
  [[ $output == unchanged && $matched == unchanged ]] || return 1
  assert_contains "$diagnostic" \
    "required executable rg was not found in PATH"
}

test_output_capture_propagates_failure() {
  local fake_bin="$test_root/producer-failure/bin"
  local diagnostic="$test_root/producer-failure.log"
  local output=caller-output
  local matched=caller-match
  local status
  write_fake_rg "$fake_bin" $'printf \'deceptive-match\\n\'\nexit 7'

  if PATH="$fake_bin:$original_path" \
    checked_rg_capture output matched "capture failing producer" "/fixture/path" \
      needle /fixture/path 2>"$diagnostic"; then
    return 1
  else
    status=$?
  fi
  [[ $status == 7 ]] || return 1
  [[ $output == caller-output && $matched == caller-match ]] || return 1
  assert_contains "$diagnostic" \
    "ripgrep scan failed: operation=capture failing producer path=/fixture/path exit=7"
}

test_match_only_wrapper_propagates_failure() {
  local fake_bin="$test_root/match-wrapper-failure/bin"
  local diagnostic="$test_root/match-wrapper-failure.log"
  local matched=unchanged
  local status
  write_fake_rg "$fake_bin" 'exit 19'

  if PATH="$fake_bin:$original_path" \
    checked_rg_match matched "match-only failing producer" "/fixture/path" \
      needle /fixture/path 2>"$diagnostic"; then
    return 1
  else
    status=$?
  fi
  [[ $status == 19 && $matched == unchanged ]] || return 1
  assert_contains "$diagnostic" \
    "ripgrep scan failed: operation=match-only failing producer path=/fixture/path exit=19"
}

test_supply_chain_scan_error_has_no_success() {
  local fake_bin="$test_root/supply-chain-failure/bin"
  local output="$test_root/supply-chain-failure.log"
  local status
  write_fake_rg "$fake_bin" $'printf \'deceptive-match\\n\'\nexit 9'

  if PATH="$fake_bin:$original_path" \
    "$script_dir/check-supply-chain-pins.sh" >"$output" 2>&1; then
    return 1
  else
    status=$?
  fi
  [[ $status == 9 ]] || return 1
  assert_contains "$output" "ripgrep scan failed: operation=scan GitHub Action references"
  if assert_contains "$output" "supply-chain pins verified"; then
    return 1
  fi
}

test_external_cargo_patch_path_is_rejected() {
  local fixture_root="$test_root/external-cargo-patch"
  local workspace="$fixture_root/workspace"
  local fake_bin="$fixture_root/bin"
  local output="$fixture_root/output.log"
  local status

  mkdir -p \
    "$workspace/scripts/lib" "$workspace/apps/olp/src" \
    "$workspace/crates/domain" "$workspace/crates/protocols" \
    "$workspace/crates/providers" "$workspace/crates/storage" \
    "$workspace/console/src/routes" "$fixture_root/external" "$fake_bin"
  cp "$script_dir/check-boundaries.sh" "$workspace/scripts/check-boundaries.sh"
  cp "$helper" "$workspace/scripts/lib/repository-validation.sh"
  printf '%s\n' \
    '[workspace]' \
    'members = []' \
    '[patch.crates-io]' \
    'serde = { path = "../external" }' > "$workspace/Cargo.toml"
  printf 'version = 4\n' > "$workspace/Cargo.lock"
  printf '{}\n' > "$workspace/console/package.json"
  printf '@sveltejs/adapter-static\n' > "$workspace/console/svelte.config.js"
  printf 'export const ssr = false;\n' > "$workspace/console/src/routes/+layout.ts"
  printf '%s\n' \
    '[package]' \
    'name = "serde"' \
    'version = "0.0.0"' > "$fixture_root/external/Cargo.toml"
  {
    printf '#!%s\n' "$real_bash"
    printf 'exec %q metadata --locked --no-deps --format-version 1 --manifest-path %q\n' \
      "$real_cargo" "$script_dir/../Cargo.toml"
  } > "$fake_bin/cargo"
  chmod +x "$fake_bin/cargo"

  if PATH="$fake_bin:$original_path" \
    "$workspace/scripts/check-boundaries.sh" >"$output" 2>&1; then
    return 1
  else
    status=$?
  fi
  [[ $status == 1 ]] || return 1
  assert_contains "$output" \
    'Cargo.toml has a path dependency outside the workspace: ../external'
}

test_actual_repository_checks() {
  local output="$test_root/actual-checks.log"

  "$script_dir/check-supply-chain-pins.sh" >"$output" 2>&1 || return
  "$script_dir/check-boundaries.sh" >>"$output" 2>&1 || return
  "$script_dir/check-storage-sqlx.sh" >>"$output" 2>&1 || return
  "$script_dir/check-release-version.sh" >>"$output" 2>&1 || return

  assert_contains "$output" "supply-chain pins verified" || return
  assert_contains "$output" "architecture boundaries are clean" || return
  assert_contains "$output" "storage SQLx policy is clean" || return
  assert_contains "$output" "release metadata is consistent"
}

run_test "matching scan returns expected matches" test_matching_scan
run_test "valid no-match scan succeeds" test_valid_no_match_scan
run_test "missing required directory fails" test_missing_required_directory
run_test "wrong-type required directory fails" test_invalid_directory_path
run_test "invalid ripgrep path fails" test_invalid_scan_path
run_test "ripgrep exit greater than one fails" test_simulated_rg_failure
run_test "missing rg is actionable" test_missing_rg_executable
run_test "captured producer output cannot hide failure" test_output_capture_propagates_failure
run_test "match-only wrapper propagates producer failure" \
  test_match_only_wrapper_propagates_failure
run_test "supply-chain success is suppressed after scan error" \
  test_supply_chain_scan_error_has_no_success
run_test "external Cargo patch paths are rejected" \
  test_external_cargo_patch_path_is_rejected
run_test "actual repository invariant checks pass" test_actual_repository_checks

printf 'repository validation regression tests passed: %d\n' "$tests_run"
