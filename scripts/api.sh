#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
(( $# <= 1 )) || { echo "Usage: $0 [output-directory]" >&2; exit 2; }
mkdir -p -- "${1:-$PWD}"
output_root=$(cd -- "${1:-$PWD}" && pwd)
mkdir -p -- "$output_root/internal/management/contract" "$output_root/management" "$output_root/guardrails" "$output_root/inference"
index_dir=$(mktemp -d)
trap 'rm -rf -- "$index_dir"' EXIT
node scripts/generate-management-index.mjs management/openapi "$index_dir/management-index.json"
contract="$output_root/management/management.json"
REDOCLY_TELEMETRY=off REDOCLY_SUPPRESS_UPDATE_NOTICE=true \
  pnpm exec redocly bundle "$index_dir/management-index.json" --output "$contract"
printf '\n' >>"$contract"

go tool oapi-codegen --config internal/management/oapi-codegen.yaml -o "$output_root/internal/management/contract/types.gen.go" "$contract"
node scripts/generate-management-operations.mjs "$contract" "$output_root/management/operations.gen.json"

go tool oapi-codegen -generate types,client -package guardrails -o "$output_root/guardrails/client.gen.go" guardrails/guardrails.json
pnpm --dir console exec openapi-typescript ../guardrails/guardrails.json -o "$output_root/guardrails/schema.d.ts"

node scripts/generate-inference-contract.mjs "$output_root/inference/client.gen.go"
