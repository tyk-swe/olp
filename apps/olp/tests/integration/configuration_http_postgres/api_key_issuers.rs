use super::*;

pub(super) async fn verify(app: &Router, store: &olp_db::store::Store, cookie: &str) {
    let all_before = response_json(list(app, cookie, "limit=100").await).await;
    let issuer = Uuid::now_v7();
    sqlx::query("INSERT INTO users (id, email, display_name, role, active) VALUES ($1, 'inactive-issuer@configuration.test', 'Inactive issuer', 'developer', false)")
        .bind(issuer).execute(store.pool()).await.unwrap();
    let mut ids: Vec<Uuid> = (0..2).map(|_| Uuid::now_v7()).collect();
    ids.sort();
    for (index, id) in ids.iter().enumerate() {
        sqlx::query("INSERT INTO api_keys (id, lookup_id, secret_digest, name, created_by, daily_cost_limit) VALUES ($1, $2, $3, 'Inactive issuer key', $4, 1.25)")
            .bind(id).bind(format!("http_issuer_{index}")).bind(vec![0u8; 32])
            .bind(issuer).execute(store.pool()).await.unwrap();
    }
    let first = list(app, cookie, &format!("created_by={issuer}&limit=1")).await;
    assert_eq!(first.status(), StatusCode::OK);
    let first = response_json(first).await;
    assert_eq!(first["items"].as_array().unwrap().len(), 1);
    assert_eq!(first["items"][0]["id"], ids[0].to_string());
    assert_eq!(first["items"][0]["created_by"], issuer.to_string());
    assert_eq!(
        first["items"][0]["created_by_email"],
        "inactive-issuer@configuration.test"
    );
    assert_eq!(first["items"][0]["budget"]["daily"]["limit"], "1.25");
    assert!(first["items"][0].get("secret").is_none());
    assert_eq!(first["next_cursor"], ids[0].to_string());
    let second = list(
        app,
        cookie,
        &format!("created_by={issuer}&limit=1&cursor={}", ids[0]),
    )
    .await;
    assert_eq!(second.status(), StatusCode::OK);
    let second = response_json(second).await;
    assert_eq!(second["items"].as_array().unwrap().len(), 1);
    assert_eq!(second["items"][0]["id"], ids[1].to_string());
    assert!(second["next_cursor"].is_null());
    let missing = list(app, cookie, &format!("created_by={}", Uuid::now_v7())).await;
    assert_eq!(missing.status(), StatusCode::OK);
    assert_eq!(response_json(missing).await["items"], json!([]));
    for invalid in [
        "created_by=not-a-uuid",
        "created_by=",
        "limit=0",
        "cursor=bad",
    ] {
        assert_eq!(
            list(app, cookie, invalid).await.status(),
            StatusCode::BAD_REQUEST
        );
    }
    let all_after = response_json(list(app, cookie, "limit=100").await).await;
    assert_eq!(
        all_after["items"].as_array().unwrap().len(),
        all_before["items"].as_array().unwrap().len() + 2
    );
    for original in all_before["items"].as_array().unwrap() {
        assert!(
            all_after["items"]
                .as_array()
                .unwrap()
                .iter()
                .any(|key| key["id"] == original["id"])
        );
    }
}

async fn list(app: &Router, cookie: &str, query: &str) -> Response<Body> {
    send(
        app,
        Method::GET,
        &format!("/api/v1/api-keys?{query}"),
        None,
        Some(cookie),
        None,
        None,
        None,
    )
    .await
}
