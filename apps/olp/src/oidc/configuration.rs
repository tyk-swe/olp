use std::{collections::BTreeSet, fmt};

use axum::{
    Json,
    extract::{State, rejection::JsonRejection},
    http::{HeaderMap, HeaderValue, StatusCode, header},
    response::{IntoResponse, Response},
};
use jsonwebtoken::jwk::{JwkSet, PublicKeyUse};
use olp_domain::Role;
use olp_providers::OidcNetworkPolicy;
use olp_storage::{
    OidcConfiguration, OidcError, OidcRoleMapping, UpsertOidcConfiguration,
    oidc_client_secret_aad as client_secret_aad,
};
use serde::{Deserialize, Serialize};
use tracing::error;
use url::Url;
use utoipa::ToSchema;
use uuid::Uuid;
use zeroize::Zeroizing;

use super::claims::is_allowed_algorithm_name;
use super::error::{field_problem, map_discovery_network, map_oidc, oidc_not_configured};
use super::helpers::{network_policy, optional_if_match, require_master_key, valid_claim_name};
use crate::{
    ApiState, Problem,
    management_api::{
        Permission, json_payload, require_mutation_session, require_permission,
        require_read_session, require_store,
    },
};

const DISCOVERY_LIMIT: usize = 128 * 1024;
pub(super) const JWKS_LIMIT: usize = 512 * 1024;

#[derive(Deserialize, ToSchema)]
pub struct OidcConfigurationRequest {
    pub discovery_url: String,
    /// Issuer identifier configured out-of-band with the identity provider.
    /// Discovery must return this exact value.
    pub issuer: String,
    pub client_id: String,
    #[schema(value_type = Option<String>, write_only)]
    #[serde(default)]
    pub(super) client_secret: Option<OidcSecret>,
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

pub(super) struct OidcSecret(pub(super) Zeroizing<String>);

impl OidcSecret {
    pub(super) fn expose(&self) -> &str {
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
    path = "/api/v1/oidc/configuration",
    tag = "oidc",
    responses(
        (status = 200, description = "Redacted single-provider OIDC configuration", body = OidcConfigurationResponse),
        (status = 401, description = "No active session", body = Problem),
        (status = 403, description = "Only owners can manage OIDC", body = Problem),
        (status = 404, description = "OIDC is not configured", body = Problem)
    )
)]
pub(super) async fn get_configuration(
    State(state): State<ApiState>,
    headers: HeaderMap,
) -> Result<Response, Problem> {
    let principal = require_read_session(&state, &headers).await?;
    require_permission(&principal, Permission::ManageAccess)?;
    let configuration = require_store(&state)?
        .oidc_configuration()
        .await
        .map_err(map_oidc)?
        .ok_or_else(oidc_not_configured)?;
    configuration_response(configuration)
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
pub(super) async fn put_configuration(
    State(state): State<ApiState>,
    headers: HeaderMap,
    payload: Result<Json<OidcConfigurationRequest>, JsonRejection>,
) -> Result<Response, Problem> {
    let principal = require_mutation_session(&state, &headers).await?;
    require_permission(&principal, Permission::ManageAccess)?;
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

fn default_true() -> bool {
    true
}

pub(super) fn default_scopes() -> Vec<String> {
    vec![
        "openid".to_owned(),
        "email".to_owned(),
        "profile".to_owned(),
    ]
}

pub(super) fn default_email_claim() -> String {
    "email".to_owned()
}

pub(super) fn default_groups_claim() -> String {
    "groups".to_owned()
}
