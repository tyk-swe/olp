use olp_db::security::MasterKey;
use olp_engine::providers::OidcNetworkPolicy;

use crate::{ManagementState, Problem};

pub(super) fn callback_url(state: &ManagementState) -> Result<String, Problem> {
    Ok(state
        .public_origin
        .with_path("/api/v1/oidc/callback")
        .to_string())
}

pub(super) fn require_master_key(state: &ManagementState) -> Result<&MasterKey, Problem> {
    state
        .master_key
        .as_deref()
        .ok_or_else(|| Problem::service_unavailable("master_key_not_configured"))
}

pub(super) fn network_policy(state: &ManagementState) -> OidcNetworkPolicy {
    OidcNetworkPolicy {
        allow_insecure_test_endpoints: state.oidc_allow_insecure_test_endpoints,
    }
}

pub(super) fn valid_binding_token(value: &str) -> bool {
    value.len() == 43
        && value
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'-' | b'_'))
}

pub(super) fn valid_claim_name(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= 128
        && value
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'_' | b'.' | b':' | b'-'))
}

pub(super) fn oauth_form_component(value: &str) -> String {
    url::form_urlencoded::byte_serialize(value.as_bytes()).collect()
}
