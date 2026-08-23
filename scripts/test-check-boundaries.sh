#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
helper="$script_dir/lib/repository-validation.sh"

for required_executable in rg cargo jq; do
  command -v "$required_executable" >/dev/null || {
    echo "test-check-boundaries.sh: $required_executable is required" >&2
    exit 1
  }
done

real_bash=$(command -v bash)
real_cat=$(command -v cat)
real_grep=$(command -v grep)
original_path=$PATH
test_root=$(mktemp -d)
trap 'rm -rf "$test_root"' EXIT

# shellcheck source=scripts/lib/repository-validation.sh
source "$helper"
VALIDATION_SCRIPT_NAME=test-check-boundaries.sh

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
  "$real_grep" -Fq "$2" "$1"
}

# Runs a command expected to fail with exactly STATUS; stdout+stderr go to OUTPUT.
expect_fail() {
  local status=$1 output=$2
  shift 2
  local actual=0
  "$@" > "$output" 2>&1 || actual=$?
  [[ $actual == "$status" ]]
}

write_fake_bin() {
  local directory=$1 name=$2 body=$3
  mkdir -p "$directory"
  printf '#!%s\n%s\n' "$real_bash" "$body" > "$directory/$name"
  chmod +x "$directory/$name"
}

write_fake_rg() {
  write_fake_bin "$1" rg "$2"
}

# Creates a boundary fixture under $test_root/NAME with a two-package workspace
# (roles SOURCE_ROLE -> DEPENDENCY_ROLE) and a fake `cargo metadata`. Sets
# workspace, metadata_file, fake_bin, and output for the caller.
boundary_fixture() {
  local name=$1 source_role=$2 dependency_role=$3
  local fixture_root="$test_root/$name"
  workspace="$fixture_root/workspace"
  metadata_file="$fixture_root/metadata.json"
  fake_bin="$fixture_root/bin"
  output="$fixture_root/output.log"

  mkdir -p "$workspace/scripts/lib" "$workspace/console/src/routes" \
    "$workspace/packages/source/src" "$workspace/packages/dependency/src"
  cp "$script_dir/check-boundaries.sh" "$workspace/scripts/check-boundaries.sh"
  cp "$helper" "$workspace/scripts/lib/repository-validation.sh"
  printf 'version = 4\n' > "$workspace/Cargo.lock"
  printf '{}\n' > "$workspace/console/package.json"
  printf '@sveltejs/adapter-static\n' > "$workspace/console/svelte.config.js"
  printf 'export const ssr = false;\n' > "$workspace/console/src/routes/+layout.ts"
  printf '%s\n' \
    '[workspace]' \
    'members = ["packages/source", "packages/dependency"]' \
    'resolver = "3"' > "$workspace/Cargo.toml"
  printf '%s\n' \
    '[package]' \
    'name = "fixture-source"' \
    'version = "0.0.0"' \
    'edition = "2024"' \
    '[package.metadata.olp]' \
    "role = \"$source_role\"" \
    '[dependencies]' \
    'fixture-dependency = { path = "../dependency" }' > \
    "$workspace/packages/source/Cargo.toml"
  printf '%s\n' \
    '[package]' \
    'name = "fixture-dependency"' \
    'version = "0.0.0"' \
    'edition = "2024"' \
    '[package.metadata.olp]' \
    "role = \"$dependency_role\"" > \
    "$workspace/packages/dependency/Cargo.toml"
  : > "$workspace/packages/source/src/lib.rs"
  : > "$workspace/packages/dependency/src/lib.rs"
  local package role
  for package in source dependency; do
    role=$source_role
    [[ $package == source ]] || role=$dependency_role
    [[ $role == engine ]] || continue
    mkdir -p "$workspace/packages/$package/src"/{domain,protocols,providers,inference}
  done

  jq -n \
    --arg workspace "$workspace" \
    --arg source_role "$source_role" \
    --arg dependency_role "$dependency_role" '
      {
        packages: [
          {
            name: "fixture-source",
            manifest_path: ($workspace + "/packages/source/Cargo.toml"),
            metadata: {olp: {role: $source_role}},
            dependencies: [
              {
                name: "fixture-dependency",
                path: ($workspace + "/packages/dependency"),
                kind: null
              }
            ],
            targets: [
              {
                kind: ["lib"],
                src_path: ($workspace + "/packages/source/src/lib.rs")
              }
            ]
          },
          {
            name: "fixture-dependency",
            manifest_path: ($workspace + "/packages/dependency/Cargo.toml"),
            metadata: {olp: {role: $dependency_role}},
            dependencies: [],
            targets: [
              {
                kind: ["lib"],
                src_path: ($workspace + "/packages/dependency/src/lib.rs")
              }
            ]
          }
        ]
      }
    ' > "$metadata_file"
  write_fake_bin "$fake_bin" cargo "exec $(printf '%q' "$real_cat") \"\$FAKE_CARGO_METADATA\""
  export FAKE_CARGO_METADATA=$metadata_file
}

check_boundaries() {
  PATH="$fake_bin:$original_path" "$workspace/scripts/check-boundaries.sh"
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

test_simulated_rg_failure() {
  local fake_bin="$test_root/fake-rg-failure/bin"
  local diagnostic="$test_root/fake-rg-failure.log"
  local output=unchanged
  local matched=unchanged
  write_fake_rg "$fake_bin" 'exit 23'

  PATH="$fake_bin:$original_path" expect_fail 23 "$diagnostic" \
    checked_rg_capture output matched "simulate rg failure" "/fixture/path" \
    needle /fixture/path || return
  [[ $output == unchanged && $matched == unchanged ]] || return 1
  assert_contains "$diagnostic" \
    "ripgrep scan failed: operation=simulate rg failure path=/fixture/path exit=23"
}

test_supply_chain_scan_error_has_no_success() {
  local fake_bin="$test_root/supply-chain-failure/bin"
  local output="$test_root/supply-chain-failure.log"
  write_fake_rg "$fake_bin" $'printf \'deceptive-match\\n\'\nexit 9'

  PATH="$fake_bin:$original_path" expect_fail 9 "$output" \
    "$script_dir/check-supply-chain-pins.sh" || return
  assert_contains "$output" "ripgrep scan failed: operation=scan GitHub Action references" || return
  ! assert_contains "$output" "supply-chain pins verified"
}

test_semantic_role_edge_allows_new_packages() {
  boundary_fixture semantic-role-allowed db engine
  check_boundaries > "$output" 2>&1 || return
  assert_contains "$output" "architecture boundaries are clean"
}

test_forbidden_role_edge_is_rejected() {
  boundary_fixture semantic-role-rejected engine db
  expect_fail 1 "$output" check_boundaries || return
  assert_contains "$output" \
    "fixture-source (engine) must not depend on fixture-dependency (db)"
}

test_missing_architecture_role_is_rejected() {
  boundary_fixture missing-architecture-role db engine
  jq 'del(.packages[0].metadata.olp.role)' "$metadata_file" > "$metadata_file.edited"
  FAKE_CARGO_METADATA=$metadata_file.edited
  expect_fail 1 "$output" check_boundaries || return
  assert_contains "$output" "fixture-source must declare a valid architecture role"
}

test_infrastructure_dependency_wrong_role_is_rejected() {
  boundary_fixture infrastructure-role-rejected db db
  jq '.packages[0].dependencies += [
        {name: "reqwest", path: null, kind: null},
        {name: "aws-sdk-s3", path: null, kind: null}
      ]' "$metadata_file" > "$metadata_file.edited"
  FAKE_CARGO_METADATA=$metadata_file.edited
  expect_fail 1 "$output" check_boundaries || return
  assert_contains "$output" \
    "reqwest is owned by the engine role, not fixture-source (db)" || return
  assert_contains "$output" \
    "aws-sdk-s3 is owned by the engine role, not fixture-source (db)"
}

test_external_metadata_paths_are_rejected() {
  boundary_fixture external-metadata-paths db db
  local external_workspace="$workspace-copy"
  mkdir -p "$external_workspace/packages/dependency" "$workspace/../external/src"
  jq --arg manifest "$external_workspace/packages/dependency/Cargo.toml" \
    --arg source "$workspace/packages/source/../../../external/src/lib.rs" '
      .packages[1].manifest_path = $manifest
      | .packages[0].targets[0].src_path = $source
    ' "$metadata_file" > "$metadata_file.edited"
  FAKE_CARGO_METADATA=$metadata_file.edited
  expect_fail 1 "$output" check_boundaries || return
  assert_contains "$output" "fixture-dependency manifest is outside the workspace" || return
  assert_contains "$output" "production source root outside the workspace"
}

test_engine_internal_boundaries_are_rejected() {
  boundary_fixture engine-internal-boundaries engine engine
  printf '%s\n' 'use crate::providers::Connector;' > \
    "$workspace/packages/source/src/domain/forbidden.rs"
  printf '%s\n' 'fn forbidden() { let _ = reqwest::Client::new(); }' > \
    "$workspace/packages/source/src/protocols/forbidden.rs"
  printf '%s\n' \
    'use olp_db::Store;' \
    'fn forbidden() { let _ = OpenAiConnector::new(); }' > \
    "$workspace/packages/source/src/inference/forbidden.rs"
  printf '%s\n' 'fn allowed() { let _ = OpenAiConnector::new(); }' > \
    "$workspace/packages/source/src/providers/allowed.rs"

  expect_fail 1 "$output" check_boundaries || return
  assert_contains "$output" "engine domain must not depend on sibling modules:" || return
  assert_contains "$output" "engine protocols must remain infrastructure-free:" || return
  assert_contains "$output" \
    "engine inference must use provider and persistence ports instead of infrastructure:" || return
  assert_contains "$output" \
    "concrete provider construction escaped olp_engine::providers:" || return
  assert_contains "$output" "src/inference/forbidden.rs" || return
  ! assert_contains "$output" "src/providers/allowed.rs"
}

test_same_name_non_workspace_path_is_rejected() {
  boundary_fixture same-name-unclassified-path db engine
  mkdir -p "$workspace/tools/spoof/src"
  sed -i 's#path = "../dependency"#path = "../../tools/spoof"#' \
    "$workspace/packages/source/Cargo.toml"
  printf '%s\n' \
    '[package]' \
    'name = "fixture-dependency"' \
    'version = "0.0.0"' \
    'edition = "2024"' \
    '[workspace]' > "$workspace/tools/spoof/Cargo.toml"
  : > "$workspace/tools/spoof/src/lib.rs"
  jq --arg path "$workspace/tools/spoof" \
    '.packages[0].dependencies[0].path = $path' \
    "$metadata_file" > "$metadata_file.edited"
  FAKE_CARGO_METADATA=$metadata_file.edited
  expect_fail 1 "$output" check_boundaries || return
  assert_contains "$output" \
    'fixture-source (db) has an unclassified path dependency on fixture-dependency'
}

test_external_cargo_patch_path_is_rejected() {
  boundary_fixture external-cargo-patch db db
  mkdir -p "$workspace/../external"
  printf '%s\n' \
    '[workspace]' \
    'members = []' \
    '[patch.crates-io]' \
    "serde = { path = '../external' }" > "$workspace/Cargo.toml"
  printf '%s\n' \
    '[package]' \
    'name = "serde"' \
    'version = "0.0.0"' > "$workspace/../external/Cargo.toml"
  printf '{"packages": []}\n' > "$metadata_file"
  expect_fail 1 "$output" check_boundaries || return
  assert_contains "$output" \
    'Cargo.toml has a path dependency outside the workspace: ../external'
}

test_discovery_failures_abort_without_success() {
  local system_find system_jq
  system_find=$(command -v find)
  system_jq=$(command -v jq)

  boundary_fixture manifest-discovery-failure db engine
  write_fake_bin "$fake_bin" find \
    "if [[ \${1-} == . ]]; then
  printf 'Cargo.toml\\n'
  exit 23
fi
exec $(printf '%q' "$system_find") \"\$@\""
  expect_fail 23 "$output" check_boundaries || return
  ! assert_contains "$output" "architecture boundaries are clean" || return 1

  boundary_fixture delivery-api-discovery-failure delivery engine
  write_fake_bin "$fake_bin" jq \
    "if [[ \${2-} == *'any(. == \"lib\")'* ]]; then
  printf '%s\\n' $(printf '%q' "$workspace/packages/source/src/lib.rs")
  exit 24
fi
exec $(printf '%q' "$system_jq") \"\$@\""
  expect_fail 24 "$output" check_boundaries || return
  ! assert_contains "$output" "architecture boundaries are clean" || return 1

  boundary_fixture server-file-discovery-failure db engine
  write_fake_bin "$fake_bin" find \
    "if [[ \${1-} == console/src ]]; then
  printf 'console/src/routes/+page.server.ts\\n'
  exit 25
fi
exec $(printf '%q' "$system_find") \"\$@\""
  expect_fail 25 "$output" check_boundaries || return
  ! assert_contains "$output" "architecture boundaries are clean" || return 1
}

run_test "valid no-match scan succeeds" test_valid_no_match_scan
run_test "ripgrep exit greater than one fails" test_simulated_rg_failure
run_test "supply-chain success is suppressed after scan error" \
  test_supply_chain_scan_error_has_no_success
run_test "new packages are accepted through semantic role edges" \
  test_semantic_role_edge_allows_new_packages
run_test "forbidden semantic role edges are rejected" \
  test_forbidden_role_edge_is_rejected
run_test "missing architecture roles are rejected" \
  test_missing_architecture_role_is_rejected
run_test "infrastructure dependencies follow role ownership" \
  test_infrastructure_dependency_wrong_role_is_rejected
run_test "external metadata paths are rejected" \
  test_external_metadata_paths_are_rejected
run_test "engine internal module boundaries are enforced" \
  test_engine_internal_boundaries_are_rejected
run_test "same-name non-workspace paths remain unclassified" \
  test_same_name_non_workspace_path_is_rejected
run_test "external Cargo patch paths are rejected" \
  test_external_cargo_patch_path_is_rejected
run_test "discovery failures abort without reporting success" \
  test_discovery_failures_abort_without_success

printf 'check-boundaries regression tests passed: %d\n' "$tests_run"
