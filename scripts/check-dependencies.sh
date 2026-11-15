#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
toolchain=$(sed -n 's/^toolchain go//p' go.mod)
grep -q "^FROM golang:$toolchain-trixie@sha256:" deploy/Dockerfile || { echo "deploy/Dockerfile must build with the go.mod toolchain go$toolchain." >&2; exit 1; }
package_manager=$(node -p "require('./package.json').packageManager")
grep -qxF "RUN npm install --global $package_manager" deploy/Dockerfile || { echo "deploy/Dockerfile must install the package.json $package_manager." >&2; exit 1; }
# Tools are pinned independently of runtime dependencies.
findings=$(go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 -format json ./...)
# GO-2026-6909 has no fixed crewjam/saml release. samlidentity.Verify requires
# exactly one Issuer and one NameID, both in the SAML assertion namespace,
# before the library parses the assertion, which closes both reported paths.
called=$(jq -r 'select(.finding.trace[0].function) | .finding.osv' <<<"$findings" | sort -u | grep -vxF GO-2026-6909 || true)
[[ -z $called ]] || { printf 'The code calls symbols affected by %s. Run govulncheck ./... for traces.\n' "$called" >&2; exit 1; }
# nullable v1.2.0 carries the standard Apache-2.0 notice, rather than the
# full text the classifier requires. Pin the reviewed notice, so an upstream
# license change cannot inherit this classification silently.
nullable_dir=$(go list -m -f '{{.Dir}}' github.com/oapi-codegen/nullable)
printf '%s  %s\n' d41218ff141de28756b85f79958bc33e077e9bfa6bd695b5e2b8736b779f5a9d "$nullable_dir/LICENSE" | sha256sum --check --status
# arrow/go ships the Arrow monorepo LICENSE.txt, whose Apache-2.0 text is
# followed by notices for vendored C++ sources absent from the Go module.
arrow_dir=$(go list -m -f '{{.Dir}}' github.com/apache/arrow/go/v15)
printf '%s  %s\n' bd939b3a0ca7ffedd7444240915f4f297f09739fa9d07bac8c85ec4ea8c94c35 "$arrow_dir/LICENSE.txt" | sha256sum --check --status
# segmentio/asm v1.2.1 is MIT No Attribution, which the classifier reports as
# unknown. Pin the reviewed notice.
asm_dir=$(go list -m -f '{{.Dir}}' github.com/segmentio/asm)
printf '%s  %s\n' cca993712df289a5958bdef69031a5dac0f951ac15afeb313f9eeea55ed59443 "$asm_dir/LICENSE" | sha256sum --check --status
go run github.com/google/go-licenses/v2@v2.0.1 check ./cmd/olp --ignore=github.com/oapi-codegen/nullable --ignore=github.com/apache/arrow/go/v15 --ignore=github.com/segmentio/asm \
  --allowed_licenses=AGPL-3.0,AGPL-3.0-only,Apache-2.0,BSD-2-Clause,BSD-3-Clause,ISC,MIT,MPL-2.0,Unicode-DFS-2016,Unicode-3.0
# The OpenAI rank files embedded for token estimation are data under tiktoken's
# MIT license, which ships beside them and in the image. Pin the reviewed notice.
tiktoken_notice=internal/operations/tokenization/estimate/ranks/LICENSE
printf '%s  %s\n' 418cb499b436128d653d79941333a5437b7be2ea9213dcc2f04d15d5d2c51d86 "$tiktoken_notice" | sha256sum --check --status
pnpm audit --prod --audit-level high
