use super::*;

pub(super) async fn exercise(
    app: &Router,
    configuration_state: &ProcessComposition,
    mock_provider: &MockOpenAiProvider,
    cookie: &str,
    csrf: &str,
) -> (String, String) {
    let provider = send(
        app,
        Method::POST,
        "/api/v1/providers",
        Some(json!({
            "name": "openai-primary",
            "kind": "openai",
            "credential": "sk-openai-test-secret",
            "model": "compatible-model",
            "display_name": "Compatible Model"
        })),
        Some(cookie),
        Some(csrf),
        Some("provider-http-create-0001"),
        None,
    )
    .await;
    assert_eq!(provider.status(), StatusCode::CREATED);
    let mut provider_etag = etag(&provider);
    let provider_body = response_json(provider).await;
    let provider_id = provider_body["id"].as_str().unwrap().to_owned();
    configuration_state.register_certification_probe_connector_for_test(
        Uuid::parse_str(&provider_id).unwrap(),
        mock_provider.connector("sk-openai-test-secret"),
    );
    let provider_replay = send(
        app,
        Method::POST,
        "/api/v1/providers",
        Some(json!({
            "name": "openai-primary",
            "kind": "openai",
            "credential": "sk-openai-test-secret",
            "model": "compatible-model",
            "display_name": "Compatible Model"
        })),
        Some(cookie),
        Some(csrf),
        Some("provider-http-create-0001"),
        None,
    )
    .await;
    assert_eq!(provider_replay.status(), StatusCode::CREATED);
    assert_eq!(etag(&provider_replay), provider_etag);
    assert_eq!(response_json(provider_replay).await, provider_body);
    let provider_mismatch = send(
        app,
        Method::POST,
        "/api/v1/providers",
        Some(json!({
            "name": "openai-changed",
            "kind": "openai",
            "credential": "sk-openai-test-secret",
            "model": "compatible-model",
            "display_name": "Compatible Model"
        })),
        Some(cookie),
        Some(csrf),
        Some("provider-http-create-0001"),
        None,
    )
    .await;
    assert_eq!(provider_mismatch.status(), StatusCode::CONFLICT);

    let provider_update = send(
        app,
        Method::PATCH,
        &format!("/api/v1/providers/{provider_id}"),
        Some(json!({
            "name": "openai-primary-updated",
            "auth_mode": "api_key"
        })),
        Some(cookie),
        Some(csrf),
        None,
        Some(&provider_etag),
    )
    .await;
    assert_eq!(provider_update.status(), StatusCode::OK);
    provider_etag = etag(&provider_update);
    assert_eq!(
        response_json(provider_update).await["name"],
        "openai-primary-updated"
    );

    let providers = send(
        app,
        Method::GET,
        "/api/v1/providers?limit=10",
        None,
        Some(cookie),
        None,
        None,
        None,
    )
    .await;
    assert_eq!(providers.status(), StatusCode::OK);
    let providers_body = response_json(providers).await;
    let openai = providers_body["items"]
        .as_array()
        .unwrap()
        .iter()
        .find(|provider| provider["kind"] == "openai")
        .unwrap();
    assert!(openai.get("credential").is_none());
    assert!(openai.get("models").is_none());
    assert!(openai.get("endpoint").is_none());
    assert_eq!(openai["model_count"], 1);

    let missing_probe_precondition = send(
        app,
        Method::POST,
        &format!("/api/v1/providers/{provider_id}/probe"),
        None,
        Some(cookie),
        Some(csrf),
        None,
        None,
    )
    .await;
    assert_eq!(
        missing_probe_precondition.status(),
        StatusCode::PRECONDITION_REQUIRED
    );
    let probe = send(
        app,
        Method::POST,
        &format!("/api/v1/providers/{provider_id}/probe"),
        None,
        Some(cookie),
        Some(csrf),
        None,
        Some(&provider_etag),
    )
    .await;
    assert_eq!(probe.status(), StatusCode::OK);
    let probe_body = response_json(probe).await;
    assert_eq!(probe_body["succeeded"], true);
    assert_eq!(probe_body["probe_type"], "connector_connectivity");
    assert_eq!(mock_provider.model_requests(), 1);

    let discovery = send(
        app,
        Method::POST,
        &format!("/api/v1/providers/{provider_id}/discovery"),
        Some(json!({
            "models": [
                {
                    "upstream_model": "compatible-model",
                    "display_name": "Compatible Model"
                },
                {
                    "upstream_model": "compatible-model-secondary",
                    "display_name": "Compatible Model Secondary"
                }
            ]
        })),
        Some(cookie),
        Some(csrf),
        None,
        Some(&provider_etag),
    )
    .await;
    assert_eq!(discovery.status(), StatusCode::OK);
    let discovery_etag = etag(&discovery);
    let discovery_body = response_json(discovery).await;
    assert_eq!(discovery_body["model_count"], 2);
    assert!(discovery_body.get("models").is_none());
    let first_model_page = send(
        app,
        Method::GET,
        &format!("/api/v1/providers/{provider_id}/models?limit=1"),
        None,
        Some(cookie),
        None,
        None,
        None,
    )
    .await;
    assert_eq!(first_model_page.status(), StatusCode::OK);
    let first_model_page = response_json(first_model_page).await;
    assert_eq!(first_model_page["items"].as_array().unwrap().len(), 1);
    let model_cursor = first_model_page["next_cursor"].as_str().unwrap();
    let second_model_page = send(
        app,
        Method::GET,
        &format!("/api/v1/providers/{provider_id}/models?limit=1&cursor={model_cursor}"),
        None,
        Some(cookie),
        None,
        None,
        None,
    )
    .await;
    assert_eq!(second_model_page.status(), StatusCode::OK);
    let second_model_page = response_json(second_model_page).await;
    assert_eq!(second_model_page["items"].as_array().unwrap().len(), 1);
    assert!(second_model_page["next_cursor"].is_null());
    let model_id = first_model_page["items"]
        .as_array()
        .unwrap()
        .iter()
        .chain(second_model_page["items"].as_array().unwrap())
        .find(|model| model["upstream_model"] == "compatible-model")
        .and_then(|model| model["id"].as_str())
        .unwrap()
        .to_owned();

    let stale_discovery = send(
        app,
        Method::POST,
        &format!("/api/v1/providers/{provider_id}/discovery"),
        Some(json!({
            "models": [{
                "upstream_model": "compatible-model",
                "display_name": "Compatible Model"
            }]
        })),
        Some(cookie),
        Some(csrf),
        None,
        Some(&provider_etag),
    )
    .await;
    assert_eq!(stale_discovery.status(), StatusCode::PRECONDITION_FAILED);

    let reviewed_model = send(
        app,
        Method::PATCH,
        &format!("/api/v1/providers/{provider_id}/models/{model_id}"),
        Some(json!({
            "enabled": true,
            "capabilities": [
                {"operation": "embeddings", "surface": "openai", "mode": "unary"}
            ]
        })),
        Some(cookie),
        Some(csrf),
        None,
        Some(&discovery_etag),
    )
    .await;
    assert_eq!(reviewed_model.status(), StatusCode::OK);
    let reviewed_etag = etag(&reviewed_model);
    let reviewed_body = response_json(reviewed_model).await;
    assert_eq!(reviewed_body["enabled_model_count"], 1);
    assert!(reviewed_body.get("models").is_none());
    let reviewed_models = send(
        app,
        Method::GET,
        &format!("/api/v1/providers/{provider_id}/models?limit=100"),
        None,
        Some(cookie),
        None,
        None,
        None,
    )
    .await;
    let reviewed_models = response_json(reviewed_models).await;
    let reviewed_model = reviewed_models["items"]
        .as_array()
        .unwrap()
        .iter()
        .find(|model| model["id"] == model_id)
        .unwrap();
    assert_eq!(reviewed_model["enabled"], true);
    assert_eq!(reviewed_model["capabilities"][0]["source"], "declared");
    let eligible_inventory = send(
        app,
        Method::GET,
        "/api/v1/provider-models?enabled=true&limit=100",
        None,
        Some(cookie),
        None,
        None,
        None,
    )
    .await;
    assert_eq!(eligible_inventory.status(), StatusCode::OK);
    let eligible_inventory = response_json(eligible_inventory).await;
    let inventory_model = eligible_inventory["items"]
        .as_array()
        .unwrap()
        .iter()
        .find(|item| item["provider_id"] == provider_id && item["model"]["id"] == model_id)
        .unwrap();
    assert_eq!(inventory_model["provider_kind"], "openai");
    assert_eq!(inventory_model["model"]["enabled"], true);

    let reviewed_probe = send(
        app,
        Method::POST,
        &format!("/api/v1/providers/{provider_id}/probe"),
        None,
        Some(cookie),
        Some(csrf),
        None,
        Some(&reviewed_etag),
    )
    .await;
    assert_eq!(reviewed_probe.status(), StatusCode::OK);
    assert_eq!(response_json(reviewed_probe).await["succeeded"], true);
    assert_eq!(mock_provider.model_requests(), 2);

    let native_certification = send(
        app,
        Method::POST,
        &format!("/api/v1/providers/{provider_id}/models/{model_id}/certify"),
        None,
        Some(cookie),
        Some(csrf),
        None,
        Some(&reviewed_etag),
    )
    .await;
    assert_eq!(native_certification.status(), StatusCode::OK);
    let certified_etag = etag(&native_certification);
    assert_eq!(
        response_json(native_certification).await["certified_count"],
        1
    );

    let missing_activation_key = send(
        app,
        Method::POST,
        &format!("/api/v1/providers/{provider_id}/activate"),
        None,
        Some(cookie),
        Some(csrf),
        None,
        Some(&certified_etag),
    )
    .await;
    assert_eq!(missing_activation_key.status(), StatusCode::BAD_REQUEST);

    let activation = send(
        app,
        Method::POST,
        &format!("/api/v1/providers/{provider_id}/activate"),
        None,
        Some(cookie),
        Some(csrf),
        Some("provider-http-activate-01"),
        Some(&certified_etag),
    )
    .await;
    assert_eq!(activation.status(), StatusCode::OK);
    let active_etag = etag(&activation);
    let duplicate_activation = send(
        app,
        Method::POST,
        &format!("/api/v1/providers/{provider_id}/activate"),
        None,
        Some(cookie),
        Some(csrf),
        Some("provider-http-activate-01"),
        Some(&reviewed_etag),
    )
    .await;
    assert_eq!(duplicate_activation.status(), StatusCode::CONFLICT);

    let rotated_provider = send(
        app,
        Method::POST,
        &format!("/api/v1/providers/{provider_id}/credentials"),
        Some(json!({"credential": "sk-openai-rotated-secret"})),
        Some(cookie),
        Some(csrf),
        Some("provider-http-rotate-0001"),
        Some(&active_etag),
    )
    .await;
    assert_eq!(rotated_provider.status(), StatusCode::CREATED);
    let rotated_provider_etag = etag(&rotated_provider);
    let rotated_provider_body = response_json(rotated_provider).await;
    assert!(rotated_provider_body["runtime_generation"].is_null());
    let rotated_credential_id = rotated_provider_body["credential_id"]
        .as_str()
        .unwrap()
        .to_owned();
    let rotated_provider_replay = send(
        app,
        Method::POST,
        &format!("/api/v1/providers/{provider_id}/credentials"),
        Some(json!({"credential": "sk-openai-rotated-secret"})),
        Some(cookie),
        Some(csrf),
        Some("provider-http-rotate-0001"),
        Some(&active_etag),
    )
    .await;
    assert_eq!(rotated_provider_replay.status(), StatusCode::CREATED);
    assert_eq!(etag(&rotated_provider_replay), rotated_provider_etag);
    assert_eq!(
        response_json(rotated_provider_replay).await,
        rotated_provider_body
    );
    let rotated_provider_mismatch = send(
        app,
        Method::POST,
        &format!("/api/v1/providers/{provider_id}/credentials"),
        Some(json!({"credential": "sk-openai-different-secret"})),
        Some(cookie),
        Some(csrf),
        Some("provider-http-rotate-0001"),
        Some(&active_etag),
    )
    .await;
    assert_eq!(rotated_provider_mismatch.status(), StatusCode::CONFLICT);

    let staged_provider = send(
        app,
        Method::GET,
        &format!("/api/v1/providers/{provider_id}"),
        None,
        Some(cookie),
        None,
        None,
        None,
    )
    .await;
    assert_eq!(staged_provider.status(), StatusCode::OK);
    let staged_provider_body = response_json(staged_provider).await;
    assert_eq!(staged_provider_body["state"], "draft");
    assert_eq!(staged_provider_body["active_revision"], 1);
    assert_eq!(staged_provider_body["pending_activation"], true);
    assert_eq!(
        staged_provider_body["draft_credential_id"],
        rotated_credential_id
    );
    assert_ne!(
        staged_provider_body["runtime_credential_id"],
        staged_provider_body["draft_credential_id"]
    );
    let runtime_credential_id = staged_provider_body["runtime_credential_id"]
        .as_str()
        .unwrap()
        .to_owned();

    let credential_page = send(
        app,
        Method::GET,
        &format!("/api/v1/providers/{provider_id}/credentials?limit=1"),
        None,
        Some(cookie),
        None,
        None,
        None,
    )
    .await;
    assert_eq!(credential_page.status(), StatusCode::OK);
    let credential_page = response_json(credential_page).await;
    assert_eq!(credential_page["items"].as_array().unwrap().len(), 1);
    assert_eq!(credential_page["items"][0]["id"], rotated_credential_id);
    assert_eq!(credential_page["items"][0]["active"], false);
    assert_eq!(credential_page["items"][0]["draft_selected"], true);
    let credential_cursor = credential_page["next_cursor"].as_str().unwrap();
    let older_credentials = send(
        app,
        Method::GET,
        &format!("/api/v1/providers/{provider_id}/credentials?limit=1&cursor={credential_cursor}"),
        None,
        Some(cookie),
        None,
        None,
        None,
    )
    .await;
    assert_eq!(older_credentials.status(), StatusCode::OK);
    let older_credentials = response_json(older_credentials).await;
    assert_eq!(older_credentials["items"].as_array().unwrap().len(), 1);
    assert_eq!(older_credentials["items"][0]["id"], runtime_credential_id);
    assert_eq!(older_credentials["items"][0]["active"], true);
    assert_eq!(older_credentials["items"][0]["draft_selected"], false);
    assert!(older_credentials["next_cursor"].is_null());

    let cannot_revoke_runtime_credential = send(
        app,
        Method::POST,
        &format!("/api/v1/providers/{provider_id}/credentials/{runtime_credential_id}/revoke"),
        None,
        Some(cookie),
        Some(csrf),
        Some("provider-runtime-credential-revoke-blocked-01"),
        Some(&rotated_provider_etag),
    )
    .await;
    assert_eq!(
        cannot_revoke_runtime_credential.status(),
        StatusCode::CONFLICT
    );

    configuration_state.register_certification_probe_connector_for_test(
        Uuid::parse_str(&provider_id).unwrap(),
        mock_provider.connector("sk-openai-rotated-secret"),
    );
    let rotated_probe = send(
        app,
        Method::POST,
        &format!("/api/v1/providers/{provider_id}/probe"),
        None,
        Some(cookie),
        Some(csrf),
        None,
        Some(&rotated_provider_etag),
    )
    .await;
    assert_eq!(rotated_probe.status(), StatusCode::OK);
    assert_eq!(response_json(rotated_probe).await["succeeded"], true);
    assert_eq!(mock_provider.model_requests(), 3);
    assert_eq!(
        mock_provider.last_authorization().as_deref(),
        Some("Bearer sk-openai-rotated-secret")
    );
    let rotated_certification = send(
        app,
        Method::POST,
        &format!("/api/v1/providers/{provider_id}/models/{model_id}/certify"),
        None,
        Some(cookie),
        Some(csrf),
        None,
        Some(&rotated_provider_etag),
    )
    .await;
    assert_eq!(rotated_certification.status(), StatusCode::OK);
    let rotated_certified_etag = etag(&rotated_certification);
    assert_eq!(
        response_json(rotated_certification).await["certified_count"],
        1
    );
    let rotated_activation = send(
        app,
        Method::POST,
        &format!("/api/v1/providers/{provider_id}/activate"),
        None,
        Some(cookie),
        Some(csrf),
        Some("provider-http-activate-02"),
        Some(&rotated_certified_etag),
    )
    .await;
    assert_eq!(rotated_activation.status(), StatusCode::OK);
    let reactivated_provider_etag = etag(&rotated_activation);

    let reactivated_provider = send(
        app,
        Method::GET,
        &format!("/api/v1/providers/{provider_id}"),
        None,
        Some(cookie),
        None,
        None,
        None,
    )
    .await;
    let reactivated_provider_body = response_json(reactivated_provider).await;
    assert_eq!(reactivated_provider_body["state"], "active");
    assert_eq!(reactivated_provider_body["active_revision"], 2);
    assert_eq!(reactivated_provider_body["pending_activation"], false);
    assert_eq!(
        reactivated_provider_body["runtime_credential_id"],
        rotated_credential_id
    );
    assert_eq!(
        reactivated_provider_body["draft_credential_id"],
        rotated_credential_id
    );

    let provider_revisions = send(
        app,
        Method::GET,
        &format!("/api/v1/providers/{provider_id}/revisions?limit=1"),
        None,
        Some(cookie),
        None,
        None,
        None,
    )
    .await;
    assert_eq!(provider_revisions.status(), StatusCode::OK);
    let provider_revisions = response_json(provider_revisions).await;
    let latest_provider_revision = &provider_revisions["items"][0];
    assert_eq!(latest_provider_revision["revision"], 2);
    assert_eq!(latest_provider_revision["model_count"], 2);
    assert!(latest_provider_revision.get("models").is_none());
    assert!(provider_revisions["next_cursor"].is_string());
    let latest_provider_revision_id = latest_provider_revision["id"].as_str().unwrap();
    let provider_revision_detail = send(
        app,
        Method::GET,
        &format!("/api/v1/providers/{provider_id}/revisions/{latest_provider_revision_id}"),
        None,
        Some(cookie),
        None,
        None,
        None,
    )
    .await;
    let provider_revision_detail = response_json(provider_revision_detail).await;
    assert_eq!(provider_revision_detail["model_count"], 2);
    assert!(provider_revision_detail.get("models").is_none());
    let revision_models = send(
        app,
        Method::GET,
        &format!(
            "/api/v1/providers/{provider_id}/revisions/{latest_provider_revision_id}/models?limit=1"
        ),
        None,
        Some(cookie),
        None,
        None,
        None,
    )
    .await;
    assert_eq!(revision_models.status(), StatusCode::OK);
    let revision_models = response_json(revision_models).await;
    assert_eq!(revision_models["items"].as_array().unwrap().len(), 1);
    assert!(revision_models["next_cursor"].is_string());

    let active_credentials = send(
        app,
        Method::GET,
        &format!("/api/v1/providers/{provider_id}/credentials?limit=100"),
        None,
        Some(cookie),
        None,
        None,
        None,
    )
    .await;
    let active_credentials = response_json(active_credentials).await;
    let active_credential = active_credentials["items"]
        .as_array()
        .unwrap()
        .iter()
        .find(|credential| credential["id"] == rotated_credential_id)
        .unwrap();
    assert_eq!(active_credential["active"], true);
    assert_eq!(active_credential["draft_selected"], false);
    let obsolete_credential = active_credentials["items"]
        .as_array()
        .unwrap()
        .iter()
        .find(|credential| credential["id"] == runtime_credential_id)
        .unwrap();
    assert_eq!(obsolete_credential["active"], false);
    assert_eq!(obsolete_credential["draft_selected"], false);

    let revoked_credential = send(
        app,
        Method::POST,
        &format!("/api/v1/providers/{provider_id}/credentials/{runtime_credential_id}/revoke"),
        None,
        Some(cookie),
        Some(csrf),
        Some("provider-credential-revoke-01"),
        Some(&reactivated_provider_etag),
    )
    .await;
    assert_eq!(revoked_credential.status(), StatusCode::OK);

    (provider_id, model_id)
}
