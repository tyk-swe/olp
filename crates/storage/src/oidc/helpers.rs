use base64::{Engine as _, engine::general_purpose::URL_SAFE_NO_PAD};
use chrono::{DateTime, Duration};
use olp_domain::Role;
use rand::RngCore;
use sha2::{Digest, Sha256};
use sqlx::{Postgres, Row, Transaction};
use uuid::Uuid;
use zeroize::{Zeroize, Zeroizing};

use super::configuration::OIDC_CONFIGURATION_LOCK_ID;
use super::{OidcAuthenticatedUser, OidcError};
use crate::{EncryptedSecret, SessionMaterial};

pub(super) fn encrypted_from_row(
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

pub(super) fn required_string(
    row: &sqlx::postgres::PgRow,
    name: &str,
) -> Result<String, OidcError> {
    row.get::<Option<String>, _>(name)
        .filter(|value| !value.is_empty())
        .ok_or(OidcError::Corrupt)
}

pub(super) fn authenticated_user_from_row(
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

pub(super) async fn require_current_enabled_configuration(
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

pub(super) async fn lock_email(
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

pub(super) async fn lock_subject(
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

pub(super) async fn insert_session(
    transaction: &mut Transaction<'_, Postgres>,
    user_id: Uuid,
    material: &SessionMaterial,
    expires_at: DateTime<chrono::Utc>,
    now: DateTime<chrono::Utc>,
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

pub(super) async fn insert_audit(
    transaction: &mut Transaction<'_, Postgres>,
    actor_user_id: Option<Uuid>,
    action: &str,
    resource_type: &str,
    resource_id: &str,
    now: DateTime<chrono::Utc>,
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

pub(super) fn checked_session_expiry(
    now: DateTime<chrono::Utc>,
    ttl: Duration,
) -> Result<DateTime<chrono::Utc>, OidcError> {
    if ttl <= Duration::zero() {
        return Err(OidcError::Invalid("session lifetime is invalid".to_owned()));
    }
    now.checked_add_signed(ttl)
        .ok_or_else(|| OidcError::Invalid("session lifetime is invalid".to_owned()))
}

pub(super) fn normalize_email(email: &str) -> Result<String, OidcError> {
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

pub(super) fn normalize_display_name(display_name: Option<&str>, email: &str) -> String {
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

pub(super) fn validate_subject(subject: &str) -> Result<(), OidcError> {
    if subject.is_empty() || subject.len() > 255 || subject.chars().any(char::is_control) {
        Err(OidcError::Invalid("OIDC subject is invalid".to_owned()))
    } else {
        Ok(())
    }
}

pub(super) fn valid_claim_name(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= 128
        && value
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'_' | b'.' | b':' | b'-'))
}

pub(super) const fn role_rank(role: Role) -> u8 {
    match role {
        Role::Owner => 0,
        Role::Operator => 1,
        Role::Developer => 2,
        Role::Viewer => 3,
    }
}

pub(super) fn token_digest(value: &str) -> [u8; 32] {
    Sha256::digest(value.as_bytes()).into()
}

pub(super) fn random_token() -> Zeroizing<String> {
    let mut bytes = [0_u8; 32];
    rand::rng().fill_bytes(&mut bytes);
    let token = Zeroizing::new(URL_SAFE_NO_PAD.encode(bytes));
    bytes.zeroize();
    token
}
