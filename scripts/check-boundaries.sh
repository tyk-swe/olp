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
for required_directory in apps crates console/src console/src/routes; do
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

report_matches \
  "bootstrap composition types must not be exported from the production olp API:" \
  '^pub[[:space:]]+use[[:space:]]+bootstrap::(state|mode_dependencies)' \
  "apps/olp/src/lib.rs" \
  apps/olp/src/lib.rs

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

metadata="$(cargo metadata --locked --no-deps --format-version 1)"
# One jq pass emits package roles, manifests, source roots, and all non-dev
# dependencies. Build dependencies are production edges; dev dependencies are
# deliberately outside the production graph.
metadata_rows=
if metadata_rows=$(jq -r '
  (.packages[]
    | ["package", .name, (.metadata.olp.role // "__missing__"), .manifest_path] | @tsv),
  (.packages[]
    | select((.metadata.olp.role // "__missing__") != "test")
    | .name as $package
    | .targets[]
    | select(any(.kind[]; . == "lib" or . == "bin" or . == "proc-macro"))
    | .src_path
    | sub("/[^/]+$"; "")
    | ["source", $package, .] | @tsv),
  (.packages[] as $package
    | $package.dependencies[]
    | select(.kind != "dev")
    | ["dependency", $package.name, ($package.metadata.olp.role // "__missing__"), .name,
       (if .path == null then "external" else "path" end)] | @tsv)
' <<<"$metadata"); then
  :
else
  status=$?
  printf '%s: producer failed: operation=read workspace metadata path=%s exit=%d\n' \
    "$(validation_script_name)" "cargo metadata" "$status" >&2
  exit "$status"
fi

declare -A package_roles=()
manifest_paths=(Cargo.toml console/package.json)
while IFS=$'\t' read -r tag package role manifest; do
  [[ $tag == package ]] || continue
  case "$role" in
    domain|protocol|provider|storage|inference|delivery|test) ;;
    __missing__)
      echo "$package must declare [package.metadata.olp] role" >&2
      violations=1
      ;;
    *)
      echo "$package declares unsupported OLP role: $role" >&2
      violations=1
      ;;
  esac
  package_roles["$package"]=$role
  manifest_paths+=("$manifest")
done <<< "$metadata_rows"

# A path dependency may point to another workspace crate, but never escape the
# repository workspace. Cargo metadata supplies the complete manifest set, so
# crates outside today's apps/ and crates/ organization are covered too.
path_dependencies=
path_dependencies_matched=
checked_rg_capture path_dependencies path_dependencies_matched \
  "scan workspace path dependencies" "workspace manifests" \
  -n --no-heading -o 'path[[:space:]]*=[[:space:]]*"[^"]+"' \
  "${manifest_paths[@]}" --glob 'Cargo.toml'
if (( path_dependencies_matched )); then
  while IFS= read -r match; do
    manifest="${match%%:*}"
    remainder="${match#*:}"
    remainder="${remainder#*:}"
    dependency_path="${remainder#*\"}"
    dependency_path="${dependency_path%\"}"
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

production_source_roots=()
while IFS=$'\t' read -r tag _package source_root; do
  [[ $tag == source ]] || continue
  production_source_roots+=("$source_root")
done <<< "$metadata_rows"
mapfile -t production_source_roots < <(printf '%s\n' "${production_source_roots[@]}" | sort -u)

report_matches \
  "workspace manifest enables an unsupported platform dependency:" \
  '^[[:space:]]*"?(@sveltejs/adapter-(node|cloudflare)|@cloudflare/[^"[:space:]]+|@libsql/[^"[:space:]]+|wrangler|better-sqlite3|cloudflare|cloudflare-workers|rusqlite|libsql|sqlite3?|worker)["[:space:]]*[:=]' \
  "workspace manifests and console/package.json" \
  "${manifest_paths[@]}" --glob 'Cargo.toml' --glob 'package.json'

report_matches \
  "PostgreSQL-only workspace enables the SQLite backend:" \
  '^[[:space:]]*"sqlite"[[:space:]]*,?[[:space:]]*$' \
  "workspace manifests" "${manifest_paths[@]}" --glob 'Cargo.toml'

if (( ${#production_source_roots[@]} )); then
  report_matches \
    "production crates must not expose wildcard re-export surfaces:" \
    'pub(\([^)]*\))?[[:space:]]+use[^;]*::\*;' \
    "production workspace sources" "${production_source_roots[@]}" --glob '*.rs'
fi

dependency_rows="$(awk -F'\t' '$1 == "dependency" { print $2 "\t" $3 "\t" $4 "\t" $5 }' <<<"$metadata_rows")"
if [[ -n $dependency_rows ]]; then
  while IFS=$'\t' read -r package role dependency dependency_type; do
    if [[ $dependency_type == path && -v package_roles["$dependency"] ]]; then
      target_role=${package_roles["$dependency"]}
      allowed=0
      case "$role:$target_role" in
        domain:domain|protocol:domain|protocol:protocol|provider:domain|provider:protocol|provider:provider|storage:domain|storage:storage|inference:domain|inference:protocol|inference:provider|inference:storage|inference:inference|delivery:domain|delivery:protocol|delivery:provider|delivery:storage|delivery:inference|delivery:delivery|test:*)
          allowed=1
          ;;
      esac
      if (( ! allowed )); then
        echo "$package ($role) must not depend on $dependency ($target_role)" >&2
        violations=1
      fi
      if [[ $package == olp-domain && $target_role != test ]]; then
        echo "olp-domain must not depend on production workspace crate $dependency" >&2
        violations=1
      fi
    fi

    case "$dependency" in
      sqlx|redis)
        expected_role='storage'
        ;;
      reqwest|aws-*|google-cloud-auth)
        expected_role='provider'
        ;;
      axum|tower|tower-http|clap)
        expected_role='delivery'
        ;;
      *)
        continue
        ;;
    esac
    if [[ "$role" != "$expected_role" ]]; then
      echo "$dependency is owned by the $expected_role role, not $package ($role)" >&2
      violations=1
    fi
  done <<< "$dependency_rows"
fi

constructor_scan_roots=()
while IFS=$'\t' read -r tag package source_root; do
  [[ $tag == source && ${package_roles["$package"]} != provider ]] || continue
  constructor_scan_roots+=("$source_root")
done <<< "$metadata_rows"
mapfile -t constructor_scan_roots < <(printf '%s\n' "${constructor_scan_roots[@]}" | sort -u)
if (( ${#constructor_scan_roots[@]} )); then
  report_matches \
    "concrete provider construction escaped provider-role packages:" \
    '(OpenAiConnector|AnthropicConnector|GeminiConnector|VertexConnector|BedrockConnector|AzureOpenAiConnector)::(new|with_application_default|with_service_account_json)' \
    "non-provider production workspace packages" \
    "${constructor_scan_roots[@]}" --glob '*.rs'
fi

if (( violations )); then
  exit 1
fi

echo "architecture boundaries are clean"
