use chrono::Utc;
use uuid::Uuid;

use crate::{
    audit_events::{AuditEvent, record_audit_event},
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

        insert_default_settings(&mut transaction, user_id, now).await?;

        record_audit_event(
            &mut *transaction,
            AuditEvent {
                provenance: self.provenance(),
                actor: Some(user_id),
                action: "installation.setup",
                resource_type: "installation",
                resource_id: Some("singleton"),
                outcome: "success",
                occurred_at: Some(now),
            },
        )
        .await?;

        let session_id = match session {
            Some((material, ttl)) => Some(
                self.create_setup_session(&mut transaction, user_id, now, material, ttl)
                    .await?,
            ),
            None => None,
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

    async fn create_setup_session(
        &self,
        transaction: &mut sqlx::Transaction<'_, sqlx::Postgres>,
        user_id: Uuid,
        now: chrono::DateTime<Utc>,
        material: &SessionMaterial,
        ttl: chrono::Duration,
    ) -> Result<Uuid, Error> {
        let expires_at = checked_session_expiry(now, ttl)?;
        let session_id =
            insert_versioned_session(transaction, user_id, 1, material, expires_at, now).await?;
        record_audit_event(
            &mut **transaction,
            AuditEvent {
                provenance: self.provenance(),
                actor: Some(user_id),
                action: "session.create",
                resource_type: "session",
                resource_id: Some(&session_id.to_string()),
                outcome: "success",
                occurred_at: Some(now),
            },
        )
        .await?;
        Ok(session_id)
    }
}

async fn insert_default_settings(
    transaction: &mut sqlx::Transaction<'_, sqlx::Postgres>,
    user_id: Uuid,
    now: chrono::DateTime<Utc>,
) -> Result<(), Error> {
    for (key, value) in [
        ("retention.requests_days", "30"),
        ("retention.usage_days", "90"),
        ("retention.audit_days", "365"),
        ("limits.valkey_unavailable", "fail_closed"),
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
        .execute(&mut **transaction)
        .await?;
    }
    Ok(())
}
