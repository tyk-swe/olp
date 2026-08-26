use chrono::Utc;
use uuid::Uuid;

use crate::{
    authentication::{insert_versioned_session, sessions::checked_session_expiry},
    error::Error,
    security::session_material::SessionMaterial,
    store::Store,
};

use super::{InstallationSetupInput, InstallationSetupResult};

const SETUP_LOCK_ID: i64 = 0x4f4c_505f_5632; // "OLP_V2"

impl Store {
    pub async fn setup_required(&self) -> Result<bool, Error> {
        let exists: bool =
            sqlx::query_scalar!("SELECT EXISTS (SELECT 1 FROM installation) AS \"value!\"")
                .fetch_one(self.pool())
                .await?;
        Ok(!exists)
    }

    /// Creates the one installation and its first owner as a serialized,
    /// all-or-nothing operation. The advisory lock closes the two-request setup
    /// race even when control-plane replicas receive setup concurrently.
    #[cfg(any(test, feature = "test-util"))]
    pub async fn setup_installation(
        &self,
        input: InstallationSetupInput,
    ) -> Result<InstallationSetupResult, Error> {
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
    ) -> Result<(InstallationSetupResult, Uuid), Error> {
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
    ) -> Result<(InstallationSetupResult, Option<Uuid>), Error> {
        let mut transaction = self.pool().begin().await?;
        sqlx::query!("SELECT pg_advisory_xact_lock($1)", SETUP_LOCK_ID)
            .fetch_one(&mut *transaction)
            .await?;

        let already_setup: bool =
            sqlx::query_scalar!("SELECT EXISTS (SELECT 1 FROM installation) AS \"value!\"")
                .fetch_one(&mut *transaction)
                .await?;
        if already_setup {
            return Err(Error::AlreadySetup);
        }

        let user_id = Uuid::now_v7();
        let now = Utc::now();
        let normalized_email = input.email.trim().to_lowercase();
        sqlx::query!(
            "INSERT INTO installation (singleton, installation_name, created_at, updated_at) \
             VALUES (true, $1, $2, $2)",
            input.installation_name.trim(),
            now
        )
        .execute(&mut *transaction)
        .await?;
        sqlx::query!(
            "INSERT INTO users \
             (id, email, display_name, password_hash, role, active, created_at, updated_at) \
             VALUES ($1, $2, $3, $4, 'owner'::user_role, true, $5, $5)",
            user_id,
            &normalized_email,
            input.display_name.trim(),
            &input.password_hash,
            now
        )
        .execute(&mut *transaction)
        .await?;

        for (key, value) in [
            ("retention.requests_days", "30"),
            ("retention.usage_days", "90"),
            ("retention.audit_days", "365"),
        ] {
            sqlx::query!(
                "INSERT INTO settings (key, value, etag, updated_by, updated_at) \
                 VALUES ($1, $2, $3, $4, $5)",
                key,
                value,
                Uuid::now_v7(),
                user_id,
                now
            )
            .execute(&mut *transaction)
            .await?;
        }

        sqlx::query!(
            "INSERT INTO audit_events \
             (id, actor_user_id, action, resource_type, resource_id, outcome, occurred_at, \
              source_ip, user_agent_family) \
             VALUES ($1, $2, 'installation.setup', 'installation', 'singleton', 'success', $3, \
              $4::text::inet, $5)",
            Uuid::now_v7(),
            user_id,
            now,
            self.provenance().source_ip_text(),
            self.provenance().user_agent_family()
        )
        .execute(&mut *transaction)
        .await?;

        let session_id = if let Some((material, ttl)) = session {
            let expires_at = checked_session_expiry(now, ttl)?;
            let session_id =
                insert_versioned_session(&mut transaction, user_id, 1, material, expires_at, now)
                    .await?;
            sqlx::query!(
                "INSERT INTO audit_events \
                 (id, actor_user_id, action, resource_type, resource_id, outcome, occurred_at, \
                  source_ip, user_agent_family) \
                 VALUES ($1, $2, 'session.create', 'session', $3, 'success', $4, \
                  $5::text::inet, $6)",
                Uuid::now_v7(),
                user_id,
                session_id.to_string(),
                now,
                self.provenance().source_ip_text(),
                self.provenance().user_agent_family()
            )
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
