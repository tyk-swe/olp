use super::*;

pub(super) async fn exercise(
    app: &Router,
    cookie: &str,
    csrf: &str,
    draft_id: &str,
    model_id: &str,
) {
    let (api_key_id, key_etag) = create_key(app, cookie, csrf).await;
    let key_etag = update_key(app, cookie, csrf, &api_key_id, &key_etag).await;
    let stale_key_etag = key_etag.clone();
    let key_etag = rotate_key(app, cookie, csrf, &api_key_id, &key_etag).await;
    assert_budget_audits(app, cookie, &api_key_id).await;
    revoke_key(app, cookie, csrf, &api_key_id, &stale_key_etag, &key_etag).await;
    assert_draft_target(app, cookie, draft_id, model_id).await;
}

async fn create_key(app: &Router, cookie: &str, csrf: &str) -> (String, String) {
    let api_key = send(
        app,
        Method::POST,
        "/api/v1/api-keys",
        Some(create_request("SDK key")),
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

    let replay = send(
        app,
        Method::POST,
        "/api/v1/api-keys",
        Some(create_request("SDK key")),
        Some(cookie),
        Some(csrf),
        Some("api-key-http-create-0001"),
        None,
    )
    .await;
    assert_eq!(replay.status(), StatusCode::CREATED);
    assert_eq!(etag(&replay), api_key_create_etag);
    assert_eq!(response_json(replay).await, api_key_body);

    let mismatch = send(
        app,
        Method::POST,
        "/api/v1/api-keys",
        Some(create_request("Changed SDK key")),
        Some(cookie),
        Some(csrf),
        Some("api-key-http-create-0001"),
        None,
    )
    .await;
    assert_eq!(mismatch.status(), StatusCode::CONFLICT);

    let detail = get_key(app, cookie, &api_key_id).await;
    assert_eq!(detail.status(), StatusCode::OK);
    assert_eq!(etag(&detail), api_key_create_etag);
    let detail = response_json(detail).await;
    assert!(detail.get("secret").is_none());
    assert_eq!(detail["allowed_routes"], json!(["default"]));
    assert_budget_shape(&detail, Some("0.25"), Some("12.34"));
    assert_list_budget(app, cookie, &api_key_id, Some("0.25"), Some("12.34")).await;

    (api_key_id, api_key_create_etag)
}

fn create_request(name: &str) -> Value {
    json!({
        "name": name,
        "scopes": ["inference", "models_read"],
        "allowed_routes": ["default"],
        "daily_cost_limit": "0.2500",
        "monthly_cost_limit": "12.3400"
    })
}

async fn update_key(
    app: &Router,
    cookie: &str,
    csrf: &str,
    api_key_id: &str,
    key_etag: &str,
) -> String {
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
        Some(key_etag),
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
            "daily_cost_limit": "1.234500",
            "monthly_cost_limit": null,
            "expires_at": null
        })),
        Some(cookie),
        Some(csrf),
        None,
        Some(key_etag),
    )
    .await;
    assert_eq!(updated_key.status(), StatusCode::OK);
    let updated_etag = etag(&updated_key);
    assert!(response_json(updated_key).await["runtime_generation"].is_object());

    let detail = get_key(app, cookie, api_key_id).await;
    assert_eq!(detail.status(), StatusCode::OK);
    let detail = response_json(detail).await;
    assert_eq!(detail["name"], "Updated SDK key");
    assert_eq!(detail["scopes"], json!(["inference"]));
    assert_eq!(detail["allowed_routes"], json!([]));
    assert_eq!(detail["requests_per_minute"], 60);
    assert_budget_shape(&detail, Some("1.2345"), None);

    let stale_update = send(
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
        Some(key_etag),
    )
    .await;
    assert_eq!(stale_update.status(), StatusCode::PRECONDITION_FAILED);

    updated_etag
}

async fn rotate_key(
    app: &Router,
    cookie: &str,
    csrf: &str,
    api_key_id: &str,
    key_etag: &str,
) -> String {
    let rotated_key = send(
        app,
        Method::POST,
        &format!("/api/v1/api-keys/{api_key_id}/rotate"),
        None,
        Some(cookie),
        Some(csrf),
        Some("api-key-http-rotate-0001"),
        Some(key_etag),
    )
    .await;
    assert_eq!(rotated_key.status(), StatusCode::OK);
    let rotated_key_etag = etag(&rotated_key);
    let rotated_key_body = response_json(rotated_key).await;
    assert_secret_and_generation(&rotated_key_body);

    let detail = get_key(app, cookie, api_key_id).await;
    assert_eq!(detail.status(), StatusCode::OK);
    assert_budget_shape(&response_json(detail).await, Some("1.2345"), None);

    let replay = send(
        app,
        Method::POST,
        &format!("/api/v1/api-keys/{api_key_id}/rotate"),
        None,
        Some(cookie),
        Some(csrf),
        Some("api-key-http-rotate-0001"),
        Some(key_etag),
    )
    .await;
    assert_eq!(replay.status(), StatusCode::OK);
    assert_eq!(etag(&replay), rotated_key_etag);
    assert_eq!(response_json(replay).await, rotated_key_body);

    let mismatch = send(
        app,
        Method::POST,
        &format!("/api/v1/api-keys/{api_key_id}/rotate"),
        Some(json!({"daily_cost_limit": "9.99"})),
        Some(cookie),
        Some(csrf),
        Some("api-key-http-rotate-0001"),
        Some(key_etag),
    )
    .await;
    assert_eq!(mismatch.status(), StatusCode::CONFLICT);

    let patched = send(
        app,
        Method::POST,
        &format!("/api/v1/api-keys/{api_key_id}/rotate"),
        Some(json!({
            "daily_cost_limit": null,
            "monthly_cost_limit": "250.7500"
        })),
        Some(cookie),
        Some(csrf),
        Some("api-key-http-rotate-0002"),
        Some(&rotated_key_etag),
    )
    .await;
    assert_eq!(patched.status(), StatusCode::OK);
    let patched_etag = etag(&patched);
    assert_secret_and_generation(&response_json(patched).await);

    let detail = get_key(app, cookie, api_key_id).await;
    assert_eq!(detail.status(), StatusCode::OK);
    assert_budget_shape(&response_json(detail).await, None, Some("250.75"));

    patched_etag
}

fn assert_secret_and_generation(body: &Value) {
    assert!(body["secret"].as_str().unwrap().starts_with("olp_v2_"));
    assert!(body["runtime_generation"].is_object());
}

async fn assert_list_budget(
    app: &Router,
    cookie: &str,
    api_key_id: &str,
    daily_limit: Option<&str>,
    monthly_limit: Option<&str>,
) {
    let list = send(
        app,
        Method::GET,
        "/api/v1/api-keys?limit=100",
        None,
        Some(cookie),
        None,
        None,
        None,
    )
    .await;
    assert_eq!(list.status(), StatusCode::OK);
    let list = response_json(list).await;
    let key = list["items"]
        .as_array()
        .unwrap()
        .iter()
        .find(|key| key["id"] == api_key_id)
        .unwrap();
    assert!(key.get("secret").is_none());
    assert_budget_shape(key, daily_limit, monthly_limit);
}

fn assert_budget_shape(body: &Value, daily_limit: Option<&str>, monthly_limit: Option<&str>) {
    let budget = body["budget"].as_object().unwrap();
    assert_eq!(budget.len(), 3);
    assert_budget_window(&budget["daily"], daily_limit);
    assert_budget_window(&budget["monthly"], monthly_limit);
    assert_eq!(budget["unpriced_attempts"], 0);
}

fn assert_budget_window(window: &Value, expected_limit: Option<&str>) {
    let window = window.as_object().unwrap();
    assert_eq!(window.len(), 3);
    assert_eq!(
        window["limit"],
        expected_limit.map_or(Value::Null, |limit| json!(limit))
    );
    assert_eq!(window["accrued"], "0");
    let window_ends_at = window["window_ends_at"].as_str().unwrap();
    chrono::DateTime::parse_from_rfc3339(window_ends_at).unwrap();
}

async fn assert_budget_audits(app: &Router, cookie: &str, api_key_id: &str) {
    for (action, expected_count) in [
        ("api_key.create", 1),
        ("api_key.update", 1),
        ("api_key.rotate", 2),
    ] {
        let audit = send(
            app,
            Method::GET,
            &format!(
                "/api/v1/audit?action={action}&resource_type=api_key&resource_id={api_key_id}&outcome=success"
            ),
            None,
            Some(cookie),
            None,
            None,
            None,
        )
        .await;
        assert_eq!(audit.status(), StatusCode::OK);
        let audit = response_json(audit).await;
        let items = audit["items"].as_array().unwrap();
        assert_eq!(items.len(), expected_count, "unexpected {action} audits");
        assert!(items.iter().all(|event| {
            event["action"] == action
                && event["resource_type"] == "api_key"
                && event["resource_id"] == api_key_id
                && event["outcome"] == "success"
        }));
    }
}

async fn revoke_key(
    app: &Router,
    cookie: &str,
    csrf: &str,
    api_key_id: &str,
    stale_etag: &str,
    current_etag: &str,
) {
    let stale_revoke = send(
        app,
        Method::POST,
        &format!("/api/v1/api-keys/{api_key_id}/revoke"),
        None,
        Some(cookie),
        Some(csrf),
        Some("api-key-http-revoke-0001"),
        Some(stale_etag),
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
        Some(current_etag),
    )
    .await;
    assert_eq!(revoked_key.status(), StatusCode::OK);
    let revoked_etag = etag(&revoked_key);
    assert!(revoked_etag.starts_with('"'));
    let revoked_body = response_json(revoked_key).await;

    let replay = send(
        app,
        Method::POST,
        &format!("/api/v1/api-keys/{api_key_id}/revoke"),
        None,
        Some(cookie),
        Some(csrf),
        Some("api-key-http-revoke-0001"),
        Some(current_etag),
    )
    .await;
    assert_eq!(replay.status(), StatusCode::OK);
    assert_eq!(etag(&replay), revoked_etag);
    assert_eq!(response_json(replay).await, revoked_body);

    let duplicate = send(
        app,
        Method::POST,
        &format!("/api/v1/api-keys/{api_key_id}/revoke"),
        None,
        Some(cookie),
        Some(csrf),
        Some("api-key-http-revoke-0001"),
        Some(stale_etag),
    )
    .await;
    assert_eq!(duplicate.status(), StatusCode::CONFLICT);
}

async fn get_key(app: &Router, cookie: &str, api_key_id: &str) -> Response<Body> {
    send(
        app,
        Method::GET,
        &format!("/api/v1/api-keys/{api_key_id}"),
        None,
        Some(cookie),
        None,
        None,
        None,
    )
    .await
}

async fn assert_draft_target(app: &Router, cookie: &str, draft_id: &str, model_id: &str) {
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
