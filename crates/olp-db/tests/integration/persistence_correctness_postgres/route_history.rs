use super::*;

// E1 + E11: a revision is immutable history and a draft is written back
// verbatim by the console. Neither read may drop a target because the
// provider's *current* revision no longer carries its model.
#[tokio::test]
#[ignore = "requires OLP_TEST_DATABASE_ADMIN_URL and OLP_TEST_DATABASE_URL_PREFIX"]
async fn route_targets_survive_a_provider_revision_that_dropped_their_model() {
    let db = olp_db::test_support::TestDb::create_migrated("route_history").await;
    let store = db.store(5).await;
    let actor = owner_id(&store, "route-history").await;
    let (provider_id, primary, secondary, _) =
        insert_two_model_provider(store.pool(), actor, "history").await;
    let (route_id, revision_id, draft_id) =
        insert_route(store.pool(), actor, "history", &[primary, secondary]).await;

    let before = store
        .get_route_revision(route_id, revision_id)
        .await
        .unwrap();
    assert_eq!(before.targets.len(), 2);
    assert!(before.targets.iter().all(|target| target.available));

    // A newer activated revision keeps only the primary model.
    let narrowed = insert_provider_revision(store.pool(), actor, provider_id, 2, &[primary]).await;
    sqlx::query("UPDATE providers SET active_revision_id = $1 WHERE id = $2")
        .bind(narrowed)
        .bind(provider_id)
        .execute(store.pool())
        .await
        .unwrap();

    let after = store
        .get_route_revision(route_id, revision_id)
        .await
        .unwrap();
    assert_eq!(
        after.targets.len(),
        2,
        "frozen revision history must not shrink when a provider revision drops a model"
    );
    let dropped = after
        .targets
        .iter()
        .find(|target| target.provider_model_id == secondary)
        .expect("the target whose model left the activated revision is still part of history");
    assert!(!dropped.available);
    assert_eq!(dropped.upstream_model, "history-secondary");
    assert!(
        after
            .targets
            .iter()
            .find(|target| target.provider_model_id == primary)
            .unwrap()
            .available
    );

    let draft = store.get_route_draft(draft_id).await.unwrap();
    assert_eq!(
        draft.targets.len(),
        2,
        "a draft read feeds a full-list write back; a hidden target would be deleted"
    );

    // E11: disabling the provider clears active_revision_id entirely. History
    // still has to survive that, or every past revision reads as empty.
    sqlx::query(
        "UPDATE providers SET state = 'disabled'::provider_state, active_revision_id = NULL \
         WHERE id = $1",
    )
    .bind(provider_id)
    .execute(store.pool())
    .await
    .unwrap();
    let disabled = store
        .get_route_revision(route_id, revision_id)
        .await
        .unwrap();
    assert_eq!(disabled.targets.len(), 2);
    assert!(disabled.targets.iter().all(|target| !target.available));
    assert_eq!(
        store.get_route_draft(draft_id).await.unwrap().targets.len(),
        2
    );
}

// E2: activation checked operation coverage, not per-target survival, so it
// could orphan a live route target and only fail later with an opaque
// "stored runtime configuration is invalid".
#[tokio::test]
#[ignore = "requires OLP_TEST_DATABASE_ADMIN_URL and OLP_TEST_DATABASE_URL_PREFIX"]
async fn activating_a_provider_that_orphans_a_live_route_target_names_it() {
    let db = olp_db::test_support::TestDb::create_migrated("orphan_target").await;
    let store = db.store(5).await;
    let actor = owner_id(&store, "orphan-target").await;
    let (provider_id, primary, secondary, etag) =
        insert_two_model_provider(store.pool(), actor, "orphan").await;
    insert_route(store.pool(), actor, "orphan", &[primary, secondary]).await;

    // The operator disables the secondary model and re-activates. The primary
    // still covers every route operation, so the coverage guard is satisfied.
    sqlx::query("UPDATE provider_models SET enabled = false WHERE id = $1")
        .bind(secondary)
        .execute(store.pool())
        .await
        .unwrap();
    sqlx::query(
        "UPDATE providers SET state = 'draft'::provider_state, updated_at = last_probe_at \
         WHERE id = $1",
    )
    .bind(provider_id)
    .execute(store.pool())
    .await
    .unwrap();

    let error = store
        .activate_provider(provider_id, etag, actor, "orphan-activate-01")
        .await
        .unwrap_err();
    let ConfigurationError::Invalid(detail) = &error else {
        panic!("activation must fail with a named target, got {error:?}");
    };
    assert!(detail.contains("orphan"), "{detail}");
    assert!(detail.contains("orphan-secondary"), "{detail}");

    // Re-enabling it makes the same activation succeed.
    sqlx::query("UPDATE provider_models SET enabled = true WHERE id = $1")
        .bind(secondary)
        .execute(store.pool())
        .await
        .unwrap();
    store
        .activate_provider(provider_id, etag, actor, "orphan-activate-02")
        .await
        .unwrap();
}

#[tokio::test]
#[ignore = "requires OLP_TEST_DATABASE_ADMIN_URL and OLP_TEST_DATABASE_URL_PREFIX"]
async fn concurrent_route_target_index_migrations_retry_leftover_relations() {
    let db = olp_db::test_support::TestDb::create_empty("index_retry").await;
    let store = db.store(2).await;
    store.migrate_to(45).await.unwrap();

    sqlx::query(
        "CREATE INDEX route_draft_targets_provider_model_idx \
         ON route_draft_targets(provider_model_id)",
    )
    .execute(store.pool())
    .await
    .unwrap();
    store.migrate_to(46).await.unwrap();

    sqlx::query(
        "CREATE INDEX route_revision_targets_provider_model_idx \
         ON route_revision_targets(provider_model_id)",
    )
    .execute(store.pool())
    .await
    .unwrap();
    store.migrate_to(47).await.unwrap();

    let indexes: Vec<(String, bool)> = sqlx::query_as(
        "SELECT indexrelid::regclass::text, indisvalid FROM pg_index \
         WHERE indexrelid IN ( \
           'route_draft_targets_provider_model_idx'::regclass, \
           'route_revision_targets_provider_model_idx'::regclass) \
         ORDER BY indexrelid::regclass::text",
    )
    .fetch_all(store.pool())
    .await
    .unwrap();
    assert_eq!(
        indexes,
        [
            ("route_draft_targets_provider_model_idx".to_owned(), true),
            ("route_revision_targets_provider_model_idx".to_owned(), true)
        ]
    );
}
