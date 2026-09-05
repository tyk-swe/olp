use self::results::{
    mark_missing_delete_as_success, media_job_state, media_job_update, set_video_route,
    valid_upstream_media_job_id,
};
use super::{
    provider_runtime::{runtime_provider_config, runtime_provider_credential},
    transports::TransportRegistry,
};
use chrono::Utc;
use futures::{StreamExt, stream};
use olp_db::{
    media_jobs::{
        MediaJobError, MediaJobLifecycle, MediaJobRecord, MediaJobState, MediaJobUpdate,
        MediaReconciliationPass,
    },
    security::envelope::MasterKey,
    store::Store,
};
use olp_engine::{
    domain::{
        canonical::{identity::Surface, requests::Operation, results::CanonicalResult},
        ids::ProviderId,
    },
    inference::{
        execution::RequiredTarget,
        runtime::{Bundle, Manager},
        service::Service,
    },
    providers::{connector::ResponseLimits, factory::assembly::Factory, http_egress::EgressPolicy},
};
use std::{
    sync::{
        Arc,
        atomic::{AtomicU64, Ordering},
    },
    time::Duration,
};
use tracing::{error, warn};
pub(crate) mod creation;
pub(crate) mod results;

#[derive(Clone)]
pub struct MediaJobs {
    pub(crate) store: Store,
    pub(crate) inference: Arc<Service>,
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
    store: &olp_db::store::Store,
    id: uuid::Uuid,
    upstream_job_id: &str,
    update: MediaJobUpdate,
) -> Result<MediaJobRecord, MediaJobError> {
    for attempt in 0..3 {
        match store
            .attach_media_job_upstream(id, upstream_job_id, update.clone())
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
    store: &olp_db::store::Store,
    id: uuid::Uuid,
) -> Result<bool, MediaJobError> {
    if store.finalize_media_job_deletion(id).await? {
        return Ok(true);
    }
    Ok(store.media_job(id).await?.lifecycle == MediaJobLifecycle::Deleted)
}

pub async fn reconcile_media_jobs_once(
    state: &MediaJobs,
    limit: u16,
) -> Result<MediaReconciliationPass, MediaJobError> {
    let records = state
        .store
        .claim_media_reconciliation_jobs(Utc::now(), limit)
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
    let store = &state.store;
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
    if let Err(error) = store
        .finish_media_reconciliation(record.id, claim_id, next_attempt_at, error_class)
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
    let store = &state.store;
    match record.lifecycle {
        MediaJobLifecycle::Creating => {
            if let Some(upstream_id) = record.upstream_job_id.as_deref() {
                *record = store
                    .mark_media_job_create_cleanup_pending(
                        record.id,
                        upstream_id,
                        "stale_post_create_reservation",
                    )
                    .await
                    .map_err(|_| "persistence_unavailable")?;
            } else {
                *record = store
                    .mark_media_job_create_ambiguous(
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
            *record = store
                .mark_media_job_create_cleanup_pending(
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
        *record = store
            .begin_media_job_deletion(record.id)
            .await
            .map_err(|_| "persistence_unavailable")?;
    }

    let upstream_id = record
        .upstream_job_id
        .clone()
        .filter(|value| valid_upstream_media_job_id(value))
        .ok_or("media_job_upstream_id_unavailable")?;
    if record.lifecycle == MediaJobLifecycle::Active {
        let mut operation = olp_engine::protocols::openai::video::decode_video_get(upstream_id);
        set_video_route(&mut operation, &record.route_slug).map_err(|error| error.code())?;
        let result = execute_media_reconciliation_result(state, record, operation).await?;
        let CanonicalResult::VideoJob(result) = result.as_ref() else {
            return Err("provider_protocol_error");
        };
        let state_update = media_job_state(&result.status).map_err(|error| error.code())?;
        *record = store
            .refresh_media_job(record.id, media_job_update(result, state_update))
            .await
            .map_err(|_| "persistence_unavailable")?;
        return Ok(());
    }

    let mut operation = olp_engine::protocols::openai::video::decode_video_delete(upstream_id);
    set_video_route(&mut operation, &record.route_slug).map_err(|error| error.code())?;
    mark_missing_delete_as_success(&mut operation).map_err(|error| error.code())?;
    let result = execute_media_reconciliation_result(state, record, operation).await?;
    if !matches!(
        result.as_ref(),
        CanonicalResult::VideoDelete(deleted) if deleted.deleted
    ) {
        return Err("video_delete_not_confirmed");
    }
    let finalized = media_job_deletion_finalized(store, record.id)
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
    operation: Operation,
) -> Result<Box<CanonicalResult>, &'static str> {
    let runtime = media_job_runtime(state, record).await?;
    state
        .inference
        .execute_reconciliation_result(
            runtime,
            record.api_key_id,
            operation,
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
    let (generation_id, provider_revision_id) =
        match (record.runtime_generation_id, record.provider_revision_id) {
            (Some(generation_id), Some(provider_revision_id)) => {
                (generation_id, provider_revision_id)
            }
            _ => return Err("media_job_runtime_unavailable"),
        };
    let release = state
        .store
        .valid_runtime_release(generation_id)
        .await
        .map_err(|_| "media_job_runtime_unavailable")?;
    let snapshot = Manager::decode_persisted_release(&release.activation_candidate())
        .map_err(|_| "media_job_runtime_unavailable")?;
    let provider_id = ProviderId::from_uuid(record.provider_id);
    let provider = state
        .store
        .media_job_runtime_provider_configuration(&snapshot, provider_id, provider_revision_id)
        .await
        .map_err(|_| "media_job_runtime_unavailable")?;
    let transport = if let Some(master_key) = state.master_key.as_deref() {
        let config = runtime_provider_config(&provider, &snapshot)
            .map_err(|_| "media_job_runtime_unavailable")?;
        let credential = runtime_provider_credential(&provider, &config, master_key)
            .map_err(|_| "media_job_runtime_unavailable")?;
        Factory::transport(
            config,
            credential,
            &state.provider_egress_policy,
            state.provider_response_limits,
        )
        .await
        .map_err(|_| "media_job_runtime_unavailable")?
    } else {
        let current = state
            .store
            .runtime_provider_authority_is_current(
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
