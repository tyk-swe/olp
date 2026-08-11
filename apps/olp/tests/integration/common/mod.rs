#![allow(dead_code)]

use std::sync::Arc;

use olp::test_support::ProcessComposition;
use olp_db::security::AuthHmacKey;

/// URL of the empty PostgreSQL 18 database each integration test expects.
/// `scripts/run-postgres-tests.sh` (`make db-test`) provisions one per test.
pub fn test_database_url() -> String {
    std::env::var("OLP_TEST_DATABASE_URL")
        .expect("OLP_TEST_DATABASE_URL must point to an empty PostgreSQL 18 database")
}

pub const BOOTSTRAP_TOKEN: &str = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=";

pub fn configure_bootstrap(state: &mut ProcessComposition, key: [u8; 32]) {
    let auth_hmac_key = Arc::new(AuthHmacKey::new(key));
    state.set_bootstrap_token_digest(
        auth_hmac_key
            .bootstrap_token_digest_from_base64(BOOTSTRAP_TOKEN)
            .expect("test bootstrap token is valid base64"),
    );
    state.auth_hmac_key = Some(auth_hmac_key);
}
