use chrono::Utc;
use uuid::Uuid;

use crate::SessionMaterial;

use super::{
    InstallationSetupInput, InstallationSetupResult, PersistenceError, PgStore,
    sessions::checked_session_expiry,
};

const SETUP_LOCK_ID: i64 = 0x4f4c_505f_5632; // "OLP_V2"

impl PgStore {
    /// Creates the one installation and its first owner as a serialized,
    /// all-or-nothing operation. The advisory lock closes the two-request setup
    /// race even when control-plane replicas receive setup concurrently.
    pub async fn setup_installation(
        &self,
        input: InstallationSetupInput,
    ) -> Result<InstallationSetupResult, PersistenceError> {
        self.setup_installation_inner(input, None)
            .await
            .map(|(result, _)| result)
    }

    /// Creates the installation, owner, defaults, audit event, and initial
    /// session in one transaction. Only session digests enter PostgreSQL.
    pub async fn setup_installation_with_session(
        &self,
        input: InstallationSetupInput,
        material: &SessionMaterial,
        ttl: chrono::Duration,
    ) -> Result<(InstallationSetupResult, Uuid), PersistenceError> {
        checked_session_expiry(Utc::now(), ttl)?;
        let (result, session_id) = self
            .setup_installation_inner(input, Some((material, ttl)))
            .await?;
        Ok((
            result,
            session_id.expect("session was requested from setup transaction"),
        ))
    }

    async fn setup_installation_inner(
        &self,
        input: InstallationSetupInput,
        session: Option<(&SessionMaterial, chrono::Duration)>,
    ) -> Result<(InstallationSetupResult, Option<Uuid>), PersistenceError> {
        let mut transaction = self.pool.begin().await?;
        sqlx::query("SELECT pg_advisory_xact_lock($1)")
            .bind(SETUP_LOCK_ID)
            .execute(&mut *transaction)
            .await?;

        let already_setup: bool = sqlx::query_scalar("SELECT EXISTS (SELECT 1 FROM installation)")
            .fetch_one(&mut *transaction)
            .await?;
        if already_setup {
            return Err(PersistenceError::AlreadySetup);
        }

        let user_id = Uuid::now_v7();
        let now = Utc::now();
        let normalized_email = input.email.trim().to_lowercase();
        sqlx::query(
            "INSERT INTO installation (singleton, installation_name, created_at, updated_at) \
             VALUES (true, $1, $2, $2)",
        )
        .bind(input.installation_name.trim())
        .bind(now)
        .execute(&mut *transaction)
        .await?;
        sqlx::query(
            "INSERT INTO users \
             (id, email, display_name, password_hash, role, active, created_at, updated_at) \
             VALUES ($1, $2, $3, $4, 'owner'::user_role, true, $5, $5)",
        )
        .bind(user_id)
        .bind(&normalized_email)
        .bind(input.display_name.trim())
        .bind(&input.password_hash)
        .bind(now)
        .execute(&mut *transaction)
        .await?;

        for (key, value) in [
            ("retention.requests_days", "30"),
            ("retention.usage_days", "90"),
            ("retention.audit_days", "365"),
        ] {
            sqlx::query(
                "INSERT INTO settings (key, value, etag, updated_by, updated_at) \
                 VALUES ($1, $2, $3, $4, $5)",
            )
            .bind(key)
            .bind(value)
            .bind(Uuid::now_v7())
            .bind(user_id)
            .bind(now)
            .execute(&mut *transaction)
            .await?;
        }

        sqlx::query(
            "INSERT INTO audit_events \
             (id, actor_user_id, action, resource_type, resource_id, outcome, occurred_at) \
             VALUES ($1, $2, 'installation.setup', 'installation', 'singleton', 'success', $3)",
        )
        .bind(Uuid::now_v7())
        .bind(user_id)
        .bind(now)
        .execute(&mut *transaction)
        .await?;

        let session_id = if let Some((material, ttl)) = session {
            let session_id = Uuid::now_v7();
            let expires_at = checked_session_expiry(now, ttl)?;
            sqlx::query(
                "INSERT INTO sessions \
                 (id, user_id, token_digest, csrf_digest, expires_at, last_seen_at, created_at) \
                 VALUES ($1, $2, $3, $4, $5, $6, $6)",
            )
            .bind(session_id)
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
            .bind(session_id.to_string())
            .bind(now)
            .execute(&mut *transaction)
            .await?;
            Some(session_id)
        } else {
            None
        };
        transaction.commit().await?;

        Ok((
            InstallationSetupResult {
                user_id,
                email: normalized_email,
                display_name: input.display_name.trim().to_owned(),
                created_at: now,
            },
            session_id,
        ))
    }
}
