use chrono::{Duration, Utc};
use olp_db::{
    idempotency::Outcome, idempotency::Replayable, idempotency::Response, idempotency::fingerprint,
    identity::AcceptInvitation, identity::Error, identity::NewInvitation,
    security::envelope::MasterKey, security::password::hash,
    security::session_material::SessionMaterial,
};
use olp_engine::domain::auth::Role;
use uuid::Uuid;

#[tokio::test]
#[ignore = "requires OLP_TEST_DATABASE_ADMIN_URL and OLP_TEST_DATABASE_URL_PREFIX"]
async fn local_identity_lifecycle_is_transactional_and_audited() {
    let db = olp_db::test_support::TestDb::create_migrated("identity").await;
    let store = db.store(5).await;

    let owner_session = SessionMaterial::generate();
    let (owner, owner_session_id) = store
        .setup_installation_with_session(
            crate::support::owner_setup("example"),
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
        Err(Error::LastOwner)
    ));

    let operator_fingerprint = fingerprint(&"invite-operator-001").unwrap();
    let operator_invitation = store
        .create_invitation(
            NewInvitation {
                email: "operator@example.test".to_owned(),
                role: Role::Operator,
                expires_at: Utc::now() + Duration::days(7),
                actor: owner.user_id,
                idempotency_key: "invite-operator-001".to_owned(),
            },
            Replayable::new(operator_fingerprint, &master_key),
            |_| Response::new(201, None, None, Vec::new()),
        )
        .await
        .unwrap();
    let Outcome::Executed {
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
                password_hash: hash("another correct local password").unwrap(),
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
                    password_hash: hash("another correct local password").unwrap(),
                },
                &SessionMaterial::generate(),
                Duration::hours(12),
            )
            .await
            .is_err()
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

    let viewer_fingerprint = fingerprint(&"invite-viewer-0001").unwrap();
    let viewer_invitation = store
        .create_invitation(
            NewInvitation {
                email: "viewer@example.test".to_owned(),
                role: Role::Viewer,
                expires_at: Utc::now() + Duration::days(1),
                actor: owner.user_id,
                idempotency_key: "invite-viewer-0001".to_owned(),
            },
            Replayable::new(viewer_fingerprint, &master_key),
            |_| Response::new(201, None, None, Vec::new()),
        )
        .await
        .unwrap();
    let Outcome::Executed {
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
                    password_hash: hash("a third correct local password").unwrap(),
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
