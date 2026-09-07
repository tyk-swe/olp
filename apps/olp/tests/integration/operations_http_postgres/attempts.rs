use olp_db::store::Store;
use olp_engine::inference::request_metadata::RequestAttemptUsageMetadata;

use super::*;

pub(super) async fn exercise(
    store: &Store,
    app: &Router,
    cookie: &str,
    generation: Uuid,
    provider: Uuid,
    key: Uuid,
) {
    let observed = Utc::now() - Duration::hours(1);
    let event = two_charge_event(generation, provider, key, observed);
    store.persist_request_metadata_event(&event).await.unwrap();
    let path = format!("/api/v1/requests/{}", event.request_id);
    let response = get(app, &path, cookie).await;
    assert_eq!(response.status(), StatusCode::OK);
    let detail = response_json(response).await;
    assert_eq!(detail["estimated_cost"], "0.000030000000");
    assert_eq!(detail["input_tokens"], 14);
    assert_eq!(detail["output_tokens"], 8);
    assert_eq!(detail["cached_input_tokens"], 2);
    let attempts = detail["attempts"].as_array().unwrap();
    assert_eq!(attempts.len(), 2);
    assert_eq!(attempts[0]["estimated_cost"], "0.000020000000");
    assert_eq!(attempts[1]["estimated_cost"], "0.000010000000");
    assert_eq!(attempts[0]["input_tokens"], 10);
    assert_eq!(attempts[0]["output_tokens"], 5);
    assert_eq!(attempts[0]["cached_input_tokens"], 2);
    assert_eq!(attempts[1]["cached_input_tokens"], 0);
    for (index, attempt) in attempts.iter().enumerate() {
        assert_eq!(attempt["id"], event.attempts[index].id.to_string());
        assert_eq!(attempt["charge_status"], "billable");
        assert_eq!(attempt["usage_observed"], true);
        assert_eq!(attempt["usage_complete"], true);
        assert_eq!(attempt["unpriced"], false);
        assert_eq!(attempt["currency"], "USD");
        assert!(attempt["pricing_revision_id"].as_str().is_some());
        assert_eq!(attempt.get("media_units"), Some(&Value::Null));
        for field in ["prompt", "output", "body", "headers", "credential"] {
            assert!(attempt.get(field).is_none());
        }
    }
    let expected_revision: Uuid =
        sqlx::query_scalar("SELECT id FROM pricing_revisions WHERE revision = 1")
            .fetch_one(store.pool())
            .await
            .unwrap();
    assert_eq!(
        attempts[0]["pricing_revision_id"],
        expected_revision.to_string()
    );
    assert_eq!(
        attempts[1]["pricing_revision_id"],
        expected_revision.to_string()
    );

    assert_retained_attempt_without_facts(store, app, cookie, &event).await;
    let unauthenticated = app
        .clone()
        .oneshot(Request::get(path).body(Body::empty()).unwrap())
        .await
        .unwrap();
    assert_eq!(unauthenticated.status(), StatusCode::UNAUTHORIZED);
}

fn two_charge_event(
    generation: Uuid,
    provider: Uuid,
    key: Uuid,
    observed: chrono::DateTime<Utc>,
) -> Event {
    let started = observed - Duration::milliseconds(20);
    Event {
        event_id: Uuid::now_v7(),
        request_id: Uuid::now_v7(),
        runtime_generation_id: generation,
        api_key_id: key,
        provider_id: Some(provider),
        route_slug: "two-charge-http".to_owned(),
        upstream_model: Some("mock-model".to_owned()),
        operation: "generation".parse().unwrap(),
        surface: Surface::OpenAi,
        request_started_at: started,
        request_completed_at: observed,
        observed_at: observed,
        status_code: Some(200),
        error_class: None,
        committed: true,
        latency_ms: 20,
        first_byte_ms: Some(15),
        input_tokens: Some(4),
        output_tokens: Some(3),
        cached_input_tokens: Some(0),
        media_units: None,
        usage_complete: true,
        unpriced: false,
        attempts: [(10, 5, 2), (4, 3, 0)]
            .into_iter()
            .enumerate()
            .map(|(index, (input, output, cached))| RequestAttemptMetadata {
                id: Uuid::now_v7(),
                ordinal: u16::try_from(index + 1).unwrap(),
                provider_id: provider,
                upstream_model: "mock-model".to_owned(),
                started_at: started + Duration::milliseconds(i64::try_from(index).unwrap() * 10),
                completed_at: started
                    + Duration::milliseconds(i64::try_from(index + 1).unwrap() * 10),
                status_code: Some(if index == 0 { 503 } else { 200 }),
                error_class: (index == 0).then(|| "upstream_http".to_owned()),
                committed: index == 1,
                latency_ms: 10,
                first_byte_ms: (index == 1).then_some(5),
                usage: Some(RequestAttemptUsageMetadata {
                    observed: true,
                    complete: true,
                    billing_uncertain: false,
                    input_tokens: Some(input),
                    output_tokens: Some(output),
                    cached_input_tokens: Some(cached),
                    media_units: None,
                }),
            })
            .collect(),
    }
}

async fn assert_retained_attempt_without_facts(
    store: &Store,
    app: &Router,
    cookie: &str,
    event: &Event,
) {
    sqlx::query("DELETE FROM attempt_usage_facts WHERE attempt_id = $1")
        .bind(event.attempts[0].id)
        .execute(store.pool())
        .await
        .unwrap();
    let response = get(
        app,
        &format!("/api/v1/requests/{}", event.request_id),
        cookie,
    )
    .await;
    assert_eq!(response.status(), StatusCode::OK);
    let detail = response_json(response).await;
    assert_eq!(detail["attempts"].as_array().unwrap().len(), 2);
    let retained = &detail["attempts"][0];
    assert_eq!(retained["id"], event.attempts[0].id.to_string());
    assert_eq!(retained["status_code"], 503);
    for field in [
        "charge_status",
        "usage_observed",
        "usage_complete",
        "input_tokens",
        "output_tokens",
        "cached_input_tokens",
        "media_units",
        "estimated_cost",
        "currency",
        "unpriced",
        "pricing_revision_id",
    ] {
        assert_eq!(
            retained.get(field),
            Some(&Value::Null),
            "missing fact field {field}"
        );
    }
    assert_eq!(detail["attempts"][1]["estimated_cost"], "0.000010000000");
}
