use super::*;

// E6: a rejection at one bucket used to roll back the increments the wider
// buckets had already taken, so the source and global ceilings never advanced
// and the expiry sweep never ran.
#[tokio::test]
#[ignore = "requires OLP_TEST_DATABASE_ADMIN_URL and OLP_TEST_DATABASE_URL_PREFIX"]
async fn a_rejected_public_auth_attempt_still_consumes_the_wider_buckets() {
    let db = olp_db::test_support::TestDb::create_migrated("auth_admit").await;
    let store = db.store(3).await;

    let source = [11_u8; 32];
    let source_target = [22_u8; 32];
    for _ in 0..5 {
        assert!(
            store
                .admit_local_login_attempt(source, source_target)
                .await
                .unwrap()
        );
    }
    for _ in 0..3 {
        assert!(
            !store
                .admit_local_login_attempt(source, source_target)
                .await
                .unwrap(),
            "the 5/min source_target bucket must stay saturated"
        );
    }

    let attempts: Vec<(String, i32)> = sqlx::query_as(
        "SELECT scope, attempts FROM public_auth_rate_limits \
         WHERE action = 'local_login' ORDER BY scope",
    )
    .fetch_all(store.pool())
    .await
    .unwrap();
    assert_eq!(
        attempts,
        [
            ("global".to_owned(), 8),
            ("source".to_owned(), 8),
            // The saturated bucket itself stops counting; the wider ones the
            // attempt already passed through must not lose their increments.
            ("source_target".to_owned(), 5),
        ],
        "rejected attempts must still count against every bucket they reached"
    );

    // A rejection also has to run the expiry sweep, or a rejected-only stream
    // of traffic leaves stale rows behind forever.
    sqlx::query(
        "INSERT INTO public_auth_rate_limits \
         (action, scope, key_digest, window_started_at, attempts) \
         VALUES ('local_login', 'source', $1, now() - interval '20 minutes', 1)",
    )
    .bind([33_u8; 32].as_slice())
    .execute(store.pool())
    .await
    .unwrap();
    assert!(
        !store
            .admit_local_login_attempt(source, source_target)
            .await
            .unwrap()
    );
    let stale: i64 = sqlx::query_scalar(
        "SELECT count(*)::bigint FROM public_auth_rate_limits \
         WHERE window_started_at <= now() - interval '10 minutes'",
    )
    .fetch_one(store.pool())
    .await
    .unwrap();
    assert_eq!(
        stale, 0,
        "the expiry sweep must run on the rejection path too"
    );
}

// E7: an invitation is an out-of-band grant. Demoting or deactivating the
// inviter revokes their sessions; the token they minted must not outlive that.
// E12: an invitation that merely timed out must not be rewritten as revoked
// and attributed to an unrelated operator.
#[tokio::test]
#[ignore = "requires OLP_TEST_DATABASE_ADMIN_URL and OLP_TEST_DATABASE_URL_PREFIX"]
async fn invitations_do_not_outlive_the_inviter_and_expiry_is_not_revocation() {
    let db = olp_db::test_support::TestDb::create_migrated("invitations").await;
    let store = db.store(5).await;
    let master_key = MasterKey::new(1, [17; 32]);
    let founder = owner_id(&store, "invitations").await;

    // A second owner does the inviting, so demoting them cannot trip the
    // last-owner guard.
    let Outcome::Executed {
        value: co_owner_invitation,
        ..
    } = store
        .create_invitation(
            NewInvitation {
                email: "co-owner@invitations.test".to_owned(),
                role: Role::Owner,
                expires_at: Utc::now() + Duration::days(7),
                actor: founder,
                idempotency_key: "invite-co-owner-01".to_owned(),
            },
            Replayable::new([1; 32], &master_key),
            |_| Response::new(201, None, None, Vec::new()),
        )
        .await
        .unwrap()
    else {
        panic!("the invitation must execute");
    };
    let co_owner = store
        .accept_invitation(
            AcceptInvitation {
                token: co_owner_invitation.material.token().to_owned(),
                display_name: "Co-owner".to_owned(),
                password_hash: hash("correct horse battery staple").unwrap(),
            },
            &SessionMaterial::generate(),
            Duration::hours(1),
        )
        .await
        .unwrap()
        .user;

    let Outcome::Executed { value: granted, .. } = store
        .create_invitation(
            NewInvitation {
                email: "grantee@invitations.test".to_owned(),
                role: Role::Owner,
                expires_at: Utc::now() + Duration::days(7),
                actor: co_owner.id,
                idempotency_key: "invite-grantee-01".to_owned(),
            },
            Replayable::new([2; 32], &master_key),
            |_| Response::new(201, None, None, Vec::new()),
        )
        .await
        .unwrap()
    else {
        panic!("the invitation must execute");
    };

    // Demote the inviter. Their sessions are revoked; so is this grant.
    store
        .update_user_access(
            co_owner.id,
            Some(Role::Viewer),
            None,
            co_owner.etag,
            founder,
        )
        .await
        .unwrap();
    let error = store
        .accept_invitation(
            AcceptInvitation {
                token: granted.material.token().to_owned(),
                display_name: "Grantee".to_owned(),
                password_hash: hash("correct horse battery staple").unwrap(),
            },
            &SessionMaterial::generate(),
            Duration::hours(1),
        )
        .await
        .unwrap_err();
    assert!(matches!(error, IdentityError::InvitationUnavailable));
    let revoked_by: Option<Uuid> =
        sqlx::query_scalar("SELECT revoked_by FROM invitations WHERE id = $1")
            .bind(granted.invitation.id)
            .fetch_one(store.pool())
            .await
            .unwrap();
    assert_eq!(
        revoked_by,
        Some(founder),
        "losing ManageAccess must retire the invitations the user minted"
    );

    // E12: a timed-out invitation is freed from the pending-email index
    // without stamping anyone's revocation intent.
    let Outcome::Executed {
        value: timed_out, ..
    } = store
        .create_invitation(
            NewInvitation {
                email: "lapsed@invitations.test".to_owned(),
                role: Role::Viewer,
                expires_at: Utc::now() + Duration::days(1),
                actor: founder,
                idempotency_key: "invite-lapsed-01".to_owned(),
            },
            Replayable::new([3; 32], &master_key),
            |_| Response::new(201, None, None, Vec::new()),
        )
        .await
        .unwrap()
    else {
        panic!("the invitation must execute");
    };
    // `expires_at > created_at` is enforced, so age the whole row.
    sqlx::query(
        "UPDATE invitations \
         SET created_at = now() - interval '2 hours', expires_at = now() - interval '1 hour' \
         WHERE id = $1",
    )
    .bind(timed_out.invitation.id)
    .execute(store.pool())
    .await
    .unwrap();
    store
        .create_invitation(
            NewInvitation {
                email: "lapsed@invitations.test".to_owned(),
                role: Role::Viewer,
                expires_at: Utc::now() + Duration::days(1),
                actor: founder,
                idempotency_key: "invite-lapsed-02".to_owned(),
            },
            Replayable::new([4; 32], &master_key),
            |_| Response::new(201, None, None, Vec::new()),
        )
        .await
        .unwrap();
    let lapsed: (Option<DateTime<Utc>>, Option<DateTime<Utc>>, Option<Uuid>) =
        sqlx::query_as("SELECT expired_at, revoked_at, revoked_by FROM invitations WHERE id = $1")
            .bind(timed_out.invitation.id)
            .fetch_one(store.pool())
            .await
            .unwrap();
    assert!(lapsed.0.is_some(), "the timeout must be recorded as expiry");
    assert!(
        lapsed.1.is_none() && lapsed.2.is_none(),
        "a timeout must not be rewritten as an operator's revocation"
    );
}
