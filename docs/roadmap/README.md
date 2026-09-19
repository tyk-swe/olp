# Rewrite completion and qualification — 2026-09-18

The accepted Go rewrite closed all **64 tickets across M1–M7**. This is a
historical completion record, not an active backlog or qualification of the
current checkout. Current behavior belongs in the [architecture](../architecture.md),
[access](../access.md), [gateway](../gateway.md), [compatibility](../compatibility.md),
and [operations](../operations.md) guides.

## Qualified candidate

- Source: `ccd138f288a99f66a2bb1145eb75d5ce57398416`.
- Qualification completed **2026-09-18**; [release run 35334054213](https://github.com/tyk-swe/olp/actions/runs/35334054213).
- Image: `ghcr.io/tyk-swe/olp@sha256:0c7495977bf5a837c7d3d0fa9d21c557a7ddbabb5eb08213ec51d45a458de09d`.
- Native Linux amd64/arm64 builds, canonical CI, dependency/image scans,
  packaged Chromium journeys and replacement restore passed for that candidate.
  Local qualification included 480 console tests and 26 integration browser
  scenarios; each packaged architecture ran six fresh-installation scenarios and
  one restore scenario (history grew from four requests to six).
- **Stable promotion was intentionally skipped. Paid live-provider tests were
  not requested.** Deterministic provider fixtures do not certify paid accounts.
- The candidate contract had 100 operations and SHA-256
  `f4d2f4743efd64631201cd7ff9f68e3297f3563511cd04aa43c0197d46462ca9`.
  Later contract, mapping, test, or documentation changes are not qualified by
  those results. Current inventories and fresh CI execution are separate.

Acceptance required **fresh Go PostgreSQL storage and isolated Valkey state**.
Rust-storage migration, mixed Rust/Go writable deployments, and compatibility
with old management payloads were excluded. The retained console media scope
was metadata-only list/detail, filters, and manual refresh—not content/delete
controls, automatic polling, cancellation workflows, or a media playground.
Those remain separate product work, not missing migration parity.

## Independent reference and current verification

The frozen source is `6c21dfb917c9019161348ea24b532a77b6612e6e`.
[`tests/fixtures/reference-inventory.json`](../../tests/fixtures/reference-inventory.json)
preserves its 100 management tuples, 77 inference tuples, 133 suite sources,
and hashes for 18 unchanged neutral fixtures.
[`tests/release-behaviors.json`](../../tests/release-behaviors.json) contains the
reviewed behavior-to-successor-test mappings and reasons for retired Rust-only
harnesses. The September 18 follow-up replaced filename/milestone heuristics
with explicit mappings and named-test existence checks; existence is not proof
of historical execution. Never regenerate the reference from its Go successor.

`node scripts/release-inventory.mjs` checks those inputs, fixture hashes,
management tuple retention and the independent certification matrix, then
regenerates current source/dependency inventories under `deploy/`.
See [test commands](../../tests/README.md). GLIDE still includes a prebuilt Rust
core through CGO: [native inventory and license hashes](../../deploy/native/inventory.json)
and the no-Rust-build guard remain required; the product is not purely Go.

## Historical measurements and retrieval

Five successful samples supported each full-application build result below
(seconds, median with range), on the recorded eight-CPU Haswell/~24.6 GB runner.
Clean builds emptied a private Go cache; warm edit runs changed implementation
and relinked. Image builds disabled layer reuse but retained the BuildKit Go
cache and included downloads. Integration was not a timing benchmark.

| Case | Median (range) |
| --- | --- |
| Clean backend | 46.214 (43.621–49.054) |
| Backend edit | 5.791 (5.553–5.933) |
| Targeted SSE test | 0.682 (0.660–0.787) |
| API generation | 2.304 (2.183–2.361) |
| Console | 10.583 (10.296–11.092) |
| Checks | 56.801 (56.402–59.422) |
| Image | 70.231 (68.592–70.962) |

Clean/edit builds were 20.26%/27.42% of the frozen Rust medians
(228.062/21.121 seconds), meeting both ≤50% gates. Eight held 64 MiB uploads
reserved 512 MiB under a 1 GiB spool cap and cleaned up to zero; the held
checkpoint recorded 67,008 KiB RSS, 3,044,296 heap bytes, and ten descriptors.
These are fixture observations, not peak-memory, throughput, latency,
availability, invoice-accuracy, or recovery-point guarantees.

Bulk evidence is retained in immutable Git commits, not solely Actions artifacts:

- [`7ecaebb6dacbf0406174edd87c26e178077424a8`](https://github.com/tyk-swe/olp/tree/7ecaebb6dacbf0406174edd87c26e178077424a8/docs/roadmap/evidence)
  contains the original `release-qualification.md`, `release-candidate.json`,
  native amd64/arm64 build/smoke/link logs, packaged-browser/restore evidence,
  scans, SBOM, build scorecard, raw timings, screenshots and fresh-checkout log.
  This evidence commit **follows** the qualified application commit. Candidate
  scan/SBOM hashes were verified against its Git blobs before active-tree removal.
- [`20ad3e6dd3056a3066938584583ea1bddb9a4ed2`](https://github.com/tyk-swe/olp/tree/20ad3e6dd3056a3066938584583ea1bddb9a4ed2/docs/roadmap)
  contains the final accepted backlogs and all 149 pre-cleanup roadmap files,
  including the later mapping reconciliation. Retrieval with `git archive` was
  verified byte-for-byte against the pre-cleanup tree. For example:
  `git show 20ad3e6dd3056a3066938584583ea1bddb9a4ed2:docs/roadmap/evidence/release-qualification.md`.
