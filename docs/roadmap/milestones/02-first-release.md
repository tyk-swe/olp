# Milestone 2 — First published release: `v2.2.0`

| | |
|---|---|
| Dates | Mon 2026-09-07 → Sun 2026-09-13 |
| Goal | A user can `docker compose up` or `helm install` from artifacts CI built, signed, and attached to a GitHub Release, without a Rust toolchain |
| Backlog items | REL-01, REL-02, REL-03, REL-04, REL-05 |
| Prerequisites | Milestone 1 exit criteria (green `main`); GHCR write access for the `GITHUB_TOKEN`; the maintainer can create the `live-providers`-style environment protections if wanted for releases |
| Why 2.2.0 and not 2.1.2 | `[Unreleased]` already carries client-visible changes (list responses gain `items`, Anthropic unknown-block refusal, `Retry-After` 429s count toward the circuit) and migrations 0044–0047 |

## REL-01 — Release workflow (M)

- [ ] `.github/workflows/release.yml`: `on.push.tags: ['v*']` plus `workflow_dispatch` with a `dry_run` input; `permissions: contents: write, packages: write, id-token: write`; concurrency group per tag
- [ ] Job `verify`: checkout, `make release-version`, and a new `make release-verify` running `scripts/check-release-tag.sh`, which asserts the tag equals the version in `Cargo.toml` `[workspace.package]`, `console/package.json`, `deploy/helm/Chart.yaml` (`version` and `appVersion`), and `deploy/Dockerfile` `ARG OLP_VERSION`
- [ ] Extend `scripts/check-ci-make-lockstep.sh` to every workflow under `.github/workflows/`; every command step in `release.yml` dispatches through `make` (`release-verify`, `release-image`, `release-manifest`, `release-chart`, `release-notes`)
- [ ] Every new action pinned by SHA and registered in `scripts/check-supply-chain-pins.sh`; `make supply-chain` and `actionlint` clean
- [ ] `scripts/test-repository-validation.sh` gains a case for the tag check

## REL-02 — Image publication (M)

- [ ] Build natively per architecture: `ubuntu-24.04` (amd64) and `ubuntu-24.04-arm` (arm64), each `docker/build-push-action` with `push: true`, `outputs: type=image,push-by-digest=true`, reusing the `type=gha` cache scopes from `ci.yml`
- [ ] `make release-manifest`: `docker buildx imagetools create` combining both digests into `ghcr.io/tyk-swe/olp:2.2.0`, `:2.2`, and `:latest`; docs state that production pins the index digest (already the rule in `docs/deployment.md`) and `latest` exists for the quick start
- [ ] `make smoke-image-modes IMAGE=ghcr.io/tyk-swe/olp@<index-digest>` on both runners (`OLP_IMAGE_PLATFORM` set) against the pushed image
- [ ] SBOM per architecture with the already-pinned `anchore/sbom-action`; both `openllmproxy-<arch>.spdx.json` files become release assets
- [ ] cosign keyless: sign the index digest and attest each SBOM (`sigstore/cosign-installer`, pinned); `docs/deployment.md` gains "Verifying a release" with the `cosign verify` invocation and the expected certificate identity `https://github.com/tyk-swe/olp/.github/workflows/release.yml@refs/tags/v2.2.0`
- [ ] Trivy on the pushed index with `exit-code: 1` for HIGH/CRITICAL, same options as `ci.yml`
- [ ] GHCR package set to public and linked to the repository (the OCI `source` label already points at it)

## REL-03 — Helm chart publication (S)

- [ ] `make release-chart`: `helm lint --strict deploy/helm`, `helm package deploy/helm --version <tag> --app-version <tag>`, `helm registry login ghcr.io` with `GITHUB_TOKEN`, `helm push openllmproxy-<tag>.tgz oci://ghcr.io/tyk-swe/charts`
- [ ] Sign the chart with cosign as well
- [ ] Render the pushed chart with `image.digest` set to the release digest and diff against `helm template deploy/helm` — byte-identical
- [ ] `deploy/helm/Chart.yaml`: add the missing `artifacthub.io/*` annotations (changes, images); `make helm-verify` green

## REL-04 — Cut the release (S)

- [ ] Release commit: bump the four version locations to `2.2.0`; rename `## [Unreleased]` to `## [2.2.0] - 2026-09-1x` and add a fresh empty `[Unreleased]`; confirm `release-metadata.env` still names `0043` (the last migration of the previous release) and `make release-version` passes
- [ ] Add an "Upgrading from 2.1.1" paragraph to the 2.2.0 entry: migrations 0044–0047 run in the migration Job / `migrate` service; the N-1 rehearsal covers 0043 → 0047
- [ ] Tag after CI is green on the release commit: `git tag -a v2.2.0 -m "OpenLLMProxy 2.2.0"`, push, watch `release.yml`
- [ ] `make release-notes`: a script extracts the `[2.2.0]` CHANGELOG section into the GitHub Release body — never hand-copied; assets: both SBOMs, chart `.tgz`, `values.schema.json`, `checksums.txt`
- [ ] Optional: annotated tags `v2.0.0`, `v2.0.1`, `v2.1.0`, `v2.1.1` at the commits that set those versions, so `git describe` and the README badge history make sense

## REL-05 — Quick start pulls (S)

- [ ] `deploy/compose.yaml`: `image: ${OLP_IMAGE:-ghcr.io/tyk-swe/olp:2.2.0}`; move the `build:` block into a new `deploy/compose.build.yaml` overlay for contributors
- [ ] README Quick start: drop `--build`; one line points contributors at the build overlay; keep the bootstrap-token flow unchanged
- [ ] Re-run the README quick start verbatim on a clean machine and time it (target: under 3 minutes to the setup form)
- [ ] `docs/deployment.md` install example uses the real chart version and a real index digest
- [ ] `scripts/prepare-compose-secrets.sh` and `scripts/retire-compose-bootstrap-secret.sh` unchanged; `make e2e` still green (the harness builds its own binary)

## Exit criteria

- [ ] `docker pull ghcr.io/tyk-swe/olp:2.2.0` works anonymously on amd64 and arm64
- [ ] `helm install olp oci://ghcr.io/tyk-swe/charts/openllmproxy --version 2.2.0 …` renders and starts on a kind cluster
- [ ] GitHub Release `v2.2.0` exists with body, SBOMs, chart, and checksums; `cosign verify` succeeds for image and chart
- [ ] README quick start completes without a Rust toolchain in under 3 minutes

## Carry-over

_None yet._
