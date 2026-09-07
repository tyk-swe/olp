use chrono::Duration;
use chrono::Utc;
use olp::access::identity::AcceptInvitation;
use olp::access::identity::Error;
use olp::access::identity::InstallationSetupInput;
use olp::access::identity::NewInvitation;
use olp::access::policy::Role;
use olp::crypto::envelope::MasterKey;
use olp::crypto::password::hash;
use olp::crypto::session_material::SessionMaterial;
use olp::database::idempotency::Outcome;
use olp::database::idempotency::Replayable;
use olp::database::idempotency::Response;
use olp::database::idempotency::fingerprint;
use uuid::Uuid;

#[tokio::test]
#[ignore = "requires OLP_TEST_DATABASE_ADMIN_URL and OLP_TEST_DATABASE_URL_PREFIX"]
async fn local_identity_lifecycle_is_transactional_and_audited() {
    let db = olp::test_support::TestDb::create_migrated("identity").await;
    let pool = db.pool(5).await;

    let owner_session = SessionMaterial::generate();
    let (owner, owner_session_id) = olp::access::identity::setup::setup_installation_with_session(
        &pool,
        &olp::database::RequestProvenance::default(),
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
        .execute(&pool)
        .await
        .unwrap();
    assert!(
        olp::access::authentication::sessions::session_principal(&(pool), owner_session.token())
            .await
            .unwrap()
            .is_some()
    );
    let touched_last_seen: chrono::DateTime<Utc> =
        sqlx::query_scalar("SELECT last_seen_at FROM sessions WHERE id = $1")
            .bind(owner_session_id)
            .fetch_one(&pool)
            .await
            .unwrap();
    assert!(touched_last_seen > stale_last_seen);

    let recent_last_seen: chrono::DateTime<Utc> = sqlx::query_scalar(
        "UPDATE sessions SET last_seen_at = $1 WHERE id = $2 RETURNING last_seen_at",
    )
    .bind(Utc::now() - Duration::minutes(1))
    .bind(owner_session_id)
    .fetch_one(&pool)
    .await
    .unwrap();
    let recent_row_version: String =
        sqlx::query_scalar("SELECT xmin::text FROM sessions WHERE id = $1")
            .bind(owner_session_id)
            .fetch_one(&pool)
            .await
            .unwrap();
    assert!(
        olp::access::authentication::sessions::session_principal(&(pool), owner_session.token())
            .await
            .unwrap()
            .is_some()
    );
    let unchanged_activity: (chrono::DateTime<Utc>, String) =
        sqlx::query_as("SELECT last_seen_at, xmin::text FROM sessions WHERE id = $1")
            .bind(owner_session_id)
            .fetch_one(&pool)
            .await
            .unwrap();
    assert_eq!(unchanged_activity.0, recent_last_seen);
    assert_eq!(unchanged_activity.1, recent_row_version);

    let owner_record = olp::access::identity::accounts::user(&pool, owner.user_id)
        .await
        .unwrap()
        .unwrap();
    let master_key = MasterKey::new(1, [7; 32]);
    assert_eq!(owner_record.role, Role::Owner);
    assert!(matches!(
        olp::access::identity::accounts::update_user_role(
            &(pool),
            &olp::database::RequestProvenance::default(),
            owner.user_id,
            Role::Viewer,
            owner_record.etag,
            owner.user_id,
        )
        .await,
        Err(Error::LastOwner)
    ));

    let operator_fingerprint = fingerprint(&"invite-operator-001").unwrap();
    let operator_invitation = olp::access::identity::invitations::create_invitation(
        &pool,
        &olp::database::RequestProvenance::default(),
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
    let accepted = olp::access::identity::invitations::accept_invitation(
        &pool,
        &olp::database::RequestProvenance::default(),
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
        olp::access::identity::invitations::accept_invitation(
            &(pool),
            &olp::database::RequestProvenance::default(),
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

    let updated = olp::access::identity::accounts::update_user_role(
        &pool,
        &olp::database::RequestProvenance::default(),
        accepted.user.id,
        Role::Developer,
        accepted.user.etag,
        owner.user_id,
    )
    .await
    .unwrap();
    assert_eq!(updated.role, Role::Developer);
    assert!(
        olp::access::identity::accounts::list_sessions(&(pool), accepted.user.id, None, 50)
            .await
            .unwrap()
            .0
            .is_empty()
    );

    let viewer_fingerprint = fingerprint(&"invite-viewer-0001").unwrap();
    let viewer_invitation = olp::access::identity::invitations::create_invitation(
        &pool,
        &olp::database::RequestProvenance::default(),
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
    let revoked = olp::access::identity::invitations::revoke_invitation(
        &pool,
        &olp::database::RequestProvenance::default(),
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
    let (listed_invitations, _) =
        olp::access::identity::invitations::list_invitations(&pool, None, 50)
            .await
            .unwrap();
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
        olp::access::identity::installation::installation_name(&(pool),)
            .await
            .unwrap()
            .as_deref(),
        Some("Identity integration")
    );
    assert!(
        olp::access::identity::invitations::accept_invitation(
            &(pool),
            &olp::database::RequestProvenance::default(),
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
    let expired_session_id = olp::access::authentication::sessions::create_session(
        &pool,
        &olp::database::RequestProvenance::default(),
        owner.user_id,
        1,
        &expired_session,
        Duration::hours(1),
    )
    .await
    .unwrap();
    let expired_last_seen: chrono::DateTime<Utc> = sqlx::query_scalar(
        "UPDATE sessions SET expires_at = now() - interval '1 second', last_seen_at = $1 \
         WHERE id = $2 RETURNING last_seen_at",
    )
    .bind(Utc::now() - Duration::minutes(10))
    .bind(expired_session_id)
    .fetch_one(&pool)
    .await
    .unwrap();
    let expired_row_version: String =
        sqlx::query_scalar("SELECT xmin::text FROM sessions WHERE id = $1")
            .bind(expired_session_id)
            .fetch_one(&pool)
            .await
            .unwrap();
    assert!(
        olp::access::authentication::sessions::session_principal(&(pool), expired_session.token())
            .await
            .unwrap()
            .is_none()
    );
    let unchanged_expired: (chrono::DateTime<Utc>, String) =
        sqlx::query_as("SELECT last_seen_at, xmin::text FROM sessions WHERE id = $1")
            .bind(expired_session_id)
            .fetch_one(&pool)
            .await
            .unwrap();
    assert_eq!(unchanged_expired.0, expired_last_seen);
    assert_eq!(unchanged_expired.1, expired_row_version);
    olp::access::authentication::sessions::record_local_login_failure(
        &pool,
        &olp::database::RequestProvenance::default(),
        Some(owner.user_id),
    )
    .await
    .unwrap();
    olp::access::authentication::sessions::record_local_login_failure(
        &pool,
        &olp::database::RequestProvenance::default(),
        None,
    )
    .await
    .unwrap();

    for _ in 0..5 {
        assert!(
            olp::access::identity::auth_admission::admit_local_login_attempt(
                &(pool),
                [11; 32],
                [12; 32]
            )
            .await
            .unwrap()
        );
        assert!(
            olp::access::identity::auth_admission::admit_invitation_acceptance_attempt(
                &(pool),
                [22; 32],
                [23; 32]
            )
            .await
            .unwrap()
        );
    }
    assert!(
        !olp::access::identity::auth_admission::admit_local_login_attempt(
            &(pool),
            [11; 32],
            [12; 32]
        )
        .await
        .unwrap()
    );
    assert!(
        !olp::access::identity::auth_admission::admit_invitation_acceptance_attempt(
            &(pool),
            [22; 32],
            [23; 32]
        )
        .await
        .unwrap()
    );
    // A source-plus-target lockout cannot be used to exhaust another source.
    assert!(
        olp::access::identity::auth_admission::admit_local_login_attempt(
            &(pool),
            [13; 32],
            [14; 32]
        )
        .await
        .unwrap()
    );
    let local_login_global_attempts: i32 = sqlx::query_scalar(
        "SELECT attempts FROM public_auth_rate_limits \
         WHERE action = 'local_login' AND scope = 'global'",
    )
    .fetch_one(&pool)
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
    .fetch_one(&pool)
    .await
    .unwrap();
    assert_eq!(opaque_rate_rows, 8);

    olp::access::identity::accounts::revoke_session(
        &pool,
        &olp::database::RequestProvenance::default(),
        owner_session_id,
        owner.user_id,
        false,
    )
    .await
    .unwrap();
    assert!(
        olp::access::authentication::sessions::session_principal(&(pool), owner_session.token())
            .await
            .unwrap()
            .is_none()
    );
    let audit_actions: Vec<String> = sqlx::query_scalar(
        "SELECT action FROM audit_events WHERE action IN \
         ('invitation.create', 'invitation.accept', 'invitation.revoke', \
          'user.create', 'user.role_update', 'session.create', 'session.revoke')",
    )
    .fetch_all(&pool)
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
    .fetch_all(&pool)
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
    let db = olp::test_support::TestDb::create_migrated("identity-page-cap").await;
    let pool = db.pool(5).await;
    let master_key = MasterKey::new(1, [7; 32]);
    let owner = olp::access::identity::setup::setup_installation(
        &pool,
        &olp::database::RequestProvenance::default(),
        InstallationSetupInput {
            installation_name: "Identity page cap".to_owned(),
            email: "owner@example.test".to_owned(),
            display_name: "Owner".to_owned(),
            password_hash: hash("correct horse battery staple").unwrap(),
        },
    )
    .await
    .unwrap();

    for index in 0..150 {
        let key = format!("invite-{index:04}");
        let invitation = olp::access::identity::invitations::create_invitation(
            &pool,
            &olp::database::RequestProvenance::default(),
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
        olp::access::identity::invitations::accept_invitation(
            &pool,
            &olp::database::RequestProvenance::default(),
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

    let (invitations, next) =
        olp::access::identity::invitations::list_invitations(&pool, None, 150)
            .await
            .unwrap();
    assert_eq!(invitations.len(), 150);
    assert!(next.is_none());
    let (invitations, next) =
        olp::access::identity::invitations::list_invitations(&pool, None, 500)
            .await
            .unwrap();
    assert_eq!(invitations.len(), 150);
    assert!(next.is_none());

    let (users, next) = olp::access::identity::accounts::list_users(&pool, None, 150)
        .await
        .unwrap();
    assert_eq!(users.len(), 150);
    assert!(next.is_some());
    let (users, _) = olp::access::identity::accounts::list_users(&pool, None, 500)
        .await
        .unwrap();
    assert_eq!(users.len(), 151);
}

#[tokio::test]
#[ignore = "requires OLP_TEST_DATABASE_ADMIN_URL and OLP_TEST_DATABASE_URL_PREFIX"]
async fn password_rotation_rejects_login_with_previously_fetched_credentials() {
    use olp::access::authentication::SessionSecurityContext;

    let db = olp::test_support::TestDb::create_migrated("stale-login").await;
    let pool = db.pool(5).await;
    let session = SessionMaterial::generate();
    let (owner, session_id) = olp::access::identity::setup::setup_installation_with_session(
        &pool,
        &olp::database::RequestProvenance::default(),
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
    let credentials =
        olp::access::authentication::sessions::local_password_user(&pool, "owner@example.test")
            .await
            .unwrap()
            .unwrap();
    let user = olp::access::identity::accounts::user(&pool, owner.user_id)
        .await
        .unwrap()
        .unwrap();
    olp::access::identity::accounts::update_local_password(
        &pool,
        &olp::database::RequestProvenance::default(),
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
    assert!(olp::crypto::password::verify(
        "original correct password",
        &credentials.password_hash
    ));
    let stale_session = SessionMaterial::generate();
    assert!(matches!(
        olp::access::authentication::sessions::create_session(
            &(pool),
            &olp::database::RequestProvenance::default(),
            credentials.id,
            credentials.security_version,
            &stale_session,
            Duration::hours(1),
        )
        .await,
        Err(olp::database::error::Error::SessionUnavailable)
    ));
    assert!(
        olp::access::authentication::sessions::session_principal(&(pool), stale_session.token())
            .await
            .unwrap()
            .is_none()
    );
    let current =
        olp::access::authentication::sessions::local_password_user(&pool, "owner@example.test")
            .await
            .unwrap()
            .unwrap();
    olp::access::authentication::sessions::create_session(
        &pool,
        &olp::database::RequestProvenance::default(),
        current.id,
        current.security_version,
        &SessionMaterial::generate(),
        Duration::hours(1),
    )
    .await
    .unwrap();
}
