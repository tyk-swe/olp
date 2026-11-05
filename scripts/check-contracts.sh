#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
(( $# == 0 )) || { echo "Usage: $0" >&2; exit 2; }
node -e '
const raw = require("fs").readFileSync("management/management.json", "utf8");
if (raw !== JSON.stringify(JSON.parse(raw), null, 2) + "\n") {
  console.error("management/management.json is not canonical two-space JSON");
  process.exit(1);
}'
scratch=$(mktemp -d)
trap 'rm -rf -- "$scratch"' EXIT
./scripts/api.sh "$scratch"
while IFS= read -r -d '' generated; do
  if ! cmp -s -- "$generated" "${generated#"$scratch"/}"; then
    printf '%s is stale or missing. Run make api\n' "${generated#"$scratch"/}" >&2
    exit 1
  fi
done < <(find "$scratch" -type f -print0)
echo 'Generated contracts are current'
