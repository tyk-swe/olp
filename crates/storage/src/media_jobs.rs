use chrono::{DateTime, Utc};
use olp_domain::{OperationKind, Surface};
use serde::{Deserialize, Serialize};
use sqlx::{Postgres, QueryBuilder, Row};
use thiserror::Error;
use uuid::Uuid;

use crate::{Page, PgStore, TimestampCursor, split_page};

const MAX_PAGE_SIZE: u16 = 200;

#[derive(Debug, Error)]
pub enum MediaJobError {
    #[error("database operation failed")]
    Database(#[from] sqlx::Error),
    #[error("media job was not found")]
    NotFound,
    #[error("media job changed; refresh and retry")]
    PreconditionFailed,
    #[error("upstream media job identity conflicts with stored metadata")]
    UpstreamIdentityConflict,
    #[error("media job input is invalid: {0}")]
    Invalid(String),
}

#[derive(Clone, Copy, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum MediaJobLifecycle {
    Creating,
    Active,
    CreateAmbiguous,
    CreateCleanupPending,
    DeletePending,
    Deleted,
}

impl MediaJobLifecycle {
    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::Creating => "creating",
            Self::Active => "active",
            Self::CreateAmbiguous => "create_ambiguous",
            Self::CreateCleanupPending => "create_cleanup_pending",
            Self::DeletePending => "delete_pending",
            Self::Deleted => "deleted",
        }
    }

    fn parse(value: &str) -> Result<Self, MediaJobError> {
        match value {
            "creating" => Ok(Self::Creating),
            "active" => Ok(Self::Active),
            "create_ambiguous" => Ok(Self::CreateAmbiguous),
            "create_cleanup_pending" => Ok(Self::CreateCleanupPending),
            "delete_pending" => Ok(Self::DeletePending),
            "deleted" => Ok(Self::Deleted),
            _ => Err(MediaJobError::Invalid(
                "database returned an unknown media job lifecycle".to_owned(),
            )),
        }
    }

    #[must_use]
    pub const fn needs_reconciliation(self) -> bool {
        matches!(
            self,
            Self::Creating
                | Self::CreateAmbiguous
                | Self::CreateCleanupPending
                | Self::DeletePending
        )
    }
}

#[derive(Clone, Copy, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum MediaJobState {
    Queued,
    Running,
    Succeeded,
    Failed,
    Cancelled,
}

impl MediaJobState {
    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::Queued => "queued",
            Self::Running => "running",
            Self::Succeeded => "succeeded",
            Self::Failed => "failed",
            Self::Cancelled => "cancelled",
        }
    }

    fn parse(value: &str) -> Result<Self, MediaJobError> {
        match value {
            "queued" => Ok(Self::Queued),
            "running" => Ok(Self::Running),
            "succeeded" => Ok(Self::Succeeded),
            "failed" => Ok(Self::Failed),
            "cancelled" => Ok(Self::Cancelled),
            _ => Err(MediaJobError::Invalid(
                "database returned an unknown media job state".to_owned(),
            )),
        }
    }
}

#[derive(Clone, Debug)]
pub struct NewMediaJobReservation {
    pub id: Uuid,
    /// Runtime generation pinned before the upstream side effect. Production
    /// releases resolve this to an immutable provider revision in PostgreSQL.
    pub runtime_generation_id: Uuid,
    pub api_key_id: Uuid,
    pub provider_id: Uuid,
    pub provider_model: String,
    pub route_slug: String,
    pub operation: OperationKind,
    pub surface: Surface,
}

#[derive(Clone, Debug, Default)]
pub struct MediaJobFilters {
    pub api_key_id: Option<Uuid>,
    pub provider_id: Option<Uuid>,
    pub route_slug: Option<String>,
    /// Optional multi-route allowlist used by client-facing lifecycle lists.
    /// Empty means all routes.
    pub route_slugs: Vec<String>,
    pub operation: Option<OperationKind>,
    pub surface: Option<Surface>,
    pub state: Option<MediaJobState>,
    pub lifecycle: Option<MediaJobLifecycle>,
    pub created_after: Option<DateTime<Utc>>,
    pub created_before: Option<DateTime<Utc>>,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum MediaJobOrder {
    Ascending,
    Descending,
}

#[derive(Clone, Debug)]
pub struct MediaJobRecord {
    pub id: Uuid,
    pub upstream_job_id: Option<String>,
    pub api_key_id: Uuid,
    pub provider_id: Uuid,
    pub provider_name: String,
    pub provider_model: String,
    pub route_slug: String,
    pub operation: OperationKind,
    pub surface: Surface,
    pub state: MediaJobState,
    pub lifecycle: MediaJobLifecycle,
    pub progress_percent: Option<f32>,
    pub content_available: bool,
    pub expires_at: Option<DateTime<Utc>>,
    pub error_class: Option<String>,
    pub completed_at: Option<DateTime<Utc>>,
    pub last_polled_at: Option<DateTime<Utc>>,
    pub reconciliation_error: Option<String>,
    pub deleted_at: Option<DateTime<Utc>>,
    pub runtime_generation_id: Option<Uuid>,
    pub provider_revision_id: Option<Uuid>,
    pub reconciliation_claim_id: Option<Uuid>,
    pub reconciliation_attempts: u32,
    pub next_reconciliation_at: DateTime<Utc>,
    pub last_reconciliation_at: Option<DateTime<Utc>>,
    pub etag: Uuid,
    pub created_at: DateTime<Utc>,
    pub updated_at: DateTime<Utc>,
}

#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub struct MediaReconciliationSummary {
    pub pending: u64,
    pub stale: u64,
    pub failed: u64,
    pub unbound: u64,
    pub oldest_pending_at: Option<DateTime<Utc>>,
}

#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub struct MediaReconciliationPass {
    pub claimed: u16,
    pub completed: u16,
    pub failed: u16,
}

#[derive(Clone, Debug)]
pub struct MediaJobUpdate {
    pub state: MediaJobState,
    pub progress_percent: Option<f32>,
    pub content_available: bool,
    pub expires_at: Option<DateTime<Utc>>,
    pub error_class: Option<String>,
    pub last_polled_at: DateTime<Utc>,
}

impl PgStore {
    /// Persists the public OLP job ID and exact selected target before a
    /// non-idempotent upstream create is attempted. No prompt or file metadata
    /// is accepted by this API.
    pub async fn reserve_media_job(
        &self,
        input: NewMediaJobReservation,
    ) -> Result<MediaJobRecord, MediaJobError> {
        validate_reservation(&input)?;
        let inserted = sqlx::query(
            "WITH authority AS (
                SELECT rpc.runtime_generation_id, rpc.provider_revision_id
                FROM runtime_generation_provider_configs rpc
                WHERE rpc.runtime_generation_id = $8 AND rpc.provider_id = $3
                  AND rpc.provider_revision_id IS NOT NULL
             )
             INSERT INTO async_media_jobs (
                id, upstream_job_id, api_key_id, provider_id, provider_model,
                route_slug, operation, surface, state, lifecycle_state,
                runtime_generation_id, provider_revision_id
             )
             SELECT $1, NULL, $2, $3, $4, $5, $6, $7, 'queued', 'creating',
                    authority.runtime_generation_id, authority.provider_revision_id
             FROM (SELECT 1) seed LEFT JOIN authority ON true
             WHERE authority.provider_revision_id IS NOT NULL
                OR NOT EXISTS (SELECT 1 FROM runtime_generations)",
        )
        .bind(input.id)
        .bind(input.api_key_id)
        .bind(input.provider_id)
        .bind(input.provider_model)
        .bind(input.route_slug)
        .bind(input.operation.as_str())
        .bind(media_surface_storage_value(input.surface))
        .bind(input.runtime_generation_id)
        .execute(self.pool())
        .await?;
        if inserted.rows_affected() != 1 {
            return Err(MediaJobError::Invalid(
                "the pinned runtime generation has no durable provider authority".to_owned(),
            ));
        }
        self.media_job(input.id).await
    }

    pub async fn media_job(&self, id: Uuid) -> Result<MediaJobRecord, MediaJobError> {
        let row = sqlx::query(
            "SELECT j.id, j.upstream_job_id, j.api_key_id, j.provider_id,
                    p.name AS provider_name, j.provider_model, j.route_slug,
                    j.operation, j.surface, j.state::text AS state, j.lifecycle_state,
                    j.progress_percent::real AS progress_percent,
                    j.content_available, j.expires_at, j.error_class,
                    j.completed_at, j.last_polled_at, j.reconciliation_error, j.deleted_at,
                    j.runtime_generation_id, j.provider_revision_id, j.reconciliation_claim_id,
                    j.reconciliation_attempts, j.next_reconciliation_at,
                    j.last_reconciliation_at, j.etag,
                    j.created_at, j.updated_at
             FROM async_media_jobs j
             JOIN providers p ON p.id = j.provider_id
             WHERE j.id = $1",
        )
        .bind(id)
        .fetch_optional(self.pool())
        .await?
        .ok_or(MediaJobError::NotFound)?;
        media_job_from_row(&row)
    }

    pub async fn media_jobs(
        &self,
        filters: &MediaJobFilters,
        cursor: Option<&TimestampCursor>,
        limit: u16,
    ) -> Result<Page<MediaJobRecord>, MediaJobError> {
        let limit = limit.clamp(1, MAX_PAGE_SIZE);
        let mut query = QueryBuilder::<Postgres>::new(
            "SELECT j.id, j.upstream_job_id, j.api_key_id, j.provider_id,
                    p.name AS provider_name, j.provider_model, j.route_slug,
                    j.operation, j.surface, j.state::text AS state, j.lifecycle_state,
                    j.progress_percent::real AS progress_percent,
                    j.content_available, j.expires_at, j.error_class,
                    j.completed_at, j.last_polled_at, j.reconciliation_error, j.deleted_at,
                    j.runtime_generation_id, j.provider_revision_id, j.reconciliation_claim_id,
                    j.reconciliation_attempts, j.next_reconciliation_at,
                    j.last_reconciliation_at, j.etag,
                    j.created_at, j.updated_at
             FROM async_media_jobs j
             JOIN providers p ON p.id = j.provider_id
             WHERE TRUE",
        );
        push_filters(&mut query, filters);
        if let Some(value) = cursor {
            query
                .push(" AND (j.created_at, j.id) < (")
                .push_bind(value.at)
                .push(", ")
                .push_bind(value.id)
                .push(")");
        }
        query
            .push(" ORDER BY j.created_at DESC, j.id DESC LIMIT ")
            .push_bind(i64::from(limit) + 1);
        let rows = query.build().fetch_all(self.pool()).await?;
        let items = rows
            .iter()
            .map(media_job_from_row)
            .collect::<Result<Vec<_>, _>>()?;
        let (items, next_cursor) = split_page(items, usize::from(limit), |last| {
            TimestampCursor {
                at: last.created_at,
                id: last.id,
            }
            .encode()
        });
        Ok(Page { items, next_cursor })
    }

    /// Client-facing video pagination uses the last public OLP video ID as its
    /// cursor, not the opaque management timestamp cursor.
    pub async fn media_jobs_after_id(
        &self,
        filters: &MediaJobFilters,
        after: Option<Uuid>,
        order: MediaJobOrder,
        limit: u16,
    ) -> Result<Page<MediaJobRecord>, MediaJobError> {
        let limit = limit.clamp(1, MAX_PAGE_SIZE);
        let position = if let Some(after) = after {
            let row = sqlx::query(
                "SELECT created_at, id FROM async_media_jobs
                 WHERE id = $1
                   AND lifecycle_state = 'active'
                   AND ($2::uuid IS NULL OR api_key_id = $2)
                   AND (cardinality($3::text[]) = 0 OR route_slug = ANY($3::text[]))
                   AND ($4::text IS NULL OR operation = $4)
                   AND ($5::text IS NULL OR surface = $5)",
            )
            .bind(after)
            .bind(filters.api_key_id)
            .bind(&filters.route_slugs)
            .bind(filters.operation.map(OperationKind::as_str))
            .bind(filters.surface.map(media_surface_storage_value))
            .fetch_optional(self.pool())
            .await?
            .ok_or_else(|| MediaJobError::Invalid("video cursor is invalid".to_owned()))?;
            Some((
                row.try_get::<DateTime<Utc>, _>("created_at")?,
                row.try_get::<Uuid, _>("id")?,
            ))
        } else {
            None
        };
        let mut query = QueryBuilder::<Postgres>::new(
            "SELECT j.id, j.upstream_job_id, j.api_key_id, j.provider_id,
                    p.name AS provider_name, j.provider_model, j.route_slug,
                    j.operation, j.surface, j.state::text AS state, j.lifecycle_state,
                    j.progress_percent::real AS progress_percent,
                    j.content_available, j.expires_at, j.error_class,
                    j.completed_at, j.last_polled_at, j.reconciliation_error, j.deleted_at,
                    j.runtime_generation_id, j.provider_revision_id, j.reconciliation_claim_id,
                    j.reconciliation_attempts, j.next_reconciliation_at,
                    j.last_reconciliation_at, j.etag,
                    j.created_at, j.updated_at
             FROM async_media_jobs j
             JOIN providers p ON p.id = j.provider_id
             WHERE j.lifecycle_state = 'active'",
        );
        push_filters(&mut query, filters);
        if let Some((created_at, id)) = position {
            query.push(" AND (j.created_at, j.id) ");
            match order {
                MediaJobOrder::Ascending => query.push(">"),
                MediaJobOrder::Descending => query.push("<"),
            };
            query
                .push(" (")
                .push_bind(created_at)
                .push(", ")
                .push_bind(id)
                .push(")");
        }
        match order {
            MediaJobOrder::Ascending => query.push(" ORDER BY j.created_at ASC, j.id ASC LIMIT "),
            MediaJobOrder::Descending => {
                query.push(" ORDER BY j.created_at DESC, j.id DESC LIMIT ")
            }
        };
        query.push_bind(i64::from(limit) + 1);
        let rows = query.build().fetch_all(self.pool()).await?;
        let items = rows
            .iter()
            .map(media_job_from_row)
            .collect::<Result<Vec<_>, _>>()?;
        let (items, next_cursor) =
            split_page(items, usize::from(limit), |last| last.id.to_string());
        Ok(Page { items, next_cursor })
    }

    pub async fn attach_media_job_upstream(
        &self,
        id: Uuid,
        upstream_job_id: &str,
        update: MediaJobUpdate,
    ) -> Result<MediaJobRecord, MediaJobError> {
        if upstream_job_id.trim().is_empty() {
            return Err(MediaJobError::Invalid(
                "upstream job ID cannot be empty".to_owned(),
            ));
        }
        validate_update(&update)?;
        let result = sqlx::query(
            "UPDATE async_media_jobs SET
                upstream_job_id = $2,
                state = $3::media_job_state,
                lifecycle_state = 'active',
                progress_percent = $4,
                content_available = $5,
                expires_at = $6,
                error_class = $7,
                last_polled_at = $8,
                reconciliation_error = NULL,
                etag = uuidv7()
             WHERE id = $1 AND lifecycle_state IN ('creating', 'create_ambiguous')",
        )
        .bind(id)
        .bind(upstream_job_id)
        .bind(update.state.as_str())
        .bind(update.progress_percent)
        .bind(update.content_available)
        .bind(update.expires_at)
        .bind(update.error_class)
        .bind(update.last_polled_at)
        .execute(self.pool())
        .await;
        match result {
            Ok(result) if result.rows_affected() == 1 => self.media_job(id).await,
            Ok(_) => Err(self.missing_or_changed(id).await?),
            Err(error) if is_upstream_identity_conflict(&error) => {
                Err(MediaJobError::UpstreamIdentityConflict)
            }
            Err(error) => Err(error.into()),
        }
    }

    pub async fn mark_media_job_create_ambiguous(
        &self,
        id: Uuid,
        reconciliation_error: &str,
    ) -> Result<MediaJobRecord, MediaJobError> {
        update_reconciliation_lifecycle(
            self,
            id,
            MediaJobLifecycle::CreateAmbiguous,
            None,
            reconciliation_error,
            &[
                MediaJobLifecycle::Creating,
                MediaJobLifecycle::CreateAmbiguous,
            ],
        )
        .await
    }

    pub async fn mark_media_job_create_cleanup_pending(
        &self,
        id: Uuid,
        upstream_job_id: &str,
        reconciliation_error: &str,
    ) -> Result<MediaJobRecord, MediaJobError> {
        if upstream_job_id.trim().is_empty() {
            return Err(MediaJobError::Invalid(
                "upstream job ID cannot be empty".to_owned(),
            ));
        }
        update_reconciliation_lifecycle(
            self,
            id,
            MediaJobLifecycle::CreateCleanupPending,
            Some(upstream_job_id),
            reconciliation_error,
            &[
                MediaJobLifecycle::Creating,
                MediaJobLifecycle::CreateAmbiguous,
                MediaJobLifecycle::CreateCleanupPending,
            ],
        )
        .await
    }

    /// Persists delete intent before contacting the pinned upstream target.
    /// Repeated calls return the same pending/deleted tombstone.
    pub async fn begin_media_job_deletion(
        &self,
        id: Uuid,
    ) -> Result<MediaJobRecord, MediaJobError> {
        let result = sqlx::query(
            "UPDATE async_media_jobs SET lifecycle_state = 'delete_pending',
                    reconciliation_error = NULL, next_reconciliation_at = now(),
                    etag = uuidv7()
             WHERE id = $1 AND lifecycle_state = 'active'",
        )
        .bind(id)
        .execute(self.pool())
        .await?;
        let record = self.media_job(id).await?;
        if result.rows_affected() == 1
            || matches!(
                record.lifecycle,
                MediaJobLifecycle::DeletePending | MediaJobLifecycle::Deleted
            )
        {
            Ok(record)
        } else {
            Err(MediaJobError::PreconditionFailed)
        }
    }

    pub async fn pending_media_reconciliation_jobs(
        &self,
        api_key_id: Uuid,
        limit: u16,
    ) -> Result<Vec<MediaJobRecord>, MediaJobError> {
        let rows = sqlx::query(
            "SELECT j.id, j.upstream_job_id, j.api_key_id, j.provider_id,
                    p.name AS provider_name, j.provider_model, j.route_slug,
                    j.operation, j.surface, j.state::text AS state, j.lifecycle_state,
                    j.progress_percent::real AS progress_percent,
                    j.content_available, j.expires_at, j.error_class,
                    j.completed_at, j.last_polled_at, j.reconciliation_error, j.deleted_at,
                    j.runtime_generation_id, j.provider_revision_id, j.reconciliation_claim_id,
                    j.reconciliation_attempts, j.next_reconciliation_at,
                    j.last_reconciliation_at, j.etag,
                    j.created_at, j.updated_at
             FROM async_media_jobs j JOIN providers p ON p.id = j.provider_id
             WHERE j.api_key_id = $1
               AND j.lifecycle_state IN ('create_cleanup_pending', 'delete_pending')
             ORDER BY j.updated_at ASC, j.id ASC LIMIT $2",
        )
        .bind(api_key_id)
        .bind(i64::from(limit.clamp(1, 32)))
        .fetch_all(self.pool())
        .await?;
        rows.iter().map(media_job_from_row).collect()
    }

    /// Claims a bounded cross-replica batch for autonomous lifecycle work.
    /// The database lease is deliberately longer than an ordinary route
    /// deadline; an expired lease can be recovered after process death.
    pub async fn claim_media_reconciliation_jobs(
        &self,
        now: DateTime<Utc>,
        limit: u16,
    ) -> Result<Vec<MediaJobRecord>, MediaJobError> {
        let claim_id = Uuid::now_v7();
        let rows = sqlx::query(
            "WITH candidates AS (
                SELECT id FROM async_media_jobs
                WHERE lifecycle_state <> 'deleted'
                  AND next_reconciliation_at <= $1
                  AND (reconciliation_claimed_until IS NULL
                       OR reconciliation_claimed_until <= $1)
                  AND (
                    lifecycle_state IN (
                        'create_ambiguous', 'create_cleanup_pending', 'delete_pending'
                    )
                    OR (lifecycle_state = 'creating'
                        AND updated_at <= $1 - interval '5 minutes')
                    OR (lifecycle_state = 'active'
                        AND upstream_job_id IS NOT NULL
                        AND (
                          (state IN ('queued', 'running')
                           AND (last_polled_at IS NULL
                                OR last_polled_at <= $1 - interval '5 seconds'))
                          OR expires_at <= $1
                          OR created_at <= $1 - interval '30 days'
                        ))
                  )
                ORDER BY
                    CASE WHEN lifecycle_state = 'active' THEN 1 ELSE 0 END,
                    next_reconciliation_at, created_at, id
                FOR UPDATE SKIP LOCKED
                LIMIT $2
             ), claimed AS (
                UPDATE async_media_jobs j SET
                    reconciliation_claim_id = $3,
                    reconciliation_claimed_until = $1 + interval '2 minutes',
                    last_reconciliation_at = $1,
                    next_reconciliation_at = $1 + interval '2 minutes',
                    reconciliation_attempts = reconciliation_attempts + 1,
                    etag = uuidv7()
                FROM candidates c WHERE j.id = c.id
                RETURNING j.*
             )
             SELECT c.id, c.upstream_job_id, c.api_key_id, c.provider_id,
                    p.name AS provider_name, c.provider_model, c.route_slug,
                    c.operation, c.surface, c.state::text AS state, c.lifecycle_state,
                    c.progress_percent::real AS progress_percent,
                    c.content_available, c.expires_at, c.error_class,
                    c.completed_at, c.last_polled_at, c.reconciliation_error, c.deleted_at,
                    c.runtime_generation_id, c.provider_revision_id, c.reconciliation_claim_id,
                    c.reconciliation_attempts, c.next_reconciliation_at,
                    c.last_reconciliation_at, c.etag, c.created_at, c.updated_at
             FROM claimed c JOIN providers p ON p.id = c.provider_id
             ORDER BY c.created_at, c.id",
        )
        .bind(now)
        .bind(i64::from(limit.clamp(1, 32)))
        .bind(claim_id)
        .fetch_all(self.pool())
        .await?;
        rows.iter().map(media_job_from_row).collect()
    }

    /// Releases one reconciliation lease and records only a bounded error
    /// class. Provider bodies and request content are never accepted here.
    pub async fn finish_media_reconciliation(
        &self,
        id: Uuid,
        claim_id: Uuid,
        next_attempt_at: DateTime<Utc>,
        error_class: Option<&str>,
    ) -> Result<(), MediaJobError> {
        if error_class.is_some_and(|value| value.is_empty() || value.len() > 120) {
            return Err(MediaJobError::Invalid(
                "reconciliation error class must contain 1-120 bytes".to_owned(),
            ));
        }
        let result = sqlx::query(
            "UPDATE async_media_jobs SET
                reconciliation_claim_id = NULL,
                reconciliation_claimed_until = NULL,
                next_reconciliation_at = $3,
                reconciliation_error = $4,
                etag = uuidv7()
             WHERE id = $1 AND reconciliation_claim_id = $2",
        )
        .bind(id)
        .bind(claim_id)
        .bind(next_attempt_at)
        .bind(error_class)
        .execute(self.pool())
        .await?;
        if result.rows_affected() != 1 {
            return Err(self.missing_or_changed(id).await?);
        }
        Ok(())
    }

    pub async fn media_reconciliation_summary(
        &self,
        now: DateTime<Utc>,
    ) -> Result<MediaReconciliationSummary, MediaJobError> {
        let row = sqlx::query(
            "SELECT COUNT(*) FILTER (
                        WHERE lifecycle_state NOT IN ('active', 'deleted')
                    )::bigint AS pending,
                    COUNT(*) FILTER (
                        WHERE (lifecycle_state = 'creating' AND updated_at < $1 - interval '5 minutes')
                           OR (lifecycle_state NOT IN ('creating', 'active', 'deleted')
                               AND next_reconciliation_at < $1 - interval '1 minute')
                           OR (lifecycle_state = 'active'
                               AND state IN ('queued', 'running')
                               AND next_reconciliation_at < $1 - interval '1 minute')
                    )::bigint AS stale,
                    COUNT(*) FILTER (
                        WHERE lifecycle_state <> 'deleted' AND reconciliation_error IS NOT NULL
                    )::bigint AS failed,
                    COUNT(*) FILTER (
                        WHERE lifecycle_state <> 'deleted'
                          AND (runtime_generation_id IS NULL OR provider_revision_id IS NULL)
                    )::bigint AS unbound,
                    MIN(created_at) FILTER (
                        WHERE lifecycle_state NOT IN ('active', 'deleted')
                    ) AS oldest_pending_at
             FROM async_media_jobs
             WHERE lifecycle_state <> 'deleted'",
        )
        .bind(now)
        .fetch_one(self.pool())
        .await?;
        Ok(MediaReconciliationSummary {
            pending: u64::try_from(row.try_get::<i64, _>("pending")?)
                .map_err(|_| MediaJobError::Invalid("pending count is invalid".to_owned()))?,
            stale: u64::try_from(row.try_get::<i64, _>("stale")?)
                .map_err(|_| MediaJobError::Invalid("stale count is invalid".to_owned()))?,
            failed: u64::try_from(row.try_get::<i64, _>("failed")?)
                .map_err(|_| MediaJobError::Invalid("failed count is invalid".to_owned()))?,
            unbound: u64::try_from(row.try_get::<i64, _>("unbound")?)
                .map_err(|_| MediaJobError::Invalid("unbound count is invalid".to_owned()))?,
            oldest_pending_at: row.try_get("oldest_pending_at")?,
        })
    }

    /// Applies an upstream poll result without exposing optimistic-lock races
    /// to client GET requests. Polls are serialized per job; stale results and
    /// state regressions are ignored, while terminal states remain immutable.
    pub async fn refresh_media_job(
        &self,
        id: Uuid,
        update: MediaJobUpdate,
    ) -> Result<MediaJobRecord, MediaJobError> {
        validate_update(&update)?;
        let mut transaction = self.pool().begin().await?;
        let row = sqlx::query(
            "SELECT j.id, j.upstream_job_id, j.api_key_id, j.provider_id,
                    p.name AS provider_name, j.provider_model, j.route_slug,
                    j.operation, j.surface, j.state::text AS state, j.lifecycle_state,
                    j.progress_percent::real AS progress_percent,
                    j.content_available, j.expires_at, j.error_class,
                    j.completed_at, j.last_polled_at, j.reconciliation_error, j.deleted_at,
                    j.runtime_generation_id, j.provider_revision_id, j.reconciliation_claim_id,
                    j.reconciliation_attempts, j.next_reconciliation_at,
                    j.last_reconciliation_at, j.etag,
                    j.created_at, j.updated_at
             FROM async_media_jobs j
             JOIN providers p ON p.id = j.provider_id
             WHERE j.id = $1 FOR UPDATE OF j",
        )
        .bind(id)
        .fetch_optional(&mut *transaction)
        .await?
        .ok_or(MediaJobError::NotFound)?;
        let current = media_job_from_row(&row)?;
        let stale = current
            .last_polled_at
            .is_some_and(|last| last > update.last_polled_at);
        if stale || !allows_refresh_transition(current.state, update.state) {
            transaction.commit().await?;
            return Ok(current);
        }
        sqlx::query(
            "UPDATE async_media_jobs SET
                state = $2::media_job_state,
                progress_percent = CASE
                    WHEN $3::real IS NULL THEN progress_percent
                    WHEN progress_percent IS NULL THEN $3::real::numeric
                    ELSE GREATEST(progress_percent, $3::real::numeric)
                END,
                content_available = content_available OR $4,
                expires_at = COALESCE($5, expires_at),
                error_class = COALESCE($6, error_class),
                last_polled_at = $7,
                etag = uuidv7()
             WHERE id = $1",
        )
        .bind(id)
        .bind(update.state.as_str())
        .bind(update.progress_percent)
        .bind(update.content_available)
        .bind(update.expires_at)
        .bind(update.error_class)
        .bind(update.last_polled_at)
        .execute(&mut *transaction)
        .await?;
        transaction.commit().await?;
        self.media_job(id).await
    }

    /// Finalizes a deletion already accepted by the upstream provider.
    ///
    /// Status polling is allowed to rotate a job's ETag while the upstream
    /// delete is in flight, so optimistic locking is unsafe at this point. Job
    /// IDs are never reused and ownership was checked before transport. A
    /// metadata-only tombstone makes retries idempotent and preserves evidence
    /// that PostgreSQL finalization lagged the upstream side effect.
    pub async fn finalize_media_job_deletion(&self, id: Uuid) -> Result<bool, MediaJobError> {
        let result = sqlx::query(
            "UPDATE async_media_jobs
             SET lifecycle_state = 'deleted', deleted_at = COALESCE(deleted_at, now()),
                 reconciliation_error = NULL, content_available = false, etag = uuidv7()
             WHERE id = $1
               AND lifecycle_state IN (
                   'creating', 'create_ambiguous', 'create_cleanup_pending', 'delete_pending'
               )",
        )
        .bind(id)
        .execute(self.pool())
        .await?;
        Ok(result.rows_affected() == 1)
    }

    async fn missing_or_changed(&self, id: Uuid) -> Result<MediaJobError, sqlx::Error> {
        let exists = sqlx::query_scalar::<_, bool>(
            "SELECT EXISTS (SELECT 1 FROM async_media_jobs WHERE id = $1)",
        )
        .bind(id)
        .fetch_one(self.pool())
        .await?;
        Ok(if exists {
            MediaJobError::PreconditionFailed
        } else {
            MediaJobError::NotFound
        })
    }
}

fn push_filters(query: &mut QueryBuilder<Postgres>, filters: &MediaJobFilters) {
    if let Some(value) = filters.api_key_id {
        query.push(" AND j.api_key_id = ").push_bind(value);
    }
    if let Some(value) = filters.provider_id {
        query.push(" AND j.provider_id = ").push_bind(value);
    }
    if let Some(value) = &filters.route_slug {
        query.push(" AND j.route_slug = ").push_bind(value);
    }
    if !filters.route_slugs.is_empty() {
        query
            .push(" AND j.route_slug = ANY(")
            .push_bind(&filters.route_slugs)
            .push("::text[])");
    }
    if let Some(value) = filters.operation {
        query.push(" AND j.operation = ").push_bind(value.as_str());
    }
    if let Some(value) = filters.surface {
        query
            .push(" AND j.surface = ")
            .push_bind(media_surface_storage_value(value));
    }
    if let Some(value) = filters.state {
        query
            .push(" AND j.state = ")
            .push_bind(value.as_str())
            .push("::media_job_state");
    }
    if let Some(value) = filters.lifecycle {
        query
            .push(" AND j.lifecycle_state = ")
            .push_bind(value.as_str());
    }
    if let Some(value) = filters.created_after {
        query.push(" AND j.created_at >= ").push_bind(value);
    }
    if let Some(value) = filters.created_before {
        query.push(" AND j.created_at < ").push_bind(value);
    }
}

fn validate_reservation(input: &NewMediaJobReservation) -> Result<(), MediaJobError> {
    if input.id.get_version_num() != 7
        || input.provider_model.trim().is_empty()
        || input.route_slug.trim().is_empty()
    {
        return Err(MediaJobError::Invalid(
            "reservation ID, provider model, route, and operation are required".to_owned(),
        ));
    }
    Ok(())
}

const fn media_surface_storage_value(surface: Surface) -> &'static str {
    match surface {
        Surface::OpenAi => "openai",
        Surface::Anthropic => "anthropic",
        Surface::Gemini => "gemini",
    }
}

fn parse_media_surface_storage_value(value: &str) -> Result<Surface, MediaJobError> {
    match value {
        "openai" => Ok(Surface::OpenAi),
        "anthropic" => Ok(Surface::Anthropic),
        "gemini" => Ok(Surface::Gemini),
        _ => Err(MediaJobError::Invalid(
            "database returned an unknown surface".to_owned(),
        )),
    }
}

async fn update_reconciliation_lifecycle(
    store: &PgStore,
    id: Uuid,
    lifecycle: MediaJobLifecycle,
    upstream_job_id: Option<&str>,
    reconciliation_error: &str,
    allowed: &[MediaJobLifecycle],
) -> Result<MediaJobRecord, MediaJobError> {
    let allowed = allowed
        .iter()
        .map(|value| value.as_str())
        .collect::<Vec<_>>();
    let result = sqlx::query(
        "UPDATE async_media_jobs SET lifecycle_state = $2,
                upstream_job_id = COALESCE($3, upstream_job_id),
                reconciliation_error = $4, next_reconciliation_at = now(), etag = uuidv7()
         WHERE id = $1 AND lifecycle_state = ANY($5::text[])",
    )
    .bind(id)
    .bind(lifecycle.as_str())
    .bind(upstream_job_id)
    .bind(reconciliation_error)
    .bind(allowed)
    .execute(store.pool())
    .await?;
    if result.rows_affected() == 0 {
        return Err(store.missing_or_changed(id).await?);
    }
    store.media_job(id).await
}

fn is_upstream_identity_conflict(error: &sqlx::Error) -> bool {
    error
        .as_database_error()
        .and_then(sqlx::error::DatabaseError::constraint)
        == Some("async_media_jobs_upstream_unique_idx")
}

fn validate_update(update: &MediaJobUpdate) -> Result<(), MediaJobError> {
    if update.content_available && update.state != MediaJobState::Succeeded {
        return Err(MediaJobError::Invalid(
            "content is available only for a succeeded job".to_owned(),
        ));
    }
    if update.error_class.is_some() && update.state != MediaJobState::Failed {
        return Err(MediaJobError::Invalid(
            "an error class is valid only for a failed job".to_owned(),
        ));
    }
    validate_progress(update.progress_percent)
}

const fn allows_refresh_transition(current: MediaJobState, incoming: MediaJobState) -> bool {
    match current {
        MediaJobState::Queued => true,
        MediaJobState::Running => !matches!(incoming, MediaJobState::Queued),
        MediaJobState::Succeeded => matches!(incoming, MediaJobState::Succeeded),
        MediaJobState::Failed => matches!(incoming, MediaJobState::Failed),
        MediaJobState::Cancelled => matches!(incoming, MediaJobState::Cancelled),
    }
}

fn validate_progress(value: Option<f32>) -> Result<(), MediaJobError> {
    if value.is_some_and(|value| !value.is_finite() || !(0.0..=100.0).contains(&value)) {
        return Err(MediaJobError::Invalid(
            "progress must be a finite percentage from 0 through 100".to_owned(),
        ));
    }
    Ok(())
}

fn media_job_from_row(row: &sqlx::postgres::PgRow) -> Result<MediaJobRecord, MediaJobError> {
    Ok(MediaJobRecord {
        id: row.try_get("id")?,
        upstream_job_id: row.try_get("upstream_job_id")?,
        api_key_id: row.try_get("api_key_id")?,
        provider_id: row.try_get("provider_id")?,
        provider_name: row.try_get("provider_name")?,
        provider_model: row.try_get("provider_model")?,
        route_slug: row.try_get("route_slug")?,
        operation: row
            .try_get::<String, _>("operation")?
            .parse()
            .map_err(|_| {
                MediaJobError::Invalid("database returned an unknown operation".to_owned())
            })?,
        surface: parse_media_surface_storage_value(row.try_get("surface")?)?,
        state: MediaJobState::parse(row.try_get("state")?)?,
        lifecycle: MediaJobLifecycle::parse(row.try_get("lifecycle_state")?)?,
        progress_percent: row.try_get("progress_percent")?,
        content_available: row.try_get("content_available")?,
        expires_at: row.try_get("expires_at")?,
        error_class: row.try_get("error_class")?,
        completed_at: row.try_get("completed_at")?,
        last_polled_at: row.try_get("last_polled_at")?,
        reconciliation_error: row.try_get("reconciliation_error")?,
        deleted_at: row.try_get("deleted_at")?,
        runtime_generation_id: row.try_get("runtime_generation_id")?,
        provider_revision_id: row.try_get("provider_revision_id")?,
        reconciliation_claim_id: row.try_get("reconciliation_claim_id")?,
        reconciliation_attempts: u32::try_from(row.try_get::<i32, _>("reconciliation_attempts")?)
            .map_err(|_| {
            MediaJobError::Invalid("reconciliation attempt count is invalid".to_owned())
        })?,
        next_reconciliation_at: row.try_get("next_reconciliation_at")?,
        last_reconciliation_at: row.try_get("last_reconciliation_at")?,
        etag: row.try_get("etag")?,
        created_at: row.try_get("created_at")?,
        updated_at: row.try_get("updated_at")?,
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn rejects_nonfinite_progress_and_inconsistent_result_state() {
        assert!(validate_progress(Some(f32::NAN)).is_err());
        assert!(validate_progress(Some(100.1)).is_err());
        assert!(
            validate_update(&MediaJobUpdate {
                state: MediaJobState::Running,
                progress_percent: Some(50.0),
                content_available: true,
                expires_at: None,
                error_class: None,
                last_polled_at: Utc::now(),
            })
            .is_err()
        );
    }

    #[test]
    fn refresh_transitions_never_regress_or_change_terminal_outcomes() {
        assert!(allows_refresh_transition(
            MediaJobState::Queued,
            MediaJobState::Succeeded
        ));
        assert!(!allows_refresh_transition(
            MediaJobState::Running,
            MediaJobState::Queued
        ));
        assert!(!allows_refresh_transition(
            MediaJobState::Succeeded,
            MediaJobState::Running
        ));
        assert!(!allows_refresh_transition(
            MediaJobState::Failed,
            MediaJobState::Cancelled
        ));
        assert!(MediaJobLifecycle::Creating.needs_reconciliation());
        assert!(MediaJobLifecycle::DeletePending.needs_reconciliation());
        assert!(!MediaJobLifecycle::Active.needs_reconciliation());
        assert!(!MediaJobLifecycle::Deleted.needs_reconciliation());
    }
}
