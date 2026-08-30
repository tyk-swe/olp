#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
step=${RELEASE_IMAGE_STEP:-smoke}
dry_run=${DRY_RUN:-false}

case $step in
  record)
    [[ $dry_run != true ]] || exit 0
    arch=${RELEASE_ARCH:-}
    image=${RELEASE_IMAGE:-ghcr.io/tyk-swe/olp}
    index_digest=${RELEASE_IMAGE_INDEX_DIGEST:-}
    platform=${RELEASE_IMAGE_PLATFORM:-}
    output_dir=${RELEASE_DIGEST_DIR:-}
    [[ $arch == amd64 || $arch == arm64 ]] || {
      echo "RELEASE_ARCH must be amd64 or arm64" >&2
      exit 1
    }
    [[ $index_digest =~ ^sha256:[0-9a-f]{64}$ ]] || {
      echo "RELEASE_IMAGE_INDEX_DIGEST must be a sha256 digest" >&2
      exit 1
    }
    [[ $platform == "linux/$arch" ]] || {
      echo "RELEASE_IMAGE_PLATFORM must match linux/$arch" >&2
      exit 1
    }
    [[ -n $output_dir ]] || {
      echo "RELEASE_DIGEST_DIR is required" >&2
      exit 1
    }
    command -v docker >/dev/null || { echo "docker is required" >&2; exit 1; }
    command -v jq >/dev/null || { echo "jq is required" >&2; exit 1; }
    manifest=$(docker buildx imagetools inspect "$image@$index_digest" \
      --format '{{json .Manifest}}')
    digest=$(jq -r --arg arch "$arch" '
      [.manifests[] |
        select(.platform.os == "linux" and .platform.architecture == $arch) |
        select(.annotations["vnd.docker.reference.type"] != "attestation-manifest") |
        .digest] |
      if length == 1 then .[0] else empty end
    ' <<< "$manifest")
    [[ $digest =~ ^sha256:[0-9a-f]{64}$ ]] || {
      echo "$image@$index_digest does not contain exactly one linux/$arch image manifest" >&2
      exit 1
    }
    mkdir -p "$output_dir"
    printf '%s\n' "$index_digest" > "$output_dir/$arch.digest"
    if [[ -n ${GITHUB_OUTPUT:-} ]]; then
      printf 'digest=%s\n' "$digest" >> "$GITHUB_OUTPUT"
    fi
    ;;
  smoke)
    if [[ $dry_run == true && -z ${IMAGE:-} ]]; then
      echo "published index smoke is skipped during a dry run"
      exit 0
    fi
    [[ -n ${IMAGE:-} ]] || {
      echo "IMAGE is required" >&2
      exit 1
    }
    "$script_dir/smoke-image-modes.sh" "$IMAGE"
    ;;
  attest)
    [[ $dry_run != true ]] || exit 0
    [[ ${IMAGE:-} =~ @sha256:[0-9a-f]{64}$ ]] || {
      echo "IMAGE must be an immutable digest reference" >&2
      exit 1
    }
    [[ -f ${RELEASE_SBOM:-} ]] || {
      echo "RELEASE_SBOM must name an existing SPDX JSON file" >&2
      exit 1
    }
    command -v cosign >/dev/null || { echo "cosign is required" >&2; exit 1; }
    cosign attest --yes --type spdxjson --predicate "$RELEASE_SBOM" "$IMAGE"
    ;;
  *)
    echo "unknown RELEASE_IMAGE_STEP: $step" >&2
    exit 2
    ;;
esac
