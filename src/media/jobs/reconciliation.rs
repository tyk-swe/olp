use chrono::DateTime;
use chrono::Utc;
use sqlx::Postgres;
use sqlx::QueryBuilder;
use uuid::Uuid;

use crate::media::jobs::MediaJobError;
use crate::media::jobs::MediaJobRecord;
use crate::media::jobs::MediaReconciliationSummary;
use crate::media::jobs::POLL_GATE_SECONDS;
use crate::media::jobs::queries::MEDIA_JOB_SELECT;
use crate::media::jobs::queries::MediaJobRow;
use crate::media::jobs::queries::media_job_from_row;

#[cfg(any(test, feature = "test-util"))]
pub async fn pending_media_reconciliation_jobs(
    pool: &sqlx::PgPool,
    api_key_id: Uuid,
    limit: u16,
) -> Result<Vec<MediaJobRecord>, MediaJobError> {
    let rows = QueryBuilder::<Postgres>::new(MEDIA_JOB_SELECT)
        .push(" WHERE j.api_key_id = ")
        .push_bind(api_key_id)
        .push(" AND j.lifecycle_state IN ('create_cleanup_pending', 'delete_pending')")
        .push(" ORDER BY j.updated_at ASC, j.id ASC LIMIT ")
        .push_bind(i64::from(limit.clamp(1, 32)))
        .build_query_as::<MediaJobRow>()
        .fetch_all(pool)
        .await?;
    rows.into_iter().map(media_job_from_row).collect()
}

/// Claims a bounded cross-replica batch for autonomous lifecycle work.
/// The database lease is deliberately longer than an ordinary route
/// deadline; an expired lease can be recovered after process death.
pub async fn claim_media_reconciliation_jobs(
    pool: &sqlx::PgPool,
    now: DateTime<Utc>,
    limit: u16,
) -> Result<Vec<MediaJobRecord>, MediaJobError> {
    let claim_id = Uuid::now_v7();
    let rows = sqlx::query_as::<_, MediaJobRow>(
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
                                OR last_polled_at
                                   <= $1 - $4::int * interval '1 second'))
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
                    c.operation, c.surface, c.state::text AS \"state\", c.lifecycle_state,
                    c.progress_percent::real AS \"progress_percent\",
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
    .bind(POLL_GATE_SECONDS)
    .fetch_all(pool)
    .await?;
    rows.into_iter().map(media_job_from_row).collect()
}

/// Releases one reconciliation lease and records only a bounded error
/// class. Provider bodies and request content are never accepted here.
pub async fn finish_media_reconciliation(
    pool: &sqlx::PgPool,
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
                reconciliation_attempts =
                    CASE WHEN $4::text IS NULL THEN 0 ELSE reconciliation_attempts END,
                etag = uuidv7()
             WHERE id = $1 AND reconciliation_claim_id = $2",
    )
    .bind(id)
    .bind(claim_id)
    .bind(next_attempt_at)
    .bind(error_class)
    .execute(pool)
    .await?;
    if result.rows_affected() != 1 {
        return Err(crate::media::jobs::lifecycle::missing_or_changed(pool, id).await?);
    }
    Ok(())
}

pub async fn media_reconciliation_summary(
    pool: &sqlx::PgPool,
    now: DateTime<Utc>,
) -> Result<MediaReconciliationSummary, MediaJobError> {
    let row = sqlx::query_as::<_, MediaReconciliationSummaryRow>(
        "SELECT COUNT(*) FILTER (
                        WHERE lifecycle_state NOT IN ('active', 'deleted')
                    )::bigint AS \"pending\",
                    COUNT(*) FILTER (
                        WHERE (lifecycle_state = 'creating' AND updated_at < $1::timestamptz - interval '5 minutes')
                           OR (lifecycle_state NOT IN ('creating', 'active', 'deleted')
                               AND next_reconciliation_at < $1::timestamptz - interval '1 minute')
                           OR (lifecycle_state = 'active'
                               AND state IN ('queued', 'running')
                               AND next_reconciliation_at < $1::timestamptz - interval '1 minute'
                               -- Mirror the claim query's poll gate. A job a
                               -- client is actively polling is deliberately not
                               -- claimable and must not read as stale.
                               AND (last_polled_at IS NULL
                                    OR last_polled_at
                                       <= $1::timestamptz - $2::int * interval '1 second'))
                    )::bigint AS \"stale\",
                    COUNT(*) FILTER (
                        WHERE lifecycle_state <> 'deleted' AND reconciliation_error IS NOT NULL
                    )::bigint AS \"failed\",
                    MIN(created_at) FILTER (
                        WHERE lifecycle_state NOT IN ('active', 'deleted')
                    ) AS oldest_pending_at
             FROM async_media_jobs
             WHERE lifecycle_state <> 'deleted'",
    )
    .bind(now)
    .bind(POLL_GATE_SECONDS)
        .fetch_one(pool)
        .await?;
    Ok(MediaReconciliationSummary {
        pending: u64::try_from(row.pending)
            .map_err(|_| MediaJobError::Invalid("pending count is invalid".to_owned()))?,
        stale: u64::try_from(row.stale)
            .map_err(|_| MediaJobError::Invalid("stale count is invalid".to_owned()))?,
        failed: u64::try_from(row.failed)
            .map_err(|_| MediaJobError::Invalid("failed count is invalid".to_owned()))?,
        oldest_pending_at: row.oldest_pending_at,
    })
}

#[derive(sqlx::FromRow)]
struct MediaReconciliationSummaryRow {
    pending: i64,
    stale: i64,
    failed: i64,
    oldest_pending_at: Option<chrono::DateTime<chrono::Utc>>,
}
