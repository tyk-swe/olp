//! Shared support for the `*_postgres.rs` integration tests. Not a test
//! target itself: `scripts/run-postgres-tests.sh` discovers only files
//! directly under `tests/`.
#![allow(dead_code)]

pub mod route_fixtures;

/// URL of the empty PostgreSQL 18 database each integration test expects.
/// `scripts/run-postgres-tests.sh` (`make db-test`) provisions one per test.
pub fn test_database_url() -> String {
    std::env::var("OLP_TEST_DATABASE_URL")
        .expect("OLP_TEST_DATABASE_URL must point to an empty PostgreSQL 18 database")
}
