# Operational and release qualification

This matrix is the blocking definition of release readiness. Commands run from
the repository root. `Required` applies to every pull request, `Full` retains
the second architecture and extended browsers, and `Weekly/Release` uses live
credentials or the extended duration. Owners identify the first responder for a
failed gate; they do not make the check advisory.

| Criterion | Blocking command | Tier | Threshold | Evidence | Failure owner |
|---|---|---|---|---|---|
| QL-01 | `tests/qualification/run.sh clean-install` (Compose phase) | Required | Empty volumes; setup succeeds once; recreate has no bootstrap secret | Compose service and teardown logs | Runtime |
| QL-02 | `tests/qualification/run.sh clean-install` (Kind phase) | Required | Pinned PostgreSQL/Valkey; hook and all workloads ready | Helm debug log and cluster diagnostics | Deployment |
| QL-03 | Compose `migrate` rerun and N-1 candidate migration rerun | Required | Identical successful migration sequence on the second run | Qualification logs | Storage |
| QL-04 | Compose and Helm setup requests | Required | Owner created; bootstrap removed; session and API key remain valid | Redacted HTTP assertions | Identity |
| QL-05 | `tests/qualification/run.sh backup-restore` | Required | Quiesced v2 backup, `doctor`, session/key/generation restored | Manifest, checksum, and restore log | Storage |
| QL-06 | `scripts/test-backup-manifest.sh` and N-1 bootstrap fixture | Required | Valid v1 accepted; tampering rejected | Manifest test log | Storage |
| QL-07 | `tests/qualification/run.sh n-minus-one` | Required | Strict metadata; signed previous binary after 2.0.0; migrate twice; functional candidate | Cosign bundle output and rehearsal log | Release |
| QL-08 | `tests/sdk-smoke/run.sh` | Required | OpenAI unary, stream, count, and discovery all succeed | SDK fixture log | Protocols |
| QL-09 | `tests/sdk-smoke/run.sh` | Required | Anthropic unary, stream, count, and discovery all succeed | SDK fixture log | Protocols |
| QL-10 | `tests/sdk-smoke/run.sh` | Required | Gemini unary, stream, count, and discovery all succeed | SDK fixture log | Protocols |
| QL-11 | `tests/ha/two-gateway.sh` | Required | Exact sequence on both gateways; healthy propagation/revocation ≤5 s; missed hint ≤5.5 s | HA process logs | Runtime |
| QL-12 | `tests/qualification/run.sh load` | Required | 100 rps for 2 min after 30 s warm-up; zero failures/drops; p95 <15 ms and p99 <30 ms | k6 JSON summaries and server log | Performance |
| QL-13 | `tests/qualification/run.sh soak` | Weekly/Release | 50 rps for 30 min; same latency SLO; RSS ≤max(64 MiB,20%); FD +16; threads +4; no spool files | k6 JSON, resource TSV, server log | Performance |
| QL-14 | `tests/qualification/canary.sh` | Weekly/Release | All three live connectors discover, generate one token, complete a stream, and count positively | Redacted canary log | Connectors |
| QL-15 | amd64 image job plus `scripts/smoke-image-modes.sh` | Required | Packaged mode and clean-install smoke succeed | Image log and amd64 SPDX JSON | Release |
| QL-16 | arm64 image job plus `scripts/smoke-image-modes.sh` | Full | Native arm64 packaged and clean-install smoke succeed | Image log and arm64 SPDX JSON | Release |
| QL-17 | `helm package deploy/helm` and Kind install | Required | Chart schema/template/package/install all succeed; tested archive is retained | `.tgz`, checksum, Helm log | Deployment |
| QL-18 | SPDX validation and Trivy image jobs | Required/Full | SPDX 2.x document present; fixed HIGH/CRITICAL count is zero; unfixed reported | SPDX JSON and Trivy table | Security |
| QL-19 | release publish-and-verify job | Release | Immutable image index/platforms and chart digest signed; SBOM/provenance attested; identity and issuer verify | Checksums, bundles, attestations, release assets | Release |
| QL-20 | `scripts/check-docs.py` | Required | Local links/anchors, current version, script help, and all 20 IDs validate | Documentation check log | Documentation |

## Running qualification locally

Install Docker with Compose, Kind, Helm, kubectl, PostgreSQL 18 clients, Valkey
CLI, k6, jq, Node.js 24/pnpm 11, and Rust 1.97. The common entry points are:

```console
tests/qualification/run.sh clean-install
tests/qualification/run.sh backup-restore
tests/qualification/run.sh n-minus-one
tests/qualification/run.sh load
tests/qualification/run.sh soak
```

Database-backed targets require the URLs described by their `--help` output.
Evidence defaults to `artifacts/qualification/`, which is intentionally not a
release input. CI uploads it even when a check fails.

## N-1 baseline rollover

[`release-metadata.env`](../release-metadata.env) has exactly four fields.
`bootstrap` is valid only at version 2.0.0 with the migration-0021 fixture.
Every later workspace version fails until the file names the previous semantic
version, signed `ghcr.io/tyk-swe/olp@sha256:…` image, and its highest migration.
After publishing, generate the follow-up file:

```console
scripts/release-metadata-next.sh 2.0.0 \
  ghcr.io/tyk-swe/olp@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
```

Review and commit `release-metadata.next.env` as `release-metadata.env` on the
next development change. Never advance it while qualifying the release that
produced the digest.

## Live canaries

Weekly and release workflows require these repository secrets:
`OLP_CANARY_OPENAI_API_KEY`, `OLP_CANARY_ANTHROPIC_API_KEY`, and
`OLP_CANARY_GEMINI_API_KEY`. Matching repository variables replace `API_KEY`
with `MODEL`. Missing values intentionally fail. The harness creates draft
providers through the management API, performs credentialed discovery and
bounded certification, activates routes, then exercises the production
gateway surfaces. Logs must never print credentials, provider bodies, prompts,
or generated text.

## Consumer verification

Release verification is digest-bound. Substitute the published version and
digest, retaining the exact workflow identity and public GitHub OIDC issuer:

```console
cosign verify \
  --certificate-identity 'https://github.com/tyk-swe/olp/.github/workflows/release.yml@refs/tags/v2.0.0' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  ghcr.io/tyk-swe/olp@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
cosign verify-attestation --type spdxjson \
  --certificate-identity 'https://github.com/tyk-swe/olp/.github/workflows/release.yml@refs/tags/v2.0.0' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  ghcr.io/tyk-swe/olp@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
```

Verify the downloaded `SHA256SUMS`, chart archive, SPDX documents, and Sigstore
bundles before installation. Helm OCI uses the chart version as the tag:
`helm pull oci://ghcr.io/tyk-swe/charts/openllmproxy --version 2.0.0`.

## Release procedure

1. Confirm the workspace, console, image, and chart versions agree and the N-1
   file still describes the last completed release with
   `scripts/check-release-version.sh`.
2. Push an annotated tag that exactly matches `vX.Y.Z`. Do not create or move a
   tag until the commit's pull-request `Required` aggregate is green.
3. The tagged CI run reruns `Required` and `Full`. The release workflow waits
   for those exact-SHA aggregates while independently rerunning all live
   canaries and the 30-minute soak.
4. Publication builds only the versioned multi-architecture image, downloads
   the exact chart archive installed by CI, publishes both OCI artifacts,
   signs the image index, its two platform manifests, and the chart digest,
   attaches SBOM/provenance attestations, and verifies identity and issuer.
5. Download the GitHub release assets and perform the consumer verification
   above before announcing or deploying the release.
6. Use the emitted `release-metadata.next.env` in the first follow-up change.
   Never add a mutable `latest` image tag.
