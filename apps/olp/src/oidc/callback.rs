use std::fmt;

use axum::{
    extract::{Query, State, rejection::QueryRejection},
    http::HeaderMap,
    response::{IntoResponse, Response},
};
use jsonwebtoken::jwk::JwkSet;
use olp_storage::{
    CompleteOidcLink, CompleteOidcLogin, OidcFlowPurpose, SessionMaterial,
    oidc_client_secret_aad as client_secret_aad, oidc_flow_payload_aad as flow_payload_aad,
};
use serde::Deserialize;
use tracing::error;
use zeroize::{Zeroize, Zeroizing};

use super::claims::validate_id_token;
use super::configuration::{JWKS_LIMIT, OidcSecret};
use super::error::{invalid_callback, map_oidc, map_oidc_flow_completion, map_token_network};
use super::helpers::{
    callback_url, cookie, network_policy, oauth_form_component, require_master_key,
};
use super::session::{
    CallbackFlow, CallbackSecret, FLOW_COOKIE, FlowSecretPayload, LOGIN_FLOW_COOKIE, append_cookie,
    authenticated_redirect, clear_flow_cookie, consume_login_flow_cookie,
};
use crate::{
    ApiState, Problem,
    management_api::{require_read_session, require_store},
};

const TOKEN_RESPONSE_LIMIT: usize = 256 * 1024;
const ID_TOKEN_LIMIT: usize = 64 * 1024;

#[derive(Deserialize)]
struct TokenResponse {
    id_token: OidcSecret,
}

impl fmt::Debug for TokenResponse {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str("TokenResponse([REDACTED])")
    }
}

#[derive(Deserialize)]
pub(super) struct CallbackQuery {
    code: Option<String>,
    state: Option<String>,
    error: Option<String>,
}

impl fmt::Debug for CallbackQuery {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("CallbackQuery")
            .field("code", &"[REDACTED]")
            .field("state", &"[REDACTED]")
            .field("error", &self.error)
            .finish()
    }
}

#[utoipa::path(
    get,
    path = "/api/v1/oidc/callback",
    tag = "oidc",
    params(
        ("code" = Option<String>, Query, description = "Authorization code"),
        ("state" = Option<String>, Query, description = "One-time state"),
        ("error" = Option<String>, Query, description = "Provider error code")
    ),
    responses(
        (status = 303, description = "OIDC identity authenticated and local session issued"),
        (status = 400, description = "Invalid or rejected callback", body = Problem),
        (status = 401, description = "ID token validation failed", body = Problem),
        (status = 409, description = "Explicit link required or identity already linked", body = Problem)
    )
)]
pub(super) async fn callback(
    State(state): State<ApiState>,
    headers: HeaderMap,
    query: Result<Query<CallbackQuery>, QueryRejection>,
) -> Response {
    // A callback is a one-shot browser operation. Clear either flavour of
    // flow cookie even when parsing, decryption, IdP exchange, or completion
    // fails so a stale or tampered browser value cannot be retried forever.
    let had_login_cookie = cookie(&headers, LOGIN_FLOW_COOKIE).is_some();
    let had_legacy_cookie = cookie(&headers, FLOW_COOKIE).is_some();
    let result = match query {
        Ok(Query(query)) => callback_inner(&state, &headers, query).await,
        // Capture extractor failures so malformed query decoding still reaches
        // this handler and expires the one-shot browser flow cookie.
        Err(_) => Err(invalid_callback()),
    };
    let mut response = match result {
        Ok(response) => response,
        Err(problem) => problem.into_response(),
    };
    if had_login_cookie {
        append_cookie(&mut response, clear_flow_cookie(LOGIN_FLOW_COOKIE));
    }
    if had_legacy_cookie {
        append_cookie(&mut response, clear_flow_cookie(FLOW_COOKIE));
    }
    response
}

async fn callback_inner(
    state: &ApiState,
    headers: &HeaderMap,
    query: CallbackQuery,
) -> Result<Response, Problem> {
    let state_value = Zeroizing::new(query.state.ok_or_else(|| {
        Problem::bad_request("oidc_state_missing", "The authorization state is missing.")
    })?);
    if state_value.len() != 43 {
        return Err(invalid_callback());
    }
    // Claim valid state before inspecting the provider result or exchanging a
    // code. A rejected, malformed, or failed callback is still an attempt and
    // cannot leave reusable PKCE material behind for a later replay.
    let flow = consume_callback_flow(state, headers, &state_value).await?;
    if query.error.is_some() {
        return Err(Problem::bad_request(
            "oidc_authorization_rejected",
            "The identity provider did not authorize this request.",
        ));
    }
    let code = Zeroizing::new(query.code.ok_or_else(|| {
        Problem::bad_request("oidc_code_missing", "The authorization code is missing.")
    })?);
    if code.is_empty() || code.len() > 4096 {
        return Err(invalid_callback());
    }
    let store = require_store(state)?;
    let configuration = store.enabled_oidc_configuration().await.map_err(map_oidc)?;
    if configuration.id != flow.configuration_id {
        return Err(Problem::bad_request(
            "oidc_flow_stale",
            "The OIDC configuration changed. Start authorization again.",
        ));
    }
    let master_key = require_master_key(state)?;
    let flow_secret = flow.secret;
    if flow_secret.nonce.len() != 43 || flow_secret.pkce_verifier.len() != 43 {
        return Err(Problem::bad_request(
            "oidc_login_flow_invalid",
            "The OIDC login flow is invalid or expired.",
        ));
    }
    if flow.configuration_etag != configuration.etag {
        return Err(Problem::bad_request(
            "oidc_flow_stale",
            "The OIDC configuration changed. Start authorization again.",
        ));
    }
    let redirect_uri = callback_url(state)?;
    let client_secret_bytes = master_key
        .open(
            &configuration.encrypted_client_secret,
            &client_secret_aad(configuration.id),
        )
        .map_err(|error| {
            error!(%error, "OIDC client secret decryption failed");
            Problem::internal()
        })?;
    let client_secret = Zeroizing::new(
        String::from_utf8(client_secret_bytes.to_vec()).map_err(|_| Problem::internal())?,
    );
    let mut form = vec![
        ("grant_type".to_owned(), "authorization_code".to_owned()),
        ("code".to_owned(), code.to_string()),
        ("redirect_uri".to_owned(), redirect_uri),
        ("client_id".to_owned(), configuration.client_id.clone()),
        (
            "code_verifier".to_owned(),
            flow_secret.pkce_verifier.clone(),
        ),
    ];
    let basic_credentials = if configuration.token_endpoint_auth_method == "client_secret_basic" {
        Some((
            oauth_form_component(&configuration.client_id),
            Zeroizing::new(oauth_form_component(&client_secret)),
        ))
    } else {
        form.push(("client_secret".to_owned(), client_secret.to_string()));
        None
    };
    let basic_auth = basic_credentials
        .as_ref()
        .map(|(client_id, secret)| (client_id.as_str(), secret.as_str()));
    let token_result = network_policy(state)
        .post_form_json(
            &configuration.token_endpoint,
            &form,
            basic_auth,
            TOKEN_RESPONSE_LIMIT,
        )
        .await;
    form.iter_mut().for_each(|(_, value)| value.zeroize());
    let token_response: TokenResponse = token_result.map_err(map_token_network)?;
    if token_response.id_token.expose().len() > ID_TOKEN_LIMIT {
        return Err(Problem::unauthorized("The ID token is invalid."));
    }
    let jwks: JwkSet = network_policy(state)
        .get_json(&configuration.jwks_uri, JWKS_LIMIT)
        .await
        .map_err(map_token_network)?;
    let identity = validate_id_token(
        token_response.id_token.expose(),
        &jwks,
        &configuration,
        &flow_secret.nonce,
    )?;
    drop(token_response);
    drop(flow_secret);

    let material = SessionMaterial::generate();
    match flow.purpose {
        OidcFlowPurpose::Login => {
            let mapped_role = if identity.email_verified {
                identity
                    .email
                    .as_deref()
                    .and_then(|email| configuration.mapped_role(email, &identity.groups))
            } else {
                None
            };
            store
                .complete_oidc_login(CompleteOidcLogin {
                    configuration_id: configuration.id,
                    configuration_etag: configuration.etag,
                    issuer: &configuration.issuer,
                    subject: &identity.subject,
                    email: identity.email.as_deref(),
                    display_name: identity.display_name.as_deref(),
                    provisioning_role: mapped_role,
                    session: &material,
                    session_ttl: state.session_ttl,
                })
                .await
                .map_err(map_oidc_flow_completion)?;
        }
        OidcFlowPurpose::Link => {
            let actor_user_id = flow.actor_user_id.ok_or_else(Problem::internal)?;
            // Linking never correlates on email. It requires the same active
            // local session that explicitly initiated this flow.
            let principal = require_read_session(state, headers).await?;
            if principal.user_id != actor_user_id {
                return Err(Problem::forbidden(
                    "oidc_link_session_changed",
                    "Sign in with the local account that started this link operation.",
                ));
            }
            store
                .complete_oidc_link(CompleteOidcLink {
                    user_id: actor_user_id,
                    configuration_id: configuration.id,
                    configuration_etag: configuration.etag,
                    issuer: &configuration.issuer,
                    subject: &identity.subject,
                    email: identity
                        .email_verified
                        .then_some(identity.email.as_deref())
                        .flatten(),
                    session: &material,
                    session_ttl: state.session_ttl,
                })
                .await
                .map_err(map_oidc_flow_completion)?;
        }
    }
    authenticated_redirect(&material)
}

async fn consume_callback_flow(
    state: &ApiState,
    headers: &HeaderMap,
    state_value: &str,
) -> Result<CallbackFlow, Problem> {
    if let Some(flow) = matching_login_callback_flow(state, headers, state_value)? {
        let consumption = flow
            .login_consumption
            .as_ref()
            .ok_or_else(Problem::internal)?;
        require_store(state)?
            .consume_oidc_login_flow(consumption.flow_id, consumption.expires_at)
            .await
            .map_err(map_oidc)?;
        return Ok(flow);
    }

    // New releases never create persisted login flows, but retained rows can
    // complete during their existing ten-minute lifetime. This also preserves
    // authenticated identity-link flows, which intentionally stay durable.
    let browser_binding = Zeroizing::new(
        cookie(headers, FLOW_COOKIE)
            .ok_or_else(|| {
                Problem::bad_request(
                    "oidc_browser_binding_missing",
                    "The OIDC browser binding is missing or expired.",
                )
            })?
            .to_owned(),
    );
    let store = require_store(state)?;
    let flow = store
        .consume_oidc_flow(state_value, &browser_binding)
        .await
        .map_err(map_oidc)?;
    let master_key = require_master_key(state)?;
    let decrypted = master_key
        .open(&flow.encrypted_payload, &flow_payload_aad(flow.id))
        .map_err(|error| {
            error!(%error, "OIDC persisted flow payload decryption failed");
            Problem::internal()
        })?;
    let mut secret: FlowSecretPayload = serde_json::from_slice(&decrypted).map_err(|_| {
        error!("OIDC persisted flow payload is malformed");
        Problem::internal()
    })?;
    let configuration_etag = secret.configuration_etag.ok_or_else(|| {
        // A row written by a sufficiently old release has no configuration
        // fence and is unsafe to complete after this hardening release.
        Problem::bad_request(
            "oidc_flow_stale",
            "The OIDC configuration changed. Start authorization again.",
        )
    })?;
    Ok(CallbackFlow {
        purpose: flow.purpose,
        actor_user_id: flow.actor_user_id,
        configuration_id: flow.configuration_id,
        configuration_etag,
        login_consumption: None,
        secret: CallbackSecret {
            nonce: std::mem::take(&mut secret.nonce),
            pkce_verifier: std::mem::take(&mut secret.pkce_verifier),
        },
    })
}

pub(super) fn matching_login_callback_flow(
    state: &ApiState,
    headers: &HeaderMap,
    state_value: &str,
) -> Result<Option<CallbackFlow>, Problem> {
    let Some(value) = cookie(headers, LOGIN_FLOW_COOKIE) else {
        return Ok(None);
    };
    match consume_login_flow_cookie(state, value, state_value) {
        Ok(flow) => Ok(Some(flow)),
        Err(problem) if cookie(headers, FLOW_COOKIE).is_none() => Err(problem),
        // An abandoned login can leave its stateless cookie alongside a newer
        // persisted link flow. Let callback state plus browser binding select
        // the persisted flow instead of rejecting the valid link.
        Err(_) => Ok(None),
    }
}
