#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
helper="$script_dir/lib/repository-validation.sh"

for required_executable in bash rg grep mktemp mkdir chmod cargo cp cat jq sed; do
  command -v "$required_executable" >/dev/null || {
    echo "test-repository-validation.sh: $required_executable is required" >&2
    exit 1
  }
done

real_bash=$(command -v bash)
real_cat=$(command -v cat)
real_find=$(command -v find)
real_grep=$(command -v grep)
original_path=$PATH
test_root=$(mktemp -d)
trap 'rm -rf "$test_root"' EXIT

# shellcheck source=scripts/lib/repository-validation.sh
source "$helper"
# shellcheck source=scripts/lib/tap.sh
source "$script_dir/lib/tap.sh"
VALIDATION_SCRIPT_NAME=test-repository-validation.sh

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

write_boundary_fixture() {
  local workspace=$1

  mkdir -p "$workspace/scripts/lib" "$workspace/console/src/routes"
  cp "$script_dir/check-boundaries.sh" "$workspace/scripts/check-boundaries.sh"
  cp "$helper" "$workspace/scripts/lib/repository-validation.sh"
  printf '%s\n' '[workspace]' 'members = []' 'resolver = "3"' > "$workspace/Cargo.toml"
  printf 'version = 4\n' > "$workspace/Cargo.lock"
  printf '{}\n' > "$workspace/console/package.json"
  printf '@sveltejs/adapter-static\n' > "$workspace/console/svelte.config.js"
  printf 'export const ssr = false;\n' > "$workspace/console/src/routes/+layout.ts"
}

write_fake_cargo_metadata() {
  local fake_bin=$1
  local metadata_file=$2

  mkdir -p "$fake_bin"
  {
    printf '#!%s\n' "$real_bash"
    printf 'exec %q %q\n' "$real_cat" "$metadata_file"
  } > "$fake_bin/cargo"
  chmod +x "$fake_bin/cargo"
}

write_two_role_fixture() {
  local workspace=$1
  local metadata_file=$2
  local source_role=$3
  local dependency_role=$4

  mkdir -p "$workspace/packages/source/src" "$workspace/packages/dependency/src"
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
  if [[ $source_role == engine ]]; then
    mkdir -p \
      "$workspace/packages/source/src/domain" \
      "$workspace/packages/source/src/protocols" \
      "$workspace/packages/source/src/providers" \
      "$workspace/packages/source/src/inference"
  fi
  if [[ $dependency_role == engine ]]; then
    mkdir -p \
      "$workspace/packages/dependency/src/domain" \
      "$workspace/packages/dependency/src/protocols" \
      "$workspace/packages/dependency/src/providers" \
      "$workspace/packages/dependency/src/inference"
  fi

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

test_semantic_role_edge_allows_new_packages() {
  local fixture_root="$test_root/semantic-role-allowed"
  local workspace="$fixture_root/workspace"
  local fake_bin="$fixture_root/bin"
  local metadata_file="$fixture_root/metadata.json"
  local output="$fixture_root/output.log"

  write_boundary_fixture "$workspace"
  write_two_role_fixture "$workspace" "$metadata_file" db engine
  write_fake_cargo_metadata "$fake_bin" "$metadata_file"

  PATH="$fake_bin:$original_path" \
    "$workspace/scripts/check-boundaries.sh" > "$output" 2>&1 || return
  assert_contains "$output" "architecture boundaries are clean"
}

test_forbidden_role_edge_is_rejected() {
  local fixture_root="$test_root/semantic-role-rejected"
  local workspace="$fixture_root/workspace"
  local fake_bin="$fixture_root/bin"
  local metadata_file="$fixture_root/metadata.json"
  local output="$fixture_root/output.log"
  local status

  write_boundary_fixture "$workspace"
  write_two_role_fixture "$workspace" "$metadata_file" engine db
  write_fake_cargo_metadata "$fake_bin" "$metadata_file"

  if PATH="$fake_bin:$original_path" \
    "$workspace/scripts/check-boundaries.sh" > "$output" 2>&1; then
    return 1
  else
    status=$?
  fi
  [[ $status == 1 ]] || return 1
  assert_contains "$output" \
    "fixture-source (engine) must not depend on fixture-dependency (db)"
}

test_missing_architecture_role_is_rejected() {
  local fixture_root="$test_root/missing-architecture-role"
  local workspace="$fixture_root/workspace"
  local fake_bin="$fixture_root/bin"
  local metadata_file="$fixture_root/metadata.json"
  local missing_metadata_file="$fixture_root/missing-metadata.json"
  local output="$fixture_root/output.log"
  local status

  write_boundary_fixture "$workspace"
  write_two_role_fixture "$workspace" "$metadata_file" db engine
  jq 'del(.packages[0].metadata.olp.role)' "$metadata_file" > "$missing_metadata_file"
  write_fake_cargo_metadata "$fake_bin" "$missing_metadata_file"

  if PATH="$fake_bin:$original_path" \
    "$workspace/scripts/check-boundaries.sh" > "$output" 2>&1; then
    return 1
  else
    status=$?
  fi
  [[ $status == 1 ]] || return 1
  assert_contains "$output" \
    "fixture-source must declare a valid architecture role"
}

test_infrastructure_dependency_wrong_role_is_rejected() {
  local fixture_root="$test_root/infrastructure-role-rejected"
  local workspace="$fixture_root/workspace"
  local fake_bin="$fixture_root/bin"
  local metadata_file="$fixture_root/metadata.json"
  local ownership_metadata_file="$fixture_root/ownership-metadata.json"
  local output="$fixture_root/output.log"
  local status

  write_boundary_fixture "$workspace"
  write_two_role_fixture "$workspace" "$metadata_file" db db
  jq '.packages[0].dependencies += [
        {name: "reqwest", path: null, kind: null},
        {name: "aws-sdk-s3", path: null, kind: null}
      ]' \
    "$metadata_file" > "$ownership_metadata_file"
  write_fake_cargo_metadata "$fake_bin" "$ownership_metadata_file"

  if PATH="$fake_bin:$original_path" \
    "$workspace/scripts/check-boundaries.sh" > "$output" 2>&1; then
    return 1
  else
    status=$?
  fi
  [[ $status == 1 ]] || return 1
  assert_contains "$output" \
    "reqwest is owned by the engine role, not fixture-source (db)" || return
  assert_contains "$output" \
    "aws-sdk-s3 is owned by the engine role, not fixture-source (db)"
}

test_manifest_discovery_failure_is_rejected() {
  local fixture_root="$test_root/manifest-discovery-failure"
  local workspace="$fixture_root/workspace"
  local fake_bin="$fixture_root/bin"
  local output="$fixture_root/output.log"
  local status

  write_boundary_fixture "$workspace"
  mkdir -p "$fake_bin"
  {
    printf '#!%s\n' "$real_bash"
    printf '%s\n' "printf './Cargo.toml\\n'" 'exit 9'
  } > "$fake_bin/find"
  chmod +x "$fake_bin/find"

  if PATH="$fake_bin:$original_path" \
    "$workspace/scripts/check-boundaries.sh" > "$output" 2>&1; then
    return 1
  else
    status=$?
  fi
  [[ $status == 9 ]] || return 1
  assert_contains "$output" "producer failed: operation=find Cargo manifests" || return
  ! assert_contains "$output" "architecture boundaries are clean"
}

test_server_file_discovery_failure_is_rejected() {
  local fixture_root="$test_root/server-discovery-failure"
  local workspace="$fixture_root/workspace"
  local fake_bin="$fixture_root/bin"
  local metadata_file="$fixture_root/metadata.json"
  local output="$fixture_root/output.log"
  local status

  write_boundary_fixture "$workspace"
  printf '{"packages": []}\n' > "$metadata_file"
  write_fake_cargo_metadata "$fake_bin" "$metadata_file"
  {
    printf '#!%s\n' "$real_bash"
    printf 'if [[ %s == console/src ]]; then\n' "\${1-}"
    printf '%s\n' "  printf 'console/src/routes/+server.ts\\n'" '  exit 9' 'fi'
    printf 'exec %q "$@"\n' "$real_find"
  } > "$fake_bin/find"
  chmod +x "$fake_bin/find"

  if PATH="$fake_bin:$original_path" \
    "$workspace/scripts/check-boundaries.sh" > "$output" 2>&1; then
    return 1
  else
    status=$?
  fi
  [[ $status == 9 ]] || return 1
  assert_contains "$output" "producer failed: operation=find console server files" || return
  ! assert_contains "$output" "architecture boundaries are clean"
}

test_external_metadata_paths_are_rejected() {
  local fixture_root="$test_root/external-metadata-paths"
  local workspace="$fixture_root/workspace"
  local external_workspace="$workspace-copy"
  local fake_bin="$fixture_root/bin"
  local metadata_file="$fixture_root/metadata.json"
  local escaped_metadata_file="$fixture_root/escaped-metadata.json"
  local output="$fixture_root/output.log"
  local status

  write_boundary_fixture "$workspace"
  write_two_role_fixture "$workspace" "$metadata_file" db db
  mkdir -p "$external_workspace/packages/dependency" "$fixture_root/external/src"
  jq --arg manifest "$external_workspace/packages/dependency/Cargo.toml" \
    --arg source "$workspace/packages/source/../../../external/src/lib.rs" '
      .packages[1].manifest_path = $manifest
      | .packages[0].targets[0].src_path = $source
    ' "$metadata_file" > "$escaped_metadata_file"
  write_fake_cargo_metadata "$fake_bin" "$escaped_metadata_file"

  if PATH="$fake_bin:$original_path" \
    "$workspace/scripts/check-boundaries.sh" > "$output" 2>&1; then
    return 1
  else
    status=$?
  fi
  [[ $status == 1 ]] || return 1
  assert_contains "$output" "fixture-dependency manifest is outside the workspace" || return
  assert_contains "$output" "production source root outside the workspace"
}

test_engine_internal_boundaries_are_rejected() {
  local fixture_root="$test_root/engine-internal-boundaries"
  local workspace="$fixture_root/workspace"
  local fake_bin="$fixture_root/bin"
  local metadata_file="$fixture_root/metadata.json"
  local output="$fixture_root/output.log"
  local status

  write_boundary_fixture "$workspace"
  write_two_role_fixture "$workspace" "$metadata_file" engine engine
  write_fake_cargo_metadata "$fake_bin" "$metadata_file"
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

  if PATH="$fake_bin:$original_path" \
    "$workspace/scripts/check-boundaries.sh" > "$output" 2>&1; then
    return 1
  else
    status=$?
  fi
  [[ $status == 1 ]] || return 1
  assert_contains "$output" "engine domain must not depend on sibling modules:" || return
  assert_contains "$output" "engine protocols must remain infrastructure-free:" || return
  assert_contains "$output" \
    "engine inference must use provider and persistence ports instead of infrastructure:" || return
  assert_contains "$output" \
    "concrete provider construction escaped olp_engine::providers:" || return
  assert_contains "$output" "src/inference/forbidden.rs" || return
  if assert_contains "$output" "src/providers/allowed.rs"; then
    return 1
  fi
}

test_same_name_non_workspace_path_is_rejected() {
  local fixture_root="$test_root/same-name-unclassified-path"
  local workspace="$fixture_root/workspace"
  local fake_bin="$fixture_root/bin"
  local metadata_file="$fixture_root/metadata.json"
  local spoofed_metadata_file="$fixture_root/spoofed-metadata.json"
  local output="$fixture_root/output.log"
  local status

  write_boundary_fixture "$workspace"
  write_two_role_fixture "$workspace" "$metadata_file" db engine
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
    "$metadata_file" > "$spoofed_metadata_file"
  write_fake_cargo_metadata "$fake_bin" "$spoofed_metadata_file"

  if PATH="$fake_bin:$original_path" \
    "$workspace/scripts/check-boundaries.sh" > "$output" 2>&1; then
    return 1
  else
    status=$?
  fi
  [[ $status == 1 ]] || return 1
  assert_contains "$output" \
    'fixture-source (db) has an unclassified path dependency on fixture-dependency'
}

test_external_cargo_patch_path_is_rejected() {
  local fixture_root="$test_root/external-cargo-patch"
  local workspace="$fixture_root/workspace"
  local fake_bin="$fixture_root/bin"
  local metadata_file="$fixture_root/metadata.json"
  local output="$fixture_root/output.log"
  local status

  write_boundary_fixture "$workspace"
  mkdir -p "$fixture_root/external"
  printf '%s\n' \
    '[workspace]' \
    'members = []' \
    '[patch.crates-io]' \
    "serde = { path = '../external' }" > "$workspace/Cargo.toml"
  printf '%s\n' \
    '[package]' \
    'name = "serde"' \
    'version = "0.0.0"' > "$fixture_root/external/Cargo.toml"
  printf '{"packages": []}\n' > "$metadata_file"
  write_fake_cargo_metadata "$fake_bin" "$metadata_file"

  if PATH="$fake_bin:$original_path" \
    "$workspace/scripts/check-boundaries.sh" > "$output" 2>&1; then
    return 1
  else
    status=$?
  fi
  [[ $status == 1 ]] || return 1
  assert_contains "$output" \
    'Cargo.toml has a path dependency outside the workspace: ../external'
}

write_ci_workflow_fixture() {
  local file=$1
  shift
  printf '%s\n' "$@" > "$file"
}

test_ci_lockstep_accepts_make_and_allow_listed_steps() {
  local fixture="$test_root/ci-lockstep-ok.yml"
  write_ci_workflow_fixture "$fixture" \
    'defaults:' \
    '  run:' \
    '    shell: bash' \
    'jobs:' \
    '  build:' \
    '    steps:' \
    '      - name: Check' \
    '        run: make check' \
    '      - name: Client tools' \
    '        run: ./scripts/ci/install-postgres-client.sh 18' \
    '      - name: Install lint dependencies' \
    '        run: |' \
    '          sudo apt-get install --yes ripgrep'
  CI_WORKFLOW="$fixture" "$script_dir/check-ci-make-lockstep.sh" > /dev/null
}

test_ci_lockstep_rejects_raw_and_unlisted_steps() {
  local fixture="$test_root/ci-lockstep-bad.yml"
  local output="$test_root/ci-lockstep-bad.log"
  local status
  write_ci_workflow_fixture "$fixture" \
    'jobs:' \
    '  build:' \
    '    steps:' \
    '      - name: Raw build' \
    '        run: cargo build --locked' \
    '      - name: Ad hoc block' \
    '        run: |' \
    '          echo drift'
  if CI_WORKFLOW="$fixture" "$script_dir/check-ci-make-lockstep.sh" > "$output" 2>&1; then
    return 1
  else
    status=$?
  fi
  [[ $status == 1 ]] || return 1
  assert_contains "$output" "step 'Raw build' runs 'cargo build --locked' instead of a make target" || return 1
  assert_contains "$output" "multi-line step 'Ad hoc block' is not in the allow-list"
}

run_test "valid no-match scan succeeds" test_valid_no_match_scan
run_test "missing required directory fails" test_missing_required_directory
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
run_test "partial manifest discovery fails closed" \
  test_manifest_discovery_failure_is_rejected
run_test "partial server-file discovery fails closed" \
  test_server_file_discovery_failure_is_rejected
run_test "external metadata paths are rejected" \
  test_external_metadata_paths_are_rejected
run_test "engine internal module boundaries are enforced" \
  test_engine_internal_boundaries_are_rejected
run_test "same-name non-workspace paths remain unclassified" \
  test_same_name_non_workspace_path_is_rejected
run_test "external Cargo patch paths are rejected" \
  test_external_cargo_patch_path_is_rejected
run_test "CI steps that run through make pass the lockstep check" \
  test_ci_lockstep_accepts_make_and_allow_listed_steps
run_test "CI steps that bypass make fail the lockstep check" \
  test_ci_lockstep_rejects_raw_and_unlisted_steps

tap_plan
