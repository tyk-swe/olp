//! Per-test PostgreSQL database provisioning for the `#[ignore]`d
//! integration suites (`make db-test`). Compiled only with the `test-util`
//! feature; every repository gate builds with `--all-features`, so the
//! module is always available to test targets.
//!
//! Each test creates its own uniquely named database through
//! `OLP_TEST_DATABASE_ADMIN_URL` and drops it best-effort on `Drop`.
//! Names carry the run token from `OLP_TEST_RUN_TOKEN`, so the
//! `scripts/run-postgres-tests.sh` sweep removes exactly this run's
//! leftovers when workers were killed before destructors could run.

use sqlx::{Connection as _, PgConnection};
use uuid::Uuid;

use crate::{MIGRATOR, PgStore};

/// A uniquely named PostgreSQL database owned by a single test.
///
/// Tests receive their database URL directly from this handle; nothing
/// mutates process-global environment variables, so tests in one process
/// and across nextest's process-per-test workers stay isolated.
pub struct TestDb {
    name: String,
    url: String,
    admin_url: String,
}

impl TestDb {
    /// Creates `olp_test_{label}_{uuid}` and applies every migration.
    pub async fn create_migrated(label: &str) -> Self {
        let db = Self::create_empty(label).await;
        let mut connection = db.connect().await;
        MIGRATOR
            .run(&mut connection)
            .await
            .expect("apply migrations to the per-test database");
        connection
            .close()
            .await
            .expect("close the migration connection");
        db
    }

    /// Creates `olp_test_{run}_{label}_{uuid}` without running migrations —
    /// for upgrade tests that replay the migration history themselves.
    ///
    /// The run segment comes from `OLP_TEST_RUN_TOKEN` (exported by
    /// `scripts/run-postgres-tests.sh`), which scopes the harness sweep to
    /// this run's databases so concurrent runs never delete each other's.
    pub async fn create_empty(label: &str) -> Self {
        let admin_url = required_env("OLP_TEST_DATABASE_ADMIN_URL");
        let prefix = required_env("OLP_TEST_DATABASE_URL_PREFIX");
        let owner = std::env::var("OLP_TEST_DATABASE_OWNER").unwrap_or_else(|_| "olp".to_owned());
        assert!(
            !owner.is_empty()
                && owner
                    .chars()
                    .all(|c| c.is_ascii_lowercase() || c.is_ascii_digit() || c == '_'),
            "OLP_TEST_DATABASE_OWNER must match [a-z0-9_]+, got {owner:?}"
        );
        let run_token = std::env::var("OLP_TEST_RUN_TOKEN").unwrap_or_else(|_| "adhoc".to_owned());

        // 9 (prefix) + 10 + 1 + 8 + 1 + 32 = 61 bytes, within PostgreSQL's
        // 63-byte identifier limit.
        let name = format!(
            "olp_test_{}_{}_{}",
            sanitize(&run_token, 10),
            sanitize(label, 8),
            Uuid::now_v7().simple()
        );
        debug_assert!(name.len() <= 63, "generated database name is too long");

        let mut admin = PgConnection::connect(&admin_url)
            .await
            .expect("connect to OLP_TEST_DATABASE_ADMIN_URL");
        // CREATE DATABASE cannot run inside a transaction; raw_sql uses the
        // simple query protocol on a plain connection, which never wraps one.
        // AssertSqlSafe: both identifiers are generated/validated above and
        // match [a-z0-9_]+ only.
        sqlx::raw_sql(sqlx::AssertSqlSafe(format!(
            r#"CREATE DATABASE "{name}" OWNER "{owner}""#
        )))
        .execute(&mut admin)
        .await
        .expect("create the per-test database");
        admin.close().await.expect("close the admin connection");

        let url = format!("{}/{name}", prefix.trim_end_matches('/'));
        Self {
            name,
            url,
            admin_url,
        }
    }

    /// Connection URL of this test's database.
    pub fn url(&self) -> &str {
        &self.url
    }

    /// Opens a [`PgStore`] pool on this test's database.
    pub async fn store(&self, max_connections: u32) -> PgStore {
        PgStore::connect(&self.url, max_connections)
            .await
            .expect("connect to the per-test database")
    }

    async fn connect(&self) -> PgConnection {
        PgConnection::connect(&self.url)
            .await
            .expect("connect to the per-test database")
    }
}

impl Drop for TestDb {
    fn drop(&mut self) {
        let admin_url = self.admin_url.clone();
        let name = self.name.clone();
        // A drop impl cannot block_on inside the test's own runtime; a
        // scratch thread with a current-thread runtime can. Best effort
        // only — the harness sweep covers whatever cleanup misses. The wait
        // is bounded by a completion channel rather than a join: joining
        // could stall past the async timeout (e.g. a blocking DNS lookup
        // outlives the future and runtime shutdown waits for it), so on
        // deadline the worker thread is simply detached.
        let (done_tx, done_rx) = std::sync::mpsc::channel::<()>();
        let spawned = std::thread::Builder::new()
            .name("olp-test-db-drop".to_owned())
            .spawn(move || {
                let Ok(runtime) = tokio::runtime::Builder::new_current_thread()
                    .enable_all()
                    .build()
                else {
                    return;
                };
                let cleanup = async move {
                    let mut admin = match PgConnection::connect(&admin_url).await {
                        Ok(admin) => admin,
                        Err(error) => {
                            eprintln!("test database cleanup: admin connect failed: {error}");
                            return;
                        }
                    };
                    // FORCE terminates pool connections the test left behind.
                    // AssertSqlSafe: the name was generated by create_empty
                    // and matches [a-z0-9_]+ only.
                    if let Err(error) = sqlx::raw_sql(sqlx::AssertSqlSafe(format!(
                        r#"DROP DATABASE IF EXISTS "{name}" WITH (FORCE)"#
                    )))
                    .execute(&mut admin)
                    .await
                    {
                        eprintln!("test database cleanup: dropping {name} failed: {error}");
                    }
                    let _ = admin.close().await;
                };
                // The timeout future must be constructed inside block_on:
                // tokio::time::timeout needs an ambient runtime, and this
                // scratch thread has none outside of it.
                let _ = runtime.block_on(async {
                    tokio::time::timeout(std::time::Duration::from_secs(15), cleanup).await
                });
                let _ = done_tx.send(());
            });
        if spawned.is_ok() {
            let _ = done_rx.recv_timeout(std::time::Duration::from_secs(20));
        }
    }
}

fn required_env(name: &str) -> String {
    std::env::var(name).unwrap_or_else(|_| {
        panic!("{name} must be set; see CONTRIBUTING.md \"Database test environment\"")
    })
}

/// Reduces a value to `[a-z0-9_]` and bounds it so the full database name
/// stays within PostgreSQL's 63-byte identifier limit.
fn sanitize(value: &str, max_len: usize) -> String {
    let mut cleaned: String = value
        .chars()
        .map(|c| {
            if c.is_ascii_alphanumeric() {
                c.to_ascii_lowercase()
            } else {
                '_'
            }
        })
        .collect();
    cleaned.truncate(max_len);
    if cleaned.is_empty() {
        cleaned.push('t');
    }
    cleaned
}
