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

write_release_version_fixture() {
  local workspace=$1

  mkdir -p "$workspace/scripts/lib" "$workspace/console" "$workspace/deploy/helm" "$workspace/crates/olp-db/migrations"
  cp "$script_dir/check-release-version.sh" "$workspace/scripts/check-release-version.sh"
  cp "$helper" "$workspace/scripts/lib/repository-validation.sh"
  printf '%s\n' '[workspace.package]' 'version = "2.0.0"' '[workspace]' 'members = []' 'resolver = "3"' > "$workspace/Cargo.toml"
  printf '{"version": "2.0.0"}\n' > "$workspace/console/package.json"
  printf 'version: 2.0.0\nappVersion: 2.0.0\n' > "$workspace/deploy/helm/Chart.yaml"
  printf 'ARG OLP_VERSION=2.0.0\n' > "$workspace/deploy/Dockerfile"
  printf '%s\n' \
    'OLP_PREVIOUS_RELEASED_VERSION=1.0.0' \
    'OLP_PREVIOUS_RELEASED_SCHEMA_MIGRATION=0021' \
    'OLP_PREVIOUS_RELEASED_COMMIT=2b534f7aadb3834bf8048dbe83cda7768e67d34f' \
    'OLP_PREVIOUS_RELEASED_IMAGE_DIGEST=sha256:0000000000000000000000000000000000000000000000000000000000000000' \
    > "$workspace/release-metadata.env"
  touch "$workspace/crates/olp-db/migrations/0021_pricing_dimensions.sql"
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
  jq '.packages[0].dependencies += [{name: "reqwest", path: null, kind: null}]' \
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
    "reqwest is owned by the engine role, not fixture-source (db)"
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

test_release_version_valid_metadata_passes() {
  local fixture_root="$test_root/release-version-valid"
  local workspace="$fixture_root/workspace"
  local output="$fixture_root/output.log"

  write_release_version_fixture "$workspace"
  "$workspace/scripts/check-release-version.sh" > "$output" 2>&1 || return
  assert_contains "$output" "release metadata is consistent"
}

test_release_version_missing_metadata_rejected() {
  local fixture_root="$test_root/release-version-missing-metadata"
  local workspace="$fixture_root/workspace"
  local output="$fixture_root/output.log"
  local status

  write_release_version_fixture "$workspace"
  rm -f "$workspace/release-metadata.env"
  if "$workspace/scripts/check-release-version.sh" > "$output" 2>&1; then
    return 1
  else
    status=$?
  fi
  [[ $status == 2 ]] || return 1
  assert_contains "$output" "required file is missing: $workspace/release-metadata.env"
}

test_release_version_invalid_semver_rejected() {
  local fixture_root="$test_root/release-version-invalid-semver"
  local workspace="$fixture_root/workspace"
  local output="$fixture_root/output.log"
  local status

  write_release_version_fixture "$workspace"
  sed -i 's/^OLP_PREVIOUS_RELEASED_VERSION=.*/OLP_PREVIOUS_RELEASED_VERSION=not-semver/' \
    "$workspace/release-metadata.env"
  if "$workspace/scripts/check-release-version.sh" > "$output" 2>&1; then
    return 1
  else
    status=$?
  fi
  [[ $status == 1 ]] || return 1
  assert_contains "$output" "previous released version is not semantic: not-semver"
}

test_release_version_matching_candidate_rejected() {
  local fixture_root="$test_root/release-version-matching-candidate"
  local workspace="$fixture_root/workspace"
  local output="$fixture_root/output.log"
  local status

  write_release_version_fixture "$workspace"
  sed -i 's/^OLP_PREVIOUS_RELEASED_VERSION=.*/OLP_PREVIOUS_RELEASED_VERSION=2.0.0/' \
    "$workspace/release-metadata.env"
  if "$workspace/scripts/check-release-version.sh" > "$output" 2>&1; then
    return 1
  else
    status=$?
  fi
  [[ $status == 1 ]] || return 1
  assert_contains "$output" "previous released version 2.0.0 cannot match candidate workspace version 2.0.0"
}

test_release_version_invalid_migration_format_rejected() {
  local fixture_root="$test_root/release-version-invalid-migration"
  local workspace="$fixture_root/workspace"
  local output="$fixture_root/output.log"
  local status

  write_release_version_fixture "$workspace"
  sed -i 's/^OLP_PREVIOUS_RELEASED_SCHEMA_MIGRATION=.*/OLP_PREVIOUS_RELEASED_SCHEMA_MIGRATION=21/' \
    "$workspace/release-metadata.env"
  if "$workspace/scripts/check-release-version.sh" > "$output" 2>&1; then
    return 1
  else
    status=$?
  fi
  [[ $status == 1 ]] || return 1
  assert_contains "$output" "previous released schema migration must be 4 digits: 21"
}

test_release_version_invalid_commit_sha_rejected() {
  local fixture_root="$test_root/release-version-invalid-commit"
  local workspace="$fixture_root/workspace"
  local output="$fixture_root/output.log"
  local status

  write_release_version_fixture "$workspace"
  sed -i 's/^OLP_PREVIOUS_RELEASED_COMMIT=.*/OLP_PREVIOUS_RELEASED_COMMIT=2b534f7/' \
    "$workspace/release-metadata.env"
  if "$workspace/scripts/check-release-version.sh" > "$output" 2>&1; then
    return 1
  else
    status=$?
  fi
  [[ $status == 1 ]] || return 1
  assert_contains "$output" "previous released commit must be a 40-character hex SHA: 2b534f7"
}

test_release_version_invalid_image_digest_rejected() {
  local fixture_root="$test_root/release-version-invalid-digest"
  local workspace="$fixture_root/workspace"
  local output="$fixture_root/output.log"
  local status

  write_release_version_fixture "$workspace"
  sed -i 's/^OLP_PREVIOUS_RELEASED_IMAGE_DIGEST=.*/OLP_PREVIOUS_RELEASED_IMAGE_DIGEST=latest/' \
    "$workspace/release-metadata.env"
  if "$workspace/scripts/check-release-version.sh" > "$output" 2>&1; then
    return 1
  else
    status=$?
  fi
  [[ $status == 1 ]] || return 1
  assert_contains "$output" "previous released image digest must be sha256:<64-hex>: latest"
}

test_release_version_unsupported_line_rejected() {
  local fixture_root="$test_root/release-version-unsupported-line"
  local workspace="$fixture_root/workspace"
  local output="$fixture_root/output.log"
  local status

  write_release_version_fixture "$workspace"
  printf 'UNKNOWN_ASSIGNMENT=true\n' >> "$workspace/release-metadata.env"
  if "$workspace/scripts/check-release-version.sh" > "$output" 2>&1; then
    return 1
  else
    status=$?
  fi
  [[ $status == 1 ]] || return 1
  assert_contains "$output" "release metadata contains an unsupported line: UNKNOWN_ASSIGNMENT=true"
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
run_test "new packages are accepted through semantic role edges" \
  test_semantic_role_edge_allows_new_packages
run_test "forbidden semantic role edges are rejected" \
  test_forbidden_role_edge_is_rejected
run_test "missing architecture roles are rejected" \
  test_missing_architecture_role_is_rejected
run_test "infrastructure dependencies follow role ownership" \
  test_infrastructure_dependency_wrong_role_is_rejected
run_test "engine internal module boundaries are enforced" \
  test_engine_internal_boundaries_are_rejected
run_test "same-name non-workspace paths remain unclassified" \
  test_same_name_non_workspace_path_is_rejected
run_test "external Cargo patch paths are rejected" \
  test_external_cargo_patch_path_is_rejected
run_test "release version with valid metadata passes" \
  test_release_version_valid_metadata_passes
run_test "release version rejects missing release-metadata.env" \
  test_release_version_missing_metadata_rejected
run_test "release version rejects non-semver previous version" \
  test_release_version_invalid_semver_rejected
run_test "release version rejects previous version matching candidate" \
  test_release_version_matching_candidate_rejected
run_test "release version rejects non-4-digit migration" \
  test_release_version_invalid_migration_format_rejected
run_test "release version rejects invalid commit SHA" \
  test_release_version_invalid_commit_sha_rejected
run_test "release version rejects invalid image digest" \
  test_release_version_invalid_image_digest_rejected
run_test "release version rejects unsupported lines in metadata" \
  test_release_version_unsupported_line_rejected
run_test "actual repository invariant checks pass" test_actual_repository_checks

printf 'repository validation regression tests passed: %d\n' "$tests_run"
