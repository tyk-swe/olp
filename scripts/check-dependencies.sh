#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
# Tools are pinned independently of runtime dependencies.
go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...
# nullable v1.2.0 carries the standard Apache-2.0 notice, rather than the
# full text the classifier requires. Pin the reviewed notice, so an upstream
# license change cannot inherit this classification silently.
nullable_dir=$(go list -m -f '{{.Dir}}' github.com/oapi-codegen/nullable)
printf '%s  %s\n' d41218ff141de28756b85f79958bc33e077e9bfa6bd695b5e2b8736b779f5a9d "$nullable_dir/LICENSE" | sha256sum --check --status
go run github.com/google/go-licenses/v2@v2.0.1 check ./cmd/olp --ignore=github.com/oapi-codegen/nullable \
  --allowed_licenses=AGPL-3.0,AGPL-3.0-only,Apache-2.0,BSD-2-Clause,BSD-3-Clause,ISC,MIT,MPL-2.0,Unicode-DFS-2016,Unicode-3.0
pnpm audit --prod --audit-level high
# GLIDE's prebuilt Rust core is not traversed by Go's vulnerability database.
# Image qualification additionally requires its shipped notices, native linking
# inventory, and scans of both the runtime image and its build-stage SBOMs.
glide_dir=$(go list -m -f '{{.Dir}}' github.com/valkey-io/valkey-glide/go/v2)
test -s "$glide_dir/LICENSE"
test -s "$glide_dir/THIRD_PARTY_LICENSES_GO"

./scripts/native-sbom.py --check
