use olp_db::security::envelope::MasterKey;
use olp_engine::providers::oidc::Policy;

use crate::{management::state::ManagementState, public_http::problem::Problem};

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

pub(super) fn network_policy(state: &ManagementState) -> Policy {
    Policy {
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

#[cfg(test)]
mod tests {
    use std::{path::PathBuf, sync::Arc};

    use olp_db::security::envelope::MasterKey;
    use olp_engine::inference::runtime::Manager;

    use super::*;

    fn state() -> ManagementState {
        ManagementState::new(
            crate::application::mode::ApiMode::Control,
            None,
            Arc::new(Manager::empty()),
            "https://console.example.test",
            PathBuf::from("missing-console"),
        )
    }

    #[test]
    fn oidc_tokens_and_claim_names_use_distinct_character_policies() {
        assert!(valid_binding_token(&"a".repeat(43)));
        assert!(valid_binding_token(&format!("{}-_", "a".repeat(41))));
        for value in [
            "a".repeat(42),
            "a".repeat(44),
            format!("{}.", "a".repeat(42)),
        ] {
            assert!(!valid_binding_token(&value));
        }

        for value in ["email", "custom.groups:v2", "given_name"] {
            assert!(valid_claim_name(value));
        }
        for value in ["", "contains/slash", "contains space"] {
            assert!(!valid_claim_name(value));
        }
        assert!(!valid_claim_name(&"x".repeat(129)));
    }

    #[test]
    fn callback_and_form_encoding_preserve_origin_and_escape_values() {
        assert_eq!(
            callback_url(&state()).unwrap(),
            "https://console.example.test/api/v1/oidc/callback"
        );
        assert_eq!(oauth_form_component("a b+c/&"), "a+b%2Bc%2F%26");
    }

    #[test]
    fn master_key_requirement_and_network_policy_follow_process_state() {
        let mut state = state();
        assert_eq!(require_master_key(&state).unwrap_err().status, 503);
        assert!(!network_policy(&state).allow_insecure_test_endpoints);

        state.master_key = Some(Arc::new(MasterKey::new(3, [7; 32])));
        state.oidc_allow_insecure_test_endpoints = true;
        assert_eq!(require_master_key(&state).unwrap().version(), 3);
        assert!(network_policy(&state).allow_insecure_test_endpoints);
    }
}
