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
const ATTACH_ATTEMPTS: u64 = 3;
/// Reconciliation jobs run concurrently per pass. Claims are taken in chunks
/// of this size so every claimed lease starts running immediately.
pub(crate) const RECONCILIATION_CONCURRENCY: usize = 4;
/// Extra lease time beyond the route deadline for persistence work around
/// the upstream call (mirrors the admission lease sizing).
const RECONCILIATION_LEASE_SLACK: chrono::Duration = chrono::Duration::seconds(60);
const RECONCILIATION_CLAIM_LOST: &str = "reconciliation_claim_lost";

pub(crate) async fn attach_media_job_with_retry(
    pool: &sqlx::PgPool,
    id: uuid::Uuid,
    upstream_job_id: &str,
    update: MediaJobUpdate,
) -> Result<MediaJobRecord, MediaJobError> {
    let mut attempt = 0_u64;
    loop {
        match crate::media::jobs::lifecycle::attach_media_job_upstream(
            pool,
            id,
            upstream_job_id,
            update.clone(),
        )
        .await
        {
            Ok(record) => return Ok(record),
            Err(MediaJobError::Database(_)) if attempt + 1 < ATTACH_ATTEMPTS => {
                attempt += 1;
                tokio::time::sleep(Duration::from_millis(25 * attempt)).await;
            }
            Err(error) => return Err(error),
        }
    }
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
    let chunk = u16::try_from(RECONCILIATION_CONCURRENCY).unwrap_or(u16::MAX);
    let mut claimed: u16 = 0;
    let mut completed: u16 = 0;
    let mut handed_off: u16 = 0;
    // Claim only what can run now: a lease starts ticking at claim time, so a
    // job queued behind running work would otherwise burn its lease waiting.
    while claimed < limit {
        let wanted = chunk.min(limit - claimed);
        let records = crate::media::jobs::reconciliation::claim_media_reconciliation_jobs(
            &state.pool,
            Utc::now(),
            wanted,
        )
        .await?;
        let count = u16::try_from(records.len()).unwrap_or(u16::MAX);
        claimed = claimed.saturating_add(count);
        let outcomes = stream::iter(records)
            .map(|record| reconcile_claimed_media_job(state, record))
            .buffer_unordered(RECONCILIATION_CONCURRENCY)
            .collect::<Vec<_>>()
            .await;
        for outcome in outcomes {
            match outcome {
                ReconciliationOutcome::Completed => completed = completed.saturating_add(1),
                ReconciliationOutcome::HandedOff => handed_off = handed_off.saturating_add(1),
                ReconciliationOutcome::Failed => {}
            }
        }
        if count < wanted {
            break;
        }
    }
    Ok(MediaReconciliationPass {
        claimed,
        completed,
        failed: claimed.saturating_sub(completed).saturating_sub(handed_off),
    })
}

enum ReconciliationOutcome {
    Completed,
    Failed,
    /// Another worker reclaimed the job before its upstream call; nothing was
    /// executed or checkpointed here.
    HandedOff,
}

async fn reconcile_claimed_media_job(
    state: &MediaJobs,
    mut record: MediaJobRecord,
) -> ReconciliationOutcome {
    let Some(claim_id) = record.reconciliation_claim_id else {
        state.record_media_reconciliation_gap();
        return ReconciliationOutcome::Failed;
    };
    let pool = &state.pool;
    let outcome = reconcile_media_job_operation(state, &mut record, claim_id).await;
    if outcome == Err(RECONCILIATION_CLAIM_LOST) {
        tracing::info!(
            job_id = %record.id,
            "autonomous media reconciliation claim was reassigned before the upstream call"
        );
        return ReconciliationOutcome::HandedOff;
    }
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
        return ReconciliationOutcome::Failed;
    }
    if let Some(code) = error_class {
        warn!(job_id = %record.id, error_class = code, "autonomous media reconciliation will retry");
        ReconciliationOutcome::Failed
    } else {
        ReconciliationOutcome::Completed
    }
}

async fn reconcile_media_job_operation(
    state: &MediaJobs,
    record: &mut MediaJobRecord,
    claim_id: uuid::Uuid,
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

    let result = execute_media_reconciliation_result(state, record, claim_id).await?;
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
    claim_id: uuid::Uuid,
) -> Result<Box<CanonicalResult>, &'static str> {
    let upstream_id = record
        .upstream_job_id
        .clone()
        .filter(|value| valid_upstream_media_job_id(value))
        .ok_or("media_job_upstream_id_unavailable")?;
    let route = RouteSlug::parse(&record.route_slug).map_err(|_| "media_job_route_invalid")?;
    let record_route = route.clone();
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
    // Revalidate ownership and bound the lease across the whole upstream call.
    // Another replica may have reclaimed this row; if so, stop before issuing
    // a duplicate poll or delete.
    let deadline = runtime
        .routes
        .get(&record_route)
        .map(|route| route.overall_timeout.as_duration())
        .ok_or("media_job_route_invalid")?;
    let lease_until = Utc::now()
        + chrono::Duration::from_std(deadline).unwrap_or(RECONCILIATION_LEASE_SLACK)
        + RECONCILIATION_LEASE_SLACK;
    let owned = crate::media::jobs::reconciliation::extend_media_reconciliation_claim(
        &state.pool,
        record.id,
        claim_id,
        lease_until,
    )
    .await
    .map_err(|_| "persistence_unavailable")?;
    if !owned {
        return Err(RECONCILIATION_CLAIM_LOST);
    }
    state
        .inference
        .execute_reconciliation_result(
            runtime,
            record.api_key_id,
            Operation::Video(operation),
            Surface::OpenAi,
            RequiredTarget {
                credential_version_id: record.credential_version_id,
                provider_id: record.provider_id,
                upstream_model: record.upstream_model.clone(),
            },
        )
        .await
        .map_err(|failure| failure.code())
}

pub(crate) async fn media_job_runtime(
    state: &MediaJobs,
    record: &MediaJobRecord,
) -> Result<Arc<Bundle>, &'static str> {
    let generation_id = record.runtime_generation_id;
    let provider_revision_id = record.provider_revision_id;
    let release =
        crate::runtime::publication::releases::valid_runtime_release(&state.pool, generation_id)
            .await
            .map_err(|_| "media_job_runtime_unavailable")?;
    let mut snapshot = Manager::decode_persisted_release(&release.activation_candidate())
        .map_err(|_| "media_job_runtime_unavailable")?;
    let provider_id = ProviderId::from_uuid(record.provider_id);
    refresh_media_quotas(&state.pool, &mut snapshot, provider_id).await?;
    let provider = crate::providers::runtime::media_job_runtime_provider_configuration(
        &state.pool,
        &snapshot,
        provider_id,
        provider_revision_id,
        record.credential_version_id,
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
        if provider.credential_id
            != snapshot.providers[&provider_id]
                .active_credential
                .map(|credential| credential.as_uuid())
        {
            return Err("media_job_runtime_unavailable");
        }
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
    let mut pool = crate::providers::pool_transport::PoolTransport::for_provider(
        &snapshot,
        provider_id,
        provider.configuration.options.clone(),
        provider.credential_id,
        &state.inference.limiter,
    );
    pool.transports.insert(provider.credential_id, transport);
    let transport = Arc::new(pool);
    Manager::reconciliation_bundle(snapshot, provider_id, transport)
        .map_err(|_| "media_job_runtime_unavailable")
}

pub mod creation;

/// HTTP media operations and background reconciliation share this runtime
/// builder. Read both quota scopes from one current revision while retaining
/// the job's historical transport configuration and secret version.
async fn refresh_media_quotas(
    pool: &PgPool,
    snapshot: &mut crate::runtime::snapshot::Snapshot,
    provider: ProviderId,
) -> Result<(), &'static str> {
    let (limits, slots) = sqlx::query_as::<_, (
        sqlx::types::Json<Option<crate::providers::options::ConnectionLimits>>,
        sqlx::types::Json<Vec<crate::providers::pool::CredentialSlot>>,
    )>(
        "SELECT COALESCE(pr.options->'limits','null'::jsonb), \
         COALESCE((SELECT jsonb_agg(pc.configuration ORDER BY pc.slot_id) \
                   FROM provider_revision_credentials pc WHERE pc.provider_revision_id=pr.id),'[]'::jsonb) \
         FROM providers p JOIN provider_revisions pr ON pr.id=p.active_revision_id WHERE p.id=$1",
    ).bind(provider.as_uuid()).fetch_optional(pool).await
        .map_err(|_| "media_job_quota_authority_unavailable")?
        .ok_or("media_job_quota_authority_unavailable")?;
    snapshot.routing.connection_limit_authority =
        Some(BTreeMap::from([(provider, limits.0.unwrap_or_default())]));
    snapshot.routing.credential_authority = Some(BTreeMap::from([(provider, slots.0)]));
    Ok(())
}

pub mod results;

#[cfg(test)]
mod quota_tests {
    use super::*;
    #[tokio::test]
    #[ignore = "requires PostgreSQL via make integration"]
    async fn historical_media_runtime_uses_current_quotas_without_rebinding_the_secret() {
        let db = crate::test_support::TestDb::create_migrated("media_quotas").await;
        let pool = db.pool(2).await;
        let actor = uuid::Uuid::now_v7();
        let provider = ProviderId::new();
        sqlx::query("INSERT INTO users(id,email,display_name,role) VALUES($1,'media-quotas@test.example','Owner','owner')").bind(actor).execute(&pool).await.unwrap();
        sqlx::query("INSERT INTO providers(id,name,kind,auth_mode,state,etag,created_by) VALUES($1,'media','openai','api_key','active',$2,$3)").bind(provider.as_uuid()).bind(uuid::Uuid::now_v7()).bind(actor).execute(&pool).await.unwrap();
        sqlx::query("INSERT INTO provider_credential_slots(id,provider_id,name,is_default) VALUES($1,$1,'Default',true)").bind(provider.as_uuid()).execute(&pool).await.unwrap();
        let master = crate::crypto::envelope::MasterKey::new(1, [62; 32]);
        let mut historical_slot = None;
        for version in 1..=2 {
            let credential = uuid::Uuid::now_v7();
            let encrypted = master
                .seal(
                    b"media-secret",
                    &crate::crypto::aad::credential(provider.as_uuid(), credential, version),
                )
                .unwrap();
            sqlx::query("INSERT INTO provider_credential_versions(id,provider_id,slot_id,version,ciphertext,nonce,master_key_version,created_by) VALUES($1,$2,$2,$3,$4,$5,1,$6)").bind(credential).bind(provider.as_uuid()).bind(version as i32).bind(encrypted.ciphertext).bind(encrypted.nonce.to_vec()).bind(actor).execute(&pool).await.unwrap();
            let revision = uuid::Uuid::now_v7();
            let rpm = if version == 1 { 10 } else { 1 };
            sqlx::query("INSERT INTO provider_revisions(id,provider_id,revision,name,kind,auth_mode,connector_ready,credential_version_id,source_etag,activated_by,options) VALUES($1,$2,$3,'media','openai','api_key',true,$4,$5,$6,$7)").bind(revision).bind(provider.as_uuid()).bind(version as i32).bind(credential).bind(uuid::Uuid::now_v7()).bind(actor).bind(serde_json::json!({"limits":{"requests_per_minute":rpm}})).execute(&pool).await.unwrap();
            let slot = crate::providers::pool::CredentialSlot {
                id: provider.as_uuid(),
                name: "Default".into(),
                credential_version_id: Some(credential),
                requests_per_minute: Some(rpm),
                ..Default::default()
            };
            sqlx::query("INSERT INTO provider_revision_credentials(provider_revision_id,slot_id,credential_version_id,configuration) VALUES($1,$2,$3,$4)").bind(revision).bind(provider.as_uuid()).bind(credential).bind(sqlx::types::Json(&slot)).execute(&pool).await.unwrap();
            sqlx::query("UPDATE providers SET active_revision_id=$2,active_credential_version_id=$3 WHERE id=$1").bind(provider.as_uuid()).bind(revision).bind(credential).execute(&pool).await.unwrap();
            if version == 1 {
                historical_slot = Some(slot);
            }
        }
        let historical_slot = historical_slot.unwrap();
        let mut snapshot = crate::runtime::snapshot::Snapshot::clone(&Manager::empty().pin());
        snapshot
            .routing
            .credentials
            .insert(provider, vec![historical_slot.clone()]);
        refresh_media_quotas(&pool, &mut snapshot, provider)
            .await
            .unwrap();
        assert_eq!(snapshot.routing.credentials[&provider][0], historical_slot);
        assert_eq!(
            snapshot.routing.credential_authority.as_ref().unwrap()[&provider][0]
                .requests_per_minute,
            Some(1)
        );
        assert_ne!(
            snapshot.routing.credential_authority.as_ref().unwrap()[&provider][0]
                .credential_version_id,
            historical_slot.credential_version_id
        );
        assert_eq!(
            snapshot
                .routing
                .connection_limit_authority
                .as_ref()
                .unwrap()[&provider]
                .requests_per_minute,
            Some(1)
        );
    }
}
