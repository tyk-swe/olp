#!/usr/bin/env bash
set -euo pipefail

workspace_root="$(cd "$(dirname "$0")/.." && pwd -P)"
# shellcheck source=scripts/lib/repository-validation.sh
source "$workspace_root/scripts/lib/repository-validation.sh"
cd "$workspace_root"

for required_executable in rg find realpath cargo jq sort dirname; do
  validation_require_executable "$required_executable"
done
for required_file in \
  Cargo.toml Cargo.lock console/package.json console/svelte.config.js \
  console/src/routes/+layout.ts; do
  validation_require_file "$required_file"
done
for required_directory in \
  apps apps/olp/src crates crates/domain crates/protocols crates/providers \
  crates/storage console/src console/src/routes; do
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
  "workspace manifest enables an unsupported platform dependency:" \
  '^[[:space:]]*"?(@sveltejs/adapter-(node|cloudflare)|@cloudflare/[^"[:space:]]+|@libsql/[^"[:space:]]+|wrangler|better-sqlite3|cloudflare|cloudflare-workers|rusqlite|libsql|sqlite3?|worker)["[:space:]]*[:=]' \
  "Cargo.toml apps crates console/package.json" \
  Cargo.toml apps crates console/package.json \
  --glob 'Cargo.toml' --glob 'package.json'

report_matches \
  "PostgreSQL-only workspace enables the SQLite backend:" \
  '^[[:space:]]*"sqlite"[[:space:]]*,?[[:space:]]*$' \
  "Cargo.toml apps crates" \
  Cargo.toml apps crates \
  --glob 'Cargo.toml'

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

# A path dependency may point to another workspace crate, but never escape the
# repository workspace.
path_dependencies=
path_dependencies_matched=
checked_rg_capture path_dependencies path_dependencies_matched \
  "scan workspace path dependencies" "Cargo.toml apps crates" \
  -n --no-heading -o 'path[[:space:]]*=[[:space:]]*"[^"]+"' \
  Cargo.toml apps crates --glob 'Cargo.toml'
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

metadata="$(cargo metadata --locked --no-deps --format-version 1)"
# One jq pass emits every fact the checks below need as tagged TSV rows:
#   package <name>
#   dag <package> <comma-joined non-dev path dependencies>
#   dependency <package> <dependency>
metadata_rows=
if metadata_rows=$(jq -r '
  (.packages[] | ["package", .name] | @tsv),
  (.packages[]
    | select(.name != "olp-conformance" and .name != "olp-e2e")
    | .name as $package
    | ([.dependencies[] | select(.path != null and .kind != "dev") | .name]
       | unique | sort | join(",")) as $dependencies
    | ["dag", $package, $dependencies] | @tsv),
  (.packages[] as $package
    | $package.dependencies[]
    | select(.kind != "dev")
    | ["dependency", $package.name, .name] | @tsv)
' <<<"$metadata"); then
  :
else
  status=$?
  printf '%s: producer failed: operation=read workspace metadata path=%s exit=%d\n' \
    "$(validation_script_name)" "cargo metadata" "$status" >&2
  exit "$status"
fi

actual_packages="$(awk -F'\t' '$1 == "package" { print $2 }' <<<"$metadata_rows" | sort)"
expected_packages="$(printf '%s\n' \
  olp olp-conformance olp-domain olp-e2e olp-protocols olp-providers olp-storage | sort)"
if [[ "$actual_packages" != "$expected_packages" ]]; then
  echo "workspace packages do not match the five production crates plus the conformance and e2e harnesses:" >&2
  printf '%s\n' "$actual_packages" >&2
  violations=1
fi

actual_dag="$(awk -F'\t' '$1 == "dag" { print $2 "\t" $3 }' <<<"$metadata_rows" | sort)"
expected_dag="$(printf '%s\n' \
  $'olp\tolp-domain,olp-protocols,olp-providers,olp-storage' \
  $'olp-domain\t' \
  $'olp-protocols\tolp-domain' \
  $'olp-providers\tolp-domain,olp-protocols' \
  $'olp-storage\tolp-domain' | sort)"
if [[ "$actual_dag" != "$expected_dag" ]]; then
  echo "production workspace dependency DAG is invalid:" >&2
  printf '%s\n' "$actual_dag" >&2
  violations=1
fi

dependency_rows="$(awk -F'\t' '$1 == "dependency" { print $2 "\t" $3 }' <<<"$metadata_rows")"
if [[ -n $dependency_rows ]]; then
  while IFS=$'\t' read -r package dependency; do
    case "$dependency" in
      sqlx|redis)
        expected_owner='olp-storage'
        ;;
      reqwest|aws-*|google-cloud-auth)
        expected_owner='olp-providers'
        ;;
      axum|clap)
        expected_owner='olp'
        ;;
      *)
        continue
        ;;
    esac
    if [[ "$package" != "$expected_owner" ]]; then
      echo "$dependency is owned by $expected_owner, not $package" >&2
      violations=1
    fi
  done <<< "$dependency_rows"
fi

report_matches \
  "concrete provider construction escaped olp-providers:" \
  '(OpenAiConnector|AnthropicConnector|GeminiConnector|VertexConnector|BedrockConnector|AzureOpenAiConnector)::(new|with_application_default|with_service_account_json)' \
  "apps/olp/src crates/domain crates/protocols crates/storage" \
  apps/olp/src crates/domain crates/protocols crates/storage \
  --glob '*.rs'

if (( violations )); then
  exit 1
fi

echo "architecture boundaries are clean"
