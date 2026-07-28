# Known defects

Defects confirmed by the end-to-end contract suite (`make e2e`). Each entry
names the documented clause it violates, a repro, and what the product does
instead.

The suite is deliberately hard-red: there is no expected-failure manifest, so
every entry here corresponds to a currently failing test. Removing an entry
means the defect is fixed and its test passes.

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
