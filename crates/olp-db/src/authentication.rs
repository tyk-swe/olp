use std::fmt;

use chrono::{DateTime, Duration, Utc};
use sqlx::{Postgres, Transaction};
use uuid::Uuid;

use crate::{
    audit_events::{AuditEvent, record_audit_event},
    error::Error,
    security::session_material::{RecentAuthMaterial, SessionMaterial},
    store::{RequestProvenance, Store},
};

pub(crate) mod sessions;

#[derive(Clone)]
pub struct SessionPrincipal {
    pub session_id: Uuid,
    pub user_id: Uuid,
    pub email: String,
    pub display_name: String,
    pub role: String,
    pub security_version: i64,
    pub csrf_digest: Vec<u8>,
    pub expires_at: DateTime<Utc>,
}

impl fmt::Debug for SessionPrincipal {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("SessionPrincipal")
            .field("session_id", &self.session_id)
            .field("user_id", &self.user_id)
            .field("email", &self.email)
            .field("display_name", &self.display_name)
            .field("role", &self.role)
            .field("security_version", &self.security_version)
            .field("csrf_digest", &"[REDACTED]")
            .field("expires_at", &self.expires_at)
            .finish()
    }
}

#[derive(Clone)]
pub struct LocalPasswordUser {
    pub id: Uuid,
    pub email: String,
    pub display_name: String,
    pub password_hash: String,
    pub role: String,
}

impl fmt::Debug for LocalPasswordUser {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("LocalPasswordUser")
            .field("id", &self.id)
            .field("email", &self.email)
            .field("display_name", &self.display_name)
            .field("password_hash", &"[REDACTED]")
            .field("role", &self.role)
            .finish()
    }
}

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

impl Store {
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
    ) -> Result<bool, Error> {
        if resource_id.is_some() != purpose.requires_resource()
            || ttl <= Duration::zero()
            || ttl > Duration::minutes(15)
        {
            return Err(Error::InvalidRecentAuthentication);
        }
        let now = Utc::now();
        let expires_at = now
            .checked_add_signed(ttl)
            .ok_or(Error::InvalidRecentAuthentication)?;
        let mut transaction = self.pool().begin().await?;
        let installed = install_recent_authentication(
            &mut transaction,
            self.provenance(),
            context,
            RecentAuthGrant {
                purpose,
                resource_id,
                material,
                expires_at,
            },
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

    pub async fn user_has_local_password(&self, user_id: Uuid) -> Result<Option<bool>, Error> {
        sqlx::query_scalar!(
            "SELECT password_hash IS NOT NULL AS \"value!\" FROM users WHERE id = $1 AND active",
            user_id
        )
        .fetch_optional(self.pool())
        .await
        .map_err(Into::into)
    }
}

/// One recent-authentication grant awaiting installation on a session.
pub(crate) struct RecentAuthGrant<'a> {
    pub(crate) purpose: RecentAuthPurpose,
    pub(crate) resource_id: Option<Uuid>,
    pub(crate) material: &'a RecentAuthMaterial,
    pub(crate) expires_at: DateTime<Utc>,
}

pub(crate) async fn install_recent_authentication(
    transaction: &mut Transaction<'_, Postgres>,
    provenance: &RequestProvenance,
    context: SessionSecurityContext,
    grant: RecentAuthGrant<'_>,
    now: DateTime<Utc>,
) -> Result<bool, sqlx::Error> {
    let RecentAuthGrant {
        purpose,
        resource_id,
        material,
        expires_at,
    } = grant;
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
        record_audit_event(
            &mut **transaction,
            AuditEvent {
                provenance,
                actor: Some(context.user_id),
                action: purpose.audit_action(),
                resource_type: "session",
                resource_id: Some(&context.session_id.to_string()),
                outcome: "success",
                occurred_at: Some(now),
            },
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

#[cfg(test)]
mod tests {
    use chrono::Utc;
    use uuid::Uuid;

    use super::{LocalPasswordUser, SessionPrincipal};

    #[test]
    fn sensitive_authentication_records_redact_debug_output() {
        let password = LocalPasswordUser {
            id: Uuid::now_v7(),
            email: "owner@example.test".into(),
            display_name: "Owner".into(),
            password_hash: "secret-hash".into(),
            role: "owner".into(),
        };
        assert!(!format!("{password:?}").contains("secret-hash"));

        let principal = SessionPrincipal {
            session_id: Uuid::now_v7(),
            user_id: Uuid::now_v7(),
            email: "owner@example.test".into(),
            display_name: "Owner".into(),
            role: "owner".into(),
            security_version: 1,
            csrf_digest: vec![1, 2, 3, 4],
            expires_at: Utc::now(),
        };
        assert!(!format!("{principal:?}").contains("1, 2, 3, 4"));
    }
}
