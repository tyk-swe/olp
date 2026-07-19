use std::{collections::BTreeMap, fmt};

use base64::{Engine as _, engine::general_purpose::URL_SAFE_NO_PAD};
use chrono::{DateTime, Duration, Utc};
use olp_domain::Role;
use rand::RngCore;
use sha2::{Digest, Sha256};
use sqlx::{Postgres, Row, Transaction};
use thiserror::Error;
use uuid::Uuid;
use zeroize::{Zeroize, Zeroizing};

use crate::{EncryptedSecret, PersistenceError, PgStore, SessionMaterial};

const OIDC_CONFIGURATION_LOCK_ID: i64 = 0x4f4c_505f_4f49; // "OLP_OI"
const OIDC_FLOW_CAPACITY_LOCK_ID: i64 = 0x4f4c_505f_4f46; // "OLP_OF"
const MAX_MAPPINGS: usize = 500;
const MAX_ACTIVE_FLOWS: i64 = 10_000;
const MAX_AUTHORIZATION_FLOWS_PER_MINUTE: i64 = 300;
const OIDC_LOGIN_CONSUMPTION_DELETE_BATCH: i64 = 1_000;

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
    const fn as_str(self) -> &'static str {
        match self {
            Self::Login => "login",
            Self::Link => "link",
        }
    }

    fn parse(value: &str) -> Result<Self, OidcError> {
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

impl PgStore {
    pub async fn oidc_configuration(&self) -> Result<Option<OidcConfiguration>, OidcError> {
        let row = sqlx::query(
            "SELECT id, discovery_url, issuer, authorization_endpoint, token_endpoint, jwks_uri, \
                    token_endpoint_auth_method, client_id, encrypted_client_secret, secret_nonce, \
                    secret_key_version, scopes, email_claim, groups_claim, default_role::text AS default_role, \
                    enabled, etag, created_at, updated_at, \
                    COALESCE((SELECT jsonb_agg(jsonb_build_object( \
                        'claim_value', mapping.email, 'role', mapping.role::text) ORDER BY mapping.email) \
                        FROM oidc_email_role_mappings mapping WHERE mapping.configuration_id = oidc_configurations.id), \
                        '[]'::jsonb) AS email_mappings, \
                    COALESCE((SELECT jsonb_agg(jsonb_build_object( \
                        'claim_value', mapping.group_name, 'role', mapping.role::text) ORDER BY mapping.group_name) \
                        FROM oidc_group_role_mappings mapping WHERE mapping.configuration_id = oidc_configurations.id), \
                        '[]'::jsonb) AS group_mappings \
             FROM oidc_configurations WHERE singleton LIMIT 1",
        )
        .fetch_optional(self.pool())
        .await?;
        let Some(row) = row else {
            return Ok(None);
        };
        oidc_configuration_from_row(row).map(Some)
    }

    pub async fn enabled_oidc_configuration(&self) -> Result<OidcConfiguration, OidcError> {
        self.oidc_configuration()
            .await?
            .ok_or(OidcError::NotConfigured)
            .and_then(|configuration| {
                if configuration.enabled {
                    Ok(configuration)
                } else {
                    Err(OidcError::Disabled)
                }
            })
    }

    pub async fn upsert_oidc_configuration(
        &self,
        input: UpsertOidcConfiguration,
    ) -> Result<OidcConfiguration, OidcError> {
        validate_configuration(&input)?;
        let mut transaction = self.pool().begin().await?;
        sqlx::query("SELECT pg_advisory_xact_lock($1)")
            .bind(OIDC_CONFIGURATION_LOCK_ID)
            .execute(&mut *transaction)
            .await?;
        let current =
            sqlx::query("SELECT id, etag FROM oidc_configurations WHERE singleton FOR UPDATE")
                .fetch_optional(&mut *transaction)
                .await?;
        match current {
            Some(row) => {
                let current_id: Uuid = row.get("id");
                let current_etag: Uuid = row.get("etag");
                let expected = input.expected_etag.ok_or(OidcError::PreconditionRequired)?;
                if current_id != input.id || current_etag != expected {
                    return Err(OidcError::PreconditionFailed);
                }
            }
            None if input.expected_etag.is_some() => return Err(OidcError::PreconditionFailed),
            None => {}
        }

        let etag = Uuid::now_v7();
        let now = Utc::now();
        sqlx::query(
            "INSERT INTO oidc_configurations \
             (id, singleton, discovery_url, issuer, authorization_endpoint, token_endpoint, jwks_uri, \
              token_endpoint_auth_method, client_id, encrypted_client_secret, secret_nonce, \
              secret_key_version, scopes, email_claim, groups_claim, default_role, enabled, etag, \
              updated_by, created_at, updated_at) \
             VALUES ($1, true, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, \
                     CAST($15 AS user_role), $16, $17, $18, $19, $19) \
             ON CONFLICT (singleton) DO UPDATE SET \
               discovery_url = EXCLUDED.discovery_url, issuer = EXCLUDED.issuer, \
               authorization_endpoint = EXCLUDED.authorization_endpoint, \
               token_endpoint = EXCLUDED.token_endpoint, jwks_uri = EXCLUDED.jwks_uri, \
               token_endpoint_auth_method = EXCLUDED.token_endpoint_auth_method, \
               client_id = EXCLUDED.client_id, encrypted_client_secret = EXCLUDED.encrypted_client_secret, \
               secret_nonce = EXCLUDED.secret_nonce, secret_key_version = EXCLUDED.secret_key_version, \
               scopes = EXCLUDED.scopes, email_claim = EXCLUDED.email_claim, \
               groups_claim = EXCLUDED.groups_claim, default_role = EXCLUDED.default_role, \
               enabled = EXCLUDED.enabled, etag = EXCLUDED.etag, updated_by = EXCLUDED.updated_by, \
               updated_at = EXCLUDED.updated_at",
        )
        .bind(input.id)
        .bind(input.discovery_url.trim())
        .bind(input.issuer.trim())
        .bind(input.authorization_endpoint.trim())
        .bind(input.token_endpoint.trim())
        .bind(input.jwks_uri.trim())
        .bind(&input.token_endpoint_auth_method)
        .bind(input.client_id.trim())
        .bind(&input.encrypted_client_secret.ciphertext)
        .bind(input.encrypted_client_secret.nonce.to_vec())
        .bind(i32::try_from(input.encrypted_client_secret.key_version).map_err(|_| OidcError::Corrupt)?)
        .bind(&input.scopes)
        .bind(&input.email_claim)
        .bind(&input.groups_claim)
        .bind(input.default_role.map(|role| role.as_str()))
        .bind(input.enabled)
        .bind(etag)
        .bind(input.actor_user_id)
        .bind(now)
        .execute(&mut *transaction)
        .await?;

        sqlx::query("DELETE FROM oidc_email_role_mappings WHERE configuration_id = $1")
            .bind(input.id)
            .execute(&mut *transaction)
            .await?;
        sqlx::query("DELETE FROM oidc_group_role_mappings WHERE configuration_id = $1")
            .bind(input.id)
            .execute(&mut *transaction)
            .await?;
        insert_mappings(&mut transaction, input.id, &input.email_role_mappings, true).await?;
        insert_mappings(
            &mut transaction,
            input.id,
            &input.group_role_mappings,
            false,
        )
        .await?;
        // Configuration changes invalidate outstanding redirects and their
        // encrypted PKCE material.
        sqlx::query("DELETE FROM oidc_authorization_flows WHERE configuration_id = $1")
            .bind(input.id)
            .execute(&mut *transaction)
            .await?;
        sqlx::query(
            "INSERT INTO audit_events \
             (id, actor_user_id, action, resource_type, resource_id, outcome, occurred_at) \
             VALUES ($1, $2, 'oidc.configuration_update', 'oidc_configuration', $3, 'success', $4)",
        )
        .bind(Uuid::now_v7())
        .bind(input.actor_user_id)
        .bind(input.id.to_string())
        .bind(now)
        .execute(&mut *transaction)
        .await?;
        transaction.commit().await?;
        self.oidc_configuration().await?.ok_or(OidcError::Corrupt)
    }

    /// Persists an authenticated identity-link flow. New anonymous login
    /// flows are encrypted browser cookies and must never create a database
    /// row; persisted login rows are accepted only by the legacy callback
    /// consumer until they expire.
    pub async fn create_oidc_flow(&self, flow: NewOidcFlow) -> Result<(), OidcError> {
        if flow.purpose == OidcFlowPurpose::Login {
            return Err(OidcError::Invalid(
                "new OIDC login flows are stateless and cannot be persisted".to_owned(),
            ));
        }
        if flow.expires_at <= Utc::now() || flow.actor_user_id.is_none() {
            return Err(OidcError::Invalid(
                "authorization flow metadata is invalid".to_owned(),
            ));
        }
        let mut transaction = self.pool().begin().await?;
        // Configuration updates delete every outstanding redirect while
        // holding this lock. Serialize insertion with that invalidation and
        // reject a flow built from a configuration that changed while its
        // authorization URL was being prepared.
        require_current_enabled_configuration(
            &mut transaction,
            flow.configuration_id,
            flow.configuration_etag,
        )
        .await?;
        sqlx::query("SELECT pg_advisory_xact_lock($1)")
            .bind(OIDC_FLOW_CAPACITY_LOCK_ID)
            .execute(&mut *transaction)
            .await?;
        sqlx::query("DELETE FROM oidc_authorization_flows WHERE expires_at <= now()")
            .execute(&mut *transaction)
            .await?;
        let active_flows: i64 = sqlx::query_scalar("SELECT count(*) FROM oidc_authorization_flows")
            .fetch_one(&mut *transaction)
            .await?;
        if active_flows >= MAX_ACTIVE_FLOWS {
            return Err(OidcError::FlowCapacity);
        }
        let recent_flows: i64 = sqlx::query_scalar(
            "SELECT count(*) FROM oidc_authorization_flows \
             WHERE created_at > now() - interval '1 minute'",
        )
        .fetch_one(&mut *transaction)
        .await?;
        if recent_flows >= MAX_AUTHORIZATION_FLOWS_PER_MINUTE {
            return Err(OidcError::FlowRateLimited);
        }
        sqlx::query(
            "INSERT INTO oidc_authorization_flows \
             (id, configuration_id, configuration_etag, purpose, actor_user_id, state_digest, \
              browser_binding_digest, client_digest, encrypted_payload, payload_nonce, \
              payload_key_version, expires_at, created_at) \
             VALUES ($1, $2, $3, $4, $5, $6, $7, NULL, $8, $9, $10, $11, now())",
        )
        .bind(flow.id)
        .bind(flow.configuration_id)
        .bind(flow.configuration_etag)
        .bind(flow.purpose.as_str())
        .bind(flow.actor_user_id)
        .bind(flow.state_digest.to_vec())
        .bind(flow.browser_binding_digest.to_vec())
        .bind(flow.encrypted_payload.ciphertext)
        .bind(flow.encrypted_payload.nonce.to_vec())
        .bind(i32::try_from(flow.encrypted_payload.key_version).map_err(|_| OidcError::Corrupt)?)
        .bind(flow.expires_at)
        .execute(&mut *transaction)
        .await?;
        transaction.commit().await?;
        Ok(())
    }

    /// Atomically claims a browser-held login flow before any provider
    /// exchange. The row contains no PKCE, state, nonce, or identity material;
    /// it exists only until the encrypted cookie's own expiry. A data-modifying
    /// CTE keeps cleanup self-sustaining even in control-only deployments that
    /// do not run the periodic maintenance worker.
    pub async fn consume_oidc_login_flow(
        &self,
        flow_id: Uuid,
        expires_at: DateTime<Utc>,
    ) -> Result<(), OidcError> {
        let now = Utc::now();
        if expires_at <= now || expires_at > now + Duration::minutes(11) {
            return Err(OidcError::FlowUnavailable);
        }
        let consumed: Option<Uuid> = sqlx::query_scalar(
            "WITH expired AS ( \
               SELECT ctid FROM oidc_login_flow_consumptions \
               WHERE expires_at <= now() LIMIT $3 \
             ), deleted AS ( \
               DELETE FROM oidc_login_flow_consumptions consumption USING expired \
               WHERE consumption.ctid = expired.ctid \
             ) \
             INSERT INTO oidc_login_flow_consumptions (flow_id, expires_at, consumed_at) \
             SELECT $1, $2, now() WHERE $2 > now() \
             ON CONFLICT (flow_id) DO NOTHING \
             RETURNING flow_id",
        )
        .bind(flow_id)
        .bind(expires_at)
        .bind(OIDC_LOGIN_CONSUMPTION_DELETE_BATCH)
        .fetch_optional(self.pool())
        .await?;
        consumed.ok_or(OidcError::FlowUnavailable)?;
        Ok(())
    }

    /// Atomically consumes state only when both the callback state and the
    /// browser-binding cookie match. A mismatch cannot burn another browser's
    /// legitimate flow.
    pub async fn consume_oidc_flow(
        &self,
        state: &str,
        browser_binding: &str,
    ) -> Result<OidcFlowRecord, OidcError> {
        if state.len() != 43 || browser_binding.len() != 43 {
            return Err(OidcError::FlowUnavailable);
        }
        let row = sqlx::query(
            "DELETE FROM oidc_authorization_flows \
             WHERE state_digest = $1 AND browser_binding_digest = $2 AND expires_at > now() \
             RETURNING id, configuration_id, purpose, actor_user_id, encrypted_payload, \
                       payload_nonce, payload_key_version",
        )
        .bind(token_digest(state).to_vec())
        .bind(token_digest(browser_binding).to_vec())
        .fetch_optional(self.pool())
        .await?
        .ok_or(OidcError::FlowUnavailable)?;
        Ok(OidcFlowRecord {
            id: row.get("id"),
            configuration_id: row.get("configuration_id"),
            purpose: OidcFlowPurpose::parse(row.get("purpose"))?,
            actor_user_id: row.get("actor_user_id"),
            encrypted_payload: encrypted_from_row(
                row.get("payload_key_version"),
                row.get("payload_nonce"),
                row.get("encrypted_payload"),
            )?,
        })
    }

    pub async fn complete_oidc_login(
        &self,
        input: CompleteOidcLogin<'_>,
    ) -> Result<OidcAuthenticatedUser, OidcError> {
        validate_subject(input.subject)?;
        let now = Utc::now();
        let expires_at = checked_session_expiry(now, input.session_ttl)?;
        let mut transaction = self.pool().begin().await?;
        require_current_enabled_configuration(
            &mut transaction,
            input.configuration_id,
            input.configuration_etag,
        )
        .await?;
        lock_subject(&mut transaction, input.issuer, input.subject).await?;
        let linked = sqlx::query(
            "SELECT u.id, u.email, u.display_name, u.role::text AS role, u.active, \
                    u.password_hash IS NULL AS oidc_only \
             FROM oidc_identities oi JOIN users u ON u.id = oi.user_id \
             WHERE oi.issuer = $1 AND oi.subject = $2 FOR UPDATE OF u",
        )
        .bind(input.issuer)
        .bind(input.subject)
        .fetch_optional(&mut *transaction)
        .await?;

        let user = if let Some(row) = linked {
            if !row.get::<bool, _>("active") {
                return Err(OidcError::InactiveUser);
            }
            let mut user = authenticated_user_from_row(&row)?;
            if row.get::<bool, _>("oidc_only") {
                let mapped_role = input
                    .provisioning_role
                    .ok_or(OidcError::ProvisioningDenied)?;
                if user.role != mapped_role {
                    sqlx::query(
                        "UPDATE users SET role = CAST($2 AS user_role), etag = $3, updated_at = $4 \
                         WHERE id = $1",
                    )
                    .bind(user.id)
                    .bind(mapped_role.as_str())
                    .bind(Uuid::now_v7())
                    .bind(now)
                    .execute(&mut *transaction)
                    .await?;
                    insert_audit(
                        &mut transaction,
                        Some(user.id),
                        "user.role_sync_oidc",
                        "user",
                        &user.id.to_string(),
                        now,
                    )
                    .await?;
                    user.role = mapped_role;
                }
            }
            user
        } else {
            let role = input
                .provisioning_role
                .ok_or(OidcError::ProvisioningDenied)?;
            let email = normalize_email(input.email.ok_or(OidcError::ProvisioningDenied)?)?;
            lock_email(&mut transaction, &email).await?;
            let collision: bool =
                sqlx::query_scalar("SELECT EXISTS (SELECT 1 FROM users WHERE email = $1)")
                    .bind(&email)
                    .fetch_one(&mut *transaction)
                    .await?;
            if collision {
                return Err(OidcError::LinkRequired);
            }
            let user_id = Uuid::now_v7();
            let display_name = normalize_display_name(input.display_name, &email);
            sqlx::query(
                "INSERT INTO users \
                 (id, email, display_name, password_hash, role, active, etag, created_at, updated_at) \
                 VALUES ($1, $2, $3, NULL, CAST($4 AS user_role), true, $5, $6, $6)",
            )
            .bind(user_id)
            .bind(&email)
            .bind(&display_name)
            .bind(role.as_str())
            .bind(Uuid::now_v7())
            .bind(now)
            .execute(&mut *transaction)
            .await?;
            sqlx::query(
                "INSERT INTO oidc_identities \
                 (issuer, subject, user_id, email_at_link, last_login_at, created_at) \
                 VALUES ($1, $2, $3, $4, $5, $5)",
            )
            .bind(input.issuer)
            .bind(input.subject)
            .bind(user_id)
            .bind(&email)
            .bind(now)
            .execute(&mut *transaction)
            .await?;
            insert_audit(
                &mut transaction,
                Some(user_id),
                "user.create_oidc",
                "user",
                &user_id.to_string(),
                now,
            )
            .await?;
            OidcAuthenticatedUser {
                id: user_id,
                email,
                display_name,
                role,
            }
        };

        sqlx::query(
            "UPDATE oidc_identities SET last_login_at = $3 \
             WHERE issuer = $1 AND subject = $2",
        )
        .bind(input.issuer)
        .bind(input.subject)
        .bind(now)
        .execute(&mut *transaction)
        .await?;
        insert_session(&mut transaction, user.id, input.session, expires_at, now).await?;
        insert_audit(
            &mut transaction,
            Some(user.id),
            "oidc.login",
            "session",
            &user.id.to_string(),
            now,
        )
        .await?;
        transaction.commit().await?;
        Ok(user)
    }

    pub async fn complete_oidc_link(
        &self,
        input: CompleteOidcLink<'_>,
    ) -> Result<OidcAuthenticatedUser, OidcError> {
        validate_subject(input.subject)?;
        let now = Utc::now();
        let expires_at = checked_session_expiry(now, input.session_ttl)?;
        let mut transaction = self.pool().begin().await?;
        require_current_enabled_configuration(
            &mut transaction,
            input.configuration_id,
            input.configuration_etag,
        )
        .await?;
        lock_subject(&mut transaction, input.issuer, input.subject).await?;
        let user_row = sqlx::query(
            "SELECT id, email, display_name, role::text AS role, active \
             FROM users WHERE id = $1 FOR UPDATE",
        )
        .bind(input.user_id)
        .fetch_optional(&mut *transaction)
        .await?
        .ok_or(OidcError::InactiveUser)?;
        if !user_row.get::<bool, _>("active") {
            return Err(OidcError::InactiveUser);
        }
        let existing_subject =
            sqlx::query("SELECT user_id FROM oidc_identities WHERE issuer = $1 AND subject = $2")
                .bind(input.issuer)
                .bind(input.subject)
                .fetch_optional(&mut *transaction)
                .await?;
        if let Some(existing) = existing_subject
            && existing.get::<Uuid, _>("user_id") != input.user_id
        {
            return Err(OidcError::IdentityAlreadyLinked);
        }
        let other_identity: bool = sqlx::query_scalar(
            "SELECT EXISTS (SELECT 1 FROM oidc_identities WHERE issuer = $1 AND user_id = $2 AND subject <> $3)",
        )
        .bind(input.issuer)
        .bind(input.user_id)
        .bind(input.subject)
        .fetch_one(&mut *transaction)
        .await?;
        if other_identity {
            return Err(OidcError::IdentityAlreadyLinked);
        }
        let normalized_claim_email = input.email.and_then(|value| normalize_email(value).ok());
        sqlx::query(
            "INSERT INTO oidc_identities \
             (issuer, subject, user_id, email_at_link, last_login_at, created_at) \
             VALUES ($1, $2, $3, $4, $5, $5) \
             ON CONFLICT (issuer, subject) DO UPDATE SET last_login_at = EXCLUDED.last_login_at",
        )
        .bind(input.issuer)
        .bind(input.subject)
        .bind(input.user_id)
        .bind(normalized_claim_email)
        .bind(now)
        .execute(&mut *transaction)
        .await?;
        insert_session(
            &mut transaction,
            input.user_id,
            input.session,
            expires_at,
            now,
        )
        .await?;
        insert_audit(
            &mut transaction,
            Some(input.user_id),
            "oidc.identity_link",
            "user",
            &input.user_id.to_string(),
            now,
        )
        .await?;
        transaction.commit().await?;
        authenticated_user_from_row(&user_row)
    }

    /// Returns every identity linked to a local user. A configuration can move
    /// to a new issuer over its lifetime, so this intentionally returns a list
    /// instead of pretending the historical relationship is singular.
    pub async fn oidc_identities_for_user(
        &self,
        user_id: Uuid,
    ) -> Result<Vec<OidcIdentityRecord>, OidcError> {
        let rows = sqlx::query(
            "SELECT oi.id, oi.issuer, oi.email_at_link, oi.last_login_at, oi.created_at, \
                    (u.password_hash IS NOT NULL OR count(*) OVER () > 1) AS can_unlink \
             FROM oidc_identities oi \
             JOIN users u ON u.id = oi.user_id \
             WHERE oi.user_id = $1 \
             ORDER BY oi.created_at, oi.id",
        )
        .bind(user_id)
        .fetch_all(self.pool())
        .await?;
        Ok(rows
            .into_iter()
            .map(|row| OidcIdentityRecord {
                id: row.get("id"),
                issuer: row.get("issuer"),
                email_at_link: row.get("email_at_link"),
                last_login_at: row.get("last_login_at"),
                created_at: row.get("created_at"),
                can_unlink: row.get("can_unlink"),
            })
            .collect())
    }

    /// Removes a linked identity only when another authentication method will
    /// remain. The user row and all of its identities are locked so concurrent
    /// unlink requests cannot strand an OIDC-only account.
    pub async fn unlink_oidc_identity(
        &self,
        user_id: Uuid,
        identity_id: Uuid,
    ) -> Result<(), OidcError> {
        let now = Utc::now();
        let mut transaction = self.pool().begin().await?;
        let user = sqlx::query(
            "SELECT password_hash IS NOT NULL AS has_local_password \
             FROM users WHERE id = $1 AND active FOR UPDATE",
        )
        .bind(user_id)
        .fetch_optional(&mut *transaction)
        .await?
        .ok_or(OidcError::InactiveUser)?;
        let identities =
            sqlx::query("SELECT id FROM oidc_identities WHERE user_id = $1 FOR UPDATE")
                .bind(user_id)
                .fetch_all(&mut *transaction)
                .await?;
        if !identities
            .iter()
            .any(|identity| identity.get::<Uuid, _>("id") == identity_id)
        {
            return Err(OidcError::IdentityNotFound);
        }
        if !user.get::<bool, _>("has_local_password") && identities.len() <= 1 {
            return Err(OidcError::LastAuthenticationMethod);
        }
        let deleted = sqlx::query("DELETE FROM oidc_identities WHERE id = $1 AND user_id = $2")
            .bind(identity_id)
            .bind(user_id)
            .execute(&mut *transaction)
            .await?;
        if deleted.rows_affected() != 1 {
            return Err(OidcError::IdentityNotFound);
        }
        insert_audit(
            &mut transaction,
            Some(user_id),
            "oidc.identity_unlink",
            "oidc_identity",
            &identity_id.to_string(),
            now,
        )
        .await?;
        transaction.commit().await?;
        Ok(())
    }
}

fn oidc_configuration_from_row(row: sqlx::postgres::PgRow) -> Result<OidcConfiguration, OidcError> {
    let id: Uuid = row.get("id");
    let discovery_url = required_string(&row, "discovery_url")?;
    let authorization_endpoint = required_string(&row, "authorization_endpoint")?;
    let token_endpoint = required_string(&row, "token_endpoint")?;
    let jwks_uri = required_string(&row, "jwks_uri")?;
    let ciphertext: Option<Vec<u8>> = row.get("encrypted_client_secret");
    let nonce: Option<Vec<u8>> = row.get("secret_nonce");
    let key_version: Option<i32> = row.get("secret_key_version");
    let encrypted_client_secret = encrypted_from_row(
        key_version.ok_or(OidcError::Corrupt)?,
        nonce.ok_or(OidcError::Corrupt)?,
        ciphertext.ok_or(OidcError::Corrupt)?,
    )?;
    Ok(OidcConfiguration {
        id,
        discovery_url,
        issuer: row.get("issuer"),
        authorization_endpoint,
        token_endpoint,
        jwks_uri,
        token_endpoint_auth_method: row.get("token_endpoint_auth_method"),
        client_id: row.get("client_id"),
        encrypted_client_secret,
        scopes: row.get("scopes"),
        email_claim: row.get("email_claim"),
        groups_claim: row.get("groups_claim"),
        default_role: row
            .get::<Option<String>, _>("default_role")
            .map(|value| value.parse().map_err(|_| OidcError::Corrupt))
            .transpose()?,
        email_role_mappings: mappings_from_json(row.get("email_mappings"))?,
        group_role_mappings: mappings_from_json(row.get("group_mappings"))?,
        enabled: row.get("enabled"),
        etag: row.get("etag"),
        created_at: row.get("created_at"),
        updated_at: row.get("updated_at"),
    })
}

fn validate_configuration(input: &UpsertOidcConfiguration) -> Result<(), OidcError> {
    if input.client_id.trim().is_empty()
        || input.client_id.len() > 512
        || input.client_id.chars().any(char::is_control)
    {
        return Err(OidcError::Invalid(
            "client_id must contain 1-512 characters".to_owned(),
        ));
    }
    if !matches!(
        input.token_endpoint_auth_method.as_str(),
        "client_secret_basic" | "client_secret_post"
    ) {
        return Err(OidcError::Invalid(
            "unsupported token endpoint authentication method".to_owned(),
        ));
    }
    if input.scopes.is_empty()
        || input.scopes.len() > 20
        || !input.scopes.iter().any(|scope| scope == "openid")
        || input.scopes.iter().any(|scope| {
            scope.is_empty()
                || scope.len() > 128
                || !scope.bytes().all(|byte| byte.is_ascii_graphic())
        })
    {
        return Err(OidcError::Invalid(
            "scopes must be URL-safe and include openid".to_owned(),
        ));
    }
    if !valid_claim_name(&input.email_claim) || !valid_claim_name(&input.groups_claim) {
        return Err(OidcError::Invalid("claim names are invalid".to_owned()));
    }
    validate_mappings(&input.email_role_mappings, true)?;
    validate_mappings(&input.group_role_mappings, false)?;
    Ok(())
}

fn validate_mappings(mappings: &[OidcRoleMapping], email: bool) -> Result<(), OidcError> {
    if mappings.len() > MAX_MAPPINGS {
        return Err(OidcError::Invalid("too many role mappings".to_owned()));
    }
    let mut seen = BTreeMap::new();
    for mapping in mappings {
        let value = if email {
            normalize_email(&mapping.claim_value)?
        } else {
            let value = mapping.claim_value.trim();
            if value.is_empty() || value.len() > 256 || value.chars().any(char::is_control) {
                return Err(OidcError::Invalid("group mapping is invalid".to_owned()));
            }
            value.to_owned()
        };
        if seen.insert(value, mapping.role).is_some() {
            return Err(OidcError::Invalid(
                "role mappings contain a duplicate".to_owned(),
            ));
        }
    }
    Ok(())
}

async fn insert_mappings(
    transaction: &mut Transaction<'_, Postgres>,
    configuration_id: Uuid,
    mappings: &[OidcRoleMapping],
    email: bool,
) -> Result<(), OidcError> {
    for mapping in mappings {
        let value = if email {
            normalize_email(&mapping.claim_value)?
        } else {
            mapping.claim_value.trim().to_owned()
        };
        let statement = if email {
            "INSERT INTO oidc_email_role_mappings (configuration_id, email, role) \
             VALUES ($1, $2, CAST($3 AS user_role))"
        } else {
            "INSERT INTO oidc_group_role_mappings (configuration_id, group_name, role) \
             VALUES ($1, $2, CAST($3 AS user_role))"
        };
        sqlx::query(statement)
            .bind(configuration_id)
            .bind(value)
            .bind(mapping.role.as_str())
            .execute(&mut **transaction)
            .await?;
    }
    Ok(())
}

fn mappings_from_json(value: serde_json::Value) -> Result<Vec<OidcRoleMapping>, OidcError> {
    value
        .as_array()
        .ok_or(OidcError::Corrupt)?
        .iter()
        .map(|row| {
            Ok(OidcRoleMapping {
                claim_value: row
                    .get("claim_value")
                    .and_then(serde_json::Value::as_str)
                    .ok_or(OidcError::Corrupt)?
                    .to_owned(),
                role: row
                    .get("role")
                    .and_then(serde_json::Value::as_str)
                    .ok_or(OidcError::Corrupt)?
                    .parse()
                    .map_err(|_| OidcError::Corrupt)?,
            })
        })
        .collect()
}

fn encrypted_from_row(
    key_version: i32,
    nonce: Vec<u8>,
    ciphertext: Vec<u8>,
) -> Result<EncryptedSecret, OidcError> {
    Ok(EncryptedSecret {
        key_version: u32::try_from(key_version).map_err(|_| OidcError::Corrupt)?,
        nonce: nonce.try_into().map_err(|_| OidcError::Corrupt)?,
        ciphertext,
    })
}

fn required_string(row: &sqlx::postgres::PgRow, name: &str) -> Result<String, OidcError> {
    row.get::<Option<String>, _>(name)
        .filter(|value| !value.is_empty())
        .ok_or(OidcError::Corrupt)
}

fn authenticated_user_from_row(
    row: &sqlx::postgres::PgRow,
) -> Result<OidcAuthenticatedUser, OidcError> {
    Ok(OidcAuthenticatedUser {
        id: row.get("id"),
        email: row.get("email"),
        display_name: row.get("display_name"),
        role: row
            .get::<String, _>("role")
            .parse()
            .map_err(|_| OidcError::Corrupt)?,
    })
}

async fn require_current_enabled_configuration(
    transaction: &mut Transaction<'_, Postgres>,
    configuration_id: Uuid,
    configuration_etag: Uuid,
) -> Result<(), OidcError> {
    sqlx::query("SELECT pg_advisory_xact_lock($1)")
        .bind(OIDC_CONFIGURATION_LOCK_ID)
        .execute(&mut **transaction)
        .await?;
    let enabled: Option<bool> =
        sqlx::query_scalar("SELECT enabled FROM oidc_configurations WHERE id = $1 AND etag = $2")
            .bind(configuration_id)
            .bind(configuration_etag)
            .fetch_optional(&mut **transaction)
            .await?;
    match enabled {
        Some(true) => {
            sqlx::query("SELECT set_config('olp.oidc_configuration_etag', $1, true)")
                .bind(configuration_etag.to_string())
                .execute(&mut **transaction)
                .await?;
            Ok(())
        }
        Some(false) => Err(OidcError::Disabled),
        None => Err(OidcError::PreconditionFailed),
    }
}

async fn lock_email(
    transaction: &mut Transaction<'_, Postgres>,
    email: &str,
) -> Result<(), OidcError> {
    sqlx::query("SELECT pg_advisory_xact_lock(hashtextextended($1, $2))")
        .bind(email)
        .bind(OIDC_CONFIGURATION_LOCK_ID)
        .execute(&mut **transaction)
        .await?;
    Ok(())
}

async fn lock_subject(
    transaction: &mut Transaction<'_, Postgres>,
    issuer: &str,
    subject: &str,
) -> Result<(), OidcError> {
    sqlx::query("SELECT pg_advisory_xact_lock(hashtextextended($1, $2))")
        .bind(format!("{}:{issuer}:{subject}", issuer.len()))
        .bind(OIDC_CONFIGURATION_LOCK_ID ^ 0x5355_424a)
        .execute(&mut **transaction)
        .await?;
    Ok(())
}

async fn insert_session(
    transaction: &mut Transaction<'_, Postgres>,
    user_id: Uuid,
    material: &SessionMaterial,
    expires_at: DateTime<Utc>,
    now: DateTime<Utc>,
) -> Result<(), OidcError> {
    sqlx::query(
        "INSERT INTO sessions \
         (id, user_id, token_digest, csrf_digest, expires_at, last_seen_at, created_at) \
         VALUES ($1, $2, $3, $4, $5, $6, $6)",
    )
    .bind(Uuid::now_v7())
    .bind(user_id)
    .bind(material.token_digest().to_vec())
    .bind(material.csrf_digest().to_vec())
    .bind(expires_at)
    .bind(now)
    .execute(&mut **transaction)
    .await?;
    Ok(())
}

async fn insert_audit(
    transaction: &mut Transaction<'_, Postgres>,
    actor_user_id: Option<Uuid>,
    action: &str,
    resource_type: &str,
    resource_id: &str,
    now: DateTime<Utc>,
) -> Result<(), OidcError> {
    sqlx::query(
        "INSERT INTO audit_events \
         (id, actor_user_id, action, resource_type, resource_id, outcome, occurred_at) \
         VALUES ($1, $2, $3, $4, $5, 'success', $6)",
    )
    .bind(Uuid::now_v7())
    .bind(actor_user_id)
    .bind(action)
    .bind(resource_type)
    .bind(resource_id)
    .bind(now)
    .execute(&mut **transaction)
    .await?;
    Ok(())
}

fn checked_session_expiry(now: DateTime<Utc>, ttl: Duration) -> Result<DateTime<Utc>, OidcError> {
    if ttl <= Duration::zero() {
        return Err(OidcError::Invalid("session lifetime is invalid".to_owned()));
    }
    now.checked_add_signed(ttl)
        .ok_or_else(|| OidcError::Invalid("session lifetime is invalid".to_owned()))
}

fn normalize_email(email: &str) -> Result<String, OidcError> {
    let email = email.trim().to_lowercase();
    if email.len() > 254
        || !email.contains('@')
        || email.starts_with('@')
        || email.ends_with('@')
        || email.chars().any(char::is_control)
    {
        return Err(OidcError::Invalid("email is invalid".to_owned()));
    }
    Ok(email)
}

fn normalize_display_name(display_name: Option<&str>, email: &str) -> String {
    let candidate = display_name.unwrap_or_default().trim();
    if candidate.is_empty() {
        email
            .split('@')
            .next()
            .unwrap_or(email)
            .chars()
            .take(100)
            .collect()
    } else {
        candidate.chars().take(100).collect()
    }
}

fn validate_subject(subject: &str) -> Result<(), OidcError> {
    if subject.is_empty() || subject.len() > 255 || subject.chars().any(char::is_control) {
        Err(OidcError::Invalid("OIDC subject is invalid".to_owned()))
    } else {
        Ok(())
    }
}

fn valid_claim_name(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= 128
        && value
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'_' | b'.' | b':' | b'-'))
}

const fn role_rank(role: Role) -> u8 {
    match role {
        Role::Owner => 0,
        Role::Operator => 1,
        Role::Developer => 2,
        Role::Viewer => 3,
    }
}

fn token_digest(value: &str) -> [u8; 32] {
    Sha256::digest(value.as_bytes()).into()
}

fn random_token() -> Zeroizing<String> {
    let mut bytes = [0_u8; 32];
    rand::rng().fill_bytes(&mut bytes);
    let token = Zeroizing::new(URL_SAFE_NO_PAD.encode(bytes));
    bytes.zeroize();
    token
}

#[cfg(test)]
mod tests {
    use super::*;

    fn mapping(value: &str, role: Role) -> OidcRoleMapping {
        OidcRoleMapping {
            claim_value: value.to_owned(),
            role,
        }
    }

    #[test]
    fn flow_material_has_s256_challenge_and_redacted_debug() {
        let material = OidcFlowMaterial::generate();
        assert_eq!(material.state().len(), 43);
        assert_eq!(material.browser_binding().len(), 43);
        assert_eq!(material.nonce().len(), 43);
        assert_eq!(material.pkce_verifier().len(), 43);
        assert_eq!(material.pkce_challenge().len(), 43);
        assert!(!format!("{material:?}").contains(material.state()));
    }

    #[test]
    fn mapping_precedence_is_exact_email_then_strongest_group_then_default() {
        let configuration = OidcConfiguration {
            id: Uuid::now_v7(),
            discovery_url: "https://idp.example/.well-known/openid-configuration".to_owned(),
            issuer: "https://idp.example".to_owned(),
            authorization_endpoint: "https://idp.example/authorize".to_owned(),
            token_endpoint: "https://idp.example/token".to_owned(),
            jwks_uri: "https://idp.example/jwks".to_owned(),
            token_endpoint_auth_method: "client_secret_basic".to_owned(),
            client_id: "olp".to_owned(),
            encrypted_client_secret: EncryptedSecret {
                key_version: 1,
                nonce: [0; 12],
                ciphertext: vec![0; 16],
            },
            scopes: vec!["openid".to_owned()],
            email_claim: "email".to_owned(),
            groups_claim: "groups".to_owned(),
            default_role: Some(Role::Viewer),
            email_role_mappings: vec![mapping("owner@example.test", Role::Owner)],
            group_role_mappings: vec![
                mapping("engineering", Role::Developer),
                mapping("operations", Role::Operator),
            ],
            enabled: true,
            etag: Uuid::now_v7(),
            created_at: Utc::now(),
            updated_at: Utc::now(),
        };
        assert_eq!(
            configuration.mapped_role("OWNER@example.test", &["engineering".to_owned()]),
            Some(Role::Owner)
        );
        assert_eq!(
            configuration.mapped_role(
                "person@example.test",
                &["engineering".to_owned(), "operations".to_owned()]
            ),
            Some(Role::Operator)
        );
        assert_eq!(
            configuration.mapped_role("person@example.test", &[]),
            Some(Role::Viewer)
        );
    }
}
