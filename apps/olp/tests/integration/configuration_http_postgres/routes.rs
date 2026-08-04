use super::*;

pub(super) async fn exercise(
    app: &Router,
    cookie: &str,
    csrf: &str,
    provider_id: &str,
    model_id: &str,
) -> String {
    let route = send(
        app,
        Method::POST,
        "/api/v1/route-drafts",
        Some(json!({
            "slug": "default",
            "operations": ["embeddings"],
            "overall_timeout_ms": 30000,
            "max_attempts": 1,
            "targets": [{
                "provider_id": provider_id,
                "provider_model": "compatible-model",
                "priority": 0,
                "weight": 1,
                "timeout_ms": 20000
            }]
        })),
        Some(cookie),
        Some(csrf),
        Some("route-http-create-0001"),
        None,
    )
    .await;
    assert_eq!(route.status(), StatusCode::CREATED);
    let mut route_etag = etag(&route);
    let route_body = response_json(route).await;
    let draft_id = route_body["id"].as_str().unwrap().to_owned();
    let route_replay = send(
        app,
        Method::POST,
        "/api/v1/route-drafts",
        Some(json!({
            "slug": "default",
            "operations": ["embeddings"],
            "overall_timeout_ms": 30000,
            "max_attempts": 1,
            "targets": [{
                "provider_id": provider_id,
                "provider_model": "compatible-model",
                "priority": 0,
                "weight": 1,
                "timeout_ms": 20000
            }]
        })),
        Some(cookie),
        Some(csrf),
        Some("route-http-create-0001"),
        None,
    )
    .await;
    assert_eq!(route_replay.status(), StatusCode::CREATED);
    assert_eq!(etag(&route_replay), route_etag);
    assert_eq!(response_json(route_replay).await, route_body);
    let route_mismatch = send(
        app,
        Method::POST,
        "/api/v1/route-drafts",
        Some(json!({
            "slug": "default",
            "operations": ["embeddings"],
            "overall_timeout_ms": 31000,
            "max_attempts": 1,
            "targets": [{
                "provider_id": provider_id,
                "provider_model": "compatible-model",
                "priority": 0,
                "weight": 1,
                "timeout_ms": 20000
            }]
        })),
        Some(cookie),
        Some(csrf),
        Some("route-http-create-0001"),
        None,
    )
    .await;
    assert_eq!(route_mismatch.status(), StatusCode::CONFLICT);

    let route_update = send(
        app,
        Method::PUT,
        &format!("/api/v1/route-drafts/{draft_id}"),
        Some(json!({
            "slug": "default",
            "operations": ["embeddings"],
            "overall_timeout_ms": 35000,
            "max_attempts": 1,
            "targets": [{
                "provider_model_id": model_id,
                "priority": 0,
                "weight": 5,
                "timeout_ms": 22000
            }]
        })),
        Some(cookie),
        Some(csrf),
        None,
        Some(&route_etag),
    )
    .await;
    assert_eq!(route_update.status(), StatusCode::OK);
    route_etag = etag(&route_update);

    let simulation = send(
        app,
        Method::POST,
        &format!("/api/v1/route-drafts/{draft_id}/simulate"),
        Some(json!({
            "operation": "embeddings",
            "surface": "openai",
            "mode": "unary",
            "seed": "sdk-request-1"
        })),
        Some(cookie),
        Some(csrf),
        None,
        None,
    )
    .await;
    assert_eq!(simulation.status(), StatusCode::OK);
    assert_eq!(response_json(simulation).await["targets"][0]["attempt"], 1);

    let validation = send(
        app,
        Method::POST,
        &format!("/api/v1/route-drafts/{draft_id}/validate"),
        None,
        Some(cookie),
        Some(csrf),
        None,
        Some(&route_etag),
    )
    .await;
    assert_eq!(validation.status(), StatusCode::OK);
    let validated_etag = etag(&validation);
    let activation = send(
        app,
        Method::POST,
        &format!("/api/v1/route-drafts/{draft_id}/activate"),
        None,
        Some(cookie),
        Some(csrf),
        Some("route-http-activate-0001"),
        Some(&validated_etag),
    )
    .await;
    assert_eq!(activation.status(), StatusCode::OK);
    let activation_body = response_json(activation).await;
    let route_id = activation_body["route_id"].as_str().unwrap();
    let first_revision_id = activation_body["revision_id"].as_str().unwrap();

    let revisions = send(
        app,
        Method::GET,
        &format!("/api/v1/routes/{route_id}/revisions"),
        None,
        Some(cookie),
        None,
        None,
        None,
    )
    .await;
    assert_eq!(revisions.status(), StatusCode::OK);
    assert_eq!(response_json(revisions).await["items"][0]["revision"], 1);

    let routes = send(
        app,
        Method::GET,
        "/api/v1/routes?limit=1",
        None,
        Some(cookie),
        None,
        None,
        None,
    )
    .await;
    assert_eq!(routes.status(), StatusCode::OK);
    let routes_body = response_json(routes).await;
    assert_eq!(routes_body["items"][0]["id"], route_id);
    assert_eq!(routes_body["items"][0]["latest_revision"]["revision"], 1);
    assert_eq!(routes_body["items"][0]["revision_count"], 1);
    let route_detail = send(
        app,
        Method::GET,
        &format!("/api/v1/routes/{route_id}"),
        None,
        Some(cookie),
        None,
        None,
        None,
    )
    .await;
    assert_eq!(route_detail.status(), StatusCode::OK);
    assert_eq!(response_json(route_detail).await["slug"], "default");

    let active_draft = send(
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
    let active_draft_etag = etag(&active_draft);
    let second_draft = send(
        app,
        Method::PUT,
        &format!("/api/v1/route-drafts/{draft_id}"),
        Some(json!({
            "slug": "default",
            "operations": ["embeddings"],
            "overall_timeout_ms": 45000,
            "max_attempts": 1,
            "targets": [{
                "provider_model_id": model_id,
                "priority": 0,
                "weight": 9,
                "timeout_ms": 25000
            }]
        })),
        Some(cookie),
        Some(csrf),
        None,
        Some(&active_draft_etag),
    )
    .await;
    let second_draft_etag = etag(&second_draft);
    let second_validation = send(
        app,
        Method::POST,
        &format!("/api/v1/route-drafts/{draft_id}/validate"),
        None,
        Some(cookie),
        Some(csrf),
        None,
        Some(&second_draft_etag),
    )
    .await;
    let second_validated_etag = etag(&second_validation);
    let second_activation = send(
        app,
        Method::POST,
        &format!("/api/v1/route-drafts/{draft_id}/activate"),
        None,
        Some(cookie),
        Some(csrf),
        Some("route-http-activate-0002"),
        Some(&second_validated_etag),
    )
    .await;
    assert_eq!(second_activation.status(), StatusCode::OK);
    let second_activation_body = response_json(second_activation).await;
    let second_revision_id = second_activation_body["revision_id"].as_str().unwrap();
    let revision_page = send(
        app,
        Method::GET,
        &format!("/api/v1/routes/{route_id}/revisions?limit=1"),
        None,
        Some(cookie),
        None,
        None,
        None,
    )
    .await;
    assert_eq!(revision_page.status(), StatusCode::OK);
    let revision_page = response_json(revision_page).await;
    assert_eq!(revision_page["items"].as_array().unwrap().len(), 1);
    assert_eq!(revision_page["items"][0]["revision"], 2);
    let revision_cursor = revision_page["next_cursor"].as_str().unwrap();
    let older_revisions = send(
        app,
        Method::GET,
        &format!("/api/v1/routes/{route_id}/revisions?limit=1&cursor={revision_cursor}"),
        None,
        Some(cookie),
        None,
        None,
        None,
    )
    .await;
    assert_eq!(older_revisions.status(), StatusCode::OK);
    let older_revisions = response_json(older_revisions).await;
    assert_eq!(older_revisions["items"].as_array().unwrap().len(), 1);
    assert_eq!(older_revisions["items"][0]["revision"], 1);
    assert!(older_revisions["next_cursor"].is_null());
    let revision_diff = send(
        app,
        Method::GET,
        &format!(
            "/api/v1/routes/{route_id}/revisions/diff?from={first_revision_id}&to={second_revision_id}"
        ),
        None,
        Some(cookie),
        None,
        None,
        None,
    )
    .await;
    assert_eq!(revision_diff.status(), StatusCode::OK);
    assert_eq!(response_json(revision_diff).await["timeout_changed"], true);
    let restored = send(
        app,
        Method::POST,
        &format!("/api/v1/routes/{route_id}/revisions/{first_revision_id}/restore-as-draft"),
        None,
        Some(cookie),
        Some(csrf),
        Some("route-http-restore-0001"),
        None,
    )
    .await;
    assert_eq!(restored.status(), StatusCode::CREATED);
    let restored_etag = etag(&restored);
    let restored_id = response_json(restored).await["id"]
        .as_str()
        .unwrap()
        .to_owned();
    let deleted_restored = send(
        app,
        Method::DELETE,
        &format!("/api/v1/route-drafts/{restored_id}"),
        None,
        Some(cookie),
        Some(csrf),
        None,
        Some(&restored_etag),
    )
    .await;
    assert_eq!(deleted_restored.status(), StatusCode::NO_CONTENT);

    draft_id
}
