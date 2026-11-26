#!/usr/bin/env bash
set -euo pipefail
bin=${1:?set the directory to link the client binaries into}
arch=$(uname -m)
[[ $arch == x86_64 ]] || { printf 'qualification clients require x86_64, found %s\n' "$arch" >&2; exit 1; }
clients="$(cd "$(dirname "$0")/.." && pwd)/tests/qualification/clients"
pnpm --dir "$clients" install --frozen-lockfile --ignore-scripts
modules="$clients/node_modules"
mkdir -p "$bin"
declare -A binaries=(
  [codex]="$modules/@openai/codex-linux-x64/vendor/x86_64-unknown-linux-musl/bin/codex"
  [claude]="$modules/@anthropic-ai/claude-code-linux-x64/claude"
  [opencode]="$modules/opencode-linux-x64/bin/opencode"
)
for name in codex claude opencode; do
  target=${binaries[$name]}
  [[ -x $target ]] || { printf 'missing expected qualification client binary: %s\n' "$target" >&2; exit 1; }
  ln -sf "$target" "$bin/$name"
done
