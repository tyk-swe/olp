//! Emits the management API OpenAPI document to stdout. `make api` and
//! `make build` redirect it into `openapi/management.json` and regenerate
//! the console's TypeScript contract. Both generated files are ignored outputs.

use olp::http::control::openapi::document;

fn main() {
    println!(
        "{}",
        serde_json::to_string_pretty(&document()).expect("OpenAPI document must serialize")
    );
}
