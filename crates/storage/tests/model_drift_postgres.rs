use olp_domain::ProviderKind;
use olp_storage::{
    CapabilityCertificationOutcome, CatalogError, DiscoveredModelInput, IdempotencyOutcome,
    IdempotencyResponse, MasterKey, NewOwner, NewProviderDraft, PgStore,
    ProviderModelDiscoveryCompleteness, ProviderModelDiscoveryOrigin,
    ReconcileProviderModelDiscoveryInput, ReplayableIdempotency, SessionMaterial, credential_aad,
    hash_password, idempotency_fingerprint,
};
use sqlx::Row;
use uuid::Uuid;

trait ExpectExecuted<T> {
    fn expect_executed(self) -> T;
}

impl<T> ExpectExecuted<T> for IdempotencyOutcome<T> {
    fn expect_executed(self) -> T {
        match self {
            Self::Executed { value, .. } => value,
            Self::Replayed(_) => panic!("fresh integration operation replayed"),
        }
    }
}

fn replay<'a>(master_key: &'a MasterKey, seed: &str) -> ReplayableIdempotency<'a> {
    ReplayableIdempotency::new(idempotency_fingerprint(&seed).unwrap(), master_key)
}

fn empty_created_response<T>(_: &T) -> Result<IdempotencyResponse, olp_storage::PersistenceError> {
    IdempotencyResponse::new(201, None, None, Vec::new())
}

#[tokio::test]
#[ignore = "requires an empty PostgreSQL 18 database in OLP_TEST_DATABASE_URL"]
async fn authoritative_model_drift_preserves_live_evidence_and_recovers_expired_runs() {
    let database_url = std::env::var("OLP_TEST_DATABASE_URL")
        .expect("OLP_TEST_DATABASE_URL must point to an empty PostgreSQL 18 database");
    let store = PgStore::connect(&database_url, 5).await.unwrap();
    store.migrate().await.unwrap();
    let session = SessionMaterial::generate();
    let (owner, _) = store
        .setup_owner_with_session(
            NewOwner {
                organization_name: "Model drift integration".to_owned(),
                email: "owner@model-drift.test".to_owned(),
                display_name: "Owner".to_owned(),
                password_hash: hash_password("correct horse battery staple").unwrap(),
            },
            &session,
            chrono::Duration::hours(12),
        )
        .await
        .unwrap();
    let actor = owner.user_id;
    let master_key = MasterKey::new(1, [8; 32]);
    let provider_id = Uuid::now_v7();
    let credential_id = Uuid::now_v7();
    let model_id = Uuid::now_v7();
    let credential = master_key
        .seal(
            b"provider-secret",
            &credential_aad(provider_id, credential_id, 1),
        )
        .unwrap();
    let created = store
        .create_provider_draft(
            NewProviderDraft {
                provider_id,
                credential_id: Some(credential_id),
                model_id: Some(model_id),
                name: "drift-openai".to_owned(),
                kind: ProviderKind::OpenAi,
                endpoint: Some("https://api.openai.com/v1/".to_owned()),
                cloud_region: None,
                cloud_project: None,
                deployment: None,
                api_version: None,
                auth_mode: "api_key".parse().unwrap(),
                connector_ready: true,
                credential: Some(credential),
                model: Some("gpt-drift".to_owned()),
                display_name: Some("GPT Drift".to_owned()),
                model_enabled: true,
                surface: Some("open_ai".parse().unwrap()),
                actor,
                idempotency_key: "model-drift-provider-create".to_owned(),
            },
            replay(&master_key, "model-drift-provider-create"),
            empty_created_response,
        )
        .await
        .unwrap()
        .expect_executed();
    store
        .record_provider_probe(
            provider_id,
            created.etag,
            true,
            "credentialed discovery succeeded",
            actor,
        )
        .await
        .unwrap();
    let initial = store
        .get_provider_model_catalog(provider_id, model_id)
        .await
        .unwrap();
    let certified = store
        .apply_compatible_capability_certification(
            provider_id,
            model_id,
            created.etag,
            actor,
            &initial
                .capabilities
                .iter()
                .map(|capability| CapabilityCertificationOutcome {
                    operation: capability.operation,
                    surface: capability.surface,
                    mode: capability.mode,
                    succeeded: true,
                })
                .collect::<Vec<_>>(),
        )
        .await
        .unwrap();
    let activated = store
        .activate_provider(
            provider_id,
            certified.etag,
            actor,
            "model-drift-activate-initial",
        )
        .await
        .unwrap();
    assert_eq!(activated.release.sequence, 1);

    let observation = DiscoveredModelInput {
        upstream_model: "gpt-drift".to_owned(),
        display_name: "GPT Drift".to_owned(),
        enabled: false,
        capabilities: Vec::new(),
    };
    let observed = store
        .reconcile_provider_model_discovery(ReconcileProviderModelDiscoveryInput {
            provider_id,
            expected_etag: activated.etag,
            models: std::slice::from_ref(&observation),
            origin: ProviderModelDiscoveryOrigin::Upstream,
            completeness: ProviderModelDiscoveryCompleteness::Complete,
            actor: Some(actor),
            claim_id: None,
        })
        .await
        .unwrap();
    let after_observation = store
        .get_provider_model_catalog(provider_id, model_id)
        .await
        .unwrap();
    assert_eq!(after_observation.inventory_source, "upstream");
    assert_eq!(after_observation.availability, "available");
    assert!(
        after_observation
            .capabilities
            .iter()
            .all(|capability| capability.source == olp_domain::CapabilitySource::Certified)
    );

    let first_miss = store
        .reconcile_provider_model_discovery(ReconcileProviderModelDiscoveryInput {
            provider_id,
            expected_etag: observed.etag,
            models: &[],
            origin: ProviderModelDiscoveryOrigin::Upstream,
            completeness: ProviderModelDiscoveryCompleteness::Complete,
            actor: Some(actor),
            claim_id: None,
        })
        .await
        .unwrap();
    assert_eq!(first_miss.newly_missing_model_count, 0);
    assert_eq!(
        store
            .get_provider_model_catalog(provider_id, model_id)
            .await
            .unwrap()
            .availability,
        "available"
    );

    let second_miss = store
        .reconcile_provider_model_discovery(ReconcileProviderModelDiscoveryInput {
            provider_id,
            expected_etag: first_miss.etag,
            models: &[],
            origin: ProviderModelDiscoveryOrigin::Upstream,
            completeness: ProviderModelDiscoveryCompleteness::Complete,
            actor: Some(actor),
            claim_id: None,
        })
        .await
        .unwrap();
    assert_eq!(second_miss.newly_missing_model_count, 1);
    let missing_provider = store.get_provider_catalog(provider_id).await.unwrap();
    assert!(missing_provider.pending_activation);
    assert_eq!(missing_provider.active_revision, Some(1));
    let missing_model = store
        .get_provider_model_catalog(provider_id, model_id)
        .await
        .unwrap();
    assert_eq!(missing_model.availability, "missing");
    assert!(
        missing_model
            .capabilities
            .iter()
            .all(|capability| capability.source == olp_domain::CapabilitySource::Declared)
    );

    let reappeared = store
        .reconcile_provider_model_discovery(ReconcileProviderModelDiscoveryInput {
            provider_id,
            expected_etag: second_miss.etag,
            models: &[observation],
            origin: ProviderModelDiscoveryOrigin::Upstream,
            completeness: ProviderModelDiscoveryCompleteness::Complete,
            actor: Some(actor),
            claim_id: None,
        })
        .await
        .unwrap();
    let reappeared_model = store
        .get_provider_model_catalog(provider_id, model_id)
        .await
        .unwrap();
    assert_eq!(reappeared_model.availability, "available");
    assert!(
        reappeared_model
            .capabilities
            .iter()
            .all(|capability| capability.source == olp_domain::CapabilitySource::Declared)
    );

    let running = store
        .begin_capability_certification(provider_id, model_id, reappeared.etag, actor)
        .await
        .unwrap();
    assert!(matches!(
        store
            .begin_capability_certification(provider_id, model_id, reappeared.etag, actor)
            .await,
        Err(CatalogError::InUse)
    ));
    sqlx::query(
        "UPDATE capability_certification_runs SET lease_expires_at = now() - interval '1 second' \
         WHERE id = $1",
    )
    .bind(running.run_id)
    .execute(store.pool())
    .await
    .unwrap();
    let replacement = store
        .begin_capability_certification(provider_id, model_id, reappeared.etag, actor)
        .await
        .unwrap();
    assert_ne!(replacement.run_id, running.run_id);
    let stale_status: String =
        sqlx::query("SELECT status FROM capability_certification_runs WHERE id = $1")
            .bind(running.run_id)
            .fetch_one(store.pool())
            .await
            .unwrap()
            .get("status");
    assert_eq!(stale_status, "superseded");
}
