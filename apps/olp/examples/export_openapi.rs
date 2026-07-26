//! Build tooling, not a usage example: emits the management API OpenAPI
//! document to stdout. `make openapi` redirects it into
//! `openapi/management.json` and regenerates the console schema;
//! `apps/olp/tests/openapi_drift.rs` fails CI when the committed copy is
//! stale.

use olp::management_openapi;

fn main() {
    println!(
        "{}",
        serde_json::to_string_pretty(&management_openapi())
            .expect("OpenAPI document must serialize")
    );
}
