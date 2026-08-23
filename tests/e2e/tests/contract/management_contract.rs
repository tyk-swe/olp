use super::*;

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
