# Management features

## Contracts and feature ownership

Feature packages own SQL, transactions, validation and audit provenance. Prefer
concrete types and explicit dependencies, and introduce interfaces only at
actual substitution boundaries. Package context supplies names instead of
redundant provider- or protocol-specific aliases. `make api` combines the
[contract fragments](#contract-fragments) into `management/management.json`,
which `management/document.go` embeds. The same command generates Go and
TypeScript types, console authorization requirements and safe SDK, CLI and MCP
consumer metadata. Unsupported serializer shapes fail generation, and
integration tests enforce contract and handler parity.

Callers own logical mutation identity. Console drafts retain submitted payload,
ETag and Idempotency-Key through ambiguous retries, retire secrets on
abandonment or resource and principal changes, and fence stale callbacks. A
confirmed write is not repeated because refresh failed. Provider detail state
owns draft reconciliation and semantic commands, while the wizard retains its
onboarding phases. Personal grant state owns enrollment lifetime and
stale-response rejection. API key and publication form modules compile typed
commands. Playground rendering, inspection and submission consume the same
resolved execution plan.

Configuration promotion owns portable intent and destination bindings rather
than feature validation. Direct and promoted provider edits share
credential-boundary, certification-invalidation and staged-inventory operations.
Unchanged slots retain enrollment lifetime, and parent and collection ETags
advance coherently. Installation serialization protects reauthorization,
replay, publication and atomic audit. Every `access` mutation helper acquires
that lock and refreshes authorization before feature commands run.
Project-policy batches and billing-import writes share their feature-owned
persistence commands with promotion.

Each promotion section parses its part of the generated contract document once
at the request boundary, reads and renders destination state, and contributes a
planner and ordered appliers. Plan, apply and export run the same section code,
and the document digest is computed over the rendered canonical document. Every
credential binding must be claimed by a section reference, and unclaimed
bindings are refused before any planner runs.

`providers.Server` owns provider draft, slot, credential, certification,
activation and grant enrollment changes. Promotion writes drafts through the
narrower `provider.Drafts` interface it declares. The stored configuration
document and the kind registry live in `providers/connection`, upstream probes
in `providers/probe`, and code accounts in `codesessions/codeaccounts`. Network
steps run outside mutation locks, with authority and slot identity checked again
before commit. Personal grant enrollment remains a separate protocol.

`internal/agents/publications` keeps two publication commands. `Publish` validates
current references even when the draft digest already exists. Promotion uses
`EnsureRetainedRevision` to reuse retained definitions without revalidating
changed dependencies. Revision retention, labels, final selection, authority and
runtime publication remain in one transaction.

Provider configuration identity and connector projection live in
`connectors/providerconfig`. Provider mutations accept a prepared configuration
whose constructor owns normalization, plugin resolution and validation. Stored
management and serving representations retain their original serialization.

MCP follows the same feature ownership. `mcp/registration` owns registration
validation, revisions, executable permissions and publication. Direct writes and
configuration promotion share preparation and persistence operations that return
new values. `mcp/upstream` owns connections and catalog certification, using the
same tool-schema compiler as routing snapshots.

The `mcp` executor owns authorization, guardrails, reservation, dispatch and
settlement through constructor-injected dependencies. JSON, Streamable HTTP,
legacy SSE and workflow adapters translate explicit execution outcomes into
their response contracts. `mcp/accounting` owns durable accounting and recovery,
while `mcp/sessions` owns session identity, capacity, expiry and Valkey
persistence. `mcp/messages` supplies bounded JSON-RPC handling with raw request
IDs to consumer, upstream and management adapters. Management keeps its own
authorization and administrative dispatch.

`grants/personal` owns personal OAuth, Entra exchange, renewal and delegated
signing for MCP and agent calls. Configure-time validation remains with
registration over the shared types in `domain/grantconfig`. Registration hands
`grants/personal` its server lookup through the constructor in `cmd/olp`.

## Contract fragments

The management contract is edited in `management/openapi/`, where every file is
a partial OpenAPI document.

| File | Content |
| --- | --- |
| `<tag>.json` | The paths and webhooks of one tag, and the components that only this tag uses, in a file named for the tag in lower case |
| `shared.json` | Components that more than one fragment uses, and the security schemes |
| `document.json` | The `openapi` version, `info` and the operation that serves the contract |

An operation with a tag belongs in the file of its first tag, and a new tag only
needs a new file. A fragment refers to its own component as
`#/components/schemas/Name` and to a shared one as
`./shared.json#/components/schemas/Name`. It cannot refer to another tag's
fragment, so move a component into `shared.json` when a second fragment needs
it. Fragments are canonical two-space JSON.

`make api` refuses a path, webhook, component or `operationId` that is defined
twice. It then runs the [Redocly CLI](https://redocly.com/docs/cli/commands/bundle)
bundler to write `management/management.json`, which lists paths, webhooks and
components in name order. Never edit that file by hand. `make check-contracts`
fails when it differs from what the fragments produce. After a merge conflict in
it, resolve the fragments and run `make api` again.

## Adding a management endpoint

Copy `internal/guardrails/credential/api.go`. It is the reference for every
management endpoint and shows the complete shape in one file.

1. Describe the operation in its [contract fragment](#contract-fragments) and
   run `make api`.
   `Route` refuses to start when a registered pattern is missing from the
   contract, and it reads the permission the route needs from there.
2. Register the handler in the feature's `Register(mux)` with
   `server.Route(mux, "METHOD /api/v1/...", handler)`.
3. A read handler queries the pool, checks the caller's scope with
   `p.Project(&projectID, authz.View)` and returns `httpkit.OK`,
   `httpkit.Detail` or a list reply.
4. A write handler decodes and validates the body, then runs one of the six
   helpers in `internal/access/mutation.go` listed below.
5. For `Mutate`, `Remove`, `Transact` and `Preflight`, provide a `lock` step
   that loads the current row `FOR UPDATE` and authorizes the caller against it,
   or calls `RequireProject` for a create (use `access.Unlocked` when there is
   nothing to load). The `apply` step checks `httpkit.Match`, writes the change,
   and returns the `httpkit.Reply` together with the `access.Event` to audit.

| Helper | Steps | Behavior |
| --- | --- | --- |
| `access.Mutate` | `lock` and `apply` | Runs a change carrying `Idempotency-Key`. Locks and authorizes current state first, replays the stored reply if the same key and input already completed, and otherwise runs `apply`, records the audit row, stores the replay and commits. |
| `access.Remove` | `lock` and `apply` | Runs an idempotent delete, checking the stored replay before `lock` because a completed deletion leaves no row to lock. |
| `access.Transact` | `lock` and `apply` | Runs a change without `Idempotency-Key`, such as an edit guarded only by `If-Match`. |
| `access.Preflight` | `lock` only | Precedes `access.Mutate` when the change must call an upstream first. Authorizes the caller and serves any stored replay, then releases the installation lock so network calls never hold it, as `internal/providers/credentials.go` does for credential rotation. |
| `access.Run` | single `step` returning `(T, Event, error)` | Commits a step whose result is a value rather than an HTTP reply, such as claiming a grant enrollment or media-job deletion before an upstream call. |
| `access.Serialize` | single `step` taking `pgx.Tx` and returning `(T, bool, error)` | Acquires the installation `FOR UPDATE` lock without caller reauthorization or automatic audit, committing only when the step returns `true`. Used for unauthenticated or setup-stage serialized flows in `internal/identity/` such as initial setup, sign-in, MFA, OIDC and SAML transitions. |

The mutation helpers own the transaction, the replay record, the audit row and
the commit, so a handler never calls `tx.Commit`. The functions that open the
management transaction and read the replay record are unexported, so only
`internal/access` can call them. `Mutate`, `Remove`, `Transact` and `Run` record
their returned `access.Event` through `access.AuditForProject`. Call
`access.Audit` or `access.AuditForProject` directly only to record an extra
audit row in the same change or inside `access.Serialize`.

## Problem values

A refusal the caller can act on is a `*problem.Problem` from
`internal/domain/problem`. It carries a status, a stable code, a detail
sentence and, for a validation failure, the field at fault. Handlers, stores,
workers and validators return it as an ordinary error.

Build one with `problem.Fail`, `problem.Invalid` or `problem.Forbidden`, or
return `problem.NotFound`. Some helpers cover the common cases.
`database.NotFoundOr` turns `pgx.ErrNoRows` into `problem.NotFound`,
`textfield.Validate` requires text within a length limit, the parsers in
`internal/domain/ids` reject a malformed identifier, and `problem.FromContract`
reports an obligation that a strict route target cannot meet.

`internal/access/httpkit` owns the HTTP side. `httpkit.ProblemFor` maps an error
to a problem, including database and routing errors that are not problem values
themselves, and turns an error it does not recognize into a 503.
`httpkit.WriteProblem` writes the result as `application/problem+json`. The
`store_worker_validation` [structure rule](code-structure.md#structure-rules)
keeps httpkit out of store, worker and validation files.
