#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
node scripts/generate-go-contract.mjs
node scripts/generate-management-operations.mjs
pnpm --dir console api:generate

go tool oapi-codegen -generate types,client -package guardrails -o sdk/guardrails/client.gen.go openapi/guardrails.json
pnpm --dir console exec openapi-typescript ../openapi/guardrails.json -o ../sdk/guardrails/schema.d.ts

go tool oapi-codegen -generate types,client -package inference -o sdk/inference/client.gen.go openapi/inference.json
pnpm --dir console exec openapi-typescript ../openapi/inference.json -o ../sdk/inference/schema.d.ts
