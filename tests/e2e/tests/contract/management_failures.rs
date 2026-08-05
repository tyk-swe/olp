use super::*;

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
