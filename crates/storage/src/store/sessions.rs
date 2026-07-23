use chrono::{DateTime, Utc};
use uuid::Uuid;

use crate::{CsrfMaterial, SessionMaterial, authentication::insert_versioned_session};

use super::{LocalPasswordUser, PersistenceError, PgStore, SessionPrincipal};

impl PgStore {
    pub async fn create_session(
        &self,
        user_id: Uuid,
        material: &SessionMaterial,
        ttl: chrono::Duration,
    ) -> Result<Uuid, PersistenceError> {
        let now = Utc::now();
        let expires_at = checked_session_expiry(now, ttl)?;
        let mut transaction = self.pool.begin().await?;
        let security_version: Option<i64> = sqlx::query_scalar!(
            "SELECT security_version FROM users WHERE id = $1 AND active FOR SHARE",
            user_id
        )
        .fetch_optional(&mut *transaction)
        .await?;
        let security_version = security_version.ok_or(PersistenceError::SessionUnavailable)?;
        let id = insert_versioned_session(
            &mut transaction,
            user_id,
            security_version,
            material,
            expires_at,
            now,
        )
        .await?;
        sqlx::query!(
            "INSERT INTO audit_events \
             (id, actor_user_id, action, resource_type, resource_id, outcome, occurred_at) \
             VALUES ($1, $2, 'session.create', 'session', $3, 'success', $4)",
            Uuid::now_v7(),
            user_id,
            id.to_string(),
            now
        )
        .execute(&mut *transaction)
        .await?;
        sqlx::query!(
            "INSERT INTO audit_events \
             (id, actor_user_id, action, resource_type, resource_id, outcome, occurred_at) \
             VALUES ($1, $2, 'local_auth.login', 'session', $3, 'success', $4)",
            Uuid::now_v7(),
            user_id,
            id.to_string(),
            now
        )
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
        sqlx::query!(
            "INSERT INTO audit_events \
             (id, actor_user_id, action, resource_type, resource_id, outcome, occurred_at) \
             VALUES ($1, $2, 'local_auth.login', 'session', NULL, 'failure', $3)",
            Uuid::now_v7(),
            user_id,
            Utc::now()
        )
        .execute(&self.pool)
        .await?;
        Ok(())
    }

    pub async fn local_password_user(
        &self,
        email: &str,
    ) -> Result<Option<LocalPasswordUser>, PersistenceError> {
        let row = sqlx::query!(
            "SELECT id, email, display_name, password_hash AS \"password_hash!\", \
                    role::text AS \"role!\" \
             FROM users WHERE email = $1 AND active AND password_hash IS NOT NULL",
            email.trim().to_lowercase()
        )
        .fetch_optional(&self.pool)
        .await?;
        Ok(row.map(|row| LocalPasswordUser {
            id: row.id,
            email: row.email,
            display_name: row.display_name,
            password_hash: row.password_hash,
            role: row.role,
        }))
    }

    pub async fn session_principal(
        &self,
        plaintext_token: &str,
    ) -> Result<Option<SessionPrincipal>, PersistenceError> {
        let digest = SessionMaterial::digest_token(plaintext_token);
        let row = sqlx::query!(
            "WITH authenticated AS MATERIALIZED ( \
                 SELECT s.id AS session_id, s.security_version, s.csrf_digest, s.expires_at, \
                        u.id AS user_id, u.email, u.display_name, u.role::text AS role \
                 FROM sessions s JOIN users u ON u.id = s.user_id \
                 WHERE s.token_digest = $1 AND s.expires_at > now() AND u.active \
                   AND s.security_version = u.security_version \
             ), touched AS ( \
                 UPDATE sessions s SET last_seen_at = now() \
                 FROM authenticated authenticated_session \
                 WHERE s.id = authenticated_session.session_id \
                   AND s.security_version = authenticated_session.security_version \
                   AND s.expires_at > now() \
                   AND s.last_seen_at <= now() - interval '5 minutes' \
                 RETURNING s.id \
             ) \
             SELECT authenticated.session_id, authenticated.security_version, \
                    authenticated.csrf_digest, authenticated.expires_at, \
                    authenticated.user_id, authenticated.email, \
                    authenticated.display_name, authenticated.role AS \"role!\" \
             FROM authenticated \
             CROSS JOIN (SELECT count(*) FROM touched) activity",
            digest.to_vec()
        )
        .fetch_optional(&self.pool)
        .await?;

        Ok(row.map(|row| SessionPrincipal {
            session_id: row.session_id,
            user_id: row.user_id,
            email: row.email,
            display_name: row.display_name,
            role: row.role,
            security_version: row.security_version,
            csrf_digest: row.csrf_digest,
            expires_at: row.expires_at,
        }))
    }

    /// Replaces only the CSRF bearer for an exact still-current session. The
    /// expected digest makes concurrent recovery requests a compare-and-swap.
    pub async fn rotate_session_csrf(
        &self,
        session_id: Uuid,
        user_id: Uuid,
        security_version: i64,
        expected_digest: &[u8],
        replacement: &CsrfMaterial,
    ) -> Result<bool, PersistenceError> {
        let now = Utc::now();
        let mut transaction = self.pool.begin().await?;
        let updated = sqlx::query!(
            "UPDATE sessions session SET csrf_digest = $5 \
             WHERE session.id = $1 AND session.user_id = $2 \
               AND session.security_version = $3 AND session.csrf_digest = $4 \
               AND session.expires_at > $6 \
               AND EXISTS ( \
                   SELECT 1 FROM users \
                   WHERE users.id = session.user_id AND users.active \
                     AND users.security_version = session.security_version \
               )",
            session_id,
            user_id,
            security_version,
            expected_digest,
            replacement.token_digest().to_vec(),
            now
        )
        .execute(&mut *transaction)
        .await?
        .rows_affected();
        if updated == 1 {
            sqlx::query!(
                "INSERT INTO audit_events \
                 (id, actor_user_id, action, resource_type, resource_id, outcome, occurred_at) \
                 VALUES ($1, $2, 'session.csrf_rotate', 'session', $3, 'success', $4)",
                Uuid::now_v7(),
                user_id,
                session_id.to_string(),
                now
            )
            .execute(&mut *transaction)
            .await?;
            transaction.commit().await?;
            Ok(true)
        } else {
            transaction.rollback().await?;
            Ok(false)
        }
    }

    /// Best-effort, token-addressed revocation for idempotent logout. Expired
    /// and security-version-stale rows are deliberately eligible for deletion.
    pub async fn revoke_session_by_token(
        &self,
        plaintext_token: &str,
    ) -> Result<(), PersistenceError> {
        let digest = SessionMaterial::digest_token(plaintext_token);
        let now = Utc::now();
        let mut transaction = self.pool.begin().await?;
        let deleted = sqlx::query!(
            "DELETE FROM sessions WHERE token_digest = $1 RETURNING id, user_id",
            digest.to_vec()
        )
        .fetch_optional(&mut *transaction)
        .await?;
        if let Some(row) = deleted {
            let session_id: Uuid = row.id;
            let user_id: Uuid = row.user_id;
            sqlx::query!(
                "INSERT INTO audit_events \
                 (id, actor_user_id, action, resource_type, resource_id, outcome, occurred_at) \
                 VALUES ($1, $2, 'session.logout', 'session', $3, 'success', $4)",
                Uuid::now_v7(),
                user_id,
                session_id.to_string(),
                now
            )
            .execute(&mut *transaction)
            .await?;
        }
        transaction.commit().await?;
        Ok(())
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
