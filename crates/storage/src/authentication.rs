use chrono::{DateTime, Duration, Utc};
use sqlx::{Postgres, Transaction};
use uuid::Uuid;

use crate::{PersistenceError, PgStore, RecentAuthMaterial, SessionMaterial};

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum RecentAuthPurpose {
    PasswordEnrollment,
    OidcLink,
    OidcUnlink,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct SessionSecurityContext {
    pub session_id: Uuid,
    pub user_id: Uuid,
    pub security_version: i64,
}

impl RecentAuthPurpose {
    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::PasswordEnrollment => "password_enrollment",
            Self::OidcLink => "oidc_link",
            Self::OidcUnlink => "oidc_unlink",
        }
    }

    #[must_use]
    pub const fn requires_resource(self) -> bool {
        matches!(self, Self::OidcUnlink)
    }

    pub fn parse(value: &str) -> Option<Self> {
        match value {
            "password_enrollment" => Some(Self::PasswordEnrollment),
            "oidc_link" => Some(Self::OidcLink),
            "oidc_unlink" => Some(Self::OidcUnlink),
            _ => None,
        }
    }

    #[must_use]
    pub const fn audit_action(self) -> &'static str {
        match self {
            Self::PasswordEnrollment => "authentication.recent_for_password_enrollment",
            Self::OidcLink => "authentication.recent_for_oidc_link",
            Self::OidcUnlink => "authentication.recent_for_oidc_unlink",
        }
    }
}

impl PgStore {
    /// Installs one short-lived recent-authentication grant on an exact active
    /// session. Issuing another grant replaces the previous one; consumption
    /// atomically clears all grant fields.
    pub async fn issue_recent_authentication(
        &self,
        context: SessionSecurityContext,
        purpose: RecentAuthPurpose,
        resource_id: Option<Uuid>,
        material: &RecentAuthMaterial,
        ttl: Duration,
    ) -> Result<bool, PersistenceError> {
        if resource_id.is_some() != purpose.requires_resource()
            || ttl <= Duration::zero()
            || ttl > Duration::minutes(15)
        {
            return Err(PersistenceError::InvalidRecentAuthentication);
        }
        let now = Utc::now();
        let expires_at = now
            .checked_add_signed(ttl)
            .ok_or(PersistenceError::InvalidRecentAuthentication)?;
        let mut transaction = self.pool().begin().await?;
        let installed = install_recent_authentication(
            &mut transaction,
            context,
            purpose,
            resource_id,
            material,
            expires_at,
            now,
        )
        .await?;
        if installed {
            transaction.commit().await?;
        } else {
            transaction.rollback().await?;
        }
        Ok(installed)
    }

    pub async fn user_has_local_password(
        &self,
        user_id: Uuid,
    ) -> Result<Option<bool>, PersistenceError> {
        sqlx::query_scalar!(
            "SELECT password_hash IS NOT NULL AS \"value!\" FROM users WHERE id = $1 AND active",
            user_id
        )
        .fetch_optional(self.pool())
        .await
        .map_err(Into::into)
    }
}

pub(crate) async fn install_recent_authentication(
    transaction: &mut Transaction<'_, Postgres>,
    context: SessionSecurityContext,
    purpose: RecentAuthPurpose,
    resource_id: Option<Uuid>,
    material: &RecentAuthMaterial,
    expires_at: DateTime<Utc>,
    now: DateTime<Utc>,
) -> Result<bool, sqlx::Error> {
    if resource_id.is_some() != purpose.requires_resource() || expires_at <= now {
        return Ok(false);
    }
    let updated = sqlx::query!(
        "UPDATE sessions session SET \
             recent_auth_token_digest = $5, recent_auth_purpose = $6, \
             recent_auth_resource_id = $7, recent_auth_expires_at = $8 \
         WHERE session.id = $1 AND session.user_id = $2 \
           AND session.security_version = $3 AND session.expires_at > $4 \
           AND EXISTS ( \
               SELECT 1 FROM users \
               WHERE users.id = session.user_id AND users.active \
                 AND users.security_version = session.security_version \
           )",
        context.session_id,
        context.user_id,
        context.security_version,
        now,
        material.token_digest().to_vec(),
        purpose.as_str(),
        resource_id,
        expires_at
    )
    .execute(&mut **transaction)
    .await?
    .rows_affected();
    if updated == 1 {
        insert_security_audit(
            transaction,
            context.user_id,
            purpose.audit_action(),
            "session",
            &context.session_id.to_string(),
            now,
        )
        .await?;
        Ok(true)
    } else {
        Ok(false)
    }
}

pub(crate) async fn consume_recent_authentication(
    transaction: &mut Transaction<'_, Postgres>,
    session_id: Uuid,
    user_id: Uuid,
    security_version: i64,
    purpose: RecentAuthPurpose,
    resource_id: Option<Uuid>,
    token_digest: [u8; 32],
) -> Result<bool, sqlx::Error> {
    if resource_id.is_some() != purpose.requires_resource() {
        return Ok(false);
    }
    let consumed = sqlx::query!(
        "UPDATE sessions session SET \
             recent_auth_token_digest = NULL, recent_auth_purpose = NULL, \
             recent_auth_resource_id = NULL, recent_auth_expires_at = NULL \
         WHERE session.id = $1 AND session.user_id = $2 \
           AND session.security_version = $3 AND session.expires_at > now() \
           AND session.recent_auth_token_digest = $4 \
           AND session.recent_auth_purpose = $5 \
           AND session.recent_auth_resource_id IS NOT DISTINCT FROM $6 \
           AND session.recent_auth_expires_at > now() \
           AND EXISTS ( \
               SELECT 1 FROM users \
               WHERE users.id = session.user_id AND users.active \
                 AND users.security_version = session.security_version \
           )",
        session_id,
        user_id,
        security_version,
        token_digest.to_vec(),
        purpose.as_str(),
        resource_id
    )
    .execute(&mut **transaction)
    .await?
    .rows_affected();
    Ok(consumed == 1)
}

pub(crate) async fn insert_versioned_session(
    transaction: &mut Transaction<'_, Postgres>,
    user_id: Uuid,
    security_version: i64,
    material: &SessionMaterial,
    expires_at: DateTime<Utc>,
    now: DateTime<Utc>,
) -> Result<Uuid, sqlx::Error> {
    let session_id = Uuid::now_v7();
    sqlx::query!(
        "INSERT INTO sessions \
         (id, user_id, security_version, token_digest, csrf_digest, expires_at, \
          last_seen_at, created_at) \
         VALUES ($1, $2, $3, $4, $5, $6, $7, $7)",
        session_id,
        user_id,
        security_version,
        material.token_digest().to_vec(),
        material.csrf_digest().to_vec(),
        expires_at,
        now
    )
    .execute(&mut **transaction)
    .await?;
    Ok(session_id)
}

pub(crate) async fn revoke_user_sessions(
    transaction: &mut Transaction<'_, Postgres>,
    user_id: Uuid,
) -> Result<u64, sqlx::Error> {
    Ok(
        sqlx::query!("DELETE FROM sessions WHERE user_id = $1", user_id)
            .execute(&mut **transaction)
            .await?
            .rows_affected(),
    )
}

pub(crate) async fn insert_security_audit(
    transaction: &mut Transaction<'_, Postgres>,
    actor_user_id: Uuid,
    action: &str,
    resource_type: &str,
    resource_id: &str,
    occurred_at: DateTime<Utc>,
) -> Result<(), sqlx::Error> {
    sqlx::query!(
        "INSERT INTO audit_events \
         (id, actor_user_id, action, resource_type, resource_id, outcome, occurred_at) \
         VALUES ($1, $2, $3, $4, $5, 'success', $6)",
        Uuid::now_v7(),
        actor_user_id,
        action,
        resource_type,
        resource_id,
        occurred_at
    )
    .execute(&mut **transaction)
    .await?;
    Ok(())
}
