use chrono::{Duration, Timelike, Utc};
use olp_domain::Surface;
use olp_storage::{
    IdempotencyOutcome, IdempotencyResponse, InstallationSetupInput, MasterKey, OperationsError,
    PgStore, PriceInput, ReplayableIdempotency, RequestAttemptMetadata, RequestFilters,
    RequestMetadataBufferSnapshot, RequestMetadataConsumerState, RequestMetadataEvent,
    RequestMetadataGap, RequestMetadataGatewayEpochState, RequestMetadataPersistenceOutcome,
    UsageDimension, UsageFilters, UsageGranularity, hash_password,
};
use rust_decimal::Decimal;
use uuid::Uuid;

#[tokio::test]
#[ignore = "requires an empty PostgreSQL 18 database in OLP_TEST_DATABASE_URL"]
async fn operations_queries_pricing_rollups_health_and_completeness_reconcile() {
    let database_url = std::env::var("OLP_TEST_DATABASE_URL")
        .expect("OLP_TEST_DATABASE_URL must point to an empty PostgreSQL 18 database");
    let store = PgStore::connect(&database_url, 5).await.unwrap();
    store.migrate().await.unwrap();
    let owner = store
        .setup_installation(InstallationSetupInput {
            installation_name: "Operations integration".to_owned(),
            email: "owner@example.test".to_owned(),
            display_name: "Owner".to_owned(),
            password_hash: hash_password("correct horse battery staple").unwrap(),
        })
        .await
        .unwrap();
    let provider_id = Uuid::now_v7();
    let master_key = MasterKey::new(1, [29; 32]);
    let api_key_id = Uuid::now_v7();
    let generation_id = Uuid::now_v7();
    sqlx::query(
        "INSERT INTO providers
         (id, name, kind, state, auth_mode, etag, created_by,
          last_probe_at, last_probe_status, last_probe_detail)
         VALUES ($1, 'operations-provider', 'openai', 'active', 'api_key', $2, $3,
                 now(), 'succeeded', 'mock probe succeeded')",
    )
    .bind(provider_id)
    .bind(Uuid::now_v7())
    .bind(owner.user_id)
    .execute(store.pool())
    .await
    .unwrap();
    sqlx::query(
        "INSERT INTO api_keys
         (id, lookup_id, secret_digest, name, created_by)
         VALUES ($1, 'olpv2oper001', $2, 'operations test', $3)",
    )
    .bind(api_key_id)
    .bind([9_u8; 32].as_slice())
    .bind(owner.user_id)
    .execute(store.pool())
    .await
    .unwrap();
    sqlx::query(
        "INSERT INTO runtime_generations
         (id, compiled_release, release_sha256, created_by)
         VALUES ($1, $2, $3, $4)",
    )
    .bind(generation_id)
    .bind([1_u8].as_slice())
    .bind([2_u8; 32].as_slice())
    .bind(owner.user_id)
    .execute(store.pool())
    .await
    .unwrap();

    let observed_at = Utc::now() - Duration::hours(2);
    let pricing = store
        .create_pricing_revision(
            owner.user_id,
            "pricing-operations-001",
            observed_at - Duration::days(3),
            &[
                PriceInput {
                    provider_kind: olp_domain::ProviderKind::OpenAi,
                    provider_id: None,
                    model: "mock-model".to_owned(),
                    operation: olp_domain::OperationKind::Generation,
                    input_per_million: Some("1.000000000000".to_owned()),
                    output_per_million: Some("2.000000000000".to_owned()),
                    unit_price: None,
                    currency: "USD".to_owned(),
                },
                PriceInput {
                    provider_kind: olp_domain::ProviderKind::OpenAi,
                    provider_id: Some(provider_id),
                    model: "mock-model".to_owned(),
                    operation: olp_domain::OperationKind::Generation,
                    input_per_million: Some("3.000000000000".to_owned()),
                    output_per_million: Some("4.000000000000".to_owned()),
                    unit_price: None,
                    currency: "USD".to_owned(),
                },
                PriceInput {
                    provider_kind: olp_domain::ProviderKind::OpenAi,
                    provider_id: None,
                    model: "mock-model".to_owned(),
                    operation: olp_domain::OperationKind::ImageGeneration,
                    input_per_million: None,
                    output_per_million: None,
                    unit_price: Some("0.040000000000".to_owned()),
                    currency: "USD".to_owned(),
                },
            ],
            ReplayableIdempotency::new([1; 32], &master_key),
            |_| IdempotencyResponse::new(201, None, None, Vec::new()),
        )
        .await
        .unwrap();
    let IdempotencyOutcome::Executed { value: pricing, .. } = pricing else {
        panic!("fresh pricing revision replayed");
    };
    assert_eq!(pricing.revision, 1);
    assert!(matches!(
        store
            .create_pricing_revision(
                owner.user_id,
                "pricing-operations-001",
                observed_at,
                &pricing.prices,
                ReplayableIdempotency::new([2; 32], &master_key),
                |_| IdempotencyResponse::new(201, None, None, Vec::new()),
            )
            .await,
        Err(OperationsError::IdempotencyConflict)
    ));
    let mut euro_price = pricing.prices[0].clone();
    euro_price.currency = "EUR".to_owned();
    assert!(matches!(
        store
            .create_pricing_revision(
                owner.user_id,
                "pricing-operations-eur-001",
                observed_at,
                &[euro_price],
                ReplayableIdempotency::new([3; 32], &master_key),
                |_| IdempotencyResponse::new(201, None, None, Vec::new()),
            )
            .await,
        Err(OperationsError::Invalid(_))
    ));

    let request_id = Uuid::now_v7();
    let request_started_at = observed_at - Duration::milliseconds(20);
    store
        .persist_request_metadata_event(&RequestMetadataEvent {
            event_id: Uuid::now_v7(),
            request_id,
            runtime_generation_id: generation_id,
            api_key_id,
            provider_id: Some(provider_id),
            route_slug: "default".to_owned(),
            upstream_model: Some("mock-model".to_owned()),
            operation: "generation".parse().unwrap(),
            surface: Surface::Anthropic,
            request_started_at,
            request_completed_at: observed_at,
            observed_at,
            status_code: Some(200),
            error_class: None,
            committed: true,
            latency_ms: 20,
            first_byte_ms: Some(5),
            input_tokens: Some(100),
            output_tokens: Some(50),
            cached_input_tokens: Some(10),
            media_units: None,
            usage_complete: true,
            unpriced: true,
            attempts: vec![RequestAttemptMetadata {
                id: Uuid::now_v7(),
                ordinal: 1,
                provider_id,
                upstream_model: "mock-model".to_owned(),
                started_at: request_started_at,
                completed_at: observed_at,
                status_code: Some(200),
                error_class: None,
                committed: true,
                latency_ms: 20,
                first_byte_ms: Some(5),
            }],
        })
        .await
        .unwrap();
    assert!(
        store
            .report_request_metadata_gap_once(
                RequestMetadataGap {
                    gateway_instance: "integration-gateway".to_owned(),
                    event_count: 3,
                    reason: "injected_test_gap".to_owned(),
                    first_observed_at: observed_at,
                    last_observed_at: observed_at + Duration::seconds(1),
                },
                "operations-integration-injected-gap",
            )
            .await
            .unwrap()
    );
    let loss_at = Utc::now();
    let loss_snapshot = RequestMetadataBufferSnapshot {
        process_epoch: Uuid::now_v7(),
        started_at: loss_at - Duration::seconds(5),
        accepted: 10,
        persisted: 7,
        dropped: 2,
        abandoned: 1,
        retrying: false,
        closed: false,
        first_loss_at: Some(loss_at - Duration::seconds(2)),
        last_loss_at: Some(loss_at),
    };
    let reported = store
        .report_request_metadata_buffer_loss("operations-gateway", &loss_snapshot)
        .await
        .unwrap();
    assert_eq!(reported.reported_events, 3);
    assert_eq!(
        store
            .report_request_metadata_buffer_loss("operations-gateway", &loss_snapshot)
            .await
            .unwrap()
            .reported_events,
        0
    );
    assert!(
        store
            .report_request_metadata_buffer_loss(
                "operations-gateway",
                &RequestMetadataBufferSnapshot {
                    accepted: 9,
                    ..loss_snapshot
                },
            )
            .await
            .is_err()
    );
    let restarted_loss = RequestMetadataBufferSnapshot {
        process_epoch: Uuid::now_v7(),
        started_at: loss_at,
        accepted: 1,
        persisted: 0,
        dropped: 1,
        abandoned: 0,
        retrying: false,
        closed: false,
        first_loss_at: Some(loss_at),
        last_loss_at: Some(loss_at),
    };
    let restarted_report = store
        .report_request_metadata_buffer_loss("operations-gateway", &restarted_loss)
        .await
        .unwrap();
    assert!(restarted_report.process_epoch_changed);
    assert_eq!(restarted_report.reported_events, 1);
    let superseded_gap: (i64, String) = sqlx::query_as(
        "SELECT event_count, certainty::text FROM request_metadata_ingestion_gaps \
         WHERE gateway_instance = 'operations-gateway' \
           AND reason = 'gateway_epoch_unclean_shutdown'",
    )
    .fetch_one(store.pool())
    .await
    .unwrap();
    assert_eq!(superseded_gap, (2, "lower_bound".to_owned()));

    let clean_epoch = Uuid::now_v7();
    let clean_open = RequestMetadataBufferSnapshot {
        process_epoch: clean_epoch,
        started_at: loss_at,
        accepted: 5,
        persisted: 5,
        dropped: 0,
        abandoned: 0,
        retrying: false,
        closed: false,
        first_loss_at: None,
        last_loss_at: None,
    };
    store
        .report_request_metadata_buffer_loss("clean-shutdown-gateway", &clean_open)
        .await
        .unwrap();
    let clean_closed = RequestMetadataBufferSnapshot {
        closed: true,
        ..clean_open
    };
    store
        .close_request_metadata_buffer_epoch("clean-shutdown-gateway", &clean_closed)
        .await
        .unwrap();
    assert_eq!(
        store
            .close_request_metadata_buffer_epoch("clean-shutdown-gateway", &clean_closed)
            .await
            .unwrap()
            .reported_events,
        0
    );
    assert!(
        store
            .report_request_metadata_buffer_loss("clean-shutdown-gateway", &clean_closed)
            .await
            .is_err()
    );
    let clean_uncertainty: i64 = sqlx::query_scalar(
        "SELECT count(*) FROM request_metadata_ingestion_gaps \
         WHERE gateway_instance = 'clean-shutdown-gateway' \
           AND certainty = 'lower_bound'::request_metadata_gap_certainty",
    )
    .fetch_one(store.pool())
    .await
    .unwrap();
    assert_eq!(clean_uncertainty, 0);

    let stale_epoch = Uuid::now_v7();
    store
        .report_request_metadata_buffer_loss(
            "stale-gateway",
            &RequestMetadataBufferSnapshot {
                process_epoch: stale_epoch,
                started_at: loss_at,
                accepted: 5,
                persisted: 2,
                dropped: 0,
                abandoned: 0,
                retrying: false,
                closed: false,
                first_loss_at: None,
                last_loss_at: None,
            },
        )
        .await
        .unwrap();
    sqlx::query(
        "UPDATE request_metadata_gateway_epochs \
         SET started_at = $1, updated_at = $1 \
         WHERE gateway_instance = 'stale-gateway' AND process_epoch = $2",
    )
    .bind(loss_at - Duration::minutes(2))
    .bind(stale_epoch)
    .execute(store.pool())
    .await
    .unwrap();
    let candidate = store
        .detect_stale_request_metadata_gateway_epochs(loss_at)
        .await
        .unwrap();
    assert_eq!(candidate.candidate_epochs, 1);
    assert_eq!(candidate.detected_epochs, 0);
    let detected = store
        .detect_stale_request_metadata_gateway_epochs(loss_at + Duration::seconds(11))
        .await
        .unwrap();
    assert_eq!(detected.detected_epochs, 1);
    assert_eq!(detected.uncertain_event_lower_bound, 3);
    assert_eq!(
        store
            .detect_stale_request_metadata_gateway_epochs(loss_at + Duration::seconds(20))
            .await
            .unwrap()
            .detected_epochs,
        0
    );
    let epoch_health = store.request_metadata_gateway_epoch_health().await.unwrap();
    assert_eq!(epoch_health.unresolved_epochs, 2);
    assert_eq!(epoch_health.historical_uncertain_gap_count, 2);
    assert_eq!(epoch_health.unresolved_event_lower_bound, 5);
    let unresolved_first_page = store
        .request_metadata_gateway_epochs(
            Some(RequestMetadataGatewayEpochState::Unresolved),
            None,
            1,
        )
        .await
        .unwrap();
    assert_eq!(unresolved_first_page.items.len(), 1);
    let unresolved_cursor = unresolved_first_page.next_cursor.as_deref().unwrap();
    let unresolved_cursor = olp_storage::TimestampCursor::parse(unresolved_cursor).unwrap();
    let unresolved_second_page = store
        .request_metadata_gateway_epochs(
            Some(RequestMetadataGatewayEpochState::Unresolved),
            Some(&unresolved_cursor),
            1,
        )
        .await
        .unwrap();
    assert_eq!(unresolved_second_page.items.len(), 1);
    assert_ne!(
        unresolved_first_page.items[0].process_epoch,
        unresolved_second_page.items[0].process_epoch
    );
    let first_acknowledgement = store
        .acknowledge_request_metadata_gateway_epoch(loss_snapshot.process_epoch, owner.user_id)
        .await
        .unwrap()
        .unwrap();
    assert_eq!(
        first_acknowledgement.process_epoch,
        loss_snapshot.process_epoch
    );
    assert_eq!(
        store
            .acknowledge_request_metadata_gateway_epoch(loss_snapshot.process_epoch, owner.user_id)
            .await
            .unwrap()
            .unwrap(),
        first_acknowledgement
    );
    store
        .acknowledge_request_metadata_gateway_epoch(stale_epoch, owner.user_id)
        .await
        .unwrap()
        .unwrap();
    let acknowledged_health = store.request_metadata_gateway_epoch_health().await.unwrap();
    assert_eq!(acknowledged_health.unresolved_epochs, 0);
    assert_eq!(acknowledged_health.historical_uncertain_gap_count, 2);
    assert_eq!(acknowledged_health.unresolved_event_lower_bound, 0);
    let acknowledged_epochs = store
        .request_metadata_gateway_epochs(
            Some(RequestMetadataGatewayEpochState::Acknowledged),
            None,
            10,
        )
        .await
        .unwrap();
    assert_eq!(acknowledged_epochs.items.len(), 2);
    assert!(
        acknowledged_epochs
            .items
            .iter()
            .all(|epoch| epoch.acknowledged_at.is_some())
    );
    let acknowledgement_audits: i64 = sqlx::query_scalar(
        "SELECT count(*) FROM audit_events \
         WHERE action = 'request_metadata.gateway_epoch_acknowledge' \
           AND resource_id = $1 AND outcome = 'success'",
    )
    .bind(loss_snapshot.process_epoch.to_string())
    .fetch_one(store.pool())
    .await
    .unwrap();
    assert_eq!(acknowledgement_audits, 1);

    let request_page = store
        .requests(&RequestFilters::default(), None, 50)
        .await
        .unwrap();
    assert_eq!(request_page.items.len(), 1);
    assert_eq!(request_page.items[0].id, request_id);
    assert_eq!(
        request_page.items[0].estimated_cost.as_deref(),
        Some("0.000500000000")
    );
    assert_eq!(
        store
            .request_detail(request_id)
            .await
            .unwrap()
            .attempts
            .len(),
        1
    );
    assert_eq!(request_page.items[0].surface.as_str(), "anthropic");

    let pre_attempt_request_id = Uuid::now_v7();
    store
        .persist_request_metadata_event(&RequestMetadataEvent {
            event_id: Uuid::now_v7(),
            request_id: pre_attempt_request_id,
            runtime_generation_id: generation_id,
            api_key_id,
            provider_id: None,
            route_slug: "missing-route".to_owned(),
            upstream_model: None,
            operation: "generation".parse().unwrap(),
            surface: Surface::Gemini,
            request_started_at: observed_at,
            request_completed_at: observed_at + Duration::milliseconds(1),
            observed_at: observed_at + Duration::milliseconds(1),
            status_code: Some(404),
            error_class: Some("route_not_found".to_owned()),
            committed: false,
            latency_ms: 1,
            first_byte_ms: None,
            input_tokens: None,
            output_tokens: None,
            cached_input_tokens: None,
            media_units: None,
            usage_complete: false,
            unpriced: true,
            attempts: Vec::new(),
        })
        .await
        .unwrap();
    let pre_attempt = store.request_detail(pre_attempt_request_id).await.unwrap();
    assert_eq!(pre_attempt.request.surface.as_str(), "gemini");
    assert_eq!(pre_attempt.request.attempt_count, 0);
    assert!(pre_attempt.attempts.is_empty());
    let pre_attempt_usage: i64 =
        sqlx::query_scalar("SELECT count(*) FROM usage_facts WHERE request_id = $1")
            .bind(pre_attempt_request_id)
            .fetch_one(store.pool())
            .await
            .unwrap();
    assert_eq!(pre_attempt_usage, 0);

    let filters = UsageFilters {
        observed_after: observed_at - Duration::hours(1),
        observed_before: observed_at + Duration::hours(1),
        route_slug: None,
        provider_id: None,
        upstream_model: None,
        api_key_id: None,
        operation: None,
    };
    store
        .report_request_metadata_consumer_health(0, 0, None)
        .await
        .unwrap();
    let series_report = store
        .usage_series(&filters, UsageGranularity::Hour)
        .await
        .unwrap();
    assert!(series_report.coverage.range_complete);
    let series = series_report.points;
    assert_eq!(series.len(), 1);
    assert_eq!(series[0].input_tokens, "100");
    let breakdown_report = store
        .usage_breakdown(&filters, UsageDimension::Provider, 50)
        .await
        .unwrap();
    assert!(breakdown_report.coverage.range_complete);
    let breakdown = breakdown_report.items;
    assert_eq!(breakdown[0].dimension, provider_id.to_string());
    let completeness = store.usage_completeness(&filters).await.unwrap();
    assert_eq!(completeness.request_count, 1);
    assert_eq!(completeness.priced_count, 1);
    assert_eq!(completeness.request_metadata_gap_events, 3);
    assert_eq!(completeness.uncertain_request_metadata_gap_count, 0);
    assert_eq!(
        completeness.request_metadata_consumer.state,
        RequestMetadataConsumerState::Healthy
    );
    assert!(!completeness.complete);
    let summary = store.usage_summary(&filters).await.unwrap();
    assert_eq!(summary.request_count, 1);
    assert_eq!(summary.cached_input_tokens, "10");
    assert_eq!(summary.estimated_cost.as_deref(), Some("0.000500000000"));
    assert_eq!(summary.currency.as_deref(), Some("USD"));
    assert_eq!(series[0].currency.as_deref(), Some("USD"));
    assert_eq!(breakdown[0].currency.as_deref(), Some("USD"));

    let unpriced_observed_at = Utc::now() - Duration::hours(5);
    store
        .persist_request_metadata_event(&RequestMetadataEvent {
            event_id: Uuid::now_v7(),
            request_id: Uuid::now_v7(),
            runtime_generation_id: generation_id,
            api_key_id,
            provider_id: Some(provider_id),
            route_slug: "moderation".to_owned(),
            upstream_model: Some("unpriced-model".to_owned()),
            operation: "moderation".parse().unwrap(),
            surface: Surface::OpenAi,
            request_started_at: unpriced_observed_at - Duration::milliseconds(5),
            request_completed_at: unpriced_observed_at,
            observed_at: unpriced_observed_at,
            status_code: Some(200),
            error_class: None,
            committed: true,
            latency_ms: 5,
            first_byte_ms: Some(2),
            input_tokens: Some(1),
            output_tokens: None,
            cached_input_tokens: None,
            media_units: None,
            usage_complete: true,
            unpriced: true,
            attempts: vec![RequestAttemptMetadata {
                id: Uuid::now_v7(),
                ordinal: 1,
                provider_id,
                upstream_model: "unpriced-model".to_owned(),
                started_at: unpriced_observed_at - Duration::milliseconds(5),
                completed_at: unpriced_observed_at,
                status_code: Some(200),
                error_class: None,
                committed: true,
                latency_ms: 5,
                first_byte_ms: Some(2),
            }],
        })
        .await
        .unwrap();
    let unpriced_filters = UsageFilters {
        observed_after: unpriced_observed_at - Duration::minutes(10),
        observed_before: unpriced_observed_at + Duration::minutes(10),
        route_slug: None,
        provider_id: None,
        upstream_model: None,
        api_key_id: None,
        operation: Some("moderation".parse().unwrap()),
    };
    let unpriced = store.usage_completeness(&unpriced_filters).await.unwrap();
    assert_eq!(unpriced.unpriced_count, 1);
    assert_eq!(unpriced.incomplete_count, 0);
    assert!(!unpriced.complete);

    let health = store.provider_health(180, None, 50).await.unwrap();
    assert_eq!(health.items.len(), 1);
    assert_eq!(health.items[0].status, "healthy");
    assert_eq!(health.items[0].attempt_count, 1);
    let generations = store.runtime_generations(None, 50).await.unwrap();
    assert_eq!(generations.items[0].id, generation_id);
    assert!(!store.audit_events(None, 50).await.unwrap().items.is_empty());

    let setting = store
        .settings()
        .await
        .unwrap()
        .into_iter()
        .find(|setting| setting.key == "retention.requests_days")
        .unwrap();
    let updated = store
        .update_setting(&setting.key, "31", setting.etag, owner.user_id)
        .await
        .unwrap();
    assert_eq!(updated.value, "31");
    assert!(matches!(
        store
            .update_setting(&setting.key, "32", setting.etag, owner.user_id)
            .await,
        Err(OperationsError::PreconditionFailed)
    ));

    let usage_setting = store
        .settings()
        .await
        .unwrap()
        .into_iter()
        .find(|setting| setting.key == "retention.usage_days")
        .unwrap();
    assert!(matches!(
        store
            .update_setting(&usage_setting.key, "0", usage_setting.etag, owner.user_id)
            .await,
        Err(OperationsError::Invalid(_))
    ));
    let usage_setting = store
        .update_setting(&usage_setting.key, "1", usage_setting.etag, owner.user_id)
        .await
        .unwrap();
    assert_eq!(usage_setting.value, "1");

    // Keep the late event below in the same hourly bucket even when this test
    // starts immediately before an hour boundary.
    let archived_observed_at = (Utc::now() - Duration::days(2))
        .with_minute(30)
        .unwrap()
        .with_second(0)
        .unwrap()
        .with_nanosecond(0)
        .unwrap();
    let archived_request_id = Uuid::now_v7();
    store
        .persist_request_metadata_event(&RequestMetadataEvent {
            event_id: Uuid::now_v7(),
            request_id: archived_request_id,
            runtime_generation_id: generation_id,
            api_key_id,
            provider_id: Some(provider_id),
            route_slug: "default".to_owned(),
            upstream_model: Some("mock-model".to_owned()),
            operation: "generation".parse().unwrap(),
            surface: Surface::Gemini,
            request_started_at: archived_observed_at - Duration::milliseconds(10),
            request_completed_at: archived_observed_at,
            observed_at: archived_observed_at,
            status_code: Some(200),
            error_class: None,
            committed: true,
            latency_ms: 10,
            first_byte_ms: Some(2),
            input_tokens: Some(7),
            output_tokens: Some(3),
            cached_input_tokens: Some(2),
            media_units: None,
            usage_complete: true,
            unpriced: true,
            attempts: vec![RequestAttemptMetadata {
                id: Uuid::now_v7(),
                ordinal: 1,
                provider_id,
                upstream_model: "mock-model".to_owned(),
                started_at: archived_observed_at - Duration::milliseconds(10),
                completed_at: archived_observed_at,
                status_code: Some(200),
                error_class: None,
                committed: true,
                latency_ms: 10,
                first_byte_ms: Some(2),
            }],
        })
        .await
        .unwrap();
    sqlx::query(
        "INSERT INTO transactional_outbox \
         (id, topic, aggregate_id, payload, created_at, published_at) \
         VALUES ($1, 'runtime.changed', $2, $3, now() - interval '8 days', \
                 now() - interval '8 days')",
    )
    .bind(Uuid::now_v7())
    .bind(Uuid::now_v7())
    .bind([1_u8].as_slice())
    .execute(store.pool())
    .await
    .unwrap();
    sqlx::query(
        "INSERT INTO request_metadata_ingestion_gaps \
         (id, gateway_instance, event_count, reason, first_observed_at, last_observed_at, reported_at) \
         VALUES ($1, 'archived-gap-gateway', 4, 'archived_test_gap', $2, $3, $4)",
    )
    .bind(Uuid::now_v7())
    .bind(archived_observed_at)
    .bind(archived_observed_at + Duration::seconds(2))
    .bind(archived_observed_at + Duration::minutes(1))
    .execute(store.pool())
    .await
    .unwrap();
    sqlx::query(
        "INSERT INTO request_metadata_ingestion_gaps \
         (id, gateway_instance, event_count, reason, certainty, \
          first_observed_at, last_observed_at, reported_at) \
         VALUES ($1, 'archived-uncertain-gateway', 0, 'archived_uncertain_epoch', \
                 'lower_bound'::request_metadata_gap_certainty, $2, $3, $4)",
    )
    .bind(Uuid::now_v7())
    .bind(archived_observed_at)
    .bind(archived_observed_at + Duration::seconds(3))
    .bind(archived_observed_at + Duration::minutes(1))
    .execute(store.pool())
    .await
    .unwrap();
    sqlx::query(
        "UPDATE request_metadata_gateway_epochs \
         SET started_at = $1, updated_at = $1, stale_candidate_at = NULL, \
             stale_detected_at = $1 + interval '1 second', \
             acknowledged_at = $1 + interval '2 seconds' \
         WHERE process_epoch = ANY($2)",
    )
    .bind(archived_observed_at)
    .bind(vec![loss_snapshot.process_epoch, stale_epoch])
    .execute(store.pool())
    .await
    .unwrap();
    sqlx::query(
        "UPDATE request_metadata_gateway_epochs \
         SET started_at = $1, updated_at = $1, gracefully_closed_at = $1 \
         WHERE process_epoch = $2",
    )
    .bind(archived_observed_at)
    .bind(clean_epoch)
    .execute(store.pool())
    .await
    .unwrap();

    // Model a rolling old consumer that does not coordinate with maintenance:
    // its uncommitted fact holds KEY SHARE on the anchor. Cleanup must skip the
    // locked anchor and finish without cascading the late fact.
    let concurrent_request_id = Uuid::now_v7();
    let concurrent_event_id = Uuid::now_v7();
    let concurrent_started_at = Utc::now() - Duration::days(40);
    sqlx::query(
        "INSERT INTO usage_request_anchors (request_id, request_started_at) VALUES ($1, $2)",
    )
    .bind(concurrent_request_id)
    .bind(concurrent_started_at)
    .execute(store.pool())
    .await
    .unwrap();
    sqlx::query(
        "INSERT INTO request_metadata_event_receipts \
         (event_id, request_id, event_sha256, status, observed_at) \
         VALUES ($1, $2, NULL, 'pending', $3)",
    )
    .bind(concurrent_event_id)
    .bind(concurrent_request_id)
    .bind(concurrent_started_at)
    .execute(store.pool())
    .await
    .unwrap();
    let expired_receipt_id = Uuid::now_v7();
    sqlx::query(
        "INSERT INTO request_metadata_event_receipts \
         (event_id, request_id, event_sha256, status, observed_at, recorded_at) \
         VALUES ($1, $2, NULL, 'pending', now() - interval '8 days', \
                 now() - interval '8 days')",
    )
    .bind(expired_receipt_id)
    .bind(Uuid::now_v7())
    .execute(store.pool())
    .await
    .unwrap();
    let future_skew_receipt_id = Uuid::now_v7();
    sqlx::query(
        "INSERT INTO request_metadata_event_receipts \
         (event_id, request_id, event_sha256, status, observed_at, recorded_at) \
         VALUES ($1, $2, NULL, 'pending', now() + interval '100 years', \
                 now() - interval '8 days')",
    )
    .bind(future_skew_receipt_id)
    .bind(Uuid::now_v7())
    .execute(store.pool())
    .await
    .unwrap();
    let mut concurrent_usage = store.pool().begin().await.unwrap();
    sqlx::query(
        "INSERT INTO usage_facts \
         (id, request_id, request_started_at, api_key_id, provider_id, route_slug, \
          upstream_model, operation, surface, observed_at, unpriced, usage_complete) \
         VALUES ($1, $2, $3, $4, $5, 'retention-race', 'mock-model', 'generation', \
                 'gemini', $3, true, true)",
    )
    .bind(concurrent_event_id)
    .bind(concurrent_request_id)
    .bind(concurrent_started_at)
    .bind(api_key_id)
    .bind(provider_id)
    .execute(&mut *concurrent_usage)
    .await
    .unwrap();
    let maintenance_store = store.clone();
    let maintenance = tokio::time::timeout(
        std::time::Duration::from_secs(5),
        tokio::spawn(async move { maintenance_store.run_maintenance(Utc::now()).await }),
    )
    .await
    .expect("maintenance waited on a late fact's anchor")
    .unwrap()
    .unwrap();
    concurrent_usage.commit().await.unwrap();
    let concurrent_retained: (i64, i64) = sqlx::query_as(
        "SELECT \
           (SELECT count(*) FROM usage_request_anchors WHERE request_id = $1), \
           (SELECT count(*) FROM usage_facts WHERE request_id = $1)",
    )
    .bind(concurrent_request_id)
    .fetch_one(store.pool())
    .await
    .unwrap();
    assert_eq!(concurrent_retained, (1, 1));
    sqlx::query("DELETE FROM usage_request_anchors WHERE request_id = $1")
        .bind(concurrent_request_id)
        .execute(store.pool())
        .await
        .unwrap();

    assert_eq!(maintenance.rollup_rows, 1);
    assert_eq!(maintenance.request_metadata_gap_rollup_rows, 2);
    assert_eq!(maintenance.request_metadata_gap_rows, 2);
    assert_eq!(maintenance.request_metadata_epoch_rows, 3);
    assert_eq!(maintenance.request_metadata_receipt_rows, 2);
    assert_eq!(maintenance.outbox_rows, 1);
    let future_skew_receipt_retained: bool = sqlx::query_scalar(
        "SELECT EXISTS (SELECT 1 FROM request_metadata_event_receipts WHERE event_id = $1)",
    )
    .bind(future_skew_receipt_id)
    .fetch_one(store.pool())
    .await
    .unwrap();
    assert!(!future_skew_receipt_retained);
    let rollup_count: i64 = sqlx::query_scalar("SELECT count(*) FROM usage_hourly")
        .fetch_one(store.pool())
        .await
        .unwrap();
    assert_eq!(rollup_count, 1);
    let legacy_rollup_error = sqlx::query(
        "UPDATE usage_hourly SET request_count = request_count \
         WHERE bucket = date_trunc('hour', $1::timestamptz)",
    )
    .bind(archived_observed_at)
    .execute(store.pool())
    .await
    .unwrap_err();
    assert_eq!(
        legacy_rollup_error
            .as_database_error()
            .and_then(sqlx::error::DatabaseError::code)
            .as_deref(),
        Some("55000")
    );
    let archived_fact_count: i64 =
        sqlx::query_scalar("SELECT count(*) FROM usage_facts WHERE request_id = $1")
            .bind(archived_request_id)
            .fetch_one(store.pool())
            .await
            .unwrap();
    assert_eq!(archived_fact_count, 0);
    let archived_bucket = archived_observed_at
        .with_minute(0)
        .unwrap()
        .with_second(0)
        .unwrap()
        .with_nanosecond(0)
        .unwrap();
    let archived_filters = UsageFilters {
        observed_after: archived_bucket,
        observed_before: archived_bucket + Duration::hours(1),
        route_slug: None,
        provider_id: None,
        upstream_model: None,
        api_key_id: Some(api_key_id),
        operation: None,
    };
    let archived_summary = store.usage_summary(&archived_filters).await.unwrap();
    assert_eq!(archived_summary.request_count, 1);
    assert_eq!(archived_summary.input_tokens, "7");
    assert_eq!(archived_summary.cached_input_tokens, "2");
    assert_eq!(archived_summary.currency.as_deref(), Some("USD"));
    assert_eq!(archived_summary.request_metadata_gap_events, 4);
    assert_eq!(archived_summary.uncertain_request_metadata_gap_count, 1);
    assert!(archived_summary.coverage.range_complete);
    assert!(!archived_summary.complete);
    let archived_gap: (i64, chrono::DateTime<Utc>, chrono::DateTime<Utc>) = sqlx::query_as(
        "SELECT event_count, first_observed_at, last_observed_at \
         FROM request_metadata_gap_hourly \
         WHERE gateway_instance = 'archived-gap-gateway' AND reason = 'archived_test_gap'",
    )
    .fetch_one(store.pool())
    .await
    .unwrap();
    assert_eq!(archived_gap.0, 4);
    assert_eq!(
        archived_gap.1.timestamp_micros(),
        archived_observed_at.timestamp_micros()
    );
    assert_eq!(
        archived_gap.2.timestamp_micros(),
        (archived_observed_at + Duration::seconds(2)).timestamp_micros()
    );
    let retained_uncertainty: i64 = sqlx::query_scalar(
        "SELECT sum(uncertain_gap_count)::bigint FROM request_metadata_gap_hourly \
         WHERE gateway_instance = 'archived-uncertain-gateway'",
    )
    .fetch_one(store.pool())
    .await
    .unwrap();
    assert_eq!(retained_uncertainty, 1);
    let retained_epoch_health = store.request_metadata_gateway_epoch_health().await.unwrap();
    assert_eq!(retained_epoch_health.unresolved_epochs, 0);
    assert_eq!(retained_epoch_health.historical_uncertain_gap_count, 3);
    let retained_old_epochs: i64 = sqlx::query_scalar(
        "SELECT count(*) FROM request_metadata_gateway_epochs \
         WHERE process_epoch = ANY($1)",
    )
    .bind(vec![loss_snapshot.process_epoch, stale_epoch, clean_epoch])
    .fetch_one(store.pool())
    .await
    .unwrap();
    assert_eq!(retained_old_epochs, 0);
    let archived_by_key = store
        .usage_breakdown(&archived_filters, UsageDimension::ApiKey, 50)
        .await
        .unwrap();
    assert_eq!(archived_by_key.items[0].dimension, api_key_id.to_string());

    let partial_archived_filters = UsageFilters {
        observed_after: archived_bucket + Duration::minutes(15),
        observed_before: archived_bucket + Duration::minutes(45),
        route_slug: Some("default".to_owned()),
        provider_id: Some(provider_id),
        upstream_model: Some("mock-model".to_owned()),
        api_key_id: Some(api_key_id),
        operation: Some("generation".parse().unwrap()),
    };
    let partial_archived = store
        .usage_summary(&partial_archived_filters)
        .await
        .unwrap();
    assert_eq!(partial_archived.request_count, 0);
    assert!(!partial_archived.coverage.range_complete);
    assert!(partial_archived.coverage.approximate);
    assert_eq!(
        partial_archived
            .coverage
            .excluded_partial_aggregate_boundaries,
        1
    );
    assert!(!partial_archived.complete);

    // A stream consumer can deliver an old event after its hour was already
    // retained. A later maintenance pass must add that event to the existing
    // aggregate instead of replacing the hour with only the late row. An
    // exact redelivery after that rollup must remain a no-op.
    let late_request_id = Uuid::now_v7();
    let late_event = RequestMetadataEvent {
        event_id: Uuid::now_v7(),
        request_id: late_request_id,
        runtime_generation_id: generation_id,
        api_key_id,
        provider_id: Some(provider_id),
        route_slug: "default".to_owned(),
        upstream_model: Some("mock-model".to_owned()),
        operation: "generation".parse().unwrap(),
        surface: Surface::Gemini,
        request_started_at: archived_observed_at + Duration::seconds(10),
        request_completed_at: archived_observed_at + Duration::seconds(11),
        observed_at: archived_observed_at + Duration::seconds(11),
        status_code: Some(200),
        error_class: None,
        committed: true,
        latency_ms: 1_000,
        first_byte_ms: Some(10),
        input_tokens: Some(5),
        output_tokens: Some(1),
        cached_input_tokens: Some(1),
        media_units: None,
        usage_complete: true,
        unpriced: true,
        attempts: vec![RequestAttemptMetadata {
            id: Uuid::now_v7(),
            ordinal: 1,
            provider_id,
            upstream_model: "mock-model".to_owned(),
            started_at: archived_observed_at + Duration::seconds(10),
            completed_at: archived_observed_at + Duration::seconds(11),
            status_code: Some(200),
            error_class: None,
            committed: true,
            latency_ms: 1_000,
            first_byte_ms: Some(10),
        }],
    };
    store
        .persist_request_metadata_event(&late_event)
        .await
        .unwrap();
    sqlx::query(
        "INSERT INTO request_metadata_ingestion_gaps \
         (id, gateway_instance, event_count, reason, first_observed_at, \
          last_observed_at, reported_at) \
         VALUES ($1, 'archived-gap-gateway', 2, 'archived_test_gap', $2, $3, $4)",
    )
    .bind(Uuid::now_v7())
    .bind(archived_observed_at + Duration::seconds(10))
    .bind(archived_observed_at + Duration::seconds(11))
    .bind(archived_observed_at + Duration::minutes(2))
    .execute(store.pool())
    .await
    .unwrap();
    let late_maintenance = store.run_maintenance(Utc::now()).await.unwrap();
    assert_eq!(late_maintenance.rollup_rows, 1);
    assert_eq!(late_maintenance.usage_rows, 1);
    assert_eq!(late_maintenance.request_metadata_gap_rollup_rows, 1);
    assert_eq!(late_maintenance.request_metadata_gap_rows, 1);
    let additive_usage: (i64, String, String) = sqlx::query_as(
        "SELECT request_count, input_tokens::text, output_tokens::text \
         FROM usage_hourly \
         WHERE bucket = $1 AND route_slug = 'default' AND provider_id = $2 \
           AND upstream_model = 'mock-model' AND operation = 'generation' \
           AND surface = 'gemini' AND api_key_id = $3",
    )
    .bind(archived_bucket)
    .bind(provider_id)
    .bind(api_key_id)
    .fetch_one(store.pool())
    .await
    .unwrap();
    assert_eq!(additive_usage, (2, "12".to_owned(), "4".to_owned()));
    let additive_gap: i64 = sqlx::query_scalar(
        "SELECT event_count FROM request_metadata_gap_hourly \
         WHERE bucket = $1 AND gateway_instance = 'archived-gap-gateway' \
           AND reason = 'archived_test_gap'",
    )
    .bind(archived_bucket)
    .fetch_one(store.pool())
    .await
    .unwrap();
    assert_eq!(additive_gap, 6);

    // Simulate an N-1 worker that knows nothing about receipts. The database
    // trigger must suppress its replay after the raw fact was retained.
    let legacy_replay = sqlx::query(
        "INSERT INTO usage_facts \
         (id, request_id, request_started_at, api_key_id, provider_id, route_slug, \
          upstream_model, operation, surface, observed_at, unpriced, usage_complete) \
         VALUES ($1, $2, $3, $4, $5, 'default', 'mock-model', 'generation', \
                 'gemini', $6, true, true)",
    )
    .bind(late_event.event_id)
    .bind(late_event.request_id)
    .bind(late_event.request_started_at)
    .bind(late_event.api_key_id)
    .bind(provider_id)
    .bind(late_event.observed_at)
    .execute(store.pool())
    .await
    .unwrap();
    assert_eq!(legacy_replay.rows_affected(), 0);

    store
        .persist_request_metadata_event(&late_event)
        .await
        .unwrap();
    let replay_maintenance = store.run_maintenance(Utc::now()).await.unwrap();
    assert_eq!(replay_maintenance.usage_rows, 0);
    assert_eq!(replay_maintenance.rollup_rows, 0);
    let replayed_usage: (i64, String, String) = sqlx::query_as(
        "SELECT request_count, input_tokens::text, output_tokens::text \
         FROM usage_hourly \
         WHERE bucket = $1 AND route_slug = 'default' AND provider_id = $2 \
           AND upstream_model = 'mock-model' AND operation = 'generation' \
           AND surface = 'gemini' AND api_key_id = $3",
    )
    .bind(archived_bucket)
    .bind(provider_id)
    .bind(api_key_id)
    .fetch_one(store.pool())
    .await
    .unwrap();
    assert_eq!(replayed_usage, additive_usage);

    let mut conflicting_late_event = late_event.clone();
    conflicting_late_event.event_id = Uuid::now_v7();
    assert!(matches!(
        store
            .persist_request_metadata_event(&conflicting_late_event)
            .await,
        Err(olp_storage::PersistenceError::InvalidRequestMetadataEvent)
    ));

    let media_request_id = Uuid::now_v7();
    let media_started_at = observed_at + Duration::minutes(1);
    store
        .persist_request_metadata_event(&RequestMetadataEvent {
            event_id: Uuid::now_v7(),
            request_id: media_request_id,
            runtime_generation_id: generation_id,
            api_key_id,
            provider_id: Some(provider_id),
            route_slug: "images".to_owned(),
            upstream_model: Some("mock-model".to_owned()),
            operation: "image_generation".parse().unwrap(),
            surface: Surface::OpenAi,
            request_started_at: media_started_at,
            request_completed_at: media_started_at + Duration::milliseconds(50),
            observed_at: media_started_at + Duration::milliseconds(50),
            status_code: Some(200),
            error_class: None,
            committed: true,
            latency_ms: 50,
            first_byte_ms: Some(20),
            input_tokens: None,
            output_tokens: None,
            cached_input_tokens: None,
            media_units: Some(Decimal::new(3, 0)),
            usage_complete: true,
            unpriced: true,
            attempts: vec![RequestAttemptMetadata {
                id: Uuid::now_v7(),
                ordinal: 1,
                provider_id,
                upstream_model: "mock-model".to_owned(),
                started_at: media_started_at,
                completed_at: media_started_at + Duration::milliseconds(50),
                status_code: Some(200),
                error_class: None,
                committed: true,
                latency_ms: 50,
                first_byte_ms: Some(20),
            }],
        })
        .await
        .unwrap();
    let media_cost: (String, String, bool) = sqlx::query_as(
        "SELECT media_units::text, estimated_cost::text, unpriced \
         FROM usage_facts WHERE request_id = $1",
    )
    .bind(media_request_id)
    .fetch_one(store.pool())
    .await
    .unwrap();
    assert_eq!(media_cost.0, "3.000000");
    assert_eq!(media_cost.1, "0.120000000000");
    assert!(!media_cost.2);

    // Delivery is intentionally bounded: a first event outside the seven-day
    // window is rejected by new code. The database applies the same fence to
    // an N-1 writer and records exactly one visible gap for repeated delivery.
    let outside_window_observed_at = Utc::now() - Duration::days(8);
    let mut outside_window_event = late_event.clone();
    outside_window_event.event_id = Uuid::now_v7();
    outside_window_event.request_id = Uuid::now_v7();
    outside_window_event.request_started_at =
        outside_window_observed_at - Duration::milliseconds(10);
    outside_window_event.request_completed_at = outside_window_observed_at;
    outside_window_event.observed_at = outside_window_observed_at;
    outside_window_event.attempts[0].id = Uuid::now_v7();
    outside_window_event.attempts[0].started_at = outside_window_event.request_started_at;
    outside_window_event.attempts[0].completed_at = outside_window_observed_at;
    assert_eq!(
        store
            .persist_request_metadata_event(&outside_window_event)
            .await
            .unwrap(),
        RequestMetadataPersistenceOutcome::RejectedOutsideReplayWindow
    );
    assert_eq!(
        store
            .persist_request_metadata_event(&outside_window_event)
            .await
            .unwrap(),
        RequestMetadataPersistenceOutcome::Duplicate
    );
    let application_gap: (i64, i64, String) = sqlx::query_as(
        "SELECT count(*), sum(event_count)::bigint, min(certainty::text) \
         FROM request_metadata_ingestion_gaps \
         WHERE gateway_instance = 'request-metadata-consumer' \
           AND reason = 'request_metadata_event_outside_replay_window'",
    )
    .fetch_one(store.pool())
    .await
    .unwrap();
    assert_eq!(application_gap, (1, 0, "lower_bound".to_owned()));

    let mut legacy_outside_window_event = outside_window_event.clone();
    legacy_outside_window_event.event_id = Uuid::now_v7();
    legacy_outside_window_event.request_id = Uuid::now_v7();

    for _ in 0..2 {
        let legacy_outside_window = sqlx::query(
            "INSERT INTO usage_facts \
             (id, request_id, request_started_at, api_key_id, provider_id, route_slug, \
              upstream_model, operation, surface, observed_at, unpriced, usage_complete) \
             VALUES ($1, $2, $3, $4, $5, 'default', 'mock-model', 'generation', \
                     'gemini', $6, true, true)",
        )
        .bind(legacy_outside_window_event.event_id)
        .bind(legacy_outside_window_event.request_id)
        .bind(legacy_outside_window_event.request_started_at)
        .bind(legacy_outside_window_event.api_key_id)
        .bind(provider_id)
        .bind(legacy_outside_window_event.observed_at)
        .execute(store.pool())
        .await
        .unwrap();
        assert_eq!(legacy_outside_window.rows_affected(), 0);
    }
    let outside_window_gaps: i64 = sqlx::query_scalar(
        "SELECT count(*) FROM request_metadata_ingestion_gaps \
         WHERE gateway_instance = 'database-fence' \
           AND reason = 'request_metadata_event_outside_replay_window'",
    )
    .fetch_one(store.pool())
    .await
    .unwrap();
    assert_eq!(outside_window_gaps, 1);

    let poison_detected_at = Utc::now();
    let poison_gap = || RequestMetadataGap {
        gateway_instance: "request-metadata-consumer".to_owned(),
        event_count: 1,
        reason: "invalid_request_metadata_event".to_owned(),
        first_observed_at: poison_detected_at,
        last_observed_at: poison_detected_at,
    };
    assert!(
        store
            .report_request_metadata_gap_once(
                poison_gap(),
                "request-metadata-event:test-poison:invalid"
            )
            .await
            .unwrap()
    );
    assert!(
        !store
            .report_request_metadata_gap_once(
                poison_gap(),
                "request-metadata-event:test-poison:invalid"
            )
            .await
            .unwrap()
    );

    // Retention must use the same PostgreSQL clock as receipt admission. A
    // worker clock ahead by ten minutes must not remove identities or poison
    // gap deduplication keys that the database still considers in-window.
    let skew_receipt_id = Uuid::now_v7();
    sqlx::query(
        "INSERT INTO request_metadata_event_receipts \
         (event_id, request_id, status, observed_at, recorded_at) \
         VALUES ($1, $2, 'fact_persisted', now() - interval '7 days 2 minutes', \
                 now() - interval '7 days 2 minutes')",
    )
    .bind(skew_receipt_id)
    .bind(Uuid::now_v7())
    .execute(store.pool())
    .await
    .unwrap();
    let skew_gap_key = "request-metadata-event:test-clock-skew:invalid";
    sqlx::query(
        "INSERT INTO request_metadata_ingestion_gaps \
         (id, gateway_instance, event_count, reason, certainty, first_observed_at, \
          last_observed_at, reported_at, deduplication_key) \
         VALUES ($1, 'request-metadata-consumer', 1, 'invalid_request_metadata_event', \
                 'exact'::request_metadata_gap_certainty, now() - interval '7 days 2 minutes', \
                 now() - interval '7 days 2 minutes', now() - interval '7 days 2 minutes', $2)",
    )
    .bind(Uuid::now_v7())
    .bind(skew_gap_key)
    .execute(store.pool())
    .await
    .unwrap();
    store
        .run_maintenance(Utc::now() + Duration::minutes(10))
        .await
        .unwrap();
    let skew_evidence_retained: (bool, bool) = sqlx::query_as(
        "SELECT \
           EXISTS (SELECT 1 FROM request_metadata_event_receipts WHERE event_id = $1), \
           EXISTS (SELECT 1 FROM request_metadata_ingestion_gaps WHERE deduplication_key = $2)",
    )
    .bind(skew_receipt_id)
    .bind(skew_gap_key)
    .fetch_one(store.pool())
    .await
    .unwrap();
    assert_eq!(skew_evidence_retained, (true, true));
}
