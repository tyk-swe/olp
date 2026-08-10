use super::*;

pub(super) async fn exercise(app: &Router, cookie: &str, csrf: &str, owner_id: &str) {
    let team = send(
        app,
        Method::POST,
        "/api/v1/teams",
        Some(json!({"name": "Platform"})),
        Some(cookie),
        Some(csrf),
        Some("scoped-team-create-0001"),
        None,
    )
    .await;
    assert_eq!(team.status(), StatusCode::CREATED);
    let team_etag = etag(&team);
    let team_body = response_json(team).await;
    let team_id = team_body["id"].as_str().unwrap().to_owned();

    let team_replay = send(
        app,
        Method::POST,
        "/api/v1/teams",
        Some(json!({"name": "Platform"})),
        Some(cookie),
        Some(csrf),
        Some("scoped-team-create-0001"),
        None,
    )
    .await;
    assert_eq!(team_replay.status(), StatusCode::CREATED);
    assert_eq!(etag(&team_replay), team_etag);
    assert_eq!(response_json(team_replay).await, team_body);

    let team_replay_mismatch = send(
        app,
        Method::POST,
        "/api/v1/teams",
        Some(json!({"name": "Different platform"})),
        Some(cookie),
        Some(csrf),
        Some("scoped-team-create-0001"),
        None,
    )
    .await;
    assert_eq!(team_replay_mismatch.status(), StatusCode::CONFLICT);

    let updated_team = send(
        app,
        Method::PATCH,
        &format!("/api/v1/teams/{team_id}"),
        Some(json!({"name": "Platform engineering"})),
        Some(cookie),
        Some(csrf),
        Some("scoped-team-update-0001"),
        Some(&team_etag),
    )
    .await;
    assert_eq!(updated_team.status(), StatusCode::OK);
    let updated_team_etag = etag(&updated_team);
    assert_eq!(
        response_json(updated_team).await["team"]["name"],
        "Platform engineering"
    );

    let stale_team_update = send(
        app,
        Method::PATCH,
        &format!("/api/v1/teams/{team_id}"),
        Some(json!({"name": "Stale name"})),
        Some(cookie),
        Some(csrf),
        Some("scoped-team-update-0002"),
        Some(&team_etag),
    )
    .await;
    assert_eq!(stale_team_update.status(), StatusCode::PRECONDITION_FAILED);

    let team_members = send(
        app,
        Method::GET,
        &format!("/api/v1/teams/{team_id}/members"),
        None,
        Some(cookie),
        None,
        None,
        None,
    )
    .await;
    assert_eq!(team_members.status(), StatusCode::OK);
    let membership = &response_json(team_members).await["items"][0];
    assert_eq!(membership["user_id"], owner_id);
    assert_eq!(membership["role"], "admin");
    let membership_etag = format!("\"{}\"", membership["etag"].as_str().unwrap());

    let put_membership = send(
        app,
        Method::PUT,
        &format!("/api/v1/teams/{team_id}/members/{owner_id}"),
        Some(json!({"role": "admin"})),
        Some(cookie),
        Some(csrf),
        Some("scoped-team-membership-put-0001"),
        Some(&membership_etag),
    )
    .await;
    assert_eq!(put_membership.status(), StatusCode::OK);

    let project = send(
        app,
        Method::POST,
        "/api/v1/projects",
        Some(json!({"team_id": team_id, "name": "Gateway"})),
        Some(cookie),
        Some(csrf),
        Some("scoped-project-create-0001"),
        None,
    )
    .await;
    assert_eq!(project.status(), StatusCode::CREATED);
    let project_body = response_json(project).await;
    let project_id = project_body["id"].as_str().unwrap().to_owned();

    let account = send(
        app,
        Method::POST,
        "/api/v1/service-accounts",
        Some(json!({
            "team_id": team_id,
            "project_id": project_id,
            "name": "Gateway deployer"
        })),
        Some(cookie),
        Some(csrf),
        Some("scoped-service-account-create-0001"),
        None,
    )
    .await;
    assert_eq!(account.status(), StatusCode::CREATED);
    let account_body = response_json(account).await;
    let account_id = account_body["id"].as_str().unwrap().to_owned();

    let key = send(
        app,
        Method::POST,
        "/api/v1/api-keys",
        Some(json!({
            "name": "Gateway deployer key",
            "owner_kind": "service_account",
            "owner_id": account_id,
            "team_id": team_id,
            "project_id": project_id,
            "scopes": ["inference"],
            "allowed_routes": ["default"]
        })),
        Some(cookie),
        Some(csrf),
        Some("scoped-api-key-create-0001"),
        None,
    )
    .await;
    assert_eq!(key.status(), StatusCode::CREATED);
    let key_etag = etag(&key);
    let key_body = response_json(key).await;
    let key_id = key_body["id"].as_str().unwrap().to_owned();
    assert_eq!(key_body["owner_kind"], "service_account");
    assert_eq!(key_body["owner_id"], account_id);
    assert_eq!(key_body["team_id"], team_id);
    assert_eq!(key_body["project_id"], project_id);

    let filtered_keys = send(
        app,
        Method::GET,
        &format!(
            "/api/v1/api-keys?owner_kind=service_account&owner_id={account_id}\
             &team_id={team_id}&project_id={project_id}"
        ),
        None,
        Some(cookie),
        None,
        None,
        None,
    )
    .await;
    assert_eq!(filtered_keys.status(), StatusCode::OK);
    let filtered_body = response_json(filtered_keys).await;
    assert_eq!(filtered_body["items"].as_array().unwrap().len(), 1);
    assert_eq!(filtered_body["items"][0]["id"], key_id);

    let revoked = send(
        app,
        Method::POST,
        &format!("/api/v1/api-keys/{key_id}/revoke"),
        None,
        Some(cookie),
        Some(csrf),
        Some("scoped-api-key-revoke-0001"),
        Some(&key_etag),
    )
    .await;
    assert_eq!(revoked.status(), StatusCode::OK);
    let revoked_body = response_json(revoked).await;
    assert_eq!(revoked_body["owner_kind"], "service_account");
    assert_eq!(revoked_body["owner_id"], account_id);
    assert!(revoked_body["runtime_generation"].is_object());

    let team_detail = send(
        app,
        Method::GET,
        &format!("/api/v1/teams/{team_id}"),
        None,
        Some(cookie),
        None,
        None,
        None,
    )
    .await;
    assert_eq!(team_detail.status(), StatusCode::OK);
    assert_eq!(etag(&team_detail), updated_team_etag);
}
