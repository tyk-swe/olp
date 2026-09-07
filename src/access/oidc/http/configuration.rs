use std::collections::BTreeSet;
use std::fmt;

use crate::access::oidc::client::Policy;
use crate::access::oidc::repository::types::OidcConfiguration;
use crate::access::oidc::repository::types::OidcError;
use crate::access::oidc::repository::types::OidcRoleMapping;
use crate::access::oidc::repository::types::UpsertOidcConfiguration;
use crate::access::policy::Permission;
use crate::access::policy::Role;
use crate::crypto::aad::oidc_client_secret as client_secret_aad;
use axum::Json;
use axum::extract::State;
use axum::extract::rejection::JsonRejection;
use axum::http::HeaderMap;
use axum::http::HeaderValue;
use axum::http::StatusCode;
use axum::http::header;
use axum::response::Response;
use jsonwebtoken::jwk::JwkSet;
use jsonwebtoken::jwk::PublicKeyUse;
use serde::Deserialize;
use serde::Serialize;
use tracing::error;
use url::Url;
use utoipa::ToSchema;
use uuid::Uuid;
use zeroize::Zeroizing;

use crate::access::oidc::http::claims::is_allowed_algorithm_name;
use crate::access::oidc::http::error::field_problem;
use crate::access::oidc::http::error::map_discovery_network;
use crate::access::oidc::http::error::map_oidc;
use crate::access::oidc::http::error::oidc_not_configured;
use crate::access::oidc::http::helpers::network_policy;
use crate::access::oidc::http::helpers::require_master_key;
use crate::access::oidc::http::helpers::valid_claim_name;
use crate::access::permissions::require_permission;
use crate::access::principal::MutationPrincipal;
use crate::access::principal::ReadPrincipal;
use crate::http::control::json_payload::json_payload;
use crate::http::control::preconditions::optional_if_match;
use crate::http::control::preconditions::with_etag;
use crate::http::control::provenance::Provenance;
use crate::http::control::state::ManagementState;
use crate::http::problem::Problem;

const DISCOVERY_LIMIT: usize = 128 * 1024;
pub(crate) const JWKS_LIMIT: usize = 512 * 1024;

#[derive(Deserialize, ToSchema)]
pub(crate) struct OidcConfigurationRequest {
    pub(crate) discovery_url: String,
    /// Issuer identifier configured out-of-band with the identity provider.
    /// Discovery must return this exact value.
    pub(crate) issuer: String,
    pub(crate) client_id: String,
    #[schema(value_type = Option<String>, write_only)]
    #[serde(default)]
    pub(crate) client_secret: Option<OidcSecret>,
    #[serde(default = "default_true")]
    pub(crate) enabled: bool,
    #[serde(default = "default_scopes")]
    pub(crate) scopes: Vec<String>,
    #[serde(default = "default_email_claim")]
    pub(crate) email_claim: String,
    #[serde(default = "default_groups_claim")]
    pub(crate) groups_claim: String,
    pub(crate) default_role: Option<String>,
    #[serde(default)]
    pub(crate) email_role_mappings: Vec<OidcRoleMappingRequest>,
    #[serde(default)]
    pub(crate) group_role_mappings: Vec<OidcRoleMappingRequest>,
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
pub(crate) struct OidcRoleMappingRequest {
    pub(crate) claim_value: String,
    pub(crate) role: String,
}

#[derive(Debug, Serialize, ToSchema)]
pub(crate) struct OidcConfigurationResponse {
    #[schema(value_type = String, format = Uuid)]
    pub(crate) id: Uuid,
    pub(crate) discovery_url: String,
    pub(crate) issuer: String,
    pub(crate) client_id: String,
    pub(crate) has_client_secret: bool,
    pub(crate) enabled: bool,
    pub(crate) scopes: Vec<String>,
    pub(crate) email_claim: String,
    pub(crate) groups_claim: String,
    pub(crate) default_role: Option<String>,
    pub(crate) email_role_mappings: Vec<OidcRoleMappingResponse>,
    pub(crate) group_role_mappings: Vec<OidcRoleMappingResponse>,
    /// Email of the operator who last saved this configuration.
    pub(crate) updated_by_email: Option<String>,
    #[schema(value_type = String, format = Uuid)]
    pub(crate) etag: Uuid,
}

#[derive(Debug, Serialize, ToSchema)]
pub(crate) struct OidcRoleMappingResponse {
    pub(crate) claim_value: String,
    pub(crate) role: String,
}

pub(crate) struct OidcSecret(pub(crate) Zeroizing<String>);

impl OidcSecret {
    pub(crate) fn expose(&self) -> &str {
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

#[utoipa::path(
    get,
    path = "/api/v3/oidc/configuration",
    tag = "oidc",
    responses(
        (status = 200, description = "Redacted single-provider OIDC configuration", body = OidcConfigurationResponse),
        (status = 401, description = "No active session", body = Problem, content_type = "application/problem+json"),
        (status = 403, description = "Only owners can manage OIDC", body = Problem, content_type = "application/problem+json"),
        (status = 404, description = "OIDC is not configured", body = Problem, content_type = "application/problem+json")
    ),
    security(("sessionCookie" = [])))]
pub(crate) async fn get_configuration(
    State(state): State<ManagementState>,
    ReadPrincipal(principal): ReadPrincipal,
) -> Result<Response, Problem> {
    require_permission(&principal, Permission::ManageAccess)?;
    let configuration = crate::access::oidc::repository::configuration::oidc_configuration(
        &state.request_boundary.pool,
    )
    .await
    .map_err(map_oidc)?
    .ok_or_else(oidc_not_configured)?;
    configuration_response(configuration)
}

#[utoipa::path(
    put,
    path = "/api/v3/oidc/configuration",
    tag = "oidc",
    request_body = OidcConfigurationRequest,
    params(("If-Match" = Option<String>, Header, description = "Required UUID ETag when updating")),
    responses(
        (status = 200, description = "OIDC configuration updated", body = OidcConfigurationResponse),
        (status = 201, description = "OIDC configuration created", body = OidcConfigurationResponse),
        (status = 412, description = "ETag mismatch", body = Problem, content_type = "application/problem+json"),
        (status = 422, description = "Discovery or configuration validation failed", body = Problem, content_type = "application/problem+json")
    ),
    security(("sessionCookie" = [], "csrfToken" = [])))]
pub(crate) async fn put_configuration(
    State(state): State<ManagementState>,
    Provenance(provenance): Provenance,
    headers: HeaderMap,
    MutationPrincipal(principal): MutationPrincipal,
    payload: Result<Json<OidcConfigurationRequest>, JsonRejection>,
) -> Result<Response, Problem> {
    require_permission(&principal, Permission::ManageAccess)?;
    let request = json_payload(payload)?;
    let policy = network_policy(&state);
    validate_configuration_request(&request, policy.allow_insecure_test_endpoints)?;
    let pool = &state.request_boundary.pool;
    let existing = crate::access::oidc::repository::configuration::oidc_configuration(pool)
        .await
        .map_err(map_oidc)?;
    let expected_etag = optional_if_match(&headers)?;
    if existing.is_some() && expected_etag.is_none() {
        return Err(map_oidc(OidcError::PreconditionRequired));
    }
    let id = existing
        .as_ref()
        .map_or_else(Uuid::now_v7, |configuration| configuration.id);
    let master_key = require_master_key(&state)?;
    let encrypted_client_secret =
        configuration_client_secret(&request, existing.as_ref(), master_key, id)?;

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
    let configuration = crate::access::oidc::repository::configuration::upsert_oidc_configuration(
        pool,
        &provenance,
        UpsertOidcConfiguration {
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
        },
    )
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

async fn validate_discovery(policy: &Policy, discovery: &DiscoveryDocument) -> Result<(), Problem> {
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
    if valid_issuer_url(value, allow_insecure) {
        return Ok(());
    }
    Err(field_problem(
        "discovery_url",
        "The discovered issuer URL is invalid.",
    ))
}

fn valid_issuer_url(value: &str, allow_insecure: bool) -> bool {
    let Ok(url) = Url::parse(value) else {
        return false;
    };
    let scheme_allowed = if allow_insecure {
        matches!(url.scheme(), "http" | "https")
    } else {
        url.scheme() == "https"
    };
    scheme_allowed
        && url.username().is_empty()
        && url.password().is_none()
        && url.query().is_none()
        && url.fragment().is_none()
        && url.host_str().is_some()
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

fn validate_configuration_request(
    request: &OidcConfigurationRequest,
    allow_insecure: bool,
) -> Result<(), Problem> {
    if request.discovery_url.trim().len() > 2048 {
        return Err(field_problem(
            "discovery_url",
            "Use a discovery URL no longer than 2,048 characters.",
        ));
    }
    let issuer = request.issuer.trim();
    if issuer.is_empty() || issuer.len() > 2048 || !valid_issuer_url(issuer, allow_insecure) {
        return Err(field_problem(
            "issuer",
            "Use an absolute https issuer URL with no credentials, query, or fragment.",
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
    let mut response = with_etag(
        Json(OidcConfigurationResponse {
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
            updated_by_email: configuration.updated_by_email,
            etag,
        }),
        etag,
    )?;
    response
        .headers_mut()
        .insert(header::CACHE_CONTROL, HeaderValue::from_static("no-pool"));
    Ok(response)
}

fn mapping_response(mapping: OidcRoleMapping) -> OidcRoleMappingResponse {
    OidcRoleMappingResponse {
        claim_value: mapping.claim_value,
        role: mapping.role.as_str().to_owned(),
    }
}

fn default_true() -> bool {
    true
}

pub(crate) fn default_scopes() -> Vec<String> {
    vec![
        "openid".to_owned(),
        "email".to_owned(),
        "profile".to_owned(),
    ]
}

pub(crate) fn default_email_claim() -> String {
    "email".to_owned()
}

pub(crate) fn default_groups_claim() -> String {
    "groups".to_owned()
}

fn configuration_client_secret(
    request: &OidcConfigurationRequest,
    existing: Option<&OidcConfiguration>,
    master_key: &crate::crypto::envelope::MasterKey,
    id: Uuid,
) -> Result<crate::crypto::envelope::EncryptedSecret, Problem> {
    Ok(match request.client_secret.as_ref() {
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
            let existing = existing.ok_or_else(|| {
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
    })
}

#[cfg(test)]
pub mod tests;
