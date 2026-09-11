use super::*;

pub(super) struct QueryFixture {
    pub(super) observed_at: chrono::DateTime<Utc>,
    pub(super) loss_epoch: Uuid,
    pub(super) stale_epoch: Uuid,
    pub(super) clean_epoch: Uuid,
}

pub(super) async fn exercise(
    pool: &PgPool,
    owner_id: Uuid,
    provider_id: Uuid,
    master_key: &MasterKey,
    api_key_id: Uuid,
    generation_id: Uuid,
) -> QueryFixture {
    let observed_at = Utc::now() - Duration::hours(2);
    let pricing = olp::usage::pricing::create_pricing_revision(
        pool,
        &olp::database::RequestProvenance::default(),
        owner_id,
        "pricing-operations-001",
        observed_at - Duration::days(3),
        &[
            PriceInput {
                vendor_id: None,
                provider_kind: olp::providers::runtime_model::ProviderKind::OpenAi,
                provider_id: None,
                model: "mock-model".to_owned(),
                operation: olp::protocols::canonical::identity::OperationKind::Generation,
                input_per_million: Some("1.000000000000".to_owned()),
                cached_input_per_million: None,
                output_per_million: Some("2.000000000000".to_owned()),
                unit_price: None,
                currency: "USD".to_owned(),
            },
            PriceInput {
                vendor_id: None,
                provider_kind: olp::providers::runtime_model::ProviderKind::OpenAi,
                provider_id: Some(provider_id),
                model: "mock-model".to_owned(),
                operation: olp::protocols::canonical::identity::OperationKind::Generation,
                input_per_million: Some("3.000000000000".to_owned()),
                cached_input_per_million: Some("1.000000000000".to_owned()),
                output_per_million: Some("4.000000000000".to_owned()),
                unit_price: None,
                currency: "USD".to_owned(),
            },
            PriceInput {
                vendor_id: None,
                provider_kind: olp::providers::runtime_model::ProviderKind::OpenAi,
                provider_id: None,
                model: "mock-model".to_owned(),
                operation: olp::protocols::canonical::identity::OperationKind::ImageGeneration,
                input_per_million: None,
                cached_input_per_million: None,
                output_per_million: None,
                unit_price: Some("0.040000000000".to_owned()),
                currency: "USD".to_owned(),
            },
        ],
        Replayable::new([1; 32], master_key),
        |_| Response::new(201, None, None, Vec::new()),
    )
    .await
    .unwrap();
    let IdempotencyOutcome::Executed { value: pricing, .. } = pricing else {
        panic!("fresh pricing revision replayed");
    };
    assert_eq!(pricing.revision, 1);
    assert!(matches!(
        olp::usage::pricing::create_pricing_revision(
            pool,
            &olp::database::RequestProvenance::default(),
            owner_id,
            "pricing-operations-001",
            observed_at,
            &pricing.prices,
            Replayable::new([2; 32], master_key),
            |_| Response::new(201, None, None, Vec::new()),
        )
        .await,
        Err(Error::IdempotencyConflict)
    ));
    let mut euro_price = pricing.prices[0].clone();
    euro_price.currency = "EUR".to_owned();
    assert!(matches!(
        olp::usage::pricing::create_pricing_revision(
            pool,
            &olp::database::RequestProvenance::default(),
            owner_id,
            "pricing-operations-eur-001",
            observed_at,
            &[euro_price],
            Replayable::new([3; 32], master_key),
            |_| Response::new(201, None, None, Vec::new()),
        )
        .await,
        Err(Error::Invalid(_))
    ));

    let request_id = Uuid::now_v7();
    let request_started_at = observed_at - Duration::milliseconds(20);
    olp::usage::ingestion::persistence::persist_request_metadata_event(
        pool,
        &Event {
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
                routing: None,
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
                usage: Some(olp::usage::emitter::RequestAttemptUsageMetadata {
                    observed: true,
                    complete: true,
                    billing_uncertain: false,
                    input_tokens: Some(100),
                    output_tokens: Some(50),
                    cached_input_tokens: Some(10),
                    media_units: None,
                }),
            }],
        },
    )
    .await
    .unwrap();
    assert!(
        olp::usage::ingestion::reconciliation::report_request_metadata_gap_once(
            pool,
            Gap {
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
    let loss_snapshot = Snapshot {
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
    let reported = olp::usage::ingestion::reconciliation::report_request_metadata_buffer_loss(
        pool,
        "operations-gateway",
        &loss_snapshot,
    )
    .await
    .unwrap();
    assert_eq!(reported.reported_events, 3);
    assert_eq!(
        olp::usage::ingestion::reconciliation::report_request_metadata_buffer_loss(
            pool,
            "operations-gateway",
            &loss_snapshot
        )
        .await
        .unwrap()
        .reported_events,
        0
    );
    assert!(
        olp::usage::ingestion::reconciliation::report_request_metadata_buffer_loss(
            pool,
            "operations-gateway",
            &Snapshot {
                accepted: 9,
                ..loss_snapshot
            },
        )
        .await
        .is_err()
    );
    let restarted_loss = Snapshot {
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
    let restarted_report =
        olp::usage::ingestion::reconciliation::report_request_metadata_buffer_loss(
            pool,
            "operations-gateway",
            &restarted_loss,
        )
        .await
        .unwrap();
    assert!(restarted_report.process_epoch_changed);
    assert_eq!(restarted_report.reported_events, 1);
    let superseded_gap: (i64, String) = sqlx::query_as(
        "SELECT event_count, certainty::text FROM request_metadata_ingestion_gaps \
         WHERE gateway_instance = 'operations-gateway' \
           AND reason = 'gateway_epoch_unclean_shutdown'",
    )
    .fetch_one(pool)
    .await
    .unwrap();
    assert_eq!(superseded_gap, (2, "lower_bound".to_owned()));

    let clean_epoch = Uuid::now_v7();
    let clean_open = Snapshot {
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
    olp::usage::ingestion::reconciliation::report_request_metadata_buffer_loss(
        pool,
        "clean-shutdown-gateway",
        &clean_open,
    )
    .await
    .unwrap();
    let clean_closed = Snapshot {
        closed: true,
        ..clean_open
    };
    olp::usage::ingestion::reconciliation::close_request_metadata_buffer_epoch(
        pool,
        "clean-shutdown-gateway",
        &clean_closed,
    )
    .await
    .unwrap();
    assert_eq!(
        olp::usage::ingestion::reconciliation::close_request_metadata_buffer_epoch(
            pool,
            "clean-shutdown-gateway",
            &clean_closed
        )
        .await
        .unwrap()
        .reported_events,
        0
    );
    assert!(
        olp::usage::ingestion::reconciliation::report_request_metadata_buffer_loss(
            pool,
            "clean-shutdown-gateway",
            &clean_closed
        )
        .await
        .is_err()
    );
    let clean_uncertainty: i64 = sqlx::query_scalar(
        "SELECT count(*) FROM request_metadata_ingestion_gaps \
         WHERE gateway_instance = 'clean-shutdown-gateway' \
           AND certainty = 'lower_bound'::request_metadata_gap_certainty",
    )
    .fetch_one(pool)
    .await
    .unwrap();
    assert_eq!(clean_uncertainty, 0);

    let stale_epoch = Uuid::now_v7();
    olp::usage::ingestion::reconciliation::report_request_metadata_buffer_loss(
        pool,
        "stale-gateway",
        &Snapshot {
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
    .execute(pool)
    .await
    .unwrap();
    let candidate =
        olp::usage::ingestion::reconciliation::detect_stale_request_metadata_gateway_epochs(
            pool, loss_at,
        )
        .await
        .unwrap();
    assert_eq!(candidate.candidate_epochs, 1);
    assert_eq!(candidate.detected_epochs, 0);
    let detected =
        olp::usage::ingestion::reconciliation::detect_stale_request_metadata_gateway_epochs(
            pool,
            loss_at + Duration::seconds(11),
        )
        .await
        .unwrap();
    assert_eq!(detected.detected_epochs, 1);
    assert_eq!(detected.uncertain_event_lower_bound, 3);
    assert_eq!(
        olp::usage::ingestion::reconciliation::detect_stale_request_metadata_gateway_epochs(
            pool,
            loss_at + Duration::seconds(20)
        )
        .await
        .unwrap()
        .detected_epochs,
        0
    );
    let epoch_health =
        olp::usage::ingestion::reconciliation::request_metadata_gateway_epoch_health(pool)
            .await
            .unwrap();
    assert_eq!(epoch_health.unresolved_epochs, 2);
    assert_eq!(epoch_health.historical_uncertain_gap_count, 2);
    assert_eq!(epoch_health.unresolved_event_lower_bound, 5);
    let unresolved_first_page =
        olp::usage::ingestion::delivery_health::request_metadata_gateway_epochs(
            pool,
            Some(GatewayEpochState::Unresolved),
            None,
            1,
        )
        .await
        .unwrap();
    assert_eq!(unresolved_first_page.items.len(), 1);
    let unresolved_cursor = unresolved_first_page.next_cursor.as_deref().unwrap();
    let unresolved_cursor = olp::database::cursor::Timestamp::parse(unresolved_cursor).unwrap();
    let unresolved_second_page =
        olp::usage::ingestion::delivery_health::request_metadata_gateway_epochs(
            pool,
            Some(GatewayEpochState::Unresolved),
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
    let acknowledgement_store = pool;
    let first_acknowledgement =
        olp::usage::ingestion::reconciliation::acknowledge_request_metadata_gateway_epoch(
            acknowledgement_store,
            &RequestProvenance {
                source_ip: Some("198.51.100.7".parse().unwrap()),
                user_agent_family: Some("olp-console".to_owned()),
            },
            loss_snapshot.process_epoch,
            owner_id,
        )
        .await
        .unwrap()
        .unwrap();
    assert_eq!(
        first_acknowledgement.process_epoch,
        loss_snapshot.process_epoch
    );
    assert_eq!(
        olp::usage::ingestion::reconciliation::acknowledge_request_metadata_gateway_epoch(
            pool,
            &olp::database::RequestProvenance::default(),
            loss_snapshot.process_epoch,
            owner_id
        )
        .await
        .unwrap()
        .unwrap(),
        first_acknowledgement
    );
    olp::usage::ingestion::reconciliation::acknowledge_request_metadata_gateway_epoch(
        pool,
        &olp::database::RequestProvenance::default(),
        stale_epoch,
        owner_id,
    )
    .await
    .unwrap()
    .unwrap();
    let acknowledged_health =
        olp::usage::ingestion::reconciliation::request_metadata_gateway_epoch_health(pool)
            .await
            .unwrap();
    assert_eq!(acknowledged_health.unresolved_epochs, 0);
    assert_eq!(acknowledged_health.historical_uncertain_gap_count, 2);
    assert_eq!(acknowledged_health.unresolved_event_lower_bound, 0);
    let acknowledged_epochs =
        olp::usage::ingestion::delivery_health::request_metadata_gateway_epochs(
            pool,
            Some(GatewayEpochState::Acknowledged),
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
    let acknowledgement_audit: (i64, Option<String>, Option<String>) = sqlx::query_as(
        "SELECT count(*), min(host(source_ip)), min(user_agent_family) FROM audit_events \
         WHERE action = 'request_metadata.gateway_epoch_acknowledge' \
           AND resource_id = $1 AND outcome = 'success'",
    )
    .bind(loss_snapshot.process_epoch.to_string())
    .fetch_one(pool)
    .await
    .unwrap();
    assert_eq!(
        acknowledgement_audit,
        (
            1,
            Some("198.51.100.7".to_owned()),
            Some("olp-console".to_owned())
        )
    );

    let request_page = olp::usage::history::requests(pool, &RequestFilters::default(), None, 50)
        .await
        .unwrap();
    assert_eq!(request_page.items.len(), 1);
    assert_eq!(request_page.items[0].id, request_id);
    assert_eq!(
        request_page.items[0].estimated_cost.as_deref(),
        // 100 input tokens of which 10 were cache reads, plus 50 output, at
        // 3/1/4 per million: (100 - 10) * 3 + 10 * 1 + 50 * 4. Billing the
        // cache reads at the full input rate would read 0.000500000000.
        Some("0.000480000000")
    );
    assert_eq!(
        olp::usage::history::request_detail(pool, request_id)
            .await
            .unwrap()
            .attempts
            .len(),
        1
    );
    assert_eq!(request_page.items[0].surface.as_str(), "anthropic");

    let pre_attempt_request_id = Uuid::now_v7();
    olp::usage::ingestion::persistence::persist_request_metadata_event(
        pool,
        &Event {
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
        },
    )
    .await
    .unwrap();
    let pre_attempt = olp::usage::history::request_detail(pool, pre_attempt_request_id)
        .await
        .unwrap();
    assert_eq!(pre_attempt.request.surface.as_str(), "gemini");
    assert_eq!(pre_attempt.request.attempt_count, 0);
    assert!(pre_attempt.attempts.is_empty());
    let pre_attempt_usage: i64 =
        sqlx::query_scalar("SELECT count(*) FROM attempt_usage_facts WHERE request_id = $1")
            .bind(pre_attempt_request_id)
            .fetch_one(pool)
            .await
            .unwrap();
    assert_eq!(pre_attempt_usage, 0);

    let filters = Filters {
        observed_after: observed_at - Duration::hours(1),
        observed_before: observed_at + Duration::hours(1),
        route_slug: None,
        provider_id: None,
        upstream_model: None,
        api_key_id: None,
        operation: None,
    };
    olp::usage::ingestion::delivery_health::report_request_metadata_consumer_health(
        pool, 0, 0, None,
    )
    .await
    .unwrap();
    let series_report =
        olp::usage::reports::series::usage_series(pool, &filters, Granularity::Hour)
            .await
            .unwrap();
    assert!(series_report.coverage.range_complete);
    let series = series_report.points;
    assert_eq!(series.len(), 1);
    assert_eq!(series[0].input_tokens, "100");
    let breakdown_report =
        olp::usage::reports::breakdown::usage_breakdown(pool, &filters, Dimension::Provider, 50)
            .await
            .unwrap();
    assert!(breakdown_report.coverage.range_complete);
    let breakdown = breakdown_report.items;
    assert_eq!(breakdown[0].dimension, provider_id.to_string());
    let completeness = olp::usage::reports::completeness::usage_completeness(pool, &filters)
        .await
        .unwrap();
    assert_eq!(completeness.request_count, 1);
    assert_eq!(completeness.priced_count, 1);
    assert_eq!(completeness.request_metadata_gap_events, 3);
    assert_eq!(completeness.uncertain_request_metadata_gap_count, 0);
    assert_eq!(
        completeness.request_metadata_consumer.state,
        ConsumerState::Healthy
    );
    assert!(!completeness.complete);
    let summary = olp::usage::reports::summary::usage_summary(pool, &filters)
        .await
        .unwrap();
    assert_eq!(summary.request_count, 1);
    assert_eq!(summary.cached_input_tokens, "10");
    assert_eq!(summary.estimated_cost.as_deref(), Some("0.000480000000"));
    assert_eq!(summary.currency.as_deref(), Some("USD"));
    assert_eq!(series[0].currency.as_deref(), Some("USD"));
    assert_eq!(breakdown[0].currency.as_deref(), Some("USD"));

    let unpriced_observed_at = Utc::now() - Duration::hours(5);
    olp::usage::ingestion::persistence::persist_request_metadata_event(
        pool,
        &Event {
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
                routing: None,
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
                usage: Some(olp::usage::emitter::RequestAttemptUsageMetadata {
                    observed: true,
                    complete: true,
                    billing_uncertain: false,
                    input_tokens: Some(1),
                    output_tokens: None,
                    cached_input_tokens: None,
                    media_units: None,
                }),
            }],
        },
    )
    .await
    .unwrap();
    let unpriced_filters = Filters {
        observed_after: unpriced_observed_at - Duration::minutes(10),
        observed_before: unpriced_observed_at + Duration::minutes(10),
        route_slug: None,
        provider_id: None,
        upstream_model: None,
        api_key_id: None,
        operation: Some("moderation".parse().unwrap()),
    };
    let unpriced = olp::usage::reports::completeness::usage_completeness(pool, &unpriced_filters)
        .await
        .unwrap();
    assert_eq!(unpriced.unpriced_count, 1);
    assert_eq!(unpriced.incomplete_count, 0);
    assert!(!unpriced.complete);

    let health = olp::observability::providers::provider_health(pool, 180, None, 50)
        .await
        .unwrap();
    assert_eq!(health.items.len(), 1);
    assert_eq!(health.items[0].status, "healthy");
    assert_eq!(health.items[0].attempt_count, 1);
    let generations = olp::runtime::history::runtime_generations(pool, None, 50)
        .await
        .unwrap();
    assert_ne!(
        generations.items[0].id, generation_id,
        "pricing publishes a new runtime price book"
    );
    assert!(
        generations
            .items
            .iter()
            .any(|generation| generation.id == generation_id),
        "historical request generations remain visible"
    );
    assert!(
        !olp::access::audit::audit_events(pool, None, 50, &AuditFilters::default())
            .await
            .unwrap()
            .items
            .is_empty()
    );
    let setup_audit = olp::access::audit::audit_events(
        pool,
        None,
        50,
        &AuditFilters {
            action: Some("installation.setup".to_owned()),
            resource_type: Some("installation".to_owned()),
            resource_id: Some("singleton".to_owned()),
            actor_user_id: Some(owner_id),
            outcome: Some("success".to_owned()),
            ..AuditFilters::default()
        },
    )
    .await
    .unwrap();
    assert_eq!(setup_audit.items.len(), 1);
    assert_eq!(
        setup_audit.items[0].actor_email.as_deref(),
        Some("owner@example.test")
    );
    let setup_at = setup_audit.items[0].occurred_at;
    assert!(
        olp::access::audit::audit_events(
            pool,
            None,
            50,
            &AuditFilters {
                action: Some("installation.setup".to_owned()),
                outcome: Some("failure".to_owned()),
                ..AuditFilters::default()
            },
        )
        .await
        .unwrap()
        .items
        .is_empty()
    );
    assert!(
        olp::access::audit::audit_events(
            pool,
            None,
            50,
            &AuditFilters {
                occurred_before: Some(setup_at - Duration::seconds(1)),
                ..AuditFilters::default()
            },
        )
        .await
        .unwrap()
        .items
        .is_empty()
    );
    assert!(
        !olp::access::audit::audit_events(
            pool,
            None,
            50,
            &AuditFilters {
                occurred_after: Some(setup_at),
                occurred_before: Some(setup_at),
                ..AuditFilters::default()
            },
        )
        .await
        .unwrap()
        .items
        .is_empty()
    );

    let setting = olp::settings::repository::settings(pool)
        .await
        .unwrap()
        .into_iter()
        .find(|setting| setting.key == "retention.requests_days")
        .unwrap();
    let updated = olp::settings::repository::update_setting(
        pool,
        &olp::database::RequestProvenance::default(),
        &setting.key,
        "31",
        setting.etag,
        owner_id,
    )
    .await
    .unwrap();
    assert_eq!(updated.value, "31");
    assert!(matches!(
        olp::settings::repository::update_setting(
            pool,
            &olp::database::RequestProvenance::default(),
            &setting.key,
            "32",
            setting.etag,
            owner_id
        )
        .await,
        Err(Error::PreconditionFailed)
    ));

    let usage_setting = olp::settings::repository::settings(pool)
        .await
        .unwrap()
        .into_iter()
        .find(|setting| setting.key == "retention.usage_days")
        .unwrap();
    assert!(matches!(
        olp::settings::repository::update_setting(
            pool,
            &olp::database::RequestProvenance::default(),
            &usage_setting.key,
            "0",
            usage_setting.etag,
            owner_id
        )
        .await,
        Err(Error::Invalid(_))
    ));
    let usage_setting = olp::settings::repository::update_setting(
        pool,
        &olp::database::RequestProvenance::default(),
        &usage_setting.key,
        "1",
        usage_setting.etag,
        owner_id,
    )
    .await
    .unwrap();
    assert_eq!(usage_setting.value, "1");

    let outage_setting = olp::settings::repository::settings(pool)
        .await
        .unwrap()
        .into_iter()
        .find(|setting| setting.key == "limits.valkey_unavailable")
        .unwrap();
    assert_eq!(outage_setting.value, "fail_closed");
    assert_eq!(
        olp::settings::repository::limits_valkey_unavailable_policy(pool,)
            .await
            .unwrap(),
        LimitsValkeyUnavailablePolicy::FailClosed
    );
    assert!(matches!(
        olp::settings::repository::update_setting(
            pool,
            &olp::database::RequestProvenance::default(),
            &outage_setting.key,
            "open",
            outage_setting.etag,
            owner_id
        )
        .await,
        Err(Error::Invalid(_))
    ));
    olp::settings::repository::update_setting(
        pool,
        &olp::database::RequestProvenance::default(),
        &outage_setting.key,
        "fail_open",
        outage_setting.etag,
        owner_id,
    )
    .await
    .unwrap();
    assert_eq!(
        olp::settings::repository::limits_valkey_unavailable_policy(pool,)
            .await
            .unwrap(),
        LimitsValkeyUnavailablePolicy::FailOpen
    );

    QueryFixture {
        observed_at,
        loss_epoch: loss_snapshot.process_epoch,
        stale_epoch,
        clean_epoch,
    }
}
