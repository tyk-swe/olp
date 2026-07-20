use std::fmt;

use axum::{
    http::{HeaderValue, StatusCode, header},
    response::{IntoResponse, Response},
};
use base64::{Engine as _, engine::general_purpose::URL_SAFE_NO_PAD};
use chrono::{DateTime, Duration, Utc};
use olp_storage::{EncryptedSecret, MasterKey, OidcFlowPurpose, SessionMaterial, constant_time_eq};
use serde::{Deserialize, Serialize};
use tracing::error;
use uuid::Uuid;
use zeroize::{Zeroize, Zeroizing};

use super::error::invalid_login_flow_cookie;
use super::helpers::{require_master_key, valid_binding_token};
use crate::{ApiState, Problem};

pub(super) const SESSION_COOKIE: &str = "__Host-olp_session";
pub(super) const CSRF_COOKIE: &str = "__Host-olp_csrf";
/// Legacy persisted-flow cookie. New login flows deliberately do not use it;
/// it remains solely for authenticated link flows and login redirects created
/// by a pre-stateless-flow release.
pub(super) const FLOW_COOKIE: &str = "__Host-olp_oidc_flow";
pub(super) const LOGIN_FLOW_COOKIE: &str = "__Host-olp_oidc_login_flow";
pub(super) const LOGIN_FLOW_COOKIE_VERSION: u8 = 2;
pub(super) const FLOW_TTL: Duration = Duration::minutes(10);
const LOGIN_FLOW_COOKIE_MAX_BYTES: usize = 4 * 1024;

#[derive(Serialize, Deserialize)]
pub(super) struct FlowSecretPayload {
    pub(super) nonce: String,
    pub(super) pkce_verifier: String,
    #[serde(default)]
    pub(super) configuration_etag: Option<Uuid>,
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
pub(super) struct LoginFlowCookiePayload {
    pub(super) version: u8,
    pub(super) flow_id: Uuid,
    pub(super) state: String,
    pub(super) nonce: String,
    pub(super) pkce_verifier: String,
    pub(super) configuration_id: Uuid,
    pub(super) configuration_etag: Uuid,
    pub(super) expires_at_unix: i64,
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

pub(super) struct CallbackFlow {
    pub(super) purpose: OidcFlowPurpose,
    pub(super) actor_user_id: Option<Uuid>,
    pub(super) configuration_id: Uuid,
    pub(super) configuration_etag: Uuid,
    pub(super) login_consumption: Option<LoginFlowConsumption>,
    pub(super) secret: CallbackSecret,
}

pub(super) struct CallbackSecret {
    pub(super) nonce: String,
    pub(super) pkce_verifier: String,
}

impl Drop for CallbackSecret {
    fn drop(&mut self) {
        self.nonce.zeroize();
        self.pkce_verifier.zeroize();
    }
}

pub(super) struct LoginFlowConsumption {
    pub(super) flow_id: Uuid,
    pub(super) expires_at: DateTime<Utc>,
}

pub(super) fn authenticated_redirect(material: &SessionMaterial) -> Result<Response, Problem> {
    let mut response = StatusCode::SEE_OTHER.into_response();
    response
        .headers_mut()
        .insert(header::LOCATION, HeaderValue::from_static("/"));
    for cookie in [
        format!(
            "{SESSION_COOKIE}={}; Path=/; Max-Age=43200; Secure; HttpOnly; SameSite=Lax",
            material.token()
        ),
        format!(
            "{CSRF_COOKIE}={}; Path=/; Max-Age=43200; Secure; SameSite=Lax",
            material.csrf_token()
        ),
        clear_flow_cookie(FLOW_COOKIE),
    ] {
        response.headers_mut().append(
            header::SET_COOKIE,
            HeaderValue::from_str(&cookie).map_err(|_| Problem::internal())?,
        );
    }
    response
        .headers_mut()
        .insert(header::CACHE_CONTROL, HeaderValue::from_static("no-store"));
    Ok(response)
}

pub(super) fn seal_login_flow_cookie(
    state: &ApiState,
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

pub(super) fn consume_login_flow_cookie(
    state: &ApiState,
    encoded: &str,
    callback_state: &str,
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
        || !constant_time_eq(payload.state.as_bytes(), callback_state.as_bytes())
    {
        return Err(invalid_login_flow_cookie());
    }
    let expires_at = DateTime::from_timestamp(payload.expires_at_unix, 0)
        .ok_or_else(invalid_login_flow_cookie)?;
    Ok(CallbackFlow {
        purpose: OidcFlowPurpose::Login,
        actor_user_id: None,
        configuration_id: payload.configuration_id,
        configuration_etag: payload.configuration_etag,
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

fn login_flow_cookie_aad(state: &ApiState) -> Vec<u8> {
    // Keep the public origin in the authenticated context. A flow issued for
    // one operator-configured external origin cannot be replayed after an
    // origin change or against another deployment sharing a master key.
    format!(
        "olp:v{LOGIN_FLOW_COOKIE_VERSION}:oidc-login-flow:login:{}",
        state.public_origin
    )
    .into_bytes()
}

pub(super) fn clear_flow_cookie(name: &str) -> String {
    format!("{name}=; Path=/; Max-Age=0; Secure; HttpOnly; SameSite=Lax")
}

pub(super) fn append_cookie(response: &mut Response, value: String) {
    if let Ok(value) = HeaderValue::from_str(&value) {
        response.headers_mut().append(header::SET_COOKIE, value);
    }
}
