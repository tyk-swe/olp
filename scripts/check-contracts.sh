#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
(( $# == 0 )) || { echo "Usage: $0" >&2; exit 2; }
# The contract is canonical two-space JSON, so an edit diffs as exactly the
# members it changes.
node -e '
const raw = require("fs").readFileSync("openapi/management.json", "utf8");
if (raw !== JSON.stringify(JSON.parse(raw), null, 2) + "\n") {
  console.error("openapi/management.json is not canonical two-space JSON");
  process.exit(1);
}'
make api
git diff --exit-code -- internal/management/contract/types.gen.go sdk/management/operations.gen.json
scratch=$(mktemp -d)
trap 'rm -rf -- "$scratch"' EXIT
cp console/src/lib/api/schema.d.ts "$scratch/schema.d.ts"
cp console/src/lib/api/requirements.ts "$scratch/requirements.ts"
pnpm --dir console api:generate
cmp "$scratch/schema.d.ts" console/src/lib/api/schema.d.ts
cmp "$scratch/requirements.ts" console/src/lib/api/requirements.ts
