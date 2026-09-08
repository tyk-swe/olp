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
for ecosystem in cargo npm; do
  jq -e --arg prefix "pkg:$ecosystem/" \
    '[.. | objects | .referenceLocator? // empty | select(startswith($prefix))] | length > 0' \
    "$reports/sboms.json" >/dev/null || { echo "The inventory is missing $ecosystem packages." >&2; exit 1; }
done
count=0
while IFS= read -r document; do
  count=$((count + 1))
  printf '%s\n' "$document" > "$reports/sbom-$count.json"
  docker run "${args[@]}" "$scanner" sbom --format json --cache-dir /reports/cache \
    --output "/reports/sbom-$count-vulnerabilities.json" --severity HIGH,CRITICAL \
    --exit-code 1 "/reports/sbom-$count.json" || status=$?
done < <(jq -c '.. | objects | select(has("spdxVersion") and has("packages"))' "$reports/sboms.json")
(( count > 0 )) || { echo 'The candidate is missing its SPDX inventory.' >&2; exit 1; }
printf 'Scanned %s and %s SPDX inventories.\n' "$image" "$count"
exit "$status"
