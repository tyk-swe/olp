use std::time::Duration;

use olp_db::{
    configuration::{
        error::Error, provider_lifecycle::NewProviderDraft, resources::RotateCredentialInput,
    },
    idempotency::{Replayable, Response, fingerprint},
    identity::InstallationSetupInput,
    media_jobs::{MediaJobError, NewMediaJobReservation},
    security::{aad::credential, envelope::MasterKey},
    store::Store,
    test_support::TestDb,
};
use sqlx::PgPool;
use uuid::Uuid;

#[derive(Clone)]
struct Fixture {
    store: Store,
    actor: Uuid,
    provider_id: Uuid,
    model_id: Uuid,
    api_key_id: Uuid,
    generation_id: Uuid,
    revision_id: Uuid,
    etag: Uuid,
}

#[derive(Clone, Copy, Debug)]
enum Mutation {
    Endpoint,
    Credential,
    Model,
    Capability,
    Compatible,
    Disable,
}

impl Fixture {
    async fn create() -> (TestDb, Self) {
        let db = TestDb::create_migrated("media_admission").await;
        let store = db.store(6).await;
        let actor = store
            .setup_installation(InstallationSetupInput {
                installation_name: "Media admission".to_owned(),
                email: "owner@media-admission.test".to_owned(),
                display_name: "Owner".to_owned(),
                password_hash: "test-password-hash".to_owned(),
            })
            .await
            .unwrap()
            .user_id;
        let provider_id = Uuid::now_v7();
        let model_id = Uuid::now_v7();
        create_provider(&store, actor, provider_id, model_id).await;
        let provider = store.get_provider(provider_id).await.unwrap();
        store
            .record_provider_probe(provider_id, provider.etag, true, "ready", actor)
            .await
            .unwrap();
        let activation = store
            .activate_provider(provider_id, provider.etag, actor, "initial-activation")
            .await
            .unwrap();
        let provider = store.get_provider(provider_id).await.unwrap();
        let revision_id =
            sqlx::query_scalar("SELECT active_revision_id FROM providers WHERE id = $1")
                .bind(provider_id)
                .fetch_one(store.pool())
                .await
                .unwrap();
        let api_key_id = Uuid::now_v7();
        sqlx::query(
            "INSERT INTO api_keys (id, lookup_id, secret_digest, name, created_by)
             VALUES ($1, 'olpv2mediaadmission', $2, 'media admission', $3)",
        )
        .bind(api_key_id)
        .bind([29_u8; 32].as_slice())
        .bind(actor)
        .execute(store.pool())
        .await
        .unwrap();
        (
            db,
            Self {
                store,
                actor,
                provider_id,
                model_id,
                api_key_id,
                generation_id: activation.release.generation_id,
                revision_id,
                etag: provider.etag,
            },
        )
    }

    fn reservation(&self) -> NewMediaJobReservation {
        NewMediaJobReservation {
            id: Uuid::now_v7(),
            runtime_generation_id: self.generation_id,
            api_key_id: self.api_key_id,
            provider_id: self.provider_id,
            upstream_model: "video-model".to_owned(),
            route_slug: "video-default".to_owned(),
            operation: "video_create".parse().unwrap(),
            surface: "openai".parse().unwrap(),
        }
    }

    async fn stage(&mut self, mutation: Mutation) {
        if matches!(mutation, Mutation::Disable) {
            return;
        }
        sqlx::query("UPDATE providers SET state = 'draft' WHERE id = $1")
            .bind(self.provider_id)
            .execute(self.store.pool())
            .await
            .unwrap();
        match mutation {
            Mutation::Endpoint => {
                sqlx::query(
                    "UPDATE providers SET endpoint = 'https://new.example.test/v1/' WHERE id = $1",
                )
                .bind(self.provider_id)
                .execute(self.store.pool())
                .await
                .unwrap();
            }
            Mutation::Credential => {
                let master_key = MasterKey::new(1, [17; 32]);
                let credential_id = Uuid::now_v7();
                let secret = master_key
                    .seal(
                        b"replacement-test-secret",
                        &credential(self.provider_id, credential_id, 2),
                    )
                    .unwrap();
                self.store
                    .rotate_provider_credential(
                        self.provider_id,
                        RotateCredentialInput {
                            expected_etag: self.etag,
                            credential_id,
                            version: 2,
                            encrypted: secret,
                            actor: self.actor,
                            idempotency_key: "rotate-media-credential".to_owned(),
                        },
                        Replayable::new(
                            fingerprint(&"rotate-media-credential").unwrap(),
                            &master_key,
                        ),
                        |_| Response::new(200, None, None, Vec::new()),
                    )
                    .await
                    .unwrap();
                self.etag = self
                    .store
                    .get_provider(self.provider_id)
                    .await
                    .unwrap()
                    .etag;
            }
            Mutation::Model => {
                sqlx::query(
                    "UPDATE provider_models SET upstream_model = 'different-model' WHERE id = $1",
                )
                .bind(self.model_id)
                .execute(self.store.pool())
                .await
                .unwrap();
            }
            Mutation::Capability => {
                sqlx::query("DELETE FROM model_capabilities WHERE provider_model_id = $1 AND operation = 'video_delete'")
                    .bind(self.model_id).execute(self.store.pool()).await.unwrap();
            }
            Mutation::Compatible => {
                sqlx::query("UPDATE providers SET name = 'renamed-provider' WHERE id = $1")
                    .bind(self.provider_id)
                    .execute(self.store.pool())
                    .await
                    .unwrap();
            }
            Mutation::Disable => unreachable!(),
        }
        sqlx::query("UPDATE model_capabilities SET source = 'certified', certified_at = now() WHERE provider_model_id = $1")
            .bind(self.model_id).execute(self.store.pool()).await.unwrap();
        self.store
            .record_provider_probe(self.provider_id, self.etag, true, "ready", self.actor)
            .await
            .unwrap();
    }

    async fn mutate(&self, mutation: Mutation) -> Result<(), Error> {
        if matches!(mutation, Mutation::Disable) {
            self.store
                .disable_provider(
                    self.provider_id,
                    self.etag,
                    self.actor,
                    "disable-media-provider",
                )
                .await
                .map(|_| ())
        } else {
            self.store
                .activate_provider(
                    self.provider_id,
                    self.etag,
                    self.actor,
                    "reactivate-media-provider",
                )
                .await
                .map(|_| ())
        }
    }
}

async fn create_provider(store: &Store, actor: Uuid, provider_id: Uuid, model_id: Uuid) {
    let master_key = MasterKey::new(1, [17; 32]);
    let credential_id = Uuid::now_v7();
    let secret = master_key
        .seal(
            b"original-test-secret",
            &credential(provider_id, credential_id, 1),
        )
        .unwrap();
    store
        .create_provider_draft(
            NewProviderDraft {
                provider_id,
                credential_id: Some(credential_id),
                model_id: Some(model_id),
                name: "media-provider".to_owned(),
                kind: "openai".parse().unwrap(),
                endpoint: Some("https://old.example.test/v1/".to_owned()),
                cloud_region: None,
                cloud_project: None,
                deployment: None,
                api_version: None,
                auth_mode: "api_key".parse().unwrap(),
                connector_ready: true,
                credential: Some(secret),
                model: Some("video-model".to_owned()),
                display_name: Some("Video model".to_owned()),
                model_enabled: true,
                surface: Some("openai".parse().unwrap()),
                actor,
                idempotency_key: "create-media-provider".to_owned(),
            },
            Replayable::new(fingerprint(&"create-media-provider").unwrap(), &master_key),
            |_| Response::new(201, None, None, Vec::new()),
        )
        .await
        .unwrap();
    sqlx::query(
        "INSERT INTO model_capabilities
         (provider_model_id, operation, surface, mode, source, certified_at)
         SELECT $1, operation, 'openai', CASE WHEN operation = 'video_create' THEN 'async' ELSE 'unary' END,
                'certified', now()
         FROM unnest(ARRAY['video_create', 'video_get', 'video_content', 'video_delete']) AS operation",
    ).bind(model_id).execute(store.pool()).await.unwrap();
    sqlx::query("UPDATE model_capabilities SET source = 'certified', certified_at = now() WHERE provider_model_id = $1")
        .bind(model_id).execute(store.pool()).await.unwrap();
}

async fn blocked_backend(pool: &PgPool, blocker: i32) -> i32 {
    tokio::time::timeout(Duration::from_secs(10), async {
        loop {
            let blocked: Option<i32> = sqlx::query_scalar(
                "SELECT pid FROM pg_stat_activity
                 WHERE datname = current_database() AND $1 = ANY(pg_blocking_pids(pid))
                 ORDER BY pid LIMIT 1",
            )
            .bind(blocker)
            .fetch_optional(pool)
            .await
            .unwrap();
            if let Some(pid) = blocked {
                return pid;
            }
            tokio::task::yield_now().await;
        }
    })
    .await
    .expect("expected transaction to reach its PostgreSQL lock barrier")
}

#[tokio::test]
#[ignore = "requires OLP_TEST_DATABASE_ADMIN_URL and OLP_TEST_DATABASE_URL_PREFIX"]
async fn reservation_first_blocks_incompatible_activation_and_disable() {
    for mutation in [
        Mutation::Endpoint,
        Mutation::Credential,
        Mutation::Model,
        Mutation::Capability,
        Mutation::Disable,
    ] {
        let (db, mut fixture) = Fixture::create().await;
        fixture.stage(mutation).await;
        let mut barrier = fixture.store.pool().begin().await.unwrap();
        sqlx::query("LOCK TABLE async_media_jobs IN SHARE MODE")
            .execute(&mut *barrier)
            .await
            .unwrap();
        let barrier_pid: i32 = sqlx::query_scalar("SELECT pg_backend_pid()")
            .fetch_one(&mut *barrier)
            .await
            .unwrap();
        let reserving = fixture.clone();
        let reservation = tokio::spawn(async move {
            reserving
                .store
                .reserve_media_job(reserving.reservation())
                .await
        });
        let reservation_pid = blocked_backend(fixture.store.pool(), barrier_pid).await;
        let mutating = fixture.clone();
        let mutation_task = tokio::spawn(async move { mutating.mutate(mutation).await });
        blocked_backend(fixture.store.pool(), reservation_pid).await;
        barrier.commit().await.unwrap();
        let job = reservation.await.unwrap().unwrap();
        assert_eq!(job.provider_revision_id, Some(fixture.revision_id));
        let result = mutation_task.await.unwrap();
        if matches!(mutation, Mutation::Disable) {
            assert!(
                matches!(result, Err(Error::InUse)),
                "{mutation:?}: {result:?}"
            );
        } else {
            assert!(
                matches!(result, Err(Error::ProviderIncomplete)),
                "{mutation:?}: {result:?}"
            );
        }
        fixture.store.pool().close().await;
        drop(db);
    }
}

#[tokio::test]
#[ignore = "requires OLP_TEST_DATABASE_ADMIN_URL and OLP_TEST_DATABASE_URL_PREFIX"]
async fn mutation_first_rejects_incompatible_pins_and_preserves_compatible_older_authority() {
    for mutation in [
        Mutation::Endpoint,
        Mutation::Credential,
        Mutation::Model,
        Mutation::Capability,
        Mutation::Disable,
        Mutation::Compatible,
    ] {
        let (db, mut fixture) = Fixture::create().await;
        fixture.stage(mutation).await;
        let mut barrier = fixture.store.pool().begin().await.unwrap();
        sqlx::query("LOCK TABLE runtime_generations IN SHARE MODE")
            .execute(&mut *barrier)
            .await
            .unwrap();
        let barrier_pid: i32 = sqlx::query_scalar("SELECT pg_backend_pid()")
            .fetch_one(&mut *barrier)
            .await
            .unwrap();
        let mutating = fixture.clone();
        let mutation_task = tokio::spawn(async move { mutating.mutate(mutation).await });
        let mutation_pid = blocked_backend(fixture.store.pool(), barrier_pid).await;
        let reserving = fixture.clone();
        let input = fixture.reservation();
        let job_id = input.id;
        let reservation =
            tokio::spawn(async move { reserving.store.reserve_media_job(input).await });
        blocked_backend(fixture.store.pool(), mutation_pid).await;
        barrier.commit().await.unwrap();
        mutation_task.await.unwrap().unwrap();
        let result = reservation.await.unwrap();
        if matches!(mutation, Mutation::Compatible) {
            let job = result.unwrap();
            assert_eq!(job.provider_revision_id, Some(fixture.revision_id));
            assert_eq!(job.runtime_generation_id, Some(fixture.generation_id));
            let current: Uuid =
                sqlx::query_scalar("SELECT active_revision_id FROM providers WHERE id = $1")
                    .bind(fixture.provider_id)
                    .fetch_one(fixture.store.pool())
                    .await
                    .unwrap();
            assert_ne!(current, fixture.revision_id);
        } else {
            assert!(
                matches!(result, Err(MediaJobError::Invalid(_))),
                "{mutation:?}: {result:?}"
            );
            assert!(matches!(
                fixture.store.media_job(job_id).await,
                Err(MediaJobError::NotFound)
            ));
        }
        fixture.store.pool().close().await;
        drop(db);
    }
}
