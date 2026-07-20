use std::fmt;

use chrono::{DateTime, Duration, Utc};
use olp_domain::Role;
use thiserror::Error;
use uuid::Uuid;
use zeroize::Zeroizing;

use super::helpers::{random_token, role_rank, token_digest};
use crate::{EncryptedSecret, PersistenceError, SessionMaterial};

#[derive(Debug, Error)]
pub enum OidcError {
    #[error(transparent)]
    Persistence(#[from] PersistenceError),
    #[error("OIDC configuration input is invalid: {0}")]
    Invalid(String),
    #[error("OIDC is not configured")]
    NotConfigured,
    #[error("OIDC is disabled")]
    Disabled,
    #[error("an If-Match precondition is required")]
    PreconditionRequired,
    #[error("the OIDC configuration changed after it was read")]
    PreconditionFailed,
    #[error("the OIDC authorization flow is invalid, expired, or already consumed")]
    FlowUnavailable,
    #[error("the OIDC authorization flow capacity is exhausted")]
    FlowCapacity,
    #[error("OIDC authorization is rate limited")]
    FlowRateLimited,
    #[error("the OIDC identity is already linked")]
    IdentityAlreadyLinked,
    #[error("the OIDC identity does not exist")]
    IdentityNotFound,
    #[error("the final authentication method cannot be removed")]
    LastAuthenticationMethod,
    #[error("an existing local account must explicitly link this OIDC identity")]
    LinkRequired,
    #[error("the OIDC identity is not eligible for automatic provisioning")]
    ProvisioningDenied,
    #[error("the linked local account is inactive")]
    InactiveUser,
    #[error("stored OIDC data is invalid")]
    Corrupt,
}

impl From<sqlx::Error> for OidcError {
    fn from(error: sqlx::Error) -> Self {
        Self::Persistence(PersistenceError::Database(error))
    }
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct OidcRoleMapping {
    pub claim_value: String,
    pub role: Role,
}

#[derive(Clone)]
pub struct OidcConfiguration {
    pub id: Uuid,
    pub discovery_url: String,
    pub issuer: String,
    pub authorization_endpoint: String,
    pub token_endpoint: String,
    pub jwks_uri: String,
    pub token_endpoint_auth_method: String,
    pub client_id: String,
    pub encrypted_client_secret: EncryptedSecret,
    pub scopes: Vec<String>,
    pub email_claim: String,
    pub groups_claim: String,
    pub default_role: Option<Role>,
    pub email_role_mappings: Vec<OidcRoleMapping>,
    pub group_role_mappings: Vec<OidcRoleMapping>,
    pub enabled: bool,
    pub etag: Uuid,
    pub created_at: DateTime<Utc>,
    pub updated_at: DateTime<Utc>,
}

impl fmt::Debug for OidcConfiguration {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("OidcConfiguration")
            .field("id", &self.id)
            .field("issuer", &self.issuer)
            .field("client_id", &self.client_id)
            .field("encrypted_client_secret", &"[REDACTED]")
            .field("enabled", &self.enabled)
            .field("etag", &self.etag)
            .finish_non_exhaustive()
    }
}

impl OidcConfiguration {
    /// Exact email mappings take precedence. If several asserted groups map to
    /// roles, choose the most privileged role deterministically. The default
    /// applies only when no explicit mapping matched.
    #[must_use]
    pub fn mapped_role(&self, email: &str, groups: &[String]) -> Option<Role> {
        let normalized_email = email.trim().to_lowercase();
        if let Some(mapping) = self
            .email_role_mappings
            .iter()
            .find(|mapping| mapping.claim_value == normalized_email)
        {
            return Some(mapping.role);
        }
        self.group_role_mappings
            .iter()
            .filter(|mapping| groups.iter().any(|group| group == &mapping.claim_value))
            .map(|mapping| mapping.role)
            .min_by_key(|role| role_rank(*role))
            .or(self.default_role)
    }
}

pub struct UpsertOidcConfiguration {
    pub id: Uuid,
    pub discovery_url: String,
    pub issuer: String,
    pub authorization_endpoint: String,
    pub token_endpoint: String,
    pub jwks_uri: String,
    pub token_endpoint_auth_method: String,
    pub client_id: String,
    pub encrypted_client_secret: EncryptedSecret,
    pub scopes: Vec<String>,
    pub email_claim: String,
    pub groups_claim: String,
    pub default_role: Option<Role>,
    pub email_role_mappings: Vec<OidcRoleMapping>,
    pub group_role_mappings: Vec<OidcRoleMapping>,
    pub enabled: bool,
    pub actor_user_id: Uuid,
    pub expected_etag: Option<Uuid>,
}

impl fmt::Debug for UpsertOidcConfiguration {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("UpsertOidcConfiguration")
            .field("id", &self.id)
            .field("discovery_url", &self.discovery_url)
            .field("issuer", &self.issuer)
            .field("client_id", &self.client_id)
            .field("encrypted_client_secret", &"[REDACTED]")
            .field("enabled", &self.enabled)
            .finish_non_exhaustive()
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum OidcFlowPurpose {
    Login,
    Link,
}

impl OidcFlowPurpose {
    pub(super) const fn as_str(self) -> &'static str {
        match self {
            Self::Login => "login",
            Self::Link => "link",
        }
    }

    pub(super) fn parse(value: &str) -> Result<Self, OidcError> {
        match value {
            "login" => Ok(Self::Login),
            "link" => Ok(Self::Link),
            _ => Err(OidcError::Corrupt),
        }
    }
}

pub struct OidcFlowMaterial {
    state: Zeroizing<String>,
    browser_binding: Zeroizing<String>,
    nonce: Zeroizing<String>,
    pkce_verifier: Zeroizing<String>,
}

impl OidcFlowMaterial {
    #[must_use]
    pub fn generate() -> Self {
        Self {
            state: random_token(),
            browser_binding: random_token(),
            nonce: random_token(),
            pkce_verifier: random_token(),
        }
    }

    #[must_use]
    pub fn state(&self) -> &str {
        &self.state
    }

    #[must_use]
    pub fn browser_binding(&self) -> &str {
        &self.browser_binding
    }

    #[must_use]
    pub fn nonce(&self) -> &str {
        &self.nonce
    }

    #[must_use]
    pub fn pkce_verifier(&self) -> &str {
        &self.pkce_verifier
    }

    #[must_use]
    pub fn pkce_challenge(&self) -> String {
        use base64::{Engine as _, engine::general_purpose::URL_SAFE_NO_PAD};
        use sha2::{Digest, Sha256};

        URL_SAFE_NO_PAD.encode(Sha256::digest(self.pkce_verifier.as_bytes()))
    }

    #[must_use]
    pub fn state_digest(&self) -> [u8; 32] {
        token_digest(&self.state)
    }

    #[must_use]
    pub fn browser_binding_digest(&self) -> [u8; 32] {
        token_digest(&self.browser_binding)
    }
}

impl fmt::Debug for OidcFlowMaterial {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str("OidcFlowMaterial([REDACTED])")
    }
}

pub struct NewOidcFlow {
    pub id: Uuid,
    pub configuration_id: Uuid,
    /// Binds flow creation to the exact enabled configuration used to build
    /// the authorization URL.
    pub configuration_etag: Uuid,
    pub purpose: OidcFlowPurpose,
    pub actor_user_id: Option<Uuid>,
    pub state_digest: [u8; 32],
    pub browser_binding_digest: [u8; 32],
    pub encrypted_payload: EncryptedSecret,
    pub expires_at: DateTime<Utc>,
}

impl fmt::Debug for NewOidcFlow {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("NewOidcFlow")
            .field("id", &self.id)
            .field("configuration_id", &self.configuration_id)
            .field("configuration_etag", &self.configuration_etag)
            .field("purpose", &self.purpose)
            .field("actor_user_id", &self.actor_user_id)
            .field("state_digest", &"[REDACTED]")
            .field("browser_binding_digest", &"[REDACTED]")
            .field("encrypted_payload", &"[REDACTED]")
            .field("expires_at", &self.expires_at)
            .finish()
    }
}

#[derive(Clone)]
pub struct OidcFlowRecord {
    pub id: Uuid,
    pub configuration_id: Uuid,
    pub purpose: OidcFlowPurpose,
    pub actor_user_id: Option<Uuid>,
    pub encrypted_payload: EncryptedSecret,
}

impl fmt::Debug for OidcFlowRecord {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("OidcFlowRecord")
            .field("id", &self.id)
            .field("configuration_id", &self.configuration_id)
            .field("purpose", &self.purpose)
            .field("actor_user_id", &self.actor_user_id)
            .field("encrypted_payload", &"[REDACTED]")
            .finish()
    }
}

#[derive(Clone, Debug)]
pub struct OidcAuthenticatedUser {
    pub id: Uuid,
    pub email: String,
    pub display_name: String,
    pub role: Role,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct OidcIdentityRecord {
    pub id: Uuid,
    pub issuer: String,
    pub email_at_link: Option<String>,
    pub last_login_at: Option<DateTime<Utc>>,
    pub created_at: DateTime<Utc>,
    pub can_unlink: bool,
}

pub struct CompleteOidcLogin<'a> {
    pub configuration_id: Uuid,
    pub configuration_etag: Uuid,
    pub issuer: &'a str,
    pub subject: &'a str,
    pub email: Option<&'a str>,
    pub display_name: Option<&'a str>,
    pub provisioning_role: Option<Role>,
    pub session: &'a SessionMaterial,
    pub session_ttl: Duration,
}

impl fmt::Debug for CompleteOidcLogin<'_> {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("CompleteOidcLogin")
            .field("configuration_id", &self.configuration_id)
            .field("configuration_etag", &self.configuration_etag)
            .field("issuer", &self.issuer)
            .field("subject", &self.subject)
            .field("email", &self.email)
            .field("display_name", &self.display_name)
            .field("provisioning_role", &self.provisioning_role)
            .field("session", &"[REDACTED]")
            .field("session_ttl", &self.session_ttl)
            .finish()
    }
}

pub struct CompleteOidcLink<'a> {
    pub user_id: Uuid,
    pub configuration_id: Uuid,
    pub configuration_etag: Uuid,
    pub issuer: &'a str,
    pub subject: &'a str,
    pub email: Option<&'a str>,
    pub session: &'a SessionMaterial,
    pub session_ttl: Duration,
}

impl fmt::Debug for CompleteOidcLink<'_> {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("CompleteOidcLink")
            .field("user_id", &self.user_id)
            .field("configuration_id", &self.configuration_id)
            .field("configuration_etag", &self.configuration_etag)
            .field("issuer", &self.issuer)
            .field("subject", &self.subject)
            .field("email", &self.email)
            .field("session", &"[REDACTED]")
            .field("session_ttl", &self.session_ttl)
            .finish()
    }
}
