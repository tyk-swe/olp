use std::{collections::BTreeSet, fmt};

use axum::{
    Json,
    extract::{State, rejection::JsonRejection},
    http::{HeaderMap, HeaderValue, StatusCode, header},
    response::Response,
};
use jsonwebtoken::jwk::{JwkSet, PublicKeyUse};
use olp_db::{
    oidc::types::OidcConfiguration, oidc::types::OidcError, oidc::types::OidcRoleMapping,
    oidc::types::UpsertOidcConfiguration, security::aad::oidc_client_secret as client_secret_aad,
};
use olp_engine::domain::auth::{Permission, Role};
use olp_engine::providers::oidc::Policy;
use serde::{Deserialize, Serialize};
use tracing::error;
use url::Url;
use utoipa::ToSchema;
use uuid::Uuid;
use zeroize::Zeroizing;

use super::claims::is_allowed_algorithm_name;
use super::error::{field_problem, map_discovery_network, map_oidc, oidc_not_configured};
use super::helpers::{network_policy, require_master_key, valid_claim_name};
use crate::{
    bootstrap::mode_dependencies::ManagementState,
    management::{
        json_payload::json_payload,
        permissions::require_permission,
        preconditions::{optional_if_match, with_etag},
        sessions::{require_mutation_session, require_read_session},
    },
    public_http::problem::Problem,
};

const DISCOVERY_LIMIT: usize = 128 * 1024;
pub(super) const JWKS_LIMIT: usize = 512 * 1024;

#[derive(Deserialize, ToSchema)]
pub(super) struct OidcConfigurationRequest {
    pub(super) discovery_url: String,
    /// Issuer identifier configured out-of-band with the identity provider.
    /// Discovery must return this exact value.
    pub(super) issuer: String,
    pub(super) client_id: String,
    #[schema(value_type = Option<String>, write_only)]
    #[serde(default)]
    pub(super) client_secret: Option<OidcSecret>,
    #[serde(default = "default_true")]
    pub(super) enabled: bool,
    #[serde(default = "default_scopes")]
    pub(super) scopes: Vec<String>,
    #[serde(default = "default_email_claim")]
    pub(super) email_claim: String,
    #[serde(default = "default_groups_claim")]
    pub(super) groups_claim: String,
    pub(super) default_role: Option<String>,
    #[serde(default)]
    pub(super) email_role_mappings: Vec<OidcRoleMappingRequest>,
    #[serde(default)]
    pub(super) group_role_mappings: Vec<OidcRoleMappingRequest>,
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
pub(super) struct OidcRoleMappingRequest {
    pub(super) claim_value: String,
    pub(super) role: String,
}

#[derive(Debug, Serialize, ToSchema)]
pub(super) struct OidcConfigurationResponse {
    #[schema(value_type = String, format = Uuid)]
    pub(super) id: Uuid,
    pub(super) discovery_url: String,
    pub(super) issuer: String,
    pub(super) client_id: String,
    pub(super) has_client_secret: bool,
    pub(super) enabled: bool,
    pub(super) scopes: Vec<String>,
    pub(super) email_claim: String,
    pub(super) groups_claim: String,
    pub(super) default_role: Option<String>,
    pub(super) email_role_mappings: Vec<OidcRoleMappingResponse>,
    pub(super) group_role_mappings: Vec<OidcRoleMappingResponse>,
    #[schema(value_type = String, format = Uuid)]
    pub(super) etag: Uuid,
}

#[derive(Debug, Serialize, ToSchema)]
pub(super) struct OidcRoleMappingResponse {
    pub(super) claim_value: String,
    pub(super) role: String,
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
    State(state): State<ManagementState>,
    headers: HeaderMap,
) -> Result<Response, Problem> {
    let principal = require_read_session(&state, &headers).await?;
    require_permission(&principal, Permission::ManageAccess)?;
    let configuration = state
        .store()
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
    State(state): State<ManagementState>,
    headers: HeaderMap,
    payload: Result<Json<OidcConfigurationRequest>, JsonRejection>,
) -> Result<Response, Problem> {
    let principal = require_mutation_session(&state, &headers).await?;
    require_permission(&principal, Permission::ManageAccess)?;
    let request = json_payload(payload)?;
    validate_configuration_request(&request)?;
    let store = state.store();
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
            etag,
        }),
        etag,
    )?;
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

#[cfg(test)]
mod tests {
    use axum::{body::Body, http::header};
    use http_body_util::BodyExt as _;
    use jsonwebtoken::jwk::JwkSet;
    use olp_db::{oidc::types::OidcConfiguration, security::envelope::EncryptedSecret};
    use serde_json::json;

    use super::*;

    fn request() -> OidcConfigurationRequest {
        OidcConfigurationRequest {
            discovery_url: "https://idp.example/.well-known/openid-configuration".to_owned(),
            issuer: "https://idp.example".to_owned(),
            client_id: "olp-console".to_owned(),
            client_secret: None,
            enabled: true,
            scopes: default_scopes(),
            email_claim: default_email_claim(),
            groups_claim: default_groups_claim(),
            default_role: None,
            email_role_mappings: Vec::new(),
            group_role_mappings: Vec::new(),
        }
    }

    fn discovery(auth_methods: &[&str]) -> DiscoveryDocument {
        DiscoveryDocument {
            issuer: "https://idp.example".to_owned(),
            authorization_endpoint: "https://idp.example/authorize".to_owned(),
            token_endpoint: "https://idp.example/token".to_owned(),
            jwks_uri: "https://idp.example/jwks".to_owned(),
            response_types_supported: vec!["code".to_owned()],
            code_challenge_methods_supported: vec!["S256".to_owned()],
            token_endpoint_auth_methods_supported: auth_methods
                .iter()
                .map(|value| (*value).to_owned())
                .collect(),
            id_token_signing_alg_values_supported: vec!["EdDSA".to_owned()],
        }
    }

    #[test]
    fn issuer_policy_rejects_non_origin_components_and_insecure_production_urls() {
        for (value, allow_insecure, accepted) in [
            ("https://idp.example", false, true),
            ("https://idp.example/tenant", false, true),
            ("http://127.0.0.1:8080", true, true),
            ("http://idp.example", false, false),
            ("ftp://idp.example", true, false),
            ("https://user@idp.example", false, false),
            ("https://idp.example?tenant=one", false, false),
            ("https://idp.example#fragment", false, false),
            ("not a URL", false, false),
        ] {
            assert_eq!(
                validate_issuer(value, allow_insecure).is_ok(),
                accepted,
                "unexpected issuer result for {value}"
            );
        }
    }

    #[test]
    fn token_auth_method_uses_interoperable_preference_order() {
        for (methods, expected) in [
            (vec![], Ok("client_secret_basic")),
            (vec!["client_secret_post"], Ok("client_secret_post")),
            (
                vec!["client_secret_post", "client_secret_basic"],
                Ok("client_secret_basic"),
            ),
            (vec!["private_key_jwt"], Err(422_u16)),
        ] {
            let result = choose_token_auth_method(&discovery(&methods));
            match expected {
                Ok(method) => assert_eq!(result.unwrap(), method),
                Err(status) => assert_eq!(result.unwrap_err().status, status),
            }
        }
    }

    #[test]
    fn scopes_are_trimmed_deduplicated_sorted_and_require_openid() {
        assert_eq!(
            normalized_scopes(&[
                " profile ".to_owned(),
                "openid".to_owned(),
                "email".to_owned(),
                "email".to_owned(),
            ])
            .unwrap(),
            ["email", "openid", "profile"]
        );

        for scopes in [
            vec![],
            vec!["email".to_owned()],
            vec!["openid".to_owned(), "two words".to_owned()],
            vec!["openid".to_owned(), "x".repeat(129)],
            std::iter::once("openid".to_owned())
                .chain((0..20).map(|index| format!("scope-{index}")))
                .collect(),
        ] {
            assert_eq!(normalized_scopes(&scopes).unwrap_err().status, 422);
        }
    }

    #[test]
    fn request_shape_validation_bounds_each_untrusted_collection_and_identifier() {
        validate_configuration_request(&request()).unwrap();

        let mut invalid = request();
        invalid.discovery_url = "x".repeat(2_049);
        assert!(validate_configuration_request(&invalid).is_err());

        for client_id in [String::new(), "x".repeat(513), "bad\nclient".to_owned()] {
            let mut invalid = request();
            invalid.client_id = client_id;
            assert!(validate_configuration_request(&invalid).is_err());
        }

        for claim in [String::new(), "contains/slash".to_owned(), "x".repeat(129)] {
            let mut invalid = request();
            invalid.email_claim = claim;
            assert!(validate_configuration_request(&invalid).is_err());
        }

        let mut invalid = request();
        invalid.email_role_mappings = (0..501)
            .map(|index| OidcRoleMappingRequest {
                claim_value: format!("person-{index}"),
                role: "viewer".to_owned(),
            })
            .collect();
        assert!(validate_configuration_request(&invalid).is_err());
    }

    #[test]
    fn role_mappings_trim_values_and_reject_ambiguous_input() {
        let mapping = parse_mapping(&OidcRoleMappingRequest {
            claim_value: " engineering ".to_owned(),
            role: "operator".to_owned(),
        })
        .unwrap();
        assert_eq!(mapping.claim_value, "engineering");
        assert_eq!(mapping.role, Role::Operator);

        for (claim_value, role) in [
            (" ".to_owned(), "viewer"),
            ("x".repeat(257), "viewer"),
            ("bad\nvalue".to_owned(), "viewer"),
            ("engineering".to_owned(), "administrator"),
        ] {
            assert!(
                parse_mapping(&OidcRoleMappingRequest {
                    claim_value,
                    role: role.to_owned(),
                })
                .is_err()
            );
        }
    }

    #[test]
    fn jwks_requires_a_supported_asymmetric_signing_key() {
        let valid: JwkSet = serde_json::from_value(json!({"keys": [{
            "kty": "OKP", "crv": "Ed25519", "use": "sig", "alg": "EdDSA",
            "kid": "valid", "x": "WOts4ZqTyrsFm_sqwXTJZQngsj3-LQRk-4kz9WFJaYc"
        }]}))
        .unwrap();
        validate_jwks(&valid).unwrap();

        for value in [
            json!({"keys": []}),
            json!({"keys": [{"kty": "oct", "k": "c2VjcmV0", "alg": "HS256"}]}),
            json!({"keys": [{
                "kty": "OKP", "crv": "Ed25519", "use": "enc", "alg": "EdDSA",
                "x": "WOts4ZqTyrsFm_sqwXTJZQngsj3-LQRk-4kz9WFJaYc"
            }]}),
        ] {
            let jwks: JwkSet = serde_json::from_value(value).unwrap();
            assert_eq!(validate_jwks(&jwks).unwrap_err().status, 422);
        }
    }

    #[tokio::test]
    async fn configuration_response_is_redacted_uncacheable_and_versioned() {
        let id = Uuid::now_v7();
        let etag = Uuid::now_v7();
        let now = chrono::Utc::now();
        let secret_nonce = [0xa5; 12];
        let secret_ciphertext = b"encrypted-secret-sentinel".to_vec();
        let encoded_nonce = serde_json::to_string(&secret_nonce).unwrap();
        let encoded_ciphertext = serde_json::to_string(&secret_ciphertext).unwrap();
        let response = configuration_response(OidcConfiguration {
            id,
            discovery_url: "https://idp.example/.well-known/openid-configuration".to_owned(),
            issuer: "https://idp.example".to_owned(),
            authorization_endpoint: "https://idp.example/authorize".to_owned(),
            token_endpoint: "https://idp.example/token".to_owned(),
            jwks_uri: "https://idp.example/jwks".to_owned(),
            token_endpoint_auth_method: "client_secret_basic".to_owned(),
            client_id: "olp-console".to_owned(),
            encrypted_client_secret: EncryptedSecret {
                key_version: 1,
                nonce: secret_nonce,
                ciphertext: secret_ciphertext,
            },
            scopes: default_scopes(),
            email_claim: default_email_claim(),
            groups_claim: default_groups_claim(),
            default_role: Some(Role::Viewer),
            email_role_mappings: vec![OidcRoleMapping {
                claim_value: "owner@example.test".to_owned(),
                role: Role::Owner,
            }],
            group_role_mappings: Vec::new(),
            enabled: true,
            etag,
            created_at: now,
            updated_at: now,
        })
        .unwrap();

        assert_eq!(response.headers()[header::ETAG], format!("\"{etag}\""));
        assert_eq!(response.headers()[header::CACHE_CONTROL], "no-store");
        let body = Body::new(response.into_body())
            .collect()
            .await
            .unwrap()
            .to_bytes();
        let encoded_body = std::str::from_utf8(&body).unwrap();
        for secret in [
            "encrypted_client_secret",
            "ciphertext",
            "nonce",
            &encoded_nonce,
            &encoded_ciphertext,
        ] {
            assert!(
                !encoded_body.contains(secret),
                "leaked secret material: {secret}"
            );
        }
        let body: serde_json::Value = serde_json::from_slice(&body).unwrap();
        assert_eq!(body["id"], id.to_string());
        assert_eq!(body["default_role"], "viewer");
        assert_eq!(body["email_role_mappings"][0]["role"], "owner");
        assert_eq!(body["has_client_secret"], true);
        assert!(body.get("client_secret").is_none());
    }
}
