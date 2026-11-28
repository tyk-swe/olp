#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."

golangci_version=v2.15.0
lint_bin=".local/bin/golangci-lint-$golangci_version"
if [[ ! -x $lint_bin ]]; then
  mkdir -p .local/bin
  find .local/bin -maxdepth 1 -name 'golangci-lint-*' -delete
  tools=$(mktemp -d)
  trap 'rm -rf -- "$tools"' EXIT
  GOBIN=$tools go install "github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$golangci_version"
  mv -f "$tools/golangci-lint" "$lint_bin"
  rm -rf -- "$tools"
  trap - EXIT
fi

"$lint_bin" run --show-stats=false ./...

base=${LINT_BASE:-origin/main}
if ! git merge-base "$base" HEAD >/dev/null; then
  echo "check-lint: cannot find where this branch left $base, fetch it or set LINT_BASE" >&2
  exit 1
fi
"$lint_bin" run --show-stats=false --enable-only errcheck,forcetypeassert --new-from-merge-base "$base" ./...

gateway=github.com/tyk-swe/olp/internal/gateway
gateway_list=$(go list -f '{{.ImportPath}} {{join .Imports " "}}' ./internal/gateway/...)
while IFS=' ' read -r package imports; do
  [[ $package == "$gateway" ]] && continue
  allowlist="internal/gateway/${package#"$gateway/"}/imports.txt"
  if [[ ! -f $allowlist ]]; then
    echo "check-lint: declare which gateway packages $package may import in $allowlist, one per line" >&2
    exit 1
  fi
  read -r -a imported_list <<<"$imports"
  for imported in "${imported_list[@]}"; do
    [[ $imported == "$gateway/"* ]] || continue
    if ! grep -qxF -- "${imported#"$gateway/"}" "$allowlist"; then
      echo "check-lint: $package must not import $imported" >&2
      exit 1
    fi
  done
done <<<"$gateway_list"

internal_list=$(go list -f '{{.ImportPath}} {{join .Imports " "}}' ./internal/...)
edges=$(awk -v root=github.com/tyk-swe/olp/internal/ '
  function module(path, rest) {
    if (index(path, root) != 1) return ""
    rest = substr(path, length(root) + 1)
    sub(/\/.*/, "", rest)
    return rest
  }
  {
    from = module($1)
    for (i = 2; i <= NF; i++) {
      to = module($i)
      if (to != "" && to != from) print from, to
    }
  }' <<<"$internal_list" | sort -u)
if ! loop=$(tsort <<<"$edges" 2>&1 >/dev/null); then
  echo "$loop" >&2
  echo "check-lint: modules under internal/ import each other in a cycle. Move the shared piece into internal/domain or inject it from cmd/olp" >&2
  exit 1
fi

if branches=$(git grep -nE '(VendorID|(Provider|provider|cfg|config)\.Kind) *(==|!=) *"|connectors\.Kind[A-Z]' -- 'internal/gateway/kernel/*.go' ':!*_test.go'); then
  echo "$branches" >&2
  echo "check-lint: the kernel must not branch on a provider kind or vendor. Declare the fact on connectors.Config or domain/vendors, or move the branch into its surface" >&2
  exit 1
fi
