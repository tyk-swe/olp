use chrono::{Duration, Utc};
use olp_domain::Role;
use olp_storage::{
    AcceptInvitation, IdempotencyOutcome, IdempotencyResponse, IdentityError,
    InstallationSetupInput, MasterKey, NewInvitation, ReplayableIdempotency, SessionMaterial,
    hash_password, idempotency_fingerprint,
};
use uuid::Uuid;

const IDENTITY_EMAIL_LOCK_SEED: i64 = 0x4f4c_505f_4944;

#[tokio::test]
#[ignore = "requires an empty PostgreSQL 18 database in OLP_TEST_DATABASE_URL"]
async fn local_identity_lifecycle_is_transactional_and_audited() {
    let db = olp_storage::test_support::TestDb::create_migrated("identity").await;
    let store = db.store(5).await;

    let owner_session = SessionMaterial::generate();
    let (owner, owner_session_id) = store
        .setup_installation_with_session(
            InstallationSetupInput {
                installation_name: "Identity integration".to_owned(),
                email: "owner@example.test".to_owned(),
                display_name: "Owner".to_owned(),
                password_hash: hash_password("correct horse battery staple").unwrap(),
            },
            &owner_session,
            Duration::hours(12),
        )
        .await
        .unwrap();

    let stale_last_seen = Utc::now() - Duration::minutes(10);
    sqlx::query("UPDATE sessions SET last_seen_at = $1 WHERE id = $2")
        .bind(stale_last_seen)
        .bind(owner_session_id)
        .execute(store.pool())
        .await
        .unwrap();
    assert!(
        store
            .session_principal(owner_session.token())
            .await
            .unwrap()
            .is_some()
    );
    let touched_last_seen: chrono::DateTime<Utc> =
        sqlx::query_scalar("SELECT last_seen_at FROM sessions WHERE id = $1")
            .bind(owner_session_id)
            .fetch_one(store.pool())
            .await
            .unwrap();
    assert!(touched_last_seen > stale_last_seen);

    let recent_last_seen: chrono::DateTime<Utc> = sqlx::query_scalar(
        "UPDATE sessions SET last_seen_at = $1 WHERE id = $2 RETURNING last_seen_at",
    )
    .bind(Utc::now() - Duration::minutes(1))
    .bind(owner_session_id)
    .fetch_one(store.pool())
    .await
    .unwrap();
    let recent_row_version: String =
        sqlx::query_scalar("SELECT xmin::text FROM sessions WHERE id = $1")
            .bind(owner_session_id)
            .fetch_one(store.pool())
            .await
            .unwrap();
    assert!(
        store
            .session_principal(owner_session.token())
            .await
            .unwrap()
            .is_some()
    );
    let unchanged_activity: (chrono::DateTime<Utc>, String) =
        sqlx::query_as("SELECT last_seen_at, xmin::text FROM sessions WHERE id = $1")
            .bind(owner_session_id)
            .fetch_one(store.pool())
            .await
            .unwrap();
    assert_eq!(unchanged_activity.0, recent_last_seen);
    assert_eq!(unchanged_activity.1, recent_row_version);

    let owner_record = store.user(owner.user_id).await.unwrap().unwrap();
    let master_key = MasterKey::new(1, [7; 32]);
    assert_eq!(owner_record.role, Role::Owner);
    assert!(matches!(
        store
            .update_user_role(
                owner.user_id,
                Role::Viewer,
                owner_record.etag,
                owner.user_id,
            )
            .await,
        Err(IdentityError::LastOwner)
    ));

    let operator_fingerprint = idempotency_fingerprint(&"invite-operator-001").unwrap();
    let operator_invitation = store
        .create_invitation(
            NewInvitation {
                email: "operator@example.test".to_owned(),
                role: Role::Operator,
                expires_at: Utc::now() + Duration::days(7),
                actor: owner.user_id,
                idempotency_key: "invite-operator-001".to_owned(),
            },
            ReplayableIdempotency::new(operator_fingerprint, &master_key),
            |_| IdempotencyResponse::new(201, None, None, Vec::new()),
        )
        .await
        .unwrap();
    let IdempotencyOutcome::Executed {
        value: operator_invitation,
        ..
    } = operator_invitation
    else {
        panic!("new invitation must execute");
    };
    let invited_session = SessionMaterial::generate();
    let accepted = store
        .accept_invitation(
            AcceptInvitation {
                token: operator_invitation.material.token().to_owned(),
                display_name: "Operator".to_owned(),
                password_hash: hash_password("another correct local password").unwrap(),
            },
            &invited_session,
            Duration::hours(12),
        )
        .await
        .unwrap();
    assert_eq!(accepted.user.role, Role::Operator);
    assert!(
        store
            .accept_invitation(
                AcceptInvitation {
                    token: operator_invitation.material.token().to_owned(),
                    display_name: "Replay".to_owned(),
                    password_hash: hash_password("another correct local password").unwrap(),
                },
                &SessionMaterial::generate(),
                Duration::hours(12),
            )
            .await
            .is_err()
    );

    let unchanged = store
        .update_user_access(
            accepted.user.id,
            Some(Role::Operator),
            Some(true),
            accepted.user.etag,
            owner.user_id,
        )
        .await
        .unwrap();
    assert_eq!(unchanged.etag, accepted.user.etag);
    assert!(
        !store
            .list_sessions(accepted.user.id, None, 50)
            .await
            .unwrap()
            .0
            .is_empty()
    );

    let updated = store
        .update_user_role(
            accepted.user.id,
            Role::Developer,
            accepted.user.etag,
            owner.user_id,
        )
        .await
        .unwrap();
    assert_eq!(updated.role, Role::Developer);
    assert!(
        store
            .list_sessions(accepted.user.id, None, 50)
            .await
            .unwrap()
            .0
            .is_empty()
    );

    let viewer_fingerprint = idempotency_fingerprint(&"invite-viewer-0001").unwrap();
    let viewer_invitation = store
        .create_invitation(
            NewInvitation {
                email: "viewer@example.test".to_owned(),
                role: Role::Viewer,
                expires_at: Utc::now() + Duration::days(1),
                actor: owner.user_id,
                idempotency_key: "invite-viewer-0001".to_owned(),
            },
            ReplayableIdempotency::new(viewer_fingerprint, &master_key),
            |_| IdempotencyResponse::new(201, None, None, Vec::new()),
        )
        .await
        .unwrap();
    let IdempotencyOutcome::Executed {
        value: viewer_invitation,
        ..
    } = viewer_invitation
    else {
        panic!("new invitation must execute");
    };
    let revoked = store
        .revoke_invitation(
            viewer_invitation.invitation.id,
            owner.user_id,
            "revoke-viewer-0001",
        )
        .await
        .unwrap();
    assert!(revoked.revoked_at.is_some());
    assert!(
        store
            .accept_invitation(
                AcceptInvitation {
                    token: viewer_invitation.material.token().to_owned(),
                    display_name: "Viewer".to_owned(),
                    password_hash: hash_password("a third correct local password").unwrap(),
                },
                &SessionMaterial::generate(),
                Duration::hours(12),
            )
            .await
            .is_err()
    );

    let expired_session = SessionMaterial::generate();
    let expired_session_id = store
        .create_session(owner.user_id, &expired_session, Duration::hours(1))
        .await
        .unwrap();
    let expired_last_seen: chrono::DateTime<Utc> = sqlx::query_scalar(
        "UPDATE sessions SET expires_at = now() - interval '1 second', last_seen_at = $1 \
         WHERE id = $2 RETURNING last_seen_at",
    )
    .bind(Utc::now() - Duration::minutes(10))
    .bind(expired_session_id)
    .fetch_one(store.pool())
    .await
    .unwrap();
    let expired_row_version: String =
        sqlx::query_scalar("SELECT xmin::text FROM sessions WHERE id = $1")
            .bind(expired_session_id)
            .fetch_one(store.pool())
            .await
            .unwrap();
    assert!(
        store
            .session_principal(expired_session.token())
            .await
            .unwrap()
            .is_none()
    );
    let unchanged_expired: (chrono::DateTime<Utc>, String) =
        sqlx::query_as("SELECT last_seen_at, xmin::text FROM sessions WHERE id = $1")
            .bind(expired_session_id)
            .fetch_one(store.pool())
            .await
            .unwrap();
    assert_eq!(unchanged_expired.0, expired_last_seen);
    assert_eq!(unchanged_expired.1, expired_row_version);
    store
        .record_local_login_failure(Some(owner.user_id))
        .await
        .unwrap();
    store.record_local_login_failure(None).await.unwrap();

    for _ in 0..5 {
        assert!(
            store
                .admit_local_login_attempt([11; 32], [12; 32])
                .await
                .unwrap()
        );
        assert!(
            store
                .admit_invitation_acceptance_attempt([22; 32], [23; 32])
                .await
                .unwrap()
        );
    }
    assert!(
        !store
            .admit_local_login_attempt([11; 32], [12; 32])
            .await
            .unwrap()
    );
    assert!(
        !store
            .admit_invitation_acceptance_attempt([22; 32], [23; 32])
            .await
            .unwrap()
    );
    // A source-plus-target lockout cannot be used to exhaust another source.
    assert!(
        store
            .admit_local_login_attempt([13; 32], [14; 32])
            .await
            .unwrap()
    );
    let local_login_global_attempts: i32 = sqlx::query_scalar(
        "SELECT attempts FROM public_auth_rate_limits \
         WHERE action = 'local_login' AND scope = 'global'",
    )
    .fetch_one(store.pool())
    .await
    .unwrap();
    assert_eq!(local_login_global_attempts, 6);
    let opaque_rate_rows: i64 = sqlx::query_scalar(
        "SELECT count(*) FROM public_auth_rate_limits \
         WHERE octet_length(key_digest) = 32",
    )
    .fetch_one(store.pool())
    .await
    .unwrap();
    assert_eq!(opaque_rate_rows, 8);

    store
        .revoke_session(owner_session_id, owner.user_id, false)
        .await
        .unwrap();
    assert!(
        store
            .session_principal(owner_session.token())
            .await
            .unwrap()
            .is_none()
    );
    let audit_actions: Vec<String> = sqlx::query_scalar(
        "SELECT action FROM audit_events WHERE action IN \
         ('invitation.create', 'invitation.accept', 'invitation.revoke', \
          'user.create', 'user.role_update', 'session.create', 'session.revoke')",
    )
    .fetch_all(store.pool())
    .await
    .unwrap();
    for expected in [
        "invitation.create",
        "invitation.accept",
        "invitation.revoke",
        "user.create",
        "user.role_update",
        "session.create",
        "session.revoke",
    ] {
        assert!(audit_actions.iter().any(|action| action == expected));
    }
    let local_login_audits: Vec<(Option<Uuid>, String, Option<String>)> = sqlx::query_as(
        "SELECT actor_user_id, outcome, resource_id FROM audit_events \
         WHERE action = 'local_auth.login' ORDER BY occurred_at",
    )
    .fetch_all(store.pool())
    .await
    .unwrap();
    assert!(local_login_audits.iter().any(|(actor, outcome, resource)| {
        *actor == Some(owner.user_id) && outcome == "success" && resource.is_some()
    }));
    assert!(local_login_audits.iter().any(|(actor, outcome, resource)| {
        *actor == Some(owner.user_id) && outcome == "failure" && resource.is_none()
    }));
    assert!(local_login_audits.iter().any(|(actor, outcome, resource)| {
        actor.is_none() && outcome == "failure" && resource.is_none()
    }));
}

#[tokio::test]
#[ignore = "requires an empty PostgreSQL 18 database in OLP_TEST_DATABASE_URL"]
async fn invitation_expiring_behind_identity_lock_cannot_be_accepted() {
    let db = olp_storage::test_support::TestDb::create_migrated("identity_expiry_lock").await;
    let store = db.store(5).await;
    let owner = store
        .setup_installation(InstallationSetupInput {
            installation_name: "Invitation expiry lock".to_owned(),
            email: "owner@example.test".to_owned(),
            display_name: "Owner".to_owned(),
            password_hash: hash_password("correct horse battery staple").unwrap(),
        })
        .await
        .unwrap();
    let master_key = MasterKey::new(1, [9; 32]);
    let fingerprint = idempotency_fingerprint(&"invite-expiry-lock-001").unwrap();
    let invitation = store
        .create_invitation(
            NewInvitation {
                email: "waiting@example.test".to_owned(),
                role: Role::Viewer,
                expires_at: Utc::now() + Duration::days(1),
                actor: owner.user_id,
                idempotency_key: "invite-expiry-lock-001".to_owned(),
            },
            ReplayableIdempotency::new(fingerprint, &master_key),
            |_| IdempotencyResponse::new(201, None, None, Vec::new()),
        )
        .await
        .unwrap();
    let IdempotencyOutcome::Executed {
        value: invitation, ..
    } = invitation
    else {
        panic!("new invitation must execute");
    };

    let mut blocker = store.pool().begin().await.unwrap();
    sqlx::query("SELECT pg_advisory_xact_lock(hashtextextended($1, $2))")
        .bind("waiting@example.test")
        .bind(IDENTITY_EMAIL_LOCK_SEED)
        .execute(&mut *blocker)
        .await
        .unwrap();
    let expires_at: chrono::DateTime<Utc> = sqlx::query_scalar(
        "UPDATE invitations SET expires_at = clock_timestamp() + interval '500 milliseconds' \
         WHERE id = $1 RETURNING expires_at",
    )
    .bind(invitation.invitation.id)
    .fetch_one(&mut *blocker)
    .await
    .unwrap();

    let accepting_store = store.clone();
    let token = invitation.material.token().to_owned();
    let acceptance = tokio::spawn(async move {
        accepting_store
            .accept_invitation(
                AcceptInvitation {
                    token,
                    display_name: "Waiting".to_owned(),
                    password_hash: hash_password("another correct local password").unwrap(),
                },
                &SessionMaterial::generate(),
                Duration::hours(1),
            )
            .await
    });
    let mut waiting = false;
    for _ in 0..100 {
        waiting = sqlx::query_scalar(
            "SELECT EXISTS (SELECT 1 FROM pg_locks \
             WHERE locktype = 'advisory' AND database = \
                   (SELECT oid FROM pg_database WHERE datname = current_database()) \
               AND NOT granted)",
        )
        .fetch_one(store.pool())
        .await
        .unwrap();
        if waiting {
            break;
        }
        tokio::time::sleep(std::time::Duration::from_millis(10)).await;
    }
    assert!(
        waiting,
        "invitation acceptance did not wait for the email lock"
    );
    loop {
        let expired: bool = sqlx::query_scalar("SELECT clock_timestamp() >= $1")
            .bind(expires_at)
            .fetch_one(store.pool())
            .await
            .unwrap();
        if expired {
            break;
        }
        tokio::time::sleep(std::time::Duration::from_millis(10)).await;
    }
    blocker.commit().await.unwrap();

    assert!(matches!(
        acceptance.await.unwrap(),
        Err(IdentityError::InvitationUnavailable)
    ));
    let member_exists: bool =
        sqlx::query_scalar("SELECT EXISTS (SELECT 1 FROM users WHERE email = $1)")
            .bind("waiting@example.test")
            .fetch_one(store.pool())
            .await
            .unwrap();
    assert!(!member_exists);
}
