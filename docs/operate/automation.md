# Management automation

The `olp` binary includes management clients as well as the server. Client
commands use the public management API; they do not load the server's database
URLs, bootstrap token or encryption keys. Commands and MCP tool schemas are
generated from `openapi/management.json` by `make api`.

## Connect the CLI

Create a [management token](../use/access.md#management-tokens-and-provisioning) with
only the scopes and projects the automation needs. Mount it in a private file:

```sh
export OLP_MANAGEMENT_URL=https://olp.example.com
export OLP_MANAGEMENT_TOKEN_FILE=/run/secrets/olp-management-token
olp keys list
olp providers list
olp routes list
```

The URL is an origin, without `/api/v1`. HTTPS is required except on loopback.
The client refuses redirects and rereads the token file for each request.
Credentials are never passed as arguments or printed in errors.

### Generated commands

`olp GROUP help` lists commands, operation IDs, methods and paths in that group.
`olp api help` lists machine-admitted operations with JSON or empty success
responses. Uploads, downloads, live SSE and XML use their native console or HTTP
transports. Use an operation ID to avoid ambiguity in nested resource names:

```sh
olp keys get KEY_ID
olp keys update KEY_ID --body-file key.json --if-match OBSERVED_ETAG
olp providers activate PROVIDER_ID --if-match OBSERVED_ETAG \
  --idempotency-key DEPLOYMENT_KEY
olp api put_credential_slot PROVIDER_ID SLOT_ID \
  --body-file slot.json --if-match OBSERVED_ETAG \
  --idempotency-key SLOT_EDIT_KEY
olp api simulate_route_draft ROUTE_ID --body-file request.json
```

Path values are positional in contract order, or `--path NAME=VALUE`. Queries
use repeatable `--query NAME=VALUE` for distinct names. Current generated query
parameters are scalar strings, integers or booleans; repeated values for one
name are refused. Optional null values are omitted by the Go client; zero and
false remain explicit values. JSON requests use `--body-file FILE`;
output goes to stdout or `--output FILE`. Output files are replaced atomically
with owner-only permissions. The server validates schemas and project access
just as it does for the console.

Reads print an `ETag:` line to stderr when the API supplies one. Atomic child
mutations also expose the previous and resulting parent ETags; MCP returns
`previous_parent_etag` and `parent_etag` from the declared response contract. Conditional
writes require that observed ETag with `--if-match`; the CLI never fetches a
new ETag to force a stale write through. When the contract requires an
idempotency key, supply `--idempotency-key` before the first write; omission
fails before dispatch. Retain that key and the exact payload after an uncertain
response, and reuse both for the same logical write. A deliberate new write
requires a new identity.
The CLI implements no application-level mutation retry loop. The standard HTTP transport may replay the original request and idempotency key on a recoverable reused-connection failure.

Key creation and rotation return their one-time secret, matching the API.
Keep that output out of CI logs, or use `--output` and store the file securely.
Error messages exclude server-controlled request details.

### Published route lifecycle

`routes create` and `routes update` publish a validated route revision atomically.
Use `routes create-draft` and `routes update-draft` for editable console drafts.
Published updates require the observed published ETag and an Idempotency-Key;
they retain the current routing policy and preserve independent drafts.

### Provider and credential-slot lifecycle

`olp api delete_provider PROVIDER_ID --if-match ETAG --idempotency-key PROVIDER_DELETE_KEY`
removes an unused draft provider. Published revisions, route drafts,
provider-specific price history,
notification evidence and other retained dependencies produce a conflict.
Disable published providers through `disable_provider`. Successful deletion
removes the draft's owned sealed credentials, advances authority and records
one audit event; an explicit idempotency key replays its completed result.

`get_credential_slot` returns one draft slot and its individual ETag. Use that
observed value with `put_credential_slot` or `delete_credential_slot` to edit a
slot independently. Pool edits can continue using the collection ETag from
`credential_slots`. A stale individual or collection ETag refuses mutation.
The default slot is required. Removing another draft slot preserves immutable
credential versions and the slots pinned in published revisions; activate the
changed provider draft to publish its new pool.

### Usage export

```sh
olp usage summary --query start=2026-10-01T00:00:00Z \
  --query end=2026-10-09T00:00:00Z
olp usage breakdown --query start=2026-10-01T00:00:00Z \
  --query end=2026-10-09T00:00:00Z --format csv --output usage.csv
```

CSV columns are sorted; nested objects remain JSON in cells. Cells that
spreadsheets could interpret as formulas get an apostrophe prefix. JSON output
preserves the API's coverage and completeness metadata.

Slot PUT/DELETE responses include `OLP-Previous-Parent-ETag` and `OLP-Parent-ETag`: the exact provider transition committed by that request. Idempotent replay preserves those original values. Clients coordinating parent and child resources can follow a contiguous chain of their own writes; a missing transition remains a conflict and never authorizes adopting an unrelated edit.

## Configuration promotion

Export from the source, then plan and apply against the destination:

```sh
olp config export --output configuration.json
# Switch OLP_MANAGEMENT_URL and OLP_MANAGEMENT_TOKEN_FILE to the destination.
olp config plan --file configuration.json \
  --bindings-file /run/secrets/promotion-bindings.json --output plan.json
olp config apply --plan-file plan.json \
  --bindings-file /run/secrets/promotion-bindings.json \
  --idempotency-key DEPLOYMENT_KEY
```

`--file` accepts the export envelope or its `document` object. Bindings are a
JSON object mapping artifact credential references to secret strings. They are
sent only to the destination and **never saved in the plan**. Supply them
separately at both stages.

Saved plans use CLI format version 2. They record the destination origin and
export digest, an external-reference binding digest, the secret-free desired
document, actions, conflicts and blockers, and a local intent consistency
checksum. Indentation changes preserve that checksum; changing native object
member order or numeric representations does not. The checksum is a local
consistency check; the server decides authorization and whether to mutate.
Regenerate existing version 1 plan files with `config plan`. This CLI artifact
format change does not change the configuration document or management API.

Planning saves the artifact even when blockers or conflicts cause a nonzero
exit. Apply refuses such plans, a different destination, changed external
references, or inconsistent saved intent. It sends the original document and
destination digest directly to the apply API. The server checks current
authority and exact completed replay before evaluating a new write against
the destination digest. An intervening destination change refuses a new write;
resolve it and compute a new plan.

After an uncertain apply response, retry the same saved plan with its original
`--idempotency-key` and unchanged binding files. A completed replay returns the
original result even if the destination changed afterward. The CLI does not
re-plan that retry or replace its original precondition; revoked authority
still prevents replay.

## Configure qualified clients

`client-env` prints POSIX shell setup without embedding the gateway key:

```sh
olp client-env claude-code --url https://olp.example.com \
  --key-file /run/secrets/inference-key --model assistant --output client.sh
. ./client.sh
```

Supported names are `openai`, `anthropic`, `gemini`, `claude-code`, `gemini-cli`
and `codex`. The shell reads the key file when sourced. Claude Code's background
models use the requested route. Codex setup defines `olp_codex`, a wrapper that
selects the custom Responses provider and disables hosted tools unsupported
on strict routes; run `olp_codex exec "Hello"`. It does not change sandbox or
permission settings. See [qualified clients](../use/clients.md) for release policy
and qualification boundaries.

Qualified framework setup supports `openai-agents`, `vercel`, `langchain` and
`llamaindex`. These names default to JavaScript modules, with `--surface openai`,
`anthropic` or `gemini` selecting the qualified constructor. Agents uses the OpenAI
Responses surface with tracing disabled and `store: false`; choose a route that
supports those settings. Vercel uses Chat Completions for its OpenAI model. Native
Anthropic/Google clients receive their required versioned endpoints. Frameworks
that recognize model names require a compatible route name, as described in the
[client qualification guide](../use/clients.md).

```sh
olp client-env langchain --surface anthropic --url https://olp.example.com \
  --key-file /run/secrets/inference-key --model claude-sonnet-4-6 \
  --output olp-client.mjs
```

Import the exported `client` and `modelName` into your application; Vercel exports
`model`, while Agents exports `runner` and `agent`. The generated module only
configures clients and reads the mounted key when loaded. It makes no inference
request. The aliases `agents`, `ai-sdk` and `llamaindex-ts` are also supported.

The native `openai`, `anthropic` and `gemini` names accept `--format javascript`,
`python` or `go` in addition to their default shell environment. Python exports
`client` and `model_name`; Go produces a formatted `olpclient` package with `New`
and `Model`. Code formats set explicit keys and base URLs instead of depending on
SDK-specific environment behavior. OpenAI loopback Go setup opts into plain HTTP
only after the CLI's origin validation. SDK/framework retries are disabled where
supported so application retries remain an explicit choice. Generated files never
contain the key value.

## Management MCP

Point a Streamable HTTP client at `https://olp.example.com/api/v1/mcp`, with
`Authorization: Bearer MANAGEMENT_TOKEN`. This stateless endpoint accepts
JSON-RPC over POST; it does not create sessions or maintain an SSE stream.
It supports initialization, ping, `tools/list`, `tools/call`, and initialized
and cancellation notifications. Supported protocol versions are `2025-03-26`,
`2025-06-18` and `2025-11-25`.

Tool names are contract operation IDs. Generated JSON input schemas use `path`,
`query`, `body`, `if_match` and `idempotency_key` as applicable:

```json
{
  "jsonrpc": "2.0",
  "id": 1,
  "method": "tools/call",
  "params": {
    "name": "update_api_key",
    "arguments": {
      "path": { "api_key_id": "KEY_ID" },
      "body": { "name": "renamed" },
      "if_match": "OBSERVED_ETAG"
    }
  }
}
```

Only currently authorized tools appear in `tools/list`. Read-only tokens
cannot mutate; guessing a hidden tool's name does not bypass authorization.
Session-only operations, raw uploads/downloads, live SSE, XML and MCP itself
are not JSON tools. Each call
uses the API's authentication, scoped project access, validation, ETag check,
idempotency and audit handler. MCP requires an explicit idempotency key wherever
the API does; it does not invent one.

Results include API status, non-secret body and ETag in `structuredContent` and
text content. One-time keys, tokens, passwords and secret bindings are removed
recursively. Failed responses omit details that could echo secret input.
A key created through MCP therefore has **no retrievable secret**; use the CLI
or console when you need to capture a new key. Token revocation and changes
to the creator's authority apply to both transports.

## GitHub Actions

The reusable
[`configuration-promotion.yml`](../../.github/workflows/configuration-promotion.yml)
plans a configuration artifact on a pull request, comments its actions,
conflicts and blockers, and applies the merged artifact on a push to the
configuration repository's default branch. It uses the generated management
CLI and the same saved-plan destination digest checks as an interactive apply.

Call it from a configuration repository:

```yaml
name: OLP configuration
on:
  pull_request:
  push:
    branches: [main]
permissions:
  contents: read
  pull-requests: write
jobs:
  promote:
    uses: tyk-swe/olp/.github/workflows/configuration-promotion.yml@REVIEWED_OLP_COMMIT
    with:
      installation_url: https://olp.example.com
      artifact_path: installation.json
      olp_ref: REVIEWED_OLP_COMMIT
      external_bindings_path: production-references.json
      reviewed_plan_path: olp-plan-review.json
      apply: ${{ github.event_name == 'push' }}
    secrets:
      management_token: ${{ secrets.OLP_MANAGEMENT_TOKEN }}
```

Replace both commit placeholders with the same reviewed, full 40-character OLP
commit containing this workflow and CLI. The workflow builds only that source;
configuration checkout contents are read as data. Artifact and reference paths
must resolve inside that checkout. Its token is written to a private temporary
file for each operation and removed afterward. Fork pull requests require the
caller's explicit secret-access policy; the workflow cannot manufacture missing
credentials or permissions.

The token needs installation-wide configuration/read authority and any
additional scopes required by the artifact: pricing and external credential bindings require `settings`, and
identity or public-catalog changes require `access`. Give a deployment token
only the required scopes. Destination secret references belong in the optional
external-bindings JSON; omit that input when there are no references. The
workflow accepts no plaintext secret bindings in committed configuration.
See [external bindings](external-secrets.md#configuration-promotion) and
[saved-plan semantics](#configuration-promotion).

Planning failures fail the job after a review comment is prepared. The comment
contains only actions, conflicts and blockers. The complete review artifact
includes the destination origin, destination digest, external-reference digest,
plan digest and actions,
and is retained for seven days. Download `olp-plan-review.json`, commit it to
the configuration pull request, and review that file together with the desired
configuration. Set `reviewed_plan_path` to its path; apply refuses a missing
review. Neither the desired document nor credential bindings enter comments
or review artifacts.

Applies run only when `apply` is true and the event is a push to the caller's
default branch. The push plan must exactly match the committed review,
including the original destination digest and planned actions. Any intervening
management edit, changed desired state or changed binding requires a new
review. Per-installation workflow concurrency serializes jobs in that
repository, and the server rechecks the destination digest transactionally. An uncertain response should be reconciled
against installation state before rerunning deployment.

## Terraform and OpenTofu

The [OpenLLMProxy provider](https://github.com/tyk-swe/terraform-provider-openllmproxy)
lives in a separate repository. Projects,
providers, credential slots, routes, routing policies, guardrails, export sinks, keys, budget groups,
notification destinations, notification rules and upstream MCP servers have
live create, update, import and destroy qualification with Terraform 1.16.5 and
OpenTofu 1.13.1. All twelve resource types are represented. See
[Qualification](../develop/qualification.md#operator-ecosystem) for remaining
release-candidate checks and the historical evidence boundary.

Connect with `OLP_MANAGEMENT_URL` and `OLP_MANAGEMENT_TOKEN_FILE`. The provider
uses the generated management contract and reads the private token file for
every operation. A token's creator role, project boundary and current scopes
remain authoritative. Project resources require an installation-wide
`manage_projects` token whose creator is an owner. Budget and notification
resources use their ordinary `keys` or installation `settings` requirements;
the provider does not require configuration-promotion permissions.

Each resource manages its own UUID, JSON `definition` and observed ETag.
Creation-only field changes require replacement; updates use the API's declared
mutable fields. Removing a nullable budget ceiling explicitly clears it.
Equivalent fixed-scale monetary values keep the configured representation,
avoiding repeated plans. Import discovers nonsecret creation fields from the
current server response. An external edit after a reviewed plan's observed
ETag refuses apply and requires a fresh review.

Notification signing values use a separate write-only `secret` attribute,
an ephemeral input and a positive `secret_version` change counter. Values are
excluded from saved plans and state. The acceptance suite checks that boundary
with both engines. The provider rejects credential values in the state-bearing
`definition` field.

Destroy is conditional. Dependent resources and retained accounting/delivery
history return a conflict. Delete notification rules before their destination
or budget group, and detach dependent resources before deleting a project.
Successful conditional deletion retains its audit and idempotent replay;
deleting a signing destination removes its stored secret. See
[access administration](../use/access.md) for these API boundaries.

The provider repository documents source builds, development overrides,
required disposable-service environment and its CI acceptance suite. Its OLP
binary and SDK come from the same immutable dependency version. For
installation-wide artifact promotion through GitHub Actions, use the separate
[configuration-promotion workflow](#github-actions).

API keys use `openllmproxy_key` and revoke on destroy, preserving retained
accounting. Their mutable ceiling fields are projected from budget windows,
separately from accrued spend. An optional absolute `secret_file` atomically
captures the one-time key into an owner-only file; state stores its path, not
its value. Changing that path replaces the key. Omit the option when importing
existing metadata, since an existing key's secret cannot be recovered. The
operator removes the local file after revocation. An output failure preserves
the created UUID in state so the authority can be reconciled or destroyed.

Providers manage draft configuration and validate write-only credential rotations
before staging a new version. Normalized configuration defaults are projected to
operator-selected fields. Credential slots use their own ETags and import with
`PROVIDER_UUID/SLOT_UUID`; creation uses the observed pool ETag. Deleting a draft
slot preserves published bindings and immutable versions. Parent deletion follows
only atomic transitions from the same client's slot writes; foreign edits still
refuse a saved destruction plan. See the provider README for the resource forms
and partial-validation recovery behavior.

`openllmproxy_route` publishes validated immutable revisions atomically, preserving
independent console drafts and existing routing policy. Target refresh preserves
chosen facts; UUID import uses canonical provider-model references. Destroy retires
the route and retains history and slug ownership. A project boundary change must
use a new slug; saved plans refuse external publications committed afterward.

Routing-policy resources use `scope` and `scope_id`: installation (nil UUID),
API key, or route draft. Import uses `SCOPE/UUID`. Their definitions replace the
complete policy; removing a constraint returns it to the default. Destroy removes
the explicit override while retaining a fresh ETag. The scoped API's settings,
key or configuration authority remains required. Draft policy changes become
live upon activation. Key/policy graphs coordinate only their own proven parent
writes; a foreign parent or policy edit still invalidates a saved plan.

`openllmproxy_guardrail` manages project-scoped `builtin.regex` definitions,
conditional immutable policy revisions, UUID import and retirement. Compose a
route's `content_policy` from
`jsondecode(openllmproxy_guardrail.filter.definition).policy` to review and
publish a selected copy. Existing serving revisions stay pinned; foreign saved
plans are refused before dependent publication. Retained guardrail history can
prevent project deletion. See [Named guardrails](../use/guardrails.md).

`openllmproxy_sink` manages scoped HTTPS JSON exports of content-free durable
facts. Its UUID import reconstructs nonsecret definitions, and its sensitive
write-only signing input uses a change counter and ephemeral variables. Neither
saved plans nor state contain signing values. Actual installation workers deliver
stable event IDs with exact-byte signatures and bounded retries; delivery counters
do not change resource ETags. Destroy retires the destination and removes signing
material while retaining identity/history. A saved plan refuses external edits.
Project-owned history can prevent project deletion. See [Export sinks](observability.md#managed-export-sinks).

`openllmproxy_mcp_server` conditionally certifies project-owned Streamable HTTP
registrations into immutable, pinned tool catalogs. Its sensitive write-only
bearer uses an ephemeral value and public change counter. UUID imports exclude
authentication material; reads never adopt live upstream drift. Updates repeat
project authority and ETag checks after network certification outside database
locks. Saved plans refuse foreign edits. Destroy removes the bearer and retains
registration/revision history. See [Upstream MCP servers](../use/mcp-servers.md).
