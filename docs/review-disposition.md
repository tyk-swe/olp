# 3.0 readiness review disposition

This record applies the September 2026 review to the repository, rather than
treating 120 suggestions as confirmed vulnerabilities. “Implemented” describes
code or documentation present in this change; qualification still depends on
its recorded test results and the actual deployment. Optional product scope
and unmeasured redesigns are deliberately excluded. No live-provider, storage
power-loss, production traffic or external repository-protection evidence is
inferred from a green local check.

| IDs | Disposition and evidence |
| --- | --- |
| R001, R026 | Implemented `no-store` in the affected helpers and across API response families, including rejections and streams. Unit and packaged-browser response assertions cover the contract. |
| R002 | Security policy now covers published 3.0 patches and accepts development/older-version reports. A bespoke documentation/version synchronization test is unnecessary; release versions have a focused publication check. |
| R003, R004, R005, R093 | CI now runs deterministic integration and scheduled HA/recovery coverage. Tagged source qualification precedes candidate creation; both native architectures run packaged browser journeys before digest-preserving promotion. External branch protections must require the jobs. |
| R006 | `cargo deny` and a high/critical pnpm audit gate run in PR, release and daily qualification. Missing duplicate exceptions were reviewed; the unmaintained build-time `paste` dependency has a narrow dated explanation/removal condition. No known vulnerability is being silently suppressed. |
| R007, R074, R075, R076 | Paginate provider metrics through 10,000 providers within the existing collection deadline; emit completeness/count status. Tests cover 101 and 250 providers. Zero samples omit success/latency estimates. Quantitative labels use stable IDs. Existing stale-cache preservation remains. |
| R008 | Runtime query/lock/idle deadlines with deployment overrides, separate migration deadlines, and SQLSTATE/lock tests. |
| R009, R061, R066 | Conservative production concurrency and explicit memory arithmetic added. Complete throughput, maximum-payload and slow-reader capacity qualification remains workload/hardware dependent; no unsupported OOM guarantee or weighted admission redesign is claimed. |
| R010 | Durability comments and operations now state the AOF loss window and accounting uncertainty. Actual power-loss/failover RPO qualification remains external; lost request facts cannot be reconstructed by adding a metric. |
| R011 | Production Compose setup generates/preserves a password and derives an encoded URL; the production overlay mounts the raw password as a secret and refuses the example value. Special-character round trips are checked. |
| R012 | Separate migration Secret supported in Helm, runtime grant procedure and permission-boundary tests added. Operators must provision non-owner logins and reapply grants after migrations; evaluation Compose remains a single-owner installation. |
| R013–R015 | Excluded optional private-issuer, MFA-context and idle-timeout product policies. They need deployment requirements and must not be inferred as existing guarantees. Local/OIDC absolute session and recent-authentication enforcement remain. |
| R016 | Same-origin credential-free authentication-change broadcasts cancel/clear other tabs and revalidate their current cookie. Browser logout test added. |
| R017 | Added rollover overlap, unknown/duplicate key ID and algorithm-mismatch cases to existing token tests. No discovery/JWKS cache was introduced; actual issuer-outage callbacks remain covered only to the extent of existing service tests. |
| R018, R020 | Existing redaction, encryption, key-reference, media-recovery and service fixtures retained and now gated. Restore now invokes a retained credential. A single combined live-stream/rotation/off-site canary drill is still qualification work, not a justification to replace the existing safeguards. |
| R019 | Confirmed release builds already compile out insecure OIDC support. Added explicit rejection of a test setting in release startup; existing provider-exemption warnings remain. |
| R021 | Retain reviewed fail-closed allocation data and tests; registry updates need an actual changed allocation. No speculative network exemptions added. |
| R022, R038, R042, R053, R063, R065, R067, R083 | Measurement-first proposals. No unmeasured cache, index, allocator, snapshot, estimator or table virtualization redesign. Representative deployment/cardinality benchmarks remain necessary before optimization. |
| R023 | Broad provider exemptions remain an explicit trusted-administrator policy, documented as such. Per-origin policy is a distinct configuration feature; not added without the deployment requirement. |
| R024 | Bounded safe incoming correlation IDs; invalid IDs are replaced. Routine trace paths use route templates and an unmatched sentinel. |
| R025 | Existing native handler/middleware contracts retained. Generic unsupported-route Problems are not an established defect requiring a new native fallback contract. |
| R027, R028 | Missing assets return non-HTML 404s; console errors offer reload. Build-produced checksums cover the packaged bundle; release startup validates it. Strict CSP remains. |
| R094 | Browser installations remain intentionally serial for the setup/publication lifecycle; recovery now has its own disposable database. Per-test parallel fixtures are deferred until runtime cost justifies breaking up that lifecycle. |
| R029, R095 | Packaged-origin Chromium coverage added. Real TLS proxy and Firefox/WebKit qualification remain deployment/browser-matrix work; localhost Secure-cookie tests do not certify an ingress. |
| R030 | Documented provider-owned resource limitations; existing media pinning retained. No speculative general conversation-handle router. |
| R031, R036 | Shared create/edit budget form explains concurrent overshoot, unpriced attempts and UTC reset boundaries. |
| R032, R033 | Excluded new reservation and priced-only product modes. Current exact accrued-cost semantics and explicit coverage are preserved; adding these modes without an agreed unknown-usage policy would overstate a guarantee. |
| R034, R068 | Existing missing/malformed/wrong-window spend protections retained; database deadlines and separate process/pool guidance added. A same-window hash is not proof of live attribution freshness. A strict attribution-lag admission bound remains unimplemented and needs a defined policy plus load qualification. |
| R035 | Invoice reconciliation is a separate product/data-integration workflow. Existing exact attempt/pricing history retained; no unsupported upstream invoice equivalence promised. |
| R037 | Tiny positive summaries use a less-than label; very large values avoid Number rounding. Request drilldown exposes the exact decimal, retaining exact budget formatting and existing exact attempt details. |
| R039 | Existing boundary/late-fact/idempotency/takeover tests now run in required integration. Further composed state-machine testing remains useful; no test expectations are weakened. |
| R040 | Version 1 queue envelope supports original unversioned records; unsupported versions remain pending without acknowledgement. Rolling-reader compatibility and stop/drain requirements are explicit. |
| R041, R117, R118 | Preserve the single package, feature owners and existing Rust visibility. No speculative module/crate split or forbidden custom architecture/size gate. Small new test helpers keep changed files within repository guidance. |
| R043, R079 | New authority admissions expire after 60 seconds even on isolated replicas; admitted work stays pinned. Independent authority age and desired generation are exported alongside active generation; stale authority also fails readiness. |
| R044 | Existing retained snapshot/credential ownership is preserved. A new live-generation registry solely for observability is deferred until retention problems are observed. |
| R045, R046, R048, R049 | Existing simulator, ETags/publication, circuits and routing retained. Expanded simulation inputs, impact diffs, circuit tuning and shared-account routing are product/performance extensions, not absent baseline mechanisms. |
| R047, R096, R112 | Existing detailed compatibility tables and endpoint/certification registry remain authoritative; explicit pinned mock SDK versions and qualification/live-certification distinctions documented. No duplicate manually synchronized compatibility matrix. |
| R050, R090, R113, R114 | Excluded optional routing experiments, bulk administration, GitOps and workspace tenancy. No demand or new isolation contract was supplied. |
| R051, R052 | Existing default partition and bounded retention batches are correct for current behavior. Automated partition movement/pressure adaptation needs dataset and lock-budget evidence; no speculative DDL scheduler. |
| R054 | Documented that the backup environment flag and zero backlog do not prove stopped admission. An actual external ingress fence is required. A distributed application admission fence remains unimplemented; it must not be claimed from an operator flag. |
| R055, R056 | PITR/WAL, off-site authenticated encryption, separate key custody, explicit RPO/RTO and consistency limits documented. Actual managed-service/off-site drills remain external. |
| R057, R100 | Integration now restores, authenticates, serves a retained-provider request, exercises playground and checks new plus historical accounting. Scheduled runs repeat the drill. It does not certify every deployment's disaster behavior or all live historical media/rotation combinations. |
| R058 | Replacement identity/isolation and a fresh-installation procedure for independent copies documented. Unsupported ad hoc UUID mutation is not introduced. |
| R059, R060 | Backup records exact migration hashes/build identity, bounds SQL/dump waits and publishes a complete directory atomically. Restore checks included migration lineage before writing. |
| R062, R070, R071 | Existing admission, tracing, queue and drain signals retained. Phase histograms, new local request counters and autoscaling controls are further observability/scale extensions; no measurements justify changing autoscaling defaults. |
| R064, R069 | Fleet connection budget including rollout/migration headroom and tmpfs versus disk-spool resource accounting documented in the production profile. |
| R072, R073, R077 | Documented global versus local aggregation, percentile limitations, request versus attempt/terminal success, and actionable incident diagnosis. Existing rules already deduplicate shared backlog values. Actual notification routing must be tested by the operator. |
| R078 | Existing usage UI already displays pending, unpriced, range and epoch uncertainty. Cost summary now explicitly points to coverage and distinguishes the estimate from an invoice. |
| R080 | Doctor is already sanitized operational tooling. A new support archive/export surface is optional and excluded without support-workflow requirements. |
| R081, R082 | Native Escape dismissal restored; browser secret lifecycle exercises it. Query retries exclude permanent/auth/validation/rate-limit failures and retry only eligible transient failures once. |
| R084–R086 | Existing validation, unsaved-change guards and native SDK fixtures retained. Universal form-summary refactors, SDK generation UI and draft persistence are optional product scope; credentials remain absent from browser persistence. |
| R087, R088 | RequestTimeline and staged provider/capability/route activation already exist and are exercised by the browser journey. No duplicate timeline or onboarding system. |
| R089, R116 | Existing reauthentication, revision conflicts and reference protections remain. Broad impact/removal-plan UX is a distinct product extension. |
| R091, R092, R099 | Independent cache, authority, lock and manifest behavior assertions added; one timing-sensitive failover test now uses a paused clock while preserving its original deadlines and expectations; existing parser/property/hostile-provider limits retained in required suites. A separate mutation/fuzz infrastructure campaign is deferred rather than adding nightly/build machinery without a bounded corpus plan. |
| R097 | Existing generation retained; CI publishes its contract artifact and oasdiff compares against the latest published stable 3.x release. Release publication attaches the baseline for subsequent releases. The first 3.x release explicitly establishes the baseline. |
| R098 | Existing fresh-schema/idempotence/2.x rejection tests remain required. Future populated 3.x migration and mixed-version qualification is explicit; no nonexistent upgrade lineage is invented. |
| R101, R102 | Third-party Actions resolved to full commit SHAs; build/runtime dependency images pinned to multiarch digests. Dependabot update review retained; no custom dependency-pin gate. |
| R103, R104 | Candidate Buildx provenance/SBOM and GitHub image/chart attestations configured; digest provenance is verified before promotion and deployment guidance added. A weekly released-image vulnerability rescan retains a digest-specific report and fails on high/critical findings. Actual registry attestations and scan results still require published artifacts and workflow execution. |
| R105, R106, R107 | Prepublication version check, repository Rust toolchain, and separate main-branch selected-provider credential jobs with timeouts. |
| R108, R109, R110 | Production values compose existing controls; default/production/mode/schema-negative Helm checks added. Candidate browser qualification and existing six-mode smoke run on each native architecture. Production renders also pass Kubernetes schema validation for the declared test versions; monitoring CRDs require the operator’s installed CRD schemas. Each candidate also starts all/gateway/control/worker with disposable services, migrates, runs doctor and checks health/surface separation and shutdown. Node-failure qualification remains deployment work. |
| R111, R115, R119, R120 | Operational contracts, responsible feature owners, assumptions and release-evidence requirements documented. Pricing/provider owners must review current external data; no invented verification dates or feature expansion. |


## Local validation (7 September 2026)

- `make check`: 955 Rust tests and 392 console tests passed, including formatting, Clippy, ESLint and Svelte/type checks.
- `make integration`: 139 service/HA Rust tests, official JavaScript SDK smoke tests, packaged and Vite browser journeys, and both logical restore drills passed. Paid live-provider tests remain excluded.
- Helm default/production/mode/negative-input checks passed. Kubernetes 1.33–1.35 schema validation passed for 17 core resources per production render; the three monitoring CRDs need their separately installed schemas.
- Actionlint, shell/JavaScript syntax checks, version-mismatch rejection and an oasdiff removed-operation negative check passed.
- Production credential encoding was checked with reserved URL characters, quotes, backslashes and spaces. Database role and timeout integration cases passed.
- `cargo deny check` passed with the explicitly reviewed `paste` maintenance exception. `pnpm audit --audit-level high` passed but reports one low-severity [cookie advisory](https://github.com/advisories/GHSA-pxg6-pf52-xh8x) through the latest SvelteKit's development/build server dependency. The production image serves the static console through Rust; no transitive version override was forced.
- The local amd64 runtime image built and passed all/gateway/control/worker startup, migration, doctor and shutdown checks. Five fresh container-origin browser journeys and the restored-container journey passed with automatic graceful teardown; the insecure local OIDC section remains confined to the debug/source suite. Its runtime OS scan reported no HIGH/CRITICAL vulnerabilities; the scanner detected no language-package manifests in the final image, which is why CI additionally requires the Rust/console build-stage SPDX inventories.

Actual registry attestations, release promotion, arm64 runners, published-baseline
API comparison and full build-stage SBOM scans must execute in CI. These local
results do not replace deployment-specific load, TLS/proxy, power-loss or
managed-service recovery qualification.
