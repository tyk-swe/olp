#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
# Tools are pinned independently of runtime dependencies, as in
# check-dependencies.sh. Enabled checks live in staticcheck.conf.
case ${1:-} in
  '') bench=true ;;
  --no-bench) bench=false ;;
  *) echo "Usage: $0 [--no-bench]" >&2; exit 2 ;;
esac
go run honnef.co/go/tools/cmd/staticcheck@v0.8.2 ./...
if [[ $bench == true ]]; then
  go run honnef.co/go/tools/cmd/staticcheck@v0.8.2 -tags=bench ./tests/bench/...
fi
