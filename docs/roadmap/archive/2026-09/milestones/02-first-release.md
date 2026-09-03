# Milestone 2 — First published release: `v2.2.0`

| | |
|---|---|
| Dates | Mon 2026-09-07 → Sun 2026-09-13 |
| Goal | A user can `docker compose up` or `helm install` from artifacts CI built, signed, and attached to a GitHub Release, without a Rust toolchain |
| Backlog items | REL-01, REL-02, REL-03, REL-04, REL-05 |
| Prerequisites | Milestone 1 exit criteria (green `main`); GHCR write access for the `GITHUB_TOKEN`; after the first registry push creates the packages, a maintainer can make both `olp` and `charts/openllmproxy` public and rerun the anonymous verification jobs |
| Why 2.2.0 and not 2.1.2 | The release carries client-visible changes (list responses gain `items`, Anthropic unknown-block refusal, `Retry-After` 429s count toward the circuit) and migrations 0044–0048 |

The exit proof found two quick-start defects in `v2.2.0`. Patch release
[`v2.2.1`](https://github.com/tyk-swe/olp/releases/tag/v2.2.1) repairs them;
the pull-based exit criteria below use that current patch without rewriting the
historical `v2.2.0` release evidence.

## REL-01 — Release workflow (M)

- [x] `.github/workflows/release.yml`: `on.push.tags: ['v*']` plus `workflow_dispatch` with a `dry_run` input; `permissions: contents: write, packages: write, id-token: write`; concurrency group per tag
- [x] Job `verify`: checkout, `make release-version`, and a new `make release-verify` running `scripts/check-release-tag.sh`, which asserts the tag equals the version in `Cargo.toml` `[workspace.package]`, `console/package.json`, `deploy/helm/Chart.yaml` (`version` and `appVersion`), and `deploy/Dockerfile` `ARG OLP_VERSION`
- [x] Extend `scripts/check-ci-make-lockstep.sh` to every workflow under `.github/workflows/`; every command step in `release.yml` dispatches through `make` (`release-verify`, `release-image`, `release-manifest`, `release-chart`, `release-notes`)
- [x] Every new action pinned by SHA and registered in `scripts/check-supply-chain-pins.sh`; `make supply-chain` and `actionlint` clean
- [x] `scripts/test-repository-validation.sh` gains a case for the tag check

## REL-02 — Image publication (M)

- [x] Build natively per architecture: `ubuntu-24.04` (amd64) and `ubuntu-24.04-arm` (arm64), each `docker/build-push-action` with `push: true`, `outputs: type=image,push-by-digest=true`, reusing the `type=gha` cache scopes from `ci.yml`
- [x] `make release-manifest`: `docker buildx imagetools create` combines both digests under a run-scoped candidate, then promotes a scanned and signed digest to `ghcr.io/tyk-swe/olp:2.2.0`, `:2.2`, and `:latest`; docs state that production pins the index digest and `latest` is a convenience alias
- [x] `make smoke-image-modes IMAGE=ghcr.io/tyk-swe/olp@<index-digest>` on both runners (`OLP_IMAGE_PLATFORM` set) against the pushed image
- [x] SBOM per architecture with the already-pinned `anchore/sbom-action`; both `openllmproxy-<arch>.spdx.json` files become release assets
- [x] cosign keyless: sign the index digest and attest each SBOM (`sigstore/cosign-installer`, pinned); `docs/deployment.md` gains "Verifying a release" with the `cosign verify` invocation and the expected certificate identity `https://github.com/tyk-swe/olp/.github/workflows/release.yml@refs/tags/v2.2.0`
- [x] Trivy scans both linux/amd64 and linux/arm64 from the candidate index with `exit-code: 1` for HIGH/CRITICAL before stable tags are promoted
- [x] GHCR package is repository-linked and anonymously pullable by digest on
  native amd64 and arm64 runners in
  [release run 33310224902](https://github.com/tyk-swe/olp/actions/runs/33310224902)

## REL-03 — Helm chart publication (S)

- [x] `make release-chart`: `helm lint --strict deploy/helm`, `helm package deploy/helm --version <tag> --app-version <tag>`, `helm registry login ghcr.io` with `GITHUB_TOKEN`, `helm push openllmproxy-<tag>.tgz oci://ghcr.io/tyk-swe/charts`
- [x] Sign the chart with cosign as well
- [x] Render the pushed chart with `image.digest` set to the release digest and diff against `helm template deploy/helm` — byte-identical
- [x] `deploy/helm/Chart.yaml`: add the missing `artifacthub.io/*` annotations (changes, images); `make helm-verify` green
- [x] The chart package is public and repository-linked; anonymous pull resolves
  digest `sha256:2f40b7dc…cbf2`, its tag-identity signature verifies, and its
  pulled archive renders byte-identically to the repository chart

## REL-04 — Cut the release (S)

- [x] Release commit: bump the four canonical version locations to `2.2.0`, regenerate `Cargo.lock` and `fuzz/Cargo.lock`, rename `## [Unreleased]` to `## [2.2.0] - 2026-08-30`, and add a fresh empty `[Unreleased]`; confirm `release-metadata.env` still names `0043` (the last migration of the previous release) and `make release-version` passes
- [x] Add an "Upgrading from 2.1.1" paragraph to the 2.2.0 entry: migrations 0044–0048 run in the migration Job / `migrate` service; the N-1 rehearsal covers 0043 → 0048
- [x] Tag after the complete candidate
  [run 33309244354](https://github.com/tyk-swe/olp/actions/runs/33309244354)
  is green: annotated `v2.2.0` points at release commit `780d1ab`
- [x] `make release-notes`: a script extracts the `[2.2.0]` CHANGELOG section into the GitHub Release body — never hand-copied; assets: both SBOMs, chart `.tgz`, `values.schema.json`, `checksums.txt`
- [-] Add historical `v2.0.0` through `v2.1.1` tags — dropped: they are not
  needed for artifact integrity, and retroactively changing release history has
  no user-facing benefit

## REL-05 — Quick start pulls (S)

- [x] `deploy/compose.yaml`: the default image is the current
  `ghcr.io/tyk-swe/olp:2.2.1`; the `build:` block lives in
  `deploy/compose.build.yaml` for contributors
- [x] README Quick start: drop `--build`; one line points contributors at the build overlay; keep the bootstrap-token flow unchanged
- [x] Re-run the README quick start from a pristine tag and empty Docker daemon:
  the complete setup form rendered in 18.389 seconds, below the 3-minute target
- [x] `docs/deployment.md` uses chart `2.2.1` and the published image/chart digests
- [x] `scripts/prepare-compose-secrets.sh` and `scripts/retire-compose-bootstrap-secret.sh` unchanged; `make e2e` still green (23 contract tests)
- [x] [Issues #137](https://github.com/tyk-swe/olp/issues/137) and
  [#138](https://github.com/tyk-swe/olp/issues/138) record the failed first
  proof, root causes, regression, and published fix rather than treating a
  visible form as sufficient evidence

## Exit criteria

- [x] `docker pull ghcr.io/tyk-swe/olp:2.2.1` works anonymously on
  [amd64](https://github.com/tyk-swe/olp/actions/runs/33315228525/job/99267571117)
  and
  [arm64](https://github.com/tyk-swe/olp/actions/runs/33315228525/job/99267571050);
  final
  [promotion](https://github.com/tyk-swe/olp/actions/runs/33315228525/job/99267672200)
  made the version tag, `2.2`, and `latest` resolve to index digest
  `sha256:4b511434…eee1a8`
- [x] `helm upgrade` from chart `2.2.0` to `2.2.1` on kind v0.33.0 /
  Kubernetes 1.35 preserved `48|48|0` migration state and installation identity,
  and left control/worker Available and Ready with zero restarts, backlog, or
  [supervisor errors](https://github.com/tyk-swe/olp/issues/138#issuecomment-5469178070)
- [x] [GitHub Release `v2.2.1`](https://github.com/tyk-swe/olp/releases/tag/v2.2.1)
  has the generated body, two SBOMs, chart, schema, and checksums; image and
  chart signatures verify against the tag workflow identity
- [x] README quick start completes without a Rust toolchain in under 3 minutes:
  a pristine `v2.2.1` checkout and empty disposable daemon pulled anonymously,
  rendered the full form in 18.389 seconds, created the owner, retired the
  bootstrap token, and recorded zero application restarts or metadata-consumer
  supervisor errors. Docker readiness remained unhealthy as designed because a
  fresh installation has no published runtime generation; both control and
  worker health are proven separately by the kind deployment. The
  [full proof](https://github.com/tyk-swe/olp/issues/137#issuecomment-5469178073)
  records the empty daemon, credentials, lifecycle, and cleanup checks

## Carry-over

_None._

## Local implementation evidence — 2026-08-30

- `make check`: Clippy clean; 915 Rust tests and 333 console tests pass; console
  type-check, lint, and production build pass.
- `make check-static`, `make helm-verify`, `make fuzz-check`, locked Cargo
  metadata for both workspaces, actionlint 1.7.12, and `git diff --check` pass.
- `make e2e`: 23 contract tests pass against PostgreSQL and Valkey.
- Defect-first and adversarial reviews found and fixed the Compose, Valkey,
  Firefox, chart-verification, and partial-promotion gaps before the patch tag;
  the two publication-dependent issues closed after the public proof passed.
- [Main run 33314516539](https://github.com/tyk-swe/olp/actions/runs/33314516539)
  passed `Required` and `Full` at the annotated patch tag commit.
- [Release run 33315228525](https://github.com/tyk-swe/olp/actions/runs/33315228525)
  published image digest `sha256:4b511434…eee1a8` and chart digest
  `sha256:367ebb53…2994c`; both signatures and native manifests, release
  assets/checksums, and `2.2.1`/`2.2`/`latest` alias equality were independently
  verified. Separate post-release `cosign verify-attestation` checks matched
  each architecture's signed SPDX predicate to its Release asset.
- A [kind v0.33.0 / Kubernetes 1.35 upgrade](https://github.com/tyk-swe/olp/issues/138#issuecomment-5469178070)
  superseded Helm revision 1
  (`2.2.0`) with revision 2 (`2.2.1`), preserved the installation identity and
  `48|48|0` successful-count/max-version/failed-count tuple, left
  control/worker Ready with zero restarts and zero backlog, and reduced the
  baseline worker's 327 supervisor errors to zero for roughly one minute.
