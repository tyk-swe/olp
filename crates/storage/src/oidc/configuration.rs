use std::collections::BTreeMap;

use chrono::{DateTime, Utc};
use sqlx::{FromRow, Postgres, Transaction};
use uuid::Uuid;

use super::helpers::{encrypted_from_row, normalize_email, required_string, valid_claim_name};
use super::{OidcConfiguration, OidcError, OidcRoleMapping, UpsertOidcConfiguration};
use crate::PgStore;

pub(super) const OIDC_CONFIGURATION_LOCK_ID: i64 = 0x4f4c_505f_4f49; // "OLP_OI"
const MAX_MAPPINGS: usize = 500;

impl PgStore {
    pub async fn oidc_configuration(&self) -> Result<Option<OidcConfiguration>, OidcError> {
        let row = sqlx::query_as!(
            OidcConfigurationRow,
            "SELECT id, discovery_url, issuer, authorization_endpoint, token_endpoint, jwks_uri, \
                    token_endpoint_auth_method, client_id, encrypted_client_secret, secret_nonce, \
                    secret_key_version, scopes, email_claim, groups_claim, default_role::text AS \"default_role?\", \
                    enabled, etag, created_at, updated_at, \
                    COALESCE((SELECT jsonb_agg(jsonb_build_object( \
                        'claim_value', mapping.email, 'role', mapping.role::text) ORDER BY mapping.email) \
                        FROM oidc_email_role_mappings mapping WHERE mapping.configuration_id = oidc_configurations.id), \
                        '[]'::jsonb) AS \"email_mappings!\", \
                    COALESCE((SELECT jsonb_agg(jsonb_build_object( \
                        'claim_value', mapping.group_name, 'role', mapping.role::text) ORDER BY mapping.group_name) \
                        FROM oidc_group_role_mappings mapping WHERE mapping.configuration_id = oidc_configurations.id), \
                        '[]'::jsonb) AS \"group_mappings!\" \
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
        sqlx::query!(
            "SELECT pg_advisory_xact_lock($1)",
            OIDC_CONFIGURATION_LOCK_ID
        )
        .fetch_one(&mut *transaction)
        .await?;
        let current =
            sqlx::query!("SELECT id, etag FROM oidc_configurations WHERE singleton FOR UPDATE")
                .fetch_optional(&mut *transaction)
                .await?;
        match current {
            Some(row) => {
                let current_id: Uuid = row.id;
                let current_etag: Uuid = row.etag;
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
        sqlx::query!(
            "INSERT INTO oidc_configurations \
             (id, singleton, discovery_url, issuer, authorization_endpoint, token_endpoint, jwks_uri, \
              token_endpoint_auth_method, client_id, encrypted_client_secret, secret_nonce, \
              secret_key_version, scopes, email_claim, groups_claim, default_role, enabled, etag, \
              updated_by, created_at, updated_at) \
             VALUES ($1, true, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, \
                     CAST($15::text AS user_role), $16, $17, $18, $19, $19) \
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
        input.id, input.discovery_url.trim(), input.issuer.trim(), input.authorization_endpoint.trim(), input.token_endpoint.trim(), input.jwks_uri.trim(), &input.token_endpoint_auth_method, input.client_id.trim(), &input.encrypted_client_secret.ciphertext, input.encrypted_client_secret.nonce.to_vec(), i32::try_from(input.encrypted_client_secret.key_version).map_err(|_| OidcError::Corrupt)?, &input.scopes, &input.email_claim, &input.groups_claim, input.default_role.map(|role| role.as_str()), input.enabled, etag, input.actor_user_id, now)
        .execute(&mut *transaction)
        .await?;

        sqlx::query!(
            "DELETE FROM oidc_email_role_mappings WHERE configuration_id = $1",
            input.id
        )
        .execute(&mut *transaction)
        .await?;
        sqlx::query!(
            "DELETE FROM oidc_group_role_mappings WHERE configuration_id = $1",
            input.id
        )
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
        sqlx::query!(
            "DELETE FROM oidc_authorization_flows WHERE configuration_id = $1",
            input.id
        )
        .execute(&mut *transaction)
        .await?;
        sqlx::query!(
            "INSERT INTO audit_events \
             (id, actor_user_id, action, resource_type, resource_id, outcome, occurred_at) \
             VALUES ($1, $2, 'oidc.configuration_update', 'oidc_configuration', $3, 'success', $4)",
            Uuid::now_v7(),
            input.actor_user_id,
            input.id.to_string(),
            now
        )
        .execute(&mut *transaction)
        .await?;
        transaction.commit().await?;
        self.oidc_configuration().await?.ok_or(OidcError::Corrupt)
    }
}

#[derive(Debug, FromRow)]
struct OidcConfigurationRow {
    id: Uuid,
    discovery_url: Option<String>,
    issuer: String,
    authorization_endpoint: Option<String>,
    token_endpoint: Option<String>,
    jwks_uri: Option<String>,
    token_endpoint_auth_method: String,
    client_id: String,
    encrypted_client_secret: Option<Vec<u8>>,
    secret_nonce: Option<Vec<u8>>,
    secret_key_version: Option<i32>,
    scopes: Vec<String>,
    email_claim: String,
    groups_claim: String,
    default_role: Option<String>,
    enabled: bool,
    etag: Uuid,
    created_at: DateTime<Utc>,
    updated_at: DateTime<Utc>,
    email_mappings: serde_json::Value,
    group_mappings: serde_json::Value,
}

fn oidc_configuration_from_row(row: OidcConfigurationRow) -> Result<OidcConfiguration, OidcError> {
    let id: Uuid = row.id;
    let discovery_url = required_string(row.discovery_url, "discovery_url")?;
    let authorization_endpoint =
        required_string(row.authorization_endpoint, "authorization_endpoint")?;
    let token_endpoint = required_string(row.token_endpoint, "token_endpoint")?;
    let jwks_uri = required_string(row.jwks_uri, "jwks_uri")?;
    let ciphertext: Option<Vec<u8>> = row.encrypted_client_secret;
    let nonce: Option<Vec<u8>> = row.secret_nonce;
    let key_version: Option<i32> = row.secret_key_version;
    let encrypted_client_secret = encrypted_from_row(
        key_version.ok_or(OidcError::Corrupt)?,
        nonce.ok_or(OidcError::Corrupt)?,
        ciphertext.ok_or(OidcError::Corrupt)?,
    )?;
    Ok(OidcConfiguration {
        id,
        discovery_url,
        issuer: row.issuer,
        authorization_endpoint,
        token_endpoint,
        jwks_uri,
        token_endpoint_auth_method: row.token_endpoint_auth_method,
        client_id: row.client_id,
        encrypted_client_secret,
        scopes: row.scopes,
        email_claim: row.email_claim,
        groups_claim: row.groups_claim,
        default_role: row
            .default_role
            .map(|value| value.parse().map_err(|_| OidcError::Corrupt))
            .transpose()?,
        email_role_mappings: mappings_from_json(row.email_mappings)?,
        group_role_mappings: mappings_from_json(row.group_mappings)?,
        enabled: row.enabled,
        etag: row.etag,
        created_at: row.created_at,
        updated_at: row.updated_at,
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
        if email {
            sqlx::query!(
                "INSERT INTO oidc_email_role_mappings (configuration_id, email, role) \
                 VALUES ($1, $2, CAST(CAST($3 AS text) AS user_role))",
                configuration_id,
                value,
                mapping.role.as_str(),
            )
            .execute(&mut **transaction)
            .await?;
        } else {
            sqlx::query!(
                "INSERT INTO oidc_group_role_mappings (configuration_id, group_name, role) \
                 VALUES ($1, $2, CAST(CAST($3 AS text) AS user_role))",
                configuration_id,
                value,
                mapping.role.as_str(),
            )
            .execute(&mut **transaction)
            .await?;
        }
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
