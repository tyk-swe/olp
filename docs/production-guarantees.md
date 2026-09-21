# Production contracts

These are the limits of the 3.0 single-installation model. A successful mock
suite qualifies the tested behavior; it does not establish a production SLO,
provider certification, upstream invoice accuracy, or a disaster recovery RPO.
Feature owners follow the ownership map in [architecture.md](architecture.md).

| Contract and owner | Assumptions and failure behavior | Evidence |
| --- | --- | --- |
| Identity (`access`, `runtime`) | Administrators belong to one trusted installation. API-key authority is polled every five seconds. New admissions stop when the successful authority read is 60 seconds old, measured from the start of that read with a monotonic clock. Database isolation therefore cannot keep old keys usable indefinitely. Already admitted ordinary streams retain their pinned policy and may finish; realtime sessions recheck the key every five seconds. A newly revoked key normally disappears on the next successful poll or hint. Explicit provider credential-version revocation travels on the same poll and applies to retained releases; a request already pinned to that version fails selection rather than reusing it. | [authority and replica tests](../tests/integration/m4_process_test.go), [runtime tests](../internal/runtime/) |
| Budget (`limits`, `usage`) | Individual-key and shared-group daily/monthly accrued-cost thresholds use UTC boundaries and exact decimal arithmetic. Concurrent accepted work can exceed a threshold. Unpriced work accrues no money. These are not reserved invoice caps. Missing, malformed and wrong-window spend state fails closed. A current-window hash alone does not prove that attribution is current; there is no measured maximum lag or monetary overshoot guarantee. | [limits tests](../tests/integration/limits_test.go), [recovery tests](../tests/integration/m4_recovery_test.go), [concepts.md](concepts.md) |
| Metadata privacy (`usage`, `observability`) | Request facts contain metadata only. Prompts, outputs, authentication tokens, cookies, and uploaded content are prohibited from persistent diagnostics. Provider-retained content is a separate opt-in/resource policy. Provider names, route slugs and operator labels are metadata: do not put secrets in them. API response families use `no-store`; public static assets remain separate. | [metadata tests](../internal/usage/), browser response-header checks, [process isolation](../tests/integration/process_test.go) |
| Queue durability (`usage`) | A Valkey acknowledgement is not an fsync guarantee. Compose uses AOF `everysec`; a power/storage disaster can lose roughly one second of recent writes, and replication/failover can add loss. This is a persistence setting, not a guaranteed fleet RPO. Surviving epochs, gaps and pending counts expose known incompleteness; lost facts cannot be reconstructed from counters. | [Valkey persistence](https://valkey.io/topics/persistence/), [replica and recovery suites](../tests/integration/), [operations.md](operations.md) |
| Egress (`egress`, `access`, `providers`) | HTTPS, DNS validation/pinning and refusal of redirects remain the default. Explicit provider exceptions are installation-wide administrative trust decisions, not origin-specific tenant isolation. They never grant OIDC access. Release builds refuse test-only OIDC configuration. | [egress tests](../internal/egress/), [OIDC integration](../tests/integration/oidc_test.go) |
| Recovery identity (`database`, `secrets`, `usage`) | Replacement recovery preserves the installation UUID, keys and historical references. It must use an isolated Valkey service. A database copy is not a supported independently writable clone. Create a fresh installation for independent use, then recreate nonsecret configuration through the management API. | `scripts/backup.sh`, `scripts/restore.sh`, `console/tests/journeys/recovery.spec.ts` |

## Durable metadata and upgrades

The metadata stream carries JSON in one `event` field. `version: 1` and the
original unversioned 3.0 envelope share the version-1 contract; older readers
can ignore the added version field. Readers retain unsupported versions in the
pending list without acknowledging or deleting them. Deploy a compatible reader
to resume processing; do not delete pending entries to silence a backlog.
Malformed and permanently invalid records retain the existing explicit
loss-report policy.

Only identical schema/event contracts may overlap during a rolling update. A
future incompatible event writer must not overlap old readers merely because an
old reader ignores unknown JSON fields. Introduce such writers only after all
readers support the version, or quiesce and drain before the change. Workers use
`Recreate` in Helm. Rust 2.x and 3.x storage is incompatible with the Go storage
format. Go upgrades preserve and verify sequential migration checksums;
populated forward-upgrade and interrupted-DDL tests qualify that path. Every
subsequent migration must qualify populated data, lock duration and recovery
from failure before promotion.

## Capacity qualification

The production Helm example starts at 32 in-flight inference requests per
gateway. At a 16 MiB response cap this permits 512 MiB of response bytes before
parser expansion, copies, request bodies, streams and other memory. This is
headroom, not a measured memory guarantee. Measure maximum-size unary, streaming
and media traffic and slow-reader/mass-disconnect behavior before increasing
concurrency. The default evaluation chart's 256-request limit can permit 4 GiB
of response bytes alone; it is unsuitable for simultaneous maximum-size buffered
responses under a 2 GiB memory limit.

Helm's media spool is disk-backed `emptyDir`; the production gateway requests 2
GiB and limits 3 GiB of ephemeral storage for a 1 GiB application spool inside a
2 GiB volume. Compose uses tmpfs, which consumes memory; its production overlay
allows 4 GiB for the process and spool. Capacity reservations and volume limits
do not make an unhealthy host disk reliable. Monitor reserved bytes, filesystem
free space, evictions and worker recovery. A node-local spool is not a durable
replacement for provider-owned asynchronous job references.

Keep analytics in control processes and ingestion in workers, with database
CPU/I/O headroom for both. Qualify optimizations with measured workloads.

The September 18, 2026 candidate qualification contains resource observations
for maximum-size concurrent uploads, slow reads, mass disconnects, connection
aging and shutdown. Those fixture measurements are reproducible bounds checks,
not a throughput target or a production availability guarantee. See
[historical qualification](roadmap/README.md), not a qualification of newer
source.
