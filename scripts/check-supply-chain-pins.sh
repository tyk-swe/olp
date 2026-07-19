#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
failed=false

while IFS= read -r entry; do
  reference=${entry#*uses: }
  reference=${reference%%[[:space:]#]*}
  [[ $reference == ./* ]] && continue
  if [[ ! $reference =~ @([0-9a-f]{40})$ ]]; then
    echo "GitHub Action is not pinned to a full commit SHA: $entry" >&2
    failed=true
  fi
done < <(rg -n '^[[:space:]]*-[[:space:]]+uses:' "$root/.github/workflows" || true)

dockerfile="$root/deploy/Dockerfile"
if ! head -n 1 "$dockerfile" | grep -Eq '^# syntax=[^[:space:]]+@sha256:[0-9a-f]{64}$'; then
  echo "Dockerfile frontend is not pinned to an immutable digest" >&2
  failed=true
fi
while IFS= read -r entry; do
  if [[ ! $entry =~ @sha256:[0-9a-f]{64}([[:space:]]+AS[[:space:]]+[[:alnum:]_-]+)?$ ]]; then
    echo "Dockerfile base is not pinned to an immutable digest: $entry" >&2
    failed=true
  fi
done < <(rg '^FROM ' "$dockerfile")

while IFS= read -r reference; do
  if [[ ! $reference =~ ^(tonistiigi/binfmt|moby/buildkit)(:[[:alnum:]._+-]+)?@sha256:[0-9a-f]{64}$ ]]; then
    echo "BuildKit/binfmt image digest must be exactly 64 lowercase hexadecimal characters: $reference" >&2
    failed=true
  fi
done < <(
  rg --hidden --no-filename -o --glob '*.yml' --glob '*.yaml' --glob '*.sh' \
    --glob '!check-supply-chain-pins.sh' \
    '(tonistiigi/binfmt|moby/buildkit)(:[[:alnum:]._+-]+)?(@sha256:[[:alnum:]_.+-]+)?' \
    "$root" || true
)

directory="$root/deploy/helm"
for document in LICENSE NOTICE; do
  if [[ ! -f $directory/$document ]]; then
    echo "release artifact is missing $directory/$document" >&2
    failed=true
  elif ! cmp --silent "$root/$document" "$directory/$document"; then
    echo "release artifact $directory/$document differs from the repository copy" >&2
    failed=true
  fi
done
if ! grep -Fq 'COPY LICENSE NOTICE /usr/share/doc/openllmproxy/' "$dockerfile"; then
  echo "final image does not install LICENSE and NOTICE" >&2
  failed=true
fi

dockerignore="$root/deploy/Dockerfile.dockerignore"
if [[ ! -f $dockerignore ]]; then
  echo "Dockerfile-specific context policy is missing: $dockerignore" >&2
  failed=true
else
  for required in \
    '.env' '**/.env.*' '**/secrets/**' '**/credentials/**' '**/*.key' '**/*.pem' \
    '**/target/**' '**/node_modules/**' 'backups/**' \
    'console/build' 'console/test-results' 'fuzz/artifacts/**' '**/*.spdx.json' \
    '**/*.sarif'; do
    if ! grep -Fxq "$required" "$dockerignore"; then
      echo "Dockerfile context policy does not exclude: $required" >&2
      failed=true
    fi
  done
  if rg -n '^!' "$dockerignore" >/dev/null; then
    echo "Dockerfile context policy must not re-include ignored secret/generated paths" >&2
    failed=true
  fi
fi

while IFS= read -r entry; do
  if [[ ! $entry =~ @sha256:[0-9a-f]{64} ]]; then
    echo "executed third-party container is not digest-pinned: $entry" >&2
    failed=true
  fi
done < <(
  rg -n --glob '*.yml' --glob '*.yaml' --glob '*.sh' \
    '(image:[[:space:]]*(postgres|valkey/valkey|node|nginx|grafana/k6|alpine|ghcr\.io/shopify/toxiproxy):|(?:docker[[:space:]]+(?:pull|run)[^\n]*|image=)(grafana/k6|alpine):|^[[:space:]]+(postgres|valkey/valkey|node|nginx|grafana/k6|alpine|ghcr\.io/shopify/toxiproxy):[0-9])' \
    "$root/.github" "$root" || true
)

if [[ $failed == true ]]; then
  exit 1
fi

echo "supply-chain pins verified"
