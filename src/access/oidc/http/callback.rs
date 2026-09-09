use std::fmt;

use crate::access::authentication::RecentAuthPurpose;
use crate::access::authentication::SessionPrincipal;
use crate::access::oidc::repository::types::CompleteOidcLink;
use crate::access::oidc::repository::types::CompleteOidcLogin;
use crate::access::oidc::repository::types::CompleteOidcReauthentication;
use crate::access::oidc::repository::types::OidcConfiguration;
use crate::access::oidc::repository::types::OidcFlowPurpose;
use crate::crypto::aad::oidc_client_secret as client_secret_aad;
use crate::crypto::aad::oidc_flow_payload as flow_payload_aad;
use crate::crypto::envelope::MasterKey;
use crate::crypto::session_material::RecentAuthMaterial;
use crate::crypto::session_material::SessionMaterial;
use crate::database::RequestProvenance;
use axum::extract::OriginalUri;
use axum::extract::Query;
use axum::extract::State;
use axum::extract::rejection::QueryRejection;
use axum::http::HeaderMap;
use axum::http::header;
use axum::response::IntoResponse;
use axum::response::Response;
use jsonwebtoken::jwk::JwkSet;
use serde::Deserialize;
use sqlx::PgPool;
use tracing::error;
use uuid::Uuid;
use zeroize::Zeroize;
use zeroize::Zeroizing;

use crate::access::cookies::validate_session_cookie_ttl;
use crate::access::oidc::http::claims::ValidatedIdentity;
use crate::access::oidc::http::claims::validate_id_token;
use crate::access::oidc::http::configuration::JWKS_LIMIT;
use crate::access::oidc::http::configuration::OidcSecret;
use crate::access::oidc::http::error::flow_stale;
use crate::access::oidc::http::error::invalid_callback;
use crate::access::oidc::http::error::is_authenticated_flow_session_changed;
use crate::access::oidc::http::error::map_oidc;
use crate::access::oidc::http::error::map_oidc_flow_completion;
use crate::access::oidc::http::error::map_token_network;
use crate::access::oidc::http::helpers::callback_url;
use crate::access::oidc::http::helpers::network_policy;
use crate::access::oidc::http::helpers::oauth_form_component;
use crate::access::oidc::http::helpers::require_master_key;
use crate::access::oidc::http::session::CallbackFlow;
use crate::access::oidc::http::session::CallbackSecret;
use crate::access::oidc::http::session::FlowSecretPayload;
use crate::access::oidc::http::session::OidcCallbackState;
use crate::access::oidc::http::session::RECENT_AUTH_TTL;
use crate::access::oidc::http::session::authenticated_redirect;
use crate::access::oidc::http::session::clear_flow_cookie;
use crate::access::oidc::http::session::consume_login_flow_cookie;
use crate::access::oidc::http::session::reauthenticated_redirect;
use crate::access::sessions::require_read_session;
use crate::http::control::provenance::Provenance;
use crate::http::control::state::ManagementState;
use crate::http::problem::Problem;
use crate::http::relative_url::RelativeReturnTo;
use crate::http::request_cookies::RequestCookies;

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
pub(crate) struct CallbackQuery {
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
    path = "/api/v3/oidc/callback",
    tag = "oidc",
    params(
        ("code" = Option<String>, Query, description = "Authorization code"),
        ("state" = Option<String>, Query, description = "One-time flow identifier and state"),
        ("error" = Option<String>, Query, description = "Provider error code")
    ),
    responses(
        (status = 303, description = "OIDC login, identity link, or recent authentication completed"),
        (status = 400, description = "Invalid or rejected callback", body = Problem, content_type = "application/problem+json"),
        (status = 401, description = "ID token validation or initiating session failed", body = Problem, content_type = "application/problem+json"),
        (status = 403, description = "Fresh identity did not match the initiating local account", body = Problem, content_type = "application/problem+json"),
        (status = 409, description = "Explicit link required or identity already linked", body = Problem, content_type = "application/problem+json")
    ),
    security())]
pub(crate) async fn callback(
    State(state): State<ManagementState>,
    Provenance(provenance): Provenance,
    OriginalUri(uri): OriginalUri,
    headers: HeaderMap,
    query: Result<Query<CallbackQuery>, QueryRejection>,
) -> Response {
    let cookies = RequestCookies::parse(&headers);
    let cookies_to_clear = cookies
        .as_ref()
        .map(|cookies| callback_cookie_names(cookies, &query, uri.query()))
        .unwrap_or_default();
    let result = match (cookies, query) {
        (Err(problem), _) => Err(problem),
        (Ok(cookies), Ok(Query(query))) => {
            callback_inner(&state, &provenance, &headers, &cookies, query).await
        }
        // Capture extractor failures so malformed query decoding still reaches
        // this handler and expires any identifiable one-shot cookie.
        (Ok(_), Err(_)) => Err(invalid_callback()),
    };
    let preserve_flow_cookie = result
        .as_ref()
        .err()
        .is_some_and(is_authenticated_flow_session_changed);
    let mut response = match result {
        Ok(response) => response,
        Err(problem) => problem.into_response(),
    };
    if !preserve_flow_cookie {
        for name in cookies_to_clear {
            response
                .headers_mut()
                .append(header::SET_COOKIE, clear_flow_cookie(&name));
        }
    }
    response
}

fn callback_cookie_names(
    cookies: &RequestCookies<'_>,
    query: &Result<Query<CallbackQuery>, QueryRejection>,
    raw_query: Option<&str>,
) -> Vec<String> {
    let mut names = Vec::new();
    if let Ok(Query(query)) = query {
        add_callback_state_cookie_names(&mut names, cookies, query.state.as_deref());
    } else {
        // A duplicate or malformed non-state parameter makes Axum reject the
        // typed query extractor. Recover only state values from the raw query
        // so an identifiable flow is still one-shot without affecting other
        // tabs' scoped cookies.
        for (name, value) in raw_query
            .into_iter()
            .flat_map(|query| url::form_urlencoded::parse(query.as_bytes()))
        {
            if name == "state" {
                add_callback_state_cookie_names(&mut names, cookies, Some(value.as_ref()));
            }
        }
    }
    names
}

fn add_callback_state_cookie_names(
    names: &mut Vec<String>,
    cookies: &RequestCookies<'_>,
    state: Option<&str>,
) {
    let Some(state) = state else {
        return;
    };
    let Ok(state) = OidcCallbackState::parse(state.to_owned()) else {
        return;
    };
    for name in [state.login_cookie_name(), state.authenticated_cookie_name()] {
        if cookies.get(&name).is_some() && !names.iter().any(|existing| existing == &name) {
            names.push(name);
        }
    }
}

async fn callback_inner(
    state: &ManagementState,
    provenance: &RequestProvenance,
    headers: &HeaderMap,
    cookies: &RequestCookies<'_>,
    query: CallbackQuery,
) -> Result<Response, Problem> {
    let callback_state = OidcCallbackState::parse(query.state.ok_or_else(|| {
        Problem::bad_request("oidc_state_missing", "The authorization state is missing.")
    })?)?;
    // Consume state even when the provider rejects the callback.
    let flow = consume_callback_flow(state, headers, cookies, &callback_state).await?;
    if query.error.is_some() {
        return Err(Problem::bad_request(
            "oidc_authorization_rejected",
            "The identity provider did not authorize this request.",
        ));
    }
    let actor = resolve_callback_actor(state, headers, &flow).await?;
    let code = Zeroizing::new(query.code.ok_or_else(|| {
        Problem::bad_request("oidc_code_missing", "The authorization code is missing.")
    })?);
    if code.is_empty() || code.len() > 4096 {
        return Err(invalid_callback());
    }
    let pool = &state.request_boundary.pool;
    let (configuration, master_key) = load_flow_configuration(state, pool, &flow).await?;
    let CallbackFlow {
        purpose,
        recent_auth_purpose,
        recent_auth_resource_id,
        return_to,
        secret: flow_secret,
        ..
    } = flow;
    if flow_secret.nonce.len() != 43 || flow_secret.pkce_verifier.len() != 43 {
        return Err(Problem::bad_request(
            "oidc_login_flow_invalid",
            "The OIDC login flow is invalid or expired.",
        ));
    }
    let token_response = exchange_authorization_code(
        state,
        &configuration,
        master_key,
        &code,
        &flow_secret.pkce_verifier,
    )
    .await?;
    let identity = fetch_validated_identity(
        state,
        &configuration,
        &token_response,
        &flow_secret.nonce,
        purpose,
    )
    .await?;
    drop(token_response);
    drop(flow_secret);
    match purpose {
        OidcFlowPurpose::Login => {
            complete_login(
                provenance,
                state,
                pool,
                &configuration,
                &identity,
                &return_to,
            )
            .await
        }
        OidcFlowPurpose::Link => {
            let actor = actor.ok_or_else(Problem::internal)?;
            complete_link(
                provenance,
                state,
                pool,
                &configuration,
                &identity,
                &actor,
                &return_to,
            )
            .await
        }
        OidcFlowPurpose::Reauthenticate => {
            let actor = actor.ok_or_else(Problem::internal)?;
            complete_reauthentication(
                provenance,
                pool,
                &configuration,
                &identity,
                &actor,
                recent_auth_purpose,
                recent_auth_resource_id,
            )
            .await
        }
    }
}

async fn load_flow_configuration<'a>(
    state: &'a ManagementState,
    pool: &PgPool,
    flow: &CallbackFlow,
) -> Result<(OidcConfiguration, &'a MasterKey), Problem> {
    let configuration =
        crate::access::oidc::repository::configuration::enabled_oidc_configuration(pool)
            .await
            .map_err(map_oidc)?;
    if configuration.id != flow.configuration_id || flow.configuration_etag != configuration.etag {
        return Err(flow_stale());
    }
    let master_key = require_master_key(state)?;
    Ok((configuration, master_key))
}

async fn resolve_callback_actor(
    state: &ManagementState,
    headers: &HeaderMap,
    flow: &CallbackFlow,
) -> Result<Option<SessionPrincipal>, Problem> {
    match flow.purpose {
        OidcFlowPurpose::Login => {
            validate_session_cookie_ttl(state.session_ttl)?;
            Ok(None)
        }
        OidcFlowPurpose::Link => {
            validate_session_cookie_ttl(state.session_ttl)?;
            Ok(Some(require_exact_actor(state, headers, flow).await?))
        }
        OidcFlowPurpose::Reauthenticate => {
            Ok(Some(require_exact_actor(state, headers, flow).await?))
        }
    }
}

async fn exchange_authorization_code(
    state: &ManagementState,
    configuration: &OidcConfiguration,
    master_key: &MasterKey,
    code: &Zeroizing<String>,
    pkce_verifier: &str,
) -> Result<TokenResponse, Problem> {
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
        ("code_verifier".to_owned(), pkce_verifier.to_owned()),
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
    Ok(token_response)
}

async fn fetch_validated_identity(
    state: &ManagementState,
    configuration: &OidcConfiguration,
    token_response: &TokenResponse,
    nonce: &str,
    purpose: OidcFlowPurpose,
) -> Result<ValidatedIdentity, Problem> {
    let jwks: JwkSet = network_policy(state)
        .get_json(&configuration.jwks_uri, JWKS_LIMIT)
        .await
        .map_err(map_token_network)?;
    validate_id_token(
        token_response.id_token.expose(),
        &jwks,
        configuration,
        nonce,
        purpose == OidcFlowPurpose::Reauthenticate,
    )
}

async fn complete_login(
    provenance: &RequestProvenance,
    state: &ManagementState,
    pool: &PgPool,
    configuration: &OidcConfiguration,
    identity: &ValidatedIdentity,
    return_to: &RelativeReturnTo,
) -> Result<Response, Problem> {
    let material = SessionMaterial::generate();
    let mapped_role = if identity.email_verified {
        identity
            .email
            .as_deref()
            .and_then(|email| configuration.mapped_role(email, &identity.groups))
    } else {
        None
    };
    crate::access::oidc::repository::identities::complete_oidc_login(
        pool,
        provenance,
        CompleteOidcLogin {
            configuration_id: configuration.id,
            configuration_etag: configuration.etag,
            issuer: &configuration.issuer,
            subject: &identity.subject,
            email: identity.email.as_deref(),
            display_name: identity.display_name.as_deref(),
            provisioning_role: mapped_role,
            session: &material,
            session_ttl: state.session_ttl,
        },
    )
    .await
    .map_err(map_oidc_flow_completion)?;
    authenticated_redirect(&material, return_to, state.session_ttl)
}

async fn complete_link(
    provenance: &RequestProvenance,
    state: &ManagementState,
    pool: &PgPool,
    configuration: &OidcConfiguration,
    identity: &ValidatedIdentity,
    actor: &SessionPrincipal,
    return_to: &RelativeReturnTo,
) -> Result<Response, Problem> {
    let material = SessionMaterial::generate();
    crate::access::oidc::repository::identities::complete_oidc_link(
        pool,
        provenance,
        CompleteOidcLink {
            user_id: actor.user_id,
            session_id: actor.session_id,
            security_version: actor.security_version,
            configuration_id: configuration.id,
            configuration_etag: configuration.etag,
            issuer: &configuration.issuer,
            subject: &identity.subject,
            email: identity
                .email_verified
                .then_some(identity.email.as_deref())
                .flatten(),
            replacement_session: &material,
            session_ttl: state.session_ttl,
        },
    )
    .await
    .map_err(map_oidc_flow_completion)?;
    authenticated_redirect(&material, return_to, state.session_ttl)
}

async fn complete_reauthentication(
    provenance: &RequestProvenance,
    pool: &PgPool,
    configuration: &OidcConfiguration,
    identity: &ValidatedIdentity,
    actor: &SessionPrincipal,
    recent_auth_purpose: Option<RecentAuthPurpose>,
    recent_auth_resource_id: Option<Uuid>,
) -> Result<Response, Problem> {
    let purpose = recent_auth_purpose.ok_or_else(Problem::internal)?;
    let material = RecentAuthMaterial::generate();
    crate::access::oidc::repository::identities::complete_oidc_reauthentication(
        pool,
        provenance,
        CompleteOidcReauthentication {
            user_id: actor.user_id,
            session_id: actor.session_id,
            security_version: actor.security_version,
            configuration_id: configuration.id,
            configuration_etag: configuration.etag,
            issuer: &configuration.issuer,
            subject: &identity.subject,
            purpose,
            resource_id: recent_auth_resource_id,
            material: &material,
            grant_ttl: RECENT_AUTH_TTL,
        },
    )
    .await
    .map_err(map_oidc_flow_completion)?;
    reauthenticated_redirect(&material, purpose, recent_auth_resource_id)
}

async fn require_exact_actor(
    state: &ManagementState,
    headers: &HeaderMap,
    flow: &CallbackFlow,
) -> Result<SessionPrincipal, Problem> {
    let principal = require_read_session(state, headers).await?;
    if Some(principal.user_id) != flow.actor_user_id
        || Some(principal.session_id) != flow.actor_session_id
        || Some(principal.security_version) != flow.actor_security_version
    {
        return Err(Problem::forbidden(
            "oidc_flow_session_changed",
            "Sign in with the exact session that started this security operation.",
        ));
    }
    Ok(principal)
}

async fn consume_callback_flow(
    state: &ManagementState,
    headers: &HeaderMap,
    cookies: &RequestCookies<'_>,
    callback_state: &OidcCallbackState,
) -> Result<CallbackFlow, Problem> {
    if let Some(flow) = matching_login_callback_flow_from_cookies(state, cookies, callback_state)? {
        let consumption = flow
            .login_consumption
            .as_ref()
            .ok_or_else(Problem::internal)?;
        crate::access::oidc::repository::flows::consume_oidc_login_flow(
            &state.request_boundary.pool,
            consumption.flow_id,
            consumption.expires_at,
        )
        .await
        .map_err(map_oidc)?;
        return Ok(flow);
    }

    let authenticated_cookie_name = callback_state.authenticated_cookie_name();
    let browser_binding = Zeroizing::new(
        cookies
            .get(&authenticated_cookie_name)
            .ok_or_else(|| {
                Problem::bad_request(
                    "oidc_browser_binding_missing",
                    "The OIDC browser binding is missing or expired.",
                )
            })?
            .to_owned(),
    );

    // Persisted authenticated flows must match the protected flow ID and exact
    // current session in one row-locked transaction. A mismatch rejects
    // session B without consuming session A's still-valid flow or cookie.
    let flow_id = callback_state.flow_id();
    let principal = require_read_session(state, headers).await?;
    let pool = &state.request_boundary.pool;
    let flow = crate::access::oidc::repository::flows::consume_oidc_flow(
        pool,
        flow_id.as_uuid(),
        callback_state.secret(),
        &browser_binding,
        principal.session_id,
    )
    .await
    .map_err(map_oidc)?;
    if flow.id != flow_id.as_uuid()
        || !matches!(
            flow.purpose,
            OidcFlowPurpose::Link | OidcFlowPurpose::Reauthenticate
        )
        || flow.actor_user_id != Some(principal.user_id)
        || flow.actor_session_id != Some(principal.session_id)
        || flow.actor_security_version != Some(principal.security_version)
    {
        return Err(Problem::internal());
    }
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
    let configuration_etag = secret.configuration_etag.ok_or_else(flow_stale)?;
    if secret.actor_session_id != Some(principal.session_id) {
        return Err(Problem::internal());
    }
    Ok(CallbackFlow {
        purpose: flow.purpose,
        actor_user_id: flow.actor_user_id,
        actor_session_id: flow.actor_session_id,
        actor_security_version: flow.actor_security_version,
        recent_auth_purpose: flow.recent_auth_purpose,
        recent_auth_resource_id: flow.recent_auth_resource_id,
        configuration_id: flow.configuration_id,
        configuration_etag,
        return_to: Default::default(),
        login_consumption: None,
        secret: CallbackSecret {
            nonce: std::mem::take(&mut secret.nonce),
            pkce_verifier: std::mem::take(&mut secret.pkce_verifier),
        },
    })
}

fn matching_login_callback_flow_from_cookies(
    state: &ManagementState,
    cookies: &RequestCookies<'_>,
    callback_state: &OidcCallbackState,
) -> Result<Option<CallbackFlow>, Problem> {
    let login_cookie_name = callback_state.login_cookie_name();
    let authenticated_cookie_name = callback_state.authenticated_cookie_name();
    let Some(value) = cookies.get(&login_cookie_name) else {
        return Ok(None);
    };
    match consume_login_flow_cookie(state, value, callback_state) {
        Ok(flow) => Ok(Some(flow)),
        Err(problem) if cookies.get(&authenticated_cookie_name).is_none() => Err(problem),
        // An abandoned login can coexist with an authenticated flow. The
        // exact flow ID, state secret, and purpose-specific cookie select it.
        Err(_) => Ok(None),
    }
}

#[cfg(test)]
pub(crate) fn matching_login_callback_flow(
    state: &ManagementState,
    headers: &HeaderMap,
    state_value: &str,
) -> Result<Option<CallbackFlow>, Problem> {
    let cookies = RequestCookies::parse(headers)?;
    let callback_state = OidcCallbackState::parse(state_value.to_owned())?;
    matching_login_callback_flow_from_cookies(state, &cookies, &callback_state)
}
