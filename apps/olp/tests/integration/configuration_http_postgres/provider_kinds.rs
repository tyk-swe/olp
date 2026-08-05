use super::*;

pub(super) async fn verify(app: &Router, cookie: &str, csrf: &str) {
    let vertex = send(
        app,
        Method::POST,
        "/api/v1/providers",
        Some(json!({
            "name": "vertex-draft",
            "kind": "vertex_ai",
            "cloud_project": "project-test",
            "cloud_region": "us-central1",
            "auth_mode": "adc",
            "model": "gemini-test",
            "display_name": "Gemini Test"
        })),
        Some(cookie),
        Some(csrf),
        Some("provider-vertex-create-01"),
        None,
    )
    .await;
    assert_eq!(vertex.status(), StatusCode::CREATED);
    let vertex_etag = etag(&vertex);
    let vertex_id = response_json(vertex).await["id"]
        .as_str()
        .unwrap()
        .to_owned();
    let vertex_detail = send(
        app,
        Method::GET,
        &format!("/api/v1/providers/{vertex_id}"),
        None,
        Some(cookie),
        None,
        None,
        None,
    )
    .await;
    let vertex_body = response_json(vertex_detail).await;
    assert_eq!(vertex_body["connector_ready"], true);
    assert_eq!(vertex_body["model_count"], 1);
    assert!(vertex_body.get("models").is_none());
    let vertex_models = send(
        app,
        Method::GET,
        &format!("/api/v1/providers/{vertex_id}/models?limit=100"),
        None,
        Some(cookie),
        None,
        None,
        None,
    )
    .await;
    assert_eq!(
        response_json(vertex_models).await["items"][0]["enabled"],
        true
    );
    let vertex_probe = send(
        app,
        Method::POST,
        &format!("/api/v1/providers/{vertex_id}/probe"),
        None,
        Some(cookie),
        Some(csrf),
        None,
        Some(&vertex_etag),
    )
    .await;
    assert_eq!(vertex_probe.status(), StatusCode::UNPROCESSABLE_ENTITY);
    assert!(!vertex_etag.is_empty());

    let invalid_azure = send(
        app,
        Method::POST,
        "/api/v1/providers",
        Some(json!({
            "name": "invalid-azure",
            "kind": "azure_openai",
            "model": "deployment-model"
        })),
        Some(cookie),
        Some(csrf),
        Some("provider-azure-invalid-01"),
        None,
    )
    .await;
    assert_eq!(invalid_azure.status(), StatusCode::UNPROCESSABLE_ENTITY);

    let azure = send(
        app,
        Method::POST,
        "/api/v1/providers",
        Some(json!({
            "name": "azure-primary",
            "kind": "azure_openai",
            "endpoint": "https://resource.openai.azure.com",
            "deployment": "team-chat",
            "api_version": "2024-10-21",
            "credential": "azure-test-secret"
        })),
        Some(cookie),
        Some(csrf),
        Some("provider-azure-create-01"),
        None,
    )
    .await;
    assert_eq!(azure.status(), StatusCode::CREATED);
    let azure_etag = etag(&azure);
    let azure_id = response_json(azure).await["id"]
        .as_str()
        .unwrap()
        .to_owned();
    let azure_detail = send(
        app,
        Method::GET,
        &format!("/api/v1/providers/{azure_id}"),
        None,
        Some(cookie),
        None,
        None,
        None,
    )
    .await;
    let azure_body = response_json(azure_detail).await;
    assert_eq!(azure_body["connector_ready"], true);
    assert_eq!(azure_body["deployment"], "team-chat");
    assert_eq!(azure_body["api_version"], "2024-10-21");
    assert_eq!(azure_body["model_count"], 0);
    assert!(azure_body.get("models").is_none());
    let azure_probe = send(
        app,
        Method::POST,
        &format!("/api/v1/providers/{azure_id}/probe"),
        None,
        Some(cookie),
        Some(csrf),
        None,
        Some(&azure_etag),
    )
    .await;
    assert_eq!(azure_probe.status(), StatusCode::UNPROCESSABLE_ENTITY);
    assert!(!azure_etag.is_empty());

    for (idempotency_key, request) in [
        (
            "provider-http-legacy-vertex-key-0001",
            json!({
                "name": "legacy-vertex",
                "kind": "vertex_ai",
                "cloud_project": "project-test",
                "cloud_region": "us-central1",
                "auth_mode": "adc",
                "model": "gemini-test",
                "api_key": "legacy-secret"
            }),
        ),
        (
            "provider-http-legacy-bedrock-key-0001",
            json!({
                "name": "legacy-bedrock",
                "kind": "bedrock",
                "cloud_region": "us-east-1",
                "auth_mode": "default_chain",
                "api_key": "legacy-secret"
            }),
        ),
    ] {
        let legacy_provider_request = send(
            app,
            Method::POST,
            "/api/v1/providers",
            Some(request),
            Some(cookie),
            Some(csrf),
            Some(idempotency_key),
            None,
        )
        .await;
        assert_eq!(
            legacy_provider_request.status(),
            StatusCode::UNPROCESSABLE_ENTITY
        );
        assert!(response_json(legacy_provider_request).await["errors"]["api_key"].is_array());
    }
}
