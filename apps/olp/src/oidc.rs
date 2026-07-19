use std::{collections::BTreeSet, fmt, net::SocketAddr};

use axum::{
    Json, Router,
    extract::{
        ConnectInfo, Extension, Path, Query, State,
        rejection::{JsonRejection, QueryRejection},
    },
    http::{HeaderMap, HeaderValue, StatusCode, header},
    response::{IntoResponse, Response},
    routing::{get, post},
};
use base64::{Engine as _, engine::general_purpose::URL_SAFE_NO_PAD};
use chrono::{DateTime, Duration, Utc};
use jsonwebtoken::{
    Algorithm, DecodingKey, Validation, decode, decode_header,
    jwk::{Jwk, JwkSet, KeyOperations, PublicKeyUse},
};
use olp_domain::Role;
use olp_providers::{OidcNetworkError, OidcNetworkPolicy};
use olp_storage::{
    CompleteOidcLink, CompleteOidcLogin, EncryptedSecret, MasterKey, NewOidcFlow,
    OidcConfiguration, OidcError, OidcFlowMaterial, OidcFlowPurpose, OidcIdentityRecord,
    OidcRoleMapping, SessionMaterial, UpsertOidcConfiguration, constant_time_eq,
    oidc_client_secret_aad as client_secret_aad, oidc_flow_payload_aad as flow_payload_aad,
};
use serde::{Deserialize, Serialize};
use serde_json::Value;
use tracing::{error, warn};
use url::Url;
use utoipa::{OpenApi, ToSchema};
use uuid::Uuid;
use zeroize::{Zeroize, Zeroizing};

use crate::{
    ApiState, FieldErrors, Problem,
    management::{
        Permission, json_payload, require_mutation_session, require_permission,
        require_read_session, require_store,
    },
};

const SESSION_COOKIE: &str = "__Host-olp_session";
const CSRF_COOKIE: &str = "__Host-olp_csrf";
/// Legacy persisted-flow cookie. New login flows deliberately do not use it;
/// it remains solely for authenticated link flows and login redirects created
/// by a pre-stateless-flow release.
const FLOW_COOKIE: &str = "__Host-olp_oidc_flow";
const LOGIN_FLOW_COOKIE: &str = "__Host-olp_oidc_login_flow";
const LOGIN_FLOW_COOKIE_VERSION: u8 = 2;
const FLOW_TTL: Duration = Duration::minutes(10);
const LOGIN_FLOW_COOKIE_MAX_BYTES: usize = 4 * 1024;
const DISCOVERY_LIMIT: usize = 128 * 1024;
const JWKS_LIMIT: usize = 512 * 1024;
const TOKEN_RESPONSE_LIMIT: usize = 256 * 1024;
const ID_TOKEN_LIMIT: usize = 64 * 1024;

pub(crate) fn router() -> Router<ApiState> {
    Router::new()
        .route(
            "/api/v1/oidc/configuration",
            get(get_configuration).put(put_configuration),
        )
        .route("/api/v1/oidc/login", get(begin_login))
        .route("/api/v1/oidc/link", post(begin_link))
        .route("/api/v1/oidc/identities", get(list_identities))
        .route(
            "/api/v1/oidc/identities/{identity_id}",
            axum::routing::delete(unlink_identity),
        )
        .route("/api/v1/oidc/callback", get(callback))
}

#[derive(OpenApi)]
#[openapi(
    paths(
        get_configuration,
        put_configuration,
        begin_login,
        begin_link,
        list_identities,
        unlink_identity,
        callback
    ),
    components(schemas(
        OidcConfigurationRequest,
        OidcConfigurationResponse,
        OidcRoleMappingRequest,
        OidcRoleMappingResponse,
        OidcAuthorizationResponse,
        OidcIdentityResponse,
        OidcIdentityListResponse,
        Problem
    )),
    tags((name = "oidc"))
)]
pub(crate) struct OidcApiDoc;

pub(crate) fn openapi() -> utoipa::openapi::OpenApi {
    OidcApiDoc::openapi()
}

#[derive(Deserialize, ToSchema)]
pub struct OidcConfigurationRequest {
    pub discovery_url: String,
    /// Issuer identifier configured out-of-band with the identity provider.
    /// Discovery must return this exact value.
    pub issuer: String,
    pub client_id: String,
    #[schema(value_type = Option<String>, write_only)]
    #[serde(default)]
    client_secret: Option<OidcSecret>,
    #[serde(default = "default_true")]
    pub enabled: bool,
    #[serde(default = "default_scopes")]
    pub scopes: Vec<String>,
    #[serde(default = "default_email_claim")]
    pub email_claim: String,
    #[serde(default = "default_groups_claim")]
    pub groups_claim: String,
    pub default_role: Option<String>,
    #[serde(default)]
    pub email_role_mappings: Vec<OidcRoleMappingRequest>,
    #[serde(default)]
    pub group_role_mappings: Vec<OidcRoleMappingRequest>,
}

impl fmt::Debug for OidcConfigurationRequest {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("OidcConfigurationRequest")
            .field("discovery_url", &self.discovery_url)
            .field("issuer", &self.issuer)
            .field("client_id", &self.client_id)
            .field("client_secret", &"[REDACTED]")
            .field("enabled", &self.enabled)
            .field("scopes", &self.scopes)
            .field("email_claim", &self.email_claim)
            .field("groups_claim", &self.groups_claim)
            .field("default_role", &self.default_role)
            .field("email_role_mappings", &self.email_role_mappings)
            .field("group_role_mappings", &self.group_role_mappings)
            .finish()
    }
}

#[derive(Debug, Clone, Deserialize, ToSchema)]
pub struct OidcRoleMappingRequest {
    pub claim_value: String,
    pub role: String,
}

#[derive(Debug, Serialize, ToSchema)]
pub struct OidcConfigurationResponse {
    #[schema(value_type = String, format = Uuid)]
    pub id: Uuid,
    pub discovery_url: String,
    pub issuer: String,
    pub client_id: String,
    pub has_client_secret: bool,
    pub enabled: bool,
    pub scopes: Vec<String>,
    pub email_claim: String,
    pub groups_claim: String,
    pub default_role: Option<String>,
    pub email_role_mappings: Vec<OidcRoleMappingResponse>,
    pub group_role_mappings: Vec<OidcRoleMappingResponse>,
    #[schema(value_type = String, format = Uuid)]
    pub etag: Uuid,
}

#[derive(Debug, Serialize, ToSchema)]
pub struct OidcRoleMappingResponse {
    pub claim_value: String,
    pub role: String,
}

#[derive(Debug, Serialize, ToSchema)]
pub struct OidcAuthorizationResponse {
    pub authorization_url: String,
}

#[derive(Clone, Debug, Serialize, ToSchema)]
pub struct OidcIdentityResponse {
    pub id: Uuid,
    pub issuer: String,
    pub email_at_link: Option<String>,
    pub last_login_at: Option<chrono::DateTime<Utc>>,
    pub created_at: chrono::DateTime<Utc>,
    pub can_unlink: bool,
}

impl From<OidcIdentityRecord> for OidcIdentityResponse {
    fn from(identity: OidcIdentityRecord) -> Self {
        Self {
            id: identity.id,
            issuer: identity.issuer,
            email_at_link: identity.email_at_link,
            last_login_at: identity.last_login_at,
            created_at: identity.created_at,
            can_unlink: identity.can_unlink,
        }
    }
}

#[derive(Clone, Debug, Serialize, ToSchema)]
pub struct OidcIdentityListResponse {
    pub data: Vec<OidcIdentityResponse>,
    pub linking_available: bool,
}

struct OidcSecret(Zeroizing<String>);

impl OidcSecret {
    fn expose(&self) -> &str {
        &self.0
    }
}

impl<'de> Deserialize<'de> for OidcSecret {
    fn deserialize<D>(deserializer: D) -> Result<Self, D::Error>
    where
        D: serde::Deserializer<'de>,
    {
        String::deserialize(deserializer)
            .map(Zeroizing::new)
            .map(Self)
    }
}

impl fmt::Debug for OidcSecret {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str("OidcSecret([REDACTED])")
    }
}

#[derive(Debug, Deserialize)]
struct DiscoveryDocument {
    issuer: String,
    authorization_endpoint: String,
    token_endpoint: String,
    jwks_uri: String,
    #[serde(default)]
    response_types_supported: Vec<String>,
    #[serde(default)]
    code_challenge_methods_supported: Vec<String>,
    #[serde(default)]
    token_endpoint_auth_methods_supported: Vec<String>,
    #[serde(default)]
    id_token_signing_alg_values_supported: Vec<String>,
}

#[derive(Serialize, Deserialize)]
struct FlowSecretPayload {
    nonce: String,
    pkce_verifier: String,
    #[serde(default)]
    configuration_etag: Option<Uuid>,
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
struct LoginFlowCookiePayload {
    version: u8,
    flow_id: Uuid,
    state: String,
    nonce: String,
    pkce_verifier: String,
    configuration_id: Uuid,
    configuration_etag: Uuid,
    expires_at_unix: i64,
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

struct CallbackFlow {
    purpose: OidcFlowPurpose,
    actor_user_id: Option<Uuid>,
    configuration_id: Uuid,
    configuration_etag: Uuid,
    login_consumption: Option<LoginFlowConsumption>,
    secret: CallbackSecret,
}

struct CallbackSecret {
    nonce: String,
    pkce_verifier: String,
}

impl Drop for CallbackSecret {
    fn drop(&mut self) {
        self.nonce.zeroize();
        self.pkce_verifier.zeroize();
    }
}

struct LoginFlowConsumption {
    flow_id: Uuid,
    expires_at: DateTime<Utc>,
}

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
struct CallbackQuery {
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

#[derive(Debug)]
struct ValidatedIdentity {
    subject: String,
    email: Option<String>,
    email_verified: bool,
    display_name: Option<String>,
    groups: Vec<String>,
}

#[utoipa::path(
    get,
    path = "/api/v1/oidc/configuration",
    tag = "oidc",
    responses(
        (status = 200, description = "Redacted single-provider OIDC configuration", body = OidcConfigurationResponse),
        (status = 401, description = "No active session", body = Problem),
        (status = 403, description = "Only owners can manage OIDC", body = Problem),
        (status = 404, description = "OIDC is not configured", body = Problem)
    )
)]
async fn get_configuration(
    State(state): State<ApiState>,
    headers: HeaderMap,
) -> Result<Response, Problem> {
    let principal = require_read_session(&state, &headers).await?;
    require_permission(&principal, Permission::ManageTeam)?;
    let configuration = require_store(&state)?
        .oidc_configuration()
        .await
        .map_err(map_oidc)?
        .ok_or_else(oidc_not_configured)?;
    configuration_response(configuration)
}

#[utoipa::path(
    get,
    path = "/api/v1/oidc/identities",
    tag = "oidc",
    responses(
        (status = 200, description = "OIDC identities linked to the current account", body = OidcIdentityListResponse),
        (status = 401, description = "No active session", body = Problem)
    )
)]
async fn list_identities(
    State(state): State<ApiState>,
    headers: HeaderMap,
) -> Result<Json<OidcIdentityListResponse>, Problem> {
    let principal = require_read_session(&state, &headers).await?;
    let store = require_store(&state)?;
    let identities = store
        .oidc_identities_for_user(principal.user_id)
        .await
        .map_err(map_oidc)?;
    let linking_available = store
        .oidc_configuration()
        .await
        .map_err(map_oidc)?
        .is_some_and(|configuration| configuration.enabled);
    Ok(Json(OidcIdentityListResponse {
        data: identities.into_iter().map(Into::into).collect(),
        linking_available,
    }))
}

#[utoipa::path(
    delete,
    path = "/api/v1/oidc/identities/{identity_id}",
    tag = "oidc",
    params(("identity_id" = Uuid, Path, description = "Linked identity ID")),
    responses(
        (status = 204, description = "Identity unlinked"),
        (status = 404, description = "Identity not linked to this account", body = Problem),
        (status = 409, description = "Unlink would remove the final authentication method", body = Problem)
    )
)]
async fn unlink_identity(
    State(state): State<ApiState>,
    Path(identity_id): Path<Uuid>,
    headers: HeaderMap,
) -> Result<StatusCode, Problem> {
    let principal = require_mutation_session(&state, &headers).await?;
    require_store(&state)?
        .unlink_oidc_identity(principal.user_id, identity_id)
        .await
        .map_err(map_oidc)?;
    Ok(StatusCode::NO_CONTENT)
}

#[utoipa::path(
    put,
    path = "/api/v1/oidc/configuration",
    tag = "oidc",
    request_body = OidcConfigurationRequest,
    params(("If-Match" = Option<String>, Header, description = "Required UUID ETag when updating")),
    responses(
        (status = 200, description = "OIDC configuration updated", body = OidcConfigurationResponse),
        (status = 201, description = "OIDC configuration created", body = OidcConfigurationResponse),
        (status = 412, description = "ETag mismatch", body = Problem),
        (status = 422, description = "Discovery or configuration validation failed", body = Problem)
    )
)]
async fn put_configuration(
    State(state): State<ApiState>,
    headers: HeaderMap,
    payload: Result<Json<OidcConfigurationRequest>, JsonRejection>,
) -> Result<Response, Problem> {
    let principal = require_mutation_session(&state, &headers).await?;
    require_permission(&principal, Permission::ManageTeam)?;
    let request = json_payload(payload)?;
    validate_configuration_request(&request)?;
    let store = require_store(&state)?;
    let existing = store.oidc_configuration().await.map_err(map_oidc)?;
    let expected_etag = optional_if_match(&headers)?;
    if existing.is_some() && expected_etag.is_none() {
        return Err(map_oidc(OidcError::PreconditionRequired));
    }
    let id = existing
        .as_ref()
        .map_or_else(Uuid::now_v7, |configuration| configuration.id);
    let master_key = require_master_key(&state)?;
    let encrypted_client_secret = match request.client_secret.as_ref() {
        Some(secret) => {
            if secret.expose().is_empty() || secret.expose().len() > 4096 {
                return Err(field_problem(
                    "client_secret",
                    "Use a client secret between 1 and 4,096 bytes.",
                ));
            }
            master_key
                .seal(secret.expose().as_bytes(), &client_secret_aad(id))
                .map_err(|error| {
                    error!(%error, "OIDC client secret encryption failed");
                    Problem::internal()
                })?
        }
        None => {
            let existing = existing.as_ref().ok_or_else(|| {
                field_problem(
                    "client_secret",
                    "A client secret is required when OIDC is first configured.",
                )
            })?;
            if existing.encrypted_client_secret.key_version != master_key.version() {
                return Err(field_problem(
                    "client_secret",
                    "Re-enter the client secret to rotate it to the active master key.",
                ));
            }
            existing.encrypted_client_secret.clone()
        }
    };

    let policy = network_policy(&state);
    validate_issuer(request.issuer.trim(), policy.allow_insecure_test_endpoints)?;
    let discovery: DiscoveryDocument = policy
        .get_json(request.discovery_url.trim(), DISCOVERY_LIMIT)
        .await
        .map_err(map_discovery_network)?;
    validate_discovery(&policy, &discovery).await?;
    if discovery.issuer != request.issuer.trim() {
        return Err(field_problem(
            "issuer",
            "The discovery document issuer does not match the configured issuer.",
        ));
    }
    let jwks: JwkSet = policy
        .get_json(&discovery.jwks_uri, JWKS_LIMIT)
        .await
        .map_err(map_discovery_network)?;
    validate_jwks(&jwks)?;
    let token_endpoint_auth_method = choose_token_auth_method(&discovery)?;
    let scopes = normalized_scopes(&request.scopes)?;
    let default_role = request
        .default_role
        .as_deref()
        .map(parse_role)
        .transpose()?;
    let email_role_mappings = request
        .email_role_mappings
        .iter()
        .map(parse_mapping)
        .collect::<Result<Vec<_>, _>>()?;
    let group_role_mappings = request
        .group_role_mappings
        .iter()
        .map(parse_mapping)
        .collect::<Result<Vec<_>, _>>()?;
    let created = existing.is_none();
    let configuration = store
        .upsert_oidc_configuration(UpsertOidcConfiguration {
            id,
            discovery_url: request.discovery_url.trim().to_owned(),
            issuer: request.issuer.trim().to_owned(),
            authorization_endpoint: discovery.authorization_endpoint,
            token_endpoint: discovery.token_endpoint,
            jwks_uri: discovery.jwks_uri,
            token_endpoint_auth_method,
            client_id: request.client_id.trim().to_owned(),
            encrypted_client_secret,
            scopes,
            email_claim: request.email_claim,
            groups_claim: request.groups_claim,
            default_role,
            email_role_mappings,
            group_role_mappings,
            enabled: request.enabled,
            actor_user_id: principal.user_id,
            expected_etag,
        })
        .await
        .map_err(map_oidc)?;
    let mut response = configuration_response(configuration)?;
    *response.status_mut() = if created {
        StatusCode::CREATED
    } else {
        StatusCode::OK
    };
    Ok(response)
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
async fn begin_login(
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
async fn begin_link(
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
async fn callback(
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

fn matching_login_callback_flow(
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

fn validate_id_token(
    id_token: &str,
    jwks: &JwkSet,
    configuration: &OidcConfiguration,
    expected_nonce: &str,
) -> Result<ValidatedIdentity, Problem> {
    let header = decode_header(id_token).map_err(|_| invalid_id_token())?;
    if matches!(
        header.alg,
        Algorithm::HS256 | Algorithm::HS384 | Algorithm::HS512
    ) {
        return Err(invalid_id_token());
    }
    let kid = header.kid.as_deref().ok_or_else(invalid_id_token)?;
    let matching_keys = jwks
        .keys
        .iter()
        .filter(|key| key.common.key_id.as_deref() == Some(kid))
        .collect::<Vec<_>>();
    if matching_keys.len() != 1 {
        return Err(invalid_id_token());
    }
    let jwk = matching_keys[0];
    validate_jwk_for_signature(jwk, header.alg)?;
    let key = DecodingKey::from_jwk(jwk).map_err(|_| invalid_id_token())?;
    let mut validation = Validation::new(header.alg);
    validation.set_required_spec_claims(&["exp", "iss", "aud", "sub"]);
    validation.set_issuer(&[configuration.issuer.as_str()]);
    validation.set_audience(&[configuration.client_id.as_str()]);
    validation.validate_nbf = true;
    validation.leeway = 60;
    let claims = decode::<Value>(id_token, &key, &validation)
        .map_err(|_| invalid_id_token())?
        .claims;
    let issued_at = claims
        .get("iat")
        .and_then(Value::as_i64)
        .ok_or_else(invalid_id_token)?;
    let expires_at = claims
        .get("exp")
        .and_then(Value::as_i64)
        .ok_or_else(invalid_id_token)?;
    let now = Utc::now().timestamp();
    if issued_at > now + 60 || issued_at < now - FLOW_TTL.num_seconds() || expires_at <= issued_at {
        return Err(invalid_id_token());
    }
    let nonce = claims
        .get("nonce")
        .and_then(Value::as_str)
        .ok_or_else(invalid_id_token)?;
    let expected_nonce_digest = SessionMaterial::digest_token(expected_nonce);
    if !SessionMaterial::verify_csrf(nonce, &expected_nonce_digest) {
        return Err(invalid_id_token());
    }
    let audience_count = match claims.get("aud") {
        Some(Value::Array(values)) => values.len(),
        Some(Value::String(_)) => 1,
        _ => return Err(invalid_id_token()),
    };
    if audience_count > 1
        && claims.get("azp").and_then(Value::as_str) != Some(configuration.client_id.as_str())
    {
        return Err(invalid_id_token());
    }
    let subject = bounded_claim(&claims, "sub", 255)?.ok_or_else(invalid_id_token)?;
    let email = bounded_claim(&claims, &configuration.email_claim, 254)?;
    let email_verified = claims
        .get("email_verified")
        .and_then(Value::as_bool)
        .unwrap_or(false);
    let display_name = bounded_claim(&claims, "name", 100)?;
    let groups = match claims.get(&configuration.groups_claim) {
        None => Vec::new(),
        Some(Value::Array(values)) if values.len() <= 200 => values
            .iter()
            .map(|value| {
                value
                    .as_str()
                    .filter(|group| {
                        !group.is_empty()
                            && group.len() <= 256
                            && !group.chars().any(char::is_control)
                    })
                    .map(str::to_owned)
                    .ok_or_else(invalid_id_token)
            })
            .collect::<Result<Vec<_>, _>>()?,
        Some(_) => return Err(invalid_id_token()),
    };
    Ok(ValidatedIdentity {
        subject,
        email,
        email_verified,
        display_name,
        groups,
    })
}

async fn validate_discovery(
    policy: &OidcNetworkPolicy,
    discovery: &DiscoveryDocument,
) -> Result<(), Problem> {
    if [
        &discovery.issuer,
        &discovery.authorization_endpoint,
        &discovery.token_endpoint,
        &discovery.jwks_uri,
    ]
    .iter()
    .any(|value| value.is_empty() || value.len() > 2048)
    {
        return Err(field_problem(
            "discovery_url",
            "Discovered issuer and endpoint URLs must contain 1-2,048 characters.",
        ));
    }
    validate_issuer(&discovery.issuer, policy.allow_insecure_test_endpoints)?;
    if !discovery.response_types_supported.is_empty()
        && !discovery
            .response_types_supported
            .iter()
            .any(|value| value == "code")
    {
        return Err(field_problem(
            "discovery_url",
            "The provider does not advertise Authorization Code flow support.",
        ));
    }
    if !discovery.code_challenge_methods_supported.is_empty()
        && !discovery
            .code_challenge_methods_supported
            .iter()
            .any(|value| value == "S256")
    {
        return Err(field_problem(
            "discovery_url",
            "The provider does not advertise PKCE S256 support.",
        ));
    }
    if !discovery.id_token_signing_alg_values_supported.is_empty()
        && !discovery
            .id_token_signing_alg_values_supported
            .iter()
            .any(|algorithm| is_allowed_algorithm_name(algorithm))
    {
        return Err(field_problem(
            "discovery_url",
            "The provider does not advertise a supported asymmetric ID-token algorithm.",
        ));
    }
    let authorization_url = policy
        .validate_url(&discovery.authorization_endpoint)
        .await
        .map_err(map_discovery_network)?;
    const RESERVED_AUTHORIZATION_PARAMETERS: [&str; 8] = [
        "response_type",
        "client_id",
        "redirect_uri",
        "scope",
        "state",
        "nonce",
        "code_challenge",
        "code_challenge_method",
    ];
    if authorization_url.query_pairs().any(|(name, _)| {
        RESERVED_AUTHORIZATION_PARAMETERS
            .iter()
            .any(|reserved| name == *reserved)
    }) {
        return Err(field_problem(
            "discovery_url",
            "The authorization endpoint contains a reserved OAuth query parameter.",
        ));
    }
    for endpoint in [&discovery.token_endpoint, &discovery.jwks_uri] {
        policy
            .validate_url(endpoint)
            .await
            .map_err(map_discovery_network)?;
    }
    Ok(())
}

fn validate_issuer(value: &str, allow_insecure: bool) -> Result<(), Problem> {
    let url = Url::parse(value)
        .map_err(|_| field_problem("discovery_url", "The discovered issuer URL is invalid."))?;
    if (!allow_insecure && url.scheme() != "https")
        || (allow_insecure && !matches!(url.scheme(), "http" | "https"))
        || !url.username().is_empty()
        || url.password().is_some()
        || url.query().is_some()
        || url.fragment().is_some()
        || url.host_str().is_none()
    {
        return Err(field_problem(
            "discovery_url",
            "The discovered issuer URL is invalid.",
        ));
    }
    Ok(())
}

fn validate_jwks(jwks: &JwkSet) -> Result<(), Problem> {
    if jwks.keys.is_empty() || jwks.keys.len() > 100 {
        return Err(field_problem(
            "discovery_url",
            "The provider JWKS must contain between 1 and 100 keys.",
        ));
    }
    if !jwks.keys.iter().any(|key| {
        !matches!(
            key.algorithm,
            jsonwebtoken::jwk::AlgorithmParameters::OctetKey(_)
        ) && key
            .common
            .key_algorithm
            .is_none_or(|algorithm| is_allowed_algorithm_name(&algorithm.to_string()))
            && matches!(
                key.common.public_key_use,
                None | Some(PublicKeyUse::Signature)
            )
    }) {
        return Err(field_problem(
            "discovery_url",
            "The provider JWKS contains no supported asymmetric signing key.",
        ));
    }
    Ok(())
}

fn validate_jwk_for_signature(jwk: &Jwk, algorithm: Algorithm) -> Result<(), Problem> {
    if !matches!(
        jwk.common.public_key_use,
        None | Some(PublicKeyUse::Signature)
    ) || jwk
        .common
        .key_operations
        .as_ref()
        .is_some_and(|operations| !operations.contains(&KeyOperations::Verify))
        || jwk
            .common
            .key_algorithm
            .is_some_and(|declared| declared.to_string() != format!("{algorithm:?}"))
    {
        return Err(invalid_id_token());
    }
    Ok(())
}

fn choose_token_auth_method(discovery: &DiscoveryDocument) -> Result<String, Problem> {
    if discovery.token_endpoint_auth_methods_supported.is_empty()
        || discovery
            .token_endpoint_auth_methods_supported
            .iter()
            .any(|method| method == "client_secret_basic")
    {
        Ok("client_secret_basic".to_owned())
    } else if discovery
        .token_endpoint_auth_methods_supported
        .iter()
        .any(|method| method == "client_secret_post")
    {
        Ok("client_secret_post".to_owned())
    } else {
        Err(field_problem(
            "discovery_url",
            "The provider does not support client_secret_basic or client_secret_post.",
        ))
    }
}

fn validate_configuration_request(request: &OidcConfigurationRequest) -> Result<(), Problem> {
    if request.discovery_url.trim().len() > 2048 {
        return Err(field_problem(
            "discovery_url",
            "Use a discovery URL no longer than 2,048 characters.",
        ));
    }
    if request.client_id.trim().is_empty()
        || request.client_id.len() > 512
        || request.client_id.chars().any(char::is_control)
    {
        return Err(field_problem(
            "client_id",
            "Use a client ID between 1 and 512 characters.",
        ));
    }
    if !valid_claim_name(&request.email_claim) || !valid_claim_name(&request.groups_claim) {
        return Err(field_problem(
            "claims",
            "Claim names may contain letters, digits, underscore, dot, colon, and hyphen.",
        ));
    }
    if request.email_role_mappings.len() > 500 || request.group_role_mappings.len() > 500 {
        return Err(field_problem(
            "role_mappings",
            "Configure at most 500 mappings of each type.",
        ));
    }
    Ok(())
}

fn normalized_scopes(scopes: &[String]) -> Result<Vec<String>, Problem> {
    let normalized = scopes
        .iter()
        .map(|scope| scope.trim().to_owned())
        .collect::<BTreeSet<_>>();
    if normalized.is_empty()
        || normalized.len() > 20
        || !normalized.contains("openid")
        || normalized.iter().any(|scope| {
            scope.is_empty()
                || scope.len() > 128
                || !scope.bytes().all(|byte| byte.is_ascii_graphic())
        })
    {
        return Err(field_problem(
            "scopes",
            "Use 1-20 non-empty scopes and include openid.",
        ));
    }
    Ok(normalized.into_iter().collect())
}

fn parse_mapping(mapping: &OidcRoleMappingRequest) -> Result<OidcRoleMapping, Problem> {
    if mapping.claim_value.trim().is_empty()
        || mapping.claim_value.len() > 256
        || mapping.claim_value.chars().any(char::is_control)
    {
        return Err(field_problem(
            "role_mappings",
            "Mapping claim values must contain 1-256 characters.",
        ));
    }
    Ok(OidcRoleMapping {
        claim_value: mapping.claim_value.trim().to_owned(),
        role: parse_role(&mapping.role)?,
    })
}

fn parse_role(value: &str) -> Result<Role, Problem> {
    value
        .parse()
        .map_err(|_| field_problem("role", "Use owner, operator, developer, or viewer."))
}

fn configuration_response(configuration: OidcConfiguration) -> Result<Response, Problem> {
    let etag = configuration.etag;
    let mut response = Json(OidcConfigurationResponse {
        id: configuration.id,
        discovery_url: configuration.discovery_url,
        issuer: configuration.issuer,
        client_id: configuration.client_id,
        has_client_secret: true,
        enabled: configuration.enabled,
        scopes: configuration.scopes,
        email_claim: configuration.email_claim,
        groups_claim: configuration.groups_claim,
        default_role: configuration
            .default_role
            .map(|role| role.as_str().to_owned()),
        email_role_mappings: configuration
            .email_role_mappings
            .into_iter()
            .map(mapping_response)
            .collect(),
        group_role_mappings: configuration
            .group_role_mappings
            .into_iter()
            .map(mapping_response)
            .collect(),
        etag,
    })
    .into_response();
    response.headers_mut().insert(
        header::ETAG,
        HeaderValue::from_str(&format!("\"{etag}\"")).map_err(|_| Problem::internal())?,
    );
    response
        .headers_mut()
        .insert(header::CACHE_CONTROL, HeaderValue::from_static("no-store"));
    Ok(response)
}

fn mapping_response(mapping: OidcRoleMapping) -> OidcRoleMappingResponse {
    OidcRoleMappingResponse {
        claim_value: mapping.claim_value,
        role: mapping.role.as_str().to_owned(),
    }
}

fn authenticated_redirect(material: &SessionMaterial) -> Result<Response, Problem> {
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

fn callback_url(state: &ApiState) -> Result<String, Problem> {
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

fn seal_login_flow_cookie(
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

fn consume_login_flow_cookie(
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

fn invalid_login_flow_cookie() -> Problem {
    Problem::bad_request(
        "oidc_login_flow_invalid",
        "The OIDC login flow is invalid or expired.",
    )
}

fn invalid_callback() -> Problem {
    Problem::bad_request(
        "oidc_callback_invalid",
        "The authorization callback parameters are invalid.",
    )
}

fn clear_flow_cookie(name: &str) -> String {
    format!("{name}=; Path=/; Max-Age=0; Secure; HttpOnly; SameSite=Lax")
}

fn append_cookie(response: &mut Response, value: String) {
    if let Ok(value) = HeaderValue::from_str(&value) {
        response.headers_mut().append(header::SET_COOKIE, value);
    }
}

fn require_master_key(state: &ApiState) -> Result<&MasterKey, Problem> {
    state
        .master_key
        .as_deref()
        .ok_or_else(|| Problem::service_unavailable("master_key_not_configured"))
}

fn network_policy(state: &ApiState) -> OidcNetworkPolicy {
    OidcNetworkPolicy {
        allow_insecure_test_endpoints: state.oidc_allow_insecure_test_endpoints,
    }
}

fn bounded_claim(claims: &Value, name: &str, maximum: usize) -> Result<Option<String>, Problem> {
    match claims.get(name) {
        None | Some(Value::Null) => Ok(None),
        Some(Value::String(value))
            if !value.is_empty()
                && value.len() <= maximum
                && !value.chars().any(char::is_control) =>
        {
            Ok(Some(value.clone()))
        }
        Some(_) => Err(invalid_id_token()),
    }
}

fn optional_if_match(headers: &HeaderMap) -> Result<Option<Uuid>, Problem> {
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

fn cookie<'a>(headers: &'a HeaderMap, expected_name: &str) -> Option<&'a str> {
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

fn valid_binding_token(value: &str) -> bool {
    value.len() == 43
        && value
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'-' | b'_'))
}

fn is_allowed_algorithm_name(value: &str) -> bool {
    matches!(
        value,
        "RS256" | "RS384" | "RS512" | "PS256" | "PS384" | "PS512" | "ES256" | "ES384" | "EdDSA"
    )
}

fn valid_claim_name(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= 128
        && value
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'_' | b'.' | b':' | b'-'))
}

fn oauth_form_component(value: &str) -> String {
    url::form_urlencoded::byte_serialize(value.as_bytes()).collect()
}

fn invalid_id_token() -> Problem {
    Problem::unauthorized("The ID token is invalid.")
}

fn field_problem(field: &str, detail: &str) -> Problem {
    let mut errors = FieldErrors::new();
    errors.insert(field.to_owned(), vec![detail.to_owned()]);
    Problem::validation(errors)
}

fn oidc_not_configured() -> Problem {
    Problem::new(
        StatusCode::NOT_FOUND,
        "oidc_not_configured",
        "OIDC not configured",
        "OIDC has not been configured for this installation.",
    )
}

fn map_oidc(error: OidcError) -> Problem {
    match error {
        OidcError::Persistence(error) => {
            error!(%error, "OIDC persistence operation failed");
            Problem::service_unavailable("database_unavailable")
        }
        OidcError::Invalid(detail) => field_problem("oidc", &detail),
        OidcError::NotConfigured | OidcError::Disabled => oidc_not_configured(),
        OidcError::PreconditionRequired => Problem::new(
            StatusCode::PRECONDITION_REQUIRED,
            "if_match_required",
            "Precondition required",
            "Supply the current OIDC configuration ETag in If-Match.",
        ),
        OidcError::PreconditionFailed => Problem::new(
            StatusCode::PRECONDITION_FAILED,
            "etag_mismatch",
            "Precondition failed",
            "The OIDC configuration changed after it was loaded. Refresh and retry.",
        ),
        OidcError::FlowUnavailable => Problem::bad_request(
            "oidc_flow_unavailable",
            "The authorization flow is invalid, expired, or already consumed.",
        ),
        OidcError::FlowCapacity => Problem::service_unavailable("oidc_flow_capacity_exhausted"),
        OidcError::FlowRateLimited => Problem::new(
            StatusCode::TOO_MANY_REQUESTS,
            "oidc_flow_rate_limited",
            "Too many OIDC authorization attempts",
            "Too many OIDC authorization flows were started. Wait before retrying.",
        ),
        OidcError::IdentityAlreadyLinked => Problem::conflict(
            "oidc_identity_already_linked",
            "This OIDC identity or local account is already linked.",
        ),
        OidcError::IdentityNotFound => Problem::new(
            StatusCode::NOT_FOUND,
            "oidc_identity_not_found",
            "OIDC identity not found",
            "The requested OIDC identity is not linked to the current account.",
        ),
        OidcError::LastAuthenticationMethod => Problem::conflict(
            "last_authentication_method",
            "Add a local password or another OIDC identity before unlinking this identity.",
        ),
        OidcError::LinkRequired => Problem::conflict(
            "oidc_explicit_link_required",
            "A local account with this email already exists. Sign in locally and explicitly link it.",
        ),
        OidcError::ProvisioningDenied => Problem::forbidden(
            "oidc_provisioning_denied",
            "This identity does not match an OIDC role mapping.",
        ),
        OidcError::InactiveUser => {
            Problem::forbidden("account_inactive", "The linked local account is inactive.")
        }
        OidcError::Corrupt => {
            error!("stored OIDC data is invalid");
            Problem::internal()
        }
    }
}

fn map_oidc_flow_completion(error: OidcError) -> Problem {
    match error {
        OidcError::NotConfigured | OidcError::Disabled | OidcError::PreconditionFailed => {
            Problem::bad_request(
                "oidc_flow_stale",
                "The OIDC configuration changed. Start authorization again.",
            )
        }
        other => map_oidc(other),
    }
}

fn map_discovery_network(error: OidcNetworkError) -> Problem {
    warn!(%error, "OIDC discovery validation failed");
    field_problem(
        "discovery_url",
        "Discovery, endpoint safety validation, or JWKS retrieval failed.",
    )
}

fn map_token_network(error: OidcNetworkError) -> Problem {
    warn!(%error, "OIDC provider request failed");
    Problem::service_unavailable("oidc_provider_unavailable")
}

fn default_true() -> bool {
    true
}

fn default_scopes() -> Vec<String> {
    vec![
        "openid".to_owned(),
        "email".to_owned(),
        "profile".to_owned(),
    ]
}

fn default_email_claim() -> String {
    "email".to_owned()
}

fn default_groups_claim() -> String {
    "groups".to_owned()
}

#[cfg(test)]
mod tests {
    use super::*;
    use base64::engine::general_purpose::STANDARD;
    use jsonwebtoken::{EncodingKey, Header, encode};
    use serde_json::json;
    use tower::ServiceExt as _;

    // Public test fixture used only to exercise the verifier.
    const ED25519_PRIVATE_DER_B64: &str =
        "MC4CAQAwBQYDK2VwBCIEIBrf5enAkeYcV99WmDtSpbEHFio5SdSot7TRRtzNDW11";
    const ED25519_PUBLIC_X: &str = "WOts4ZqTyrsFm_sqwXTJZQngsj3-LQRk-4kz9WFJaYc";

    #[test]
    fn configuration_debug_redacts_client_secret() {
        let request = OidcConfigurationRequest {
            discovery_url: "https://idp.example/.well-known/openid-configuration".to_owned(),
            issuer: "https://idp.example".to_owned(),
            client_id: "olp".to_owned(),
            client_secret: Some(OidcSecret(Zeroizing::new("super-secret".to_owned()))),
            enabled: true,
            scopes: default_scopes(),
            email_claim: default_email_claim(),
            groups_claim: default_groups_claim(),
            default_role: None,
            email_role_mappings: vec![],
            group_role_mappings: vec![],
        };
        assert!(!format!("{request:?}").contains("super-secret"));
    }

    #[test]
    fn hmac_id_tokens_are_rejected_before_key_use() {
        let configuration = test_configuration();
        let header = Header::new(Algorithm::HS256);
        let token = encode(
            &header,
            &json!({
                "iss": configuration.issuer,
                "sub": "subject",
                "aud": configuration.client_id,
                "exp": Utc::now().timestamp() + 300,
                "nonce": "nonce"
            }),
            &EncodingKey::from_secret(b"secret"),
        )
        .unwrap();
        assert!(
            validate_id_token(&token, &JwkSet { keys: vec![] }, &configuration, "nonce").is_err()
        );
    }

    #[test]
    fn malformed_claims_are_rejected_without_panicking() {
        let claims = json!({"sub": ["not", "a", "string"]});
        assert!(bounded_claim(&claims, "sub", 255).is_err());
    }

    #[test]
    fn optional_etag_parser_requires_a_strong_quoted_uuid() {
        let id = Uuid::now_v7();
        let mut headers = HeaderMap::new();
        assert_eq!(optional_if_match(&headers).unwrap(), None);
        headers.insert(
            header::IF_MATCH,
            HeaderValue::from_str(&format!("\"{id}\"")).unwrap(),
        );
        assert_eq!(optional_if_match(&headers).unwrap(), Some(id));
        headers.insert(
            header::IF_MATCH,
            HeaderValue::from_str(&id.to_string()).unwrap(),
        );
        assert_eq!(optional_if_match(&headers).unwrap_err().status, 400);
    }

    #[test]
    fn stateless_login_cookie_is_encrypted_origin_bound_and_state_checked() {
        let mut state = ApiState::new(
            crate::ApiMode::Control,
            None,
            std::sync::Arc::new(crate::RuntimeManager::empty()),
            "https://console.example.test",
            std::path::PathBuf::from("missing-console"),
        );
        let master_key = MasterKey::new(7, [42; 32]);
        state.master_key = Some(std::sync::Arc::new(MasterKey::new(7, [42; 32])));
        let configuration_id = Uuid::now_v7();
        let configuration_etag = Uuid::now_v7();
        let state_token = "a".repeat(43);
        let payload = LoginFlowCookiePayload {
            version: LOGIN_FLOW_COOKIE_VERSION,
            flow_id: Uuid::now_v7(),
            state: state_token.clone(),
            nonce: "b".repeat(43),
            pkce_verifier: "c".repeat(43),
            configuration_id,
            configuration_etag,
            expires_at_unix: (Utc::now() + FLOW_TTL).timestamp(),
        };
        let encoded = seal_login_flow_cookie(&state, &master_key, &payload).unwrap();
        assert!(encoded.starts_with("v2."));
        assert!(!encoded.contains(&payload.nonce));

        let flow = consume_login_flow_cookie(&state, &encoded, &state_token).unwrap();
        assert_eq!(flow.purpose, OidcFlowPurpose::Login);
        assert_eq!(flow.configuration_id, configuration_id);
        assert_eq!(flow.configuration_etag, configuration_etag);
        assert_eq!(flow.login_consumption.unwrap().flow_id, payload.flow_id);
        assert_eq!(flow.secret.nonce, payload.nonce);
        assert!(consume_login_flow_cookie(&state, &encoded, &"d".repeat(43)).is_err());

        let mut other_origin = ApiState::new(
            crate::ApiMode::Control,
            None,
            std::sync::Arc::new(crate::RuntimeManager::empty()),
            "https://other.example.test",
            std::path::PathBuf::from("missing-console"),
        );
        other_origin.master_key = Some(std::sync::Arc::new(master_key));
        assert!(consume_login_flow_cookie(&other_origin, &encoded, &state_token).is_err());
    }

    #[test]
    fn callback_prefers_the_flow_cookie_matching_its_state() {
        let mut state = ApiState::new(
            crate::ApiMode::Control,
            None,
            std::sync::Arc::new(crate::RuntimeManager::empty()),
            "https://console.example.test",
            std::path::PathBuf::from("missing-console"),
        );
        let master_key = MasterKey::new(1, [8; 32]);
        state.master_key = Some(std::sync::Arc::new(MasterKey::new(1, [8; 32])));
        let login_state = "a".repeat(43);
        let link_state = "d".repeat(43);
        let encoded = seal_login_flow_cookie(
            &state,
            &master_key,
            &LoginFlowCookiePayload {
                version: LOGIN_FLOW_COOKIE_VERSION,
                flow_id: Uuid::now_v7(),
                state: login_state.clone(),
                nonce: "b".repeat(43),
                pkce_verifier: "c".repeat(43),
                configuration_id: Uuid::now_v7(),
                configuration_etag: Uuid::now_v7(),
                expires_at_unix: (Utc::now() + FLOW_TTL).timestamp(),
            },
        )
        .unwrap();
        let mut headers = HeaderMap::new();
        headers.insert(
            header::COOKIE,
            HeaderValue::from_str(&format!(
                "{LOGIN_FLOW_COOKIE}={encoded}; {FLOW_COOKIE}={}",
                "e".repeat(43)
            ))
            .unwrap(),
        );

        assert!(
            matching_login_callback_flow(&state, &headers, &login_state)
                .unwrap()
                .is_some()
        );
        assert!(
            matching_login_callback_flow(&state, &headers, &link_state)
                .unwrap()
                .is_none()
        );

        headers.insert(
            header::COOKIE,
            HeaderValue::from_str(&format!("{LOGIN_FLOW_COOKIE}={encoded}")).unwrap(),
        );
        assert!(matching_login_callback_flow(&state, &headers, &link_state).is_err());
    }

    #[test]
    fn expired_or_tampered_stateless_login_cookie_is_rejected() {
        let mut state = ApiState::new(
            crate::ApiMode::Control,
            None,
            std::sync::Arc::new(crate::RuntimeManager::empty()),
            "https://console.example.test",
            std::path::PathBuf::from("missing-console"),
        );
        let master_key = MasterKey::new(1, [7; 32]);
        state.master_key = Some(std::sync::Arc::new(MasterKey::new(1, [7; 32])));
        let payload = LoginFlowCookiePayload {
            version: LOGIN_FLOW_COOKIE_VERSION,
            flow_id: Uuid::now_v7(),
            state: "a".repeat(43),
            nonce: "b".repeat(43),
            pkce_verifier: "c".repeat(43),
            configuration_id: Uuid::now_v7(),
            configuration_etag: Uuid::now_v7(),
            expires_at_unix: Utc::now().timestamp() - 1,
        };
        let encoded = seal_login_flow_cookie(&state, &master_key, &payload).unwrap();
        assert!(consume_login_flow_cookie(&state, &encoded, &payload.state).is_err());
        let mut tampered = encoded;
        tampered.push('x');
        assert!(consume_login_flow_cookie(&state, &tampered, &payload.state).is_err());

        let encoded = seal_login_flow_cookie(&state, &master_key, &payload).unwrap();
        for alias in ["v02.", "v+2."] {
            let aliased = encoded.replacen("v2.", alias, 1);
            assert!(consume_login_flow_cookie(&state, &aliased, &payload.state).is_err());
        }
    }

    #[tokio::test]
    async fn callback_clears_a_login_cookie_when_query_extraction_fails() {
        let state = ApiState::new(
            crate::ApiMode::Control,
            None,
            std::sync::Arc::new(crate::RuntimeManager::empty()),
            "https://console.example.test",
            std::path::PathBuf::from("missing-console"),
        );
        let response = crate::public_router(state)
            .oneshot(
                axum::http::Request::get("/api/v1/oidc/callback?code=one&code=two")
                    .header(header::COOKIE, format!("{LOGIN_FLOW_COOKIE}=opaque"))
                    .body(axum::body::Body::empty())
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(response.status(), StatusCode::BAD_REQUEST);
        assert!(
            response
                .headers()
                .get_all(header::SET_COOKIE)
                .iter()
                .filter_map(|value| value.to_str().ok())
                .any(|value| value.starts_with(&format!("{LOGIN_FLOW_COOKIE}=;")))
        );
    }

    #[test]
    fn asymmetric_validation_enforces_signature_issuer_audience_nonce_and_time() {
        let configuration = test_configuration();
        let now = Utc::now().timestamp();
        let valid_claims = json!({
            "iss": configuration.issuer,
            "sub": "subject",
            "aud": configuration.client_id,
            "iat": now,
            "exp": now + 300,
            "nonce": "expected-nonce",
            "email": "person@example.test",
            "email_verified": true,
            "groups": ["engineering"]
        });
        let jwks: JwkSet = serde_json::from_value(json!({"keys": [{
            "kty": "OKP", "crv": "Ed25519", "use": "sig", "alg": "EdDSA",
            "kid": "test-key", "x": ED25519_PUBLIC_X
        }]}))
        .unwrap();
        let valid_token = sign_ed_token(&valid_claims);
        assert!(validate_id_token(&valid_token, &jwks, &configuration, "expected-nonce").is_ok());

        for (claim, invalid_value) in [
            ("iss", json!("https://other-issuer.example")),
            ("aud", json!("other-client")),
            ("iat", json!(now + 600)),
            ("iat", json!(now - 1_200)),
            ("exp", json!(now - 120)),
            ("nonce", json!("wrong-nonce")),
        ] {
            let mut claims = valid_claims.clone();
            claims[claim] = invalid_value;
            assert!(
                validate_id_token(
                    &sign_ed_token(&claims),
                    &jwks,
                    &configuration,
                    "expected-nonce"
                )
                .is_err(),
                "{claim} must be validated"
            );
        }

        let mut tampered_parts = valid_token
            .split('.')
            .map(str::to_owned)
            .collect::<Vec<_>>();
        let replacement = if tampered_parts[2].starts_with('A') {
            "B"
        } else {
            "A"
        };
        tampered_parts[2].replace_range(..1, replacement);
        assert!(
            validate_id_token(
                &tampered_parts.join("."),
                &jwks,
                &configuration,
                "expected-nonce"
            )
            .is_err()
        );
    }

    fn sign_ed_token(claims: &Value) -> String {
        let private_der = STANDARD.decode(ED25519_PRIVATE_DER_B64).unwrap();
        let mut header = Header::new(Algorithm::EdDSA);
        header.kid = Some("test-key".to_owned());
        encode(&header, claims, &EncodingKey::from_ed_der(&private_der)).unwrap()
    }

    fn test_configuration() -> OidcConfiguration {
        OidcConfiguration {
            id: Uuid::now_v7(),
            discovery_url: "https://idp.example/.well-known/openid-configuration".to_owned(),
            issuer: "https://idp.example".to_owned(),
            authorization_endpoint: "https://idp.example/authorize".to_owned(),
            token_endpoint: "https://idp.example/token".to_owned(),
            jwks_uri: "https://idp.example/jwks".to_owned(),
            token_endpoint_auth_method: "client_secret_basic".to_owned(),
            client_id: "olp".to_owned(),
            encrypted_client_secret: olp_storage::EncryptedSecret {
                key_version: 1,
                nonce: [0; 12],
                ciphertext: vec![0; 16],
            },
            scopes: default_scopes(),
            email_claim: default_email_claim(),
            groups_claim: default_groups_claim(),
            default_role: None,
            email_role_mappings: vec![],
            group_role_mappings: vec![],
            enabled: true,
            etag: Uuid::now_v7(),
            created_at: Utc::now(),
            updated_at: Utc::now(),
        }
    }
}
