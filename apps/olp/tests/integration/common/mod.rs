use std::sync::Arc;

use olp::bootstrap::state::ProcessComposition;
use olp_db::security::key_material::AuthHmacKey;

pub(crate) const BOOTSTRAP_TOKEN: &str = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=";

pub(crate) fn configure_bootstrap(state: &mut ProcessComposition, key: [u8; 32]) {
    let auth_hmac_key = Arc::new(AuthHmacKey::new(key));
    state.set_bootstrap_token_digest(
        auth_hmac_key
            .bootstrap_token_digest_from_base64(BOOTSTRAP_TOKEN)
            .expect("test bootstrap token is valid base64"),
    );
    state.auth_hmac_key = Some(auth_hmac_key);
}
