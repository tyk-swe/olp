# ADR 0001: Enterprise resource hierarchy and ownership

- Status: Accepted target (approval pending)
- Decision issue: XOD-83 / M0-01
- Scope: domain, storage, management API, gateway, security, operations, and console
- Companion contract: `../contracts/resource-ownership.json`
- Supersedes: the implicit installation-global ownership model for future schema and API work

## Context

OLP 2.0 is deliberately a hardened single-installation system. The database has
one `installation` row, users have installation-wide fixed roles, configuration
names are globally unique, one runtime contains every provider, route, and
inference key, and operational facts do not carry tenant scope. Those are valid
single-installation choices, but they are not an enterprise isolation model.

Enterprise work needs a stable answer to three separate questions:

1. Which boundary owns a resource and may mutate it?
2. Which boundary may use a resource without owning it?
3. Which immutable scope must be copied onto operational and financial facts?

Ownership must not be inferred from a route slug, a user's current UI context,
an API-key prefix, a provider name, or a PostgreSQL session variable. UUIDs are
identifiers, not authorization. PostgreSQL row-level security may later add
defense in depth, but application authorization and database constraints must
remain sufficient without it.

## Decision

OLP adopts exactly four hierarchy levels for the enterprise beta:

```text
installation
└── organization
    ├── memberships, identity providers, groups, and custom roles
    ├── provider connections and immutable provider/credential revisions
    ├── pricing books and organization policy defaults
    └── project
        ├── applications and service accounts
        └── environment
            ├── provider grants
            ├── routes and immutable revisions
            ├── inference credentials
            ├── policies, quotas, and budgets
            └── environment runtime generations
```

No additional hierarchy level is supported in the beta contract. Labels and
applications must not be used to simulate another security boundary.

### Boundary meanings

| Boundary | Meaning | Security consequence |
|---|---|---|
| `installation` | One deployed OLP control plane and its operator trust domain. | Owns global identities, installed connector types, cryptographic keyrings, process coordination, and installation-only evidence. It is not a tenant. |
| `organization` | Security, identity, billing, residency, and provider-credential boundary. | Cross-organization references are forbidden unless this ADR explicitly names an installation-owned reference. Organization administrators cannot enumerate another organization. |
| `project` | Ownership, application administration, and cost-allocation boundary inside one organization. | A project cannot outlive or move away from its organization. Project-scoped names need only be unique within that organization where stated. |
| `environment` | Independently published runtime boundary inside one project. | Routes, public inference credentials, policies, limits, and runtime activation/rollback are isolated here. A request resolves exactly one environment before routing. |

Users are installation-global identities. A user gains organization access only
through an explicit organization membership. Authentication never grants a
membership, and membership does not imply access to every project or
environment. Resource-scoped role bindings provide that access.

### Ownership and use are distinct

A provider connection, its secret revisions, and its immutable activated
revisions are organization-owned. A project or environment may use that
connection only through an explicit provider grant. A grant is an authorization
resource, not a copy or ownership transfer. It may narrow models, operations,
network policy, credential revision policy, and environment access; it may not
widen the provider connection's organization policy.

An environment runtime may reference only provider revisions reachable through
an active grant whose organization matches the environment's project. Revoking
a grant prevents future activation and is a security publication event. Pinned
requests retain their already-authorized immutable transport, while new requests
fail closed after the bounded propagation contract in ADR 0002.

### Durable scope rules

Every durable resource is one of the following:

- directly owned: it stores its authoritative scope ID;
- inherited child: it stores enough parent and scope IDs for a composite foreign
  key to prove same-scope ancestry;
- immutable fact: it copies organization, project, and environment attribution
  fixed at event creation;
- installation internal: it is unavailable to tenant APIs and has a documented
  installation-only reason; or
- global identity/reference data: it has explicit membership or reference edges
  into organizations and is never treated as tenant-owned.

New scoped tables must normally store the full bounded ancestry required by
their access path. A leaf `environment_id` foreign key alone is not sufficient
for high-value cross-table references: composite keys must prove that related
rows share organization, project, and environment. Denormalized ancestry is an
integrity and query-safety mechanism, not a second source of truth.

Operational records copy the scope that was authenticated and pinned. They do
not recompute scope from mutable configuration during reads or rollups. Records
for installation-wide failures, such as a process epoch or an unrecoverable
multi-environment stream interval, remain installation-only evidence. When a
failure can be attributed to an environment, a scoped fact is emitted as well;
unknown scope is never guessed or exposed as another tenant's gap.

The exhaustive mapping for current physical tables and planned resource kinds
is machine-readable in `resource-ownership.json`. A future migration adding a
`CREATE TABLE` must update that contract in the same change.

## Naming and uniqueness

1. UUID resource IDs are globally unique within an installation and stable
   across revisions, backup/restore, and default-scope migration. They do not
   authorize access.
2. Human names are normalized and unique only inside their owning boundary:
   organization names per installation; project names per organization;
   environment names per project; provider connection names per organization;
   route slugs and inference-credential names per environment.
3. Public inference credential lookup IDs remain installation-unique because
   authentication must find credential authority before trusting a client scope.
   The lookup ID reveals no scope and is not a tenant selector.
4. External identity subjects are unique within an identity-provider
   configuration, not globally by issuer string alone.
5. Idempotency keys are unique by authenticated principal, resolved scope,
   semantic operation, and key. A replay from another scope is a conflict, never
   a hit.
6. Cursor payloads bind the scope, filter fingerprint, ordering version, and
   last position. Reusing a cursor in another scope fails without enumeration.

## References

- Same-scope references use composite foreign keys wherever PostgreSQL can
  enforce them. Application checks are additional, not substitutes.
- Environment resources cannot reference a project in another organization or
  an environment in another project.
- Environment resources may reference an organization-owned provider or pricing
  revision only through a valid same-organization grant/reference edge.
- Global users may be referenced by creator/actor fields, but authorization is
  proven by a membership and binding captured at mutation time. A creator
  reference never confers continuing access.
- Immutable facts may retain identifiers for deleted or disabled resources.
  Referential actions must not erase audit, usage, pricing provenance, or
  security evidence.
- Polymorphic `resource_type/resource_id` links are display metadata only unless
  accompanied by explicit scope and an integrity-checked typed reference.

## Deletion, transfer, and restore

### Deletion

Configuration owners first enter a disabled or deleting state. Admission and
new mutations stop, credentials and grants are revoked, active work drains, and
retention policy runs before physical deletion. Cascades are allowed only for
ephemeral or unpublished children. Published revisions, facts, audit evidence,
and records needed to interpret them are restricted or tombstoned.

Organization deletion is an installation-operator workflow because it crosses
identity, secret, billing, and residency obligations. Project and environment
deletion is organization-authorized but still asynchronous and evidence-backed.
Deleting a parent must not make a retained fact appear to belong to a surviving
sibling.

### Transfer

The beta does not transfer organizations between installations, projects
between organizations, environments between projects, provider connections
between organizations, routes between environments, or inference credentials
between environments. Clone/export, validate, activate, then delete the source.
This preserves scope IDs, provenance, idempotency, budgets, and audit semantics.

Users may join or leave organizations by membership changes. Applications and
service accounts may change project-local ownership bindings but do not change
their project. Provider grants may be created or revoked without moving the
provider connection.

### Restore

A full restore preserves installation and all scope IDs. It must not silently
merge two installations. Import into a live installation is a declarative
resource operation with collision and scope checks, not a database restore.
Partial database restore of one organization is outside the beta contract;
declarative export/import is the supported future path.

## Retention

- Prompts, outputs, reasoning, tool payloads, uploads, raw headers, and
  credentials are not added to durable operational records.
- Current request, usage, and audit defaults (30, 90, and 365 days) remain
  compatibility defaults until an organization policy explicitly overrides an
  allowed range. Retention selection is captured with each fact or batch.
- Hourly usage, gap evidence, pricing provenance, and audit records remain
  scoped after raw facts age out. Unknown usage remains incomplete or unpriced,
  never zero.
- Secret ciphertext and credential revisions survive while active runtime,
  audit, or recovery evidence references them; plaintext never becomes a
  migration or export field.
- Expired sessions, authorization flows, public-auth buckets, replay receipts,
  and published outbox rows follow bounded operational retention and cannot be
  used as tenant discovery surfaces.

## Deterministic default-scope migration

Every existing 2.x installation migrates into one default organization, project,
and environment without changing any existing resource ID, revision number,
route slug, public key material, runtime digest history, or fact identifier.

### Frozen compatibility identities, roles, and settings

The UUIDv5 namespace for all three default hierarchy IDs is the persisted
`installation.id`. The names are exactly `olp/default-organization/v1`,
`olp/default-project/v1`, and `olp/default-environment/v1`. The default
organization retains `installation.organization_name`; the project and
environment both have display name `Default` and normalized name `default`.
Those strings are server-owned compatibility data, not client selectors.

Migration creates four reserved built-in organization roles. Each role UUID is
UUIDv5 in the default organization namespace using the name below. One
organization-scoped binding applies to that organization's project and
environment descendants, so the default hierarchy preserves the current
fixed-role behavior without treating membership itself as authorization.

| Legacy value | Stable role key | UUIDv5 name | Exact legacy permission set |
| --- | --- | --- | --- |
| `owner` | `olp.bootstrap.organization.owner.v1` | `olp/bootstrap-role/owner/v1` | `read_configuration`, `manage_providers`, `manage_routes`, `manage_api_keys`, `read_team`, `manage_team`, `manage_sessions`, `read_operations`, `use_playground`, `manage_settings`, `manage_pricing` |
| `operator` | `olp.bootstrap.organization.operator.v1` | `olp/bootstrap-role/operator/v1` | `read_configuration`, `manage_providers`, `manage_routes`, `manage_api_keys`, `read_team`, `read_operations`, `use_playground`, `manage_settings`, `manage_pricing` |
| `developer` | `olp.bootstrap.organization.developer.v1` | `olp/bootstrap-role/developer/v1` | `read_configuration`, `manage_api_keys`, `read_operations`, `use_playground` |
| `viewer` | `olp.bootstrap.organization.viewer.v1` | `olp/bootstrap-role/viewer/v1` | `read_configuration`, `read_operations` |

The mapping is exhaustive for `users.role`, `invitations.role`,
`oidc_configurations.default_role`, `oidc_email_role_mappings.role`, and
`oidc_group_role_mappings.role`. Active users receive an active membership and
binding. Inactive users receive disabled records and no valid session
authorization. Pending, unexpired invitations preserve their digest, expiry,
inviter, and mapped role; migration never mints replacement bearer material.
Accepted or expired invitations retain only their existing lifecycle/evidence
state. Every non-null OIDC role value maps through the same table. An unknown
role aborts verification instead of widening or dropping access.

The complete current setting-key registry is also frozen. Each of
`retention.requests_days`, `retention.usage_days`, and
`retention.audit_days` becomes an organization-owned setting on the default
organization. Future environment overrides are separate scoped rows and do not
change ownership of the migrated organization default. An unregistered current
key aborts migration verification; it is never guessed or copied globally.

The migration uses expand -> backfill/dual-write -> verify -> contract:

1. Add a persistent `installation.id` and nullable default-scope IDs. The
   installation ID is generated once under the setup advisory lock and restored
   unchanged from backup.
2. Derive the three default IDs from the stored installation ID using the fixed
   UUIDv5 names above. Derivation occurs in application migration code and
   requires no PostgreSQL extension. Repeated runs produce the same IDs.
3. Create the hierarchy idempotently. The existing
   `installation.organization_name` becomes the default organization display
   name. Create the project and environment with the exact reserved names above.
4. Create one organization membership and mapped role binding per existing
   user, and translate invitation and OIDC role fields through the exhaustive
   compatibility table above. Existing unexpired sessions remain valid only
   when the migrated user, membership, and binding are active.
5. Attach providers, credentials, provider models/revisions, OIDC configuration,
   organization pricing, and the three registered retention settings to the
   default organization. Create explicit grants from each existing active
   provider to the default project/environment.
6. Attach routes, route revisions/drafts, API keys, and runtime generations to
   the default environment. The public credential authority initially maps each
   unchanged lookup ID to that environment.
7. Backfill requests, attempts, usage, media, audit, idempotency, and related
   facts from their immutable resource relationships. Rows with contradictory
   or unresolvable ancestry stop verification; they are never assigned by name
   or timestamp heuristics.
8. Dual-write old and new columns while N/N-1 behavior remains supported. The
   legacy management facade resolves only the stored default scope and rejects
   ambiguity.
9. Verify counts, nulls, composite ancestry, uniqueness, runtime digests,
   credential lookup mappings, revisions, and retained usage totals. Only then
   make scope columns non-null and remove obsolete global uniqueness.

The default hierarchy is ordinary data after migration but cannot be deleted
while the legacy compatibility facade is enabled.

## API and authorization consequences

- Scoped routes take explicit organization/project/environment identifiers or
  inherit them from a parent resource already authorized by ID.
- List, get, mutate, pagination, idempotency, audit, export, and activation all
  bind the same resolved scope.
- A missing resource and an inaccessible resource have the same external
  response. Logs and metrics use bounded opaque scope identifiers.
- The console's selected scope is presentation state. The server derives
  authority from the session, membership, binding, resource ancestry, and
  concurrency token.
- RLS, when enabled, receives already-validated scope and provides defense in
  depth. No correctness test may pass only because RLS hid an application bug.

## Consequences

This decision adds scope columns and composite indexes to many hot tables and
requires explicit compatibility migrations. In return, isolation becomes
testable at domain, storage, API, runtime, and operational layers. Environment
activation can evolve independently, organization provider reuse is controlled
by grants, and historical facts retain unambiguous cost and security ownership.

The decision intentionally defers cross-organization sharing, hierarchy
customization, project/environment transfer, partial tenant database restore,
and multi-region active-active behavior.
