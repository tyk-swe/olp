#!/usr/bin/env bash
set -euo pipefail

step=${RELEASE_MANIFEST_STEP:-create}
dry_run=${DRY_RUN:-false}
image=${RELEASE_IMAGE:-ghcr.io/tyk-swe/olp}

if [[ $dry_run == true ]]; then
  echo "release manifest $step is skipped during a dry run"
  exit 0
fi

tag=${RELEASE_TAG:-}
if [[ -z $tag && ${GITHUB_REF_TYPE:-} == tag ]]; then
  tag=${GITHUB_REF_NAME:-}
fi
[[ $tag =~ ^v(0|[1-9][0-9]*)\.([0-9]+)\.([0-9]+)$ ]] || {
  echo "RELEASE_TAG must match vMAJOR.MINOR.PATCH" >&2
  exit 1
}
version=${tag#v}
minor=${version%.*}
candidate_tag=${RELEASE_CANDIDATE_TAG:-candidate-$version-${GITHUB_RUN_ID:-local}-${GITHUB_RUN_ATTEMPT:-1}}
[[ $candidate_tag =~ ^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$ ]] || {
  echo "RELEASE_CANDIDATE_TAG is not a valid OCI tag" >&2
  exit 1
}

version_is_at_least() {
  local candidate=$1 current=$2
  local candidate_parts current_parts index
  IFS=. read -ra candidate_parts <<< "$candidate"
  IFS=. read -ra current_parts <<< "$current"
  for index in 0 1 2; do
    ((10#${candidate_parts[index]} > 10#${current_parts[index]})) && return 0
    ((10#${candidate_parts[index]} < 10#${current_parts[index]})) && return 1
  done
  return 0
}

alias_accepts_version() {
  local alias=$1 image_config inspect_error current_version
  if ! image_config=$(docker buildx imagetools inspect "$image:$alias" \
    --format '{{json (index .Image "linux/amd64")}}' 2>&1); then
    inspect_error=${image_config,,}
    if [[ $inspect_error == *"not found"* || $inspect_error == *"manifest unknown"* ]]; then
      return 0
    fi
    printf '%s\n' "$image_config" >&2
    return 2
  fi
  current_version=$(jq -r '.config.Labels["org.opencontainers.image.version"] // empty' \
    <<< "$image_config")
  [[ $current_version =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] || {
    echo "cannot determine the release version currently published at $image:$alias" >&2
    return 2
  }
  if version_is_at_least "$version" "$current_version"; then
    return 0
  fi
  echo "leaving $image:$alias at newer release $current_version"
  return 1
}

case $step in
  create)
    command -v docker >/dev/null || { echo "docker is required" >&2; exit 1; }
    command -v jq >/dev/null || { echo "jq is required" >&2; exit 1; }
    digest_dir=${RELEASE_DIGEST_DIR:-}
    output_dir=${RELEASE_OUTPUT_DIR:-}
    [[ -f $digest_dir/amd64.digest && -f $digest_dir/arm64.digest ]] || {
      echo "RELEASE_DIGEST_DIR must contain amd64.digest and arm64.digest" >&2
      exit 1
    }
    [[ -n $output_dir ]] || { echo "RELEASE_OUTPUT_DIR is required" >&2; exit 1; }
    amd64_digest=$(<"$digest_dir/amd64.digest")
    arm64_digest=$(<"$digest_dir/arm64.digest")
    for digest in "$amd64_digest" "$arm64_digest"; do
      [[ $digest =~ ^sha256:[0-9a-f]{64}$ ]] || {
        echo "architecture digest is invalid: $digest" >&2
        exit 1
      }
    done
    metadata=$(mktemp)
    trap 'rm -f "$metadata"' EXIT
    docker buildx imagetools create \
      --annotation "index:org.opencontainers.image.version=$version" \
      --tag "$image:$candidate_tag" --metadata-file "$metadata" \
      "$image@$amd64_digest" "$image@$arm64_digest"
    index_digest=$(jq -r '."containerimage.descriptor".digest // empty' "$metadata")
    [[ $index_digest =~ ^sha256:[0-9a-f]{64}$ ]] || {
      echo "Buildx did not report a valid index digest" >&2
      exit 1
    }
    candidate_digest=$(docker buildx imagetools inspect "$image:$candidate_tag" \
      --format '{{json .Manifest}}' | jq -r '.digest // empty')
    [[ $candidate_digest == "$index_digest" ]] || {
      echo "$image:$candidate_tag resolved to ${candidate_digest:-no digest}, expected $index_digest" >&2
      exit 1
    }
    mkdir -p "$output_dir"
    printf '%s\n' "$index_digest" > "$output_dir/image-index-digest.txt"
    if [[ -n ${GITHUB_OUTPUT:-} ]]; then
      printf 'digest=%s\n' "$index_digest" >> "$GITHUB_OUTPUT"
    fi
    ;;
  sign | promote | verify)
    digest=${IMAGE_DIGEST:-}
    [[ $digest =~ ^sha256:[0-9a-f]{64}$ ]] || {
      echo "IMAGE_DIGEST must be a sha256 digest" >&2
      exit 1
    }
    if [[ $step == sign ]]; then
      command -v cosign >/dev/null || { echo "cosign is required" >&2; exit 1; }
      cosign sign --yes "$image@$digest"
    elif [[ $step == promote ]]; then
      command -v docker >/dev/null || { echo "docker is required" >&2; exit 1; }
      command -v jq >/dev/null || { echo "jq is required" >&2; exit 1; }
      metadata=$(mktemp)
      trap 'rm -f "$metadata"' EXIT
      aliases=("$version")
      for alias in "$minor" latest; do
        if alias_accepts_version "$alias"; then
          aliases+=("$alias")
        else
          status=$?
          ((status == 1)) || exit "$status"
        fi
      done
      tags=()
      for alias in "${aliases[@]}"; do
        tags+=(--tag "$image:$alias")
      done
      docker buildx imagetools create \
        "${tags[@]}" \
        --metadata-file "$metadata" "$image@$digest"
      promoted_digest=$(jq -r '."containerimage.descriptor".digest // empty' "$metadata")
      [[ $promoted_digest == "$digest" ]] || {
        echo "promoted index is $promoted_digest, expected $digest" >&2
        exit 1
      }
      for alias in "${aliases[@]}"; do
        alias_digest=$(docker buildx imagetools inspect "$image:$alias" \
          --format '{{json .Manifest}}' | jq -r '.digest // empty')
        [[ $alias_digest == "$digest" ]] || {
          echo "$image:$alias resolved to ${alias_digest:-no digest}, expected $digest" >&2
          exit 1
        }
      done
    else
      command -v cosign >/dev/null || { echo "cosign is required" >&2; exit 1; }
      identity="https://github.com/tyk-swe/olp/.github/workflows/release.yml@refs/tags/$tag"
      cosign verify --certificate-identity "$identity" \
        --certificate-oidc-issuer https://token.actions.githubusercontent.com \
        "$image@$digest" >/dev/null
    fi
    ;;
  *)
    echo "unknown RELEASE_MANIFEST_STEP: $step" >&2
    exit 2
    ;;
esac
