#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
node scripts/generate-go-contract.mjs
pnpm --dir console api:generate
