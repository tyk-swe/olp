use super::*;

// ---------------------------------------------------------------------------
// Management API contract
// ---------------------------------------------------------------------------

#[test]
#[ignore = "end-to-end; run via make integration"]
fn the_openapi_endpoint_documents_itself() {
    // The OpenAPI document is the management API's published contract, so a
    // path the server answers but the document omits is undocumented surface.
    runtime().block_on(async {
        let served = served_openapi().await;
        let paths = served["paths"]
            .as_object()
            .expect("OpenAPI document has a paths object");

        assert!(
            paths.contains_key("/api/v3/openapi.json"),
            "the server answers GET /api/v3/openapi.json, but the document does \
             not list it among its {} paths",
            paths.len()
        );
    });
}

#[test]
#[ignore = "end-to-end; run via make integration"]
fn setup_cannot_be_replayed_once_an_owner_exists() {
    // README.md "Quick start": the bootstrap token is one-time and the owner is
    // created once, after which the token is retired. A second setup attempt
    // must be refused, or an installation could be re-owned.
    runtime().block_on(async {
        let world = world();
        let response = world
            .http
            .post(format!("{}/api/v3/setup", world.origin()))
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
