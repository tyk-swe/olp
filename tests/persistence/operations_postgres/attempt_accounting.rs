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
    pool: &PgPool,
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
    .execute(pool)
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
        olp::usage::ingestion::persistence::persist_request_metadata_event(pool, &first)
            .await
            .unwrap(),
        IngestionOutcome::Persisted
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
        olp::usage::ingestion::persistence::persist_request_metadata_event(pool, &uncertain)
            .await
            .unwrap(),
        IngestionOutcome::Persisted
    );
    assert_eq!(
        olp::usage::ingestion::persistence::persist_request_metadata_event(pool, &uncertain)
            .await
            .unwrap(),
        IngestionOutcome::Duplicate
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
    olp::usage::ingestion::persistence::persist_request_metadata_event(pool, &partial)
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
    olp::usage::ingestion::persistence::persist_request_metadata_event(pool, &unpriced)
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
    olp::usage::ingestion::persistence::persist_request_metadata_event(pool, &cancelled)
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
    .fetch_all(pool)
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
    assert!(!facts[2].7, "incomplete usage is not inherently unpriced");
    assert_eq!(facts[3].6.as_deref(), Some("0.000040000000"));
    assert_eq!(facts[4].4, "billable");
    assert_eq!(facts[4].6, None);
    assert!(
        !facts[4].7,
        "partial priced usage stays priced but incomplete"
    );
    assert_eq!(facts[5].3, "unpriced-attempt-model");
    assert!(facts[5].7);
    assert!(!facts[5].8);
    assert_eq!(facts[6].0, cancelled_request_id);
    assert_eq!(facts[6].4, "billing_uncertain");

    let filters = Filters {
        observed_after: observed_at - Duration::seconds(1),
        observed_before: observed_at + Duration::minutes(1),
        route_slug: Some(route_slug.to_owned()),
        provider_id: None,
        upstream_model: None,
        api_key_id: None,
        operation: None,
    };
    let summary = olp::usage::reports::summary::usage_summary(pool, &filters)
        .await
        .unwrap();
    assert_eq!(summary.request_count, 5);
    assert_eq!(summary.input_tokens, "38");
    assert_eq!(summary.output_tokens, "16");
    assert_eq!(summary.estimated_cost.as_deref(), Some("0.000060000000"));
    assert_eq!(summary.unpriced_count, 1);
    assert_eq!(summary.incomplete_count, 3);

    let providers =
        olp::usage::reports::breakdown::usage_breakdown(pool, &filters, Dimension::Provider, 10)
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
    assert_eq!(first_provider.incomplete_count, 2);
    assert_eq!(second_provider.request_count, 4);
    assert_eq!(second_provider.input_tokens, "38");
    assert_eq!(
        second_provider.estimated_cost.as_deref(),
        Some("0.000060000000")
    );
    assert_eq!(second_provider.unpriced_count, 1);
    assert_eq!(second_provider.incomplete_count, 1);

    let detail = olp::usage::history::request_detail(pool, uncertain_request_id)
        .await
        .unwrap();
    assert_eq!(detail.request.input_tokens, Some(20));
    assert_eq!(detail.request.output_tokens, Some(10));
    assert_eq!(
        detail.request.estimated_cost.as_deref(),
        Some("0.000040000000")
    );
    assert_eq!(detail.request.usage_complete, Some(false));
    assert_eq!(detail.attempts.len(), 2);

    let mismatched_target = olp::usage::history::requests(
        pool,
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

    assert_attempt_usage_states(
        pool,
        first_request_id,
        partial_request_id,
        unpriced_request_id,
        uncertain_request_id,
    )
    .await;
    assert_two_charged_attempts(
        pool,
        generation_id,
        api_key_id,
        first_provider_id,
        second_provider_id,
    )
    .await;
    assert_media_attempt_units(pool, generation_id, api_key_id, second_provider_id).await;
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
) -> Event {
    let request_started_at = attempts
        .first()
        .map_or(observed_at, |attempt| attempt.started_at);
    Event {
        event_id: Uuid::now_v7(),
        request_id,
        runtime_generation_id,
        api_key_id,
        provider_id: Some(final_provider_id),
        route_slug: route_slug.to_owned(),
        upstream_model: Some(final_model.to_owned()),
        operation: olp::protocols::canonical::identity::OperationKind::Generation,
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
        routing: None,
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

async fn assert_attempt_usage_states(
    pool: &PgPool,
    first: Uuid,
    partial: Uuid,
    unpriced: Uuid,
    uncertain: Uuid,
) {
    let first = olp::usage::history::request_detail(pool, first)
        .await
        .unwrap();
    let not_billable = &first.attempts[0];
    assert_eq!(not_billable.charge_status.as_deref(), Some("not_billable"));
    assert_eq!(not_billable.usage_observed, Some(false));
    assert_eq!(not_billable.usage_complete, Some(true));
    assert_eq!(not_billable.unpriced, Some(false));
    assert!(not_billable.estimated_cost.is_none());
    assert!(not_billable.pricing_revision_id.is_none());
    assert!(not_billable.currency.is_none());

    let partial = olp::usage::history::request_detail(pool, partial)
        .await
        .unwrap();
    let partial = &partial.attempts[0];
    assert_eq!(partial.charge_status.as_deref(), Some("billable"));
    assert_eq!(partial.usage_observed, Some(true));
    assert_eq!(partial.usage_complete, Some(false));
    assert_eq!(partial.input_tokens, Some(7));
    assert!(partial.output_tokens.is_none());
    assert_eq!(partial.unpriced, Some(false));
    assert!(partial.estimated_cost.is_none());
    assert!(partial.pricing_revision_id.is_some());
    assert_eq!(partial.currency.as_deref(), Some("USD"));

    let unpriced = olp::usage::history::request_detail(pool, unpriced)
        .await
        .unwrap();
    let unpriced = &unpriced.attempts[0];
    assert_eq!(unpriced.charge_status.as_deref(), Some("billable"));
    assert_eq!(unpriced.usage_complete, Some(true));
    assert_eq!(unpriced.unpriced, Some(true));
    assert!(unpriced.estimated_cost.is_none());
    assert!(unpriced.pricing_revision_id.is_none());

    let uncertain = olp::usage::history::request_detail(pool, uncertain)
        .await
        .unwrap();
    let uncertain = &uncertain.attempts[0];
    assert_eq!(
        uncertain.charge_status.as_deref(),
        Some("billing_uncertain")
    );
    assert_eq!(uncertain.usage_observed, Some(false));
    assert_eq!(uncertain.usage_complete, Some(false));
    assert_eq!(uncertain.unpriced, Some(false));
    assert!(uncertain.estimated_cost.is_none());
}

async fn assert_two_charged_attempts(
    pool: &PgPool,
    generation: Uuid,
    key: Uuid,
    first_provider: Uuid,
    second_provider: Uuid,
) {
    let observed = Utc::now() - Duration::minutes(20);
    let mut first_usage = complete_usage(10, 5);
    first_usage.cached_input_tokens = Some(4);
    let mut second_usage = complete_usage(10, 5);
    second_usage.cached_input_tokens = Some(0);
    let event = event(
        Uuid::now_v7(),
        generation,
        key,
        "two-charge-attempts",
        observed,
        second_provider,
        "mock-model",
        Some(10),
        Some(5),
        true,
        vec![
            attempt(
                1,
                first_provider,
                "mock-model",
                observed - Duration::milliseconds(10),
                Some(503),
                Some("upstream_http"),
                false,
                first_usage,
            ),
            attempt(
                2,
                second_provider,
                "mock-model",
                observed,
                Some(200),
                None,
                true,
                second_usage,
            ),
        ],
    );
    olp::usage::ingestion::persistence::persist_request_metadata_event(pool, &event)
        .await
        .unwrap();
    assert_eq!(
        olp::usage::ingestion::persistence::persist_request_metadata_event(pool, &event)
            .await
            .unwrap(),
        IngestionOutcome::Duplicate
    );
    let detail = olp::usage::history::request_detail(pool, event.request_id)
        .await
        .unwrap();
    assert_eq!(detail.attempts.len(), 2);
    assert_eq!(detail.attempts[0].id, event.attempts[0].id);
    assert_eq!(detail.attempts[1].id, event.attempts[1].id);
    assert_eq!(
        detail.attempts[0].estimated_cost.as_deref(),
        Some("0.000042000000")
    );
    assert_eq!(
        detail.attempts[1].estimated_cost.as_deref(),
        Some("0.000020000000")
    );
    assert_eq!(detail.attempts[0].cached_input_tokens, Some(4));
    assert_eq!(detail.attempts[1].cached_input_tokens, Some(0));
    for attempt in &detail.attempts {
        assert_eq!(attempt.charge_status.as_deref(), Some("billable"));
        assert_eq!(attempt.usage_observed, Some(true));
        assert_eq!(attempt.usage_complete, Some(true));
        assert_eq!(attempt.unpriced, Some(false));
        assert_eq!(attempt.currency.as_deref(), Some("USD"));
        assert!(attempt.pricing_revision_id.is_some());
    }
    assert_eq!(
        detail.request.estimated_cost.as_deref(),
        Some("0.000062000000")
    );
    assert_eq!(detail.request.input_tokens, Some(20));
    assert_eq!(detail.request.output_tokens, Some(10));
    assert_eq!(detail.request.cached_input_tokens, Some(4));
    assert_eq!(detail.request.usage_complete, Some(true));
}

async fn assert_media_attempt_units(pool: &PgPool, generation: Uuid, key: Uuid, provider: Uuid) {
    let observed = Utc::now() - Duration::minutes(20);
    let units = Decimal::new(1125, 3);
    let mut usage = complete_usage(0, 0);
    usage.input_tokens = None;
    usage.output_tokens = None;
    usage.media_units = Some(units);
    let mut event = event(
        Uuid::now_v7(),
        generation,
        key,
        "media-attempt-units",
        observed,
        provider,
        "mock-model",
        None,
        None,
        true,
        vec![attempt(
            1,
            provider,
            "mock-model",
            observed,
            Some(200),
            None,
            true,
            usage,
        )],
    );
    event.operation = olp::protocols::canonical::identity::OperationKind::ImageGeneration;
    event.media_units = Some(units);
    olp::usage::ingestion::persistence::persist_request_metadata_event(pool, &event)
        .await
        .unwrap();
    let detail = olp::usage::history::request_detail(pool, event.request_id)
        .await
        .unwrap();
    assert_eq!(detail.attempts.len(), 1);
    let attempt = &detail.attempts[0];
    assert_eq!(attempt.media_units.as_deref(), Some("1.125000"));
    assert_eq!(attempt.estimated_cost.as_deref(), Some("0.045000000000"));
    assert_eq!(attempt.currency.as_deref(), Some("USD"));
    assert!(attempt.input_tokens.is_none());
    assert!(attempt.output_tokens.is_none());
}
