use super::*;

pub(super) async fn verify(app: &Router, cookie: &str, csrf: &str) {
    let kind_response = send(
        app,
        Method::GET,
        "/api/v3/provider-kinds",
        None,
        Some(cookie),
        None,
        None,
        None,
    )
    .await;
    assert_eq!(kind_response.status(), StatusCode::OK);
    let kind_body = response_json(kind_response).await;
    let kinds = kind_body["items"].as_array().unwrap();
    let compatible = kinds
        .iter()
        .find(|kind| kind["kind"] == "openai_compatible")
        .unwrap();
    assert_eq!(
        compatible["presets"]
            .as_array()
            .unwrap()
            .iter()
            .map(|preset| preset["id"].as_str().unwrap())
            .collect::<Vec<_>>(),
        [
            "groq",
            "mistral_ai",
            "together_ai",
            "xai",
            "cerebras",
            "openrouter",
            "deepseek",
            "fireworks",
            "deepinfra",
            "huggingface",
            "perplexity",
            "cohere",
            "voyage"
        ]
    );
    assert!(kinds.iter().all(|kind| {
        kind["kind"] == "openai_compatible" || kind["presets"].as_array().is_some_and(Vec::is_empty)
    }));

    let vertex = send(
        app,
        Method::POST,
        "/api/v3/providers",
        Some(json!({"name": "vertex-draft", "model": "gemini-test", "display_name": "Gemini Test", "configuration": {"kind": "vertex_ai", "cloud_project": "project-test", "cloud_region": "us-central1", "auth_mode": "adc"}})),
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
        &format!("/api/v3/providers/{vertex_id}"),
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
        &format!("/api/v3/providers/{vertex_id}/models?limit=100"),
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
        &format!("/api/v3/providers/{vertex_id}/probe"),
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
        "/api/v3/providers",
        Some(json!({"name": "invalid-azure", "model": "deployment-model", "configuration": {"kind": "azure_openai", "auth_mode": "api_key"}})),
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
        "/api/v3/providers",
        Some(json!({"name": "azure-primary", "credential": "azure-test-secret", "configuration": {"kind": "azure_openai", "endpoint": "https://resource.openai.azure.com", "deployment": "team-chat", "api_version": "2024-10-21", "auth_mode": "api_key"}})),
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
        &format!("/api/v3/providers/{azure_id}"),
        None,
        Some(cookie),
        None,
        None,
        None,
    )
    .await;
    let azure_body = response_json(azure_detail).await;
    assert_eq!(azure_body["connector_ready"], true);
    assert_eq!(azure_body["configuration"]["deployment"], "team-chat");
    assert_eq!(azure_body["configuration"]["api_version"], "2024-10-21");
    assert_eq!(azure_body["model_count"], 0);
    assert!(azure_body.get("models").is_none());
    let azure_probe = send(
        app,
        Method::POST,
        &format!("/api/v3/providers/{azure_id}/probe"),
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
            "provider-http-vertex-inline-key-0001",
            json!({"name": "vertex-inline-key", "model": "gemini-test", "credential": "unused-secret", "configuration": {"kind": "vertex_ai", "cloud_project": "project-test", "cloud_region": "us-central1", "auth_mode": "adc"}}),
        ),
        (
            "provider-http-bedrock-inline-key-0001",
            json!({"name": "bedrock-inline-key", "credential": "unused-secret", "configuration": {"kind": "bedrock", "cloud_region": "us-east-1", "auth_mode": "default_chain"}}),
        ),
    ] {
        let inline_credential_request = send(
            app,
            Method::POST,
            "/api/v3/providers",
            Some(request),
            Some(cookie),
            Some(csrf),
            Some(idempotency_key),
            None,
        )
        .await;
        assert_eq!(
            inline_credential_request.status(),
            StatusCode::UNPROCESSABLE_ENTITY
        );
        assert!(response_json(inline_credential_request).await["errors"]["credential"].is_array());
    }
}

pub(super) async fn verify_egress_policy(
    app: &Router,
    mock_provider: &MockOpenAiProvider,
    cookie: &str,
    csrf: &str,
) {
    let loopback = send(
        app,
        Method::POST,
        "/api/v3/providers",
        Some(json!({"name": "loopback-compatible", "credential": "sk-loopback-secret", "model": "compatible-model", "display_name": "Loopback Compatible", "configuration": {"kind": "openai_compatible", "endpoint": mock_provider.base_url(), "auth_mode": "api_key"}})),
        Some(cookie),
        Some(csrf),
        Some("provider-egress-create-01"),
        None,
    )
    .await;
    assert_eq!(loopback.status(), StatusCode::CREATED);

    let outside_allowlist = send(
        app,
        Method::POST,
        "/api/v3/providers",
        Some(json!({"name": "private-compatible", "credential": "sk-private-secret", "model": "compatible-model", "display_name": "Private Compatible", "configuration": {"kind": "openai_compatible", "endpoint": "http://10.0.0.5:8000/v1", "auth_mode": "api_key"}})),
        Some(cookie),
        Some(csrf),
        Some("provider-egress-create-02"),
        None,
    )
    .await;
    assert_eq!(outside_allowlist.status(), StatusCode::UNPROCESSABLE_ENTITY);
    let problem = response_json(outside_allowlist).await;
    assert!(problem["errors"]["endpoint"].is_array());
}
