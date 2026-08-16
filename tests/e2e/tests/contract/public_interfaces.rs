use super::*;

// ---------------------------------------------------------------------------
// Public interface surface
//
// README.md "Interfaces": all public interfaces share one origin — console at
// `/`, management at `/api/v1`, OpenAI at `/v1` and `/openai/v1`, Anthropic
// at `/anthropic/v1`, Gemini at `/gemini/v1` *and* `/gemini/v1beta`.
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
