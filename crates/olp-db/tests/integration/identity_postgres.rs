use chrono::{Duration, Utc};
use olp_db::{
    idempotency::Outcome, idempotency::Replayable, idempotency::Response, idempotency::fingerprint,
    identity::AcceptInvitation, identity::Error, identity::InstallationSetupInput,
    identity::NewInvitation, security::envelope::MasterKey, security::password::hash,
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
            InstallationSetupInput {
                installation_name: "Identity integration".to_owned(),
                email: "owner@example.test".to_owned(),
                display_name: "Owner".to_owned(),
                password_hash: hash("correct horse battery staple").unwrap(),
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
    assert_eq!(
        operator_invitation.invitation.invited_by_email.as_deref(),
        Some("owner@example.test")
    );
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
    assert_eq!(
        revoked.revoked_by_email.as_deref(),
        Some("owner@example.test")
    );
    let (listed_invitations, _) = store.list_invitations(None, 50).await.unwrap();
    let accepted_invitation = listed_invitations
        .iter()
        .find(|invitation| invitation.id == operator_invitation.invitation.id)
        .expect("accepted invitation is listed");
    assert_eq!(
        accepted_invitation.invited_by_email.as_deref(),
        Some("owner@example.test")
    );
    assert_eq!(
        accepted_invitation.accepted_by_email.as_deref(),
        Some("operator@example.test")
    );
    assert!(accepted_invitation.revoked_by_email.is_none());
    assert_eq!(
        store.installation_name().await.unwrap().as_deref(),
        Some("Identity integration")
    );
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
        .create_session(owner.user_id, 1, &expired_session, Duration::hours(1))
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
    // Five admitted, one rejected, one admitted from a fresh source. The
    // rejection counts too: an attempt that saturated the narrow
    // source-target bucket still consumed the wider ceilings it passed
    // through, so a locked-out caller cannot keep a free global budget.
    assert_eq!(local_login_global_attempts, 7);
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
#[ignore = "requires OLP_TEST_DATABASE_ADMIN_URL and OLP_TEST_DATABASE_URL_PREFIX"]
async fn identity_listings_honour_the_shared_page_cap() {
    let db = olp_db::test_support::TestDb::create_migrated("identity-page-cap").await;
    let store = db.store(5).await;
    let master_key = MasterKey::new(1, [7; 32]);
    let owner = store
        .setup_installation(InstallationSetupInput {
            installation_name: "Identity page cap".to_owned(),
            email: "owner@example.test".to_owned(),
            display_name: "Owner".to_owned(),
            password_hash: hash("correct horse battery staple").unwrap(),
        })
        .await
        .unwrap();

    for index in 0..150 {
        let key = format!("invite-{index:04}");
        let invitation = store
            .create_invitation(
                NewInvitation {
                    email: format!("user-{index:04}@example.test"),
                    role: Role::Viewer,
                    expires_at: Utc::now() + Duration::days(1),
                    actor: owner.user_id,
                    idempotency_key: key.clone(),
                },
                Replayable::new(fingerprint(&key).unwrap(), &master_key),
                |_| Response::new(201, None, None, Vec::new()),
            )
            .await
            .unwrap();
        let Outcome::Executed { value, .. } = invitation else {
            panic!("new invitation must execute");
        };
        store
            .accept_invitation(
                AcceptInvitation {
                    token: value.material.token().to_owned(),
                    display_name: format!("User {index}"),
                    password_hash: hash("correct horse battery staple").unwrap(),
                },
                &SessionMaterial::generate(),
                Duration::hours(12),
            )
            .await
            .unwrap();
    }

    let (invitations, next) = store.list_invitations(None, 150).await.unwrap();
    assert_eq!(invitations.len(), 150);
    assert!(next.is_none());
    let (invitations, next) = store.list_invitations(None, 500).await.unwrap();
    assert_eq!(invitations.len(), 150);
    assert!(next.is_none());

    let (users, next) = store.list_users(None, 150).await.unwrap();
    assert_eq!(users.len(), 150);
    assert!(next.is_some());
    let (users, _) = store.list_users(None, 500).await.unwrap();
    assert_eq!(users.len(), 151);
}

#[tokio::test]
#[ignore = "requires OLP_TEST_DATABASE_ADMIN_URL and OLP_TEST_DATABASE_URL_PREFIX"]
async fn password_rotation_rejects_login_with_previously_fetched_credentials() {
    use olp_db::authentication::SessionSecurityContext;

    let db = olp_db::test_support::TestDb::create_migrated("stale-login").await;
    let store = db.store(5).await;
    let session = SessionMaterial::generate();
    let (owner, session_id) = store
        .setup_installation_with_session(
            InstallationSetupInput {
                installation_name: "Stale login".into(),
                email: "owner@example.test".into(),
                display_name: "Owner".into(),
                password_hash: hash("original correct password").unwrap(),
            },
            &session,
            Duration::hours(1),
        )
        .await
        .unwrap();
    let credentials = store
        .local_password_user("owner@example.test")
        .await
        .unwrap()
        .unwrap();
    let user = store.user(owner.user_id).await.unwrap().unwrap();
    store
        .update_local_password(
            &hash("replacement correct password").unwrap(),
            user.etag,
            SessionSecurityContext {
                session_id,
                user_id: owner.user_id,
                security_version: credentials.security_version,
            },
            &SessionMaterial::generate(),
            Duration::hours(1),
        )
        .await
        .unwrap();
    assert!(olp_db::security::password::verify(
        "original correct password",
        &credentials.password_hash
    ));
    let stale_session = SessionMaterial::generate();
    assert!(matches!(
        store
            .create_session(
                credentials.id,
                credentials.security_version,
                &stale_session,
                Duration::hours(1),
            )
            .await,
        Err(olp_db::error::Error::SessionUnavailable)
    ));
    assert!(
        store
            .session_principal(stale_session.token())
            .await
            .unwrap()
            .is_none()
    );
    let current = store
        .local_password_user("owner@example.test")
        .await
        .unwrap()
        .unwrap();
    store
        .create_session(
            current.id,
            current.security_version,
            &SessionMaterial::generate(),
            Duration::hours(1),
        )
        .await
        .unwrap();
}
