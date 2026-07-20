use std::net::SocketAddr;

use axum::{
    Json,
    extract::{ConnectInfo, Extension, State},
    http::{HeaderMap, HeaderValue, StatusCode, header},
    response::{IntoResponse, Response},
};
use chrono::Utc;
use olp_storage::{
    NewOidcFlow, OidcFlowMaterial, OidcFlowPurpose, oidc_flow_payload_aad as flow_payload_aad,
};
use serde::Serialize;
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
    management_api::{require_mutation_session, require_store},
};

#[derive(Debug, Serialize, ToSchema)]
pub struct OidcAuthorizationResponse {
    pub authorization_url: String,
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
        OidcFlowPurpose::Link,
        Some(principal.user_id),
        false,
    )
    .await
}

async fn begin_authorization(
    state: &ApiState,
    purpose: OidcFlowPurpose,
    actor_user_id: Option<Uuid>,
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
            if actor_user_id.is_some() {
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
        OidcFlowPurpose::Link => {
            let actor_user_id = actor_user_id.ok_or_else(Problem::internal)?;
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
                    error!(%error, "OIDC link flow encryption failed");
                    Problem::internal()
                })?;
            require_store(state)?
                .create_oidc_flow(NewOidcFlow {
                    id: flow_id,
                    configuration_id: configuration.id,
                    configuration_etag: configuration.etag,
                    purpose,
                    actor_user_id: Some(actor_user_id),
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
    authorization_url.query_pairs_mut().extend_pairs([
        ("response_type", "code"),
        ("client_id", configuration.client_id.as_str()),
        ("redirect_uri", callback_url(state)?.as_str()),
        ("scope", configuration.scopes.join(" ").as_str()),
        ("state", material.state()),
        ("nonce", material.nonce()),
        ("code_challenge", material.pkce_challenge().as_str()),
        ("code_challenge_method", "S256"),
    ]);
    let flow_cookie = format!(
        "{flow_cookie_name}={flow_cookie_value}; Path=/; Max-Age=600; Secure; HttpOnly; SameSite=Lax"
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
    response.headers_mut().append(
        header::SET_COOKIE,
        HeaderValue::from_str(&flow_cookie).map_err(|_| Problem::internal())?,
    );
    response
        .headers_mut()
        .insert(header::CACHE_CONTROL, HeaderValue::from_static("no-store"));
    Ok(response)
}
