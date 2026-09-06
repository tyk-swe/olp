use std::time::Duration;

use olp_db::{identity::InstallationSetupInput, store::Store, test_support::TestDb};
use uuid::Uuid;

async fn fixture() -> (TestDb, Store, Uuid, Uuid) {
    let db = TestDb::create_migrated("provider_model_snapshot").await;
    let store = db.store(4).await;
    let owner = store
        .setup_installation(InstallationSetupInput {
            installation_name: "Model snapshot".to_owned(),
            email: "owner@model-snapshot.test".to_owned(),
            display_name: "Owner".to_owned(),
            password_hash: "test-password-hash".to_owned(),
        })
        .await
        .unwrap();
    let provider_id = Uuid::now_v7();
    let model_id = Uuid::now_v7();
    sqlx::query(
        "INSERT INTO providers (id, name, kind, state, auth_mode, etag, created_by)
                 VALUES ($1, 'snapshot-provider', 'openai', 'draft', 'api_key', $2, $3)",
    )
    .bind(provider_id)
    .bind(Uuid::now_v7())
    .bind(owner.user_id)
    .execute(store.pool())
    .await
    .unwrap();
    sqlx::query(
        "INSERT INTO provider_models (id, provider_id, upstream_model, display_name, enabled)
                 VALUES ($1, $2, 'snapshot-model', 'Original model', true)",
    )
    .bind(model_id)
    .bind(provider_id)
    .execute(store.pool())
    .await
    .unwrap();
    sqlx::query(
        "INSERT INTO model_capabilities (provider_model_id, operation, surface, mode, source)
                 VALUES ($1, 'generation', 'openai', 'unary', 'declared')",
    )
    .bind(model_id)
    .execute(store.pool())
    .await
    .unwrap();
    (db, store, provider_id, model_id)
}

async fn wait_for_reader(pool: &sqlx::PgPool, blocker: i32) {
    tokio::time::timeout(Duration::from_secs(10), async {
        loop {
            let waiting: bool = sqlx::query_scalar(
                "SELECT EXISTS (SELECT 1 FROM pg_stat_activity
                 WHERE datname = current_database() AND $1 = ANY(pg_blocking_pids(pid)))",
            )
            .bind(blocker)
            .fetch_one(pool)
            .await
            .unwrap();
            if waiting {
                return;
            }
            tokio::task::yield_now().await;
        }
    })
    .await
    .expect("model page reader must reach the transaction barrier");
}

#[tokio::test]
#[ignore = "requires OLP_TEST_DATABASE_ADMIN_URL and OLP_TEST_DATABASE_URL_PREFIX"]
async fn provider_model_page_version_rows_and_capabilities_share_one_snapshot() {
    for lock_statement in [
        "LOCK TABLE provider_models IN ACCESS EXCLUSIVE MODE",
        "LOCK TABLE model_capabilities IN ACCESS EXCLUSIVE MODE",
    ] {
        let (db, store, provider_id, model_id) = fixture().await;
        let old_page = store
            .list_provider_models(provider_id, None, 10)
            .await
            .unwrap();
        let mut mutation = store.pool().begin().await.unwrap();
        sqlx::query(lock_statement)
            .execute(&mut *mutation)
            .await
            .unwrap();
        let blocker: i32 = sqlx::query_scalar("SELECT pg_backend_pid()")
            .fetch_one(&mut *mutation)
            .await
            .unwrap();
        let reader_store = store.clone();
        let reader = tokio::spawn(async move {
            reader_store
                .list_provider_models(provider_id, None, 10)
                .await
        });
        wait_for_reader(store.pool(), blocker).await;
        let new_etag = Uuid::now_v7();
        sqlx::query("UPDATE providers SET etag = $1 WHERE id = $2")
            .bind(new_etag)
            .bind(provider_id)
            .execute(&mut *mutation)
            .await
            .unwrap();
        sqlx::query("UPDATE provider_models SET display_name = 'Changed model', enabled = false WHERE id = $1")
            .bind(model_id).execute(&mut *mutation).await.unwrap();
        sqlx::query(
            "UPDATE model_capabilities SET mode = 'streaming' WHERE provider_model_id = $1",
        )
        .bind(model_id)
        .execute(&mut *mutation)
        .await
        .unwrap();
        mutation.commit().await.unwrap();
        let page = reader.await.unwrap().unwrap();
        assert_eq!(page.provider_etag, old_page.provider_etag);
        assert_eq!(page.items.len(), 1);
        assert_eq!(page.items[0].display_name, "Original model");
        assert!(page.items[0].enabled);
        assert_eq!(page.items[0].capabilities, old_page.items[0].capabilities);
        assert!(page.next_cursor.is_none());
        let changed = store
            .list_provider_models(provider_id, None, 10)
            .await
            .unwrap();
        assert_eq!(changed.provider_etag, new_etag);
        assert_eq!(changed.items[0].display_name, "Changed model");
        assert!(!changed.items[0].enabled);
        assert_eq!(changed.items[0].capabilities[0].mode.as_str(), "streaming");
        let empty = store
            .list_provider_models(provider_id, Some(model_id), 10)
            .await
            .unwrap();
        assert_eq!(empty.provider_etag, new_etag);
        assert!(empty.items.is_empty());
        assert!(matches!(
            store.list_provider_models(Uuid::now_v7(), None, 10).await,
            Err(olp_db::configuration::error::Error::NotFound)
        ));
        store.pool().close().await;
        drop(db);
    }
}
