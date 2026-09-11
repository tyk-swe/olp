use olp::crypto::{
    aad::credential,
    envelope::{EncryptedSecret, MasterKey},
};
use olp::providers::pool::CredentialSlot;
use olp::runtime::snapshot::Snapshot;
use sha2::{Digest, Sha256};
use sqlx::PgPool;
use uuid::Uuid;

async fn owner(pool: &PgPool) -> Uuid {
    let id = Uuid::now_v7();
    sqlx::query("INSERT INTO users(id,email,display_name,role) VALUES($1,'owner@provider-pools.test','Owner','owner')").bind(id).execute(pool).await.unwrap();
    id
}
async fn provider(pool: &PgPool, owner: Uuid, name: &str) -> Uuid {
    let id = Uuid::now_v7();
    sqlx::query("INSERT INTO providers(id,name,kind,auth_mode,etag,created_by,state) VALUES($1,$2,'openai','api_key',$3,$4,'active')").bind(id).bind(name).bind(Uuid::now_v7()).bind(owner).execute(pool).await.unwrap();
    id
}
async fn secret(pool: &PgPool, owner: Uuid, provider: Uuid, version: i32, key: &MasterKey) -> Uuid {
    let id = Uuid::now_v7();
    let sealed = key
        .seal(
            b"pool-fixture-secret",
            &credential(provider, id, version as u32),
        )
        .unwrap();
    sqlx::query("INSERT INTO provider_credential_versions(id,provider_id,version,ciphertext,nonce,master_key_version,created_by) VALUES($1,$2,$3,$4,$5,1,$6)").bind(id).bind(provider).bind(version).bind(sealed.ciphertext).bind(sealed.nonce.to_vec()).bind(owner).execute(pool).await.unwrap();
    id
}
async fn revision(
    pool: &PgPool,
    owner: Uuid,
    provider: Uuid,
    credential: Uuid,
    number: i32,
) -> Uuid {
    let id = Uuid::now_v7();
    sqlx::query("INSERT INTO provider_revisions(id,provider_id,revision,name,kind,auth_mode,connector_ready,credential_version_id,source_etag,activated_by) SELECT $1,id,$2,name,kind,auth_mode,true,$3,etag,$4 FROM providers WHERE id=$5").bind(id).bind(number).bind(credential).bind(owner).bind(provider).execute(pool).await.unwrap();
    id
}

#[tokio::test]
#[ignore = "requires PostgreSQL"]
async fn migration_preserves_secret_identity_and_wraps_all_historical_releases() {
    let db = olp::test_support::TestDb::create_empty("pool_upgrade").await;
    let pool = db.pool(2).await;
    sqlx::query("CREATE SCHEMA olp_v3")
        .execute(&pool)
        .await
        .unwrap();
    sqlx::raw_sql(include_str!("../../migrations/0001_initial.sql"))
        .execute(&pool)
        .await
        .unwrap();
    let owner = owner(&pool).await;
    let provider = provider(&pool, owner, "legacy-provider").await;
    let master = MasterKey::new(1, [61; 32]);
    let first = secret(&pool, owner, provider, 1, &master).await;
    let second = secret(&pool, owner, provider, 2, &master).await;
    let historical = revision(&pool, owner, provider, first, 1).await;
    let current = revision(&pool, owner, provider, second, 2).await;
    sqlx::query(
        "UPDATE providers SET active_revision_id=$1,active_credential_version_id=$2 WHERE id=$3",
    )
    .bind(current)
    .bind(second)
    .bind(provider)
    .execute(&pool)
    .await
    .unwrap();
    let generation = Uuid::now_v7();
    let raw = serde_json::json!({"generation":{"id":generation,"ordinal":1,"activated_at":chrono::Utc::now()},"providers":{},"routes":{},"api_keys":{}});
    let payload = serde_json::to_vec(&raw).unwrap();
    sqlx::query("INSERT INTO runtime_generations(id,compiled_release,release_sha256,created_by) VALUES($1,$2,$3,$4)").bind(generation).bind(&payload).bind(Sha256::digest(&payload).to_vec()).bind(owner).execute(&pool).await.unwrap();
    for migration in [
        include_str!("../../migrations/0002_provider_flexibility.sql"),
        include_str!("../../migrations/0003_routing_policies.sql"),
        include_str!("../../migrations/0004_runtime_routing_envelope.sql"),
        include_str!("../../migrations/0005_credentialless_slot_policies.sql"),
    ] {
        sqlx::raw_sql(migration).execute(&pool).await.unwrap();
    }
    let slots = olp::providers::pool_store::list(&pool, provider)
        .await
        .unwrap();
    assert_eq!(slots.len(), 1);
    assert_eq!(slots[0].id, provider);
    assert_eq!(slots[0].credential_version_id, Some(second));
    let pinned:Uuid=sqlx::query_scalar("SELECT credential_version_id FROM provider_revision_credentials WHERE provider_revision_id=$1").bind(historical).fetch_one(&pool).await.unwrap();
    assert_eq!(pinned, first);
    let (ciphertext, nonce): (Vec<u8>, Vec<u8>) = sqlx::query_as(
        "SELECT ciphertext,nonce FROM provider_credential_versions WHERE id=$1 AND slot_id=$2",
    )
    .bind(first)
    .bind(provider)
    .fetch_one(&pool)
    .await
    .unwrap();
    assert_eq!(
        &*master
            .open(
                &EncryptedSecret {
                    key_version: 1,
                    ciphertext,
                    nonce: nonce.try_into().unwrap()
                },
                &credential(provider, first, 1)
            )
            .unwrap(),
        b"pool-fixture-secret"
    );
    let (payload, checksum): (Vec<u8>, Vec<u8>) = sqlx::query_as(
        "SELECT compiled_release,release_sha256 FROM runtime_generations WHERE id=$1",
    )
    .bind(generation)
    .fetch_one(&pool)
    .await
    .unwrap();
    assert_eq!(checksum, Sha256::digest(&payload).to_vec());
    assert_eq!(
        serde_json::from_slice::<serde_json::Value>(&payload).unwrap()["format"],
        "olp-routing-v1"
    );
    assert_eq!(
        Snapshot::from_persisted_slice(&payload)
            .unwrap()
            .generation
            .id
            .as_uuid(),
        generation
    );
}

#[tokio::test]
#[ignore = "requires PostgreSQL"]
async fn credential_pools_enforce_ownership_and_keep_revision_pins_on_rotation() {
    let db = olp::test_support::TestDb::create_migrated("pool_revision").await;
    let pool = db.pool(3).await;
    let owner = owner(&pool).await;
    let a = provider(&pool, owner, "a").await;
    let b = provider(&pool, owner, "b").await;
    let master = MasterKey::new(1, [62; 32]);
    let a_slot = Uuid::now_v7();
    let b_slot = Uuid::now_v7();
    for (slot, provider) in [(a_slot, a), (b_slot, b)] {
        sqlx::query(
            "INSERT INTO provider_credential_slots(id,provider_id,name) VALUES($1,$2,'Account')",
        )
        .bind(slot)
        .bind(provider)
        .execute(&pool)
        .await
        .unwrap();
    }
    let first = secret(&pool, owner, a, 1, &master).await;
    assert!(
        sqlx::query("UPDATE provider_credential_versions SET slot_id=$1 WHERE id=$2")
            .bind(b_slot)
            .bind(first)
            .execute(&pool)
            .await
            .is_err()
    );
    sqlx::query("UPDATE provider_credential_versions SET slot_id=$1 WHERE id=$2")
        .bind(a_slot)
        .bind(first)
        .execute(&pool)
        .await
        .unwrap();
    assert!(
        sqlx::query("UPDATE provider_credential_slots SET selected_version_id=$1 WHERE id=$2")
            .bind(first)
            .bind(b_slot)
            .execute(&pool)
            .await
            .is_err()
    );
    sqlx::query("UPDATE provider_credential_slots SET selected_version_id=$1,validated_at=now(),allowed_models=ARRAY['model-a'],weight=3 WHERE id=$2").bind(first).bind(a_slot).execute(&pool).await.unwrap();
    let first_revision = revision(&pool, owner, a, first, 1).await;
    let mut tx = pool.begin().await.unwrap();
    olp::providers::pool_store::snapshot(&mut tx, a, first_revision)
        .await
        .unwrap();
    tx.commit().await.unwrap();
    let second = secret(&pool, owner, a, 2, &master).await;
    sqlx::query("UPDATE provider_credential_versions SET slot_id=$1 WHERE id=$2")
        .bind(a_slot)
        .bind(second)
        .execute(&pool)
        .await
        .unwrap();
    sqlx::query("UPDATE provider_credential_slots SET selected_version_id=$1,validated_at=NULL,weight=10 WHERE id=$2").bind(second).bind(a_slot).execute(&pool).await.unwrap();
    let second_revision = revision(&pool, owner, a, second, 2).await;
    let mut tx = pool.begin().await.unwrap();
    assert!(
        olp::providers::pool_store::snapshot(&mut tx, a, second_revision)
            .await
            .is_err()
    );
    tx.rollback().await.unwrap();
    let historical: sqlx::types::Json<CredentialSlot> = sqlx::query_scalar(
        "SELECT configuration FROM provider_revision_credentials WHERE provider_revision_id=$1",
    )
    .bind(first_revision)
    .fetch_one(&pool)
    .await
    .unwrap();
    assert_eq!(historical.credential_version_id, Some(first));
    assert_eq!(historical.weight, 3);
    sqlx::query("UPDATE provider_credential_slots SET validated_at=now() WHERE id=$1")
        .bind(a_slot)
        .execute(&pool)
        .await
        .unwrap();
    let mut tx = pool.begin().await.unwrap();
    olp::providers::pool_store::snapshot(&mut tx, a, second_revision)
        .await
        .unwrap();
    tx.commit().await.unwrap();
    sqlx::query("UPDATE providers SET updated_at=now()+interval '1 hour' WHERE id=$1")
        .bind(a)
        .execute(&pool)
        .await
        .unwrap();
    let unchanged_revision = revision(&pool, owner, a, second, 3).await;
    let mut tx = pool.begin().await.unwrap();
    olp::providers::pool_store::snapshot(&mut tx, a, unchanged_revision)
        .await
        .unwrap();
    tx.commit().await.unwrap();
    let model = Uuid::now_v7();
    sqlx::query("INSERT INTO provider_models(id,provider_id,upstream_model,display_name,enabled) VALUES($1,$2,'model-b','Model B',true)").bind(model).bind(a).execute(&pool).await.unwrap();
    let validated: Option<chrono::DateTime<chrono::Utc>> =
        sqlx::query_scalar("SELECT validated_at FROM provider_credential_slots WHERE id=$1")
            .bind(a_slot)
            .fetch_one(&pool)
            .await
            .unwrap();
    assert!(validated.is_none());

    // A pool-only change must appear in the revision review even when the
    // legacy default credential reference is identical.
    sqlx::query("UPDATE provider_credential_slots SET weight=20,validated_at=now() WHERE id=$1")
        .bind(a_slot)
        .execute(&pool)
        .await
        .unwrap();
    let pool_revision = revision(&pool, owner, a, second, 4).await;
    let mut tx = pool.begin().await.unwrap();
    olp::providers::pool_store::snapshot(&mut tx, a, pool_revision)
        .await
        .unwrap();
    tx.commit().await.unwrap();
    assert!(
        olp::providers::revisions::diff_provider_revisions(
            &pool,
            a,
            unchanged_revision,
            pool_revision
        )
        .await
        .unwrap()
        .credential_changed
    );

    // Restoring an extra-slot-only revision disables the default slot and
    // restores restrictions, while retaining each slot's current secret.
    let default_secret = secret(&pool, owner, a, 3, &master).await;
    sqlx::query("UPDATE providers SET active_credential_version_id=$2 WHERE id=$1")
        .bind(a)
        .bind(default_secret)
        .execute(&pool)
        .await
        .unwrap();
    let current = olp::providers::repository::get_provider(&pool, a)
        .await
        .unwrap();
    olp::providers::revisions::restore_provider_revision_as_draft(
        &pool,
        &Default::default(),
        a,
        first_revision,
        current.etag,
        owner,
        "restore-pool-fixture",
    )
    .await
    .unwrap();
    let slots = olp::providers::pool_store::list(&pool, a).await.unwrap();
    let default = slots.iter().find(|slot| slot.id == a).unwrap();
    assert!(!default.enabled);
    assert_eq!(default.credential_version_id, Some(default_secret));
    let restored = slots.iter().find(|slot| slot.id == a_slot).unwrap();
    assert!(restored.enabled);
    assert_eq!(restored.weight, 3);
    assert_eq!(restored.allowed_models, ["model-a"]);
    assert_eq!(restored.credential_version_id, Some(second));
}

#[tokio::test]
#[ignore = "requires PostgreSQL"]
async fn policy_only_route_revisions_are_visible_in_history_comparisons_and_restores() {
    let db = olp::test_support::TestDb::create_migrated("policy_history").await;
    let pool = db.pool(3).await;
    let actor = owner(&pool).await;
    let provider =
        crate::support::route_fixtures::insert_provider(&pool, actor, "policy-history").await;
    let draft = crate::support::route_fixtures::insert_unbased_route_draft(
        &pool,
        actor,
        "policy-history",
        &[provider.model_id],
    )
    .await;
    let provenance = olp::database::RequestProvenance::default();
    let (etag, _) =
        olp::routes::drafts::validate_route_draft(&pool, &provenance, draft.id, draft.etag, actor)
            .await
            .unwrap();
    let first = olp::routes::drafts::activate_route_draft(
        &pool,
        &provenance,
        draft.id,
        etag,
        actor,
        "policy-history-first",
    )
    .await
    .unwrap();
    let mut policy = olp::routes::policy::RoutingPolicy::default();
    policy.constraints.only = Some(vec![format!("provider:{}", provider.provider_id)]);
    sqlx::query("UPDATE route_drafts SET routing_policy=$2 WHERE id=$1")
        .bind(draft.id)
        .bind(sqlx::types::Json(&policy))
        .execute(&pool)
        .await
        .unwrap();
    let (etag, _) = olp::routes::drafts::validate_route_draft(
        &pool,
        &provenance,
        draft.id,
        first.draft_etag,
        actor,
    )
    .await
    .unwrap();
    let second = olp::routes::drafts::activate_route_draft(
        &pool,
        &provenance,
        draft.id,
        etag,
        actor,
        "policy-history-second",
    )
    .await
    .unwrap();
    let detail =
        olp::routes::revisions::get_route_revision(&pool, first.route_id, second.revision_id)
            .await
            .unwrap();
    assert_eq!(detail.routing_policy, policy);
    let diff = olp::routes::revisions::diff_route_revisions(
        &pool,
        first.route_id,
        first.revision_id,
        second.revision_id,
    )
    .await
    .unwrap();
    assert!(diff.routing_policy_changed);
    assert_eq!(diff.routing_policy_before, Default::default());
    assert_eq!(diff.routing_policy_after, policy);
    assert!(!diff.slug_changed && !diff.timeout_changed && !diff.max_attempts_changed);
    assert!(diff.operations_added.is_empty() && diff.operations_removed.is_empty());
    assert!(
        diff.targets_added.is_empty()
            && diff.targets_removed.is_empty()
            && diff.targets_changed.is_empty()
    );
    let restored = olp::routes::revisions::restore_route_revision_as_draft(
        &pool,
        &provenance,
        first.route_id,
        second.revision_id,
        actor,
        "policy-history-restore",
    )
    .await
    .unwrap();
    let stored: sqlx::types::Json<olp::routes::policy::RoutingPolicy> =
        sqlx::query_scalar("SELECT routing_policy FROM route_drafts WHERE id=$1")
            .bind(restored.id)
            .fetch_one(&pool)
            .await
            .unwrap();
    assert_eq!(stored.0, policy);
    let unchanged = olp::routes::revisions::diff_route_revisions(
        &pool,
        first.route_id,
        second.revision_id,
        second.revision_id,
    )
    .await
    .unwrap();
    assert!(!unchanged.routing_policy_changed);
}

#[tokio::test]
#[ignore = "requires PostgreSQL"]
async fn credentialless_default_slots_publish_restrictions_quotas_and_disabled_state() {
    let db = olp::test_support::TestDb::create_migrated("credentialless_pool").await;
    let pool = db.pool(2).await;
    let actor = owner(&pool).await;
    for (kind, auth) in [
        ("openai_compatible", "none"),
        ("vertex_ai", "adc"),
        ("bedrock", "default_chain"),
    ] {
        let id = provider(&pool, actor, auth).await;
        sqlx::query("UPDATE providers SET kind=$2,auth_mode=$3,active_credential_version_id=NULL,options='{\"limits\":{\"requests_per_minute\":17}}' WHERE id=$1")
            .bind(id).bind(kind).bind(auth).execute(&pool).await.unwrap();
        sqlx::query("UPDATE provider_credential_slots SET allowed_routes=ARRAY['allowed-route'],allowed_models=ARRAY['allowed-model'],requests_per_minute=3,tokens_per_minute=100,max_concurrency=2 WHERE provider_id=$1")
            .bind(id).execute(&pool).await.unwrap();
        for (number, enabled) in [(1, true), (2, false)] {
            sqlx::query("UPDATE provider_credential_slots SET enabled=$2 WHERE provider_id=$1")
                .bind(id)
                .bind(enabled)
                .execute(&pool)
                .await
                .unwrap();
            let revision = Uuid::now_v7();
            sqlx::query("INSERT INTO provider_revisions(id,provider_id,revision,name,kind,auth_mode,connector_ready,source_etag,activated_by,options) SELECT $1,id,$2,name,kind,auth_mode,true,etag,$3,options FROM providers WHERE id=$4")
                .bind(revision).bind(number).bind(actor).bind(id).execute(&pool).await.unwrap();
            let mut tx = pool.begin().await.unwrap();
            olp::providers::pool_store::snapshot(&mut tx, id, revision)
                .await
                .unwrap();
            sqlx::query("UPDATE providers SET active_revision_id=$2 WHERE id=$1")
                .bind(id)
                .bind(revision)
                .execute(&mut *tx)
                .await
                .unwrap();
            let routing = olp::routes::policy_store::load(&mut tx).await.unwrap();
            // A draft quota change cannot affect the published authority.
            sqlx::query("UPDATE providers SET options='{\"limits\":{\"requests_per_minute\":1}}' WHERE id=$1")
                .bind(id).execute(&mut *tx).await.unwrap();
            let limits = olp::routes::policy_store::connection_limits(&mut tx)
                .await
                .unwrap();
            assert_eq!(
                limits[&olp::ids::ProviderId::from_uuid(id)].requests_per_minute,
                Some(if number == 1 { 17 } else { 1 })
            );
            let slots = &routing.credentials[&olp::ids::ProviderId::from_uuid(id)];
            if enabled {
                assert_eq!(slots.len(), 1, "{auth}");
                assert_eq!(slots[0].credential_version_id, None);
                assert!(slots[0].permits("allowed-model", "allowed-route", None));
                assert!(!slots[0].permits("allowed-model", "other-route", None));
                assert!(!slots[0].permits("other-model", "allowed-route", None));
                assert_eq!(slots[0].requests_per_minute, Some(3));
                assert_eq!(slots[0].tokens_per_minute, Some(100));
                assert_eq!(slots[0].max_concurrency, Some(2));
            } else {
                assert!(
                    slots.is_empty(),
                    "disabled {auth} pool must not regain an implicit slot"
                );
            }
            tx.commit().await.unwrap();
        }
    }
}

#[tokio::test]
#[ignore = "requires PostgreSQL"]
async fn retained_media_jobs_allow_quota_activation_but_block_transport_changes() {
    use olp::media::jobs::{MediaJobState, MediaJobUpdate, NewMediaJobReservation};
    use olp::providers::options::{ConnectionLimits, ConnectionOptions};

    let db = olp::test_support::TestDb::create_migrated("media_quota_activation").await;
    let pool = db.pool(3).await;
    let actor = owner(&pool).await;
    let provider =
        crate::support::route_fixtures::insert_provider(&pool, actor, "media-quota").await;
    sqlx::query(
        "INSERT INTO model_capabilities
         (provider_model_id, operation, surface, mode, source, certified_at)
         SELECT prm.source_provider_model_id, prc.operation, prc.surface, prc.mode,
                prc.source, prc.certified_at
         FROM provider_revision_capabilities prc
         JOIN provider_revision_models prm ON prm.id = prc.provider_revision_model_id
         WHERE prm.source_provider_model_id = $1",
    )
    .bind(provider.model_id)
    .execute(&pool)
    .await
    .unwrap();
    let api_key = Uuid::now_v7();
    sqlx::query("INSERT INTO api_keys(id,lookup_id,secret_digest,name,created_by) VALUES($1,'mediaquotaactivation',$2,'media quota key',$3)")
        .bind(api_key).bind([39_u8; 32].as_slice()).bind(actor).execute(&pool).await.unwrap();
    let job_id = Uuid::now_v7();
    let mut options = ConnectionOptions::default();
    let mut historical_revision = None;
    for (index, limits) in [
        None,
        Some(ConnectionLimits {
            requests_per_minute: Some(100),
            tokens_per_minute: Some(10_000),
            max_concurrency: Some(5),
        }),
        Some(ConnectionLimits {
            requests_per_minute: Some(10),
            tokens_per_minute: Some(1_000),
            max_concurrency: Some(1),
        }),
        None,
    ]
    .into_iter()
    .enumerate()
    {
        options.limits = limits;
        let etag = Uuid::now_v7();
        sqlx::query("UPDATE providers SET state='draft',connector_ready=true,options=$2,etag=$3,last_probe_status='succeeded',last_probe_at=now() WHERE id=$1")
            .bind(provider.provider_id).bind(sqlx::types::Json(&options)).bind(etag).execute(&pool).await.unwrap();
        let activation = olp::providers::lifecycle::activate_provider(
            &pool,
            &Default::default(),
            provider.provider_id,
            etag,
            actor,
            &format!("media-quota-activation-{index}"),
        )
        .await
        .unwrap();
        let published: sqlx::types::Json<ConnectionOptions> = sqlx::query_scalar(
            "SELECT pr.options FROM provider_revisions pr
             JOIN providers p ON p.active_revision_id=pr.id WHERE p.id=$1",
        )
        .bind(provider.provider_id)
        .fetch_one(&pool)
        .await
        .unwrap();
        assert_eq!(published.0, options);
        if index == 0 {
            let job = olp::media::jobs::lifecycle::reserve_media_job(
                &pool,
                NewMediaJobReservation {
                    id: job_id,
                    runtime_generation_id: activation.release.generation_id,
                    api_key_id: api_key,
                    provider_id: provider.provider_id,
                    credential_version_id: None,
                    upstream_model: "media-quota-model".into(),
                    route_slug: "media-quota".into(),
                    operation: "video_create".parse().unwrap(),
                    surface: "openai".parse().unwrap(),
                },
            )
            .await
            .unwrap();
            historical_revision = Some(job.provider_revision_id);
            olp::media::jobs::lifecycle::attach_media_job_upstream(
                &pool,
                job_id,
                "upstream-media-quota-job",
                MediaJobUpdate {
                    state: MediaJobState::Succeeded,
                    progress_percent: Some(100.0),
                    content_available: true,
                    expires_at: None,
                    error_class: None,
                    last_polled_at: chrono::Utc::now(),
                },
            )
            .await
            .unwrap();
            // Revisions migrated from before connection options store an empty object.
            sqlx::query("UPDATE provider_revisions SET options='{}' WHERE id=$1")
                .bind(job.provider_revision_id)
                .execute(&pool)
                .await
                .unwrap();
        }
    }
    let job = olp::media::jobs::queries::media_job(&pool, job_id)
        .await
        .unwrap();
    assert_eq!(Some(job.provider_revision_id), historical_revision);
    assert_eq!(job.state, MediaJobState::Succeeded);

    options
        .parameter_defaults
        .insert("temperature".into(), serde_json::json!(1));
    let etag = Uuid::now_v7();
    sqlx::query("UPDATE providers SET state='draft',options=$2,etag=$3,last_probe_status='succeeded',last_probe_at=now() WHERE id=$1")
        .bind(provider.provider_id).bind(sqlx::types::Json(&options)).bind(etag).execute(&pool).await.unwrap();
    assert!(matches!(
        olp::providers::lifecycle::activate_provider(
            &pool,
            &Default::default(),
            provider.provider_id,
            etag,
            actor,
            "media-quota-transport-change",
        )
        .await,
        Err(olp::providers::error::Error::ProviderMediaJobIncompatible { job_id: blocked })
            if blocked == job_id
    ));
}

#[tokio::test]
#[ignore = "requires PostgreSQL"]
async fn routing_and_accounting_share_exact_scope_precedence_and_future_prices() {
    use olp::protocols::canonical::identity::{OperationKind, Surface, TransportMode};
    use olp::routes::policy::{AppliedRoutingPolicy, RoutingStrategy};
    use olp::usage::{
        emitter::{
            AttemptRoutingMetadata, Event, RequestAttemptMetadata, RequestAttemptUsageMetadata,
        },
        pricing::PriceInput,
    };
    let db = olp::test_support::TestDb::create_migrated("routing_prices").await;
    let pool = db.pool(3).await;
    let actor = owner(&pool).await;
    let provider = provider(&pool, actor, "priced").await;
    let master = MasterKey::new(1, [63; 32]);
    let version = secret(&pool, actor, provider, 1, &master).await;
    let provider_revision = revision(&pool, actor, provider, version, 1).await;
    sqlx::query(
        "UPDATE providers SET active_revision_id=$1,active_credential_version_id=$2 WHERE id=$3",
    )
    .bind(provider_revision)
    .bind(version)
    .bind(provider)
    .execute(&pool)
    .await
    .unwrap();
    let model = Uuid::now_v7();
    sqlx::query("INSERT INTO provider_models(id,provider_id,upstream_model,display_name,enabled) VALUES($1,$2,'priced-model','Priced',true)").bind(model).bind(provider).execute(&pool).await.unwrap();
    sqlx::query("INSERT INTO provider_revision_models(id,provider_revision_id,source_provider_model_id,upstream_model,display_name,enabled) VALUES($1,$2,$3,'priced-model','Priced',true)").bind(Uuid::now_v7()).bind(provider_revision).bind(model).execute(&pool).await.unwrap();
    let mut tx = pool.begin().await.unwrap();
    olp::providers::pool_store::snapshot(&mut tx, provider, provider_revision)
        .await
        .unwrap();
    tx.commit().await.unwrap();
    let rate = |connection, vendor: Option<&str>, amount: &str| PriceInput {
        provider_kind: olp::providers::runtime_model::ProviderKind::OpenAi,
        provider_id: connection,
        vendor_id: vendor.map(str::to_owned),
        model: "priced-model".into(),
        operation: OperationKind::Generation,
        input_per_million: Some(amount.into()),
        cached_input_per_million: None,
        output_per_million: Some(amount.into()),
        unit_price: None,
        currency: "USD".into(),
    };
    let at = chrono::Utc::now() - chrono::Duration::minutes(2);
    publish_prices(
        &pool,
        actor,
        &master,
        "routing-prices-1",
        at,
        &[
            rate(None, None, "4"),
            rate(None, Some("openai"), "3"),
            rate(Some(provider), None, "2"),
            rate(Some(provider), Some("openai"), "1"),
        ],
    )
    .await;
    let expected = publish_prices(
        &pool,
        actor,
        &master,
        "routing-prices-2",
        at + chrono::Duration::minutes(1),
        &[rate(Some(provider), Some("openai"), "0.9")],
    )
    .await;
    publish_prices(
        &pool,
        actor,
        &master,
        "routing-prices-3",
        chrono::Utc::now() + chrono::Duration::days(1),
        &[rate(Some(provider), Some("openai"), "0.1")],
    )
    .await;
    let payload: Vec<u8> = sqlx::query_scalar(
        "SELECT compiled_release FROM runtime_generations ORDER BY sequence DESC LIMIT 1",
    )
    .fetch_one(&pool)
    .await
    .unwrap();
    let snapshot = Snapshot::from_persisted_slice(&payload).unwrap();
    let selected = snapshot
        .routing
        .price(provider, "priced-model", "generation")
        .unwrap();
    assert_eq!(selected.revision_id, expected);
    assert_eq!(selected.scope_priority, 3);
    assert_eq!(
        selected
            .input_per_million
            .as_deref()
            .unwrap()
            .parse::<rust_decimal::Decimal>()
            .unwrap(),
        rust_decimal::Decimal::new(9, 1)
    );
    assert_eq!(
        snapshot.routing.prices.len(),
        5,
        "retain current scopes and scheduled rates, not superseded historical prices"
    );
    let key = Uuid::now_v7();
    sqlx::query("INSERT INTO api_keys(id,lookup_id,secret_digest,name,created_by) VALUES($1,'routing_price_key',$2,'Price test',$3)").bind(key).bind([1_u8;32].as_slice()).bind(actor).execute(&pool).await.unwrap();
    let now = chrono::Utc::now();
    let started = now - chrono::Duration::milliseconds(20);
    let attempt = Uuid::now_v7();
    let event = Event {
        event_id: Uuid::now_v7(),
        request_id: Uuid::now_v7(),
        runtime_generation_id: snapshot.generation.id.as_uuid(),
        api_key_id: key,
        provider_id: Some(provider),
        route_slug: "price-test".into(),
        upstream_model: Some("priced-model".into()),
        operation: OperationKind::Generation,
        surface: Surface::OpenAi,
        request_started_at: started,
        request_completed_at: now,
        observed_at: now,
        status_code: Some(200),
        error_class: None,
        committed: true,
        latency_ms: 20,
        first_byte_ms: Some(1),
        input_tokens: Some(1_000_000),
        output_tokens: Some(0),
        cached_input_tokens: None,
        media_units: None,
        usage_complete: true,
        unpriced: false,
        attempts: vec![RequestAttemptMetadata {
            id: attempt,
            ordinal: 1,
            provider_id: provider,
            upstream_model: "priced-model".into(),
            started_at: started,
            completed_at: now,
            status_code: Some(200),
            error_class: None,
            committed: true,
            latency_ms: 20,
            first_byte_ms: Some(1),
            routing: Some(AttemptRoutingMetadata {
                policy: Some(AppliedRoutingPolicy {
                    vendor_id: Some("openai".into()),
                    pricing_pinned: true,
                    strategy: RoutingStrategy::Price,
                    digest: "0".repeat(64),
                }),
                mode: Some(TransportMode::Unary),
                first_output_ms: None,
                streamed_output_tokens: None,
                credential_slot_id: Some(provider),
                credential_version_id: Some(version),
                provider_revision_id: provider_revision,
                pricing_revision_id: Some(expected),
            }),
            usage: Some(RequestAttemptUsageMetadata {
                observed: true,
                complete: true,
                billing_uncertain: false,
                input_tokens: Some(1_000_000),
                output_tokens: Some(0),
                cached_input_tokens: None,
                media_units: None,
            }),
        }],
    };
    olp::usage::ingestion::persistence::persist_request_metadata_event(&pool, &event)
        .await
        .unwrap();
    let (charged, cost): (Uuid, rust_decimal::Decimal) = sqlx::query_as(
        "SELECT pricing_revision_id,estimated_cost FROM attempt_usage_facts WHERE attempt_id=$1",
    )
    .bind(attempt)
    .fetch_one(&pool)
    .await
    .unwrap();
    assert_eq!(charged, expected);
    assert_eq!(cost, rust_decimal::Decimal::new(9, 1));
    for _ in 0..20 {
        let mut sample = event.clone();
        sample.event_id = Uuid::now_v7();
        sample.request_id = Uuid::now_v7();
        sample.output_tokens = Some(5);
        sample.attempts[0].id = Uuid::now_v7();
        sample.attempts[0].usage.as_mut().unwrap().output_tokens = Some(5);
        let routing = sample.attempts[0].routing.as_mut().unwrap();
        routing.mode = Some(TransportMode::Streaming);
        routing.first_output_ms = Some(4);
        routing.streamed_output_tokens = Some(5);
        olp::usage::ingestion::persistence::persist_request_metadata_event(&pool, &sample)
            .await
            .unwrap();
    }
    let (stop, shutdown) = tokio::sync::watch::channel(false);
    let worker = tokio::spawn(olp::inference::performance::supervise(
        pool.clone(),
        shutdown,
    ));
    tokio::time::timeout(std::time::Duration::from_secs(3), async {
        loop {
            let view = olp::inference::performance::snapshot();
            if let Some(measurement) = view.get(
                provider,
                "priced-model",
                OperationKind::Generation,
                TransportMode::Streaming,
            ) {
                assert_eq!(measurement.samples, 20);
                assert_eq!(measurement.latency_ms, 4);
                assert_eq!(measurement.output_tokens_per_second, Some(312));
                assert!(
                    view.get(
                        provider,
                        "priced-model",
                        OperationKind::Generation,
                        TransportMode::Unary
                    )
                    .is_none()
                );
                break;
            }
            tokio::time::sleep(std::time::Duration::from_millis(20)).await;
        }
    })
    .await
    .unwrap();
    stop.send(true).unwrap();
    worker.await.unwrap();
}

async fn publish_prices(
    pool: &PgPool,
    actor: Uuid,
    master: &MasterKey,
    label: &str,
    at: chrono::DateTime<chrono::Utc>,
    prices: &[olp::usage::pricing::PriceInput],
) -> Uuid {
    let result = olp::usage::pricing::create_pricing_revision(
        pool,
        &Default::default(),
        actor,
        label,
        at,
        prices,
        olp::database::idempotency::Replayable::new(
            olp::database::idempotency::fingerprint(&label).unwrap(),
            master,
        ),
        |_| olp::database::idempotency::Response::new(201, None, None, Vec::new()),
    )
    .await
    .unwrap();
    match result {
        olp::database::idempotency::Outcome::Executed { value, .. } => value.id,
        _ => panic!("new price must execute"),
    }
}
