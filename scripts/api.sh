#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
node scripts/generate-go-contract.mjs
node scripts/generate-management-operations.mjs
pnpm --dir console api:generate

go tool oapi-codegen -generate types,client -package guardrails -o guardrails/client.gen.go guardrails/guardrails.json
pnpm --dir console exec openapi-typescript ../guardrails/guardrails.json -o ../guardrails/schema.d.ts

node scripts/generate-inference-contract.mjs
pnpm --dir console exec openapi-typescript ../inference/inference.json -o ../inference/schema.d.ts
