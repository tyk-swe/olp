#!/usr/bin/env bash
set -euo pipefail

workspace_root="$(cd "$(dirname "$0")/.." && pwd -P)"
# shellcheck source=scripts/lib/repository-validation.sh
source "$workspace_root/scripts/lib/repository-validation.sh"
cd "$workspace_root"

for required_executable in rg find realpath cargo jq sort dirname awk; do
  validation_require_executable "$required_executable"
done
for required_file in \
  Cargo.toml Cargo.lock console/package.json console/svelte.config.js \
  console/src/routes/+layout.ts; do
  validation_require_file "$required_file"
done
for required_directory in console/src console/src/routes; do
  validation_require_directory "$required_directory"
done

violations=0

report_matches() {
  local message="$1"
  local pattern="$2"
  local path="$3"
  shift 3
  local output
  local matched
  checked_rg_capture output matched "$message" "$path" \
    -n --no-heading "$pattern" "$@"
  if (( matched )); then
    printf '%s\n%s\n' "$message" "$output" >&2
    violations=1
  fi
}

architecture_role_is_known() {
  case "$1" in
    delivery|domain|inference|protocol|provider|storage|test-harness) return 0 ;;
    *) return 1 ;;
  esac
}

architecture_role_allows_dependency() {
  local source_role=$1
  local dependency_role=$2

  # Same-role edges permit a responsibility to be decomposed into multiple
  # crates without changing policy. Cargo remains responsible for rejecting
  # dependency cycles between those crates.
  case "$source_role:$dependency_role" in
    domain:domain) return 0 ;;
    protocol:domain|protocol:protocol) return 0 ;;
    provider:domain|provider:protocol|provider:provider) return 0 ;;
    storage:domain|storage:storage) return 0 ;;
    inference:domain|inference:protocol|inference:provider|inference:storage|inference:inference) return 0 ;;
    delivery:domain|delivery:protocol|delivery:provider|delivery:storage|delivery:inference|delivery:delivery) return 0 ;;
    test-harness:*) architecture_role_is_known "$dependency_role" ;;
    *) return 1 ;;
  esac
}

cargo_manifest_output=
if cargo_manifest_output=$(find . \
  \( -path './.git' -o -path '*/target' -o -path '*/node_modules' \) -prune -o \
  -type f -name Cargo.toml -print | sort); then
  :
else
  status=$?
  printf '%s: producer failed: operation=find Cargo manifests path=%s exit=%d\n' \
    "$(validation_script_name)" "$workspace_root" "$status" >&2
  exit "$status"
fi
cargo_manifests=()
if [[ -n $cargo_manifest_output ]]; then
  mapfile -t cargo_manifests <<< "$cargo_manifest_output"
  for manifest_index in "${!cargo_manifests[@]}"; do
    cargo_manifests[manifest_index]="${cargo_manifests[manifest_index]#./}"
  done
fi
if (( ${#cargo_manifests[@]} == 0 )); then
  echo "no Cargo manifests were found in the repository" >&2
  exit 2
fi

# A path dependency may point to another package in this repository, but never
# escape it. Main-workspace path dependencies are classified by role below.
path_dependencies=
path_dependencies_matched=
checked_rg_capture path_dependencies path_dependencies_matched \
  "scan repository path dependencies" "Cargo manifests" \
  -n --no-heading --with-filename -o \
  "path[[:space:]]*=[[:space:]]*(\"[^\"]+\"|'[^']+')" \
  "${cargo_manifests[@]}"
if (( path_dependencies_matched )); then
  while IFS= read -r match; do
    manifest="${match%%:*}"
    remainder="${match#*:}"
    remainder="${remainder#*:}"
    dependency_assignment="${remainder#*=}"
    dependency_assignment="${dependency_assignment#"${dependency_assignment%%[![:space:]]*}"}"
    dependency_path="${dependency_assignment:1:${#dependency_assignment}-2}"
    resolved="$(realpath -m "$(dirname "$manifest")/$dependency_path")"
    case "$resolved" in
      "$workspace_root"|"$workspace_root"/*) ;;
      *)
        echo "$manifest has a path dependency outside the workspace: $dependency_path" >&2
        violations=1
        ;;
    esac
  done <<< "$path_dependencies"
fi

metadata=
if metadata=$(cargo metadata --locked --no-deps --format-version 1); then
  :
else
  status=$?
  printf '%s: producer failed: operation=read workspace metadata path=%s exit=%d\n' \
    "$(validation_script_name)" "Cargo.toml" "$status" >&2
  exit "$status"
fi

# One jq pass emits every fact the checks below need as tagged TSV rows. Cargo
# package names are deliberately not architecture policy; each workspace member
# declares its responsibility through package.metadata.olp.role.
metadata_rows=
if metadata_rows=$(jq -r '
  def architecture_role:
    (.metadata.olp.role? // null) as $role
    | if (($role | type) == "string" and ($role | length) > 0)
      then $role
      else "<missing>"
      end;

  .packages as $packages
  | ($packages[]
      | ["package", .name, architecture_role, .manifest_path]
      | @tsv),
    ($packages[] as $package
      | $package.dependencies[]
      | select(.path != null and .kind != "dev")
      | [
          "edge",
          $package.name,
          ($package | architecture_role),
          .name,
          .path
        ]
      | @tsv),
    ($packages[] as $package
      | $package.dependencies[]
      | select(.path == null and .kind != "dev")
      | ["dependency", $package.name, ($package | architecture_role), .name]
      | @tsv),
    ($packages[] as $package
      | ($package | architecture_role) as $role
      | select($role != "test-harness")
      | $package.targets[]
      | select(
          (.kind | index("lib")) != null
          or (.kind | index("bin")) != null
          or (.kind | index("proc-macro")) != null
        )
      | [
          "source-root",
          $package.name,
          $role,
          (.src_path | sub("/[^/]+$"; ""))
        ]
      | @tsv),
    ($packages[] as $package
      | ($package | architecture_role) as $role
      | select($role == "delivery")
      | $package.targets[]
      | select((.kind | index("lib")) != null)
      | ["delivery-api", $package.name, $role, .src_path]
      | @tsv)
' <<< "$metadata"); then
  :
else
  status=$?
  printf '%s: producer failed: operation=classify workspace metadata path=%s exit=%d\n' \
    "$(validation_script_name)" "cargo metadata" "$status" >&2
  exit "$status"
fi

declare -a workspace_cargo_manifests=(Cargo.toml)
declare -A workspace_cargo_manifest_seen=()
declare -A workspace_role_by_directory=()
workspace_cargo_manifest_seen[Cargo.toml]=1
package_rows="$(awk -F'\t' '$1 == "package" { print $2 "\t" $3 "\t" $4 }' <<< "$metadata_rows")"
if [[ -n $package_rows ]]; then
  while IFS=$'\t' read -r package role manifest; do
    if ! architecture_role_is_known "$role"; then
      echo "$package must declare a valid architecture role in $manifest under [package.metadata.olp]; found: $role" >&2
      violations=1
    fi

    resolved_manifest="$(realpath -m "$manifest")"
    case "$resolved_manifest" in
      "$workspace_root"/*)
        relative_manifest="${resolved_manifest#"$workspace_root"/}"
        workspace_role_by_directory["$(dirname "$resolved_manifest")"]=$role
        if [[ ! ${workspace_cargo_manifest_seen[$relative_manifest]+present} ]]; then
          workspace_cargo_manifest_seen[$relative_manifest]=1
          workspace_cargo_manifests+=("$relative_manifest")
        fi
        ;;
      *)
        echo "$package manifest is outside the workspace: $manifest" >&2
        violations=1
        ;;
    esac
  done <<< "$package_rows"
fi

dependency_manifests=("${workspace_cargo_manifests[@]}" console/package.json)
report_matches \
  "main workspace manifest enables an unsupported platform dependency:" \
  '^[[:space:]]*"?(@sveltejs/adapter-(node|cloudflare)|@cloudflare/[^"[:space:]]+|@libsql/[^"[:space:]]+|wrangler|better-sqlite3|cloudflare|cloudflare-workers|rusqlite|libsql|sqlite3?|worker)["[:space:]]*[:=]' \
  "main workspace Cargo manifests and console/package.json" \
  "${dependency_manifests[@]}"

report_matches \
  "PostgreSQL-only workspace enables the SQLite backend:" \
  '^[[:space:]]*"sqlite"[[:space:]]*,?[[:space:]]*$' \
  "main workspace Cargo manifests" \
  "${workspace_cargo_manifests[@]}"

edge_rows="$(awk -F'\t' '$1 == "edge" { print $2 "\t" $3 "\t" $4 "\t" $5 }' <<< "$metadata_rows")"
if [[ -n $edge_rows ]]; then
  while IFS=$'\t' read -r package role dependency dependency_path; do
    if ! architecture_role_is_known "$role"; then
      continue
    fi
    resolved_dependency_path="$(realpath -m "$dependency_path")"
    dependency_role="${workspace_role_by_directory[$resolved_dependency_path]:-<unclassified>}"
    if ! architecture_role_is_known "$dependency_role"; then
      echo "$package ($role) has an unclassified path dependency on $dependency at $dependency_path; make it a workspace package and declare [package.metadata.olp].role" >&2
      violations=1
      continue
    fi
    if ! architecture_role_allows_dependency "$role" "$dependency_role"; then
      echo "$package ($role) must not depend on $dependency ($dependency_role)" >&2
      violations=1
    fi
  done <<< "$edge_rows"
fi

dependency_rows="$(awk -F'\t' '$1 == "dependency" { print $2 "\t" $3 "\t" $4 }' <<< "$metadata_rows")"
if [[ -n $dependency_rows ]]; then
  while IFS=$'\t' read -r package role dependency; do
    if [[ $role == test-harness ]] || ! architecture_role_is_known "$role"; then
      continue
    fi
    case "$dependency" in
      sqlx|redis)
        expected_role=storage
        ;;
      reqwest|aws-*|google-cloud-auth)
        expected_role=provider
        ;;
      axum|tower|tower-http|clap)
        expected_role=delivery
        ;;
      *)
        continue
        ;;
    esac
    if [[ $role != "$expected_role" ]]; then
      echo "$dependency is owned by the $expected_role role, not $package ($role)" >&2
      violations=1
    fi
  done <<< "$dependency_rows"
fi

declare -a production_source_roots=()
declare -a non_provider_source_roots=()
declare -A production_source_root_seen=()
declare -A non_provider_source_root_seen=()
source_root_rows="$(awk -F'\t' '$1 == "source-root" { print $2 "\t" $3 "\t" $4 }' <<< "$metadata_rows")"
if [[ -n $source_root_rows ]]; then
  while IFS=$'\t' read -r package role source_root; do
    if ! architecture_role_is_known "$role" || [[ $role == test-harness ]]; then
      continue
    fi

    resolved_source_root="$(realpath -m "$source_root")"
    case "$resolved_source_root" in
      "$workspace_root") relative_source_root=. ;;
      "$workspace_root"/*) relative_source_root="${resolved_source_root#"$workspace_root"/}" ;;
      *)
        echo "$package ($role) has a production source root outside the workspace: $source_root" >&2
        violations=1
        continue
        ;;
    esac
    if [[ ! -d $resolved_source_root ]]; then
      echo "$package ($role) has a missing production source root: $source_root" >&2
      violations=1
      continue
    fi

    if [[ ! ${production_source_root_seen[$relative_source_root]+present} ]]; then
      production_source_root_seen[$relative_source_root]=1
      production_source_roots+=("$relative_source_root")
    fi
    if [[ $role != provider && ! ${non_provider_source_root_seen[$relative_source_root]+present} ]]; then
      non_provider_source_root_seen[$relative_source_root]=1
      non_provider_source_roots+=("$relative_source_root")
    fi
  done <<< "$source_root_rows"
fi

if (( ${#production_source_roots[@]} )); then
  report_matches \
    "production crates must not expose wildcard re-export surfaces:" \
    'pub(\([^)]*\))?[[:space:]]+use[^;]*::\*;' \
    "workspace production source roots" \
    "${production_source_roots[@]}" \
    --glob '*.rs'
fi

declare -a delivery_api_files=()
declare -A delivery_api_file_seen=()
delivery_api_rows="$(awk -F'\t' '$1 == "delivery-api" { print $2 "\t" $3 "\t" $4 }' <<< "$metadata_rows")"
if [[ -n $delivery_api_rows ]]; then
  while IFS=$'\t' read -r package role api_file; do
    resolved_api_file="$(realpath -m "$api_file")"
    case "$resolved_api_file" in
      "$workspace_root"/*) relative_api_file="${resolved_api_file#"$workspace_root"/}" ;;
      *)
        echo "$package ($role) has a public API entry point outside the workspace: $api_file" >&2
        violations=1
        continue
        ;;
    esac
    if [[ ! -f $resolved_api_file ]]; then
      echo "$package ($role) has a missing public API entry point: $api_file" >&2
      violations=1
      continue
    fi
    if [[ ! ${delivery_api_file_seen[$relative_api_file]+present} ]]; then
      delivery_api_file_seen[$relative_api_file]=1
      delivery_api_files+=("$relative_api_file")
    fi
  done <<< "$delivery_api_rows"
fi

if (( ${#delivery_api_files[@]} )); then
  report_matches \
    "bootstrap composition types must not be exported from a production delivery API:" \
    '^pub[[:space:]]+use[[:space:]]+bootstrap::(state|mode_dependencies)' \
    "delivery library entry points" \
    "${delivery_api_files[@]}"
fi

server_routes_output=
if server_routes_output=$(find console/src/routes -type f \
  \( -name '+page.server.*' -o -name '+layout.server.*' -o -name '+server.*' \) \
  -print); then
  :
else
  status=$?
  printf '%s: producer failed: operation=find server routes path=%s exit=%d\n' \
    "$(validation_script_name)" "console/src/routes" "$status" >&2
  exit "$status"
fi
server_routes=()
if [[ -n $server_routes_output ]]; then
  mapfile -t server_routes <<< "$server_routes_output"
fi

server_modules_output=
if server_modules_output=$(find console/src -type f \
  \( -name 'hooks.server.*' -o -path '*/lib/server/*' \) -print); then
  :
else
  status=$?
  printf '%s: producer failed: operation=find server modules path=%s exit=%d\n' \
    "$(validation_script_name)" "console/src" "$status" >&2
  exit "$status"
fi
server_modules=()
if [[ -n $server_modules_output ]]; then
  mapfile -t server_modules <<< "$server_modules_output"
fi
if (( ${#server_routes[@]} || ${#server_modules[@]} )); then
  echo "console must remain a static client-only application:" >&2
  printf '  %s\n' "${server_routes[@]}" "${server_modules[@]}" >&2
  violations=1
fi

adapter_static_matched=
checked_rg_match adapter_static_matched \
  "verify static Svelte adapter" "console/svelte.config.js" \
  -q "@sveltejs/adapter-static" console/svelte.config.js
if (( ! adapter_static_matched )); then
  echo "console must use @sveltejs/adapter-static" >&2
  violations=1
fi

ssr_disabled_matched=
checked_rg_match ssr_disabled_matched \
  "verify console SSR is disabled" "console/src/routes/+layout.ts" \
  -q 'export[[:space:]]+const[[:space:]]+ssr[[:space:]]*=[[:space:]]*false' \
  console/src/routes/+layout.ts
if (( ! ssr_disabled_matched )); then
  echo "console root layout must disable server-side rendering" >&2
  violations=1
fi

if (( ${#non_provider_source_roots[@]} )); then
  report_matches \
    "concrete provider construction escaped the provider role:" \
    '(OpenAiConnector|AnthropicConnector|GeminiConnector|VertexConnector|BedrockConnector|AzureOpenAiConnector)::(new|with_application_default|with_service_account_json)' \
    "non-provider production source roots" \
    "${non_provider_source_roots[@]}" \
    --glob '*.rs'
fi

if (( violations )); then
  exit 1
fi

echo "architecture boundaries are clean"
