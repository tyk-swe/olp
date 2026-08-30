//! End-to-end contract assertions.
//!
//! Every assertion here is derived from a document — `README.md`, `docs/*.md`,
//! or `openapi/management.json` — and cites it. None is derived from observed
//! behaviour. A failure means the product and its documentation disagree, and
//! the resolution is to fix one of them, never to soften the assertion.
//!
//! Tests are `#[ignore]`d so `make test` and the coverage gate skip them, and
//! they run single-threaded against one shared server; see
//! `scripts/run-e2e-tests.sh`.

// This file is the test target's crate root, so `mod` would resolve its
// submodules as siblings in `tests/`. The paths are given explicitly to keep
// the support modules grouped under `tests/contract/`.
#[allow(dead_code)]
#[path = "contract/durable.rs"]
mod durable;
#[allow(dead_code)]
#[path = "contract/harness.rs"]
mod harness;
#[allow(dead_code)]
#[path = "contract/mock_upstream.rs"]
mod mock_upstream;
#[allow(dead_code)]
#[path = "contract/sse.rs"]
mod sse;
#[allow(dead_code)]
#[path = "contract/world.rs"]
mod world;

// Contract modules are prefixed to preserve the documented single-threaded
// lifecycle order while keeping each product area independently reviewable.
#[path = "contract/public_interfaces.rs"]
mod a_public_interfaces;
#[path = "contract/management_contract.rs"]
mod b_management_contract;
#[path = "contract/provider_lifecycle.rs"]
mod c_provider_lifecycle;
#[path = "contract/management_failures.rs"]
mod d_management_failures;
#[path = "contract/gateway_surfaces.rs"]
mod e_gateway_surfaces;
#[path = "contract/data_safety.rs"]
mod f_data_safety;
#[path = "contract/telemetry.rs"]
mod g_telemetry;
#[path = "contract/distributed_limits.rs"]
mod h_distributed_limits;
#[path = "contract/readme_examples.rs"]
mod i_readme_examples;

use std::fs;
use std::path::{Path, PathBuf};
use std::sync::OnceLock;

use rand::RngCore as _;
use serde_json::{Value, json};
use tokio::runtime::Runtime;

use world::World;

fn runtime() -> &'static Runtime {
    static RUNTIME: OnceLock<Runtime> = OnceLock::new();
    RUNTIME.get_or_init(|| {
        tokio::runtime::Builder::new_multi_thread()
            .enable_all()
            .build()
            .expect("test runtime builds")
    })
}

/// The shared installation, built once on first use. A bootstrap failure fails
/// every test that needs it, carrying the underlying reason.
fn world() -> &'static World {
    static WORLD: OnceLock<Result<World, String>> = OnceLock::new();
    WORLD
        .get_or_init(|| {
            // Tests reach this from inside `runtime().block_on`, and a runtime
            // cannot be entered from within itself. Bootstrapping on its own
            // thread keeps the blocking call outside the runtime context while
            // still driving the future on the shared runtime.
            std::thread::spawn(|| runtime().block_on(world::bootstrap()))
                .join()
                .unwrap_or_else(|_| Err("bootstrap thread panicked".to_owned()))
        })
        .as_ref()
        .unwrap_or_else(|error| panic!("bootstrap failed: {error}"))
}

/// tests/e2e -> tests -> repository root.
fn repo_root() -> PathBuf {
    Path::new(env!("CARGO_MANIFEST_DIR"))
        .parent()
        .and_then(Path::parent)
        .expect("crate sits two levels below the repository root")
        .to_path_buf()
}

/// Fetches a path on the public origin with no credential.
async fn public_get(path: &str) -> (u16, String) {
    let world = world();
    let response = world
        .http
        .get(format!("{}{path}", world.origin()))
        .send()
        .await
        .unwrap_or_else(|error| panic!("GET {path} failed: {error}"));
    let status = response.status().as_u16();
    let body = response.text().await.unwrap_or_default();
    (status, body)
}

/// Narrows a request-log query to the inference route the telemetry
/// assertions drive, excluding the model-listing call every newly issued key
/// makes on its way to being served.
fn route_filter() -> String {
    format!("&route={}", world::OPENAI_ROUTE)
}

/// A usage window wide enough to hold the whole run and narrow enough for the
/// documented bound.
///
/// The usage endpoints reject a range longer than 366 days, so "since the epoch"
/// is not available; an hour either side of now covers a test run with room to
/// spare and keeps the window independent of the machine's clock offset.
fn usage_window() -> (String, String) {
    let now = chrono::Utc::now();
    let format =
        |moment: chrono::DateTime<chrono::Utc>| moment.format("%Y-%m-%dT%H:%M:%SZ").to_string();
    (
        format(now - chrono::Duration::hours(1)),
        format(now + chrono::Duration::hours(1)),
    )
}

/// A prompt fragment unique to one assertion.
///
/// Data-safety assertions need a needle that cannot collide with anything the
/// installation legitimately stores, and telemetry assertions need to tell
/// their own traffic apart from every other test's.
fn nonce(label: &str) -> String {
    let mut bytes = [0_u8; 8];
    rand::rng().fill_bytes(&mut bytes);
    let hex: String = bytes.iter().map(|byte| format!("{byte:02x}")).collect();
    format!("olp-e2e-{label}-{hex}")
}

/// Asserts the RFC 9457 shape every documented management error declares.
///
/// `openapi/management.json` gives every 4xx and 5xx management response the
/// media type `application/problem+json` and the `Problem` schema, whose
/// required members are `type`, `title`, `status` and `detail`.
fn assert_problem(what: &str, status: u16, response: &world::MgmtResponse) {
    assert_eq!(
        response.status, status,
        "{what} returned {} instead of {status}: {}",
        response.status, response.body
    );
    let content_type = response.header("content-type").unwrap_or_default();
    assert!(
        content_type.starts_with("application/problem+json"),
        "{what} answered {status} with content-type {content_type:?}; \
         openapi/management.json declares application/problem+json"
    );
    for member in ["type", "title", "status", "detail"] {
        assert!(
            response.body.get(member).is_some(),
            "{what} answered {status} without the required Problem member \
             {member:?}: {}",
            response.body
        );
    }
    assert_eq!(
        response.body["status"],
        json!(status),
        "{what} answered HTTP {status} but its problem reports a different \
         status: {}",
        response.body
    );
}

async fn served_openapi() -> Value {
    let (status, body) = public_get("/api/v1/openapi.json").await;
    assert_eq!(status, 200, "GET /api/v1/openapi.json returned {status}");
    serde_json::from_str(&body).expect("served OpenAPI document must be valid JSON")
}

// ---------------------------------------------------------------------------
// Teardown
//
// libtest runs tests in name order under --test-threads=1, so this root-level
// z-prefixed test follows every prefixed contract module and releases the
// per-run database and temporary directory.
// ---------------------------------------------------------------------------

#[test]
#[ignore = "end-to-end; run via make e2e"]
fn zzz_teardown_releases_the_run_database() {
    runtime().block_on(async {
        let stderr = world().shutdown().await;
        assert!(
            !stderr.contains("panicked at"),
            "the server logged a panic during the run:\n{stderr}"
        );
    });
}
