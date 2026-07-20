use axum::http::{HeaderMap, header};
use olp_providers::OidcNetworkPolicy;
use olp_storage::MasterKey;
use url::Url;
use uuid::Uuid;

use crate::{ApiState, Problem};

pub(super) fn callback_url(state: &ApiState) -> Result<String, Problem> {
    let mut url = Url::parse(state.public_origin.as_ref()).map_err(|_| Problem::internal())?;
    if url.host_str().is_none()
        || !url.username().is_empty()
        || url.password().is_some()
        || (!state.oidc_allow_insecure_test_endpoints && url.scheme() != "https")
    {
        return Err(Problem::service_unavailable("oidc_public_origin_invalid"));
    }
    url.set_path("/api/v1/oidc/callback");
    url.set_query(None);
    url.set_fragment(None);
    Ok(url.into())
}

pub(super) fn require_master_key(state: &ApiState) -> Result<&MasterKey, Problem> {
    state
        .master_key
        .as_deref()
        .ok_or_else(|| Problem::service_unavailable("master_key_not_configured"))
}

pub(super) fn network_policy(state: &ApiState) -> OidcNetworkPolicy {
    OidcNetworkPolicy {
        allow_insecure_test_endpoints: state.oidc_allow_insecure_test_endpoints,
    }
}

pub(super) fn optional_if_match(headers: &HeaderMap) -> Result<Option<Uuid>, Problem> {
    headers
        .get(header::IF_MATCH)
        .map(|value| {
            value
                .to_str()
                .ok()
                .and_then(|value| {
                    value
                        .strip_prefix('"')
                        .and_then(|value| value.strip_suffix('"'))
                })
                .and_then(|value| Uuid::parse_str(value).ok())
                .ok_or_else(|| {
                    Problem::bad_request(
                        "invalid_if_match",
                        "If-Match must contain one strong UUID ETag.",
                    )
                })
        })
        .transpose()
}

pub(super) fn cookie<'a>(headers: &'a HeaderMap, expected_name: &str) -> Option<&'a str> {
    headers
        .get(header::COOKIE)
        .and_then(|value| value.to_str().ok())
        .and_then(|cookies| {
            cookies.split(';').find_map(|cookie| {
                let (name, value) = cookie.trim().split_once('=')?;
                (name == expected_name).then_some(value)
            })
        })
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
