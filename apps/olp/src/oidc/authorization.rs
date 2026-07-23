use std::net::SocketAddr;

use axum::{
    Json,
    extract::{
        ConnectInfo, Extension, Query, State,
        rejection::{JsonRejection, QueryRejection},
    },
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
    FLOW_TTL, FlowSecretPayload, LOGIN_FLOW_COOKIE_VERSION, LoginFlowCookiePayload,
    OidcCallbackState, OidcFlowId, append_cookie, flow_cookie_evictions, flow_cookie_name,
    seal_login_flow_cookie,
};
use crate::{
    ManagementState, Problem, RelativeReturnTo,
    management_api::{
        RECENT_AUTH_COOKIE, clear_recent_auth_cookie, cookie, enforce_origin, json_payload,
        reauthentication_required, require_mutation_session,
    },
};

#[derive(Debug, Serialize, ToSchema)]
pub struct OidcAuthorizationResponse {
    pub authorization_url: String,
}

#[derive(Debug, Deserialize, ToSchema)]
pub struct OidcLoginRequest {
    /// Same-origin absolute-path destination used only after a successful callback.
    pub return_to: Option<String>,
}

#[derive(Debug, Default, Deserialize)]
pub(super) struct OidcLoginQuery {
    return_to: Option<String>,
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
    params(
        ("return_to" = Option<String>, Query, description = "Validated same-origin relative destination after login")
    ),
    responses(
        (status = 303, description = "Redirect to the configured identity provider"),
        (status = 429, description = "OIDC login flow creation is rate limited", body = Problem),
        (status = 404, description = "OIDC is not configured or enabled", body = Problem),
        (status = 503, description = "OIDC dependency unavailable", body = Problem)
    )
)]
pub(super) async fn begin_login(
    State(state): State<ManagementState>,
    connect_info: Option<Extension<ConnectInfo<SocketAddr>>>,
    headers: HeaderMap,
    query: Result<Query<OidcLoginQuery>, QueryRejection>,
) -> Result<Response, Problem> {
    admit_login(&state, &headers, connect_info).await?;
    let return_to = query
        .ok()
        .and_then(|Query(query)| query.return_to)
        .as_deref()
        .and_then(|value| RelativeReturnTo::parse(value).ok())
        .unwrap_or_default();
    begin_authorization(
        &state,
        &headers,
        OidcFlowPurpose::Login,
        None,
        return_to,
        true,
    )
    .await
}

/// Same-origin initiation used by the console so failures remain styled API
/// problems instead of becoming raw navigation responses. The GET endpoint is
/// retained for ordinary OAuth top-level navigation compatibility.
#[utoipa::path(
    post,
    path = "/api/v1/oidc/login",
    tag = "oidc",
    request_body = OidcLoginRequest,
    responses(
        (status = 200, description = "Authorization URL for OIDC login", body = OidcAuthorizationResponse),
        (status = 403, description = "Origin check failed", body = Problem),
        (status = 429, description = "OIDC login flow creation is rate limited", body = Problem),
        (status = 404, description = "OIDC is not configured or enabled", body = Problem),
        (status = 503, description = "OIDC dependency unavailable", body = Problem)
    )
)]
pub(super) async fn begin_login_post(
    State(state): State<ManagementState>,
    connect_info: Option<Extension<ConnectInfo<SocketAddr>>>,
    headers: HeaderMap,
    payload: Result<Json<OidcLoginRequest>, JsonRejection>,
) -> Result<Response, Problem> {
    enforce_origin(&state, &headers)?;
    let request = json_payload(payload)?;
    admit_login(&state, &headers, connect_info).await?;
    let return_to = request
        .return_to
        .as_deref()
        .and_then(|value| RelativeReturnTo::parse(value).ok())
        .unwrap_or_default();
    begin_authorization(
        &state,
        &headers,
        OidcFlowPurpose::Login,
        None,
        return_to,
        false,
    )
    .await
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
    State(state): State<ManagementState>,
    headers: HeaderMap,
) -> Result<Response, Problem> {
    let principal = require_mutation_session(&state, &headers).await?;
    let recent_auth_token = cookie(&headers, RECENT_AUTH_COOKIE)?
        .filter(|value| value.len() == 43)
        .ok_or_else(reauthentication_required)?;
    let context = PersistentFlowContext {
        principal: &principal,
        recent_auth_purpose: None,
        recent_auth_resource_id: None,
        recent_auth_token_digest: Some(RecentAuthMaterial::digest_token(recent_auth_token)),
    };
    let mut response = begin_authorization(
        &state,
        &headers,
        OidcFlowPurpose::Link,
        Some(context),
        RelativeReturnTo::default(),
        false,
    )
    .await?;
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
    State(state): State<ManagementState>,
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
        &headers,
        OidcFlowPurpose::Reauthenticate,
        Some(context),
        RelativeReturnTo::default(),
        false,
    )
    .await
}

async fn admit_login(
    state: &ManagementState,
    headers: &HeaderMap,
    connect_info: Option<Extension<ConnectInfo<SocketAddr>>>,
) -> Result<(), Problem> {
    let peer = connect_info.map(|Extension(ConnectInfo(address))| address);
    // Admit before constructing cryptographic material or issuing a redirect.
    // The source digest is keyed and never persisted in its raw network form.
    let source_digest = crate::public_auth_source_digest(state, headers, peer)?;
    let admitted = state
        .store()
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
    Ok(())
}

async fn begin_authorization(
    state: &ManagementState,
    headers: &HeaderMap,
    purpose: OidcFlowPurpose,
    context: Option<PersistentFlowContext<'_>>,
    return_to: RelativeReturnTo,
    redirect: bool,
) -> Result<Response, Problem> {
    // Validate the complete multi-header cookie view before creating durable
    // state. The returned expirations make room for the new flow without
    // allowing a fixed-name cookie to overwrite an unrelated tab.
    let evictions = flow_cookie_evictions(headers)?;
    let configuration = state
        .store()
        .enabled_oidc_configuration()
        .await
        .map_err(map_oidc)?;
    let master_key = require_master_key(state)?;
    let flow_id = OidcFlowId::generate();
    let material = OidcFlowMaterial::generate();
    let callback_state = OidcCallbackState::encode(flow_id, material.state());
    let (flow_cookie_name, flow_cookie_value) = match purpose {
        OidcFlowPurpose::Login => {
            if context.is_some() {
                return Err(Problem::internal());
            }
            let payload = LoginFlowCookiePayload {
                version: LOGIN_FLOW_COOKIE_VERSION,
                flow_id: flow_id.as_uuid(),
                state: material.state().to_owned(),
                nonce: material.nonce().to_owned(),
                pkce_verifier: material.pkce_verifier().to_owned(),
                configuration_id: configuration.id,
                configuration_etag: configuration.etag,
                expires_at_unix: (Utc::now() + FLOW_TTL).timestamp(),
                return_to,
            };
            (
                flow_cookie_name(purpose, flow_id),
                seal_login_flow_cookie(state, master_key, &payload)?,
            )
        }
        OidcFlowPurpose::Link | OidcFlowPurpose::Reauthenticate => {
            let context = context.ok_or_else(Problem::internal)?;
            let payload = Zeroizing::new(
                serde_json::to_vec(&FlowSecretPayload {
                    nonce: material.nonce().to_owned(),
                    pkce_verifier: material.pkce_verifier().to_owned(),
                    configuration_etag: Some(configuration.etag),
                    actor_session_id: Some(context.principal.session_id),
                })
                .map_err(|_| Problem::internal())?,
            );
            let encrypted_payload = master_key
                .seal(&payload, &flow_payload_aad(flow_id.as_uuid()))
                .map_err(|error| {
                    error!(%error, "OIDC authenticated flow encryption failed");
                    Problem::internal()
                })?;
            state
                .store()
                .create_oidc_flow(NewOidcFlow {
                    id: flow_id.as_uuid(),
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
            (
                flow_cookie_name(purpose, flow_id),
                material.browser_binding().to_owned(),
            )
        }
    };

    let callback_uri = callback_url(state)?;
    let scopes = configuration.scopes.join(" ");
    let challenge = material.pkce_challenge();
    let mut authorization_url =
        Url::parse(&configuration.authorization_endpoint).map_err(|_| Problem::internal())?;
    {
        let mut query = authorization_url.query_pairs_mut();
        query
            .append_pair("response_type", "code")
            .append_pair("client_id", &configuration.client_id)
            .append_pair("redirect_uri", &callback_uri)
            .append_pair("scope", &scopes)
            .append_pair("state", &callback_state)
            .append_pair("nonce", material.nonce())
            .append_pair("code_challenge", &challenge)
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
            authorization_url: authorization_url.into(),
        })
        .into_response()
    };
    for eviction in evictions {
        append_cookie(&mut response, eviction);
    }
    append_cookie(&mut response, flow_cookie);
    response
        .headers_mut()
        .insert(header::CACHE_CONTROL, HeaderValue::from_static("no-store"));
    Ok(response)
}
