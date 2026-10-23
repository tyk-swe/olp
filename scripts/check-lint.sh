#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
# Tools are pinned independently of runtime dependencies, as in
# check-dependencies.sh. Enabled checks live in staticcheck.conf.
go run honnef.co/go/tools/cmd/staticcheck@v0.8.2 ./...
go run honnef.co/go/tools/cmd/staticcheck@v0.8.2 -tags=bench ./tests/bench/...
