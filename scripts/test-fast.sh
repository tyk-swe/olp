#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
budget=60
start=$(date +%s)
go test -mod=readonly -vet=off -count=1 -timeout=60s \
  ./cmd/olp \
  ./internal/agents \
  ./internal/agents/a2a \
  ./internal/agents/publications \
  ./internal/secrets \
  ./internal/access \
  ./internal/billing \
  ./internal/usage \
  ./internal/protocols \
  ./internal/protocols/wire \
  ./internal/protocols/openai \
  ./internal/protocols/sse \
  ./internal/operations/tokenization/estimate \
  ./internal/catalog \
  ./internal/limits \
  ./internal/release \
  ./internal/routing \
  ./internal/guardrails \
  ./internal/signing \
  ./internal/mcp \
  ./internal/mcp/messages \
  ./internal/mcp/sessions \
  ./internal/mcp/accounting \
  ./internal/mcp/upstream \
  ./internal/mcp/registration \
  ./internal/grants/personal \
  ./internal/gateway \
  ./internal/codemode \
  ./internal/codeadapter \
  ./internal/codexwire
pnpm --dir console test --project unit --project svelte
node --test scripts/check-docs.test.mjs scripts/check-release-version.test.mjs
elapsed=$(( $(date +%s) - start ))
printf 'test-fast: %ss (budget %ss)\n' "$elapsed" "$budget"
if [ "$elapsed" -gt "$budget" ]; then
  printf 'test-fast: exceeded %ss budget\n' "$budget" >&2
  exit 1
fi
