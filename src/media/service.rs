use crate::crypto::envelope::MasterKey;
use crate::ids::ProviderId;
use crate::ids::RouteSlug;
use crate::inference::execution::RequiredTarget;
use crate::inference::executor::Executor;
use crate::media::jobs::MediaJobError;
use crate::media::jobs::MediaJobLifecycle;
use crate::media::jobs::MediaJobRecord;
use crate::media::jobs::MediaJobState;
use crate::media::jobs::MediaJobUpdate;
use crate::media::jobs::MediaReconciliationPass;
use crate::media::service::results::media_job_state;
use crate::media::service::results::media_job_update;
use crate::media::service::results::valid_upstream_media_job_id;
use crate::net::egress::EgressPolicy;
use crate::protocols::canonical::identity::Surface;
use crate::protocols::canonical::requests::MEDIA_DELETE_MISSING_IS_SUCCESS_EXTENSION;
use crate::protocols::canonical::requests::Operation;
use crate::protocols::canonical::requests::SourceExtensions;
use crate::protocols::canonical::requests::VideoJobRequest;
use crate::protocols::canonical::requests::VideoOperation;
use crate::protocols::canonical::results::CanonicalResult;
use crate::providers::connector::ResponseLimits;
use crate::providers::runtime_config::runtime_provider_config;
use crate::providers::runtime_config::runtime_provider_credential;
use crate::runtime::manager::Bundle;
use crate::runtime::manager::Manager;
use crate::runtime::transports::TransportRegistry;
use chrono::Utc;
use futures::StreamExt;
use futures::stream;
use sqlx::PgPool;
use std::collections::BTreeMap;
use std::sync::Arc;
use std::sync::atomic::AtomicU64;
use std::sync::atomic::Ordering;
use std::time::Duration;
use tracing::error;
use tracing::warn;

#[derive(Clone)]
pub struct MediaJobs {
    pub(crate) pool: PgPool,
    pub(crate) inference: Arc<Executor>,
    pub(crate) transports: TransportRegistry,
    pub(crate) master_key: Option<Arc<MasterKey>>,
    pub(crate) provider_egress_policy: Arc<EgressPolicy>,
    pub(crate) provider_response_limits: ResponseLimits,
    pub(crate) media_reconciliation_gaps: Arc<AtomicU64>,
}

impl MediaJobs {
    pub(crate) fn record_media_reconciliation_gap(&self) {
        let _ = self.media_reconciliation_gaps.fetch_update(
            Ordering::Relaxed,
            Ordering::Relaxed,
            |value| Some(value.saturating_add(1)),
        );
    }
}
pub(crate) async fn attach_media_job_with_retry(
    pool: &sqlx::PgPool,
    id: uuid::Uuid,
    upstream_job_id: &str,
    update: MediaJobUpdate,
) -> Result<MediaJobRecord, MediaJobError> {
    for attempt in 0..3 {
        match crate::media::jobs::lifecycle::attach_media_job_upstream(
            pool,
            id,
            upstream_job_id,
            update.clone(),
        )
        .await
        {
            Ok(record) => return Ok(record),
            Err(MediaJobError::Database(_)) if attempt < 2 => {
                tokio::time::sleep(Duration::from_millis(25 * (attempt + 1))).await;
            }
            Err(error) => return Err(error),
        }
    }
    unreachable!("bounded attach retry returns on every final attempt")
}

pub(crate) async fn media_job_deletion_finalized(
    pool: &sqlx::PgPool,
    id: uuid::Uuid,
) -> Result<bool, MediaJobError> {
    if crate::media::jobs::lifecycle::finalize_media_job_deletion(pool, id).await? {
        return Ok(true);
    }
    Ok(crate::media::jobs::queries::media_job(pool, id)
        .await?
        .lifecycle
        == MediaJobLifecycle::Deleted)
}

pub async fn reconcile_media_jobs_once(
    state: &MediaJobs,
    limit: u16,
) -> Result<MediaReconciliationPass, MediaJobError> {
    let records = crate::media::jobs::reconciliation::claim_media_reconciliation_jobs(
        &state.pool,
        Utc::now(),
        limit,
    )
    .await?;
    let claimed = u16::try_from(records.len()).unwrap_or(u16::MAX);
    let outcomes = stream::iter(records)
        .map(|record| reconcile_claimed_media_job(state, record))
        .buffer_unordered(4)
        .collect::<Vec<_>>()
        .await;
    let completed =
        u16::try_from(outcomes.iter().filter(|value| **value).count()).unwrap_or(u16::MAX);
    Ok(MediaReconciliationPass {
        claimed,
        completed,
        failed: claimed.saturating_sub(completed),
    })
}

async fn reconcile_claimed_media_job(state: &MediaJobs, mut record: MediaJobRecord) -> bool {
    let Some(claim_id) = record.reconciliation_claim_id else {
        state.record_media_reconciliation_gap();
        return false;
    };
    let pool = &state.pool;
    let outcome = reconcile_media_job_operation(state, &mut record).await;
    let now = Utc::now();
    let (next_attempt_at, error_class) = match outcome {
        Ok(()) => {
            let next = if matches!(record.state, MediaJobState::Queued | MediaJobState::Running)
                && record.lifecycle == MediaJobLifecycle::Active
            {
                now + chrono::Duration::seconds(5)
            } else {
                now + chrono::Duration::hours(24)
            };
            (next, None)
        }
        Err(code) => {
            let exponent = record.reconciliation_attempts.min(6);
            let seconds = 5_i64.saturating_mul(1_i64 << exponent).min(300);
            (now + chrono::Duration::seconds(seconds), Some(code))
        }
    };
    if let Err(error) = crate::media::jobs::reconciliation::finish_media_reconciliation(
        pool,
        record.id,
        claim_id,
        next_attempt_at,
        error_class,
    )
    .await
    {
        state.record_media_reconciliation_gap();
        error!(job_id = %record.id, %error, "failed to checkpoint autonomous media reconciliation");
        return false;
    }
    if let Some(code) = error_class {
        warn!(job_id = %record.id, error_class = code, "autonomous media reconciliation will retry");
        false
    } else {
        true
    }
}

async fn reconcile_media_job_operation(
    state: &MediaJobs,
    record: &mut MediaJobRecord,
) -> Result<(), &'static str> {
    let pool = &state.pool;
    match record.lifecycle {
        MediaJobLifecycle::Creating => {
            if let Some(upstream_id) = record.upstream_job_id.as_deref() {
                *record = crate::media::jobs::lifecycle::mark_media_job_create_cleanup_pending(
                    pool,
                    record.id,
                    upstream_id,
                    "stale_post_create_reservation",
                )
                .await
                .map_err(|_| "persistence_unavailable")?;
            } else {
                *record = crate::media::jobs::lifecycle::mark_media_job_create_ambiguous(
                    pool,
                    record.id,
                    "upstream_create_outcome_unknown_after_restart",
                )
                .await
                .map_err(|_| "persistence_unavailable")?;
                return Err("upstream_create_outcome_unknown");
            }
        }
        MediaJobLifecycle::CreateAmbiguous => {
            let Some(upstream_id) = record.upstream_job_id.as_deref() else {
                return Err("upstream_create_outcome_unknown");
            };
            *record = crate::media::jobs::lifecycle::mark_media_job_create_cleanup_pending(
                pool,
                record.id,
                upstream_id,
                "ambiguous_create_has_cleanup_identity",
            )
            .await
            .map_err(|_| "persistence_unavailable")?;
        }
        MediaJobLifecycle::Deleted => return Ok(()),
        MediaJobLifecycle::Active
        | MediaJobLifecycle::CreateCleanupPending
        | MediaJobLifecycle::DeletePending => {}
    }

    if record.lifecycle == MediaJobLifecycle::Active
        && (record
            .expires_at
            .is_some_and(|expires_at| expires_at <= Utc::now())
            || record.created_at <= Utc::now() - chrono::Duration::days(30))
    {
        *record = crate::media::jobs::lifecycle::begin_media_job_deletion(pool, record.id)
            .await
            .map_err(|_| "persistence_unavailable")?;
    }

    let result = execute_media_reconciliation_result(state, record).await?;
    if record.lifecycle == MediaJobLifecycle::Active {
        let CanonicalResult::VideoJob(result) = result.as_ref() else {
            return Err("provider_protocol_error");
        };
        let state_update = media_job_state(&result.status).map_err(|error| error.code())?;
        *record = crate::media::jobs::lifecycle::refresh_media_job(
            pool,
            record.id,
            media_job_update(result, state_update),
        )
        .await
        .map_err(|_| "persistence_unavailable")?;
        return Ok(());
    }

    if !matches!(
        result.as_ref(),
        CanonicalResult::VideoDelete(deleted) if deleted.deleted
    ) {
        return Err("video_delete_not_confirmed");
    }
    let finalized = media_job_deletion_finalized(pool, record.id)
        .await
        .map_err(|_| "persistence_unavailable")?;
    if !finalized {
        state.record_media_reconciliation_gap();
        return Err("persistence_unavailable");
    }
    record.lifecycle = MediaJobLifecycle::Deleted;
    Ok(())
}

async fn execute_media_reconciliation_result(
    state: &MediaJobs,
    record: &MediaJobRecord,
) -> Result<Box<CanonicalResult>, &'static str> {
    let upstream_id = record
        .upstream_job_id
        .clone()
        .filter(|value| valid_upstream_media_job_id(value))
        .ok_or("media_job_upstream_id_unavailable")?;
    let route = RouteSlug::parse(&record.route_slug).map_err(|_| "media_job_route_invalid")?;
    let mut request = VideoJobRequest {
        route: Some(route),
        job_id: upstream_id,
        extensions: SourceExtensions::new(Surface::OpenAi, BTreeMap::new()),
    };
    let operation = if record.lifecycle == MediaJobLifecycle::Active {
        VideoOperation::Get(request)
    } else {
        request.extensions.values.insert(
            MEDIA_DELETE_MISSING_IS_SUCCESS_EXTENSION.to_owned(),
            serde_json::Value::Bool(true),
        );
        VideoOperation::Delete(request)
    };
    let runtime = media_job_runtime(state, record).await?;
    state
        .inference
        .execute_reconciliation_result(
            runtime,
            record.api_key_id,
            Operation::Video(operation),
            Surface::OpenAi,
            RequiredTarget {
                provider_id: record.provider_id,
                upstream_model: record.upstream_model.clone(),
            },
        )
        .await
        .map_err(|failure| failure.code())
}

async fn media_job_runtime(
    state: &MediaJobs,
    record: &MediaJobRecord,
) -> Result<Arc<Bundle>, &'static str> {
    let generation_id = record.runtime_generation_id;
    let provider_revision_id = record.provider_revision_id;
    let release =
        crate::runtime::publication::releases::valid_runtime_release(&state.pool, generation_id)
            .await
            .map_err(|_| "media_job_runtime_unavailable")?;
    let snapshot = Manager::decode_persisted_release(&release.activation_candidate())
        .map_err(|_| "media_job_runtime_unavailable")?;
    let provider_id = ProviderId::from_uuid(record.provider_id);
    let provider = crate::providers::runtime::media_job_runtime_provider_configuration(
        &state.pool,
        &snapshot,
        provider_id,
        provider_revision_id,
    )
    .await
    .map_err(|_| "media_job_runtime_unavailable")?;
    let transport = if let Some(master_key) = state.master_key.as_deref() {
        let config = runtime_provider_config(&provider, &snapshot)
            .map_err(|_| "media_job_runtime_unavailable")?;
        let credential = runtime_provider_credential(&provider, &config, master_key)
            .map_err(|_| "media_job_runtime_unavailable")?;
        crate::providers::connectors::transport(
            config,
            credential,
            &state.provider_egress_policy,
            state.provider_response_limits,
        )
        .await
        .map_err(|_| "media_job_runtime_unavailable")?
    } else {
        let current = crate::providers::runtime::runtime_provider_authority_is_current(
            &state.pool,
            generation_id,
            record.provider_id,
            provider_revision_id,
        )
        .await
        .map_err(|_| "media_job_runtime_unavailable")?;
        if !current {
            return Err("media_job_runtime_unavailable");
        }
        state
            .transports
            .snapshot()
            .remove(&provider_id)
            .ok_or("media_job_runtime_unavailable")?
    };
    Manager::reconciliation_bundle(snapshot, provider_id, transport)
        .map_err(|_| "media_job_runtime_unavailable")
}

pub mod creation;

pub mod results;
