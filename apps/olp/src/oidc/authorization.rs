use std::net::SocketAddr;

use axum::{
    Json,
    extract::{ConnectInfo, Extension, State},
    http::{HeaderMap, HeaderValue, StatusCode, header},
    response::{IntoResponse, Response},
};
use chrono::Utc;
use olp_storage::{
    NewOidcFlow, OidcFlowMaterial, OidcFlowPurpose, RecentAuthMaterial, RecentAuthPurpose,
    SessionPrincipal, oidc_flow_payload_aad as flow_payload_aad,
};
use serde::{Deserialize, Serialize};
use tracing::error;
use url::Url;
use utoipa::ToSchema;
use uuid::Uuid;
use zeroize::Zeroizing;

use super::error::map_oidc;
use super::helpers::{callback_url, require_master_key};
use super::session::{
    FLOW_COOKIE, FLOW_TTL, FlowSecretPayload, LOGIN_FLOW_COOKIE, LOGIN_FLOW_COOKIE_VERSION,
    LoginFlowCookiePayload, seal_login_flow_cookie,
};
use crate::{
    ApiState, Problem,
    management_api::{
        RECENT_AUTH_COOKIE, clear_recent_auth_cookie, cookie, reauthentication_required,
        require_mutation_session, require_store,
    },
};

#[derive(Debug, Serialize, ToSchema)]
pub struct OidcAuthorizationResponse {
    pub authorization_url: String,
}

#[derive(Debug, Deserialize, ToSchema)]
pub struct OidcReauthenticationRequest {
    /// Exact durable security operation that the resulting one-time grant may authorize.
    pub purpose: String,
    /// Required only when unlinking one specific OIDC identity.
    #[schema(value_type = Option<String>, format = Uuid)]
    pub resource_id: Option<Uuid>,
}

struct PersistentFlowContext<'a> {
    principal: &'a SessionPrincipal,
    recent_auth_purpose: Option<RecentAuthPurpose>,
    recent_auth_resource_id: Option<Uuid>,
    recent_auth_token_digest: Option<[u8; 32]>,
}

#[utoipa::path(
    get,
    path = "/api/v1/oidc/login",
    tag = "oidc",
    responses(
        (status = 303, description = "Redirect to the configured identity provider"),
        (status = 429, description = "OIDC login flow creation is rate limited", body = Problem),
        (status = 404, description = "OIDC is not configured or enabled", body = Problem),
        (status = 503, description = "OIDC dependency unavailable", body = Problem)
    )
)]
pub(super) async fn begin_login(
    State(state): State<ApiState>,
    connect_info: Option<Extension<ConnectInfo<SocketAddr>>>,
    headers: HeaderMap,
) -> Result<Response, Problem> {
    let peer = connect_info.map(|Extension(ConnectInfo(address))| address);
    // Admit before constructing cryptographic material or issuing a redirect.
    // The source digest is keyed and never persisted in its raw network form.
    let source_digest = crate::public_auth_source_digest(&state, &headers, peer)?;
    let admitted = require_store(&state)?
        .admit_oidc_login_attempt(source_digest)
        .await
        .map_err(|error| {
            error!(%error, "OIDC login admission failed");
            Problem::service_unavailable("database_unavailable")
        })?;
    if !admitted {
        return Err(Problem::new(
            StatusCode::TOO_MANY_REQUESTS,
            "oidc_flow_rate_limited",
            "Too many OIDC authorization attempts",
            "Too many OIDC authorization flows were started. Wait before retrying.",
        ));
    }
    begin_authorization(&state, OidcFlowPurpose::Login, None, true).await
}

#[utoipa::path(
    post,
    path = "/api/v1/oidc/link",
    tag = "oidc",
    responses(
        (status = 200, description = "Authorization URL for explicit identity linking", body = OidcAuthorizationResponse),
        (status = 401, description = "No active local session", body = Problem),
        (status = 403, description = "CSRF or origin check failed", body = Problem),
        (status = 428, description = "Recent authentication is required", body = Problem),
        (status = 429, description = "OIDC authorization flow creation is rate limited", body = Problem)
    )
)]
pub(super) async fn begin_link(
    State(state): State<ApiState>,
    headers: HeaderMap,
) -> Result<Response, Problem> {
    let principal = require_mutation_session(&state, &headers).await?;
    let recent_auth_token = cookie(&headers, RECENT_AUTH_COOKIE)
        .filter(|value| value.len() == 43)
        .ok_or_else(reauthentication_required)?;
    let context = PersistentFlowContext {
        principal: &principal,
        recent_auth_purpose: None,
        recent_auth_resource_id: None,
        recent_auth_token_digest: Some(RecentAuthMaterial::digest_token(recent_auth_token)),
    };
    let mut response =
        begin_authorization(&state, OidcFlowPurpose::Link, Some(context), false).await?;
    // The server-side grant is consumed in the same transaction that persists
    // the redirect flow. Remove the now-useless browser bearer immediately.
    clear_recent_auth_cookie(&mut response);
    Ok(response)
}

#[utoipa::path(
    post,
    path = "/api/v1/oidc/reauthenticate",
    tag = "oidc",
    request_body = OidcReauthenticationRequest,
    responses(
        (status = 200, description = "Authorization URL for fresh IdP authentication", body = OidcAuthorizationResponse),
        (status = 400, description = "Invalid operation scope", body = Problem),
        (status = 401, description = "No active local session", body = Problem),
        (status = 403, description = "CSRF or origin check failed", body = Problem),
        (status = 429, description = "OIDC authorization flow creation is rate limited", body = Problem)
    )
)]
pub(super) async fn begin_reauthentication(
    State(state): State<ApiState>,
    headers: HeaderMap,
    Json(request): Json<OidcReauthenticationRequest>,
) -> Result<Response, Problem> {
    let principal = require_mutation_session(&state, &headers).await?;
    let purpose = RecentAuthPurpose::parse(&request.purpose).ok_or_else(|| {
        Problem::bad_request(
            "invalid_reauthentication_purpose",
            "The requested reauthentication purpose is not supported.",
        )
    })?;
    if request.resource_id.is_some() != purpose.requires_resource() {
        return Err(Problem::bad_request(
            "invalid_reauthentication_resource",
            "The reauthentication target does not match the requested operation.",
        ));
    }
    let context = PersistentFlowContext {
        principal: &principal,
        recent_auth_purpose: Some(purpose),
        recent_auth_resource_id: request.resource_id,
        recent_auth_token_digest: None,
    };
    begin_authorization(
        &state,
        OidcFlowPurpose::Reauthenticate,
        Some(context),
        false,
    )
    .await
}

async fn begin_authorization(
    state: &ApiState,
    purpose: OidcFlowPurpose,
    context: Option<PersistentFlowContext<'_>>,
    redirect: bool,
) -> Result<Response, Problem> {
    let configuration = require_store(state)?
        .enabled_oidc_configuration()
        .await
        .map_err(map_oidc)?;
    let master_key = require_master_key(state)?;
    let material = OidcFlowMaterial::generate();
    let (flow_cookie_name, flow_cookie_value) = match purpose {
        OidcFlowPurpose::Login => {
            if context.is_some() {
                return Err(Problem::internal());
            }
            let payload = LoginFlowCookiePayload {
                version: LOGIN_FLOW_COOKIE_VERSION,
                flow_id: Uuid::now_v7(),
                state: material.state().to_owned(),
                nonce: material.nonce().to_owned(),
                pkce_verifier: material.pkce_verifier().to_owned(),
                configuration_id: configuration.id,
                configuration_etag: configuration.etag,
                expires_at_unix: (Utc::now() + FLOW_TTL).timestamp(),
            };
            (
                LOGIN_FLOW_COOKIE,
                seal_login_flow_cookie(state, master_key, &payload)?,
            )
        }
        OidcFlowPurpose::Link | OidcFlowPurpose::Reauthenticate => {
            let context = context.ok_or_else(Problem::internal)?;
            let flow_id = Uuid::now_v7();
            let payload = Zeroizing::new(
                serde_json::to_vec(&FlowSecretPayload {
                    nonce: material.nonce().to_owned(),
                    pkce_verifier: material.pkce_verifier().to_owned(),
                    configuration_etag: Some(configuration.etag),
                })
                .map_err(|_| Problem::internal())?,
            );
            let encrypted_payload = master_key
                .seal(&payload, &flow_payload_aad(flow_id))
                .map_err(|error| {
                    error!(%error, "OIDC authenticated flow encryption failed");
                    Problem::internal()
                })?;
            require_store(state)?
                .create_oidc_flow(NewOidcFlow {
                    id: flow_id,
                    configuration_id: configuration.id,
                    configuration_etag: configuration.etag,
                    purpose,
                    actor_user_id: Some(context.principal.user_id),
                    actor_session_id: Some(context.principal.session_id),
                    actor_security_version: Some(context.principal.security_version),
                    recent_auth_purpose: context.recent_auth_purpose,
                    recent_auth_resource_id: context.recent_auth_resource_id,
                    recent_auth_token_digest: context.recent_auth_token_digest,
                    state_digest: material.state_digest(),
                    browser_binding_digest: material.browser_binding_digest(),
                    encrypted_payload,
                    expires_at: Utc::now() + FLOW_TTL,
                })
                .await
                .map_err(map_oidc)?;
            (FLOW_COOKIE, material.browser_binding().to_owned())
        }
    };

    let mut authorization_url =
        Url::parse(&configuration.authorization_endpoint).map_err(|_| Problem::internal())?;
    let redirect_uri = callback_url(state)?;
    let scope = configuration.scopes.join(" ");
    let pkce_challenge = material.pkce_challenge();
    {
        let mut query = authorization_url.query_pairs_mut();
        query
            .append_pair("response_type", "code")
            .append_pair("client_id", &configuration.client_id)
            .append_pair("redirect_uri", &redirect_uri)
            .append_pair("scope", &scope)
            .append_pair("state", material.state())
            .append_pair("nonce", material.nonce())
            .append_pair("code_challenge", &pkce_challenge)
            .append_pair("code_challenge_method", "S256");
        if purpose == OidcFlowPurpose::Reauthenticate {
            // OIDC Core requires auth_time when max_age is requested. The
            // callback separately rejects stale or absent auth_time claims.
            query
                .append_pair("prompt", "login")
                .append_pair("max_age", "0");
        }
    }
    let flow_cookie = format!(
        "{flow_cookie_name}={flow_cookie_value}; Path=/; Max-Age={}; Secure; HttpOnly; SameSite=Lax",
        FLOW_TTL.num_seconds()
    );
    let mut response = if redirect {
        let mut response = StatusCode::SEE_OTHER.into_response();
        response.headers_mut().insert(
            header::LOCATION,
            HeaderValue::from_str(authorization_url.as_str()).map_err(|_| Problem::internal())?,
        );
        response
    } else {
        Json(OidcAuthorizationResponse {
            authorization_url: authorization_url.to_string(),
        })
        .into_response()
    };
    response.headers_mut().append(
        header::SET_COOKIE,
        HeaderValue::from_str(&flow_cookie).map_err(|_| Problem::internal())?,
    );
    response
        .headers_mut()
        .insert(header::CACHE_CONTROL, HeaderValue::from_static("no-store"));
    Ok(response)
}
