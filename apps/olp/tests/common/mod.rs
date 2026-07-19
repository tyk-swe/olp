use std::sync::Arc;

use olp::ApiState;
use olp_storage::KeyHasher;

pub const BOOTSTRAP_TOKEN: &str = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=";

pub fn configure_bootstrap(state: &mut ApiState, key: [u8; 32]) {
    let key_hasher = Arc::new(KeyHasher::new(key));
    state.set_bootstrap_token_digest(
        key_hasher
            .bootstrap_token_digest_from_base64(BOOTSTRAP_TOKEN)
            .expect("test bootstrap token is valid base64"),
    );
    state.key_hasher = Some(key_hasher);
}
