use chrono::{Duration, Utc};
use olp_domain::{
    ApiKeyLimits, ApiKeyOwner, ApiKeyOwnerKind, ApiKeyScope, ProjectId, Role, ScopedRole,
    ServiceAccountId, Surface, TeamId, UserId,
};
use olp_storage::{
    MIGRATOR, PersistenceError, PgStore,
    access::{AccessError, ApiKeyCreated, NewApiKeyRecord},
    configuration::{ApiKeyListFilter, ConfigurationError, RotateApiKeyInput},
    idempotency::{
        IdempotencyOutcome, IdempotencyResponse, ReplayableIdempotency, idempotency_fingerprint,
    },
    identity::{
        IdentityError, InstallationSetupInput, NewProject, NewServiceAccount, NewTeam,
        ProjectRecord, ServiceAccountRecord, TeamRecord,
    },
    request_metadata::{
        RequestAttemptMetadata, RequestAttemptUsageMetadata, RequestMetadataEvent,
        RequestMetadataPersistenceOutcome,
    },
    security::{AuthHmacKey, MasterKey, SessionMaterial, hash_password},
};
use uuid::Uuid;

#[tokio::test]
#[ignore = "requires an empty PostgreSQL 18 database in OLP_TEST_DATABASE_URL"]
async fn migration_0034_backfills_keys_and_enforces_owner_scope_constraints() {
    let db = olp_storage::test_support::TestDb::create_empty("scoped_owner_upgrade").await;
    let store = db.store(3).await;
    MIGRATOR.run_to(33, store.pool()).await.unwrap();

    sqlx::query("INSERT INTO installation (installation_name) VALUES ('Scoped upgrade')")
        .execute(store.pool())
        .await
        .unwrap();
    let active_user = Uuid::now_v7();
    let disabled_user = Uuid::now_v7();
    insert_user(
        &store,
        active_user,
        "active-upgrade@olp.test",
        "developer",
        true,
    )
    .await;
    insert_user(
        &store,
        disabled_user,
        "disabled-upgrade@olp.test",
        "developer",
        false,
    )
    .await;
    let active_key = Uuid::now_v7();
    let disabled_key = Uuid::now_v7();
    insert_legacy_key(&store, active_key, "upgrade_active", active_user).await;
    insert_legacy_key(&store, disabled_key, "upgrade_disabled", disabled_user).await;

    MIGRATOR.run(store.pool()).await.unwrap();

    let active = sqlx::query!(
        "SELECT created_by, owner_user_id, owner_service_account_id, revoked_at \
         FROM api_keys WHERE id = $1",
        active_key
    )
    .fetch_one(store.pool())
    .await
    .unwrap();
    assert_eq!(active.created_by, active_user);
    assert_eq!(active.owner_user_id, Some(active_user));
    assert_eq!(active.owner_service_account_id, None);
    assert_eq!(active.revoked_at, None);

    let disabled = sqlx::query!(
        "SELECT owner_user_id, revoked_at FROM api_keys WHERE id = $1",
        disabled_key
    )
    .fetch_one(store.pool())
    .await
    .unwrap();
    assert_eq!(disabled.owner_user_id, Some(disabled_user));
    assert!(disabled.revoked_at.is_some());

    let compatibility_key = Uuid::now_v7();
    sqlx::query(
        "INSERT INTO api_keys \
         (id, lookup_id, secret_digest, name, created_by) \
         VALUES ($1, $2, $3, 'compatibility owner', $4)",
    )
    .bind(compatibility_key)
    .bind("compat_owner")
    .bind([3_u8; 32].as_slice())
    .bind(active_user)
    .execute(store.pool())
    .await
    .unwrap();
    let compatibility_owner = sqlx::query_scalar!(
        "SELECT owner_user_id FROM api_keys WHERE id = $1",
        compatibility_key
    )
    .fetch_one(store.pool())
    .await
    .unwrap();
    assert_eq!(compatibility_owner, Some(active_user));

    let ambiguous_owner = sqlx::query(
        "INSERT INTO api_keys \
         (id, lookup_id, secret_digest, name, created_by, owner_user_id, owner_service_account_id) \
         VALUES ($1, $2, $3, 'ambiguous owner', $4, $4, $5)",
    )
    .bind(Uuid::now_v7())
    .bind("ambiguous_owner")
    .bind([13_u8; 32].as_slice())
    .bind(active_user)
    .bind(Uuid::now_v7())
    .execute(store.pool())
    .await;
    assert!(ambiguous_owner.is_err());

    let disabled_owner = sqlx::query(
        "INSERT INTO api_keys \
         (id, lookup_id, secret_digest, name, created_by, owner_user_id) \
         VALUES ($1, $2, $3, 'disabled owner', $4, $4)",
    )
    .bind(Uuid::now_v7())
    .bind("disabled_owner")
    .bind([4_u8; 32].as_slice())
    .bind(disabled_user)
    .execute(store.pool())
    .await;
    assert!(disabled_owner.is_err());

    let project_without_team = sqlx::query(
        "INSERT INTO api_keys \
         (id, lookup_id, secret_digest, name, created_by, owner_user_id, project_id) \
         VALUES ($1, $2, $3, 'invalid scope', $4, $4, $5)",
    )
    .bind(Uuid::now_v7())
    .bind("invalid_scope")
    .bind([5_u8; 32].as_slice())
    .bind(active_user)
    .bind(Uuid::now_v7())
    .execute(store.pool())
    .await;
    assert!(project_without_team.is_err());

    let rewrite_audit_owner = sqlx::query("UPDATE api_keys SET created_by = $2 WHERE id = $1")
        .bind(active_key)
        .bind(disabled_user)
        .execute(store.pool())
        .await;
    assert!(rewrite_audit_owner.is_err());
}

#[tokio::test]
#[ignore = "requires an empty PostgreSQL 18 database in OLP_TEST_DATABASE_URL"]
async fn scoped_ownership_lifecycle_authorization_and_attribution_are_consistent() {
    let db = olp_storage::test_support::TestDb::create_migrated("scoped_owner_lifecycle").await;
    let store = db.store(5).await;
    let session = SessionMaterial::generate();
    let (owner, _) = store
        .setup_installation_with_session(
            InstallationSetupInput {
                installation_name: "Scoped ownership".to_owned(),
                email: "owner@scoped.test".to_owned(),
                display_name: "Owner".to_owned(),
                password_hash: hash_password("correct horse battery staple").unwrap(),
            },
            &session,
            Duration::hours(12),
        )
        .await
        .unwrap();
    let owner_id = owner.user_id;
    let developer_id = Uuid::now_v7();
    let ordinary_id = Uuid::now_v7();
    insert_user(
        &store,
        developer_id,
        "developer@scoped.test",
        "developer",
        true,
    )
    .await;
    insert_user(&store, ordinary_id, "member@scoped.test", "developer", true).await;
    let master_key = MasterKey::new(1, [17; 32]);
    let auth_hmac_key = AuthHmacKey::new([19; 32]);

    let team = create_team(&store, owner_id, &master_key, "Team A", "team-create-a").await;
    let replayed = store
        .create_team(
            NewTeam {
                name: "Team A".to_owned(),
                actor: owner_id,
                idempotency_key: "team-create-a".to_owned(),
            },
            replay(&master_key, "Team A"),
            empty_created_response,
        )
        .await
        .unwrap();
    assert!(matches!(replayed, IdempotencyOutcome::Replayed(_)));

    let developer_membership = store
        .put_team_membership(
            team.id,
            developer_id,
            ScopedRole::Admin,
            None,
            owner_id,
            "team-member-developer-01",
        )
        .await
        .unwrap();
    assert!(matches!(
        store
            .put_team_membership(
                team.id,
                developer_id,
                ScopedRole::Member,
                Some(Uuid::now_v7()),
                owner_id,
                "team-member-stale-01",
            )
            .await,
        Err(IdentityError::PreconditionFailed)
    ));
    assert_eq!(developer_membership.role, ScopedRole::Admin);
    store
        .put_team_membership(
            team.id,
            ordinary_id,
            ScopedRole::Member,
            None,
            owner_id,
            "team-member-ordinary-01",
        )
        .await
        .unwrap();

    let project = create_project(
        &store,
        developer_id,
        &master_key,
        team.id,
        "Project A",
        "project-create-a",
    )
    .await;
    let account = create_service_account(
        &store,
        developer_id,
        &master_key,
        team.id,
        project.id,
        "Worker A",
        "service-create-a",
    )
    .await;

    assert!(matches!(
        store
            .create_project(
                NewProject {
                    team_id: team.id,
                    name: "Member project".to_owned(),
                    actor: ordinary_id,
                    idempotency_key: "ordinary-project-create".to_owned(),
                },
                replay(&master_key, "ordinary project"),
                empty_created_response,
            )
            .await,
        Err(IdentityError::Forbidden)
    ));

    let user_key = create_key(
        &store,
        &master_key,
        &auth_hmac_key,
        developer_id,
        ApiKeyOwner::User(UserId::from_uuid(developer_id)),
        Some(team.id),
        Some(project.id),
        "developer key",
        "developer-key-create",
    )
    .await;
    let service_key = create_key(
        &store,
        &master_key,
        &auth_hmac_key,
        developer_id,
        ApiKeyOwner::ServiceAccount(ServiceAccountId::from_uuid(account.id)),
        Some(team.id),
        Some(project.id),
        "service key",
        "service-key-create",
    )
    .await;
    assert_eq!(
        store
            .get_api_key_for_actor(developer_id, service_key.id)
            .await
            .unwrap()
            .id,
        service_key.id
    );
    let rotation_fingerprint = idempotency_fingerprint(&"scoped service key rotation").unwrap();
    let rotation_material = auth_hmac_key.generate_api_key();
    let rotation_secret = rotation_material.expose_once().to_owned();
    let first_rotation = store
        .rotate_api_key(
            RotateApiKeyInput {
                id: service_key.id,
                material: &rotation_material,
                expected_etag: service_key.etag,
                actor: developer_id,
                idempotency_key: "service-key-rotation",
            },
            ReplayableIdempotency::new(rotation_fingerprint, &master_key),
            move |_| {
                IdempotencyResponse::new(
                    200,
                    Some("application/json".to_owned()),
                    None,
                    rotation_secret.into_bytes(),
                )
            },
        )
        .await
        .unwrap();
    assert!(matches!(
        first_rotation,
        IdempotencyOutcome::Executed { .. }
    ));

    let project_membership = store
        .list_project_memberships(owner_id, project.id)
        .await
        .unwrap()
        .into_iter()
        .find(|membership| membership.user_id == developer_id)
        .unwrap();
    store
        .put_team_membership(
            team.id,
            developer_id,
            ScopedRole::Member,
            Some(developer_membership.etag),
            owner_id,
            "team-member-demote-01",
        )
        .await
        .unwrap();
    let developer_project_membership = store
        .put_project_membership(
            project.id,
            developer_id,
            ScopedRole::Member,
            Some(project_membership.etag),
            owner_id,
            "project-member-demote-01",
        )
        .await
        .unwrap();
    assert!(matches!(
        store
            .create_service_account(
                NewServiceAccount {
                    team_id: team.id,
                    project_id: project.id,
                    name: "Worker A".to_owned(),
                    actor: developer_id,
                    idempotency_key: "service-create-a".to_owned(),
                },
                replay(&master_key, "Worker A"),
                |_| panic!("revoked project administration must not replay a stored response"),
            )
            .await,
        Err(IdentityError::Forbidden)
    ));
    let replay_material = auth_hmac_key.generate_api_key();
    assert!(matches!(
        store
            .create_api_key_record(
                &NewApiKeyRecord {
                    name: "service key".to_owned(),
                    material: replay_material,
                    scopes: vec![ApiKeyScope::Inference],
                    allowed_routes: Vec::new(),
                    limits: ApiKeyLimits::default(),
                    expires_at: None,
                    owner: ApiKeyOwner::ServiceAccount(ServiceAccountId::from_uuid(account.id)),
                    team_id: Some(TeamId::from_uuid(team.id)),
                    project_id: Some(ProjectId::from_uuid(project.id)),
                    actor: developer_id,
                    idempotency_key: "service-key-create".to_owned(),
                },
                replay(&master_key, "service key"),
                |_| panic!("revoked project administration must not replay a key secret"),
            )
            .await,
        Err(AccessError::Forbidden)
    ));
    let replay_rotation_material = auth_hmac_key.generate_api_key();
    assert!(matches!(
        store
            .rotate_api_key(
                RotateApiKeyInput {
                    id: service_key.id,
                    material: &replay_rotation_material,
                    expected_etag: service_key.etag,
                    actor: developer_id,
                    idempotency_key: "service-key-rotation",
                },
                ReplayableIdempotency::new(rotation_fingerprint, &master_key),
                |_| panic!("revoked project administration must not replay a rotated secret"),
            )
            .await,
        Err(ConfigurationError::NotFound)
    ));

    let service_keys = store
        .list_api_keys_for_actor(
            owner_id,
            ApiKeyListFilter {
                owner_kind: Some(ApiKeyOwnerKind::ServiceAccount),
                owner_id: Some(account.id),
                team_id: Some(team.id),
                project_id: Some(project.id),
            },
            None,
            20,
        )
        .await
        .unwrap();
    assert_eq!(service_keys.items.len(), 1);
    assert_eq!(service_keys.items[0].id, service_key.id);
    assert_eq!(service_keys.items[0].created_by, developer_id);

    let outside_team = create_team(
        &store,
        owner_id,
        &master_key,
        "Outside Team",
        "outside-team-create",
    )
    .await;
    let outside_project = create_project(
        &store,
        owner_id,
        &master_key,
        outside_team.id,
        "Outside Project",
        "outside-project-create",
    )
    .await;
    let outside_key = create_key(
        &store,
        &master_key,
        &auth_hmac_key,
        owner_id,
        ApiKeyOwner::User(UserId::from_uuid(owner_id)),
        Some(outside_team.id),
        Some(outside_project.id),
        "outside key",
        "outside-key-create",
    )
    .await;
    assert!(matches!(
        store.get_team(ordinary_id, outside_team.id).await,
        Err(IdentityError::NotFound)
    ));
    assert!(matches!(
        store
            .get_api_key_for_actor(ordinary_id, outside_key.id)
            .await,
        Err(ConfigurationError::NotFound)
    ));
    assert!(matches!(
        create_key_result(
            &store,
            &master_key,
            &auth_hmac_key,
            ordinary_id,
            ApiKeyOwner::User(UserId::from_uuid(owner_id)),
            Some(outside_team.id),
            Some(outside_project.id),
            "forbidden key",
            "outside-forbidden-key",
        )
        .await,
        Err(AccessError::Forbidden)
    ));
    assert!(matches!(
        store
            .revoke_api_key_record(
                outside_key.id,
                outside_key.etag,
                ordinary_id,
                "outside-forbidden-revoke",
            )
            .await,
        Err(AccessError::NotFound)
    ));
    assert!(!key_revoked(&store, outside_key.id).await);

    let removal_generation = store
        .remove_project_membership(
            project.id,
            developer_id,
            developer_project_membership.etag,
            owner_id,
            "project-member-remove-01",
        )
        .await
        .unwrap();
    assert!(removal_generation.is_some());
    assert!(key_revoked(&store, user_key.id).await);
    assert!(matches!(
        store.get_api_key_for_actor(developer_id, user_key.id).await,
        Err(ConfigurationError::NotFound)
    ));
    let installation_user_key = create_key(
        &store,
        &master_key,
        &auth_hmac_key,
        developer_id,
        ApiKeyOwner::User(UserId::from_uuid(developer_id)),
        None,
        None,
        "developer installation key",
        "developer-installation-key-create",
    )
    .await;

    let generations_before_disable = runtime_generation_count(&store).await;
    let developer = store.user(developer_id).await.unwrap().unwrap();
    store
        .update_user_access(
            developer_id,
            None::<Role>,
            Some(false),
            developer.etag,
            owner_id,
        )
        .await
        .unwrap();
    assert!(key_revoked(&store, installation_user_key.id).await);
    assert!(!key_revoked(&store, service_key.id).await);
    assert!(runtime_generation_count(&store).await > generations_before_disable);

    let provider_id = Uuid::now_v7();
    sqlx::query(
        "INSERT INTO providers \
         (id, name, kind, state, auth_mode, etag, created_by) \
         VALUES ($1, 'scoped-attribution-provider', 'open_ai', 'draft', 'api_key', $2, $3)",
    )
    .bind(provider_id)
    .bind(Uuid::now_v7())
    .bind(owner_id)
    .execute(store.pool())
    .await
    .unwrap();
    let request_id = Uuid::now_v7();
    let attempt_id = Uuid::now_v7();
    let observed_at = Utc::now() - Duration::seconds(1);
    let event = RequestMetadataEvent {
        event_id: Uuid::now_v7(),
        request_id,
        runtime_generation_id: service_key.release.generation_id,
        api_key_id: service_key.id,
        owner_user_id: None,
        service_account_id: Some(account.id),
        team_id: Some(team.id),
        project_id: Some(project.id),
        provider_id: Some(provider_id),
        route_slug: "scoped-attribution".to_owned(),
        upstream_model: Some("scoped-model".to_owned()),
        operation: olp_domain::OperationKind::Generation,
        surface: Surface::OpenAi,
        request_started_at: observed_at - Duration::milliseconds(10),
        request_completed_at: observed_at,
        observed_at,
        status_code: Some(200),
        error_class: None,
        committed: true,
        latency_ms: 10,
        first_byte_ms: Some(5),
        input_tokens: Some(7),
        output_tokens: Some(3),
        cached_input_tokens: None,
        media_units: None,
        usage_complete: true,
        unpriced: true,
        attempts: vec![RequestAttemptMetadata {
            id: attempt_id,
            ordinal: 1,
            provider_id,
            upstream_model: "scoped-model".to_owned(),
            started_at: observed_at - Duration::milliseconds(10),
            completed_at: observed_at,
            status_code: Some(200),
            error_class: None,
            committed: true,
            latency_ms: 10,
            first_byte_ms: Some(5),
            usage: Some(RequestAttemptUsageMetadata {
                observed: true,
                complete: true,
                billing_uncertain: false,
                input_tokens: Some(7),
                output_tokens: Some(3),
                cached_input_tokens: None,
                media_units: None,
            }),
        }],
    };
    assert_eq!(
        store.persist_request_metadata_event(&event).await.unwrap(),
        RequestMetadataPersistenceOutcome::Persisted
    );
    assert_attribution(
        &store, request_id, attempt_id, account.id, team.id, project.id,
    )
    .await;

    let account = store
        .update_service_account(
            account.id,
            None,
            Some(false),
            account.etag,
            owner_id,
            "service-disable-01",
        )
        .await
        .unwrap();
    assert!(account.runtime_generation.is_some());
    assert!(key_revoked(&store, service_key.id).await);
    assert_attribution(
        &store,
        request_id,
        attempt_id,
        account.resource.id,
        team.id,
        project.id,
    )
    .await;

    let project_update = store
        .update_project(
            outside_project.id,
            None,
            Some(false),
            outside_project.etag,
            owner_id,
            "outside-project-disable",
        )
        .await
        .unwrap();
    assert!(project_update.runtime_generation.is_some());
    assert!(key_revoked(&store, outside_key.id).await);

    let team_key = create_key(
        &store,
        &master_key,
        &auth_hmac_key,
        owner_id,
        ApiKeyOwner::User(UserId::from_uuid(owner_id)),
        Some(outside_team.id),
        None,
        "team disposition key",
        "team-disposition-key",
    )
    .await;
    let team_update = store
        .update_team(
            outside_team.id,
            None,
            Some(false),
            outside_team.etag,
            owner_id,
            "outside-team-disable",
        )
        .await
        .unwrap();
    assert!(team_update.runtime_generation.is_some());
    assert!(key_revoked(&store, team_key.id).await);
    assert!(
        !store
            .get_project(owner_id, outside_project.id)
            .await
            .unwrap()
            .active
    );

    let outbox_events: i64 = sqlx::query_scalar(
        "SELECT count(*) FROM transactional_outbox WHERE topic = 'runtime.generation.activated'",
    )
    .fetch_one(store.pool())
    .await
    .unwrap();
    assert!(outbox_events > 0);
}

async fn insert_user(store: &PgStore, id: Uuid, email: &str, role: &str, active: bool) {
    sqlx::query(
        "INSERT INTO users (id, email, display_name, role, active) \
         VALUES ($1, $2, 'Scoped user', $3::user_role, $4)",
    )
    .bind(id)
    .bind(email)
    .bind(role)
    .bind(active)
    .execute(store.pool())
    .await
    .unwrap();
}

async fn insert_legacy_key(store: &PgStore, id: Uuid, lookup: &str, creator: Uuid) {
    sqlx::query(
        "INSERT INTO api_keys (id, lookup_id, secret_digest, name, created_by) \
         VALUES ($1, $2, $3, 'legacy key', $4)",
    )
    .bind(id)
    .bind(lookup)
    .bind([7_u8; 32].as_slice())
    .bind(creator)
    .execute(store.pool())
    .await
    .unwrap();
}

fn replay<'a>(master_key: &'a MasterKey, seed: &str) -> ReplayableIdempotency<'a> {
    ReplayableIdempotency::new(idempotency_fingerprint(&seed).unwrap(), master_key)
}

fn empty_created_response<T>(_: &T) -> Result<IdempotencyResponse, PersistenceError> {
    IdempotencyResponse::new(
        201,
        Some("application/json".to_owned()),
        None,
        b"{}".to_vec(),
    )
}

fn executed<T>(outcome: IdempotencyOutcome<T>) -> T {
    match outcome {
        IdempotencyOutcome::Executed { value, .. } => value,
        IdempotencyOutcome::Replayed(_) => panic!("fresh operation unexpectedly replayed"),
    }
}

async fn create_team(
    store: &PgStore,
    actor: Uuid,
    master_key: &MasterKey,
    name: &str,
    idempotency_key: &str,
) -> TeamRecord {
    executed(
        store
            .create_team(
                NewTeam {
                    name: name.to_owned(),
                    actor,
                    idempotency_key: idempotency_key.to_owned(),
                },
                replay(master_key, name),
                empty_created_response,
            )
            .await
            .unwrap(),
    )
}

async fn create_project(
    store: &PgStore,
    actor: Uuid,
    master_key: &MasterKey,
    team_id: Uuid,
    name: &str,
    idempotency_key: &str,
) -> ProjectRecord {
    executed(
        store
            .create_project(
                NewProject {
                    team_id,
                    name: name.to_owned(),
                    actor,
                    idempotency_key: idempotency_key.to_owned(),
                },
                replay(master_key, name),
                empty_created_response,
            )
            .await
            .unwrap(),
    )
}

async fn create_service_account(
    store: &PgStore,
    actor: Uuid,
    master_key: &MasterKey,
    team_id: Uuid,
    project_id: Uuid,
    name: &str,
    idempotency_key: &str,
) -> ServiceAccountRecord {
    executed(
        store
            .create_service_account(
                NewServiceAccount {
                    team_id,
                    project_id,
                    name: name.to_owned(),
                    actor,
                    idempotency_key: idempotency_key.to_owned(),
                },
                replay(master_key, name),
                empty_created_response,
            )
            .await
            .unwrap(),
    )
}

#[allow(clippy::too_many_arguments)]
async fn create_key(
    store: &PgStore,
    master_key: &MasterKey,
    auth_hmac_key: &AuthHmacKey,
    actor: Uuid,
    owner: ApiKeyOwner,
    team_id: Option<Uuid>,
    project_id: Option<Uuid>,
    name: &str,
    idempotency_key: &str,
) -> ApiKeyCreated {
    create_key_result(
        store,
        master_key,
        auth_hmac_key,
        actor,
        owner,
        team_id,
        project_id,
        name,
        idempotency_key,
    )
    .await
    .unwrap()
}

#[allow(clippy::too_many_arguments)]
async fn create_key_result(
    store: &PgStore,
    master_key: &MasterKey,
    auth_hmac_key: &AuthHmacKey,
    actor: Uuid,
    owner: ApiKeyOwner,
    team_id: Option<Uuid>,
    project_id: Option<Uuid>,
    name: &str,
    idempotency_key: &str,
) -> Result<ApiKeyCreated, AccessError> {
    let material = auth_hmac_key.generate_api_key();
    let secret = material.expose_once().to_owned();
    let record = NewApiKeyRecord {
        name: name.to_owned(),
        material,
        scopes: vec![ApiKeyScope::Inference],
        allowed_routes: Vec::new(),
        limits: ApiKeyLimits::default(),
        expires_at: None,
        owner,
        team_id: team_id.map(TeamId::from_uuid),
        project_id: project_id.map(ProjectId::from_uuid),
        actor,
        idempotency_key: idempotency_key.to_owned(),
    };
    store
        .create_api_key_record(&record, replay(master_key, name), move |_| {
            IdempotencyResponse::new(
                201,
                Some("application/json".to_owned()),
                None,
                secret.into_bytes(),
            )
        })
        .await
        .map(executed)
}

async fn key_revoked(store: &PgStore, id: Uuid) -> bool {
    sqlx::query_scalar::<_, bool>("SELECT revoked_at IS NOT NULL FROM api_keys WHERE id = $1")
        .bind(id)
        .fetch_one(store.pool())
        .await
        .unwrap()
}

async fn runtime_generation_count(store: &PgStore) -> i64 {
    sqlx::query_scalar("SELECT count(*) FROM runtime_generations")
        .fetch_one(store.pool())
        .await
        .unwrap()
}

async fn assert_attribution(
    store: &PgStore,
    request_id: Uuid,
    attempt_id: Uuid,
    service_account_id: Uuid,
    team_id: Uuid,
    project_id: Uuid,
) {
    let request = sqlx::query!(
        "SELECT owner_user_id, service_account_id, team_id, project_id \
         FROM requests WHERE id = $1",
        request_id
    )
    .fetch_one(store.pool())
    .await
    .unwrap();
    assert_eq!(request.owner_user_id, None);
    assert_eq!(request.service_account_id, Some(service_account_id));
    assert_eq!(request.team_id, Some(team_id));
    assert_eq!(request.project_id, Some(project_id));

    let attempt = sqlx::query!(
        "SELECT service_account_id, team_id, project_id FROM attempts WHERE id = $1",
        attempt_id
    )
    .fetch_one(store.pool())
    .await
    .unwrap();
    assert_eq!(attempt.service_account_id, Some(service_account_id));
    assert_eq!(attempt.team_id, Some(team_id));
    assert_eq!(attempt.project_id, Some(project_id));

    let usage = sqlx::query!(
        "SELECT service_account_id, team_id, project_id \
         FROM attempt_usage_facts WHERE attempt_id = $1",
        attempt_id
    )
    .fetch_one(store.pool())
    .await
    .unwrap();
    assert_eq!(usage.service_account_id, Some(service_account_id));
    assert_eq!(usage.team_id, Some(team_id));
    assert_eq!(usage.project_id, Some(project_id));
}
