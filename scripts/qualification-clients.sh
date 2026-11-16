#!/usr/bin/env bash
set -euo pipefail
bin=${1:?set the directory to link the client binaries into}
clients="$(cd "$(dirname "$0")/.." && pwd)/tests/qualification/clients"
pnpm --dir "$clients" install --frozen-lockfile --ignore-scripts
modules="$clients/node_modules"
mkdir -p "$bin"
ln -sf "$modules/@openai/codex-linux-x64/vendor/x86_64-unknown-linux-musl/bin/codex" "$bin/codex"
ln -sf "$modules/@anthropic-ai/claude-code-linux-x64/claude" "$bin/claude"
ln -sf "$modules/opencode-linux-x64/bin/opencode" "$bin/opencode"
