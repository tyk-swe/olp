use chrono::{DateTime, Utc};
use sqlx::Row;
use uuid::Uuid;

use crate::SessionMaterial;

use super::{PasswordUser, PersistenceError, PgStore, SessionPrincipal};

impl PgStore {
    pub async fn create_session(
        &self,
        user_id: Uuid,
        material: &SessionMaterial,
        ttl: chrono::Duration,
    ) -> Result<Uuid, PersistenceError> {
        let id = Uuid::now_v7();
        let now = Utc::now();
        let expires_at = checked_session_expiry(now, ttl)?;
        let mut transaction = self.pool.begin().await?;
        sqlx::query(
            "INSERT INTO sessions \
             (id, user_id, token_digest, csrf_digest, expires_at, last_seen_at, created_at) \
             VALUES ($1, $2, $3, $4, $5, $6, $6)",
        )
        .bind(id)
        .bind(user_id)
        .bind(material.token_digest().to_vec())
        .bind(material.csrf_digest().to_vec())
        .bind(expires_at)
        .bind(now)
        .execute(&mut *transaction)
        .await?;
        sqlx::query(
            "INSERT INTO audit_events \
             (id, actor_user_id, action, resource_type, resource_id, outcome, occurred_at) \
             VALUES ($1, $2, 'session.create', 'session', $3, 'success', $4)",
        )
        .bind(Uuid::now_v7())
        .bind(user_id)
        .bind(id.to_string())
        .bind(now)
        .execute(&mut *transaction)
        .await?;
        sqlx::query(
            "INSERT INTO audit_events \
             (id, actor_user_id, action, resource_type, resource_id, outcome, occurred_at) \
             VALUES ($1, $2, 'local_auth.login', 'session', $3, 'success', $4)",
        )
        .bind(Uuid::now_v7())
        .bind(user_id)
        .bind(id.to_string())
        .bind(now)
        .execute(&mut *transaction)
        .await?;
        transaction.commit().await?;
        Ok(id)
    }

    /// Records a rejected local-password login without retaining the submitted
    /// email, password, headers, or network metadata. A known active local user
    /// may be attached for operator visibility; unknown identities remain
    /// anonymous.
    pub async fn record_local_login_failure(
        &self,
        user_id: Option<Uuid>,
    ) -> Result<(), PersistenceError> {
        sqlx::query(
            "INSERT INTO audit_events \
             (id, actor_user_id, action, resource_type, resource_id, outcome, occurred_at) \
             VALUES ($1, $2, 'local_auth.login', 'session', NULL, 'failure', $3)",
        )
        .bind(Uuid::now_v7())
        .bind(user_id)
        .bind(Utc::now())
        .execute(&self.pool)
        .await?;
        Ok(())
    }

    pub async fn password_user(
        &self,
        email: &str,
    ) -> Result<Option<PasswordUser>, PersistenceError> {
        let row = sqlx::query(
            "SELECT id, email, display_name, password_hash, role::text AS role \
             FROM users WHERE email = $1 AND active AND password_hash IS NOT NULL",
        )
        .bind(email.trim().to_lowercase())
        .fetch_optional(&self.pool)
        .await?;
        Ok(row.map(|row| PasswordUser {
            id: row.get("id"),
            email: row.get("email"),
            display_name: row.get("display_name"),
            password_hash: row.get("password_hash"),
            role: row.get("role"),
        }))
    }

    pub async fn session_principal(
        &self,
        plaintext_token: &str,
    ) -> Result<Option<SessionPrincipal>, PersistenceError> {
        let digest = SessionMaterial::digest_token(plaintext_token);
        let row = sqlx::query(
            "WITH authenticated AS MATERIALIZED ( \
                 SELECT s.id AS session_id, s.csrf_digest, s.expires_at, \
                        u.id AS user_id, u.email, u.display_name, u.role::text AS role \
                 FROM sessions s JOIN users u ON u.id = s.user_id \
                 WHERE s.token_digest = $1 AND s.expires_at > now() AND u.active \
             ), touched AS ( \
                 UPDATE sessions s SET last_seen_at = now() \
                 FROM authenticated authenticated_session \
                 WHERE s.id = authenticated_session.session_id \
                   AND s.expires_at > now() \
                   AND s.last_seen_at <= now() - interval '5 minutes' \
                 RETURNING s.id \
             ) \
             SELECT authenticated.session_id, authenticated.csrf_digest, \
                    authenticated.expires_at, authenticated.user_id, authenticated.email, \
                    authenticated.display_name, authenticated.role \
             FROM authenticated \
             CROSS JOIN (SELECT count(*) FROM touched) activity",
        )
        .bind(digest.to_vec())
        .fetch_optional(&self.pool)
        .await?;

        Ok(row.map(|row| SessionPrincipal {
            session_id: row.get("session_id"),
            user_id: row.get("user_id"),
            email: row.get("email"),
            display_name: row.get("display_name"),
            role: row.get("role"),
            csrf_digest: row.get("csrf_digest"),
            expires_at: row.get("expires_at"),
        }))
    }
}

pub(super) fn checked_session_expiry(
    now: DateTime<Utc>,
    ttl: chrono::Duration,
) -> Result<DateTime<Utc>, PersistenceError> {
    if ttl <= chrono::Duration::zero() {
        return Err(PersistenceError::InvalidSessionTtl);
    }
    now.checked_add_signed(ttl)
        .ok_or(PersistenceError::InvalidSessionTtl)
}
