use super::*;

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
                Some(json!({})),
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
