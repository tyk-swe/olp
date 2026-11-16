#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
# Tools are pinned independently of runtime dependencies, as in
# check-dependencies.sh. Enabled checks live in staticcheck.conf.
go run honnef.co/go/tools/cmd/staticcheck@v0.8.2 ./...

./scripts/check-imports.sh

gateway=github.com/tyk-swe/olp/internal/gateway
declare -A gateway_imports=(
  [fault]="" [events]="" [traceingest]=""
  [health]="events" [ingress]="fault" [quota]="events fault" [models]="fault ingress"
  [codeforward]="events fault ingress quota codeforward/coderesearch codeforward/codexwire" [delegation]="fault ingress"
  [codeforward/coderesearch]="" [codeforward/codexwire]=""
)
while read -r package imports; do
  [[ $package == "$gateway" ]] && continue
  name=${package#"$gateway/"}
  if [[ ! -v gateway_imports[$name] ]]; then
    echo "check-lint: declare which gateway packages $package may import in scripts/check-lint.sh" >&2
    exit 1
  fi
  for imported in $imports; do
    [[ $imported == "$gateway/"* ]] || continue
    if [[ " ${gateway_imports[$name]} " != *" ${imported#"$gateway/"} "* ]]; then
      echo "check-lint: $package must not import $imported" >&2
      exit 1
    fi
  done
done < <(go list -f '{{.ImportPath}} {{join .Imports " "}}' ./internal/gateway/...)
