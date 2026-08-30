#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
release_dir=${RELEASE_DIR:-$root/dist/release}
dry_run=${DRY_RUN:-false}

tag=${RELEASE_TAG:-}
if [[ -z $tag && ${GITHUB_REF_TYPE:-} == tag ]]; then
  tag=${GITHUB_REF_NAME:-}
fi
if [[ -z $tag && $dry_run == true ]]; then
  version=$(awk '
    /^\[workspace.package\]$/ { workspace = 1; next }
    /^\[/ { workspace = 0 }
    workspace && /^version = "/ {
      value = $0
      sub(/^version = "/, "", value)
      sub(/"$/, "", value)
      print value
      exit
    }
  ' "$root/Cargo.toml")
  tag="v$version"
fi
[[ $tag =~ ^v(0|[1-9][0-9]*)\.([0-9]+)\.([0-9]+)$ ]] || {
  echo "RELEASE_TAG must match vMAJOR.MINOR.PATCH" >&2
  exit 1
}
version=${tag#v}

mkdir -p "$release_dir"
notes="$release_dir/release-notes.md"
"$root/scripts/extract-release-notes.sh" "$tag" "$root/CHANGELOG.md" "$notes"
assets=(
  "openllmproxy-amd64.spdx.json"
  "openllmproxy-arm64.spdx.json"
  "openllmproxy-$version.tgz"
  "values.schema.json"
)
for asset in "${assets[@]}"; do
  [[ -s $release_dir/$asset ]] || {
    echo "release asset is missing or empty: $asset" >&2
    exit 1
  }
done
(
  cd "$release_dir"
  sha256sum "${assets[@]}" | LC_ALL=C sort -k2
) > "$release_dir/checksums.txt"

if [[ $dry_run == true ]]; then
  echo "release notes and checksums prepared without publishing: $release_dir"
  exit 0
fi

image_digest=
chart_digest=
[[ ! -f $release_dir/image-index-digest.txt ]] || \
  image_digest=$(<"$release_dir/image-index-digest.txt")
[[ ! -f $release_dir/chart-oci-digest.txt ]] || \
  chart_digest=$(<"$release_dir/chart-oci-digest.txt")
[[ $image_digest =~ ^sha256:[0-9a-f]{64}$ ]] || {
  echo "release image index digest is missing or invalid" >&2
  exit 1
}
[[ $chart_digest =~ ^sha256:[0-9a-f]{64}$ ]] || {
  echo "release chart digest is missing or invalid" >&2
  exit 1
}
chart_registry=${RELEASE_CHART_REGISTRY:-oci://ghcr.io/tyk-swe/charts}
printf '\n\n## Artifact digests\n\n- Image: \x60%s@%s\x60\n- Helm chart: \x60%s/openllmproxy@%s\x60\n' \
  "${RELEASE_IMAGE:-ghcr.io/tyk-swe/olp}" "$image_digest" \
  "${chart_registry#oci://}" "$chart_digest" \
  >> "$notes"

[[ -n ${GH_REPOSITORY:-} && -n ${GH_TOKEN:-} ]] || {
  echo "GH_REPOSITORY and GH_TOKEN are required" >&2
  exit 1
}
command -v gh >/dev/null || { echo "gh is required" >&2; exit 1; }
release_assets=("${assets[@]}" checksums.txt)
for index in "${!release_assets[@]}"; do
  release_assets[index]="$release_dir/${release_assets[index]}"
done
gh release create "$tag" "${release_assets[@]}" \
  --repo "$GH_REPOSITORY" --verify-tag --title "OpenLLMProxy $version" \
  --notes-file "$notes"
