use sqlx::Postgres;
use sqlx::QueryBuilder;
use uuid::Uuid;

use sqlx::PgPool;

use crate::media::jobs::MediaJobError;
use crate::media::jobs::MediaJobLifecycle;
use crate::media::jobs::MediaJobRecord;
use crate::media::jobs::MediaJobState;
use crate::media::jobs::MediaJobUpdate;
use crate::media::jobs::NewMediaJobReservation;
use crate::media::jobs::POLL_GATE_SECONDS;
use crate::media::jobs::queries::MEDIA_JOB_SELECT;
use crate::media::jobs::queries::MediaJobRow;
use crate::media::jobs::queries::media_job_from_row;

/// Persists the public OLP job ID and exact selected target before a
/// non-idempotent upstream create is attempted. No prompt or file metadata
/// is accepted by this API.
pub async fn reserve_media_job(
    pool: &sqlx::PgPool,
    input: NewMediaJobReservation,
) -> Result<MediaJobRecord, MediaJobError> {
    validate_reservation(&input)?;
    let mut transaction = pool
        .begin_with("BEGIN ISOLATION LEVEL READ COMMITTED")
        .await?;
    // Acquire admission authority before taking the INSERT's fresh snapshot.
    sqlx::query_as::<_, ReserveMediaJobRow>("SELECT id FROM providers WHERE id = $1 FOR SHARE")
        .bind(input.provider_id)
        .fetch_optional(&mut *transaction)
        .await?;
    let inserted = sqlx::query(
        "WITH authority AS (
                SELECT rpc.runtime_generation_id, rpc.provider_revision_id
                FROM runtime_generation_provider_configs rpc
                JOIN providers provider ON provider.id = rpc.provider_id
                JOIN provider_revisions current ON current.id = provider.active_revision_id
                WHERE rpc.runtime_generation_id = $8 AND rpc.provider_id = $3
                  AND rpc.kind IS NOT DISTINCT FROM current.kind
                  AND rpc.endpoint IS NOT DISTINCT FROM current.endpoint
                  AND rpc.cloud_region IS NOT DISTINCT FROM current.cloud_region
                  AND rpc.cloud_project IS NOT DISTINCT FROM current.cloud_project
                  AND rpc.deployment IS NOT DISTINCT FROM current.deployment
                  AND rpc.api_version IS NOT DISTINCT FROM current.api_version
                  AND rpc.auth_mode IS NOT DISTINCT FROM current.auth_mode
                  AND rpc.active_credential_version_id
                      IS NOT DISTINCT FROM current.credential_version_id
                  AND EXISTS (
                    SELECT 1 FROM provider_revision_models prm
                    WHERE prm.provider_revision_id = current.id
                      AND prm.upstream_model = $4 AND prm.enabled
                      AND NOT EXISTS (
                        SELECT required.operation
                        FROM (VALUES ('video_get'), ('video_content'), ('video_delete'))
                             AS required(operation)
                        WHERE NOT EXISTS (
                          SELECT 1 FROM provider_revision_capabilities prc
                          WHERE prc.provider_revision_model_id = prm.id
                            AND prc.operation = required.operation AND prc.surface = $7
                            AND prc.mode = 'unary' AND prc.source = 'certified'
                        )
                      )
                  )
             )
             INSERT INTO async_media_jobs (
                id, upstream_job_id, api_key_id, provider_id, provider_model,
                route_slug, operation, surface, state, lifecycle_state,
                runtime_generation_id, provider_revision_id
             )
             SELECT $1, NULL, $2, $3, $4, $5, $6, $7, 'queued', 'creating',
                    authority.runtime_generation_id, authority.provider_revision_id
             FROM authority
             WHERE EXISTS (SELECT 1 FROM providers
                           WHERE id = $3 AND state <> 'disabled'::provider_state)",
    )
    .bind(input.id)
    .bind(input.api_key_id)
    .bind(input.provider_id)
    .bind(&input.upstream_model)
    .bind(&input.route_slug)
    .bind(input.operation.as_str())
    .bind(input.surface.as_str())
    .bind(input.runtime_generation_id)
    .execute(&mut *transaction)
    .await?;
    if inserted.rows_affected() != 1 {
        return Err(MediaJobError::Invalid(
                "the pinned provider authority is unavailable or incompatible with current video support".to_owned(),
            ));
    }
    transaction.commit().await?;
    crate::media::jobs::queries::media_job(pool, input.id).await
}

pub async fn attach_media_job_upstream(
    pool: &sqlx::PgPool,
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
    let result = sqlx::query_as::<_, MediaJobRow>(
        "WITH attached AS (
                UPDATE async_media_jobs SET
                    upstream_job_id = $2,
                    state = $3::text::media_job_state,
                    lifecycle_state = 'active',
                    progress_percent = $4::real::numeric,
                    content_available = $5,
                    expires_at = $6,
                    error_class = $7,
                    last_polled_at = $8,
                    reconciliation_error = NULL,
                    etag = uuidv7()
                 WHERE id = $1 AND lifecycle_state = 'creating'
                 RETURNING *
             )
             SELECT j.id, j.upstream_job_id, j.api_key_id, j.provider_id,
                    p.name AS provider_name, j.provider_model, j.route_slug,
                    j.operation, j.surface, j.state::text AS \"state\", j.lifecycle_state,
                    j.progress_percent::real AS \"progress_percent\",
                    j.content_available, j.expires_at, j.error_class,
                    j.completed_at, j.last_polled_at, j.reconciliation_error, j.deleted_at,
                    j.runtime_generation_id, j.provider_revision_id, j.reconciliation_claim_id,
                    j.reconciliation_attempts, j.next_reconciliation_at,
                    j.last_reconciliation_at, j.etag,
                    j.created_at, j.updated_at
             FROM attached j
             JOIN providers p ON p.id = j.provider_id",
    )
    .bind(id)
    .bind(upstream_job_id)
    .bind(update.state.as_str())
    .bind(update.progress_percent)
    .bind(update.content_available)
    .bind(update.expires_at)
    .bind(&update.error_class)
    .bind(update.last_polled_at)
    .fetch_optional(pool)
    .await;
    match result {
        // Return the row from the same statement that made it active. A
        // connection failure after commit is retried by the caller, so a
        // subsequent active row with this exact identity is also success.
        Ok(Some(row)) => media_job_from_row(row),
        Ok(None) => {
            let current = crate::media::jobs::queries::media_job(pool, id).await?;
            if current.lifecycle == MediaJobLifecycle::Active
                && current.upstream_job_id.as_deref() == Some(upstream_job_id)
            {
                Ok(current)
            } else {
                Err(MediaJobError::PreconditionFailed)
            }
        }
        Err(error) if is_upstream_identity_conflict(&error) => {
            Err(MediaJobError::UpstreamIdentityConflict)
        }
        Err(error) => Err(error.into()),
    }
}

pub async fn mark_media_job_create_ambiguous(
    pool: &sqlx::PgPool,
    id: Uuid,
    reconciliation_error: &str,
) -> Result<MediaJobRecord, MediaJobError> {
    update_reconciliation_lifecycle(
        pool,
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
    pool: &sqlx::PgPool,
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
        pool,
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
    pool: &sqlx::PgPool,
    id: Uuid,
) -> Result<MediaJobRecord, MediaJobError> {
    let result = sqlx::query(
        "UPDATE async_media_jobs SET lifecycle_state = 'delete_pending',
                    reconciliation_error = NULL, next_reconciliation_at = now(),
                    etag = uuidv7()
             WHERE id = $1 AND lifecycle_state = 'active'",
    )
    .bind(id)
    .execute(pool)
    .await?;
    let record = crate::media::jobs::queries::media_job(pool, id).await?;
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

/// Applies an upstream poll result without exposing optimistic-lock races
/// to client GET requests. Polls are serialized per job; stale results and
/// state regressions are ignored, while terminal states remain immutable.
pub async fn refresh_media_job(
    pool: &sqlx::PgPool,
    id: Uuid,
    update: MediaJobUpdate,
) -> Result<MediaJobRecord, MediaJobError> {
    validate_update(&update)?;
    let mut transaction = pool.begin().await?;
    let row = QueryBuilder::<Postgres>::new(MEDIA_JOB_SELECT)
        .push(" WHERE j.id = ")
        .push_bind(id)
        .push(" FOR UPDATE OF j")
        .build_query_as::<MediaJobRow>()
        .fetch_optional(&mut *transaction)
        .await?
        .ok_or(MediaJobError::NotFound)?;
    let current = media_job_from_row(row)?;
    let stale = current
        .last_polled_at
        .is_some_and(|last| last > update.last_polled_at);
    if stale || !allows_refresh_transition(current.state, update.state) {
        transaction.commit().await?;
        return Ok(current);
    }
    sqlx::query(
        "UPDATE async_media_jobs SET
                state = $2::text::media_job_state,
                progress_percent = CASE
                    WHEN $3::real IS NULL THEN progress_percent
                    WHEN progress_percent IS NULL THEN $3::real::numeric
                    ELSE GREATEST(progress_percent, $3::real::numeric)
                END,
                content_available = content_available OR $4,
                expires_at = COALESCE($5, expires_at),
                error_class = COALESCE($6, error_class),
                last_polled_at = $7,
                -- A client polling faster than the reconciler's poll gate
                -- keeps the job unclaimable. Carry next_reconciliation_at past
                -- that gate too, or the job reads as reconciliation-stale while
                -- it is in fact healthy and being watched.
                next_reconciliation_at =
                    GREATEST(
                        next_reconciliation_at,
                        $7::timestamptz + $8::int * interval '1 second'
                    ),
                etag = uuidv7()
             WHERE id = $1",
    )
    .bind(id)
    .bind(update.state.as_str())
    .bind(update.progress_percent)
    .bind(update.content_available)
    .bind(update.expires_at)
    .bind(&update.error_class)
    .bind(update.last_polled_at)
    .bind(POLL_GATE_SECONDS)
    .execute(&mut *transaction)
    .await?;
    transaction.commit().await?;
    crate::media::jobs::queries::media_job(pool, id).await
}

/// Finalizes a deletion already accepted by the upstream provider.
///
/// Status polling is allowed to rotate a job's ETag while the upstream
/// delete is in flight, so optimistic locking is unsafe at this point. Job
/// IDs are never reused and ownership was checked before transport. A
/// metadata-only tombstone makes retries idempotent and preserves evidence
/// that PostgreSQL finalization lagged the upstream side effect.
pub async fn finalize_media_job_deletion(
    pool: &sqlx::PgPool,
    id: Uuid,
) -> Result<bool, MediaJobError> {
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
    .execute(pool)
    .await?;
    Ok(result.rows_affected() == 1)
}

pub(crate) async fn missing_or_changed(
    pool: &sqlx::PgPool,
    id: Uuid,
) -> Result<MediaJobError, sqlx::Error> {
    let exists = sqlx::query_scalar::<_, bool>(
        "SELECT EXISTS (SELECT 1 FROM async_media_jobs WHERE id = $1) AS \"value\"",
    )
    .bind(id)
    .fetch_one(pool)
    .await?;
    Ok(if exists {
        MediaJobError::PreconditionFailed
    } else {
        MediaJobError::NotFound
    })
}

fn validate_reservation(input: &NewMediaJobReservation) -> Result<(), MediaJobError> {
    if input.id.get_version_num() != 7
        || input.upstream_model.trim().is_empty()
        || input.route_slug.trim().is_empty()
    {
        return Err(MediaJobError::Invalid(
            "reservation ID, provider model, route, and operation are required".to_owned(),
        ));
    }
    Ok(())
}

async fn update_reconciliation_lifecycle(
    pool: &PgPool,
    id: Uuid,
    lifecycle: MediaJobLifecycle,
    upstream_job_id: Option<&str>,
    reconciliation_error: &str,
    allowed: &[MediaJobLifecycle],
) -> Result<MediaJobRecord, MediaJobError> {
    let allowed = allowed
        .iter()
        .map(|value| value.as_str().to_owned())
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
    .bind(&allowed)
    .execute(pool)
    .await?;
    if result.rows_affected() == 0 {
        return Err(crate::media::jobs::lifecycle::missing_or_changed(pool, id).await?);
    }
    crate::media::jobs::queries::media_job(pool, id).await
}

fn is_upstream_identity_conflict(error: &sqlx::Error) -> bool {
    error
        .as_database_error()
        .and_then(sqlx::error::DatabaseError::constraint)
        == Some("async_media_jobs_upstream_unique_idx")
}

pub(crate) fn validate_update(update: &MediaJobUpdate) -> Result<(), MediaJobError> {
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

pub(crate) const fn allows_refresh_transition(
    current: MediaJobState,
    incoming: MediaJobState,
) -> bool {
    match current {
        MediaJobState::Queued => true,
        MediaJobState::Running => !matches!(incoming, MediaJobState::Queued),
        MediaJobState::Succeeded => matches!(incoming, MediaJobState::Succeeded),
        MediaJobState::Failed => matches!(incoming, MediaJobState::Failed),
        MediaJobState::Cancelled => matches!(incoming, MediaJobState::Cancelled),
    }
}

pub(crate) fn validate_progress(value: Option<f32>) -> Result<(), MediaJobError> {
    if value.is_some_and(|value| !value.is_finite() || !(0.0..=100.0).contains(&value)) {
        return Err(MediaJobError::Invalid(
            "progress must be a finite percentage from 0 through 100".to_owned(),
        ));
    }
    Ok(())
}

#[derive(sqlx::FromRow)]
struct ReserveMediaJobRow {}
