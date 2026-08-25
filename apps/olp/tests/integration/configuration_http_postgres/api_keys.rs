use super::*;

pub(super) async fn exercise(
    app: &Router,
    cookie: &str,
    csrf: &str,
    draft_id: &str,
    model_id: &str,
) {
    let api_key = send(
        app,
        Method::POST,
        "/api/v1/api-keys",
        Some(json!({
            "name": "SDK key",
            "scopes": ["inference", "models_read"],
            "allowed_routes": ["default"]
        })),
        Some(cookie),
        Some(csrf),
        Some("api-key-http-create-0001"),
        None,
    )
    .await;
    assert_eq!(api_key.status(), StatusCode::CREATED);
    let api_key_create_etag = etag(&api_key);
    let api_key_body = response_json(api_key).await;
    let api_key_id = api_key_body["id"].as_str().unwrap().to_owned();
    assert!(
        api_key_body["secret"]
            .as_str()
            .unwrap()
            .starts_with("olp_v2_")
    );
    let api_key_replay = send(
        app,
        Method::POST,
        "/api/v1/api-keys",
        Some(json!({
            "name": "SDK key",
            "scopes": ["inference", "models_read"],
            "allowed_routes": ["default"]
        })),
        Some(cookie),
        Some(csrf),
        Some("api-key-http-create-0001"),
        None,
    )
    .await;
    assert_eq!(api_key_replay.status(), StatusCode::CREATED);
    assert_eq!(etag(&api_key_replay), api_key_create_etag);
    assert_eq!(response_json(api_key_replay).await, api_key_body);
    let api_key_mismatch = send(
        app,
        Method::POST,
        "/api/v1/api-keys",
        Some(json!({
            "name": "Changed SDK key",
            "scopes": ["inference", "models_read"],
            "allowed_routes": ["default"]
        })),
        Some(cookie),
        Some(csrf),
        Some("api-key-http-create-0001"),
        None,
    )
    .await;
    assert_eq!(api_key_mismatch.status(), StatusCode::CONFLICT);

    let key_detail = send(
        app,
        Method::GET,
        &format!("/api/v1/api-keys/{api_key_id}"),
        None,
        Some(cookie),
        None,
        None,
        None,
    )
    .await;
    assert_eq!(key_detail.status(), StatusCode::OK);
    let mut key_etag = etag(&key_detail);
    let key_detail_body = response_json(key_detail).await;
    assert!(key_detail_body.get("secret").is_none());
    assert_eq!(key_detail_body["allowed_routes"][0], "default");

    let duplicate_policy = send(
        app,
        Method::PATCH,
        &format!("/api/v1/api-keys/{api_key_id}"),
        Some(json!({
            "name": "Duplicate policy",
            "scopes": ["inference", "inference"],
            "allowed_routes": []
        })),
        Some(cookie),
        Some(csrf),
        None,
        Some(&key_etag),
    )
    .await;
    assert_eq!(duplicate_policy.status(), StatusCode::UNPROCESSABLE_ENTITY);
    let updated_key = send(
        app,
        Method::PATCH,
        &format!("/api/v1/api-keys/{api_key_id}"),
        Some(json!({
            "name": "Updated SDK key",
            "scopes": ["inference"],
            "allowed_routes": [],
            "requests_per_minute": 60,
            "tokens_per_minute": 10000,
            "max_concurrency": 4,
            "expires_at": null
        })),
        Some(cookie),
        Some(csrf),
        None,
        Some(&key_etag),
    )
    .await;
    assert_eq!(updated_key.status(), StatusCode::OK);
    let original_key_etag = key_etag;
    key_etag = etag(&updated_key);
    assert!(response_json(updated_key).await["runtime_generation"].is_object());
    let updated_key_detail = send(
        app,
        Method::GET,
        &format!("/api/v1/api-keys/{api_key_id}"),
        None,
        Some(cookie),
        None,
        None,
        None,
    )
    .await;
    assert_eq!(updated_key_detail.status(), StatusCode::OK);
    let updated_key_body = response_json(updated_key_detail).await;
    assert_eq!(updated_key_body["name"], "Updated SDK key");
    assert_eq!(updated_key_body["scopes"], json!(["inference"]));
    assert_eq!(updated_key_body["allowed_routes"], json!([]));
    assert_eq!(updated_key_body["requests_per_minute"], 60);
    let stale_key_update = send(
        app,
        Method::PATCH,
        &format!("/api/v1/api-keys/{api_key_id}"),
        Some(json!({
            "name": "Stale update",
            "scopes": ["inference"],
            "allowed_routes": []
        })),
        Some(cookie),
        Some(csrf),
        None,
        Some(&original_key_etag),
    )
    .await;
    assert_eq!(stale_key_update.status(), StatusCode::PRECONDITION_FAILED);

    let rotated_key = send(
        app,
        Method::POST,
        &format!("/api/v1/api-keys/{api_key_id}/rotate"),
        None,
        Some(cookie),
        Some(csrf),
        Some("api-key-http-rotate-0001"),
        Some(&key_etag),
    )
    .await;
    assert_eq!(rotated_key.status(), StatusCode::OK);
    let rotated_key_etag = etag(&rotated_key);
    let rotated_key_body = response_json(rotated_key).await;
    assert!(
        rotated_key_body["secret"]
            .as_str()
            .unwrap()
            .starts_with("olp_v2_")
    );
    assert!(rotated_key_body["runtime_generation"].is_object());
    let rotated_key_replay = send(
        app,
        Method::POST,
        &format!("/api/v1/api-keys/{api_key_id}/rotate"),
        None,
        Some(cookie),
        Some(csrf),
        Some("api-key-http-rotate-0001"),
        Some(&key_etag),
    )
    .await;
    assert_eq!(rotated_key_replay.status(), StatusCode::OK);
    assert_eq!(etag(&rotated_key_replay), rotated_key_etag);
    assert_eq!(response_json(rotated_key_replay).await, rotated_key_body);
    let rotated_key_mismatch = send(
        app,
        Method::POST,
        &format!("/api/v1/api-keys/{api_key_id}/rotate"),
        None,
        Some(cookie),
        Some(csrf),
        Some("api-key-http-rotate-0001"),
        Some(&rotated_key_etag),
    )
    .await;
    assert_eq!(rotated_key_mismatch.status(), StatusCode::CONFLICT);

    let stale_revoke = send(
        app,
        Method::POST,
        &format!("/api/v1/api-keys/{api_key_id}/revoke"),
        None,
        Some(cookie),
        Some(csrf),
        Some("api-key-http-revoke-0001"),
        Some(&key_etag),
    )
    .await;
    assert_eq!(stale_revoke.status(), StatusCode::PRECONDITION_FAILED);
    let revoked_key = send(
        app,
        Method::POST,
        &format!("/api/v1/api-keys/{api_key_id}/revoke"),
        None,
        Some(cookie),
        Some(csrf),
        Some("api-key-http-revoke-0001"),
        Some(&rotated_key_etag),
    )
    .await;
    assert_eq!(revoked_key.status(), StatusCode::OK);
    let revoked_etag = etag(&revoked_key);
    assert!(revoked_etag.starts_with('"'));
    let revoked_body = response_json(revoked_key).await;
    // A retry after a dropped connection replays the recorded response
    // instead of being told the key was already used.
    let replayed_revoke = send(
        app,
        Method::POST,
        &format!("/api/v1/api-keys/{api_key_id}/revoke"),
        None,
        Some(cookie),
        Some(csrf),
        Some("api-key-http-revoke-0001"),
        Some(&rotated_key_etag),
    )
    .await;
    assert_eq!(replayed_revoke.status(), StatusCode::OK);
    assert_eq!(etag(&replayed_revoke), revoked_etag);
    assert_eq!(response_json(replayed_revoke).await, revoked_body);
    // The same key with a different request is a reuse, not a retry.
    let duplicate_revoke = send(
        app,
        Method::POST,
        &format!("/api/v1/api-keys/{api_key_id}/revoke"),
        None,
        Some(cookie),
        Some(csrf),
        Some("api-key-http-revoke-0001"),
        Some(&key_etag),
    )
    .await;
    assert_eq!(duplicate_revoke.status(), StatusCode::CONFLICT);

    // The route target remains identified by its durable model ID in configuration responses.
    let draft_detail = send(
        app,
        Method::GET,
        &format!("/api/v1/route-drafts/{draft_id}"),
        None,
        Some(cookie),
        None,
        None,
        None,
    )
    .await;
    assert_eq!(
        response_json(draft_detail).await["targets"][0]["provider_model_id"],
        model_id
    );
}
