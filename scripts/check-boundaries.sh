#!/usr/bin/env bash
set -euo pipefail

workspace_root="$(cd "$(dirname "$0")/.." && pwd -P)"
# shellcheck source=scripts/lib/repository-validation.sh
source "$workspace_root/scripts/lib/repository-validation.sh"
cd "$workspace_root"

for required_executable in grep find realpath cargo jq sed sort; do
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

# grep exit 1 is a clean no-match. Anything above that is a failed scan and
# must never be read as "no violations", so it aborts instead.
scan() {
  local message=$1
  shift
  local output status
  output=$(grep -rEnH "$@") && status=0 || status=$?
  case $status in
    0) printf '%s\n%s\n' "$message" "$output" >&2; violations=1 ;;
    1) ;;
    *)
      printf '%s: scan failed: exit=%d check=%s\n' \
        "$(validation_script_name)" "$status" "$message" >&2
      exit "$status"
      ;;
  esac
}

# A path dependency may point at another package in this repository, but never
# escape it. `[patch]` entries live only in manifests, so this stays a text
# scan over manifests rather than a `cargo metadata` query.
cargo_manifest_output=$(
  find . \( -path './.git' -o -path '*/target' -o -path '*/node_modules' \) -prune -o \
    -type f -name Cargo.toml -print | sed 's#^\./##' | sort
)
[[ -n $cargo_manifest_output ]] || { echo "no Cargo manifests were found in the repository" >&2; exit 2; }
mapfile -t cargo_manifests <<< "$cargo_manifest_output"
path_dependency_output=$(grep -rEnoH \
  "path[[:space:]]*=[[:space:]]*(\"[^\"]+\"|'[^']+')" -- "${cargo_manifests[@]}") || (( $? == 1 ))
if [[ -n $path_dependency_output ]]; then
  while IFS=: read -r manifest _ assignment; do
    dependency_path=${assignment#*=}
    dependency_path=${dependency_path#"${dependency_path%%[![:space:]]*}"}
    dependency_path=${dependency_path:1:${#dependency_path}-2}
    case "$(realpath -m "$(dirname "$manifest")/$dependency_path")" in
      "$workspace_root"|"$workspace_root"/*) ;;
      *)
        echo "$manifest has a path dependency outside the workspace: $dependency_path" >&2
        violations=1
        ;;
    esac
  done <<< "$path_dependency_output"
fi

metadata=$(cargo metadata --locked --no-deps --format-version 1)

package_manifest_rows=$(jq -r '.packages[] | [.name, .manifest_path] | @tsv' <<< "$metadata")
while IFS=$'\t' read -r package manifest; do
  [[ -n $package ]] || continue
  resolved_manifest=$(realpath -m "$manifest")
  case "$resolved_manifest" in
    "$workspace_root"/*) ;;
    *)
      echo "$package manifest is outside the workspace: $manifest" >&2
      violations=1
      ;;
  esac
done <<< "$package_manifest_rows"

# Cargo package names are deliberately not architecture policy; each workspace
# member declares its responsibility through package.metadata.olp.role. Roles
# constrain edges; the engine defines the ports the database implements, so the
# dependency direction stays inward.
role_violations=$(jq -r '
  def role: .metadata.olp.role? // "<missing>";
  def dir($manifest): $manifest | sub("/[^/]+$"; "");
  ["engine","db","delivery","test-harness"] as $known
  # Same-role edges let one responsibility span several crates; Cargo still
  # rejects cycles between them.
  | ["engine:engine","db:engine","db:db",
     "delivery:engine","delivery:db","delivery:delivery"] as $allowed
  | {sqlx:"db", redis:"db", reqwest:"engine", "google-cloud-auth":"engine",
     axum:"delivery", tower:"delivery", "tower-http":"delivery", clap:"delivery"} as $owners
  | (reduce .packages[] as $p ({}; .[dir($p.manifest_path)] = ($p | role))) as $role_by_dir
  | .packages[]
  | . as $p | ($p | role) as $role
  | (if ($known | index($role)) == null then
       "\($p.name) must declare a valid architecture role in \($p.manifest_path) under [package.metadata.olp]; found: \($role)"
     else empty end),
    (select($known | index($role))
     | $p.dependencies[] | select(.kind != "dev")
     | . as $dep
     | if $dep.path != null then
         ($role_by_dir[$dep.path] // "<unclassified>") as $dep_role
         | if ($known | index($dep_role)) == null then
             "\($p.name) (\($role)) has an unclassified path dependency on \($dep.name) at \($dep.path); make it a workspace package and declare [package.metadata.olp].role"
           elif $role == "test-harness" or ($allowed | index("\($role):\($dep_role)")) then empty
           else "\($p.name) (\($role)) must not depend on \($dep.name) (\($dep_role))"
           end
       else
         ($owners[$dep.name] // (if $dep.name | startswith("aws-") then "engine" else null end)) as $owner
         | if $role != "test-harness" and $owner != null and $owner != $role then
             "\($dep.name) is owned by the \($owner) role, not \($p.name) (\($role))"
           else empty end
       end)
' <<< "$metadata")
if [[ -n $role_violations ]]; then
  printf '%s\n' "$role_violations" >&2
  violations=1
fi

# Production source roots, and the engine module roots whose inward topology is
# enforced by path below.
declare -a production_roots=() engine_roots=() non_engine_roots=()
source_root_output=$(jq -r '
  .packages[] | select((.metadata.olp.role? // "") | IN("engine","db","delivery"))
  | .metadata.olp.role as $role
  | .targets[] | select(.kind | any(IN("lib","bin","proc-macro")))
  | [$role, (.src_path | sub("/[^/]+$"; ""))] | @tsv
' <<< "$metadata" | sort -u)
while IFS=$'\t' read -r role source_root; do
  [[ -n $role ]] || continue
  resolved_source_root=$(realpath -m "$source_root")
  case "$resolved_source_root" in
    "$workspace_root") source_root=. ;;
    "$workspace_root"/*) source_root=${resolved_source_root#"$workspace_root"/} ;;
    *)
      echo "a $role package has a production source root outside the workspace: $source_root" >&2
      violations=1
      continue
      ;;
  esac
  if [[ ! -d $resolved_source_root ]]; then
    echo "a $role package has a missing production source root: $source_root" >&2
    violations=1
    continue
  fi
  production_roots+=("$source_root")
  if [[ $role == engine ]]; then engine_roots+=("$source_root"); else non_engine_roots+=("$source_root"); fi
done <<< "$source_root_output"

declare -a domain_roots=() protocol_roots=() provider_roots=() inference_roots=()
declare -a non_provider_roots=("${non_engine_roots[@]}")
for engine_root in "${engine_roots[@]}"; do
  for engine_module in domain protocols providers inference; do
    if [[ ! -d $engine_root/$engine_module ]]; then
      echo "engine source root is missing its $engine_module module: $engine_root/$engine_module" >&2
      violations=1
      continue
    fi
    case $engine_module in
      domain) domain_roots+=("$engine_root/domain") ;;
      protocols) protocol_roots+=("$engine_root/protocols") ;;
      providers) provider_roots+=("$engine_root/providers") ;;
      inference) inference_roots+=("$engine_root/inference") ;;
    esac
  done
  # Root-level engine files sit outside the providers module too. Scanning the
  # whole engine root instead would sweep providers back in.
  shopt -s nullglob
  non_provider_roots+=("$engine_root"/*.rs)
  shopt -u nullglob
done
non_provider_roots+=("${domain_roots[@]}" "${protocol_roots[@]}" "${inference_roots[@]}")

infrastructure='\b(reqwest|aws_[[:alnum:]_]+|google_cloud_auth|sqlx|redis|axum|tower|tower_http|clap)::'
(( ${#domain_roots[@]} )) && {
  scan "engine domain must not depend on sibling modules:" \
    --include='*.rs' -e '\b(protocols|providers|inference)::' -- "${domain_roots[@]}"
  scan "engine domain must remain infrastructure-free:" \
    --include='*.rs' -e "$infrastructure" -- "${domain_roots[@]}"
}
(( ${#protocol_roots[@]} )) && {
  scan "engine protocols may depend only on the domain module:" \
    --include='*.rs' -e '\b(providers|inference)::' -- "${protocol_roots[@]}"
  scan "engine protocols must remain infrastructure-free:" \
    --include='*.rs' -e "$infrastructure" -- "${protocol_roots[@]}"
}
(( ${#provider_roots[@]} )) && {
  scan "engine providers must not depend on inference:" \
    --include='*.rs' -e '\binference::' -- "${provider_roots[@]}"
  scan "engine providers must not depend on database or delivery infrastructure:" \
    --include='*.rs' -e '\b(olp_db|sqlx|redis|axum|tower|tower_http|clap)::' -- "${provider_roots[@]}"
}
(( ${#inference_roots[@]} )) && \
  scan "engine inference must use provider and persistence ports instead of infrastructure:" \
    --include='*.rs' -e '\b(olp_db|reqwest|aws_[[:alnum:]_]+|google_cloud_auth|sqlx|redis|axum|tower|tower_http|clap)::' \
    -- "${inference_roots[@]}"
(( ${#production_roots[@]} )) && \
  scan "production crates must not expose wildcard re-export surfaces:" \
    --include='*.rs' -e 'pub(\([^)]*\))?[[:space:]]+use[^;]*::\*;' -- "${production_roots[@]}"
(( ${#non_provider_roots[@]} )) && \
  scan "concrete provider construction escaped olp_engine::providers:" \
    --include='*.rs' \
    -e '(OpenAi|Anthropic|Gemini|Vertex|Bedrock|AzureOpenAi)Connector::(new|with_application_default|with_service_account_json)' \
    -- "${non_provider_roots[@]}"

scan "main workspace manifest enables an unsupported platform dependency:" \
  -e '^[[:space:]]*"?(@sveltejs/adapter-(node|cloudflare)|@cloudflare/[^"[:space:]]+|@libsql/[^"[:space:]]+|wrangler|better-sqlite3|cloudflare|cloudflare-workers|rusqlite|libsql|sqlite3?|worker)["[:space:]]*[:=]' \
  -- "${cargo_manifests[@]}" console/package.json
scan "PostgreSQL-only workspace enables the SQLite backend:" \
  -e '^[[:space:]]*"sqlite"[[:space:]]*,?[[:space:]]*$' -- "${cargo_manifests[@]}"

delivery_api_output=$(jq -r '
  .packages[] | select((.metadata.olp.role? // "") == "delivery")
  | .targets[] | select(.kind | any(. == "lib")) | .src_path
' <<< "$metadata" | sort -u)
while IFS= read -r delivery_api_file; do
  [[ -n $delivery_api_file ]] || continue
  resolved_delivery_api_file=$(realpath -m "$delivery_api_file")
  case "$resolved_delivery_api_file" in
    "$workspace_root"/*) delivery_api_file=${resolved_delivery_api_file#"$workspace_root"/} ;;
    *)
      echo "a delivery package has a public API entry point outside the workspace: $delivery_api_file" >&2
      violations=1
      continue
      ;;
  esac
  [[ -f $resolved_delivery_api_file ]] || {
    echo "a delivery package has a missing public API entry point: $delivery_api_file" >&2
    violations=1
    continue
  }
  scan "bootstrap composition types must not be exported from a production delivery API:" \
    -e '^pub[[:space:]]+use[[:space:]]+bootstrap::(state|mode_dependencies)' -- "$delivery_api_file"
done <<< "$delivery_api_output"

# The console ships as static assets from the Rust binary; a server route or
# server-only module would silently require a Node runtime in production.
server_file_output=$(find console/src -type f \
  \( -name '+page.server.*' -o -name '+layout.server.*' -o -name '+server.*' \
     -o -name 'hooks.server.*' -o -path '*/lib/server/*' \) -print)
if [[ -n $server_file_output ]]; then
  mapfile -t server_files <<< "$server_file_output"
  echo "console must remain a static client-only application:" >&2
  printf '  %s\n' "${server_files[@]}" >&2
  violations=1
fi
grep -Fq '@sveltejs/adapter-static' console/svelte.config.js || {
  echo "console must use @sveltejs/adapter-static" >&2
  violations=1
}
grep -Eq 'export[[:space:]]+const[[:space:]]+ssr[[:space:]]*=[[:space:]]*false' console/src/routes/+layout.ts || {
  echo "console root layout must disable server-side rendering" >&2
  violations=1
}

(( violations )) && exit 1
echo "architecture boundaries are clean"
