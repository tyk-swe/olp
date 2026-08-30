#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
chart=${RELEASE_CHART_DIR:-$root/deploy/helm}
output_dir=${RELEASE_DIR:-$root/dist/release}
registry=${RELEASE_CHART_REGISTRY:-oci://ghcr.io/tyk-swe/charts}
dry_run=${DRY_RUN:-false}
step=${RELEASE_CHART_STEP:-publish}

tag=${RELEASE_TAG:-}
if [[ -z $tag && ${GITHUB_REF_TYPE:-} == tag ]]; then
  tag=${GITHUB_REF_NAME:-}
fi
if [[ -z $tag && $dry_run == true ]]; then
  tag="v$(sed -nE 's/^version: "?([^"[:space:]]+)"?$/\1/p' "$chart/Chart.yaml")"
fi
[[ $tag =~ ^v(0|[1-9][0-9]*)\.([0-9]+)\.([0-9]+)$ ]] || {
  echo "RELEASE_TAG must match vMAJOR.MINOR.PATCH" >&2
  exit 1
}
version=${tag#v}

command -v helm >/dev/null || { echo "helm is required" >&2; exit 1; }
if [[ $step == verify-public ]]; then
  [[ ${IMAGE_DIGEST:-} =~ ^sha256:[0-9a-f]{64}$ ]] || {
    echo "IMAGE_DIGEST must be the release index digest" >&2
    exit 1
  }
  [[ ${CHART_DIGEST:-} =~ ^sha256:[0-9a-f]{64}$ ]] || {
    echo "CHART_DIGEST must be the release chart digest" >&2
    exit 1
  }
  command -v cosign >/dev/null || { echo "cosign is required" >&2; exit 1; }
  work=$(mktemp -d)
  trap 'rm -rf "$work"' EXIT
  chart_reference="$registry/openllmproxy"
  if pull_output=$(helm pull "$chart_reference" --version "$version" \
    --destination "$work" 2>&1); then
    :
  else
    status=$?
    printf '%s\n' "$pull_output" >&2
    exit "$status"
  fi
  printf '%s\n' "$pull_output"
  resolved_digest=$(sed -nE \
    's/^Digest:[[:space:]]+(sha256:[0-9a-f]{64})$/\1/p' <<< "$pull_output")
  [[ $resolved_digest == "$CHART_DIGEST" ]] || {
    echo "public chart tag resolved to ${resolved_digest:-no digest}, expected $CHART_DIGEST" >&2
    exit 1
  }
  helm template olp "$work/openllmproxy-$version.tgz" --namespace olp \
    --set-string image.digest="$IMAGE_DIGEST" > "$work/public.yaml"
  identity="https://github.com/tyk-swe/olp/.github/workflows/release.yml@refs/tags/$tag"
  cosign verify --certificate-identity "$identity" \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com \
    "${chart_reference#oci://}@$CHART_DIGEST" >/dev/null
  echo "public release chart and signature verified: $chart_reference:$version"
  exit 0
fi
[[ $step == publish ]] || { echo "unknown RELEASE_CHART_STEP: $step" >&2; exit 2; }
command -v diff >/dev/null || { echo "diff is required" >&2; exit 1; }
mkdir -p "$output_dir"
helm lint --strict "$chart"
helm package "$chart" --destination "$output_dir" \
  --version "$version" --app-version "$version" >/dev/null
install -m 0644 "$chart/values.schema.json" "$output_dir/values.schema.json"
package="$output_dir/openllmproxy-$version.tgz"
[[ -s $package ]] || { echo "Helm chart package was not produced" >&2; exit 1; }
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
render_digest=${IMAGE_DIGEST:-sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa}
helm template olp "$chart" --namespace olp \
  --set-string image.digest="$render_digest" > "$work/local.yaml"
helm template olp "$package" --namespace olp \
  --set-string image.digest="$render_digest" > "$work/package.yaml"
diff -u "$work/local.yaml" "$work/package.yaml"

if [[ $dry_run == true ]]; then
  echo "release chart prepared without publishing: $package"
  exit 0
fi

[[ ${IMAGE_DIGEST:-} =~ ^sha256:[0-9a-f]{64}$ ]] || {
  echo "IMAGE_DIGEST must be the release index digest" >&2
  exit 1
}
[[ -n ${GITHUB_TOKEN:-} && -n ${GITHUB_ACTOR:-} ]] || {
  echo "GITHUB_TOKEN and GITHUB_ACTOR are required" >&2
  exit 1
}
command -v cosign >/dev/null || { echo "cosign is required" >&2; exit 1; }

registry_config="$work/registry.json"
export HELM_REGISTRY_CONFIG=$registry_config
printf '%s' "$GITHUB_TOKEN" | helm registry login ghcr.io \
  --username "$GITHUB_ACTOR" --password-stdin >/dev/null
if push_output=$(helm push "$package" "$registry" 2>&1); then
  :
else
  status=$?
  printf '%s\n' "$push_output" >&2
  exit "$status"
fi
printf '%s\n' "$push_output"
chart_digest=$(sed -nE 's/^Digest:[[:space:]]+(sha256:[0-9a-f]{64})$/\1/p' \
  <<< "$push_output")
[[ $chart_digest =~ ^sha256:[0-9a-f]{64}$ ]] || {
  echo "Helm did not report a valid chart digest" >&2
  exit 1
}
printf '%s\n' "$chart_digest" > "$output_dir/chart-oci-digest.txt"

chart_reference="$registry/openllmproxy"
cosign sign --yes "${chart_reference#oci://}@$chart_digest"
helm template olp "$chart_reference" --version "$version" --namespace olp \
  --set-string image.digest="$IMAGE_DIGEST" > "$work/remote.yaml"
diff -u "$work/local.yaml" "$work/remote.yaml"
if [[ -n ${GITHUB_OUTPUT:-} ]]; then
  printf 'digest=%s\n' "$chart_digest" >> "$GITHUB_OUTPUT"
fi
