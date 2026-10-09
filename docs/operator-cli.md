# Management CLI and MCP

The `olp` binary includes management clients as well as the server. Client
commands use the public management API; they do not load the server's database
URLs, bootstrap token or encryption keys. Commands and MCP tool schemas are
generated from `openapi/management.json` by `make api`.

## Connect the CLI

Create a [management token](access.md#management-tokens-and-provisioning) with
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
`olp api help` lists every machine-admitted JSON operation. Use an operation ID
to avoid ambiguity in nested resource names:

```sh
olp keys get KEY_ID
olp keys update KEY_ID --body-file key.json --if-match OBSERVED_ETAG
olp providers activate PROVIDER_ID --if-match OBSERVED_ETAG \
  --idempotency-key DEPLOYMENT_KEY
olp api put_credential_slot PROVIDER_ID SLOT_NAME \
  --body-file slot.json --if-match OBSERVED_ETAG
olp api simulate_route_draft ROUTE_ID --body-file request.json
```

Path values are positional in contract order, or `--path NAME=VALUE`. Queries
use repeatable `--query NAME=VALUE`. JSON requests use `--body-file FILE`;
output goes to stdout or `--output FILE`. Output files are replaced atomically
with owner-only permissions. The server validates schemas and project access
just as it does for the console.

Reads print an `ETag:` line to stderr when the API supplies one. Conditional
writes require that observed ETag with `--if-match`; the CLI never fetches a
new ETag to force a stale write through. When the contract requires an
idempotency key, the client generates one unless `--idempotency-key` supplies
it. **For retries after an uncertain response, supply the same explicit key.**
There are no automatic mutation retries.

Key creation and rotation return their one-time secret, matching the API.
Keep that output out of CI logs, or use `--output` and store the file securely.
Error messages exclude server-controlled request details.

### Provider and credential-slot lifecycle

`olp api delete_provider PROVIDER_ID --if-match ETAG` removes an unused draft
provider. Published revisions, route drafts, provider-specific price history,
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

The saved plan records the destination origin and its current export digest,
the secret-free desired document, and actions, conflicts and blockers. Planning
saves it even when blockers or conflicts cause a nonzero exit. Apply refuses
such plans, a different destination, or a changed plan. The API checks the
original destination digest again in its apply transaction, so a change
between re-planning and applying also refuses the write. Resolve the change
and compute a new plan instead of silently replacing the destination.

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
permission settings. See [qualified clients](clients.md) for pinned releases
and qualification boundaries.

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
Session-only operations, raw uploads and MCP itself are not tools. Each call
uses the API's authentication, scoped project access, validation, ETag check,
idempotency and audit handler. MCP requires an explicit idempotency key wherever
the API does; it does not invent one.

Results include API status, non-secret body and ETag in `structuredContent` and
text content. One-time keys, tokens, passwords and secret bindings are removed
recursively. Failed responses omit details that could echo secret input.
A key created through MCP therefore has **no retrievable secret**; use the CLI
or console when you need to capture a new key. Token revocation and changes
to the creator's authority apply to both transports.

Slot PUT/DELETE responses include `OLP-Previous-Parent-ETag` and `OLP-Parent-ETag`: the exact provider transition committed by that request. Idempotent replay preserves those original values. Clients coordinating parent and child resources can follow a contiguous chain of their own writes; a missing transition remains a conflict and never authorizes adopting an unrelated edit.
