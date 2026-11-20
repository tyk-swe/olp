#!/usr/bin/env bash
set -euo pipefail

image=${1:?usage: OLP_IMAGE_PLATFORM=linux/amd64 $0 IMAGE}
platform=${OLP_IMAGE_PLATFORM:?set the image platform}
command -v docker >/dev/null || { echo "docker is required" >&2; exit 1; }

platform_args=(--platform "$platform")

for mode in all gateway control worker migrate doctor; do
  docker run --rm "${platform_args[@]}" "$image" "$mode" --help >/dev/null
done
docker run --rm "${platform_args[@]}" "$image" internal-pre-stop --seconds 0

configured_user=$(docker image inspect "$image" --format '{{.Config.User}}')
case "$configured_user" in
  "" | 0 | 0:0 | root | root:root)
    echo "final image is not configured with a non-root user: ${configured_user:-unset}" >&2
    exit 1
    ;;
esac
expected_arch=${platform#*/}
actual_arch=$(docker image inspect "$image" --format '{{.Architecture}}')
[[ $actual_arch == "$expected_arch" ]] || {
  echo "image architecture mismatch: expected=$expected_arch actual=$actual_arch" >&2
  exit 1
}

work=$(mktemp -d)
container=
cleanup() {
  [[ -z $container ]] || docker rm -f "$container" >/dev/null 2>&1 || true
  rm -rf "$work"
}
trap cleanup EXIT
container=$(docker create "${platform_args[@]}" "$image" all --help)
docker cp "$container:/opt/olp/console/index.html" "$work/index.html"
grep -qi '<html\|<!doctype html' "$work/index.html" || {
  echo "packaged console index is missing or invalid" >&2
  exit 1
}

echo "image smoke passed: image=$image platform=$platform user=$configured_user modes=6"
