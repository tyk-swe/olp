use olp_domain::RouteSlug;
use sqlx::{Postgres, Transaction};
use uuid::Uuid;

use crate::{
    IdempotencyOutcome, IdempotencyResponse, PersistenceError, PgStore, ReplayableIdempotency,
    runtime_compiler::{compile_and_publish_runtime_in_transaction, lock_runtime_publication},
    store::{
        ReplayableIdempotencyClaim, claim_idempotency, claim_replayable_idempotency,
        complete_idempotency, complete_replayable_idempotency,
    },
};

use super::{ConfigurationError, NewRouteDraft, RouteActivated, RouteDraftCreated};

impl PgStore {
    pub async fn create_route_draft<F>(
        &self,
        route: NewRouteDraft,
        replay: ReplayableIdempotency<'_>,
        build_response: F,
    ) -> Result<IdempotencyOutcome<RouteDraftCreated>, ConfigurationError>
    where
        F: FnOnce(&RouteDraftCreated) -> Result<IdempotencyResponse, PersistenceError>,
    {
        let mut transaction = self.pool().begin().await?;
        match claim_replayable_idempotency(
            &mut transaction,
            route.actor,
            "route.create_draft",
            &route.idempotency_key,
            replay.request_fingerprint(),
            replay.master_key(),
        )
        .await?
        {
            ReplayableIdempotencyClaim::Execute => {}
            ReplayableIdempotencyClaim::Replay(response) => {
                transaction.rollback().await?;
                return Ok(IdempotencyOutcome::Replayed(response));
            }
            ReplayableIdempotencyClaim::Conflict => {
                transaction.rollback().await?;
                return Err(ConfigurationError::IdempotencyConflict);
            }
            ReplayableIdempotencyClaim::InProgress => {
                transaction.rollback().await?;
                return Err(ConfigurationError::IdempotencyInProgress);
            }
        }
        let slug = RouteSlug::parse(route.slug)
            .map_err(|error| ConfigurationError::InvalidRoute(error.to_string()))?;
        if route.operations.is_empty() {
            return Err(ConfigurationError::InvalidRoute(
                "at least one operation is required".to_owned(),
            ));
        }
        if route.targets.is_empty() {
            return Err(ConfigurationError::InvalidRoute(
                "at least one target is required".to_owned(),
            ));
        }
        if route.max_attempts == 0 || usize::from(route.max_attempts) > route.targets.len() {
            return Err(ConfigurationError::InvalidRoute(
                "max_attempts must be between one and the target count".to_owned(),
            ));
        }
        let overall_timeout_ms = i32::try_from(route.overall_timeout_ms).map_err(|_| {
            ConfigurationError::InvalidRoute("overall timeout is too large".to_owned())
        })?;
        if overall_timeout_ms <= 0 {
            return Err(ConfigurationError::InvalidRoute(
                "overall timeout must be positive".to_owned(),
            ));
        }
        let id = Uuid::now_v7();
        let routing_id = Uuid::now_v7();
        let etag = Uuid::now_v7();
        let now = chrono::Utc::now();
        sqlx::query!(
            "INSERT INTO route_drafts \
             (id, routing_id, slug, state, overall_timeout_ms, max_attempts, etag, created_by, created_at, updated_at) \
             VALUES ($1, $2, $3, 'draft'::route_draft_state, $4, $5, $6, $7, $8, $8)",
        id, routing_id, slug.as_str(), overall_timeout_ms, i16::try_from(route.max_attempts).map_err(|_| {
            ConfigurationError::InvalidRoute("max attempts is too large".to_owned())
        })?, etag, route.actor, now)
        .execute(&mut *transaction)
        .await?;
        for operation in route.operations {
            sqlx::query!(
                "INSERT INTO route_draft_operations (route_draft_id, operation) VALUES ($1, $2)",
                id,
                operation.as_str()
            )
            .execute(&mut *transaction)
            .await?;
        }
        for (position, target) in route.targets.into_iter().enumerate() {
            if target.weight == 0
                || target.timeout_ms == 0
                || target.timeout_ms > route.overall_timeout_ms
            {
                return Err(ConfigurationError::InvalidRoute(
                    "target weight/timeout is invalid".to_owned(),
                ));
            }
            let provider_model_id: Option<Uuid> = sqlx::query_scalar!(
                "SELECT prm.source_provider_model_id \
                 FROM providers p \
                 JOIN provider_revision_models prm ON prm.provider_revision_id = p.active_revision_id \
                 WHERE p.id = $1 AND prm.upstream_model = $2 AND prm.enabled \
                   AND p.state <> 'disabled'::provider_state",
            target.provider_id, target.upstream_model.trim())
            .fetch_optional(&mut *transaction)
            .await?;
            let provider_model_id = provider_model_id.ok_or_else(|| {
                ConfigurationError::InvalidRoute(format!(
                    "target provider/model {}/{} is not active",
                    target.provider_id, target.upstream_model
                ))
            })?;
            sqlx::query!(
                "INSERT INTO route_draft_targets \
                 (id, routing_id, route_draft_id, provider_model_id, priority, weight, timeout_ms, position) \
                 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)",
            Uuid::now_v7(), Uuid::now_v7(), id, provider_model_id, i32::from(target.priority), i32::try_from(target.weight).map_err(|_| {
                ConfigurationError::InvalidRoute("target weight is too large".to_owned())
            })?, i32::try_from(target.timeout_ms).map_err(|_| {
                ConfigurationError::InvalidRoute("target timeout is too large".to_owned())
            })?, i32::try_from(position)
                    .map_err(|_| ConfigurationError::InvalidRoute("too many targets".to_owned()))?,)
            .execute(&mut *transaction)
            .await?;
        }
        sqlx::query!(
            "INSERT INTO audit_events \
             (id, actor_user_id, action, resource_type, resource_id, outcome, occurred_at) \
             VALUES ($1, $2, 'route.create_draft', 'route_draft', $3, 'success', $4)",
            Uuid::now_v7(),
            route.actor,
            id.to_string(),
            now
        )
        .execute(&mut *transaction)
        .await?;
        let created = RouteDraftCreated {
            id,
            slug,
            etag,
            created_at: now,
        };
        let response = build_response(&created)?;
        complete_replayable_idempotency(
            &mut transaction,
            route.actor,
            "route.create_draft",
            &route.idempotency_key,
            replay.request_fingerprint(),
            replay.master_key(),
            &response,
        )
        .await?;
        transaction.commit().await?;
        Ok(IdempotencyOutcome::Executed {
            value: created,
            response,
        })
    }

    pub async fn validate_route_draft(
        &self,
        draft_id: Uuid,
        expected_etag: Uuid,
        actor: Uuid,
    ) -> Result<(Uuid, RouteSlug), ConfigurationError> {
        let mut transaction = self.pool().begin().await?;
        let row = sqlx::query!(
            "SELECT etag, slug FROM route_drafts WHERE id = $1",
            draft_id
        )
        .fetch_optional(&mut *transaction)
        .await?;
        let Some(row) = row else {
            return Err(ConfigurationError::RouteNotFound);
        };
        if row.etag != expected_etag {
            return Err(ConfigurationError::PreconditionFailed);
        }
        revalidate_route_draft(&mut transaction, draft_id).await?;
        let etag = Uuid::now_v7();
        let updated = sqlx::query!(
            "UPDATE route_drafts SET state = 'validated'::route_draft_state, etag = $1, updated_at = now() \
             WHERE id = $2 AND etag = $3",
        etag, draft_id, expected_etag)
        .execute(&mut *transaction)
        .await?;
        if updated.rows_affected() != 1 {
            return Err(ConfigurationError::PreconditionFailed);
        }
        let slug = RouteSlug::parse(row.slug)
            .map_err(|error| ConfigurationError::InvalidRoute(error.to_string()))?;
        sqlx::query!(
            "INSERT INTO audit_events \
             (id, actor_user_id, action, resource_type, resource_id, outcome) \
             VALUES ($1, $2, 'route.validate_draft', 'route_draft', $3, 'success')",
            Uuid::now_v7(),
            actor,
            draft_id.to_string()
        )
        .execute(&mut *transaction)
        .await?;
        transaction.commit().await?;
        Ok((etag, slug))
    }

    pub async fn activate_route_draft(
        &self,
        draft_id: Uuid,
        expected_etag: Uuid,
        actor: Uuid,
        idempotency_key: &str,
    ) -> Result<RouteActivated, ConfigurationError> {
        let mut transaction = self
            .pool()
            .begin_with("BEGIN ISOLATION LEVEL READ COMMITTED")
            .await?;
        // READ COMMITTED is intentional here. Acquiring an advisory lock is a
        // statement in PostgreSQL, so a REPEATABLE READ transaction would pin
        // its snapshot before waiting for a concurrent provider activation.
        // With the publication lock held, READ COMMITTED observes the exact
        // active revisions that won the lock immediately before this draft.
        lock_runtime_publication(&mut transaction).await?;
        if !claim_idempotency(&mut transaction, actor, "route.activate", idempotency_key).await? {
            return Err(ConfigurationError::IdempotencyConflict);
        }
        sqlx::query!(
            "SELECT pg_advisory_xact_lock(hashtextextended($1::text, 0))",
            draft_id.to_string()
        )
        .fetch_one(&mut *transaction)
        .await?;
        let draft = sqlx::query!(
            "SELECT rd.slug, rd.routing_id, rd.state::text AS \"state!\", rd.etag, rd.overall_timeout_ms, \
                    rd.max_attempts, rr.route_id AS \"based_route_id?\", rr.slug AS \"based_slug?\" \
             FROM route_drafts rd \
             LEFT JOIN route_revisions rr ON rr.id = rd.based_on_revision_id \
             WHERE rd.id = $1 FOR UPDATE OF rd",
        draft_id)
        .fetch_optional(&mut *transaction)
        .await?
        .ok_or(ConfigurationError::RouteNotFound)?;
        if draft.etag != expected_etag {
            return Err(ConfigurationError::PreconditionFailed);
        }
        if draft.state != "validated" {
            return Err(ConfigurationError::RouteNotValidated);
        }
        // Media reservations are not runtime-publication mutations. Block
        // their short INSERT/UPDATE transactions while checking and
        // publishing so a job cannot appear against the old route after the
        // compatibility decision but before this activation commits.
        sqlx::query!("LOCK TABLE async_media_jobs IN SHARE MODE")
            .execute(&mut *transaction)
            .await?;
        revalidate_route_draft(&mut transaction, draft_id).await?;
        let slug: String = draft.slug;
        let based_route_id: Option<Uuid> = draft.based_route_id;
        let based_slug: Option<String> = draft.based_slug;
        let route_id = if let Some(route_id) = based_route_id {
            if based_slug.as_deref() != Some(slug.as_str()) {
                return Err(ConfigurationError::InvalidRoute(
                    "a restored route draft must retain its original stable slug".to_owned(),
                ));
            }
            route_id
        } else {
            sqlx::query_scalar!(
                "INSERT INTO routes (id, slug, created_by) VALUES ($1, $2, $3) \
                 ON CONFLICT (slug) DO UPDATE SET slug = EXCLUDED.slug RETURNING id AS \"value!\"",
                Uuid::now_v7(),
                &slug,
                actor
            )
            .fetch_one(&mut *transaction)
            .await?
        };
        let revision: i32 = sqlx::query_scalar!(
            "SELECT COALESCE(max(revision), 0) + 1 AS \"value!\" FROM route_revisions WHERE route_id = $1",
            route_id
        )
        .fetch_one(&mut *transaction)
        .await?;
        let revision_id = Uuid::now_v7();
        sqlx::query!(
            "INSERT INTO route_revisions \
             (id, route_id, routing_id, revision, slug, overall_timeout_ms, max_attempts, source_draft_id, activated_by) \
             VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)",
        revision_id, route_id, draft.routing_id, revision, &slug, draft.overall_timeout_ms, draft.max_attempts, draft_id, actor)
        .execute(&mut *transaction)
        .await?;
        sqlx::query!(
            "INSERT INTO route_revision_operations (route_revision_id, operation) \
             SELECT $1, operation FROM route_draft_operations WHERE route_draft_id = $2",
            revision_id,
            draft_id
        )
        .execute(&mut *transaction)
        .await?;
        let targets = sqlx::query!(
            "SELECT routing_id, provider_model_id, priority, weight, timeout_ms, position \
             FROM route_draft_targets WHERE route_draft_id = $1 ORDER BY position",
            draft_id
        )
        .fetch_all(&mut *transaction)
        .await?;
        for target in targets {
            sqlx::query!(
                "INSERT INTO route_revision_targets \
                 (id, routing_id, route_revision_id, provider_model_id, priority, weight, timeout_ms, position) \
                 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)",
            Uuid::now_v7(), target.routing_id, revision_id, target.provider_model_id, target.priority, target.weight, target.timeout_ms, target.position)
            .execute(&mut *transaction)
            .await?;
        }
        sqlx::query!(
            "INSERT INTO audit_events \
             (id, actor_user_id, action, resource_type, resource_id, outcome) \
             VALUES ($1, $2, 'route.activate', 'route', $3, 'success')",
            Uuid::now_v7(),
            actor,
            route_id.to_string()
        )
        .execute(&mut *transaction)
        .await?;
        complete_idempotency(
            &mut transaction,
            actor,
            "route.activate",
            idempotency_key,
            &route_id.to_string(),
        )
        .await?;
        let release = compile_and_publish_runtime_in_transaction(&mut transaction, actor).await?;
        transaction.commit().await?;
        Ok(RouteActivated {
            route_id,
            revision_id,
            revision,
            release,
        })
    }
}

async fn revalidate_route_draft(
    transaction: &mut Transaction<'_, Postgres>,
    draft_id: Uuid,
) -> Result<(), ConfigurationError> {
    let target_count: i64 = sqlx::query_scalar!(
        "SELECT count(*)::bigint AS \"value!\" FROM route_draft_targets WHERE route_draft_id = $1",
        draft_id
    )
    .fetch_one(&mut **transaction)
    .await?;
    if target_count == 0 {
        return Err(ConfigurationError::InvalidRoute(
            "the draft must contain at least one target".to_owned(),
        ));
    }

    let unavailable: Option<String> = sqlx::query_scalar!(
        "SELECT concat(p.name, '/', pm.upstream_model) AS \"value!\" \
         FROM route_draft_targets rdt \
         JOIN provider_models pm ON pm.id = rdt.provider_model_id \
         JOIN providers p ON p.id = pm.provider_id \
         LEFT JOIN provider_revision_models prm \
           ON prm.provider_revision_id = p.active_revision_id \
          AND prm.source_provider_model_id = pm.id \
         WHERE rdt.route_draft_id = $1 \
           AND (p.state = 'disabled'::provider_state OR prm.id IS NULL OR NOT prm.enabled) \
         ORDER BY rdt.position LIMIT 1",
        draft_id
    )
    .fetch_optional(&mut **transaction)
    .await?;
    if let Some(unavailable) = unavailable {
        return Err(ConfigurationError::InvalidRoute(format!(
            "target provider/model is not in an activated provider revision: {unavailable}"
        )));
    }

    let uncovered_operation: Option<String> = sqlx::query_scalar!(
        "SELECT rdo.operation FROM route_draft_operations rdo \
         WHERE rdo.route_draft_id = $1 AND NOT EXISTS ( \
           SELECT 1 FROM route_draft_targets rdt \
           JOIN provider_models pm ON pm.id = rdt.provider_model_id \
           JOIN providers p ON p.id = pm.provider_id \
           JOIN provider_revision_models prm \
             ON prm.provider_revision_id = p.active_revision_id \
            AND prm.source_provider_model_id = pm.id AND prm.enabled \
           JOIN provider_revision_capabilities prc \
             ON prc.provider_revision_model_id = prm.id \
            AND prc.operation = rdo.operation AND prc.source = 'certified' \
           WHERE rdt.route_draft_id = $1 \
             AND p.state <> 'disabled'::provider_state) \
         ORDER BY rdo.operation LIMIT 1",
        draft_id
    )
    .fetch_optional(&mut **transaction)
    .await?;
    if let Some(operation) = uncovered_operation {
        return Err(ConfigurationError::InvalidRoute(format!(
            "no activated target has a certified capability for route operation {operation}"
        )));
    }

    let media_job_without_target: Option<Uuid> = sqlx::query_scalar!(
        "SELECT j.id FROM async_media_jobs j \
         JOIN route_drafts rd ON rd.id = $1 AND rd.slug = j.route_slug \
         WHERE j.lifecycle_state <> 'deleted' AND NOT EXISTS ( \
           SELECT 1 FROM route_draft_targets rdt \
           JOIN provider_models pm ON pm.id = rdt.provider_model_id \
           WHERE rdt.route_draft_id = rd.id \
             AND pm.provider_id = j.provider_id \
             AND pm.upstream_model = j.provider_model) \
         ORDER BY j.created_at, j.id LIMIT 1",
        draft_id
    )
    .fetch_optional(&mut **transaction)
    .await?;
    if let Some(job_id) = media_job_without_target {
        return Err(ConfigurationError::InvalidRoute(format!(
            "active media job {job_id} requires its exact provider/model target"
        )));
    }

    let media_job_without_lifecycle: Option<String> = sqlx::query_scalar!(
        "SELECT concat(j.id::text, '/', required.operation) AS \"value!\" \
         FROM async_media_jobs j \
         JOIN route_drafts rd ON rd.id = $1 AND rd.slug = j.route_slug \
         CROSS JOIN (VALUES ('video_get'), ('video_content'), ('video_delete')) \
                    AS required(operation) \
         WHERE j.lifecycle_state <> 'deleted' AND ( \
           NOT EXISTS ( \
             SELECT 1 FROM route_draft_operations rdo \
             WHERE rdo.route_draft_id = rd.id AND rdo.operation = required.operation) \
           OR NOT EXISTS ( \
             SELECT 1 FROM route_draft_targets rdt \
             JOIN provider_models pm ON pm.id = rdt.provider_model_id \
             JOIN providers p ON p.id = pm.provider_id \
             JOIN provider_revision_models prm \
               ON prm.provider_revision_id = p.active_revision_id \
              AND prm.source_provider_model_id = pm.id AND prm.enabled \
             JOIN provider_revision_capabilities prc \
               ON prc.provider_revision_model_id = prm.id \
              AND prc.operation = required.operation \
              AND prc.surface = j.surface \
              AND prc.mode = 'unary' AND prc.source = 'certified' \
             WHERE rdt.route_draft_id = rd.id \
               AND p.state <> 'disabled'::provider_state \
               AND pm.provider_id = j.provider_id \
               AND pm.upstream_model = j.provider_model)) \
         ORDER BY j.created_at, j.id, required.operation LIMIT 1",
        draft_id
    )
    .fetch_optional(&mut **transaction)
    .await?;
    if let Some(requirement) = media_job_without_lifecycle {
        return Err(ConfigurationError::InvalidRoute(format!(
            "active media job requires an exact certified lifecycle capability: {requirement}"
        )));
    }

    Ok(())
}
