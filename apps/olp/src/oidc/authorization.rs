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
    NewOidcFlow, OidcFlowMaterial, OidcFlowPurpose, oidc_flow_payload_aad as flow_payload_aad,
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
    ApiState, Problem, RelativeReturnTo,
    management_api::{enforce_origin, json_payload, require_mutation_session, require_store},
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

#[derive(Clone, Copy)]
struct LinkActor {
    user_id: Uuid,
    session_id: Uuid,
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
    State(state): State<ApiState>,
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
    State(state): State<ApiState>,
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
        (status = 429, description = "OIDC authorization flow creation is rate limited", body = Problem)
    )
)]
pub(super) async fn begin_link(
    State(state): State<ApiState>,
    headers: HeaderMap,
) -> Result<Response, Problem> {
    let principal = require_mutation_session(&state, &headers).await?;
    begin_authorization(
        &state,
        &headers,
        OidcFlowPurpose::Link,
        Some(LinkActor {
            user_id: principal.user_id,
            session_id: principal.session_id,
        }),
        RelativeReturnTo::default(),
        false,
    )
    .await
}

async fn admit_login(
    state: &ApiState,
    headers: &HeaderMap,
    connect_info: Option<Extension<ConnectInfo<SocketAddr>>>,
) -> Result<(), Problem> {
    let peer = connect_info.map(|Extension(ConnectInfo(address))| address);
    // Admit before constructing cryptographic material or issuing a redirect.
    // The source digest is keyed and never persisted in its raw network form.
    let source_digest = crate::public_auth_source_digest(state, headers, peer)?;
    let admitted = require_store(state)?
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
    state: &ApiState,
    headers: &HeaderMap,
    purpose: OidcFlowPurpose,
    actor: Option<LinkActor>,
    return_to: RelativeReturnTo,
    redirect: bool,
) -> Result<Response, Problem> {
    // Validate the complete multi-header cookie view before creating durable
    // state. The returned expirations make room for the new flow without
    // allowing a fixed-name cookie to overwrite an unrelated tab.
    let evictions = flow_cookie_evictions(headers)?;
    let configuration = require_store(state)?
        .enabled_oidc_configuration()
        .await
        .map_err(map_oidc)?;
    let master_key = require_master_key(state)?;
    let flow_id = OidcFlowId::generate();
    let material = OidcFlowMaterial::generate();
    let callback_state = OidcCallbackState::encode(flow_id, material.state());
    let (flow_cookie_name, flow_cookie_value) = match purpose {
        OidcFlowPurpose::Login => {
            if actor.is_some() {
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
        OidcFlowPurpose::Link => {
            let actor = actor.ok_or_else(Problem::internal)?;
            let payload = Zeroizing::new(
                serde_json::to_vec(&FlowSecretPayload {
                    nonce: material.nonce().to_owned(),
                    pkce_verifier: material.pkce_verifier().to_owned(),
                    configuration_etag: Some(configuration.etag),
                    actor_session_id: Some(actor.session_id),
                })
                .map_err(|_| Problem::internal())?,
            );
            let encrypted_payload = master_key
                .seal(&payload, &flow_payload_aad(flow_id.as_uuid()))
                .map_err(|error| {
                    error!(%error, "OIDC link flow encryption failed");
                    Problem::internal()
                })?;
            require_store(state)?
                .create_oidc_flow(NewOidcFlow {
                    id: flow_id.as_uuid(),
                    configuration_id: configuration.id,
                    configuration_etag: configuration.etag,
                    purpose,
                    actor_user_id: Some(actor.user_id),
                    actor_session_id: Some(actor.session_id),
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
    authorization_url.query_pairs_mut().extend_pairs([
        ("response_type", "code"),
        ("client_id", configuration.client_id.as_str()),
        ("redirect_uri", callback_uri.as_str()),
        ("scope", scopes.as_str()),
        ("state", callback_state.as_str()),
        ("nonce", material.nonce()),
        ("code_challenge", challenge.as_str()),
        ("code_challenge_method", "S256"),
    ]);
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
