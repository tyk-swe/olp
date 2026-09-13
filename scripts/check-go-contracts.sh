#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
scratch=$(mktemp -d)
trap 'rm -rf -- "$scratch"' EXIT
./scripts/go-without-rust.sh make go-api
cp internal/management/contract/types.gen.go "$scratch/types.gen.go"
cp console/src/lib/api/schema.d.ts "$scratch/schema.d.ts"
./scripts/go-without-rust.sh make go-api
cmp "$scratch/types.gen.go" internal/management/contract/types.gen.go
cmp "$scratch/schema.d.ts" console/src/lib/api/schema.d.ts
git diff --exit-code -- internal/management/contract/types.gen.go
