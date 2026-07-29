# Known defects

Defects confirmed by the end-to-end contract suite (`make e2e`). Each entry
names the documented clause it violates, a repro, and what the product does
instead.

The suite is deliberately hard-red: there is no expected-failure manifest, so
every entry here corresponds to a currently failing test. Removing an entry
means the defect is fixed and its test passes.

## Incomplete usage is recorded as priced

**Failing test:** `missing_upstream_usage_is_incomplete_and_unpriced_never_zero`

**Violated clause:** `docs/architecture.md`, "Data-safety invariants" —
"Missing upstream usage is incomplete and unpriced, never zero."

**Repro:** with a pricing revision in force for the provider kind, model and
operation, make a completion whose upstream response carries no `usage` object,
then read the request back:

```
curl -s "$OLP/api/v1/requests?limit=1" | jq '.data[0] | {usage_complete, unpriced, estimated_cost}'
```

**Expected:** `usage_complete: false`, `unpriced: true`, `estimated_cost: null`.

**Actual:** `usage_complete: false`, **`unpriced: false`**, `estimated_cost:
null`. The record claims to be priced while carrying no price. Usage reports
therefore count it toward priced traffic, and `unpriced_count` understates how
much spend is unaccounted for.

**Mechanism:** `crates/storage/src/request_metadata/ingestion.rs:318-356`
derives `pricing_complete` as `pricing_revision_id IS NOT NULL AND ($5::bigint
IS NULL OR input_per_million IS NOT NULL) AND …`. When usage is missing, the
token parameters are `NULL`, so each per-dimension clause is vacuously true and
the expression reduces to "a pricing revision exists". The `estimated_cost`
expression immediately below it *does* guard on `$8::boolean`
(`event.usage_complete`), which is why the cost is correctly `NULL`; the
`pricing_complete` expression is missing that same guard, so the two disagree
about the same row.

**Note:** `RequestMetadataEvent.unpriced` at
`apps/olp/src/gateway/telemetry.rs:508` is hard-coded `true` and never read —
ingestion recomputes the value. That field is dead rather than wrong, but it
should not survive the fix.

**Why existing tests miss it:** an installation with no pricing revision prices
nothing, so `unpriced` is `true` for every request and the invariant holds
vacuously. The E2E fixture configures pricing during bootstrap
(`tests/e2e/tests/contract/world.rs`, `configure_pricing`) precisely so that
assertions about `unpriced` can fail, and `a_request_covered_by_a_pricing_revision_is_priced`
is the control proving the priced path still works.

## The management OpenAPI document omits its own endpoint

**Failing test:** `the_openapi_endpoint_documents_itself`

**Violated clause:** `README.md`, "Interfaces" — "Management OpenAPI
`/api/v1/openapi.json`". The OpenAPI document is the management API's
published contract, so a path the server answers must appear in it.

**Repro:**

```
curl -s http://localhost:8080/api/v1/openapi.json | jq '.paths | has("/api/v1/openapi.json")'
```

**Expected:** `true`.

**Actual:** `false`. The server answers the request with 200 and a document
listing 75 paths, none of which is `/api/v1/openapi.json` itself.

**Why existing tests miss it:** `apps/olp/tests/integration/openapi_drift.rs`
compares the generated document against the checked-in
`openapi/management.json`. Both are produced from the same `utoipa`
registration, so an endpoint registered on the axum router but never annotated
is absent from both sides and the comparison still succeeds. Only a test that
issues a real HTTP request and reconciles it against the served document can
see the gap.

**Registration site:** `apps/olp/src/management_api.rs:37`
(`.route("/api/v1/openapi.json", get(openapi))`).
