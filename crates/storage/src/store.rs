use std::{fmt, time::Duration};

use chrono::{DateTime, Utc};
use olp_domain::RuntimeSnapshot;
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use sqlx::{PgPool, Postgres, Row, Transaction, postgres::PgPoolOptions};
use thiserror::Error;
use uuid::Uuid;
use zeroize::Zeroizing;

use crate::{
    SessionMaterial,
    security::{EncryptedSecret, MasterKey, idempotency_replay_scope},
};

const SETUP_LOCK_ID: i64 = 0x4f4c_505f_5632; // "OLP_V2"

#[derive(Debug, Error)]
pub enum PersistenceError {
    #[error("database operation failed")]
    Database(#[from] sqlx::Error),
    #[error("database migration failed")]
    Migration(#[from] sqlx::migrate::MigrateError),
    #[error("installation setup has already completed")]
    AlreadySetup,
    #[error("runtime release failed integrity verification")]
    CorruptRelease,
    #[error("runtime snapshot is invalid: {0}")]
    InvalidRuntimeSnapshot(#[from] olp_domain::SnapshotValidationError),
    #[error("runtime release serialization failed")]
    Serialize(#[from] serde_json::Error),
    #[error("session lifetime must be positive and representable")]
    InvalidSessionTtl,
    #[error("usage gap metadata is invalid")]
    InvalidUsageGap,
    #[error("usage event timing or status metadata is invalid")]
    InvalidUsageEvent,
    #[error("stored {0} is outside the supported closed set")]
    InvalidStoredValue(&'static str),
    #[error("idempotency replay encryption failed")]
    IdempotencyReplayEncryption,
    #[error("idempotency replay material is unavailable or corrupt")]
    IdempotencyReplayUnavailable,
}

#[derive(Clone)]
pub struct PgStore {
    pool: PgPool,
}

impl PgStore {
    pub async fn connect(
        database_url: &str,
        max_connections: u32,
    ) -> Result<Self, PersistenceError> {
        let pool = PgPoolOptions::new()
            .max_connections(max_connections)
            .acquire_timeout(Duration::from_secs(5))
            .connect(database_url)
            .await?;
        Ok(Self { pool })
    }

    pub fn from_pool(pool: PgPool) -> Self {
        Self { pool }
    }

    pub fn pool(&self) -> &PgPool {
        &self.pool
    }

    pub async fn migrate(&self) -> Result<(), PersistenceError> {
        crate::MIGRATOR.run(&self.pool).await?;
        Ok(())
    }

    pub async fn ping(&self) -> Result<(), PersistenceError> {
        sqlx::query("SELECT 1").execute(&self.pool).await?;
        Ok(())
    }

    pub async fn setup_required(&self) -> Result<bool, PersistenceError> {
        let exists: bool = sqlx::query_scalar("SELECT EXISTS (SELECT 1 FROM installation)")
            .fetch_one(&self.pool)
            .await?;
        Ok(!exists)
    }

    /// Creates the one installation and its first owner as a serialized,
    /// all-or-nothing operation. The advisory lock closes the two-request setup
    /// race even when control-plane replicas receive setup concurrently.
    pub async fn setup_owner(&self, owner: NewOwner) -> Result<SetupResult, PersistenceError> {
        self.setup_owner_inner(owner, None)
            .await
            .map(|(result, _)| result)
    }

    /// Creates the installation, owner, defaults, audit event, and initial
    /// session in one transaction. Only session digests enter PostgreSQL.
    pub async fn setup_owner_with_session(
        &self,
        owner: NewOwner,
        material: &SessionMaterial,
        ttl: chrono::Duration,
    ) -> Result<(SetupResult, Uuid), PersistenceError> {
        checked_session_expiry(Utc::now(), ttl)?;
        let (result, session_id) = self.setup_owner_inner(owner, Some((material, ttl))).await?;
        Ok((
            result,
            session_id.expect("session was requested from setup transaction"),
        ))
    }

    async fn setup_owner_inner(
        &self,
        owner: NewOwner,
        session: Option<(&SessionMaterial, chrono::Duration)>,
    ) -> Result<(SetupResult, Option<Uuid>), PersistenceError> {
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
        let normalized_email = owner.email.trim().to_lowercase();
        sqlx::query(
            "INSERT INTO installation (singleton, organization_name, created_at, updated_at) \
             VALUES (true, $1, $2, $2)",
        )
        .bind(owner.organization_name.trim())
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
        .bind(owner.display_name.trim())
        .bind(&owner.password_hash)
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
            SetupResult {
                user_id,
                email: normalized_email,
                display_name: owner.display_name.trim().to_owned(),
                created_at: now,
            },
            session_id,
        ))
    }

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

    /// Returns newest verified releases, skipping and visibly logging corrupt
    /// envelopes so a replacement gateway can try its previous durable LKG.
    pub async fn recent_valid_releases(
        &self,
        limit: u16,
    ) -> Result<Vec<PublishedRelease>, PersistenceError> {
        let rows = sqlx::query(
            "SELECT id, sequence, compiled_release, release_sha256, created_at \
             FROM runtime_generations ORDER BY sequence DESC LIMIT $1",
        )
        .bind(i64::from(limit.clamp(1, 100)))
        .fetch_all(&self.pool)
        .await?;
        let mut releases = Vec::with_capacity(rows.len());
        for row in rows {
            let payload: Vec<u8> = row.get("compiled_release");
            let stored_sha: Vec<u8> = row.get("release_sha256");
            let generation_id: Uuid = row.get("id");
            let sequence: i64 = row.get("sequence");
            let actual_sha: [u8; 32] = Sha256::digest(&payload).into();
            if stored_sha.as_slice() != actual_sha
                || verify_release_envelope(&payload, generation_id, sequence).is_err()
            {
                tracing::error!(
                    %generation_id,
                    sequence,
                    "skipping corrupt runtime release while searching for last-known-good"
                );
                continue;
            }
            releases.push(PublishedRelease {
                generation_id,
                sequence,
                payload,
                sha256: actual_sha,
                created_at: row.get("created_at"),
            });
        }
        Ok(releases)
    }

    pub async fn pending_outbox(&self, limit: i64) -> Result<Vec<OutboxRecord>, PersistenceError> {
        let rows = sqlx::query(
            "SELECT id, topic, aggregate_id, payload, created_at \
             FROM transactional_outbox WHERE published_at IS NULL \
             ORDER BY created_at LIMIT $1",
        )
        .bind(limit.clamp(1, 1_000))
        .fetch_all(&self.pool)
        .await?;
        Ok(rows
            .into_iter()
            .map(|row| OutboxRecord {
                id: row.get("id"),
                topic: row.get("topic"),
                aggregate_id: row.get("aggregate_id"),
                payload: row.get("payload"),
                created_at: row.get("created_at"),
            })
            .collect())
    }

    pub async fn mark_outbox_published(&self, id: Uuid) -> Result<bool, PersistenceError> {
        let result = sqlx::query(
            "UPDATE transactional_outbox SET published_at = now() \
             WHERE id = $1 AND published_at IS NULL",
        )
        .bind(id)
        .execute(&self.pool)
        .await?;
        Ok(result.rows_affected() == 1)
    }

    /// Records a gap exactly once for a durable source identity such as a
    /// Valkey Stream entry or decoded event ID. This closes the commit-before-
    /// acknowledgement crash window without storing content.
    pub async fn report_usage_gap_once(
        &self,
        gap: UsageGap,
        deduplication_key: &str,
    ) -> Result<bool, PersistenceError> {
        if deduplication_key.is_empty() || deduplication_key.len() > 256 {
            return Err(PersistenceError::InvalidUsageGap);
        }
        self.insert_usage_gap(gap, Some(deduplication_key)).await
    }

    async fn insert_usage_gap(
        &self,
        gap: UsageGap,
        deduplication_key: Option<&str>,
    ) -> Result<bool, PersistenceError> {
        if gap.event_count <= 0
            || gap.gateway_instance.trim().is_empty()
            || gap.reason.trim().is_empty()
            || gap.last_observed_at < gap.first_observed_at
        {
            return Err(PersistenceError::InvalidUsageGap);
        }
        let result = sqlx::query(
            "INSERT INTO usage_ingestion_gaps \
             (id, gateway_instance, event_count, reason, first_observed_at, last_observed_at, \
              deduplication_key) \
             VALUES ($1, $2, $3, $4, $5, $6, $7) \
             ON CONFLICT (deduplication_key) WHERE deduplication_key IS NOT NULL DO NOTHING",
        )
        .bind(Uuid::now_v7())
        .bind(gap.gateway_instance)
        .bind(gap.event_count)
        .bind(gap.reason)
        .bind(gap.first_observed_at)
        .bind(gap.last_observed_at)
        .bind(deduplication_key)
        .execute(&self.pool)
        .await?;
        Ok(result.rows_affected() == 1)
    }
}

pub struct NewOwner {
    pub organization_name: String,
    pub email: String,
    pub display_name: String,
    pub password_hash: String,
}

impl fmt::Debug for NewOwner {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("NewOwner")
            .field("organization_name", &self.organization_name)
            .field("email", &self.email)
            .field("display_name", &self.display_name)
            .field("password_hash", &"[REDACTED]")
            .finish()
    }
}

#[derive(Debug, Clone)]
pub struct SetupResult {
    pub user_id: Uuid,
    pub email: String,
    pub display_name: String,
    pub created_at: DateTime<Utc>,
}

#[derive(Clone)]
pub struct SessionPrincipal {
    pub session_id: Uuid,
    pub user_id: Uuid,
    pub email: String,
    pub display_name: String,
    pub role: String,
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
            .field("csrf_digest", &"[REDACTED]")
            .field("expires_at", &self.expires_at)
            .finish()
    }
}

#[derive(Clone)]
pub struct PasswordUser {
    pub id: Uuid,
    pub email: String,
    pub display_name: String,
    pub password_hash: String,
    pub role: String,
}

impl fmt::Debug for PasswordUser {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("PasswordUser")
            .field("id", &self.id)
            .field("email", &self.email)
            .field("display_name", &self.display_name)
            .field("password_hash", &"[REDACTED]")
            .field("role", &self.role)
            .finish()
    }
}

#[derive(Clone)]
pub struct PublishedRelease {
    pub generation_id: Uuid,
    pub sequence: i64,
    pub payload: Vec<u8>,
    pub sha256: [u8; 32],
    pub created_at: DateTime<Utc>,
}

impl fmt::Debug for PublishedRelease {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("PublishedRelease")
            .field("generation_id", &self.generation_id)
            .field("sequence", &self.sequence)
            .field("payload", &"[REDACTED]")
            .field("sha256", &self.sha256)
            .field("created_at", &self.created_at)
            .finish()
    }
}

#[derive(Clone)]
pub struct OutboxRecord {
    pub id: Uuid,
    pub topic: String,
    pub aggregate_id: Uuid,
    pub payload: Vec<u8>,
    pub created_at: DateTime<Utc>,
}

impl fmt::Debug for OutboxRecord {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("OutboxRecord")
            .field("id", &self.id)
            .field("topic", &self.topic)
            .field("aggregate_id", &self.aggregate_id)
            .field("payload", &"[REDACTED]")
            .field("created_at", &self.created_at)
            .finish()
    }
}

#[derive(Debug, Clone)]
pub struct UsageGap {
    pub gateway_instance: String,
    pub event_count: i64,
    pub reason: String,
    pub first_observed_at: DateTime<Utc>,
    pub last_observed_at: DateTime<Utc>,
}

const IDEMPOTENCY_REPLAY_VERSION: u8 = 1;
const MAX_IDEMPOTENCY_REPLAY_BODY_BYTES: usize = 1024 * 1024;
const MAX_IDEMPOTENCY_REPLAY_CIPHERTEXT_BYTES: usize = MAX_IDEMPOTENCY_REPLAY_BODY_BYTES * 4 + 4096;

/// Opaque HTTP replay material persisted only inside an authenticated
/// encryption envelope. Debug output deliberately never includes the body.
pub struct IdempotencyResponse {
    status: u16,
    content_type: Option<String>,
    etag: Option<String>,
    body: Zeroizing<Vec<u8>>,
}

impl IdempotencyResponse {
    pub fn new(
        status: u16,
        content_type: Option<String>,
        etag: Option<String>,
        body: Vec<u8>,
    ) -> Result<Self, PersistenceError> {
        let response = Self {
            status,
            content_type,
            etag,
            body: Zeroizing::new(body),
        };
        response.validate()?;
        Ok(response)
    }

    pub fn json<T: Serialize>(
        status: u16,
        value: &T,
        etag: Option<String>,
    ) -> Result<Self, PersistenceError> {
        Self::new(
            status,
            Some("application/json".to_owned()),
            etag,
            serde_json::to_vec(value)?,
        )
    }

    #[must_use]
    pub fn into_parts(mut self) -> (u16, Option<String>, Option<String>, Vec<u8>) {
        let body = std::mem::take(&mut *self.body);
        (
            self.status,
            self.content_type.take(),
            self.etag.take(),
            body,
        )
    }

    fn validate(&self) -> Result<(), PersistenceError> {
        if !(200..=599).contains(&self.status)
            || self.body.len() > MAX_IDEMPOTENCY_REPLAY_BODY_BYTES
            || self
                .content_type
                .as_ref()
                .is_some_and(|value| !valid_replay_header(value))
            || self
                .etag
                .as_ref()
                .is_some_and(|value| !valid_replay_header(value))
        {
            return Err(PersistenceError::IdempotencyReplayUnavailable);
        }
        Ok(())
    }
}

impl fmt::Debug for IdempotencyResponse {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("IdempotencyResponse")
            .field("status", &self.status)
            .field("content_type", &self.content_type)
            .field("etag", &self.etag)
            .field("body", &"[REDACTED]")
            .finish()
    }
}

#[derive(Debug)]
pub enum IdempotencyOutcome<T> {
    Executed {
        value: T,
        response: IdempotencyResponse,
    },
    Replayed(IdempotencyResponse),
}

#[derive(Clone, Copy)]
pub struct ReplayableIdempotency<'a> {
    request_fingerprint: [u8; 32],
    master_key: &'a MasterKey,
}

impl<'a> ReplayableIdempotency<'a> {
    #[must_use]
    pub const fn new(request_fingerprint: [u8; 32], master_key: &'a MasterKey) -> Self {
        Self {
            request_fingerprint,
            master_key,
        }
    }

    #[must_use]
    pub const fn request_fingerprint(&self) -> &[u8; 32] {
        &self.request_fingerprint
    }

    #[must_use]
    pub const fn master_key(&self) -> &'a MasterKey {
        self.master_key
    }
}

impl fmt::Debug for ReplayableIdempotency<'_> {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("ReplayableIdempotency")
            .field("request_fingerprint", &"[SHA-256]")
            .field("master_key", &"[REDACTED]")
            .finish()
    }
}

#[derive(Debug)]
pub(crate) enum ReplayableIdempotencyClaim {
    Execute,
    Replay(IdempotencyResponse),
    Conflict,
    InProgress,
}

#[derive(Serialize)]
struct StoredIdempotencyResponseRef<'a> {
    version: u8,
    status: u16,
    content_type: &'a Option<String>,
    etag: &'a Option<String>,
    body: &'a [u8],
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct StoredIdempotencyResponse {
    version: u8,
    status: u16,
    content_type: Option<String>,
    etag: Option<String>,
    body: Vec<u8>,
}

/// Produces a stable SHA-256 fingerprint from a typed management request.
/// Callers should serialize only request semantics, never generated secrets.
pub fn idempotency_fingerprint<T: Serialize>(request: &T) -> Result<[u8; 32], PersistenceError> {
    Ok(Sha256::digest(serde_json::to_vec(request)?).into())
}

/// Reduces a write-only request secret to a stable fingerprint component so
/// the plaintext never enters the serialized idempotency request envelope.
#[must_use]
pub fn idempotency_secret_digest(secret: &[u8]) -> [u8; 32] {
    Sha256::digest(secret).into()
}

pub(crate) async fn claim_replayable_idempotency(
    transaction: &mut Transaction<'_, Postgres>,
    actor: Uuid,
    operation: &str,
    key: &str,
    request_fingerprint: &[u8; 32],
    master_key: &MasterKey,
) -> Result<ReplayableIdempotencyClaim, PersistenceError> {
    let scope = idempotency_replay_scope(actor, operation, key);
    let locked: bool =
        sqlx::query_scalar("SELECT pg_try_advisory_xact_lock(hashtextextended($1::text, 0))")
            .bind(&scope)
            .fetch_one(&mut **transaction)
            .await?;
    if !locked {
        return Ok(ReplayableIdempotencyClaim::InProgress);
    }

    sqlx::query(
        "DELETE FROM idempotency_records \
         WHERE actor_user_id = $1 AND operation = $2 AND idempotency_key = $3 \
           AND expires_at <= now()",
    )
    .bind(actor)
    .bind(operation)
    .bind(key)
    .execute(&mut **transaction)
    .await?;

    let existing = sqlx::query(
        "SELECT state, request_fingerprint, replay_ciphertext, replay_nonce, replay_key_version \
         FROM idempotency_records \
         WHERE actor_user_id = $1 AND operation = $2 AND idempotency_key = $3",
    )
    .bind(actor)
    .bind(operation)
    .bind(key)
    .fetch_optional(&mut **transaction)
    .await?;
    if let Some(row) = existing {
        let stored_fingerprint: Option<Vec<u8>> = row.get("request_fingerprint");
        if stored_fingerprint.as_deref() != Some(request_fingerprint.as_slice()) {
            return Ok(ReplayableIdempotencyClaim::Conflict);
        }
        let state: String = row.get("state");
        if state == "in_progress" {
            return Ok(ReplayableIdempotencyClaim::InProgress);
        }
        if state != "completed" {
            return Err(PersistenceError::IdempotencyReplayUnavailable);
        }
        let ciphertext: Option<Vec<u8>> = row.get("replay_ciphertext");
        let nonce: Option<Vec<u8>> = row.get("replay_nonce");
        let key_version: Option<i32> = row.get("replay_key_version");
        let ciphertext = ciphertext.ok_or(PersistenceError::IdempotencyReplayUnavailable)?;
        if ciphertext.len() > MAX_IDEMPOTENCY_REPLAY_CIPHERTEXT_BYTES {
            return Err(PersistenceError::IdempotencyReplayUnavailable);
        }
        let nonce: [u8; 12] = nonce
            .ok_or(PersistenceError::IdempotencyReplayUnavailable)?
            .try_into()
            .map_err(|_| PersistenceError::IdempotencyReplayUnavailable)?;
        let key_version =
            u32::try_from(key_version.ok_or(PersistenceError::IdempotencyReplayUnavailable)?)
                .map_err(|_| PersistenceError::IdempotencyReplayUnavailable)?;
        let encrypted = EncryptedSecret {
            key_version,
            nonce,
            ciphertext,
        };
        let plaintext = master_key
            .open(&encrypted, scope.as_bytes())
            .map_err(|_| PersistenceError::IdempotencyReplayUnavailable)?;
        let stored: StoredIdempotencyResponse = serde_json::from_slice(&plaintext)
            .map_err(|_| PersistenceError::IdempotencyReplayUnavailable)?;
        if stored.version != IDEMPOTENCY_REPLAY_VERSION {
            return Err(PersistenceError::IdempotencyReplayUnavailable);
        }
        let response =
            IdempotencyResponse::new(stored.status, stored.content_type, stored.etag, stored.body)?;
        return Ok(ReplayableIdempotencyClaim::Replay(response));
    }

    sqlx::query(
        "INSERT INTO idempotency_records \
         (id, actor_user_id, operation, idempotency_key, state, request_fingerprint, expires_at) \
         VALUES ($1, $2, $3, $4, 'in_progress', $5, now() + interval '24 hours')",
    )
    .bind(Uuid::now_v7())
    .bind(actor)
    .bind(operation)
    .bind(key)
    .bind(request_fingerprint.as_slice())
    .execute(&mut **transaction)
    .await?;
    Ok(ReplayableIdempotencyClaim::Execute)
}

pub(crate) async fn complete_replayable_idempotency(
    transaction: &mut Transaction<'_, Postgres>,
    actor: Uuid,
    operation: &str,
    key: &str,
    request_fingerprint: &[u8; 32],
    master_key: &MasterKey,
    response: &IdempotencyResponse,
) -> Result<(), PersistenceError> {
    response.validate()?;
    let scope = idempotency_replay_scope(actor, operation, key);
    let plaintext = Zeroizing::new(serde_json::to_vec(&StoredIdempotencyResponseRef {
        version: IDEMPOTENCY_REPLAY_VERSION,
        status: response.status,
        content_type: &response.content_type,
        etag: &response.etag,
        body: &response.body,
    })?);
    let encrypted = master_key
        .seal(&plaintext, scope.as_bytes())
        .map_err(|_| PersistenceError::IdempotencyReplayEncryption)?;
    let key_version = i32::try_from(encrypted.key_version)
        .map_err(|_| PersistenceError::IdempotencyReplayEncryption)?;
    let result = sqlx::query(
        "UPDATE idempotency_records \
         SET state = 'completed', replay_ciphertext = $1, \
             replay_nonce = $2, replay_key_version = $3 \
         WHERE actor_user_id = $4 AND operation = $5 AND idempotency_key = $6 \
           AND state = 'in_progress' AND request_fingerprint = $7",
    )
    .bind(encrypted.ciphertext)
    .bind(encrypted.nonce.to_vec())
    .bind(key_version)
    .bind(actor)
    .bind(operation)
    .bind(key)
    .bind(request_fingerprint.as_slice())
    .execute(&mut **transaction)
    .await?;
    if result.rows_affected() != 1 {
        return Err(PersistenceError::IdempotencyReplayUnavailable);
    }
    Ok(())
}

fn valid_replay_header(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= 256
        && value
            .bytes()
            .all(|byte| (0x20..=0x7e).contains(&byte) && byte != b'\r' && byte != b'\n')
}

pub(crate) async fn claim_idempotency(
    transaction: &mut Transaction<'_, Postgres>,
    actor: Uuid,
    operation: &str,
    key: &str,
) -> Result<bool, sqlx::Error> {
    // Expired claims must not permanently reserve a key. Keeping cleanup in
    // the caller's transaction also serializes a retry with any concurrent
    // attempt using the same actor/operation/key tuple.
    sqlx::query(
        "DELETE FROM idempotency_records \
         WHERE actor_user_id = $1 AND operation = $2 AND idempotency_key = $3 \
           AND expires_at <= now()",
    )
    .bind(actor)
    .bind(operation)
    .bind(key)
    .execute(&mut **transaction)
    .await?;
    let result = sqlx::query(
        "INSERT INTO idempotency_records \
         (id, actor_user_id, operation, idempotency_key, state, expires_at) \
         VALUES ($1, $2, $3, $4, 'in_progress', now() + interval '24 hours') \
         ON CONFLICT (actor_user_id, operation, idempotency_key) DO NOTHING",
    )
    .bind(Uuid::now_v7())
    .bind(actor)
    .bind(operation)
    .bind(key)
    .execute(&mut **transaction)
    .await?;
    Ok(result.rows_affected() == 1)
}

pub(crate) async fn complete_idempotency(
    transaction: &mut Transaction<'_, Postgres>,
    actor: Uuid,
    operation: &str,
    key: &str,
    resource_id: &str,
) -> Result<(), sqlx::Error> {
    sqlx::query(
        "UPDATE idempotency_records SET state = 'completed', resource_id = $1 \
         WHERE actor_user_id = $2 AND operation = $3 AND idempotency_key = $4",
    )
    .bind(resource_id)
    .bind(actor)
    .bind(operation)
    .bind(key)
    .execute(&mut **transaction)
    .await?;
    Ok(())
}

fn verify_release_envelope(
    payload: &[u8],
    generation_id: Uuid,
    sequence: i64,
) -> Result<(), PersistenceError> {
    if generation_id.get_version_num() != 7 {
        return Err(PersistenceError::CorruptRelease);
    }
    let ordinal = u64::try_from(sequence).map_err(|_| PersistenceError::CorruptRelease)?;
    let snapshot: RuntimeSnapshot =
        serde_json::from_slice(payload).map_err(|_| PersistenceError::CorruptRelease)?;
    if snapshot.generation.id.as_uuid() != generation_id
        || snapshot.generation.ordinal != ordinal
        || snapshot.validate().is_err()
    {
        return Err(PersistenceError::CorruptRelease);
    }
    Ok(())
}

fn checked_session_expiry(
    now: DateTime<Utc>,
    ttl: chrono::Duration,
) -> Result<DateTime<Utc>, PersistenceError> {
    if ttl <= chrono::Duration::zero() {
        return Err(PersistenceError::InvalidSessionTtl);
    }
    now.checked_add_signed(ttl)
        .ok_or(PersistenceError::InvalidSessionTtl)
}

#[cfg(test)]
mod tests {
    use super::*;
    use olp_domain::{RuntimeGeneration, RuntimeGenerationId};

    fn snapshot() -> RuntimeSnapshot {
        RuntimeSnapshot {
            generation: RuntimeGeneration {
                id: RuntimeGenerationId::new(),
                ordinal: 7,
                activated_at: Utc::now(),
            },
            providers: Default::default(),
            routes: Default::default(),
            api_keys: Default::default(),
        }
    }

    #[test]
    fn release_envelope_binds_payload_id_and_sequence() {
        let snapshot = snapshot();
        let payload = serde_json::to_vec(&snapshot).unwrap();
        let id = snapshot.generation.id.as_uuid();
        assert!(verify_release_envelope(&payload, id, 7).is_ok());
        assert!(verify_release_envelope(&payload, Uuid::now_v7(), 7).is_err());
        assert!(verify_release_envelope(&payload, id, 8).is_err());
        assert!(verify_release_envelope(&payload, id, 0).is_err());
    }

    #[test]
    fn sensitive_repository_records_redact_debug_output() {
        let password = PasswordUser {
            id: Uuid::now_v7(),
            email: "owner@example.test".into(),
            display_name: "Owner".into(),
            password_hash: "secret-hash".into(),
            role: "owner".into(),
        };
        assert!(!format!("{password:?}").contains("secret-hash"));

        let mut principal = SessionPrincipal {
            session_id: Uuid::now_v7(),
            user_id: Uuid::now_v7(),
            email: "owner@example.test".into(),
            display_name: "Owner".into(),
            role: "owner".into(),
            csrf_digest: vec![1, 2, 3, 4],
            expires_at: Utc::now(),
        };
        assert!(!format!("{principal:?}").contains("1, 2, 3, 4"));
        principal.csrf_digest.clear();

        let response = IdempotencyResponse::json(
            201,
            &serde_json::json!({"secret": "one-time-secret"}),
            Some("\"etag\"".to_owned()),
        )
        .unwrap();
        assert!(!format!("{response:?}").contains("one-time-secret"));
    }

    #[test]
    fn typed_idempotency_fingerprints_are_stable_and_request_bound() {
        let first = idempotency_fingerprint(&serde_json::json!({
            "name": "key",
            "scopes": ["inference"]
        }))
        .unwrap();
        let identical = idempotency_fingerprint(&serde_json::json!({
            "name": "key",
            "scopes": ["inference"]
        }))
        .unwrap();
        let changed = idempotency_fingerprint(&serde_json::json!({
            "name": "changed",
            "scopes": ["inference"]
        }))
        .unwrap();
        assert_eq!(first, identical);
        assert_ne!(first, changed);
    }
}
