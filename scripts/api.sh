#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
(( $# <= 1 )) || { echo "Usage: $0 [output-directory]" >&2; exit 2; }
mkdir -p -- "${1:-$PWD}"
output_root=$(cd -- "${1:-$PWD}" && pwd)
mkdir -p -- "$output_root/internal/management/contract" "$output_root/management" "$output_root/guardrails" "$output_root/inference"
node scripts/generate-go-contract.mjs "$output_root/internal/management/contract/types.gen.go"
node scripts/generate-management-operations.mjs "$output_root/management/operations.gen.json"

go tool oapi-codegen -generate types,client -package guardrails -o "$output_root/guardrails/client.gen.go" guardrails/guardrails.json
pnpm --dir console exec openapi-typescript ../guardrails/guardrails.json -o "$output_root/guardrails/schema.d.ts"

node scripts/generate-inference-contract.mjs "$output_root/inference/client.gen.go"
pnpm --dir console exec openapi-typescript ../inference/inference.json -o "$output_root/inference/schema.d.ts"
