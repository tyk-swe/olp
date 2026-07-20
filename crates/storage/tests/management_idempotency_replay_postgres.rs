use chrono::{Duration, Utc};
use olp_domain::{OperationKind, ProviderKind};
use olp_storage::{
    ConfigurationError, IdempotencyOutcome, IdempotencyResponse, MasterKey, NewOwner,
    NewProviderDraft, NewRouteDraft, NewRouteTarget, OperationsError, PgStore, PriceInput,
    ReplayableIdempotency, RotateCredentialInput, credential_aad, hash_password,
    idempotency_fingerprint,
};
use serde_json::json;
use sqlx::Executor as _;
use uuid::Uuid;

#[tokio::test]
#[ignore = "requires an empty PostgreSQL 18 database in OLP_TEST_DATABASE_URL"]
async fn remaining_management_mutations_exactly_replay_without_double_execution() {
    let database_url = std::env::var("OLP_TEST_DATABASE_URL")
        .expect("OLP_TEST_DATABASE_URL must point to an empty PostgreSQL 18 database");
    let store = PgStore::connect(&database_url, 8).await.unwrap();
    store.migrate().await.unwrap();
    let owner = store
        .setup_owner(NewOwner {
            installation_name: "Management replay integration".to_owned(),
            email: "owner@management-replay.test".to_owned(),
            display_name: "Owner".to_owned(),
            password_hash: hash_password("correct horse battery staple").unwrap(),
        })
        .await
        .unwrap();
    let actor = owner.user_id;
    let master_key = MasterKey::new(11, [61; 32]);

    let provider_key = "provider-replay-001";
    let provider_fingerprint = idempotency_fingerprint(&json!({
        "name": "replay-provider",
        "kind": "openai",
        "credential_sha256": "stable-provider-secret",
        "model": "replay-model"
    }))
    .unwrap();
    let provider_id = Uuid::now_v7();
    let credential_id = Uuid::now_v7();
    let model_id = Uuid::now_v7();
    let provider_secret = master_key
        .seal(
            b"provider-secret",
            &credential_aad(provider_id, credential_id, 1),
        )
        .unwrap();
    let first_provider = store
        .create_provider_draft(
            provider_input(
                provider_id,
                credential_id,
                model_id,
                provider_secret,
                actor,
                provider_key,
                "replay-provider",
            ),
            ReplayableIdempotency::new(provider_fingerprint, &master_key),
            |created| {
                IdempotencyResponse::json(
                    201,
                    &json!({
                        "id": created.provider_id,
                        "name": "replay-provider",
                        "kind": "openai",
                        "state": "draft",
                        "model": "replay-model",
                        "etag": created.etag
                    }),
                    Some(format!("\"{}\"", created.etag)),
                )
            },
        )
        .await
        .unwrap();
    let IdempotencyOutcome::Executed {
        value: created_provider,
        response: provider_response,
    } = first_provider
    else {
        panic!("first provider draft must execute");
    };
    let provider_parts = provider_response.into_parts();
    assert_eq!(provider_parts.0, 201);
    assert_eq!(
        provider_parts.2.as_deref(),
        Some(format!("\"{}\"", created_provider.etag).as_str())
    );

    // The HTTP layer generates fresh candidate IDs and encryption nonces on a
    // retry. They must be ignored once the durable replay is found.
    let replay_provider_id = Uuid::now_v7();
    let replay_credential_id = Uuid::now_v7();
    let replay_model_id = Uuid::now_v7();
    let replay_provider_secret = master_key
        .seal(
            b"provider-secret",
            &credential_aad(replay_provider_id, replay_credential_id, 1),
        )
        .unwrap();
    let replayed_provider = store
        .create_provider_draft(
            provider_input(
                replay_provider_id,
                replay_credential_id,
                replay_model_id,
                replay_provider_secret,
                actor,
                provider_key,
                "replay-provider",
            ),
            ReplayableIdempotency::new(provider_fingerprint, &master_key),
            |_| panic!("provider replay must not rebuild its response"),
        )
        .await
        .unwrap();
    let IdempotencyOutcome::Replayed(response) = replayed_provider else {
        panic!("identical provider draft must replay");
    };
    assert_eq!(response.into_parts(), provider_parts);
    assert_eq!(
        sqlx::query_scalar::<_, i64>("SELECT count(*) FROM providers")
            .fetch_one(store.pool())
            .await
            .unwrap(),
        1
    );

    let changed_provider_fingerprint =
        idempotency_fingerprint(&json!({"name": "changed-provider"})).unwrap();
    let changed_provider_id = Uuid::now_v7();
    let changed_credential_id = Uuid::now_v7();
    let changed_secret = master_key
        .seal(
            b"changed-provider-secret",
            &credential_aad(changed_provider_id, changed_credential_id, 1),
        )
        .unwrap();
    assert!(matches!(
        store
            .create_provider_draft(
                provider_input(
                    changed_provider_id,
                    changed_credential_id,
                    Uuid::now_v7(),
                    changed_secret,
                    actor,
                    provider_key,
                    "changed-provider",
                ),
                ReplayableIdempotency::new(changed_provider_fingerprint, &master_key),
                |_| panic!("mismatched provider request must not execute"),
            )
            .await,
        Err(ConfigurationError::IdempotencyConflict)
    ));

    sqlx::query(
        "UPDATE model_capabilities SET source = 'certified', certified_at = now() \
         WHERE provider_model_id IN (SELECT id FROM provider_models WHERE provider_id = $1)",
    )
    .bind(provider_id)
    .execute(store.pool())
    .await
    .unwrap();
    store
        .record_provider_probe(
            provider_id,
            created_provider.etag,
            true,
            "replay provider probe",
            actor,
        )
        .await
        .unwrap();
    let activated_provider = store
        .activate_provider(
            provider_id,
            created_provider.etag,
            actor,
            "provider-replay-activate-001",
        )
        .await
        .unwrap();

    let route_key = "route-replay-001";
    let route_fingerprint = idempotency_fingerprint(&json!({
        "slug": "replay-route",
        "operations": ["generation"],
        "target": {"provider_id": provider_id, "model": "replay-model"}
    }))
    .unwrap();
    let first_route = store
        .create_route_draft(
            route_input(actor, provider_id, route_key, "replay-route"),
            ReplayableIdempotency::new(route_fingerprint, &master_key),
            |created| {
                IdempotencyResponse::json(
                    201,
                    &json!({
                        "id": created.id,
                        "slug": created.slug.as_str(),
                        "state": "draft",
                        "etag": created.etag
                    }),
                    Some(format!("\"{}\"", created.etag)),
                )
            },
        )
        .await
        .unwrap();
    let IdempotencyOutcome::Executed {
        value: created_route,
        response: route_response,
    } = first_route
    else {
        panic!("first route draft must execute");
    };
    let route_parts = route_response.into_parts();
    assert_eq!(route_parts.0, 201);
    assert_eq!(
        route_parts.2.as_deref(),
        Some(format!("\"{}\"", created_route.etag).as_str())
    );
    let replayed_route = store
        .create_route_draft(
            route_input(actor, provider_id, route_key, "replay-route"),
            ReplayableIdempotency::new(route_fingerprint, &master_key),
            |_| panic!("route replay must not rebuild its response"),
        )
        .await
        .unwrap();
    let IdempotencyOutcome::Replayed(response) = replayed_route else {
        panic!("identical route draft must replay");
    };
    assert_eq!(response.into_parts(), route_parts);
    assert_eq!(
        sqlx::query_scalar::<_, i64>("SELECT count(*) FROM route_drafts")
            .fetch_one(store.pool())
            .await
            .unwrap(),
        1
    );
    let changed_route_fingerprint =
        idempotency_fingerprint(&json!({"slug": "changed-route"})).unwrap();
    assert!(matches!(
        store
            .create_route_draft(
                route_input(actor, provider_id, route_key, "changed-route"),
                ReplayableIdempotency::new(changed_route_fingerprint, &master_key),
                |_| panic!("mismatched route request must not execute"),
            )
            .await,
        Err(ConfigurationError::IdempotencyConflict)
    ));

    let rotation_key = "provider-credential-replay-001";
    let rotation_fingerprint = idempotency_fingerprint(&json!({
        "provider_id": provider_id,
        "expected_etag": activated_provider.etag,
        "credential_sha256": "rotated-provider-secret"
    }))
    .unwrap();
    let rotation_credential_id = Uuid::now_v7();
    let rotation_secret = master_key
        .seal(
            b"rotated-provider-secret",
            &credential_aad(provider_id, rotation_credential_id, 2),
        )
        .unwrap();
    let first_rotation = store
        .rotate_provider_credential(
            provider_id,
            RotateCredentialInput {
                credential_id: rotation_credential_id,
                version: 2,
                encrypted: rotation_secret,
                expected_etag: activated_provider.etag,
                actor,
                idempotency_key: rotation_key.to_owned(),
            },
            ReplayableIdempotency::new(rotation_fingerprint, &master_key),
            |result| {
                IdempotencyResponse::json(
                    201,
                    &json!({
                        "provider_id": provider_id,
                        "etag": result.etag,
                        "credential_id": rotation_credential_id,
                        "credential_version": 2,
                        "runtime_generation": null
                    }),
                    Some(format!("\"{}\"", result.etag)),
                )
            },
        )
        .await
        .unwrap();
    let IdempotencyOutcome::Executed {
        value: rotation,
        response: rotation_response,
    } = first_rotation
    else {
        panic!("first provider credential rotation must execute");
    };
    assert!(rotation.release.is_none());
    let rotation_parts = rotation_response.into_parts();
    assert_eq!(rotation_parts.0, 201);
    assert_eq!(
        rotation_parts.2.as_deref(),
        Some(format!("\"{}\"", rotation.etag).as_str())
    );

    // The original If-Match is stale after a successful rotation. Exact replay
    // must still win before either ETag or candidate-version validation.
    let discarded_credential_id = Uuid::now_v7();
    let discarded_secret = master_key
        .seal(
            b"rotated-provider-secret",
            &credential_aad(provider_id, discarded_credential_id, 3),
        )
        .unwrap();
    let replayed_rotation = store
        .rotate_provider_credential(
            provider_id,
            RotateCredentialInput {
                credential_id: discarded_credential_id,
                version: 3,
                encrypted: discarded_secret,
                expected_etag: activated_provider.etag,
                actor,
                idempotency_key: rotation_key.to_owned(),
            },
            ReplayableIdempotency::new(rotation_fingerprint, &master_key),
            |_| panic!("credential rotation replay must not rebuild its response"),
        )
        .await
        .unwrap();
    let IdempotencyOutcome::Replayed(response) = replayed_rotation else {
        panic!("identical provider credential rotation must replay");
    };
    assert_eq!(response.into_parts(), rotation_parts);
    assert_eq!(
        sqlx::query_scalar::<_, i64>(
            "SELECT count(*) FROM provider_credential_versions WHERE provider_id = $1",
        )
        .bind(provider_id)
        .fetch_one(store.pool())
        .await
        .unwrap(),
        2
    );

    let changed_precondition_fingerprint = idempotency_fingerprint(&json!({
        "provider_id": provider_id,
        "expected_etag": rotation.etag,
        "credential_sha256": "rotated-provider-secret"
    }))
    .unwrap();
    let changed_precondition_id = Uuid::now_v7();
    let changed_precondition_secret = master_key
        .seal(
            b"rotated-provider-secret",
            &credential_aad(provider_id, changed_precondition_id, 3),
        )
        .unwrap();
    assert!(matches!(
        store
            .rotate_provider_credential(
                provider_id,
                RotateCredentialInput {
                    credential_id: changed_precondition_id,
                    version: 3,
                    encrypted: changed_precondition_secret,
                    expected_etag: rotation.etag,
                    actor,
                    idempotency_key: rotation_key.to_owned(),
                },
                ReplayableIdempotency::new(changed_precondition_fingerprint, &master_key),
                |_| panic!("changed rotation precondition must not execute"),
            )
            .await,
        Err(ConfigurationError::IdempotencyConflict)
    ));

    let effective_at = Utc::now() - Duration::hours(1);
    let prices = vec![PriceInput {
        provider_kind: olp_domain::ProviderKind::OpenAi,
        provider_id: None,
        model: "replay-model".to_owned(),
        operation: olp_domain::OperationKind::Generation,
        input_per_million: Some("1.250000000000".to_owned()),
        output_per_million: Some("2.500000000000".to_owned()),
        unit_price: None,
        currency: "USD".to_owned(),
    }];
    let pricing_key = "pricing-replay-001";
    let pricing_fingerprint = idempotency_fingerprint(&json!({
        "effective_at": effective_at,
        "prices": [{
            "provider_kind": "openai",
            "model": "replay-model",
            "operation": "generation",
            "input_per_million": "1.250000000000",
            "output_per_million": "2.500000000000",
            "currency": "USD"
        }]
    }))
    .unwrap();
    let first_pricing = store
        .create_pricing_revision(
            actor,
            pricing_key,
            effective_at,
            &prices,
            ReplayableIdempotency::new(pricing_fingerprint, &master_key),
            |revision| {
                IdempotencyResponse::json(
                    201,
                    &json!({
                        "id": revision.id,
                        "revision": revision.revision,
                        "effective_at": revision.effective_at,
                        "created_at": revision.created_at
                    }),
                    None,
                )
            },
        )
        .await
        .unwrap();
    let IdempotencyOutcome::Executed {
        value: pricing,
        response: pricing_response,
    } = first_pricing
    else {
        panic!("first pricing revision must execute");
    };
    assert_eq!(pricing.revision, 1);
    let pricing_parts = pricing_response.into_parts();
    let replayed_pricing = store
        .create_pricing_revision(
            actor,
            pricing_key,
            effective_at,
            &prices,
            ReplayableIdempotency::new(pricing_fingerprint, &master_key),
            |_| panic!("pricing replay must not rebuild its response"),
        )
        .await
        .unwrap();
    let IdempotencyOutcome::Replayed(response) = replayed_pricing else {
        panic!("identical pricing revision must replay");
    };
    assert_eq!(response.into_parts(), pricing_parts);
    assert_eq!(
        sqlx::query_scalar::<_, i64>("SELECT count(*) FROM pricing_revisions")
            .fetch_one(store.pool())
            .await
            .unwrap(),
        1
    );
    let changed_pricing_fingerprint =
        idempotency_fingerprint(&json!({"effective_at": effective_at, "price": "changed"}))
            .unwrap();
    assert!(matches!(
        store
            .create_pricing_revision(
                actor,
                pricing_key,
                effective_at,
                &prices,
                ReplayableIdempotency::new(changed_pricing_fingerprint, &master_key),
                |_| panic!("mismatched pricing request must not execute"),
            )
            .await,
        Err(OperationsError::IdempotencyConflict)
    ));

    let concurrent_key = "pricing-concurrent-001";
    let concurrent_scope =
        format!("olp:v2:idempotency:{actor}:pricing_revision.create:{concurrent_key}");
    let mut concurrent_transaction = store.pool().begin().await.unwrap();
    concurrent_transaction
        .execute(
            sqlx::query("SELECT pg_advisory_xact_lock(hashtextextended($1::text, 0))")
                .bind(&concurrent_scope),
        )
        .await
        .unwrap();
    assert!(matches!(
        store
            .create_pricing_revision(
                actor,
                concurrent_key,
                effective_at,
                &prices,
                ReplayableIdempotency::new([83; 32], &master_key),
                |_| panic!("in-progress pricing request must not execute"),
            )
            .await,
        Err(OperationsError::IdempotencyInProgress)
    ));
    concurrent_transaction.rollback().await.unwrap();
}

fn provider_input(
    provider_id: Uuid,
    credential_id: Uuid,
    model_id: Uuid,
    credential: olp_storage::EncryptedSecret,
    actor: Uuid,
    idempotency_key: &str,
    name: &str,
) -> NewProviderDraft {
    NewProviderDraft {
        provider_id,
        credential_id: Some(credential_id),
        model_id: Some(model_id),
        name: name.to_owned(),
        kind: ProviderKind::OpenAi,
        endpoint: Some("https://api.openai.com/v1/".to_owned()),
        cloud_region: None,
        cloud_project: None,
        deployment: None,
        api_version: None,
        auth_mode: "api_key".parse().unwrap(),
        connector_ready: true,
        credential: Some(credential),
        model: Some("replay-model".to_owned()),
        display_name: Some("Replay Model".to_owned()),
        model_enabled: true,
        surface: Some("openai".parse().unwrap()),
        actor,
        idempotency_key: idempotency_key.to_owned(),
    }
}

fn route_input(actor: Uuid, provider_id: Uuid, idempotency_key: &str, slug: &str) -> NewRouteDraft {
    NewRouteDraft {
        slug: slug.to_owned(),
        operations: vec![OperationKind::Generation],
        overall_timeout_ms: 30_000,
        max_attempts: 1,
        targets: vec![NewRouteTarget {
            provider_id,
            provider_model: "replay-model".to_owned(),
            priority: 0,
            weight: 1,
            timeout_ms: 20_000,
        }],
        actor,
        idempotency_key: idempotency_key.to_owned(),
    }
}
