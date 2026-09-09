use std::fmt;

use crate::access::authentication::RecentAuthPurpose;
use crate::access::oidc::repository::types::OidcFlowPurpose;
use crate::crypto::envelope::EncryptedSecret;
use crate::crypto::envelope::MasterKey;
use crate::crypto::session_material::RecentAuthMaterial;
use crate::crypto::session_material::SessionMaterial;
use crate::crypto::session_material::constant_time_eq;
use axum::http::HeaderMap;
use axum::http::HeaderValue;
use axum::http::StatusCode;
use axum::http::header;
use axum::response::IntoResponse;
use axum::response::Response;
use base64::Engine as _;
use base64::engine::general_purpose::URL_SAFE_NO_PAD;
use chrono::DateTime;
use chrono::Duration;
use chrono::Utc;
use serde::Deserialize;
use serde::Serialize;
use tracing::error;
use uuid::Uuid;
use zeroize::Zeroize;
use zeroize::Zeroizing;

use crate::access::cookies::append_recent_auth_cookie;
use crate::access::cookies::append_security_transition_cookies;
use crate::access::oidc::http::error::invalid_login_flow_cookie;
use crate::access::oidc::http::helpers::require_master_key;
use crate::access::oidc::http::helpers::valid_binding_token;
use crate::http::control::response_policy::prevent_sensitive_response_caching;
use crate::http::control::state::ManagementState;
use crate::http::problem::Problem;
use crate::http::relative_url::RelativeReturnTo;
use crate::http::request_cookies::OIDC_LINK_FLOW_COOKIE_PREFIX;
use crate::http::request_cookies::OIDC_LOGIN_FLOW_COOKIE_PREFIX;
use crate::http::request_cookies::RequestCookies;

pub(crate) const LOGIN_FLOW_COOKIE_VERSION: u8 = 3;
pub(crate) const FLOW_TTL: Duration = Duration::minutes(10);
pub(crate) const RECENT_AUTH_TTL: Duration = Duration::minutes(5);
pub(crate) const MAX_ACTIVE_BROWSER_FLOWS: usize = 4;
const LOGIN_FLOW_COOKIE_MAX_BYTES: usize = 4 * 1024;

#[derive(Clone, Copy, Debug, Eq, Ord, PartialEq, PartialOrd)]
pub(crate) struct OidcFlowId(Uuid);

impl OidcFlowId {
    #[must_use]
    pub(crate) fn generate() -> Self {
        Self(Uuid::now_v7())
    }

    #[cfg(test)]
    #[must_use]
    pub(crate) const fn from_uuid(value: Uuid) -> Self {
        Self(value)
    }

    pub(crate) fn parse(value: &str) -> Option<Self> {
        let parsed = Uuid::parse_str(value).ok()?;
        (parsed.hyphenated().to_string() == value).then_some(Self(parsed))
    }

    #[must_use]
    pub(crate) const fn as_uuid(self) -> Uuid {
        self.0
    }
}

impl fmt::Display for OidcFlowId {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(formatter, "{}", self.0.hyphenated())
    }
}

pub(crate) struct OidcCallbackState {
    flow_id: OidcFlowId,
    secret: Zeroizing<String>,
}

impl OidcCallbackState {
    pub(crate) fn parse(value: String) -> Result<Self, Problem> {
        let Some((flow_id, secret)) = value.split_once('.') else {
            return Err(crate::access::oidc::http::error::invalid_callback());
        };
        if secret.contains('.') || !valid_binding_token(secret) {
            return Err(crate::access::oidc::http::error::invalid_callback());
        }
        let flow_id = OidcFlowId::parse(flow_id)
            .ok_or_else(crate::access::oidc::http::error::invalid_callback)?;
        Ok(Self {
            flow_id,
            secret: Zeroizing::new(secret.to_owned()),
        })
    }

    #[must_use]
    pub(crate) fn encode(flow_id: OidcFlowId, secret: &str) -> String {
        format!("{flow_id}.{secret}")
    }

    #[must_use]
    pub(crate) const fn flow_id(&self) -> OidcFlowId {
        self.flow_id
    }

    #[must_use]
    pub(crate) fn secret(&self) -> &str {
        &self.secret
    }

    #[must_use]
    pub(crate) fn login_cookie_name(&self) -> String {
        flow_cookie_name(OidcFlowPurpose::Login, self.flow_id)
    }

    #[must_use]
    pub(crate) fn authenticated_cookie_name(&self) -> String {
        flow_cookie_name(OidcFlowPurpose::Link, self.flow_id)
    }
}

#[derive(Serialize, Deserialize)]
pub(crate) struct FlowSecretPayload {
    pub(crate) nonce: String,
    pub(crate) pkce_verifier: String,
    #[serde(default)]
    pub(crate) configuration_etag: Option<Uuid>,
    #[serde(default)]
    pub(crate) actor_session_id: Option<Uuid>,
}

impl fmt::Debug for FlowSecretPayload {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str("FlowSecretPayload([REDACTED])")
    }
}

impl Drop for FlowSecretPayload {
    fn drop(&mut self) {
        self.nonce.zeroize();
        self.pkce_verifier.zeroize();
    }
}

/// The encrypted, short-lived browser-held material for a login flow. The
/// authorization code is never included here. Encryption authenticates every
/// field, while the callback additionally validates its expiry and the exact
/// OIDC configuration generation before exchanging a code.
#[derive(Serialize, Deserialize)]
pub(crate) struct LoginFlowCookiePayload {
    pub(crate) version: u8,
    pub(crate) flow_id: Uuid,
    pub(crate) state: String,
    pub(crate) nonce: String,
    pub(crate) pkce_verifier: String,
    pub(crate) configuration_id: Uuid,
    pub(crate) configuration_etag: Uuid,
    pub(crate) expires_at_unix: i64,
    #[serde(default)]
    pub(crate) return_to: RelativeReturnTo,
}

impl fmt::Debug for LoginFlowCookiePayload {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str("LoginFlowCookiePayload([REDACTED])")
    }
}

impl Drop for LoginFlowCookiePayload {
    fn drop(&mut self) {
        self.state.zeroize();
        self.nonce.zeroize();
        self.pkce_verifier.zeroize();
    }
}

pub(crate) struct CallbackFlow {
    pub(crate) purpose: OidcFlowPurpose,
    pub(crate) actor_user_id: Option<Uuid>,
    pub(crate) actor_session_id: Option<Uuid>,
    pub(crate) actor_security_version: Option<i64>,
    pub(crate) recent_auth_purpose: Option<RecentAuthPurpose>,
    pub(crate) recent_auth_resource_id: Option<Uuid>,
    pub(crate) configuration_id: Uuid,
    pub(crate) configuration_etag: Uuid,
    pub(crate) return_to: RelativeReturnTo,
    pub(crate) login_consumption: Option<LoginFlowConsumption>,
    pub(crate) secret: CallbackSecret,
}

pub(crate) struct CallbackSecret {
    pub(crate) nonce: String,
    pub(crate) pkce_verifier: String,
}

impl Drop for CallbackSecret {
    fn drop(&mut self) {
        self.nonce.zeroize();
        self.pkce_verifier.zeroize();
    }
}

pub(crate) struct LoginFlowConsumption {
    pub(crate) flow_id: Uuid,
    pub(crate) expires_at: DateTime<Utc>,
}

pub(crate) fn authenticated_redirect(
    material: &SessionMaterial,
    destination: &RelativeReturnTo,
    session_ttl: Duration,
) -> Result<Response, Problem> {
    let mut response = successful_redirect(destination)?;
    append_security_transition_cookies(&mut response, material, session_ttl)?;
    Ok(response)
}

pub(crate) fn reauthenticated_redirect(
    material: &RecentAuthMaterial,
    purpose: RecentAuthPurpose,
    resource_id: Option<Uuid>,
) -> Result<Response, Problem> {
    let location = match resource_id {
        Some(resource_id) => format!(
            "/settings/profile?reauthenticated={}&resource_id={resource_id}",
            purpose.as_str()
        ),
        None => format!("/settings/profile?reauthenticated={}", purpose.as_str()),
    };
    let mut response = StatusCode::SEE_OTHER.into_response();
    response.headers_mut().insert(
        header::LOCATION,
        HeaderValue::from_str(&location).map_err(|_| Problem::internal())?,
    );
    append_recent_auth_cookie(&mut response, material, RECENT_AUTH_TTL)?;
    prevent_sensitive_response_caching(&mut response);
    Ok(response)
}

pub(crate) fn successful_redirect(destination: &RelativeReturnTo) -> Result<Response, Problem> {
    let mut response = StatusCode::SEE_OTHER.into_response();
    response.headers_mut().insert(
        header::LOCATION,
        HeaderValue::from_str(destination.as_str()).map_err(|_| Problem::internal())?,
    );
    prevent_sensitive_response_caching(&mut response);
    Ok(response)
}

pub(crate) fn seal_login_flow_cookie(
    state: &ManagementState,
    master_key: &MasterKey,
    payload: &LoginFlowCookiePayload,
) -> Result<String, Problem> {
    let plaintext = Zeroizing::new(serde_json::to_vec(payload).map_err(|_| Problem::internal())?);
    let encrypted = master_key
        .seal(&plaintext, &login_flow_cookie_aad(state))
        .map_err(|error| {
            error!(%error, "OIDC login flow cookie encryption failed");
            Problem::internal()
        })?;
    let value = format!(
        "v{LOGIN_FLOW_COOKIE_VERSION}.{}.{}.{}",
        encrypted.key_version,
        URL_SAFE_NO_PAD.encode(encrypted.nonce),
        URL_SAFE_NO_PAD.encode(encrypted.ciphertext),
    );
    if value.len() > LOGIN_FLOW_COOKIE_MAX_BYTES {
        error!(
            length = value.len(),
            "OIDC login flow cookie unexpectedly exceeded its bound"
        );
        return Err(Problem::internal());
    }
    Ok(value)
}

pub(crate) fn consume_login_flow_cookie(
    state: &ManagementState,
    encoded: &str,
    callback_state: &OidcCallbackState,
) -> Result<CallbackFlow, Problem> {
    let encrypted = parse_login_flow_cookie_envelope(encoded)?;
    let master_key = require_master_key(state)?;
    let plaintext = Zeroizing::new(
        master_key
            .open(&encrypted, &login_flow_cookie_aad(state))
            .map_err(|_| invalid_login_flow_cookie())?,
    );
    let mut payload: LoginFlowCookiePayload =
        serde_json::from_slice(&plaintext).map_err(|_| invalid_login_flow_cookie())?;
    let now = Utc::now().timestamp();
    if payload.version != LOGIN_FLOW_COOKIE_VERSION
        || payload.expires_at_unix <= now
        || payload.expires_at_unix > now + FLOW_TTL.num_seconds() + 60
        || !valid_binding_token(&payload.state)
        || !valid_binding_token(&payload.nonce)
        || !valid_binding_token(&payload.pkce_verifier)
        || !constant_time_eq(payload.state.as_bytes(), callback_state.secret().as_bytes())
        || callback_state.flow_id().as_uuid() != payload.flow_id
    {
        return Err(invalid_login_flow_cookie());
    }
    let expires_at = DateTime::from_timestamp(payload.expires_at_unix, 0)
        .ok_or_else(invalid_login_flow_cookie)?;
    Ok(CallbackFlow {
        purpose: OidcFlowPurpose::Login,
        actor_user_id: None,
        actor_session_id: None,
        actor_security_version: None,
        recent_auth_purpose: None,
        recent_auth_resource_id: None,
        configuration_id: payload.configuration_id,
        configuration_etag: payload.configuration_etag,
        return_to: payload.return_to.clone(),
        login_consumption: Some(LoginFlowConsumption {
            flow_id: payload.flow_id,
            expires_at,
        }),
        secret: CallbackSecret {
            nonce: std::mem::take(&mut payload.nonce),
            pkce_verifier: std::mem::take(&mut payload.pkce_verifier),
        },
    })
}

fn parse_login_flow_cookie_envelope(value: &str) -> Result<EncryptedSecret, Problem> {
    if value.is_empty() || value.len() > LOGIN_FLOW_COOKIE_MAX_BYTES {
        return Err(invalid_login_flow_cookie());
    }
    let mut parts = value.split('.');
    let (Some(version), Some(key_version), Some(nonce), Some(ciphertext), None) = (
        parts.next(),
        parts.next(),
        parts.next(),
        parts.next(),
        parts.next(),
    ) else {
        return Err(invalid_login_flow_cookie());
    };
    if version.as_bytes() != [b'v', b'0' + LOGIN_FLOW_COOKIE_VERSION]
        || key_version.is_empty()
        || nonce.is_empty()
        || ciphertext.is_empty()
    {
        return Err(invalid_login_flow_cookie());
    }
    let key_version = key_version
        .parse::<u32>()
        .ok()
        .filter(|value| *value > 0)
        .ok_or_else(invalid_login_flow_cookie)?;
    let nonce = URL_SAFE_NO_PAD
        .decode(nonce)
        .map_err(|_| invalid_login_flow_cookie())?
        .try_into()
        .map_err(|_| invalid_login_flow_cookie())?;
    let ciphertext = URL_SAFE_NO_PAD
        .decode(ciphertext)
        .map_err(|_| invalid_login_flow_cookie())?;
    if ciphertext.len() < 16 || ciphertext.len() > LOGIN_FLOW_COOKIE_MAX_BYTES {
        return Err(invalid_login_flow_cookie());
    }
    Ok(EncryptedSecret {
        key_version,
        nonce,
        ciphertext,
    })
}

fn login_flow_cookie_aad(state: &ManagementState) -> Vec<u8> {
    // Keep the public origin in the authenticated context. A flow issued for
    // one operator-configured external origin cannot be replayed after an
    // origin change or against another deployment sharing a master key.
    format!(
        "olp:v{LOGIN_FLOW_COOKIE_VERSION}:oidc-login-flow:login:{}",
        state.public_origin
    )
    .into_bytes()
}

#[must_use]
pub(crate) fn flow_cookie_name(purpose: OidcFlowPurpose, flow_id: OidcFlowId) -> String {
    let prefix = match purpose {
        OidcFlowPurpose::Login => OIDC_LOGIN_FLOW_COOKIE_PREFIX,
        OidcFlowPurpose::Link | OidcFlowPurpose::Reauthenticate => OIDC_LINK_FLOW_COOKIE_PREFIX,
    };
    format!("{prefix}{flow_id}")
}

pub(crate) fn flow_cookie_evictions(headers: &HeaderMap) -> Result<Vec<HeaderValue>, Problem> {
    let cookies = RequestCookies::parse(headers)?;
    let mut active = Vec::<(OidcFlowId, String)>::new();
    for prefix in [OIDC_LOGIN_FLOW_COOKIE_PREFIX, OIDC_LINK_FLOW_COOKIE_PREFIX] {
        for name in cookies.names_with_prefix(prefix) {
            if let Some(flow_id) = name.strip_prefix(prefix).and_then(OidcFlowId::parse) {
                active.push((flow_id, name.to_owned()));
            }
        }
    }

    active.sort_by(|left, right| left.0.cmp(&right.0).then_with(|| left.1.cmp(&right.1)));
    let overflow = active
        .len()
        .saturating_add(1)
        .saturating_sub(MAX_ACTIVE_BROWSER_FLOWS);
    Ok(active
        .into_iter()
        .take(overflow)
        .map(|(_, name)| clear_flow_cookie(&name))
        .collect())
}

/// Flow cookie names are a constant prefix followed by a canonical UUID, so
/// the expiry header is always representable.
#[must_use]
pub(crate) fn clear_flow_cookie(name: &str) -> HeaderValue {
    HeaderValue::from_str(&format!(
        "{name}=; Path=/; Max-Age=0; Secure; HttpOnly; SameSite=Lax"
    ))
    .expect("flow cookie names are a constant prefix and a canonical UUID")
}

#[cfg(test)]
mod flow_tests {
    use axum::http::HeaderValue;

    use crate::access::oidc::http::session::*;

    #[test]
    fn scoped_state_round_trips_and_selects_distinct_cookie_names() {
        let flow_id = OidcFlowId::generate();
        let encoded = OidcCallbackState::encode(flow_id, &"a".repeat(43));
        let state = OidcCallbackState::parse(encoded).unwrap();
        assert_eq!(state.flow_id(), flow_id);
        assert_eq!(state.secret(), "a".repeat(43).as_str());
        assert_ne!(state.login_cookie_name(), state.authenticated_cookie_name());
        assert!(state.login_cookie_name().ends_with(&flow_id.to_string()));
        assert_eq!(
            flow_cookie_name(OidcFlowPurpose::Link, flow_id),
            flow_cookie_name(OidcFlowPurpose::Reauthenticate, flow_id)
        );
    }

    #[test]
    fn callback_state_requires_the_protected_flow_id() {
        assert!(OidcCallbackState::parse("a".repeat(43)).is_err());
    }

    #[test]
    fn active_flow_bound_evicts_oldest_uuidv7_cookie_deterministically() {
        let mut ids = (0..MAX_ACTIVE_BROWSER_FLOWS + 2)
            .map(|_| OidcFlowId::generate())
            .collect::<Vec<_>>();
        ids.sort();
        let mut headers = HeaderMap::new();
        for id in ids.iter().rev() {
            headers.append(
                header::COOKIE,
                HeaderValue::from_str(&format!(
                    "{}=binding",
                    flow_cookie_name(OidcFlowPurpose::Login, *id)
                ))
                .unwrap(),
            );
        }
        let evictions = flow_cookie_evictions(&headers).unwrap();
        assert_eq!(evictions.len(), 3);
        for (eviction, flow_id) in evictions.iter().zip(ids.iter()) {
            assert!(eviction.to_str().unwrap().starts_with(&format!(
                "{}=;",
                flow_cookie_name(OidcFlowPurpose::Login, *flow_id)
            )));
        }
    }
}
