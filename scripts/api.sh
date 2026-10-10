#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
node scripts/generate-go-contract.mjs
node scripts/generate-management-operations.mjs
pnpm --dir console api:generate
