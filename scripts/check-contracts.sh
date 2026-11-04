#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
(( $# == 0 )) || { echo "Usage: $0" >&2; exit 2; }
# The contract is canonical two-space JSON, so an edit diffs as exactly the
# members it changes.
node -e '
const raw = require("fs").readFileSync("management/management.json", "utf8");
if (raw !== JSON.stringify(JSON.parse(raw), null, 2) + "\n") {
  console.error("management/management.json is not canonical two-space JSON");
  process.exit(1);
}'
make api
git diff --exit-code -- internal/management/contract/types.gen.go management/operations.gen.json
