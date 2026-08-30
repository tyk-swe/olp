#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
root=${OLP_REPOSITORY_ROOT:-$(cd "$script_dir/.." && pwd)}
# shellcheck source=scripts/lib/repository-validation.sh
source "$script_dir/lib/repository-validation.sh"

for required_executable in rg head tail grep cmp dirname; do
  validation_require_executable "$required_executable"
done
for required_directory in "$root" "$root/.github" "$root/deploy" "$root/deploy/helm"; do
  validation_require_directory "$required_directory"
done

dockerfile="$root/deploy/Dockerfile"
dockerignore="$root/deploy/Dockerfile.dockerignore"
root_dockerignore="$root/.dockerignore"
gitignore="$root/.gitignore"
for required_file in \
  "$root/LICENSE" "$dockerfile" "$dockerignore" "$root_dockerignore" "$gitignore" \
  "$root/deploy/helm/LICENSE"; do
  validation_require_file "$required_file"
done

failed=false

action_entries=
action_entries_matched=
checked_rg_capture action_entries action_entries_matched \
  "scan GitHub Action references" "$root/.github" \
  --hidden -n --glob '*.yml' --glob '*.yaml' \
  '^[[:space:]]*(-[[:space:]]+)?uses:[[:space:]]+' "$root/.github"
if (( action_entries_matched )); then
  while IFS= read -r entry; do
    reference=${entry##*uses:}
    reference=${reference%%#*}
    read -r reference _ <<< "$reference"
    reference=${reference#\"}
    reference=${reference%\"}
    reference=${reference#\'}
    reference=${reference%\'}
    [[ $reference == ./* ]] && continue
    if [[ $reference == docker://* ]]; then
      if [[ ! $reference =~ @sha256:[0-9a-f]{64}$ ]]; then
        echo "Docker-based GitHub Action is not pinned to a full digest: $entry" >&2
        failed=true
      fi
    elif [[ ! $reference =~ @([0-9a-f]{40})$ ]]; then
      echo "GitHub Action is not pinned to a full commit SHA: $entry" >&2
      failed=true
    fi
  done <<< "$action_entries"
fi

if ! head -n 1 "$dockerfile" | grep -Eq '^# syntax=[^[:space:]]+@sha256:[0-9a-f]{64}$'; then
  echo "Dockerfile frontend is not pinned to an immutable digest" >&2
  failed=true
fi

dockerfile_bases=
dockerfile_bases_matched=
checked_rg_capture dockerfile_bases dockerfile_bases_matched \
  "scan Dockerfile base images" "$dockerfile" '^FROM ' "$dockerfile"
if (( dockerfile_bases_matched )); then
  # FROM lines referencing a previously defined build stage are internal
  # aliases, not registry pulls; only external images need digest pins. The
  # base is checked against stages registered by EARLIER lines before this
  # line's own alias is recorded, so `FROM ubuntu AS ubuntu` cannot exempt
  # itself.
  declare -A dockerfile_stages=()
  while IFS= read -r entry; do
    base=${entry#FROM }
    base=${base%% *}
    if [[ -z ${dockerfile_stages[$base]:-} ]] &&
      [[ ! $entry =~ @sha256:[0-9a-f]{64}([[:space:]]+AS[[:space:]]+[[:alnum:]_-]+)?$ ]]; then
      echo "Dockerfile base is not pinned to an immutable digest: $entry" >&2
      failed=true
    fi
    if [[ $entry =~ [[:space:]]AS[[:space:]]+([[:alnum:]_-]+)$ ]]; then
      dockerfile_stages[${BASH_REMATCH[1]}]=1
    fi
  done <<< "$dockerfile_bases"
fi

buildkit_references=
buildkit_references_matched=
checked_rg_capture buildkit_references buildkit_references_matched \
  "scan BuildKit and binfmt image references" "$root" \
  --hidden --no-filename -o --glob '*.yml' --glob '*.yaml' --glob '*.sh' \
  --glob '!check-supply-chain-pins.sh' \
  '(tonistiigi/binfmt|moby/buildkit)(:[[:alnum:]._+-]+)?(@sha256:[[:alnum:]_.+-]+)?' \
  "$root"
if (( buildkit_references_matched )); then
  while IFS= read -r reference; do
    if [[ ! $reference =~ ^(tonistiigi/binfmt|moby/buildkit)(:[[:alnum:]._+-]+)?@sha256:[0-9a-f]{64}$ ]]; then
      echo "BuildKit/binfmt image digest must be exactly 64 lowercase hexadecimal characters: $reference" >&2
      failed=true
    fi
  done <<< "$buildkit_references"
fi

directory="$root/deploy/helm"
if ! cmp --silent "$root/LICENSE" "$directory/LICENSE"; then
  echo "release artifact $directory/LICENSE differs from the repository copy" >&2
  failed=true
fi
if ! grep -Fq 'COPY LICENSE /usr/share/doc/openllmproxy/' "$dockerfile"; then
  echo "final image does not install LICENSE" >&2
  failed=true
fi

for required in \
  '.env' '**/.env.*' 'gha-creds-*.json' '**/secrets/**' '**/credentials/**' \
  '**/*.key' '**/*.pem' \
  '**/target/**' '**/node_modules/**' 'backups/**' \
  'console/build' 'console/test-results' 'fuzz/artifacts/**' '**/*.spdx.json' \
  '**/*.sarif'; do
  if ! grep -Fxq "$required" "$dockerignore"; then
    echo "Dockerfile context policy does not exclude: $required" >&2
    failed=true
  fi
done
if ! grep -Fxq 'gha-creds-*.json' "$gitignore"; then
  echo "Git ignore policy does not exclude: gha-creds-*.json" >&2
  failed=true
fi
dockerignore_reincludes_matched=
checked_rg_match dockerignore_reincludes_matched \
  "scan Dockerfile context re-inclusions" "$dockerignore" -n '^!' "$dockerignore"
if (( dockerignore_reincludes_matched )); then
  echo "Dockerfile context policy must not re-include ignored secret/generated paths" >&2
  failed=true
fi

# The root .dockerignore declares itself a synchronized copy from line 4 on, so
# non-BuildKit and root-context builds exclude the same paths.
if ! tail -n +4 "$root_dockerignore" | cmp --silent - "$dockerignore"; then
  echo "$root_dockerignore is not a synchronized copy of $dockerignore from line 4 on" >&2
  failed=true
fi

third_party_containers=
third_party_containers_matched=
checked_rg_capture third_party_containers third_party_containers_matched \
  "scan executed third-party containers" "$root" \
  --hidden -n --glob '*.yml' --glob '*.yaml' --glob '*.sh' \
  --glob '!.git/**' \
  '(image:[[:space:]]*(postgres|valkey/valkey|node|nginx|grafana/k6|alpine|ghcr\.io/shopify/toxiproxy):|(?:docker[[:space:]]+(?:pull|run)[^\n]*|image=)(grafana/k6|alpine):|^[[:space:]]+(postgres|valkey/valkey|node|nginx|grafana/k6|alpine|ghcr\.io/shopify/toxiproxy):[0-9])' \
  "$root"
if (( third_party_containers_matched )); then
  while IFS= read -r entry; do
    if [[ ! $entry =~ @sha256:[0-9a-f]{64} ]]; then
      echo "executed third-party container is not digest-pinned: $entry" >&2
      failed=true
    fi
  done <<< "$third_party_containers"
fi

if [[ $failed == true ]]; then
  exit 1
fi

echo "supply-chain pins verified"
