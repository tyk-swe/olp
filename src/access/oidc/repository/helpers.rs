use crate::access::policy::Role;
use chrono::DateTime;
use chrono::Duration;
use sqlx::Postgres;
use sqlx::Transaction;
use uuid::Uuid;

use crate::access::oidc::repository::configuration::OIDC_CONFIGURATION_LOCK_ID;
use crate::access::oidc::repository::types::OidcAuthenticatedUser;
use crate::access::oidc::repository::types::OidcError;
use crate::crypto::envelope::EncryptedSecret;

pub(crate) fn encrypted_from_row(
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

pub(crate) fn required_string(value: Option<String>) -> Result<String, OidcError> {
    value
        .filter(|value| !value.is_empty())
        .ok_or(OidcError::Corrupt)
}

#[derive(Debug, sqlx::FromRow)]
pub(crate) struct AuthenticatedUserRow {
    pub(crate) id: Uuid,
    pub(crate) email: String,
    pub(crate) display_name: String,
    pub(crate) role: String,
}

pub(crate) fn authenticated_user_from_row(
    row: AuthenticatedUserRow,
) -> Result<OidcAuthenticatedUser, OidcError> {
    Ok(OidcAuthenticatedUser {
        id: row.id,
        email: row.email,
        display_name: row.display_name,
        role: row.role.parse().map_err(|_| OidcError::Corrupt)?,
    })
}

pub(crate) async fn require_current_enabled_configuration(
    transaction: &mut Transaction<'_, Postgres>,
    configuration_id: Uuid,
    configuration_etag: Uuid,
) -> Result<(), OidcError> {
    sqlx::query("SELECT pg_advisory_xact_lock($1)")
        .bind(OIDC_CONFIGURATION_LOCK_ID)
        .fetch_one(&mut **transaction)
        .await?;
    let enabled: Option<bool> = sqlx::query_scalar::<_, bool>(
        "SELECT enabled FROM oidc_configurations WHERE id = $1 AND etag = $2",
    )
    .bind(configuration_id)
    .bind(configuration_etag)
    .fetch_optional(&mut **transaction)
    .await?;
    match enabled {
        Some(true) => {
            sqlx::query_as::<_, RequireCurrentEnabledConfigurationRow>(
                "SELECT set_config('olp.oidc_configuration_etag', $1, true)",
            )
            .bind(configuration_etag.to_string())
            .fetch_one(&mut **transaction)
            .await?;
            Ok(())
        }
        Some(false) => Err(OidcError::Disabled),
        None => Err(OidcError::PreconditionFailed),
    }
}

pub(crate) async fn lock_email(
    transaction: &mut Transaction<'_, Postgres>,
    email: &str,
) -> Result<(), OidcError> {
    sqlx::query("SELECT pg_advisory_xact_lock(hashtextextended($1, $2))")
        .bind(email)
        .bind(OIDC_CONFIGURATION_LOCK_ID)
        .fetch_one(&mut **transaction)
        .await?;
    Ok(())
}

pub(crate) async fn lock_subject(
    transaction: &mut Transaction<'_, Postgres>,
    issuer: &str,
    subject: &str,
) -> Result<(), OidcError> {
    sqlx::query("SELECT pg_advisory_xact_lock(hashtextextended($1, $2))")
        .bind(format!("{}:{issuer}:{subject}", issuer.len()))
        .bind(OIDC_CONFIGURATION_LOCK_ID ^ 0x5355_424a)
        .fetch_one(&mut **transaction)
        .await?;
    Ok(())
}

pub(crate) fn checked_session_expiry(
    now: DateTime<chrono::Utc>,
    ttl: Duration,
) -> Result<DateTime<chrono::Utc>, OidcError> {
    if ttl <= Duration::zero() {
        return Err(OidcError::Invalid("session lifetime is invalid".to_owned()));
    }
    now.checked_add_signed(ttl)
        .ok_or_else(|| OidcError::Invalid("session lifetime is invalid".to_owned()))
}

pub(crate) fn normalize_email(email: &str) -> Result<String, OidcError> {
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

pub(crate) fn normalize_display_name(display_name: Option<&str>, email: &str) -> String {
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

pub(crate) fn validate_subject(subject: &str) -> Result<(), OidcError> {
    if subject.is_empty() || subject.len() > 255 || subject.chars().any(char::is_control) {
        Err(OidcError::Invalid("OIDC subject is invalid".to_owned()))
    } else {
        Ok(())
    }
}

pub(crate) fn valid_claim_name(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= 128
        && value
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'_' | b'.' | b':' | b'-'))
}

pub(crate) const fn role_rank(role: Role) -> u8 {
    match role {
        Role::Owner => 0,
        Role::Operator => 1,
        Role::Developer => 2,
        Role::Viewer => 3,
    }
}

#[derive(sqlx::FromRow)]
struct RequireCurrentEnabledConfigurationRow {}
