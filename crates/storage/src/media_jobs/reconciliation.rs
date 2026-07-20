use chrono::{DateTime, Utc};
use sqlx::Row;
use uuid::Uuid;

use crate::PgStore;

use super::{
    MediaJobError, MediaJobRecord, MediaReconciliationSummary, queries::media_job_from_row,
};

impl PgStore {
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
}
