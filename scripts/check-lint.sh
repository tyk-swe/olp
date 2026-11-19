#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
# Tools are pinned independently of runtime dependencies, as in
# check-dependencies.sh. Enabled checks live in staticcheck.conf.
go run honnef.co/go/tools/cmd/staticcheck@v0.8.2 -tags integration,e2e,qualification,qualificationlive ./...

./scripts/check-imports.sh

# A management write goes through access.Mutate, Remove or Transact. This list
# only shrinks, see "Adding a management endpoint" in docs/develop/architecture.md.
hand_rolled="internal/mcp/registration/server.go
internal/media/jobs/api.go
internal/promotion/serve/server.go
internal/providers/credentials.go
internal/providers/enrollments.go
internal/release/delegation_management.go
internal/resources/retained_management.go"
found=$(grep -rlE '\.(BeginMutation|CompleteReplay)\(r[,.)]' --include='*.go' --exclude='*_test.go' internal cmd | grep -v '^internal/access/' | LC_ALL=C sort)
if [[ $found != "$hand_rolled" ]]; then
  echo "check-lint: use access.Mutate, Remove or Transact instead of BeginMutation, or drop a migrated file from hand_rolled in scripts/check-lint.sh" >&2
  diff <(echo "$hand_rolled") <(echo "$found") >&2 || true
  exit 1
fi

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
