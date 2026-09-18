#!/usr/bin/env bash
set -euo pipefail
image=${1:?set an immutable image reference}
reports=${2:?set a report directory}
[[ $image == *@sha256:* ]] || { echo 'Image scan requires a digest.' >&2; exit 2; }
mkdir -p "$reports"
reports=$(cd "$reports" && pwd)
scanner=aquasec/trivy@sha256:62b1e65e8869bc4b4c6aa4fa2b21595256c7c2f6018a9d9ad61caf87187c1969
args=(--rm -v /var/run/docker.sock:/var/run/docker.sock
  --mount "type=bind,src=$reports,dst=/reports")
status=0
docker run "${args[@]}" "$scanner" image --scanners vuln --format json \
  --cache-dir /reports/cache --output /reports/runtime-vulnerabilities.json \
  --severity HIGH,CRITICAL --exit-code 1 "$image" || status=$?
docker buildx imagetools inspect "$image" --format '{{json .SBOM}}' > "$reports/sboms.json"
for ecosystem in golang npm; do
  jq -e --arg prefix "pkg:$ecosystem/" \
    '[.. | objects | .referenceLocator? // empty | select(startswith($prefix))] | length > 0' \
    "$reports/sboms.json" >/dev/null || { echo "The inventory is missing $ecosystem packages." >&2; exit 1; }
done
# The shipped GLIDE archive is native Rust-derived code. Its prebuilt sources
# and licenses remain part of the inventory even though Cargo is never run.
jq -e '[.. | objects | .name? // empty | select(test("valkey-glide"))] | length > 0' \
  "$reports/sboms.json" >/dev/null
container=$(docker create "$image")
docker cp "$container:/usr/share/doc/openllmproxy" "$reports/native-inventory"
docker rm "$container" >/dev/null
test -s "$reports/native-inventory/GLIDE-THIRD-PARTY-LICENSES"
test -s "$reports/native-inventory/native-link.txt"
count=0
while IFS= read -r document; do
  count=$((count + 1))
  printf '%s\n' "$document" > "$reports/sbom-$count.json"
  docker run "${args[@]}" "$scanner" sbom --format json --cache-dir /reports/cache \
    --output "/reports/sbom-$count-vulnerabilities.json" --severity HIGH,CRITICAL \
    --exit-code 1 "/reports/sbom-$count.json" || status=$?
done < <(jq -c '.. | objects | select(has("spdxVersion") and has("packages"))' "$reports/sboms.json")
(( count > 0 )) || { echo 'The candidate is missing its SPDX inventory.' >&2; exit 1; }
test -s "$reports/native-inventory/native/valkey-glide.spdx.json"
docker run "${args[@]}" "$scanner" sbom --cache-dir /reports/cache \
  --severity HIGH,CRITICAL --exit-code 1 --format json \
  --output /reports/native-rust-vulnerabilities.json \
  /reports/native-inventory/native/valkey-glide.spdx.json || status=$?
printf 'Scanned %s, %s build inventories, and the native FFI inventory.\n' "$image" "$count"
exit "$status"
