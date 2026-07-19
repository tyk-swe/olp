#!/usr/bin/env bash
set -euo pipefail

workspace_root="$(cd "$(dirname "$0")/.." && pwd -P)"
cd "$workspace_root"

violations=0

report_matches() {
  local message="$1"
  local pattern="$2"
  shift 2
  local output
  if output="$(rg -n --no-heading "$pattern" "$@")"; then
    printf '%s\n%s\n' "$message" "$output" >&2
    violations=1
  else
    local status=$?
    if (( status != 1 )); then
      echo "boundary check could not scan the workspace" >&2
      exit "$status"
    fi
  fi
}

for required_directory in apps crates console/src console/src/routes; do
  if [[ ! -d "$required_directory" ]]; then
    echo "boundary check expected $required_directory to exist" >&2
    exit 2
  fi
done

report_matches \
  "workspace manifest enables an unsupported platform dependency:" \
  '^[[:space:]]*"?(@sveltejs/adapter-(node|cloudflare)|@cloudflare/[^"[:space:]]+|@libsql/[^"[:space:]]+|wrangler|better-sqlite3|cloudflare|cloudflare-workers|rusqlite|libsql|sqlite3?|worker)["[:space:]]*[:=]' \
  Cargo.toml apps crates console/package.json \
  --glob 'Cargo.toml' --glob 'package.json'

report_matches \
  "PostgreSQL-only workspace enables the SQLite backend:" \
  '^[[:space:]]*"sqlite"[[:space:]]*,?[[:space:]]*$' \
  Cargo.toml apps crates \
  --glob 'Cargo.toml'

mapfile -t server_routes < <(
  find console/src/routes -type f \
    \( -name '+page.server.*' -o -name '+layout.server.*' -o -name '+server.*' \) \
    -print
)
mapfile -t server_modules < <(
  find console/src -type f \( -name 'hooks.server.*' -o -path '*/lib/server/*' \) -print
)
if (( ${#server_routes[@]} || ${#server_modules[@]} )); then
  echo "console must remain a static client-only application:" >&2
  printf '  %s\n' "${server_routes[@]}" "${server_modules[@]}" >&2
  violations=1
fi

if ! rg -q "@sveltejs/adapter-static" console/svelte.config.js; then
  echo "console must use @sveltejs/adapter-static" >&2
  violations=1
fi
if ! rg -q 'export[[:space:]]+const[[:space:]]+ssr[[:space:]]*=[[:space:]]*false' \
  console/src/routes/+layout.ts; then
  echo "console root layout must disable server-side rendering" >&2
  violations=1
fi

# A path dependency may point to another workspace crate, but never escape the
# repository workspace.
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
done < <(rg -n --no-heading -o 'path[[:space:]]*=[[:space:]]*"[^"]+"' \
  Cargo.toml apps crates --glob 'Cargo.toml' || true)

metadata="$(cargo metadata --locked --no-deps --format-version 1)"
actual_packages="$(jq -r '.packages[].name' <<<"$metadata" | sort)"
expected_packages="$(printf '%s\n' \
  olp olp-conformance-fixtures olp-domain olp-protocols olp-providers olp-storage | sort)"
if [[ "$actual_packages" != "$expected_packages" ]]; then
  echo "workspace packages do not match the five-crate architecture plus conformance fixtures:" >&2
  printf '%s\n' "$actual_packages" >&2
  violations=1
fi

actual_dag="$(jq -r '
  .packages[]
  | select(.name != "olp-conformance-fixtures")
  | .name as $package
  | ([.dependencies[] | select(.path != null and .kind != "dev") | .name] | unique | sort | join(",")) as $dependencies
  | "\($package)\t\($dependencies)"
' <<<"$metadata" | sort)"
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
done < <(jq -r '
  .packages[] as $package
  | $package.dependencies[]
  | select(.kind != "dev")
  | [$package.name, .name]
  | @tsv
' <<<"$metadata")

report_matches \
  "concrete provider construction escaped olp-providers:" \
  '(OpenAiConnector|AnthropicConnector|GeminiConnector|VertexConnector|BedrockConnector|AzureOpenAiConnector)::(new|with_application_default|with_service_account_json)' \
  apps/olp/src crates/domain crates/protocols crates/storage \
  --glob '*.rs'

if (( violations )); then
  exit 1
fi

echo "architecture boundaries are clean"
