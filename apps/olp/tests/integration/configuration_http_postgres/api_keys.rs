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

    let unknown_field_patch = send(
        app,
        Method::PATCH,
        &format!("/api/v1/api-keys/{api_key_id}"),
        Some(json!({
            "unknown_property": "not_allowed"
        })),
        Some(cookie),
        Some(csrf),
        None,
        Some(&key_etag),
    )
    .await;
    assert_eq!(unknown_field_patch.status(), StatusCode::BAD_REQUEST);

    let misspelled_limit_patch = send(
        app,
        Method::PATCH,
        &format!("/api/v1/api-keys/{api_key_id}"),
        Some(json!({
            "request_per_minute": 60
        })),
        Some(cookie),
        Some(csrf),
        None,
        Some(&key_etag),
    )
    .await;
    assert_eq!(misspelled_limit_patch.status(), StatusCode::BAD_REQUEST);

    let null_name_patch = send(
        app,
        Method::PATCH,
        &format!("/api/v1/api-keys/{api_key_id}"),
        Some(json!({
            "name": null
        })),
        Some(cookie),
        Some(csrf),
        None,
        Some(&key_etag),
    )
    .await;
    assert_eq!(null_name_patch.status(), StatusCode::UNPROCESSABLE_ENTITY);

    let null_scopes_patch = send(
        app,
        Method::PATCH,
        &format!("/api/v1/api-keys/{api_key_id}"),
        Some(json!({
            "scopes": null
        })),
        Some(cookie),
        Some(csrf),
        None,
        Some(&key_etag),
    )
    .await;
    assert_eq!(null_scopes_patch.status(), StatusCode::UNPROCESSABLE_ENTITY);

    let null_routes_patch = send(
        app,
        Method::PATCH,
        &format!("/api/v1/api-keys/{api_key_id}"),
        Some(json!({
            "allowed_routes": null
        })),
        Some(cookie),
        Some(csrf),
        None,
        Some(&key_etag),
    )
    .await;
    assert_eq!(null_routes_patch.status(), StatusCode::UNPROCESSABLE_ENTITY);

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
    assert_eq!(updated_key_body["tokens_per_minute"], 10000);
    assert_eq!(updated_key_body["max_concurrency"], 4);

    // Update only name - preserves limits, routes, scopes
    let update_name_only = send(
        app,
        Method::PATCH,
        &format!("/api/v1/api-keys/{api_key_id}"),
        Some(json!({
            "name": "Name Only Update"
        })),
        Some(cookie),
        Some(csrf),
        None,
        Some(&key_etag),
    )
    .await;
    assert_eq!(update_name_only.status(), StatusCode::OK);
    key_etag = etag(&update_name_only);
    let name_only_detail = send(
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
    let name_only_body = response_json(name_only_detail).await;
    assert_eq!(name_only_body["name"], "Name Only Update");
    assert_eq!(name_only_body["scopes"], json!(["inference"]));
    assert_eq!(name_only_body["allowed_routes"], json!([]));
    assert_eq!(name_only_body["requests_per_minute"], 60);
    assert_eq!(name_only_body["tokens_per_minute"], 10000);
    assert_eq!(name_only_body["max_concurrency"], 4);

    // Clear only requests_per_minute by passing explicit null, preserving tokens_per_minute and max_concurrency
    let clear_rpm_only = send(
        app,
        Method::PATCH,
        &format!("/api/v1/api-keys/{api_key_id}"),
        Some(json!({
            "requests_per_minute": null
        })),
        Some(cookie),
        Some(csrf),
        None,
        Some(&key_etag),
    )
    .await;
    assert_eq!(clear_rpm_only.status(), StatusCode::OK);
    key_etag = etag(&clear_rpm_only);
    let clear_rpm_detail = send(
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
    let clear_rpm_body = response_json(clear_rpm_detail).await;
    assert!(clear_rpm_body["requests_per_minute"].is_null());
    assert_eq!(clear_rpm_body["tokens_per_minute"], 10000);
    assert_eq!(clear_rpm_body["max_concurrency"], 4);

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
    let revoked_key_etag = etag(&revoked_key);
    assert!(revoked_key_etag.starts_with('"'));
    let revoked_key_body = response_json(revoked_key).await;
    let revoked_key_replay = send(
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
    assert_eq!(revoked_key_replay.status(), StatusCode::OK);
    assert_eq!(etag(&revoked_key_replay), revoked_key_etag);
    assert_eq!(response_json(revoked_key_replay).await, revoked_key_body);
    let revoked_key_mismatch = send(
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
    assert_eq!(revoked_key_mismatch.status(), StatusCode::CONFLICT);

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
