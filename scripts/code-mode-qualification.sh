#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."

usage() { echo 'usage: code-mode-qualification.sh [cli|process|all|resolve] | install [codex|claude-code|opencode]' >&2; exit 2; }
mode=${1:-cli}
case "$mode" in cli|process|all|resolve) (( $# <= 1 )) || usage ;; install) (( $# <= 2 )) || usage ;; *) usage ;; esac
if [[ "$mode" == resolve ]]; then
  # Always refresh metadata, even when a previous run's environment is present.
  exec node scripts/lib/codecli-install.mjs
fi
if [[ "$mode" == install ]]; then
  case "${2:-codex}" in codex|claude-code|opencode) ;; *) usage ;; esac
  exec node scripts/lib/codecli-install.mjs "${2:-codex}"
fi
if [[ -z ${OLP_CODECLI_ENV:-} ]]; then
  OLP_CODECLI_ENV=$(./scripts/code-mode-qualification.sh resolve)
fi
source "$OLP_CODECLI_ENV"
if [[ "$mode" == cli || "$mode" == all ]]; then
  go test -mod=readonly -race -tags=codecli -count=1 -timeout=10m -v ./tests/codecli
fi
if [[ "$mode" == process || "$mode" == all ]]; then
  : "${OLP_TEST_DATABASE_URL:?Provision integration PostgreSQL first}"
  : "${OLP_TEST_VALKEY_URL:?Provision integration Valkey first}"
  : "${OLP_TEST_BINARY:?Build the integrated OLP binary first}"
  go test -mod=readonly -race -tags=integration,codecli -count=1 -timeout=20m -v \
    -run '^TestCodeQualification' ./tests/integration
fi
