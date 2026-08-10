use super::*;

pub(super) async fn exercise(store: &PgStore, actor: Uuid, master_key: &MasterKey) -> (Uuid, Uuid) {
    let provider_id = Uuid::now_v7();
    let credential_id = Uuid::now_v7();
    let model_id = Uuid::now_v7();
    let encrypted = master_key
        .seal(
            b"provider-secret",
            &credential_aad(provider_id, credential_id, 1),
        )
        .unwrap();
    let provider = store
        .create_provider_draft(
            NewProviderDraft {
                provider_id,
                credential_id: Some(credential_id),
                model_id: Some(model_id),
                name: "primary-openai".to_owned(),
                kind: ProviderKind::OpenAi,
                endpoint: Some("https://api.openai.com/v1/".to_owned()),
                cloud_region: None,
                cloud_project: None,
                deployment: None,
                api_version: None,
                auth_mode: "api_key".parse().unwrap(),
                connector_ready: true,
                credential: Some(encrypted),
                model: Some("gpt-test".to_owned()),
                display_name: Some("GPT Test".to_owned()),
                model_enabled: true,
                surface: Some("openai".parse().unwrap()),
                actor,
                idempotency_key: "provider-configuration-0001".to_owned(),
            },
            test_replay(master_key, "provider-configuration-0001"),
            empty_created_response,
        )
        .await
        .unwrap()
        .expect_executed();
    assert_eq!(store.list_providers(None, 10).await.unwrap().items.len(), 1);
    let first = store.get_provider(provider_id).await.unwrap();
    assert_eq!(first.model_count, 1);
    assert_eq!(
        provider_models(store, provider_id).await[0]
            .capabilities
            .len(),
        2
    );
    let vertex_id = Uuid::now_v7();
    let vertex = store
        .create_provider_draft(
            NewProviderDraft {
                provider_id: vertex_id,
                credential_id: None,
                model_id: Some(Uuid::now_v7()),
                name: "vertex-draft".to_owned(),
                kind: ProviderKind::VertexAi,
                endpoint: None,
                cloud_region: Some("us-central1".to_owned()),
                cloud_project: Some("project-test".to_owned()),
                deployment: None,
                api_version: None,
                auth_mode: "adc".parse().unwrap(),
                connector_ready: false,
                credential: None,
                model: Some("gemini-test".to_owned()),
                display_name: Some("Gemini Test".to_owned()),
                model_enabled: false,
                surface: Some("gemini".parse().unwrap()),
                actor,
                idempotency_key: "provider-vertex-draft-01".to_owned(),
            },
            test_replay(master_key, "provider-vertex-draft-01"),
            empty_created_response,
        )
        .await
        .unwrap()
        .expect_executed();
    let vertex_record = store.get_provider(vertex_id).await.unwrap();
    assert!(!vertex_record.connector_ready);
    assert!(!provider_models(store, vertex_id).await[0].enabled);
    assert!(
        store
            .activate_provider(vertex_id, vertex.etag, actor, "provider-activate-vertex-01")
            .await
            .is_err()
    );
    for impossible in [
        CapabilityRecord {
            operation: "image_generation".parse().unwrap(),
            surface: "gemini".parse().unwrap(),
            mode: "unary".parse().unwrap(),
            source: olp_domain::CapabilitySource::Declared,
            certified_at: None,
        },
        CapabilityRecord {
            operation: "generation".parse().unwrap(),
            surface: "openai".parse().unwrap(),
            mode: "async".parse().unwrap(),
            source: olp_domain::CapabilitySource::Declared,
            certified_at: None,
        },
    ] {
        assert!(matches!(
            store
                .set_provider_model_enabled(
                    vertex_id,
                    vertex.model_id.unwrap(),
                    true,
                    &[impossible],
                    vertex.etag,
                    actor,
                )
                .await,
            Err(ConfigurationError::Invalid(_))
        ));
    }
    assert!(matches!(
        store
            .activate_provider(
                provider_id,
                provider.etag,
                actor,
                "provider-activate-before-fresh-probe-01",
            )
            .await,
        Err(ConfigurationError::ProviderIncomplete)
    ));
    let probed_at = store
        .record_provider_probe(
            provider_id,
            provider.etag,
            true,
            "connector configuration accepted",
            actor,
        )
        .await
        .unwrap();
    assert!(probed_at <= chrono::Utc::now());

    let discovered_etag = store
        .discover_provider_models(
            provider_id,
            provider.etag,
            &[DiscoveredModelInput {
                upstream_model: "gpt-test".to_owned(),
                display_name: "GPT Test".to_owned(),
                enabled: true,
                capabilities: vec![
                    CapabilityRecord {
                        operation: "generation".parse().unwrap(),
                        surface: "openai".parse().unwrap(),
                        mode: "unary".parse().unwrap(),
                        source: olp_domain::CapabilitySource::Probed,
                        certified_at: None,
                    },
                    CapabilityRecord {
                        operation: "generation".parse().unwrap(),
                        surface: "openai".parse().unwrap(),
                        mode: "streaming".parse().unwrap(),
                        source: olp_domain::CapabilitySource::Probed,
                        certified_at: None,
                    },
                ],
            }],
            actor,
        )
        .await
        .unwrap();
    let discovered = store.get_provider(provider_id).await.unwrap();
    assert!(discovered.last_probe_at.is_none());
    assert!(discovered.last_probe_status.is_none());
    assert!(matches!(
        store
            .record_provider_probe(
                provider_id,
                provider.etag,
                true,
                "late success from the pre-discovery configuration",
                actor,
            )
            .await,
        Err(ConfigurationError::PreconditionFailed)
    ));
    let after_stale_probe = store.get_provider(provider_id).await.unwrap();
    assert!(after_stale_probe.last_probe_at.is_none());
    assert!(after_stale_probe.last_probe_status.is_none());
    assert!(matches!(
        store
            .activate_provider(
                provider_id,
                discovered_etag,
                actor,
                "provider-activate-after-stale-probe-01",
            )
            .await,
        Err(ConfigurationError::ProviderIncomplete)
    ));
    certify_all_capabilities(store, provider_id).await;
    store
        .record_provider_probe(
            provider_id,
            discovered_etag,
            true,
            "fresh post-discovery probe",
            actor,
        )
        .await
        .unwrap();
    let activated = store
        .activate_provider(
            provider_id,
            discovered_etag,
            actor,
            "provider-activate-openai-01",
        )
        .await
        .unwrap();
    assert_eq!(activated.release.sequence, 1);
    assert!(matches!(
        store
            .activate_provider(
                provider_id,
                discovered_etag,
                actor,
                "provider-activate-openai-01",
            )
            .await,
        Err(olp_storage::configuration::ConfigurationError::IdempotencyConflict)
    ));
    let initial_runtime: RuntimeSnapshot =
        serde_json::from_slice(&activated.release.payload).unwrap();
    let initial_secrets = store
        .runtime_provider_configurations(&initial_runtime)
        .await
        .unwrap();
    assert_eq!(initial_secrets.len(), 1);
    assert_eq!(initial_secrets[0].credential_id, Some(credential_id));

    let next_version = store
        .next_credential_version_candidate(provider_id)
        .await
        .unwrap();
    let rotated_credential_id = Uuid::now_v7();
    let rotated_encrypted = master_key
        .seal(
            b"rotated-provider-secret",
            &credential_aad(provider_id, rotated_credential_id, next_version),
        )
        .unwrap();
    let rotation = store
        .rotate_provider_credential(
            provider_id,
            RotateCredentialInput {
                credential_id: rotated_credential_id,
                version: next_version,
                encrypted: rotated_encrypted,
                expected_etag: activated.etag,
                actor,
                idempotency_key: "provider-rotate-0001".to_owned(),
            },
            test_replay(master_key, "provider-credential-rotate-01"),
            empty_created_response,
        )
        .await
        .unwrap()
        .expect_executed();
    assert!(rotation.release.is_none());
    let staged_secrets = store
        .runtime_provider_configurations(&initial_runtime)
        .await
        .unwrap();
    assert_eq!(staged_secrets[0].credential_id, Some(credential_id));
    certify_all_capabilities(store, provider_id).await;
    store
        .record_provider_probe(
            provider_id,
            rotation.etag,
            true,
            "rotated credential probe succeeded",
            actor,
        )
        .await
        .unwrap();
    let rotation_activation = store
        .activate_provider(
            provider_id,
            rotation.etag,
            actor,
            "provider-rotate-activate-0001",
        )
        .await
        .unwrap();
    let rotation_release = rotation_activation.release.clone();
    assert_eq!(rotation_release.sequence, 2);
    assert!(matches!(
        store
            .runtime_provider_configurations(&initial_runtime)
            .await,
        Err(ConfigurationError::InvalidCredential)
    ));
    let rotated_runtime: RuntimeSnapshot =
        serde_json::from_slice(&rotation_release.payload).unwrap();
    let rotated_secrets = store
        .runtime_provider_configurations(&rotated_runtime)
        .await
        .unwrap();
    assert_eq!(
        rotated_secrets[0].credential_id,
        Some(rotated_credential_id)
    );
    let mut impossible_future_runtime = rotated_runtime.clone();
    impossible_future_runtime
        .providers
        .get_mut(&ProviderId::from_uuid(provider_id))
        .unwrap()
        .active_credential = Some(CredentialVersionId::from_uuid(Uuid::now_v7()));
    assert!(matches!(
        store
            .runtime_provider_configurations(&impossible_future_runtime)
            .await,
        Err(ConfigurationError::InvalidCredential)
    ));
    assert!(
        store
            .revoke_provider_credential(
                provider_id,
                rotated_credential_id,
                rotation_activation.etag,
                actor,
                "credential-active-revoke-01",
            )
            .await
            .is_err()
    );
    let revoked_etag = store
        .revoke_provider_credential(
            provider_id,
            credential_id,
            rotation_activation.etag,
            actor,
            "credential-old-revoke-0001",
        )
        .await
        .unwrap();

    let route = store
        .create_route_draft(
            NewRouteDraft {
                slug: "default".to_owned(),
                operations: vec![OperationKind::Generation],
                overall_timeout_ms: 30_000,
                max_attempts: 1,
                targets: vec![NewRouteTarget {
                    provider_id,
                    upstream_model: "gpt-test".to_owned(),
                    priority: 0,
                    weight: 1,
                    timeout_ms: 20_000,
                }],
                actor,
                idempotency_key: "route-configuration-create-01".to_owned(),
            },
            test_replay(master_key, "route-configuration-create-01"),
            empty_created_response,
        )
        .await
        .unwrap()
        .expect_executed();
    let simulation = store
        .simulate_route_draft(
            route.id,
            "generation".parse().unwrap(),
            "openai".parse().unwrap(),
            "streaming".parse().unwrap(),
            "stable-affinity",
        )
        .await
        .unwrap();
    assert_eq!(simulation.targets[0].attempt, Some(1));
    let (validated_etag, _) = store
        .validate_route_draft(route.id, route.etag, actor)
        .await
        .unwrap();
    let first_revision = store
        .activate_route_draft(
            route.id,
            validated_etag,
            actor,
            "route-configuration-activate-01",
        )
        .await
        .unwrap();
    assert_eq!(first_revision.release.sequence, 3);
    let draft = store.get_route_draft(route.id).await.unwrap();
    let second_draft_etag = store
        .replace_route_draft(
            route.id,
            draft.etag,
            &ReplaceRouteDraftInput {
                slug: "default".to_owned(),
                operations: vec!["generation".parse().unwrap()],
                overall_timeout_ms: 40_000,
                max_attempts: 1,
                targets: vec![(model_id, 0, 5, 25_000)],
            },
            actor,
        )
        .await
        .unwrap();
    let (second_validated_etag, _) = store
        .validate_route_draft(route.id, second_draft_etag, actor)
        .await
        .unwrap();
    let second_revision = store
        .activate_route_draft(
            route.id,
            second_validated_etag,
            actor,
            "route-configuration-activate-02",
        )
        .await
        .unwrap();
    let diff = store
        .diff_route_revisions(
            first_revision.route_id,
            first_revision.revision_id,
            second_revision.revision_id,
        )
        .await
        .unwrap();
    assert!(diff.timeout_changed);
    assert_eq!(diff.targets_changed.len(), 1);
    let first_revision_record = store
        .get_route_revision(first_revision.route_id, first_revision.revision_id)
        .await
        .unwrap();
    let restored = store
        .restore_route_revision_as_draft(
            first_revision.route_id,
            first_revision.revision_id,
            actor,
            "route-configuration-restore-01",
        )
        .await
        .unwrap();
    assert_eq!(
        restored.based_on_revision_id,
        Some(first_revision.revision_id)
    );
    assert_eq!(
        restored.operations,
        vec![olp_domain::OperationKind::Generation]
    );
    assert_eq!(restored.overall_timeout_ms, 30_000);
    assert_eq!(restored.max_attempts, 1);
    assert_eq!(restored.routing_id, first_revision_record.routing_id);
    assert_eq!(restored.targets.len(), 1);
    assert_ne!(restored.targets[0].id, first_revision_record.targets[0].id);
    assert_eq!(
        restored.targets[0].routing_id,
        first_revision_record.targets[0].routing_id
    );
    assert_eq!(restored.targets[0].provider_model_id, model_id);
    assert_eq!(restored.targets[0].weight, 1);
    assert_eq!(restored.targets[0].timeout_ms, 20_000);
    assert!(matches!(
        store
            .replace_route_draft(
                restored.id,
                restored.etag,
                &ReplaceRouteDraftInput {
                    slug: "forked-route".to_owned(),
                    operations: restored.operations.clone(),
                    overall_timeout_ms: restored.overall_timeout_ms,
                    max_attempts: restored.max_attempts,
                    targets: restored
                        .targets
                        .iter()
                        .map(|target| (
                            target.provider_model_id,
                            target.priority,
                            target.weight,
                            target.timeout_ms,
                        ))
                        .collect(),
                },
                actor,
            )
            .await,
        Err(ConfigurationError::Invalid(message)) if message.contains("stable slug")
    ));
    assert_eq!(
        store.get_route_draft(restored.id).await.unwrap().slug,
        "default"
    );

    let auth_hmac_key = AuthHmacKey::new([9; 32]);
    let material = auth_hmac_key.generate_api_key();
    let key_fingerprint = idempotency_fingerprint(&"api-key-configuration-create-01").unwrap();
    let key = store
        .create_api_key_record(
            &NewApiKeyRecord {
                name: "SDK key".to_owned(),
                material,
                scopes: vec![ApiKeyScope::Inference, ApiKeyScope::ModelsRead],
                allowed_routes: vec![RouteSlug::parse("default").unwrap()],
                limits: ApiKeyLimits::default(),
                expires_at: None,
                owner: olp_domain::ApiKeyOwner::User(olp_domain::UserId::from_uuid(actor)),
                team_id: None,
                project_id: None,
                actor,
                idempotency_key: "api-key-configuration-create-01".to_owned(),
            },
            ReplayableIdempotency::new(key_fingerprint, master_key),
            |_| IdempotencyResponse::new(201, None, None, Vec::new()),
        )
        .await
        .unwrap();
    let IdempotencyOutcome::Executed { value: key, .. } = key else {
        panic!("new API-key request must execute");
    };
    assert_eq!(key.release.sequence, 5);
    let key_creation_release = key.release.clone();
    let initial_key_record = store.get_api_key(key.id).await.unwrap();
    assert!(matches!(
        store
            .update_api_key(
                key.id,
                initial_key_record.etag,
                &UpdateApiKeyInput {
                    name: "duplicate scopes".to_owned(),
                    scopes: vec!["inference".to_owned(), "inference".to_owned()],
                    allowed_routes: Vec::new(),
                    requests_per_minute: None,
                    tokens_per_minute: None,
                    max_concurrency: None,
                    expires_at: None,
                },
                actor,
            )
            .await,
        Err(olp_storage::configuration::ConfigurationError::Invalid(_))
    ));
    let key_update = store
        .update_api_key(
            key.id,
            initial_key_record.etag,
            &UpdateApiKeyInput {
                name: "Updated SDK key".to_owned(),
                scopes: vec!["inference".to_owned()],
                allowed_routes: Vec::new(),
                requests_per_minute: Some(60),
                tokens_per_minute: Some(10_000),
                max_concurrency: Some(4),
                expires_at: Some(Utc::now() + Duration::days(30)),
            },
            actor,
        )
        .await
        .unwrap();
    assert_eq!(key_update.release.sequence, 6);
    let key_record = store.get_api_key(key.id).await.unwrap();
    assert_eq!(key_record.name, "Updated SDK key");
    assert_eq!(key_record.scopes, vec!["inference"]);
    assert!(key_record.allowed_routes.is_empty());
    assert_eq!(key_record.requests_per_minute, Some(60));
    assert_eq!(key_record.tokens_per_minute, Some(10_000));
    assert_eq!(key_record.max_concurrency, Some(4));
    assert_eq!(key_record.etag, key_update.etag);
    let updated_runtime: RuntimeSnapshot =
        serde_json::from_slice(&key_update.release.payload).unwrap();
    let updated_runtime_key = updated_runtime
        .api_keys
        .values()
        .find(|runtime_key| runtime_key.id.as_uuid() == key.id)
        .unwrap();
    assert_eq!(
        updated_runtime_key
            .limits
            .requests_per_minute
            .map(std::num::NonZeroU32::get),
        Some(60)
    );
    assert!(updated_runtime_key.allowed_routes.is_empty());
    let replacement = auth_hmac_key.generate_api_key();
    let rotation_fingerprint = idempotency_fingerprint(&"api-key-configuration-rotate-01").unwrap();
    let key_rotation = store
        .rotate_api_key(
            RotateApiKeyInput {
                id: key.id,
                material: &replacement,
                expected_etag: key_record.etag,
                actor,
                idempotency_key: "api-key-configuration-rotate-01",
            },
            ReplayableIdempotency::new(rotation_fingerprint, master_key),
            |_| IdempotencyResponse::new(200, None, None, Vec::new()),
        )
        .await
        .unwrap();
    let IdempotencyOutcome::Executed {
        value: key_rotation,
        ..
    } = key_rotation
    else {
        panic!("new API-key rotation must execute");
    };
    assert_eq!(key_rotation.release.sequence, 7);
    assert_ne!(key.lookup_id, key_rotation.lookup_id);

    let key_revocation = store
        .revoke_api_key_record(
            key.id,
            key_rotation.etag,
            actor,
            "api-key-configuration-revoke-01",
        )
        .await
        .unwrap();
    assert_eq!(key_revocation.release.sequence, 8);
    let current_api_keys = store.current_runtime_api_keys().await.unwrap();
    assert!(
        !current_api_keys
            .keys()
            .any(|lookup_id| lookup_id.as_str() == key.lookup_id)
    );
    assert!(
        !current_api_keys
            .keys()
            .any(|lookup_id| lookup_id.as_str() == key_rotation.lookup_id)
    );

    let mut historical_runtime: RuntimeSnapshot =
        serde_json::from_slice(&key_creation_release.payload).unwrap();
    assert!(
        historical_runtime
            .api_keys
            .keys()
            .any(|lookup| lookup.as_str() == key.lookup_id)
    );
    historical_runtime.api_keys = current_api_keys;
    assert!(historical_runtime.api_keys.is_empty());

    let corrupt_generation_id = Uuid::now_v7();
    let corrupt_sequence: i64 = sqlx::query_scalar(
        "INSERT INTO runtime_generations \
         (id, compiled_release, release_sha256, created_by) VALUES ($1, $2, $3, $4) \
         RETURNING sequence",
    )
    .bind(corrupt_generation_id)
    .bind(b"corrupt runtime envelope".as_slice())
    .bind([0_u8; 32].as_slice())
    .bind(actor)
    .fetch_one(store.pool())
    .await
    .unwrap();
    assert!(corrupt_sequence > key_revocation.release.sequence);
    let recent_valid = store.recent_valid_runtime_releases(16).await.unwrap();
    assert_eq!(recent_valid[0].sequence, key_revocation.release.sequence);
    assert!(
        recent_valid
            .iter()
            .all(|release| release.generation_id != corrupt_generation_id)
    );
    sqlx::query("DELETE FROM runtime_generations WHERE id = $1")
        .bind(corrupt_generation_id)
        .execute(store.pool())
        .await
        .unwrap();

    let audit_actions: Vec<String> = sqlx::query_scalar(
        "SELECT action FROM audit_events WHERE action LIKE 'provider.%' OR action LIKE 'route.%' \
         OR action LIKE 'api_key.%'",
    )
    .fetch_all(store.pool())
    .await
    .unwrap();
    for action in [
        "provider.probe",
        "provider.discover",
        "provider.rotate_credential",
        "provider.revoke_credential",
        "route.update_draft",
        "route.restore_as_draft",
        "api_key.update",
        "api_key.rotate",
        "api_key.revoke",
    ] {
        assert!(
            audit_actions.iter().any(|stored| stored == action),
            "missing {action}"
        );
    }

    // ETags remain concurrency tokens across unrelated probe evidence updates.
    assert_ne!(revoked_etag, provider.etag);

    (provider_id, revoked_etag)
}
