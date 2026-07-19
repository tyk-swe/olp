# Enterprise control-plane threat model

- Status: Accepted target and risk register (approval pending)
- Decision issue: XOD-87
- Applies to: enterprise-beta architecture described by the M0 ADRs
- Implementation status: incomplete; M1-M7 supply the controls
- Qualification status: not qualified until the M8 evidence gates pass
- Review trigger: material trust-boundary, data-classification, connector,
  identity, policy, recovery, or topology change

## Purpose and interpretation

This model freezes the security claims that later milestones must implement and
test. It is not a claim that OLP 2.0 already provides tenant isolation. The
current schema, authentication map, and runtime are installation-global; the
enterprise organization/project/environment hierarchy and split runtime are
accepted targets in ADRs 0001 and 0002.

Repository review and completion of XOD-87 record approval of this model and
the residual-risk dispositions. M8 still requires external security evidence
and a separate go/no-go decision. A planned control or test cannot be cited as
evidence that a threat is mitigated.

## Security objectives

1. One organization, project, or environment cannot enumerate, read, mutate,
   route through, authenticate as, charge, audit, restore, or receive content
   from another scope without an explicit authorized relationship.
2. Public authentication resolves server-owned scope before routing. Client
   headers, connector fields, policy output, and resource IDs are never scope
   authority.
3. A request pins one credential security revision and one environment runtime
   generation. Mixed authority observations deny safely.
4. Provider, inference, OIDC, session, service-account, and external-secret
   material is revisioned, least-privilege, encrypted where OLP stores it, and
   absent from durable content-free records and diagnostics.
5. Connector and policy failures cannot escape their declared resource,
   network, content, phase, or decision boundaries.
6. Hard security and economic controls fail closed unless an accepted,
   versioned policy names an advisory mode.
7. Requests, attempts, usage, audit, and decision records remain metadata-only.
   Missing usage or pricing is incomplete or unpriced, never silently zero.
8. Publication, revocation, offboarding, backup, restore, and rollback behavior
   meets the measurable bounds in `contracts/capacity-envelope.json`.

## Assets and data classification

| Class | Assets | Required handling |
| --- | --- | --- |
| Secret | inference key plaintext/digest inputs, provider credentials, OIDC client secrets, session/CSRF tokens, service-account signing material, external-secret references when sensitive, master/hash keys | never log or export; encrypt at rest when stored by OLP; revision and revoke; zeroize plaintext; least-privilege delivery |
| Customer content | prompts, outputs, reasoning, tool schemas/arguments/results, uploads, response media | request-memory or bounded media-object lifetime only; never durable metadata/audit/policy trace/telemetry; send only to selected connector/provider or explicitly bound content policy |
| Security metadata | scope and principal IDs, memberships, groups, role bindings, policy and grant revisions, connector identity, runtime generations, source network class | authorized and scoped; bounded; auditable; not a metrics-cardinality default |
| Economic metadata | request/attempt IDs, token/media units, prices, reservations, budgets, completeness | scoped, tamper-evident through integrity controls, atomically reconciled, explicit uncertainty |
| Operational metadata | timing, stable error classes, health, SLO and convergence measurements | bounded and redacted; preserve scope where required without content or arbitrary labels |
| Public contract | API schemas, connector manifests and SDKs, policy language version, capability tuples | versioned, integrity-checked, compatibility-gated |

## Actors

- An unauthenticated Internet client, including a credential stuffing or DoS
  attacker.
- An authenticated human, application, or service account acting maliciously
  or with a stolen credential.
- An organization administrator who is trusted for that organization but not
  for another organization or the installation operator boundary.
- An installation operator with infrastructure and connector-registry access.
- A compromised or malicious identity provider, SCIM client, connector,
  upstream provider, secret manager, proxy, CA, telemetry sink, or support
  recipient.
- A buggy, stale, or compromised OLP replica during publication, upgrade,
  restore, or dependency failure.

## Trust boundaries

```text
untrusted client / SDK
        |
        | public TLS, untrusted headers and content
        v
gateway admission -> credential authority -> environment runtime cache
        |                    |                     |
        |                    |                     +-- PostgreSQL release authority
        |                    +-- PostgreSQL security authority / hint channel
        |
        +-- policy evaluator (trusted bounded IR, no customer code)
        |
        +-- mTLS/UDS Connector v1 boundary -> isolated connector -> provider/private egress
        |
        +-- bounded usage channel -> Valkey -> worker -> PostgreSQL

browser / automation -> control API -> scoped authorization -> PostgreSQL/outbox
                           |
                           +-- OIDC/SAML/SCIM and external-secret boundaries

operators -> deployment, keys, backup/restore, connector registry, telemetry/export sinks
```

PostgreSQL is durable authority. Valkey carries hints, distributed reservations,
and usage work but is not authority for scope ownership or configuration.
Connector, identity-provider, secret-manager, upstream-provider, and telemetry
payloads are untrusted even when their transport is authenticated.

## Security assumptions

- The supported deployment protects PostgreSQL, Valkey, object storage,
  workload identities, TLS private keys, master keys, and key-hash keys using
  the documented HA and secret-management controls.
- Installation operators can access infrastructure and therefore remain a
  privileged trust role. Organization administrators are not installation
  operators.
- The host kernel, container runtime, Kubernetes control plane, and configured
  CNI enforce workload and network isolation. A kernel or cluster-admin
  compromise is outside OLP's application isolation claim.
- Provider APIs necessarily receive content routed to them. OLP cannot stop an
  authorized upstream or connector from retaining data outside OLP; deployment
  and provider agreements must cover that behavior.
- Clocks are authenticated and monitored closely enough for expiry, federation,
  leases, budgets, and evidence timestamps within the accepted skew.

## Threat register

The evidence column names the required proof, not evidence that already exists.

| ID | Threat and attack path | Required controls | Required evidence | Residual disposition |
| --- | --- | --- | --- | --- |
| TM-01 | Cross-scope IDOR or list/search timing reveals or mutates another organization's provider, route, key, request, usage, media, or audit record. | Explicit scope IDs on every durable record; scoped queries and authorization; scoped uniqueness; composite foreign keys; indistinguishable not-found response; RLS only as defense in depth; cursor scope binding. | M1 storage/API isolation tests and M8 adversarial two-organization suite. | Mitigate. |
| TM-02 | A client supplies organization, project, environment, principal, forwarded-IP, or role headers and becomes a confused deputy. | Resolve scope only from verified credential/membership and trusted proxy configuration; strip/ignore public authority headers; bounded RequestContext provenance; deny ambiguous credentials. | M2 malicious-header and proxy-chain tests; fuzz header parsing. | Mitigate. |
| TM-03 | A global lookup collision, cache-key omission, stale shard, or mixed publication authenticates a credential in the wrong environment or combines a new policy with an old runtime. | Installation-unique public lookup ID; constant-time verification; lookup-keyed credential authority; environment ID in every cache key; authority/runtime fences; per-request immutable pins; deny on mismatch; bounded staleness. | M2 concurrency/model tests for activation, revocation, eviction, cold load, mixed generations, and two-scope leakage. | Mitigate. |
| TM-04 | Runtime rollback or restore reintroduces revoked keys, grants, provider credentials, or policy. | Revocation authority independent of environment release; monotonic revisions; rollback published as a new generation; restore reconciliation against current security authority before admission; no old-secret fallback. | M2 rollback/revocation tests and M6 restore/DR security reconciliation. | Mitigate. |
| TM-05 | An OIDC/SAML issuer collision, discovery substitution, signature/key confusion, replay, weak redirect, or account-linking race grants another identity. | Organization-owned IdPs; exact issuer/client/provider identity; HTTPS/SSRF policy; state, nonce, PKCE, browser binding, one-time flow consumption; algorithm/audience/time validation; transactional linking; no email-only implicit link. | M3 federation conformance, replay/race tests, and external review. | Mitigate. |
| TM-06 | Malicious or oversized federation group claims create unbounded work or overgrant a custom role. | Bounded claim bytes/items; normalized provider-scoped group IDs; explicit group mapping; least-privilege default; immutable role/binding revision; deny unknown/ambiguous mapping; audit the decision without raw assertions. | M3 group-federation fixtures, property tests, and offboarding matrix. | Mitigate. |
| TM-07 | SCIM deprovisioning races active sessions, cached memberships, keys, approvals, or service-account delegation. | Idempotent SCIM versioning; authoritative disabled state; revocation epoch; bounded cache propagation; session and short-lived token invalidation; explicit ownership policy for attributed inference keys; offboarding transaction/outbox. | M3 deprovision race and complete offboarding conformance with measured propagation. | Mitigate; in-flight request behavior is AR-02. |
| TM-08 | A stolen service-account or machine credential is replayed across organization/project/environment or retained indefinitely. | Project-owned service accounts; audience/scope-bound short-lived credentials; asymmetric or proof-bound signing where supported; unique token ID and bounded replay controls; rotation/revocation; no long-lived secret in logs/console; grant checks at use. | M3 machine credential replay, wrong-audience, expiry, rotation, and scope tests. | Mitigate. |
| TM-09 | A compromised connector reads OLP memory/database, steals other tenants' credentials/content, calls control APIs, or moves laterally. | Connector v1 out-of-process boundary; no Rust dylib/customer code; dedicated workload identity; mTLS/UDS authentication; per-connection secrets; non-root/read-only sandbox; no DB/Valkey/control access; default-deny egress and resource limits. | M4 malicious connector suite, deployment-policy inspection, penetration test, and credential canaries. | Mitigate; authorized content/credential exposure is AR-03. |
| TM-10 | Slow, unavailable, incompatible, or malicious connector output exhausts resources, violates sequence, forges usage, or causes unsafe retry. | Handshake/version/digest pin; message/event/media/deadline/concurrency bounds; cancellation; output codec and sequence validation; OLP-owned commitment/retry decision; quarantine health; incomplete/unpriced usage on anomaly. | M4 connector conformance and chaos tests including post-commit failure. | Mitigate. |
| TM-11 | Private endpoint, proxy, custom CA, DNS rebinding, redirect, or link-local/cloud metadata access bypasses egress policy. | Typed network policy; parse/resolve/validate/connect pinning; CIDR and DNS allowlists; redirect disabled by default; proxy and no-proxy policy; CA/mTLS identity pin; block loopback/link-local/metadata unless explicit operator policy; revalidate on change. | M4 SSRF corpus, DNS-rebinding tests, private topology reference deployments, and external review. | Mitigate. |
| TM-12 | Secret-reference substitution, path traversal, resolver confusion, stale cache, or cross-scope delivery supplies the wrong secret or leaks a reference. | Opaque bounded references; allowlisted schemes; provider/scope/revision binding; workload identity; authenticated resolver; no OLP URL/path dereference; no fallback; revisioned delivery; redaction and zeroization. | M4 fake-secret-manager tests, cross-scope negative tests, rotation/revocation propagation, and support-bundle scan. | Mitigate. |
| TM-13 | Policy is skipped on one operation/surface, simulation differs from enforcement, stale code runs, or customer code escapes the process. | Central fixed phase pipeline; immutable typed IR; no native/Wasm/script extension; version/digest pin; same evaluator for simulation and production; capability/operation coverage; bounded deterministic execution; fail closed for hard decisions. | M5 exhaustive phase/operation matrix, golden replay, simulation equivalence, and fault injection. | Mitigate. |
| TM-14 | A content policy copies prompt, output, tool, or media content into decisions, audit, telemetry, or durable state. | Phase visibility allowlist; ephemeral request-memory content view; typed outputs containing only action/reason IDs; sink-level redaction; metadata-only schemas; content canary tests. | M5 content canary tests across DB, logs, metrics, traces, exports, and support bundles. | Mitigate. |
| TM-15 | Concurrent quota or budget checks oversubscribe a hierarchy, double reserve/release, underflow, or fail open during dependency loss. | Atomic hierarchical reservation with idempotency key; fixed currency/decimal semantics; lease plus terminal reconciliation; monotonic version; exact-once state transition; hard dependency failures deny; explicit advisory modes only. | M5 race/property tests, kill/retry tests, and reconciliation invariant queries at target load. | Mitigate. |
| TM-16 | Conditional routing injects an uncertified/ungranted target, uses stale telemetry, becomes unbounded, or hides its decision. | Start from certified granted finite candidate set; policy may only filter/rescore; cap attempts/concurrency/deadlines; telemetry freshness and fallback to weighted rendezvous; bounded reason IDs; no post-commit retry or non-idempotent hedging. | M5 routing fixtures, stale/missing telemetry tests, explanations, and production/simulation parity. | Mitigate. |
| TM-17 | Connector/provider lies about token/media usage or pricing lookup silently reports zero, enabling budget bypass or incorrect billing. | Treat usage as untrusted; validate bounds and operation; server-observed facts where possible; incomplete/unpriced on missing/conflict; pricing provenance; budget discrepancy alarms; no silent zero. | M4 connector usage conformance plus M5 accounting/reconciliation and export tests. | Mitigate; upstream truth limits are AR-04. |
| TM-18 | Audit rows are omitted, rewritten, truncated, cross-scoped, or export delivery is silently lost. | Transactional audit with protected scope/actor/action/outcome fields; append-oriented permissions; outbox/claim-safe export; checkpoints, retry, gap evidence, retention policy, integrity chain or signed batches; content-free bounded fields. | M3/M6/M7 mutation coverage, tamper tests, sink outage/replay tests, and completeness reconciliation. | Mitigate; privileged storage tamper is AR-05. |
| TM-19 | Valkey loss, worker crash, duplicate delivery, or receipt cleanup loses or double-counts acknowledged usage. | Bounded local accounting; primary-and-two-replica durable stream acknowledgement before local release; failover only to an offset at or beyond the last acknowledged event; backpressure/fail closed before buffer overflow; claim-safe idempotent worker; receipts and replay horizon; explicit gaps; backup quiescence; never delete before commit/ack. | M6 pod-loss/Valkey/duplicate/restore chaos, including primary death immediately after durable quorum acknowledgement, and zero acknowledged-loss evidence. | Mitigate. |
| TM-20 | Media handle guessing, local spool reuse, object-store ACL error, or async job reconciliation crosses environments or exposes bytes. | Opaque high-entropy handle bound to organization/project/environment/request/job; object-level authorization; no paths; byte/type/count bounds; encrypted transport/storage; deterministic cleanup; pinned provider/runtime authority. | M2/M6 cross-scope media tests, cancellation cleanup, job takeover, and object-store policy review. | Mitigate. |
| TM-21 | Idempotency key or approval replay executes a mutation in another scope or against changed input. | Scope and actor in idempotency key; canonical request fingerprint; encrypted replay body where necessary; bounded TTL; approval resource/version/action binding; atomic consume; no cross-version replay. | M1/M3 concurrent replay, scope substitution, payload mismatch, and expiry tests. | Mitigate. |
| TM-22 | Restore, migration, or mixed-version rollout drops scope columns/constraints, permits old unsafe writes, or produces a partially scoped runtime. | Expand/backfill/verify/contract; composite integrity before readers trust fields; N/N-1 capability matrix; database guards; migration checksum; default-scope mapping; admission frozen when required; restore rehearsal. | M1/M6 upgrade and restore matrices with old/new writers under traffic. | Mitigate. |
| TM-23 | High-cardinality scope, connector, label, extension, or policy fields cause memory/telemetry/DB exhaustion. | Published capacity limits; bounded identifiers/collections/payloads; pagination; admission and per-scope quotas; metric-label allowlist; traces/logs sampled and size-capped; load shedding. | Capacity profiles CP-01 through CP-05, fuzzing, and telemetry cardinality assertions. | Mitigate. |
| TM-24 | Error, existence, latency, health, or usage differences reveal another tenant even when the response body is hidden. | Uniform not-found/unauthorized behavior; no foreign-name echo; scoped caches and queries; coarse stable errors; timing review and minimum work for sensitive lookups; aggregate only authorized telemetry. | M1/M2 enumeration tests including response, timing distribution, pagination, and cache state. | Mitigate; perfect traffic-analysis resistance is AR-06. |

## Critical abuse-case invariants

### Public authentication and isolation

Authentication must derive the public lookup ID without accepting scope from the
request, pin the current credential authority, verify the digest/status/expiry,
and only then obtain organization/project/environment IDs. A missing or foreign
environment runtime is unavailable, not a lookup in another environment. A
client-chosen request ID may be retained only as bounded correlation metadata;
it is never the canonical request or authorization ID.

Every attempt, usage event, media object, audit event, policy decision, job,
outbox item, and export carries or inherits verifiable scope. Database RLS may
reduce blast radius, but application predicates and composite foreign keys are
mandatory.

### Federation and offboarding

Federation identity is `(organization_id, identity_provider_id, issuer,
subject)`, not email alone. Email and group claims are attributes, not globally
trusted identifiers. JIT provisioning, explicit linking, SCIM update, session
creation, and role-binding changes serialize against the membership security
revision. Deactivation prevents new requests within the propagation target.
An already committed inference stream is handled under accepted risk AR-02.

### Connector compromise

A connector is expected to see canonical content and the provider credential
for attempts assigned to its provider connection. Isolation therefore aims to
prevent expansion beyond that legitimate exposure, not to pretend the connector
cannot exfiltrate it. Provider connections with different trust requirements
use different connector workloads, identities, secret scopes, and egress
policies. Connector-supplied IDs, errors, usage, and health never become OLP
authority without validation.

### Economic controls

Quota and budget enforcement uses reserve-before-execute and reconcile-once
semantics. A reservation is identified by the canonical request ID and all
hierarchical scopes. Timeout, cancellation, connector failure, process death,
duplicate event, and unknown usage have explicit terminal states. Unknown usage
cannot release a cost reservation as if the request cost zero.

### Audit and recovery

Every security-sensitive mutation records actor or system identity, exact scope,
action, resource type/ID, outcome, policy/revision references, and time in the
same transaction or an integrity-linked durable event. It never records secret
or customer content. Export lag and loss are visible gaps.

After restore, gateways do not admit traffic merely because a historical
environment release verifies. Current credential, membership, connector,
secret, grant, and policy authority must reconcile first. Recovery evidence
records actual RPO/RTO and security-reconciliation results.

## Accepted residual risks

These risks are accepted for enterprise beta only after the approval controls in
`approvals.md` complete. Each is re-opened by a material architecture change or
failed qualification gate.

| ID | Residual risk and beta boundary | Compensating controls | Accountable authority | Review gate |
| --- | --- | --- | --- | --- |
| AR-01 | One supported installation is single-region. Region-wide recovery has nonzero RPO/RTO and no active-active service. | External HA dependencies, PITR, off-region backup, rehearsed restore/DR, published RPO/RTO. | gateway/security and product | M6 DR evidence and M8 go/no-go |
| AR-02 | Revocation/deprovisioning does not terminate an already authenticated and committed request or stream; the request retains its immutable security/runtime pins until bounded completion. | Short bounded route/stream deadlines, no new attempts after terminal policy where enforceable, rapid new-request revocation, incident kill at edge. | gateway/security | M2 propagation and M3 offboarding evidence |
| AR-03 | A connector and upstream provider can observe and exfiltrate content and the provider credential legitimately assigned to them. OLP process isolation cannot remove this inherent trust. | Connector certification, workload/secret/egress isolation, per-connection credentials, provider contracts, customer allowlisting. | provider platform and product/release | M4 external review and design-partner acceptance |
| AR-04 | Some upstreams do not provide independently verifiable usage. Fraud or defects can leave cost incomplete rather than exact. | Server-observed dimensions, bounds, provenance, discrepancy alerts, incomplete/unpriced semantics, reconciliation. | policy/economics and product/release | M5 accounting evidence and partner acceptance |
| AR-05 | A sufficiently privileged database, backup, cluster, or key operator can tamper with or disclose durable security metadata and encrypted records. | Separation of duties, external audit export/integrity batches, encryption, access logs, backup controls, key management. | gateway/security and operations | M7 audit export review and external security assessment |
| AR-06 | The beta does not promise resistance to global traffic analysis or all micro-timing inference. Authorized users may infer coarse activity from their own latency and quotas. | Uniform API semantics, scoped queries/caches, coarse errors, bounded telemetry, timing tests for direct enumeration. | gateway/security | M8 penetration test |
| AR-07 | OLP relies on the supported deployment's kernel, orchestrator, CNI, CA, DNS, secret manager, PostgreSQL, Valkey, and object-store security; it does not build their HA or isolation internally. | Reference topology, version/pin policy, readiness, runbooks, conformance, design-partner responsibility matrix. | product/release and reliability | M6/M8 reference deployment acceptance |

## Evidence and maintenance requirements

- Every `TM-*` threat must map to at least one blocking gate in
  `contracts/enterprise-beta-scorecard.json`; missing evidence keeps the gate
  open.
- Capacity and propagation evidence must use the immutable profiles and result
  schema in `contracts/capacity-envelope.json`.
- Connector evidence must validate `contracts/connector-v1.json`, including the
  malicious and incompatible cases, rather than only native provider tests.
- Content canaries must search PostgreSQL, Valkey payloads, object metadata,
  logs, metrics, traces, audit/export events, policy explanations, and support
  bundles.
- External findings are linked by stable, non-sensitive IDs. Finding detail and
  exploit material stay in the approved private system.
- A threat is closed only by implemented, passing evidence. A target, ADR,
  skipped test, or local unreviewed run is not closure.
