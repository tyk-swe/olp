#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
source scripts/lib/test.sh

start_services
go_suite 'integration' 30 -tags integration -parallel 8 -timeout=120s ./tests/integration "$@"
