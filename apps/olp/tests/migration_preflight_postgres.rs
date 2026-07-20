use std::process::{Command, Output};

use olp_storage::{MIGRATOR, PgStore};

const LEGACY_STREAM: &str = "olp:v2:usage";
const PERSISTENCE_GROUP: &str = "olp:persistence";

#[tokio::test]
#[ignore = "requires an empty PostgreSQL 18 database in OLP_TEST_DATABASE_URL and Valkey in OLP_VALKEY_URL"]
async fn migration_preflight_rejects_legacy_stream_before_schema_changes() {
    let database_url = std::env::var("OLP_TEST_DATABASE_URL")
        .expect("OLP_TEST_DATABASE_URL must point to an empty PostgreSQL 18 database");
    let valkey_url =
        std::env::var("OLP_VALKEY_URL").expect("OLP_VALKEY_URL must point to test Valkey");

    let store = PgStore::connect(&database_url, 3).await.unwrap();
    MIGRATOR.run_to(27, store.pool()).await.unwrap();
    store.pool().close().await;

    let client = redis::Client::open(valkey_url.as_str()).unwrap();
    let mut valkey = client.get_multiplexed_async_connection().await.unwrap();
    let _: u64 = redis::cmd("DEL")
        .arg(LEGACY_STREAM)
        .query_async(&mut valkey)
        .await
        .unwrap();
    let entry_id: String = redis::cmd("XADD")
        .arg(LEGACY_STREAM)
        .arg("*")
        .arg("event")
        .arg("fixture")
        .query_async(&mut valkey)
        .await
        .unwrap();

    let before_database = run_migrate("not-a-postgres-url", &valkey_url);
    assert_failed_with(&before_database, "LegacyRequestMetadataStreamNotDrained");

    let _: String = redis::cmd("XGROUP")
        .arg("CREATE")
        .arg(LEGACY_STREAM)
        .arg(PERSISTENCE_GROUP)
        .arg("0")
        .query_async(&mut valkey)
        .await
        .unwrap();
    let _: redis::Value = redis::cmd("XREADGROUP")
        .arg("GROUP")
        .arg(PERSISTENCE_GROUP)
        .arg("migration-preflight-test")
        .arg("STREAMS")
        .arg(LEGACY_STREAM)
        .arg(">")
        .query_async(&mut valkey)
        .await
        .unwrap();
    let acknowledged: u64 = redis::cmd("XACK")
        .arg(LEGACY_STREAM)
        .arg(PERSISTENCE_GROUP)
        .arg(entry_id)
        .query_async(&mut valkey)
        .await
        .unwrap();
    assert_eq!(acknowledged, 1);

    let acknowledged_remnant = run_migrate(&database_url, &valkey_url);
    assert_failed_with(
        &acknowledged_remnant,
        "LegacyRequestMetadataStreamAcknowledgedEntries",
    );
    assert_eq!(schema_state(&database_url).await, (false, true, false));

    let _: u64 = redis::cmd("XTRIM")
        .arg(LEGACY_STREAM)
        .arg("MAXLEN")
        .arg(0)
        .query_async(&mut valkey)
        .await
        .unwrap();
    let migrated = run_migrate(&database_url, &valkey_url);
    assert!(
        migrated.status.success(),
        "migration failed after draining the legacy stream: {}",
        String::from_utf8_lossy(&migrated.stderr)
    );
    assert_eq!(schema_state(&database_url).await, (true, false, true));

    let _: u64 = redis::cmd("DEL")
        .arg(LEGACY_STREAM)
        .query_async(&mut valkey)
        .await
        .unwrap();
}

fn run_migrate(database_url: &str, valkey_url: &str) -> Output {
    Command::new(env!("CARGO_BIN_EXE_olp"))
        .args([
            "migrate",
            "--database-url",
            database_url,
            "--valkey-url",
            valkey_url,
        ])
        .env_remove("OLP_DATABASE_URL")
        .env_remove("OLP_VALKEY_URL")
        .output()
        .unwrap()
}

fn assert_failed_with(output: &Output, expected: &str) {
    let stderr = String::from_utf8_lossy(&output.stderr);
    assert!(!output.status.success(), "migration unexpectedly succeeded");
    assert!(
        stderr.contains(expected),
        "migration error did not contain {expected}: {stderr}"
    );
}

async fn schema_state(database_url: &str) -> (bool, bool, bool) {
    let store = PgStore::connect(database_url, 3).await.unwrap();
    let migration_applied: bool = sqlx::query_scalar(
        "SELECT EXISTS (SELECT 1 FROM _sqlx_migrations WHERE version = 28 AND success)",
    )
    .fetch_one(store.pool())
    .await
    .unwrap();
    let legacy_table_exists: bool =
        sqlx::query_scalar("SELECT to_regclass('usage_event_receipts') IS NOT NULL")
            .fetch_one(store.pool())
            .await
            .unwrap();
    let renamed_table_exists: bool =
        sqlx::query_scalar("SELECT to_regclass('request_metadata_event_receipts') IS NOT NULL")
            .fetch_one(store.pool())
            .await
            .unwrap();
    store.pool().close().await;
    (migration_applied, legacy_table_exists, renamed_table_exists)
}
