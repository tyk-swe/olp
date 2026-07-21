use olp_domain::{ApiKeyLimits, ApiKeyScope, OperationKind, RouteSlug, RuntimeSnapshot};
use olp_storage::{
    AuthHmacKey, ConfigurationError, IdempotencyOutcome, IdempotencyResponse, MasterKey,
    MediaJobState, MediaJobUpdate, NewApiKeyRecord, NewMediaJobReservation, InstallationSetupInput,
    NewProviderDraft, NewRouteDraft, NewRouteTarget, PgStore, ReplayableIdempotency,
    RotateCredentialInput, SessionMaterial, UpdateProvider, credential_aad,
    idempotency_fingerprint,
};
use uuid::Uuid;

#[tokio::test]
#[ignore = "requires an empty PostgreSQL 18 database in OLP_TEST_DATABASE_URL"]
async fn staged_provider_changes_do_not_leak_until_reactivation() {
    let database_url = std::env::var("OLP_TEST_DATABASE_URL")
        .expect("OLP_TEST_DATABASE_URL must point to an empty PostgreSQL 18 database");
    let store = PgStore::connect(&database_url, 5).await.unwrap();
    store.migrate().await.unwrap();
    let (owner, _) = store
        .setup_installation_with_session(
            InstallationSetupInput {
                installation_name: "Provider revisions".to_owned(),
                email: "owner@provider-revisions.test".to_owned(),
                display_name: "Owner".to_owned(),
                password_hash: "test-password-hash".to_owned(),
            },
            &SessionMaterial::generate(),
            chrono::Duration::hours(1),
        )
        .await
        .unwrap();
    let actor = owner.user_id;
    let master_key = MasterKey::new(1, [17; 32]);
    let provider_id = Uuid::now_v7();
    let first_credential_id = Uuid::now_v7();
    let model_id = Uuid::now_v7();
    let embedding_model_id = Uuid::now_v7();
    let first_credential = master_key
        .seal(
            b"first-provider-secret",
            &credential_aad(provider_id, first_credential_id, 1),
        )
        .unwrap();
    let provider_fingerprint = idempotency_fingerprint(&"provider-revision-create-01").unwrap();
    let provider = store
        .create_provider_draft(
            NewProviderDraft {
                provider_id,
                credential_id: Some(first_credential_id),
                model_id: Some(model_id),
                name: "revision-provider".to_owned(),
                kind: olp_domain::ProviderKind::OpenAi,
                endpoint: Some("https://old.example.test/v1/".to_owned()),
                cloud_region: None,
                cloud_project: None,
                deployment: None,
                api_version: None,
                auth_mode: "api_key".parse().unwrap(),
                connector_ready: true,
                credential: Some(first_credential),
                model: Some("model-old".to_owned()),
                display_name: Some("Model Old".to_owned()),
                model_enabled: true,
                surface: Some("openai".parse().unwrap()),
                actor,
                idempotency_key: "provider-revision-create-01".to_owned(),
            },
            ReplayableIdempotency::new(provider_fingerprint, &master_key),
            |_| IdempotencyResponse::new(201, None, None, Vec::new()),
        )
        .await
        .unwrap();
    let IdempotencyOutcome::Executed {
        value: provider, ..
    } = provider
    else {
        panic!("new provider creation must execute");
    };
    let initial_draft = store.get_provider(provider_id).await.unwrap();
    assert_eq!(initial_draft.active_revision, None);
    assert!(!initial_draft.pending_activation);
    sqlx::query(
        "INSERT INTO provider_models \
         (id, provider_id, upstream_model, display_name, enabled, discovered_at) \
         VALUES ($1, $2, 'embed-old', 'Embed Old', true, now())",
    )
    .bind(embedding_model_id)
    .bind(provider_id)
    .execute(store.pool())
    .await
    .unwrap();
    sqlx::query(
        "INSERT INTO model_capabilities \
         (provider_model_id, operation, surface, mode, source, certified_at) \
         VALUES ($1, 'embeddings', 'openai', 'unary', 'certified', now())",
    )
    .bind(embedding_model_id)
    .execute(store.pool())
    .await
    .unwrap();
    certify_all_draft_capabilities(&store, provider_id).await;
    store
        .record_provider_probe(
            provider_id,
            provider.etag,
            true,
            "initial revision probe",
            actor,
        )
        .await
        .unwrap();
    let first_activation = store
        .activate_provider(
            provider_id,
            provider.etag,
            actor,
            "provider-revision-activate-01",
        )
        .await
        .unwrap();
    let first_configuration = store.get_provider(provider_id).await.unwrap();
    assert_eq!(first_configuration.active_revision, Some(1));
    assert!(!first_configuration.pending_activation);

    let media_api_key_id = Uuid::now_v7();
    sqlx::query(
        "INSERT INTO api_keys
         (id, lookup_id, secret_digest, name, created_by)
         VALUES ($1, 'olpv2revisionmedia', $2, 'revision media key', $3)",
    )
    .bind(media_api_key_id)
    .bind([29_u8; 32].as_slice())
    .bind(actor)
    .execute(store.pool())
    .await
    .unwrap();
    let live_media_job_id = Uuid::now_v7();
    store
        .reserve_media_job(NewMediaJobReservation {
            id: live_media_job_id,
            runtime_generation_id: first_activation.release.generation_id,
            api_key_id: media_api_key_id,
            provider_id,
            upstream_model: "model-old".to_owned(),
            route_slug: "video-durable".to_owned(),
            operation: "video_create".parse().unwrap(),
            surface: "openai".parse().unwrap(),
        })
        .await
        .unwrap();
    let live_media_job = store
        .attach_media_job_upstream(
            live_media_job_id,
            "upstream-revision-video",
            MediaJobUpdate {
                state: MediaJobState::Queued,
                progress_percent: Some(0.0),
                content_available: false,
                expires_at: None,
                error_class: None,
                last_polled_at: chrono::Utc::now(),
            },
        )
        .await
        .unwrap();
    assert!(live_media_job.provider_revision_id.is_some());

    let route_fingerprint = idempotency_fingerprint(&"provider-revision-route-create-01").unwrap();
    let route = store
        .create_route_draft(
            NewRouteDraft {
                slug: "default".to_owned(),
                operations: vec![OperationKind::Generation, OperationKind::Embeddings],
                overall_timeout_ms: 30_000,
                max_attempts: 2,
                targets: vec![
                    NewRouteTarget {
                        provider_id,
                        upstream_model: "model-old".to_owned(),
                        priority: 0,
                        weight: 1,
                        timeout_ms: 20_000,
                    },
                    NewRouteTarget {
                        provider_id,
                        upstream_model: "embed-old".to_owned(),
                        priority: 0,
                        weight: 1,
                        timeout_ms: 20_000,
                    },
                ],
                actor,
                idempotency_key: "provider-revision-route-create-01".to_owned(),
            },
            ReplayableIdempotency::new(route_fingerprint, &master_key),
            |_| IdempotencyResponse::new(201, None, None, Vec::new()),
        )
        .await
        .unwrap();
    let IdempotencyOutcome::Executed { value: route, .. } = route else {
        panic!("new route creation must execute");
    };
    let (route_etag, _) = store
        .validate_route_draft(route.id, route.etag, actor)
        .await
        .unwrap();
    store
        .activate_route_draft(
            route.id,
            route_etag,
            actor,
            "provider-revision-route-activate-01",
        )
        .await
        .unwrap();

    let staged_etag = store
        .update_provider(
            provider_id,
            first_activation.etag,
            &UpdateProvider {
                name: "revision-provider-next".to_owned(),
                endpoint: Some("https://new.example.test/v1/".to_owned()),
                cloud_region: None,
                cloud_project: None,
                deployment: None,
                api_version: None,
                auth_mode: "api_key".parse().unwrap(),
            },
            actor,
        )
        .await
        .unwrap();
    let second_credential_id = Uuid::now_v7();
    let second_credential = master_key
        .seal(
            b"second-provider-secret",
            &credential_aad(provider_id, second_credential_id, 2),
        )
        .unwrap();
    let rotation_fingerprint = idempotency_fingerprint(&"provider-revision-rotate-01").unwrap();
    let rotation = store
        .rotate_provider_credential(
            provider_id,
            RotateCredentialInput {
                credential_id: second_credential_id,
                version: 2,
                encrypted: second_credential,
                expected_etag: staged_etag,
                actor,
                idempotency_key: "provider-revision-rotate-01".to_owned(),
            },
            ReplayableIdempotency::new(rotation_fingerprint, &master_key),
            |_| IdempotencyResponse::new(200, None, None, Vec::new()),
        )
        .await
        .unwrap();
    let IdempotencyOutcome::Executed {
        value: rotation, ..
    } = rotation
    else {
        panic!("new credential rotation must execute");
    };
    assert!(rotation.release.is_none());
    let staged_configuration = store.get_provider(provider_id).await.unwrap();
    assert_eq!(staged_configuration.active_revision, Some(1));
    assert!(staged_configuration.pending_activation);
    assert_eq!(
        staged_configuration.runtime_credential_id,
        Some(first_credential_id)
    );
    assert_eq!(staged_configuration.runtime_credential_version, Some(1));
    assert_eq!(
        staged_configuration.draft_credential_id,
        Some(second_credential_id)
    );
    assert_eq!(staged_configuration.draft_credential_version, Some(2));
    let credentials = store
        .list_provider_credentials(provider_id, None, 10)
        .await
        .unwrap()
        .items;
    let runtime_credential = credentials
        .iter()
        .find(|credential| credential.id == first_credential_id)
        .unwrap();
    assert!(runtime_credential.active);
    assert!(!runtime_credential.draft_selected);
    let draft_credential = credentials
        .iter()
        .find(|credential| credential.id == second_credential_id)
        .unwrap();
    assert!(!draft_credential.active);
    assert!(draft_credential.draft_selected);
    for (credential_id, idempotency_key) in [
        (first_credential_id, "provider-revision-revoke-runtime-01"),
        (second_credential_id, "provider-revision-revoke-draft-01"),
    ] {
        assert!(matches!(
            store
                .revoke_provider_credential(
                    provider_id,
                    credential_id,
                    rotation.etag,
                    actor,
                    idempotency_key,
                )
                .await,
            Err(ConfigurationError::InUse)
        ));
    }

    // A key publication compiles the last activated provider revision, not the
    // endpoint and credential currently being tested in the mutable draft.
    let auth_hmac_key = AuthHmacKey::new([23; 32]);
    let key_material = auth_hmac_key.generate_api_key();
    let key_fingerprint = idempotency_fingerprint(&"provider-revision-key-create-01").unwrap();
    let key_creation = store
        .create_api_key_record(
            &NewApiKeyRecord {
                name: "revision test key".to_owned(),
                material: key_material,
                scopes: vec![ApiKeyScope::Inference],
                allowed_routes: vec![RouteSlug::parse("default").unwrap()],
                limits: ApiKeyLimits::default(),
                expires_at: None,
                actor,
                idempotency_key: "provider-revision-key-create-01".to_owned(),
            },
            ReplayableIdempotency::new(key_fingerprint, &master_key),
            |_| IdempotencyResponse::new(201, None, None, Vec::new()),
        )
        .await
        .unwrap();
    let IdempotencyOutcome::Executed {
        value: key_creation,
        ..
    } = key_creation
    else {
        panic!("new key creation must execute");
    };
    let staged_publication: RuntimeSnapshot =
        serde_json::from_slice(&key_creation.release.payload).unwrap();
    let staged_provider = staged_publication.providers.values().next().unwrap();
    assert_eq!(staged_provider.name, "revision-provider");
    assert_eq!(
        staged_provider.active_credential.unwrap().as_uuid(),
        first_credential_id
    );
    assert_eq!(
        staged_publication
            .routes
            .get(&RouteSlug::parse("default").unwrap())
            .unwrap()
            .targets[0]
            .upstream_model,
        "model-old"
    );
    let staged_secret = store
        .runtime_provider_configurations(&staged_publication)
        .await
        .unwrap();
    assert_eq!(
        staged_secret[0].endpoint.as_deref(),
        Some("https://old.example.test/v1/")
    );
    assert_eq!(staged_secret[0].credential_id, Some(first_credential_id));

    certify_all_draft_capabilities(&store, provider_id).await;
    store
        .record_provider_probe(
            provider_id,
            rotation.etag,
            true,
            "replacement revision probe",
            actor,
        )
        .await
        .unwrap();
    assert!(matches!(
        store
            .activate_provider(
                provider_id,
                rotation.etag,
                actor,
                "provider-revision-activate-live-media-01",
            )
            .await,
        Err(ConfigurationError::ProviderIncomplete)
    ));
    store
        .begin_media_job_deletion(live_media_job_id)
        .await
        .unwrap();
    assert!(
        store
            .finalize_media_job_deletion(live_media_job_id)
            .await
            .unwrap()
    );
    let second_activation = store
        .activate_provider(
            provider_id,
            rotation.etag,
            actor,
            "provider-revision-activate-02",
        )
        .await
        .unwrap();
    let activated: RuntimeSnapshot =
        serde_json::from_slice(&second_activation.release.payload).unwrap();
    let activated_configuration = store.get_provider(provider_id).await.unwrap();
    assert_eq!(activated_configuration.active_revision, Some(2));
    assert!(!activated_configuration.pending_activation);
    let activated_provider = activated.providers.values().next().unwrap();
    assert_eq!(activated_provider.name, "revision-provider-next");
    assert_eq!(
        activated_provider.active_credential.unwrap().as_uuid(),
        second_credential_id
    );
    let activated_secret = store
        .runtime_provider_configurations(&activated)
        .await
        .unwrap();
    assert_eq!(
        activated_secret[0].endpoint.as_deref(),
        Some("https://new.example.test/v1/")
    );
    assert_eq!(
        activated_secret[0].credential_id,
        Some(second_credential_id)
    );
    let revisions: i64 =
        sqlx::query_scalar("SELECT count(*) FROM provider_revisions WHERE provider_id = $1")
            .bind(provider_id)
            .fetch_one(store.pool())
            .await
            .unwrap();
    assert_eq!(revisions, 2);
    let first_revoked: bool = sqlx::query_scalar(
        "SELECT revoked_at IS NOT NULL FROM provider_credential_versions WHERE id = $1",
    )
    .bind(first_credential_id)
    .fetch_one(store.pool())
    .await
    .unwrap();
    assert!(first_revoked);

    let history = store
        .list_provider_revisions(provider_id, None, 10)
        .await
        .unwrap();
    assert_eq!(history.items.len(), 2);
    assert_eq!(history.items[0].revision, 2);
    assert_eq!(history.items[1].revision, 1);
    let second_revision_id = history.items[0].id;
    let first_revision_id = history.items[1].id;
    let first_revision = store
        .get_provider_revision(provider_id, first_revision_id)
        .await
        .unwrap();
    assert_eq!(
        first_revision.credential_version_id,
        Some(first_credential_id)
    );
    assert_eq!(first_revision.credential_version, Some(1));
    let diff = store
        .diff_provider_revisions(provider_id, first_revision_id, second_revision_id)
        .await
        .unwrap();
    assert_eq!((diff.from_revision, diff.to_revision), (1, 2));
    assert!(diff.name_changed);
    assert!(diff.endpoint_changed);
    assert!(diff.credential_changed);

    let restored = store
        .restore_provider_revision_as_draft(
            provider_id,
            first_revision_id,
            activated_configuration.etag,
            actor,
            "provider-revision-restore-01",
        )
        .await
        .unwrap();
    assert_eq!(restored.state, olp_domain::ProviderState::Draft);
    assert_eq!(
        restored.endpoint.as_deref(),
        Some("https://old.example.test/v1/")
    );
    assert_eq!(restored.active_revision, Some(2));
    assert!(restored.pending_activation);
    assert_eq!(restored.draft_credential_id, Some(second_credential_id));
    assert_eq!(restored.draft_credential_version, Some(2));
    assert_eq!(restored.runtime_credential_id, Some(second_credential_id));
    assert!(restored.last_probe_at.is_none());
    let restored_models = store
        .list_provider_models(provider_id, None, 100)
        .await
        .unwrap();
    assert!(restored_models.next_cursor.is_none());
    assert!(restored_models.items.iter().all(|model| {
        model.capabilities.iter().all(|capability| {
            capability.source == olp_domain::CapabilitySource::Declared
                && capability.certified_at.is_none()
        })
    }));
    assert!(matches!(
        store
            .restore_provider_revision_as_draft(
                provider_id,
                first_revision_id,
                activated_configuration.etag,
                actor,
                "provider-revision-restore-01",
            )
            .await,
        Err(ConfigurationError::IdempotencyConflict)
    ));
    let restore_audit: i64 = sqlx::query_scalar(
        "SELECT count(*) FROM audit_events \
         WHERE actor_user_id = $1 AND action = 'provider_revision.restore_as_draft' \
           AND resource_id = $2 AND outcome = 'success'",
    )
    .bind(actor)
    .bind(provider_id.to_string())
    .fetch_one(store.pool())
    .await
    .unwrap();
    assert_eq!(restore_audit, 1);
}

async fn certify_all_draft_capabilities(store: &PgStore, provider_id: Uuid) {
    sqlx::query(
        "UPDATE model_capabilities SET source = 'certified', certified_at = now() \
         WHERE provider_model_id IN (SELECT id FROM provider_models WHERE provider_id = $1)",
    )
    .bind(provider_id)
    .execute(store.pool())
    .await
    .unwrap();
}
