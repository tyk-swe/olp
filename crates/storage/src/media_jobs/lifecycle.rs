use uuid::Uuid;

use crate::PgStore;

use super::{
    MediaJobError, MediaJobLifecycle, MediaJobRecord, MediaJobState, MediaJobUpdate,
    NewMediaJobReservation, queries::media_job_from_row,
};

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
        .bind(input.upstream_model)
        .bind(input.route_slug)
        .bind(input.operation.as_str())
        .bind(input.surface.as_str())
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
            "WITH attached AS (
                UPDATE async_media_jobs SET
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
                 WHERE id = $1 AND lifecycle_state IN ('creating', 'create_ambiguous')
                 RETURNING *
             )
             SELECT j.id, j.upstream_job_id, j.api_key_id, j.provider_id,
                    p.name AS provider_name, j.provider_model, j.route_slug,
                    j.operation, j.surface, j.state::text AS state, j.lifecycle_state,
                    j.progress_percent::real AS progress_percent,
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
        .bind(update.error_class)
        .bind(update.last_polled_at)
        .fetch_optional(self.pool())
        .await;
        match result {
            // Return the row from the same statement that made it active. A
            // connection failure after commit is retried by the caller, so a
            // subsequent active row with this exact identity is also success.
            Ok(Some(row)) => media_job_from_row(&row),
            Ok(None) => {
                let current = self.media_job(id).await?;
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

    pub(super) async fn missing_or_changed(&self, id: Uuid) -> Result<MediaJobError, sqlx::Error> {
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

pub(super) fn validate_update(update: &MediaJobUpdate) -> Result<(), MediaJobError> {
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

pub(super) const fn allows_refresh_transition(
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

pub(super) fn validate_progress(value: Option<f32>) -> Result<(), MediaJobError> {
    if value.is_some_and(|value| !value.is_finite() || !(0.0..=100.0).contains(&value)) {
        return Err(MediaJobError::Invalid(
            "progress must be a finite percentage from 0 through 100".to_owned(),
        ));
    }
    Ok(())
}
