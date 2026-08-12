use super::*;

type AttemptFact = (
    Uuid,
    i16,
    Uuid,
    String,
    String,
    bool,
    Option<String>,
    bool,
    bool,
);

pub(super) async fn exercise(
    store: &PgStore,
    owner_id: Uuid,
    first_provider_id: Uuid,
    api_key_id: Uuid,
    generation_id: Uuid,
) {
    let second_provider_id = Uuid::now_v7();
    sqlx::query(
        "INSERT INTO providers
         (id, name, kind, state, auth_mode, etag, created_by,
          last_probe_at, last_probe_status, last_probe_detail)
         VALUES ($1, 'attempt-accounting-provider', 'openai', 'active', 'api_key', $2, $3,
                 now(), 'succeeded', 'mock probe succeeded')",
    )
    .bind(second_provider_id)
    .bind(Uuid::now_v7())
    .bind(owner_id)
    .execute(store.pool())
    .await
    .unwrap();

    let observed_at = Utc::now() - Duration::minutes(30);
    let route_slug = "attempt-attribution";
    let first_request_id = Uuid::now_v7();
    let first = event(
        first_request_id,
        generation_id,
        api_key_id,
        route_slug,
        observed_at,
        second_provider_id,
        "mock-model",
        Some(10),
        Some(5),
        true,
        vec![
            attempt(
                1,
                first_provider_id,
                "mock-model",
                observed_at - Duration::milliseconds(20),
                Some(503),
                Some("connect"),
                false,
                RequestAttemptUsageMetadata {
                    observed: false,
                    complete: true,
                    billing_uncertain: false,
                    input_tokens: None,
                    output_tokens: None,
                    cached_input_tokens: None,
                    media_units: None,
                },
            ),
            attempt(
                2,
                second_provider_id,
                "mock-model",
                observed_at - Duration::milliseconds(10),
                Some(200),
                None,
                true,
                complete_usage(10, 5),
            ),
        ],
    );
    assert_eq!(
        store.persist_request_metadata_event(&first).await.unwrap(),
        RequestMetadataPersistenceOutcome::Persisted
    );

    let uncertain_request_id = Uuid::now_v7();
    let uncertain_observed_at = observed_at + Duration::seconds(1);
    let uncertain = event(
        uncertain_request_id,
        generation_id,
        api_key_id,
        route_slug,
        uncertain_observed_at,
        second_provider_id,
        "mock-model",
        Some(20),
        Some(10),
        true,
        vec![
            attempt(
                1,
                first_provider_id,
                "mock-model",
                uncertain_observed_at - Duration::milliseconds(20),
                Some(504),
                Some("timeout"),
                true,
                uncertain_usage(),
            ),
            attempt(
                2,
                second_provider_id,
                "mock-model",
                uncertain_observed_at - Duration::milliseconds(10),
                Some(200),
                None,
                true,
                complete_usage(20, 10),
            ),
        ],
    );
    assert_eq!(
        store
            .persist_request_metadata_event(&uncertain)
            .await
            .unwrap(),
        RequestMetadataPersistenceOutcome::Persisted
    );
    assert_eq!(
        store
            .persist_request_metadata_event(&uncertain)
            .await
            .unwrap(),
        RequestMetadataPersistenceOutcome::Duplicate
    );

    let partial_request_id = Uuid::now_v7();
    let partial_observed_at = observed_at + Duration::seconds(2);
    let partial = event(
        partial_request_id,
        generation_id,
        api_key_id,
        route_slug,
        partial_observed_at,
        second_provider_id,
        "mock-model",
        Some(7),
        None,
        false,
        vec![attempt(
            1,
            second_provider_id,
            "mock-model",
            partial_observed_at - Duration::milliseconds(10),
            Some(200),
            None,
            true,
            RequestAttemptUsageMetadata {
                observed: true,
                complete: false,
                billing_uncertain: false,
                input_tokens: Some(7),
                output_tokens: None,
                cached_input_tokens: None,
                media_units: None,
            },
        )],
    );
    store
        .persist_request_metadata_event(&partial)
        .await
        .unwrap();

    let unpriced_request_id = Uuid::now_v7();
    let unpriced_observed_at = observed_at + Duration::seconds(3);
    let unpriced = event(
        unpriced_request_id,
        generation_id,
        api_key_id,
        route_slug,
        unpriced_observed_at,
        second_provider_id,
        "unpriced-attempt-model",
        Some(1),
        Some(1),
        true,
        vec![attempt(
            1,
            second_provider_id,
            "unpriced-attempt-model",
            unpriced_observed_at - Duration::milliseconds(10),
            Some(200),
            None,
            true,
            complete_usage(1, 1),
        )],
    );
    store
        .persist_request_metadata_event(&unpriced)
        .await
        .unwrap();

    let cancelled_request_id = Uuid::now_v7();
    let cancelled_observed_at = observed_at + Duration::seconds(4);
    let cancelled = event(
        cancelled_request_id,
        generation_id,
        api_key_id,
        route_slug,
        cancelled_observed_at,
        first_provider_id,
        "mock-model",
        None,
        None,
        false,
        vec![attempt(
            1,
            first_provider_id,
            "mock-model",
            cancelled_observed_at - Duration::milliseconds(10),
            None,
            Some("client_cancelled"),
            true,
            uncertain_usage(),
        )],
    );
    store
        .persist_request_metadata_event(&cancelled)
        .await
        .unwrap();

    let facts: Vec<AttemptFact> = sqlx::query_as(
        "SELECT request_id, attempt_ordinal, provider_id, upstream_model,
                    charge_status::text, usage_observed, estimated_cost::text,
                    unpriced, pricing_revision_id IS NOT NULL
               FROM attempt_usage_facts
              WHERE route_slug = $1
              ORDER BY observed_at, attempt_ordinal",
    )
    .bind(route_slug)
    .fetch_all(store.pool())
    .await
    .unwrap();
    assert_eq!(facts.len(), 7);
    assert_eq!(facts[0].4, "not_billable");
    assert!(!facts[0].5);
    assert_eq!(facts[1].2, second_provider_id);
    assert_eq!(facts[1].4, "billable");
    assert_eq!(facts[1].6.as_deref(), Some("0.000020000000"));
    assert!(
        facts[1].8,
        "the resolved pricing revision must be immutable"
    );
    assert_eq!(facts[2].4, "billing_uncertain");
    assert!(facts[2].7, "incomplete usage must remain unpriced");
    assert_eq!(facts[3].6.as_deref(), Some("0.000040000000"));
    assert_eq!(facts[4].4, "billable");
    assert_eq!(facts[4].6, None);
    assert!(facts[4].7, "partial usage must remain unpriced");
    assert!(facts[4].8, "partial usage still resolves available pricing");
    assert_eq!(facts[5].3, "unpriced-attempt-model");
    assert!(facts[5].7);
    assert!(!facts[5].8);
    assert_eq!(facts[6].0, cancelled_request_id);
    assert_eq!(facts[6].4, "billing_uncertain");

    let partial_fact: (String, bool, Option<String>, bool, bool) = sqlx::query_as(
        "SELECT charge_status::text, usage_complete, estimated_cost::text, unpriced,
                request_unpriced_counted
           FROM attempt_usage_facts
          WHERE request_id = $1",
    )
    .bind(partial_request_id)
    .fetch_one(store.pool())
    .await
    .unwrap();
    assert_eq!(partial_fact.0, "billable");
    assert!(!partial_fact.1);
    assert_eq!(partial_fact.2, None);
    assert!(partial_fact.3);
    assert!(
        partial_fact.4,
        "partial usage must mark its request unpriced"
    );

    let compatibility_rows: i64 =
        sqlx::query_scalar("SELECT count(*) FROM usage_facts WHERE request_id = $1")
            .bind(uncertain_request_id)
            .fetch_one(store.pool())
            .await
            .unwrap();
    assert_eq!(
        compatibility_rows, 0,
        "two potentially billable targets cannot be represented by one compatibility attribution"
    );

    let filters = UsageFilters {
        observed_after: observed_at - Duration::seconds(1),
        observed_before: observed_at + Duration::minutes(1),
        route_slug: Some(route_slug.to_owned()),
        provider_id: None,
        upstream_model: None,
        api_key_id: None,
        operation: None,
    };
    let summary = store.usage_summary(&filters).await.unwrap();
    assert_eq!(summary.request_count, 5);
    assert_eq!(summary.input_tokens, "38");
    assert_eq!(summary.output_tokens, "16");
    assert_eq!(summary.estimated_cost.as_deref(), Some("0.000060000000"));
    assert_eq!(summary.unpriced_count, 4);
    assert_eq!(summary.incomplete_count, 3);

    let providers = store
        .usage_breakdown(&filters, UsageDimension::Provider, 10)
        .await
        .unwrap()
        .items;
    let first_provider = providers
        .iter()
        .find(|item| item.dimension == first_provider_id.to_string())
        .unwrap();
    let second_provider = providers
        .iter()
        .find(|item| item.dimension == second_provider_id.to_string())
        .unwrap();
    assert_eq!(first_provider.request_count, 3);
    assert_eq!(first_provider.input_tokens, "0");
    assert_eq!(first_provider.unpriced_count, 2);
    assert_eq!(first_provider.incomplete_count, 2);
    assert_eq!(second_provider.request_count, 4);
    assert_eq!(second_provider.input_tokens, "38");
    assert_eq!(
        second_provider.estimated_cost.as_deref(),
        Some("0.000060000000")
    );
    assert_eq!(second_provider.unpriced_count, 2);
    assert_eq!(second_provider.incomplete_count, 1);

    let detail = store.request_detail(uncertain_request_id).await.unwrap();
    assert_eq!(detail.request.input_tokens, Some(20));
    assert_eq!(detail.request.output_tokens, Some(10));
    assert_eq!(
        detail.request.estimated_cost.as_deref(),
        Some("0.000040000000")
    );
    assert_eq!(detail.request.usage_complete, Some(false));
    assert_eq!(detail.attempts.len(), 2);

    let mismatched_target = store
        .requests(
            &RequestFilters {
                provider_id: Some(first_provider_id),
                upstream_model: Some("unpriced-attempt-model".to_owned()),
                ..RequestFilters::default()
            },
            None,
            10,
        )
        .await
        .unwrap();
    assert!(
        mismatched_target.items.is_empty(),
        "provider and model filters must match the same attempt"
    );

    assert_legacy_fact_mirrors_all_attempts(
        store,
        generation_id,
        api_key_id,
        first_provider_id,
        second_provider_id,
    )
    .await;
}

async fn assert_legacy_fact_mirrors_all_attempts(
    store: &PgStore,
    generation_id: Uuid,
    api_key_id: Uuid,
    first_provider_id: Uuid,
    second_provider_id: Uuid,
) {
    let request_id = Uuid::now_v7();
    let event_id = Uuid::now_v7();
    let first_attempt_id = Uuid::now_v7();
    let second_attempt_id = Uuid::now_v7();
    let started_at = Utc::now() - Duration::minutes(10);
    let observed_at = started_at + Duration::milliseconds(20);

    sqlx::query(
        "INSERT INTO requests
         (id, runtime_generation_id, api_key_id, route_slug, operation, surface,
          started_at, completed_at, status_code, total_latency_ms, first_byte_ms,
          attempt_count)
         VALUES ($1, $2, $3, 'legacy-attempt-mirror', 'generation', 'openai',
                 $4, $5, 200, 20, 12, 2)",
    )
    .bind(request_id)
    .bind(generation_id)
    .bind(api_key_id)
    .bind(started_at)
    .bind(observed_at)
    .execute(store.pool())
    .await
    .unwrap();
    sqlx::query(
        "INSERT INTO attempts
         (id, request_id, request_started_at, ordinal, provider_id, upstream_model,
          started_at, completed_at, status_code, error_class, committed, latency_ms)
         VALUES
         ($1, $2, $3, 1, $4, 'legacy-first-model', $3, $3 + interval '5 milliseconds',
          504, 'timeout', false, 5),
         ($5, $2, $3, 2, $6, 'legacy-final-model', $3 + interval '10 milliseconds',
          $3 + interval '15 milliseconds', 200, NULL, true, 5)",
    )
    .bind(first_attempt_id)
    .bind(request_id)
    .bind(started_at)
    .bind(first_provider_id)
    .bind(second_attempt_id)
    .bind(second_provider_id)
    .execute(store.pool())
    .await
    .unwrap();
    sqlx::query(
        "INSERT INTO usage_request_anchors (request_id, request_started_at) VALUES ($1, $2)",
    )
    .bind(request_id)
    .bind(started_at)
    .execute(store.pool())
    .await
    .unwrap();
    sqlx::query(
        "INSERT INTO usage_facts
         (id, request_id, request_started_at, api_key_id, provider_id, route_slug,
          upstream_model, operation, surface, observed_at, input_tokens, output_tokens,
          unpriced, usage_complete)
         VALUES ($1, $2, $3, $4, $5, 'legacy-attempt-mirror', 'legacy-final-model',
                 'generation', 'openai', $6, 3, 1, true, true)",
    )
    .bind(event_id)
    .bind(request_id)
    .bind(started_at)
    .bind(api_key_id)
    .bind(second_provider_id)
    .bind(observed_at)
    .execute(store.pool())
    .await
    .unwrap();

    let mirrored: Vec<(i16, Uuid, String, bool)> = sqlx::query_as(
        "SELECT attempt_ordinal, provider_id, charge_status::text, usage_observed
           FROM attempt_usage_facts WHERE request_id = $1 ORDER BY attempt_ordinal",
    )
    .bind(request_id)
    .fetch_all(store.pool())
    .await
    .unwrap();
    assert_eq!(
        mirrored,
        vec![
            (1, first_provider_id, "billing_uncertain".to_owned(), false),
            (2, second_provider_id, "billable".to_owned(), true),
        ],
        "an N-1 request-level fact must reconstruct every retained attempt"
    );

    let mut transaction = store.pool().begin().await.unwrap();
    sqlx::query("SELECT set_config('olp.usage_rollup_writer', 'additive-v2', true)")
        .execute(&mut *transaction)
        .await
        .unwrap();
    sqlx::query(
        "WITH expired AS (
             DELETE FROM usage_facts WHERE id = $1 RETURNING *
         )
         INSERT INTO usage_hourly
         (bucket, route_slug, provider_id, upstream_model, operation, surface, api_key_id,
          request_count, input_tokens, output_tokens, cached_input_tokens, media_units,
          estimated_cost, unpriced_count, incomplete_count, currency)
         SELECT date_trunc('hour', observed_at), route_slug, provider_id, upstream_model,
                operation, surface, api_key_id, 1, COALESCE(input_tokens, 0),
                COALESCE(output_tokens, 0), COALESCE(cached_input_tokens, 0),
                COALESCE(media_units, 0), estimated_cost, unpriced::int,
                (NOT usage_complete)::int, currency
           FROM expired",
    )
    .bind(event_id)
    .execute(&mut *transaction)
    .await
    .unwrap();

    let archived: (i64, i64, String) = sqlx::query_as(
        "SELECT COALESCE(sum(request_count), 0)::bigint,
                COALESCE(sum(provider_request_count), 0)::bigint,
                COALESCE(sum(input_tokens), 0)::text
           FROM attempt_usage_hourly WHERE route_slug = 'legacy-attempt-mirror'",
    )
    .fetch_one(&mut *transaction)
    .await
    .unwrap();
    assert_eq!(
        archived,
        (1, 2, "3".to_owned()),
        "N-1 retention must archive attempt facts once without collapsing providers"
    );
    transaction.rollback().await.unwrap();
}

#[allow(clippy::too_many_arguments)]
fn event(
    request_id: Uuid,
    runtime_generation_id: Uuid,
    api_key_id: Uuid,
    route_slug: &str,
    observed_at: chrono::DateTime<Utc>,
    final_provider_id: Uuid,
    final_model: &str,
    input_tokens: Option<i64>,
    output_tokens: Option<i64>,
    usage_complete: bool,
    attempts: Vec<RequestAttemptMetadata>,
) -> RequestMetadataEvent {
    let request_started_at = attempts
        .first()
        .map_or(observed_at, |attempt| attempt.started_at);
    RequestMetadataEvent {
        event_id: Uuid::now_v7(),
        request_id,
        runtime_generation_id,
        api_key_id,
        provider_id: Some(final_provider_id),
        route_slug: route_slug.to_owned(),
        upstream_model: Some(final_model.to_owned()),
        operation: olp_engine::domain::OperationKind::Generation,
        surface: Surface::OpenAi,
        request_started_at,
        request_completed_at: observed_at,
        observed_at,
        status_code: attempts.last().and_then(|attempt| attempt.status_code),
        error_class: attempts
            .last()
            .and_then(|attempt| attempt.error_class.clone()),
        committed: attempts.last().is_some_and(|attempt| attempt.committed),
        latency_ms: 20,
        first_byte_ms: attempts.last().and_then(|attempt| attempt.first_byte_ms),
        input_tokens,
        output_tokens,
        cached_input_tokens: None,
        media_units: None,
        usage_complete,
        unpriced: true,
        attempts,
    }
}

#[allow(clippy::too_many_arguments)]
fn attempt(
    ordinal: u16,
    provider_id: Uuid,
    upstream_model: &str,
    completed_at: chrono::DateTime<Utc>,
    status_code: Option<u16>,
    error_class: Option<&str>,
    committed: bool,
    usage: RequestAttemptUsageMetadata,
) -> RequestAttemptMetadata {
    RequestAttemptMetadata {
        id: Uuid::now_v7(),
        ordinal,
        provider_id,
        upstream_model: upstream_model.to_owned(),
        started_at: completed_at - Duration::milliseconds(5),
        completed_at,
        status_code,
        error_class: error_class.map(str::to_owned),
        committed,
        latency_ms: 5,
        first_byte_ms: committed.then_some(2),
        usage: Some(usage),
    }
}

fn complete_usage(input_tokens: i64, output_tokens: i64) -> RequestAttemptUsageMetadata {
    RequestAttemptUsageMetadata {
        observed: true,
        complete: true,
        billing_uncertain: false,
        input_tokens: Some(input_tokens),
        output_tokens: Some(output_tokens),
        cached_input_tokens: None,
        media_units: None,
    }
}

fn uncertain_usage() -> RequestAttemptUsageMetadata {
    RequestAttemptUsageMetadata {
        observed: false,
        complete: false,
        billing_uncertain: true,
        input_tokens: None,
        output_tokens: None,
        cached_input_tokens: None,
        media_units: None,
    }
}
