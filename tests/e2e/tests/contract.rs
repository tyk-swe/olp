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

use std::fs;
use std::path::{Path, PathBuf};
use std::sync::OnceLock;

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

async fn served_openapi() -> Value {
    let (status, body) = public_get("/api/v1/openapi.json").await;
    assert_eq!(status, 200, "GET /api/v1/openapi.json returned {status}");
    serde_json::from_str(&body).expect("served OpenAPI document must be valid JSON")
}

// ---------------------------------------------------------------------------
// Public interface surface
//
// README.md "Interfaces": all public interfaces share one origin — console at
// `/`, management at `/api/v1`, OpenAI at `/openai/v1`, Anthropic at
// `/anthropic/v1`, Gemini at `/gemini/v1` *and* `/gemini/v1beta`.
// ---------------------------------------------------------------------------

#[test]
#[ignore = "end-to-end; run via make e2e"]
fn observability_endpoints_are_not_public() {
    // README.md "Interfaces": liveness, readiness and metrics are served on
    // OLP_OBSERVABILITY_LISTEN_ADDR, and "public requests for these paths
    // return 404".
    runtime().block_on(async {
        for path in ["/health/live", "/health/ready", "/metrics"] {
            let (status, body) = public_get(path).await;
            assert_eq!(
                status, 404,
                "{path} must return 404 on the public origin; got {status}: {body}"
            );
        }
    });
}

#[test]
#[ignore = "end-to-end; run via make e2e"]
fn observability_endpoints_answer_on_their_own_listener() {
    // The same clause requires these to exist on the observability listener,
    // so the 404 above proves routing rather than absence.
    runtime().block_on(async {
        let world = world();
        for path in ["/health/live", "/health/ready", "/metrics"] {
            let response = world
                .http
                .get(format!("{}{path}", world.observability_base))
                .send()
                .await
                .unwrap_or_else(|error| panic!("observability GET {path} failed: {error}"));
            // Not 2xx: readiness legitimately reports 503 while a dependency
            // is unavailable, and this test only distinguishes "served here"
            // from "not served at all". Whether readiness *converges* is a
            // separate claim that deserves its own test and its own wait.
            assert_ne!(
                response.status().as_u16(),
                404,
                "{path} must be served on the observability listener"
            );
        }
    });
}

#[test]
#[ignore = "end-to-end; run via make e2e"]
fn gemini_is_reachable_on_both_documented_prefixes() {
    // README.md "Interfaces" documents Gemini-compatible APIs at BOTH
    // /gemini/v1 and /gemini/v1beta. Neither may be unrouted.
    runtime().block_on(async {
        let world = world();
        for prefix in ["/gemini/v1", "/gemini/v1beta"] {
            let path = format!("{prefix}/models");
            let response = world
                .http
                .get(format!("{}{path}", world.origin()))
                .header("x-goog-api-key", &world.api_key)
                .send()
                .await
                .unwrap_or_else(|error| panic!("GET {path} failed: {error}"));
            assert_ne!(
                response.status().as_u16(),
                404,
                "{path} is documented in README.md but is not routed"
            );
        }
    });
}

// ---------------------------------------------------------------------------
// Management API contract
// ---------------------------------------------------------------------------

#[test]
#[ignore = "end-to-end; run via make e2e"]
fn served_openapi_matches_the_tracked_document() {
    // README.md "Interfaces": the management OpenAPI is at
    // /api/v1/openapi.json "with the tracked schema at
    // openapi/management.json". AGENTS.md makes the tracked file a generated,
    // gated artefact, so the two must agree exactly.
    runtime().block_on(async {
        let served = served_openapi().await;
        let tracked_path = repo_root().join("openapi/management.json");
        let tracked: Value = serde_json::from_str(
            &fs::read_to_string(&tracked_path).expect("openapi/management.json is readable"),
        )
        .expect("openapi/management.json is valid JSON");

        assert_eq!(
            served, tracked,
            "the served OpenAPI document differs from openapi/management.json"
        );
    });
}

#[test]
#[ignore = "end-to-end; run via make e2e"]
fn the_openapi_endpoint_documents_itself() {
    // The OpenAPI document is the management API's published contract, so a
    // path the server answers but the document omits is undocumented surface.
    // apps/olp/tests/integration/openapi_drift.rs compares the generated
    // document to the checked-in one and so cannot see this.
    runtime().block_on(async {
        let served = served_openapi().await;
        let paths = served["paths"]
            .as_object()
            .expect("OpenAPI document has a paths object");

        assert!(
            paths.contains_key("/api/v1/openapi.json"),
            "the server answers GET /api/v1/openapi.json, but the document does \
             not list it among its {} paths",
            paths.len()
        );
    });
}

#[test]
#[ignore = "end-to-end; run via make e2e"]
fn setup_cannot_be_replayed_once_an_owner_exists() {
    // README.md "Quick start": the bootstrap token is one-time and the owner is
    // created once, after which the token is retired. A second setup attempt
    // must be refused, or an installation could be re-owned.
    runtime().block_on(async {
        let world = world();
        let response = world
            .http
            .post(format!("{}/api/v1/setup", world.origin()))
            .header("x-olp-setup-token", &world.setup_token)
            .header(reqwest::header::ORIGIN, world.origin())
            .json(&json!({
                "email": "intruder@e2e.test",
                "password": "correct horse battery staple",
                "display_name": "Intruder",
                "installation_name": "Replayed"
            }))
            .send()
            .await
            .expect("replayed setup request");
        let status = response.status().as_u16();
        assert_ne!(
            status, 201,
            "setup was accepted a second time; the installation can be re-owned"
        );
        assert!(
            (400..500).contains(&status),
            "a replayed setup must be refused with a 4xx; got {status}"
        );
    });
}

// ---------------------------------------------------------------------------
// Provider lifecycle
// ---------------------------------------------------------------------------

#[test]
#[ignore = "end-to-end; run via make e2e"]
fn certification_accepts_a_provider_whose_probe_is_current() {
    // Certification is gated on a probe no older than the draft's last change.
    // A provider probed *after* its final modification therefore satisfies the
    // gate, and certification must not demand a further probe.
    // `world::bootstrap` re-probes to get past this; here the documented flow
    // is exercised on its own so a defect fails one named test.
    runtime().block_on(async {
        let world = world();
        let management = &world.management;

        let key = management.next_idempotency_key();
        let created = management
            .expect(
                reqwest::Method::POST,
                "/api/v1/providers",
                Some(json!({
                    "name": "e2e-probe-freshness",
                    "kind": "openai_compatible",
                    "endpoint": format!("{}/v1/", world.mock.base),
                    "auth_mode": "api_key",
                    "credential": mock_upstream::COMPAT_CREDENTIAL
                })),
                Some(&key),
                None,
                201,
            )
            .await
            .expect("provider create");
        let provider_id = created.body["id"].as_str().expect("provider id").to_owned();
        let mut etag = created.require_etag("create").expect("create ETag");

        let discovery = management
            .expect(
                reqwest::Method::POST,
                &format!("/api/v1/providers/{provider_id}/discovery"),
                Some(json!({"mode": "live"})),
                None,
                Some(&etag),
                200,
            )
            .await
            .expect("discovery");
        etag = discovery.require_etag("discovery").expect("discovery ETag");

        let model_row = world::resolve_model_row(management, &provider_id, mock_upstream::MODEL)
            .await
            .expect("discovery surfaces the mock model");

        let reviewed = management
            .expect(
                reqwest::Method::PATCH,
                &format!("/api/v1/providers/{provider_id}/models/{model_row}"),
                Some(json!({
                    "enabled": true,
                    "capabilities": [
                        {"operation": "generation", "surface": "openai", "mode": "unary"}
                    ]
                })),
                None,
                Some(&etag),
                200,
            )
            .await
            .expect("capability review");
        etag = reviewed.require_etag("review").expect("review ETag");

        // A single probe, taken after the last modification.
        let probe = management
            .expect(
                reqwest::Method::POST,
                &format!("/api/v1/providers/{provider_id}/probe"),
                None,
                None,
                Some(&etag),
                200,
            )
            .await
            .expect("probe");
        etag = probe.etag().unwrap_or(etag);

        let certify = management
            .send(
                reqwest::Method::POST,
                &format!("/api/v1/providers/{provider_id}/models/{model_row}/certify"),
                None,
                None,
                Some(&etag),
            )
            .await
            .expect("certify request");

        assert_eq!(
            certify.status, 200,
            "certification rejected a provider probed after its last change: {}",
            certify.body
        );
    });
}

// ---------------------------------------------------------------------------
// Teardown
//
// libtest runs tests in name order under --test-threads=1, so this runs last
// and releases the per-run database and temporary directory.
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
