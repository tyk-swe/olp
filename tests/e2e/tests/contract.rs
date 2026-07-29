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
const ROUTE_FILTER: &str = "&route=e2e-openai";

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
// Documented failure modes
//
// openapi/management.json declares 401, 403, 409, 412 and 422 responses across
// the management API, all of them `application/problem+json`. A surface that
// answers the happy path correctly and improvises on failure is undocumented
// where it matters most.
// ---------------------------------------------------------------------------

#[test]
#[ignore = "end-to-end; run via make e2e"]
fn management_requires_a_session() {
    runtime().block_on(async {
        let management = &world().management;
        let response = management
            .raw(reqwest::Method::GET, "/api/v1/providers", None, &[])
            .await
            .expect("unauthenticated provider listing");
        assert_problem("GET /api/v1/providers without a session", 401, &response);
    });
}

#[test]
#[ignore = "end-to-end; run via make e2e"]
fn management_mutations_require_a_csrf_token() {
    // A session cookie alone must not authorise a mutation, or any site the
    // operator visits could drive the management API with their session.
    runtime().block_on(async {
        let management = &world().management;
        let response = management
            .raw(
                reqwest::Method::POST,
                "/api/v1/api-keys",
                Some(json!({"name": "csrf probe", "scopes": ["inference"]})),
                &[
                    (reqwest::header::COOKIE.as_str(), management.cookie()),
                    (reqwest::header::ORIGIN.as_str(), management.origin()),
                    ("idempotency-key", "e2e-csrf-probe"),
                ],
            )
            .await
            .expect("CSRF-less mutation");
        assert_problem("POST /api/v1/api-keys without a CSRF token", 403, &response);
    });
}

#[test]
#[ignore = "end-to-end; run via make e2e"]
fn management_mutations_reject_a_foreign_origin() {
    runtime().block_on(async {
        let management = &world().management;
        let response = management
            .raw(
                reqwest::Method::POST,
                "/api/v1/api-keys",
                Some(json!({"name": "origin probe", "scopes": ["inference"]})),
                &[
                    (reqwest::header::COOKIE.as_str(), management.cookie()),
                    ("x-csrf-token", management.csrf()),
                    (reqwest::header::ORIGIN.as_str(), "https://evil.example"),
                    ("idempotency-key", "e2e-origin-probe"),
                ],
            )
            .await
            .expect("foreign-origin mutation");
        assert_problem(
            "POST /api/v1/api-keys from a foreign Origin",
            403,
            &response,
        );
    });
}

#[test]
#[ignore = "end-to-end; run via make e2e"]
fn a_stale_if_match_is_refused_with_412() {
    // docs/architecture.md "Runtime publication" makes provider edits
    // ETag-bound; openapi/management.json declares the 412 that enforces it.
    runtime().block_on(async {
        let world = world();
        let stale = "\"00000000-0000-0000-0000-000000000000\"";
        // A complete, valid body: the point of the assertion is the
        // precondition, so nothing else about the request may be wrong.
        let response = world
            .management
            .send(
                reqwest::Method::PATCH,
                &format!("/api/v1/providers/{}", world.compat_provider),
                Some(json!({
                    "name": "renamed by a stale writer",
                    "auth_mode": "api_key",
                    "endpoint": format!("{}/v1/", world.mock.base)
                })),
                None,
                Some(stale),
            )
            .await
            .expect("stale If-Match update");
        assert_problem(
            "PATCH /api/v1/providers/{id} with a stale ETag",
            412,
            &response,
        );
    });
}

#[test]
#[ignore = "end-to-end; run via make e2e"]
fn replaying_an_idempotency_key_with_a_different_body_is_refused() {
    // Every mutation carries an Idempotency-Key and openapi/management.json
    // declares a 409 for the conflict. Reusing one key for two different
    // bodies must not silently create two keys, nor silently return the first.
    runtime().block_on(async {
        let management = &world().management;
        let key = format!("e2e-replay-{}", nonce("idem"));

        let first = management
            .expect(
                reqwest::Method::POST,
                "/api/v1/api-keys",
                Some(json!({"name": "idempotency probe one", "scopes": ["inference"]})),
                Some(&key),
                None,
                201,
            )
            .await
            .expect("first idempotent create");
        let first_id = first.body["id"].as_str().unwrap_or_default().to_owned();

        let second = management
            .send(
                reqwest::Method::POST,
                "/api/v1/api-keys",
                Some(json!({"name": "idempotency probe two", "scopes": ["inference"]})),
                Some(&key),
                None,
            )
            .await
            .expect("replayed idempotent create");

        assert_problem(
            "POST /api/v1/api-keys replaying an Idempotency-Key with a new body",
            409,
            &second,
        );

        // The refusal must also be a no-op: the second body must not have
        // created a key, and the first must be untouched.
        let listing = world()
            .management
            .get("/api/v1/api-keys?limit=100")
            .await
            .expect("api key listing");
        assert_eq!(
            listing.status, 200,
            "GET /api/v1/api-keys returned {}: {}",
            listing.status, listing.body
        );
        // ApiKeyListResponse names its page `items`.
        let names: Vec<&str> = listing.body["items"]
            .as_array()
            .map(|rows| rows.iter().filter_map(|row| row["name"].as_str()).collect())
            .unwrap_or_default();
        assert!(
            names.contains(&"idempotency probe one"),
            "the first key of a refused replay must survive; the listing held \
             {names:?}: {}",
            listing.body
        );
        assert!(
            !names.contains(&"idempotency probe two"),
            "the refused replay created a key anyway: {names:?}"
        );
        assert!(
            !first_id.is_empty(),
            "the first create returned no id: {}",
            first.body
        );
    });
}

#[test]
#[ignore = "end-to-end; run via make e2e"]
fn an_invalid_provider_draft_is_refused_with_a_field_report() {
    // openapi/management.json documents 422 "Validation failed" for provider
    // creation, and the Problem schema carries an `errors` map. A 422 with no
    // field report tells an operator nothing about which field to fix.
    runtime().block_on(async {
        let response = world()
            .management
            .send(
                reqwest::Method::POST,
                "/api/v1/providers",
                Some(json!({
                    "name": "",
                    "kind": "openai_compatible",
                    "endpoint": "not-a-url",
                    "auth_mode": "api_key",
                    "credential": ""
                })),
                None,
                None,
            )
            .await
            .expect("invalid provider create");
        assert_problem(
            "POST /api/v1/providers with an invalid draft",
            422,
            &response,
        );
        let errors = response.body.get("errors").and_then(Value::as_object);
        assert!(
            errors.is_some_and(|errors| !errors.is_empty()),
            "the 422 carries no populated `errors` map, so no field is named: {}",
            response.body
        );
    });
}

// ---------------------------------------------------------------------------
// Gateway journey
//
// README.md "Interfaces" lists three client surfaces on one origin;
// docs/architecture.md "Canonical endpoint and provider policy" binds each to
// a typed operation, so a request on any surface must reach the same upstream
// provider and come back in that surface's own dialect.
// ---------------------------------------------------------------------------

#[test]
#[ignore = "end-to-end; run via make e2e"]
fn the_openai_surface_answers_and_translates_upstream() {
    runtime().block_on(async {
        let world = world();
        let checkpoint = world.mock.checkpoint();
        let prompt = nonce("openai-unary");

        let response = world
            .gateway_post(
                "/openai/v1/chat/completions",
                json!({
                    "model": world::OPENAI_ROUTE,
                    "messages": [{"role": "user", "content": prompt}]
                }),
                &world.api_key,
            )
            .await
            .expect("chat completion");
        assert_eq!(
            response.status, 200,
            "POST /openai/v1/chat/completions returned {}: {}",
            response.status, response.text
        );

        let body = response.json();
        assert_eq!(
            body["choices"][0]["message"]["content"],
            json!(mock_upstream::PLAIN_TEXT),
            "the upstream reply did not reach the client unchanged: {body}"
        );
        assert_eq!(body["choices"][0]["finish_reason"], json!("stop"));
        assert_eq!(
            body["usage"]["prompt_tokens"],
            json!(mock_upstream::PROMPT_TOKENS)
        );
        assert_eq!(
            body["usage"]["completion_tokens"],
            json!(mock_upstream::COMPLETION_TOKENS)
        );

        let upstream = world.mock.since(checkpoint);
        assert_eq!(
            upstream.len(),
            1,
            "one client request produced {} upstream calls: {upstream:#?}",
            upstream.len()
        );
        let call = &upstream[0];
        assert_eq!(call.method, "POST");
        assert_eq!(call.path, "/v1/chat/completions");
        assert_eq!(
            call.body["model"],
            json!(mock_upstream::MODEL),
            "the gateway sent the route slug upstream instead of the \
             provider's own model name: {}",
            call.body
        );
        assert_eq!(
            call.authorization.as_deref(),
            Some(format!("Bearer {}", mock_upstream::COMPAT_CREDENTIAL).as_str()),
            "the provider credential did not reach the upstream unchanged"
        );
        assert!(
            !call
                .headers
                .iter()
                .any(|(_, value)| value.contains(&world.api_key)),
            "the client's own API key was forwarded upstream: {:#?}",
            call.headers
        );
    });
}

#[test]
#[ignore = "end-to-end; run via make e2e"]
fn the_anthropic_surface_answers_in_the_anthropic_dialect() {
    // README.md "Interfaces": an Anthropic-compatible API at /anthropic/v1. A
    // client of that API reads `content[].text`, `stop_reason` and
    // `usage.input_tokens`; answering in another dialect is not compatibility.
    runtime().block_on(async {
        let world = world();
        let checkpoint = world.mock.checkpoint();
        let prompt = nonce("anthropic-unary");

        let response = world
            .gateway_send(
                reqwest::Method::POST,
                "/anthropic/v1/messages",
                Some(json!({
                    "model": world::CROSS_ROUTE,
                    "max_tokens": 64,
                    "messages": [{"role": "user", "content": prompt}]
                })),
                &[
                    ("x-api-key", &world.api_key),
                    ("anthropic-version", "2023-06-01"),
                ],
            )
            .await
            .expect("anthropic message");
        assert_eq!(
            response.status, 200,
            "POST /anthropic/v1/messages returned {}: {}",
            response.status, response.text
        );

        let body = response.json();
        assert_eq!(body["type"], json!("message"));
        assert_eq!(body["role"], json!("assistant"));
        assert_eq!(
            body["content"][0]["text"],
            json!(mock_upstream::PLAIN_TEXT),
            "the reply did not reach the Anthropic client: {body}"
        );
        assert_eq!(
            body["stop_reason"],
            json!("end_turn"),
            "an ordinary completion must stop with `end_turn` in the Anthropic \
             dialect: {body}"
        );
        assert_eq!(
            body["usage"]["input_tokens"],
            json!(mock_upstream::PROMPT_TOKENS)
        );
        assert_eq!(
            body["usage"]["output_tokens"],
            json!(mock_upstream::COMPLETION_TOKENS)
        );

        let upstream = world.mock.since(checkpoint);
        assert_eq!(
            upstream.len(),
            1,
            "one client request produced {} upstream calls: {upstream:#?}",
            upstream.len()
        );
        assert_eq!(
            upstream[0].api_key_header.as_deref(),
            Some(mock_upstream::AZURE_CREDENTIAL),
            "the Azure provider credential is sent in the `api-key` header"
        );
    });
}

#[test]
#[ignore = "end-to-end; run via make e2e"]
fn the_gemini_surface_answers_in_the_gemini_dialect() {
    runtime().block_on(async {
        let world = world();
        let checkpoint = world.mock.checkpoint();
        let prompt = nonce("gemini-unary");

        let response = world
            .gateway_send(
                reqwest::Method::POST,
                &format!(
                    "/gemini/v1beta/models/{}:generateContent",
                    world::CROSS_ROUTE
                ),
                Some(json!({
                    "contents": [{"role": "user", "parts": [{"text": prompt}]}]
                })),
                &[("x-goog-api-key", &world.api_key)],
            )
            .await
            .expect("gemini generateContent");
        assert_eq!(
            response.status,
            200,
            "POST /gemini/v1beta/models/{}:generateContent returned {}: {}",
            world::CROSS_ROUTE,
            response.status,
            response.text
        );

        let body = response.json();
        assert_eq!(
            body["candidates"][0]["content"]["parts"][0]["text"],
            json!(mock_upstream::PLAIN_TEXT),
            "the reply did not reach the Gemini client: {body}"
        );
        assert_eq!(body["candidates"][0]["finishReason"], json!("STOP"));
        assert_eq!(
            body["usageMetadata"]["promptTokenCount"],
            json!(mock_upstream::PROMPT_TOKENS)
        );
        assert_eq!(
            body["usageMetadata"]["candidatesTokenCount"],
            json!(mock_upstream::COMPLETION_TOKENS)
        );
        assert_eq!(
            body["usageMetadata"]["totalTokenCount"],
            json!(mock_upstream::TOTAL_TOKENS)
        );

        assert_eq!(
            world.mock.since(checkpoint).len(),
            1,
            "one client request must produce exactly one upstream call"
        );
    });
}

#[test]
#[ignore = "end-to-end; run via make e2e"]
fn a_streamed_completion_ends_once_and_carries_the_whole_reply() {
    // docs/architecture.md "Runtime publication": a stream cannot cross a
    // generation, so one client stream is one upstream call. The event stream
    // itself is decoded with the independent WHATWG decoder in
    // `contract/sse.rs`, so a product decoder bug cannot mask a product
    // encoder bug.
    runtime().block_on(async {
        let world = world();
        let checkpoint = world.mock.checkpoint();
        let prompt = nonce("openai-stream");

        let response = world
            .gateway_post(
                "/openai/v1/chat/completions",
                json!({
                    "model": world::OPENAI_ROUTE,
                    "stream": true,
                    "messages": [{"role": "user", "content": prompt}]
                }),
                &world.api_key,
            )
            .await
            .expect("streamed chat completion");
        assert_eq!(
            response.status, 200,
            "a streaming request returned {}: {}",
            response.status, response.text
        );
        let content_type = response.header("content-type").unwrap_or_default();
        assert!(
            content_type.starts_with("text/event-stream"),
            "a streaming response must be text/event-stream; got {content_type:?}"
        );

        let stream = sse::decode(response.text.as_bytes()).expect("stream decodes");
        assert!(
            stream.undispatched_tail.is_empty(),
            "the stream ended mid-event, leaving {:?} undispatched",
            stream.undispatched_tail
        );
        let data: Vec<&str> = stream
            .events
            .iter()
            .map(|event| event.data.as_str())
            .collect();
        assert_eq!(
            data.last(),
            Some(&"[DONE]"),
            "an OpenAI-compatible stream ends with the [DONE] sentinel: {data:?}"
        );

        let chunks: Vec<Value> = data[..data.len() - 1]
            .iter()
            .map(|payload| {
                serde_json::from_str(payload)
                    .unwrap_or_else(|error| panic!("chunk {payload:?} is not JSON: {error}"))
            })
            .collect();
        assert!(!chunks.is_empty(), "the stream carried no chunks");

        let finishes: Vec<&Value> = chunks
            .iter()
            .map(|chunk| &chunk["choices"][0]["finish_reason"])
            .filter(|reason| !reason.is_null())
            .collect();
        assert_eq!(
            finishes.len(),
            1,
            "a stream must terminate exactly once; saw {finishes:?}"
        );
        assert_eq!(*finishes[0], json!("stop"));

        let text: String = chunks
            .iter()
            .filter_map(|chunk| chunk["choices"][0]["delta"]["content"].as_str())
            .collect();
        assert_eq!(
            text,
            mock_upstream::PLAIN_TEXT,
            "the concatenated deltas do not reconstruct the upstream reply"
        );

        let ids: Vec<&str> = chunks
            .iter()
            .filter_map(|chunk| chunk["id"].as_str())
            .collect();
        assert!(
            ids.windows(2).all(|pair| pair[0] == pair[1]),
            "chunk ids are not stable across one stream: {ids:?}"
        );

        assert_eq!(
            world.mock.since(checkpoint).len(),
            1,
            "one client stream must produce exactly one upstream call"
        );
    });
}

// ---------------------------------------------------------------------------
// Data-safety invariants
//
// docs/architecture.md "Data-safety invariants": durable request, attempt and
// usage records hold "only identifiers, timing, token or media units, status,
// error classification, and pricing provenance — never prompts, responses,
// reasoning, tool arguments or results, uploads, raw headers, or credentials".
// ---------------------------------------------------------------------------

#[test]
#[ignore = "end-to-end; run via make e2e"]
fn no_durable_row_holds_prompt_text_or_a_credential() {
    runtime().block_on(async {
        let world = world();
        let key = world
            .issue_key("data-safety probe", json!({}))
            .await
            .expect("dedicated key");
        let prompt = nonce("secret-prompt");

        let response = world
            .gateway_post(
                "/openai/v1/chat/completions",
                json!({
                    "model": world::OPENAI_ROUTE,
                    "messages": [{"role": "user", "content": prompt}]
                }),
                &key.secret,
            )
            .await
            .expect("chat completion");
        assert_eq!(response.status, 200, "setup call failed: {}", response.text);

        // The record must exist before its absence of prompt text means
        // anything: scanning before ingestion would pass vacuously.
        world
            .await_request_rows(&key.id, ROUTE_FILTER, 1)
            .await
            .expect("the request is logged");

        let sightings = durable::rows_containing(&world.database_url, &prompt)
            .await
            .expect("database scan");
        assert!(
            sightings.is_empty(),
            "prompt text reached durable storage in {} table(s):\n{}",
            sightings.len(),
            durable::describe(&sightings)
        );

        // The proxy key secret is a credential; only its hash and lookup id may
        // be stored.
        let secret_sightings = durable::rows_containing(&world.database_url, &key.secret)
            .await
            .expect("database scan");
        assert!(
            secret_sightings.is_empty(),
            "the API key secret is stored in the clear in {} table(s):\n{}",
            secret_sightings.len(),
            durable::describe(&secret_sightings)
        );

        // Provider credentials are encrypted at rest, so a clean result here is
        // weak evidence — the scan cannot see through encryption. It is kept
        // for the direction that matters: it fails loudly if a change ever
        // writes one in the clear.
        let credential_sightings =
            durable::rows_containing(&world.database_url, mock_upstream::COMPAT_CREDENTIAL)
                .await
                .expect("database scan");
        assert!(
            credential_sightings.is_empty(),
            "a provider credential is stored in the clear in {} table(s):\n{}",
            credential_sightings.len(),
            durable::describe(&credential_sightings)
        );
    });
}

// ---------------------------------------------------------------------------
// Telemetry
//
// docs/architecture.md "Data-safety invariants": one bounded terminal metadata
// envelope per request, and "Missing upstream usage is incomplete and
// unpriced, never zero".
// ---------------------------------------------------------------------------

#[test]
#[ignore = "end-to-end; run via make e2e"]
fn every_request_is_recorded_exactly_once() {
    runtime().block_on(async {
        let world = world();
        let key = world
            .issue_key("telemetry probe", json!({}))
            .await
            .expect("dedicated key");
        const CALLS: usize = 3;

        for index in 0..CALLS {
            let response = world
                .gateway_post(
                    "/openai/v1/chat/completions",
                    json!({
                        "model": world::OPENAI_ROUTE,
                        "messages": [{"role": "user", "content": nonce(&format!("count-{index}"))}]
                    }),
                    &key.secret,
                )
                .await
                .expect("chat completion");
            assert_eq!(
                response.status, 200,
                "call {index} failed: {}",
                response.text
            );
        }

        let rows = world
            .await_request_rows(&key.id, ROUTE_FILTER, CALLS)
            .await
            .expect("requests are logged");
        assert_eq!(
            rows.len(),
            CALLS,
            "{CALLS} requests produced {} log rows",
            rows.len()
        );

        for row in &rows {
            assert_eq!(row["route"], json!(world::OPENAI_ROUTE), "row: {row}");
            assert_eq!(row["surface"], json!("openai"), "row: {row}");
            assert_eq!(
                row["attempt_count"],
                json!(1),
                "a request that succeeded first time recorded {} attempts: {row}",
                row["attempt_count"]
            );
            assert_eq!(row["status_code"], json!(200), "row: {row}");
            assert_eq!(
                row["usage_complete"],
                json!(true),
                "the upstream reported usage, so the record must be complete: {row}"
            );
            assert_eq!(
                row["input_tokens"],
                json!(mock_upstream::PROMPT_TOKENS),
                "row: {row}"
            );
            assert_eq!(
                row["output_tokens"],
                json!(mock_upstream::COMPLETION_TOKENS),
                "row: {row}"
            );
            assert!(
                row["first_byte_ms"].is_number(),
                "a completed request must record time to first byte: {row}"
            );
        }

        let (start, end) = usage_window();
        let summary = world
            .management
            .get(&format!(
                "/api/v1/usage/summary?start={start}&end={end}&api_key_id={}",
                key.id
            ))
            .await
            .expect("usage summary");
        assert_eq!(
            summary.status, 200,
            "GET /api/v1/usage/summary returned {}: {}",
            summary.status, summary.body
        );
        assert_eq!(
            summary.body["request_count"],
            json!(CALLS),
            "usage summary: {}",
            summary.body
        );
        assert_eq!(
            summary.body["incomplete_count"],
            json!(0),
            "every call reported usage, so none is incomplete: {}",
            summary.body
        );
        assert_eq!(
            summary.body["input_tokens"],
            json!((mock_upstream::PROMPT_TOKENS * CALLS as u64).to_string()),
            "usage summary: {}",
            summary.body
        );
        assert_eq!(
            summary.body["output_tokens"],
            json!((mock_upstream::COMPLETION_TOKENS * CALLS as u64).to_string()),
            "usage summary: {}",
            summary.body
        );
    });
}

#[test]
#[ignore = "end-to-end; run via make e2e"]
fn missing_upstream_usage_is_incomplete_and_unpriced_never_zero() {
    // docs/architecture.md "Data-safety invariants", verbatim: "Missing
    // upstream usage is incomplete and unpriced, never zero." A record that
    // claims complete, priced, zero-token usage understates real spend and
    // cannot be told apart from a genuinely free request.
    runtime().block_on(async {
        let world = world();
        let key = world
            .issue_key("unpriced probe", json!({}))
            .await
            .expect("dedicated key");

        let response = world
            .gateway_post(
                "/openai/v1/chat/completions",
                json!({
                    "model": world::OPENAI_ROUTE,
                    "messages": [{
                        "role": "user",
                        "content": format!("{} {}", mock_upstream::NO_USAGE_MARKER, nonce("no-usage"))
                    }]
                }),
                &key.secret,
            )
            .await
            .expect("chat completion");
        assert_eq!(
            response.status, 200,
            "the upstream answered without usage, which is still a successful \
             completion: {}",
            response.text
        );

        let rows = world
            .await_request_rows(&key.id, ROUTE_FILTER, 1)
            .await
            .expect("the request is logged");
        assert_eq!(rows.len(), 1, "expected one row, got {}", rows.len());
        let row = &rows[0];

        assert_eq!(
            row["usage_complete"],
            json!(false),
            "the upstream reported no usage, so the record must be incomplete: {row}"
        );
        assert_eq!(
            row["unpriced"],
            json!(true),
            "incomplete usage must be recorded unpriced: {row}"
        );
        assert_ne!(
            row["input_tokens"],
            json!(0),
            "missing usage was recorded as zero input tokens rather than \
             absent: {row}"
        );
        assert_ne!(
            row["output_tokens"],
            json!(0),
            "missing usage was recorded as zero output tokens rather than \
             absent: {row}"
        );

        let (start, end) = usage_window();
        let completeness = world
            .management
            .get(&format!(
                "/api/v1/usage/completeness?start={start}&end={end}&api_key_id={}",
                key.id
            ))
            .await
            .expect("usage completeness");
        assert_eq!(
            completeness.status, 200,
            "GET /api/v1/usage/completeness returned {}: {}",
            completeness.status, completeness.body
        );
        assert_eq!(
            completeness.body["incomplete_count"],
            json!(1),
            "a request with no upstream usage must count as incomplete: {}",
            completeness.body
        );
        assert_eq!(
            completeness.body["unpriced_count"],
            json!(1),
            "a request with no upstream usage must count as unpriced: {}",
            completeness.body
        );
        assert_eq!(
            completeness.body["complete"],
            json!(false),
            "a range holding an incomplete request is not complete: {}",
            completeness.body
        );
    });
}

// ---------------------------------------------------------------------------
// Distributed limits
//
// docs/architecture.md "Distributed limit semantics": RPM, TPM and concurrency
// are decided by one atomic reservation against Valkey server time, which also
// derives `Retry-After`; "A rejection consumes no dimension".
// ---------------------------------------------------------------------------

#[test]
#[ignore = "end-to-end; run via make e2e"]
fn a_key_over_its_request_limit_is_refused_with_a_retry_after() {
    runtime().block_on(async {
        let world = world();
        let key = world
            .issue_key("rpm probe", json!({"requests_per_minute": 1}))
            .await
            .expect("rate-limited key");

        // Issuing the key already spent a request against the gateway's model
        // listing, so drive the limit from a known state: send until a 429
        // arrives, bounded, and assert the shape of the refusal.
        let checkpoint = world.mock.checkpoint();
        let mut refusal = None;
        let mut accepted = 0;
        for _ in 0..4 {
            let response = world
                .gateway_post(
                    "/openai/v1/chat/completions",
                    json!({
                        "model": world::OPENAI_ROUTE,
                        "messages": [{"role": "user", "content": nonce("rpm")}]
                    }),
                    &key.secret,
                )
                .await
                .expect("chat completion");
            if response.status == 429 {
                refusal = Some(response);
                break;
            }
            assert_eq!(
                response.status, 200,
                "an in-limit request failed with {}: {}",
                response.status, response.text
            );
            accepted += 1;
        }

        let refusal = refusal.unwrap_or_else(|| {
            panic!("a key limited to one request per minute served {accepted} requests without refusing any")
        });
        assert!(
            accepted <= 1,
            "a key limited to one request per minute served {accepted} before refusing"
        );

        let retry_after = refusal
            .header("retry-after")
            .unwrap_or_else(|| panic!("the 429 carries no Retry-After: {}", refusal.text));
        let seconds: u64 = retry_after.parse().unwrap_or_else(|_| {
            panic!("Retry-After must be a delay in seconds; got {retry_after:?}")
        });
        assert!(
            (1..=60).contains(&seconds),
            "Retry-After is derived from the remaining fixed minute window, so \
             it must fall in 1..=60; got {seconds}"
        );

        // "A rejection consumes no dimension" — and a refused request must not
        // reach the provider at all.
        let upstream = world.mock.since(checkpoint);
        assert_eq!(
            upstream.len(),
            accepted,
            "{accepted} admitted requests produced {} upstream calls, so a \
             refused request still reached the provider",
            upstream.len()
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
