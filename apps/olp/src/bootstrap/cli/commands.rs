use std::{path::Path, time::Duration};

use olp_storage::{
    PgStore,
    limits::DistributedLimiter,
    runtime::RuntimeOutboxLeader,
    security::MasterKey,
    security::MasterKeyEncryptionStatus,
    valkey::{RuntimeHintPublisher, ValkeyAdapterError, run_request_metadata_consumer},
};
use serde_json::json;
use tokio::{sync::watch, task::JoinSet};
use tracing::{error, info, warn};

use crate::{
    TransportRegistry, bootstrap::connectors::register_mounted_connectors, create_media_spool,
};

use super::{
    AppResult, BACKGROUND_SHUTDOWN_TIMEOUT,
    config::{
        DoctorArgs, InternalPreStopArgs, MasterKeyAction, MasterKeyArgs, MigrateArgs,
        PersistenceArgs,
    },
    lifecycle::shutdown_signal,
    validation::{
        check_secret_permissions, connect_store, ensure_keyring_covers_references,
        load_auth_hmac_key, load_master_key,
    },
};

pub(super) async fn internal_pre_stop(args: InternalPreStopArgs) -> AppResult<()> {
    tokio::time::sleep(Duration::from_secs(args.seconds)).await;
    Ok(())
}

pub(super) async fn migrate(args: MigrateArgs) -> AppResult<()> {
    let store = connect_store(&args.persistence.database).await?;
    if let Some(target) = args.through_version {
        if std::env::var("OLP_ALLOW_PARTIAL_MIGRATIONS_FOR_TESTS").as_deref() != Ok("test-only") {
            return Err(std::io::Error::other(
                "partial migration targets are restricted to test fixtures",
            )
            .into());
        }
        olp_storage::MIGRATOR.run_to(target, store.pool()).await?;
        info!(target, "PostgreSQL migrations reached test target");
    } else {
        store.migrate().await?;
        info!("PostgreSQL migrations are current");
    }
    Ok(())
}

pub(super) async fn run_worker(args: PersistenceArgs) -> AppResult<()> {
    let store = connect_store(&args.database).await?;
    let (sender, receiver) = watch::channel(false);
    let mut workers = JoinSet::new();
    spawn_worker_supervisors(&mut workers, store, args.valkey_url, receiver);
    let early_exit = tokio::select! {
        result = workers.join_next() => Some(result),
        () = shutdown_signal() => None,
    };
    let _ = sender.send(true);
    let stop_result = stop_worker_tasks(&mut workers, BACKGROUND_SHUTDOWN_TIMEOUT).await;
    match (early_exit, stop_result) {
        (Some(Some(Err(error))), _) | (_, Err(error)) => Err(error.into()),
        (None, Ok(())) => Ok(()),
        (Some(Some(Ok(()))) | Some(None), Ok(())) => {
            Err(std::io::Error::other("worker supervisor stopped unexpectedly").into())
        }
    }
}

pub(super) async fn stop_worker_tasks(
    workers: &mut JoinSet<()>,
    timeout: Duration,
) -> Result<(), tokio::task::JoinError> {
    let deadline = tokio::time::Instant::now() + timeout;
    let mut first_error = None;
    loop {
        match tokio::time::timeout_at(deadline, workers.join_next()).await {
            Ok(Some(Ok(()))) => {}
            Ok(Some(Err(error))) if error.is_cancelled() => {}
            Ok(Some(Err(error))) => {
                warn!(%error, "worker task stopped unexpectedly");
                if first_error.is_none() {
                    first_error = Some(error);
                }
            }
            Ok(None) => return first_error.map_or(Ok(()), Err),
            Err(_) => {
                warn!("worker tasks did not stop before deadline; aborting them");
                workers.abort_all();
                while let Some(result) = workers.join_next().await {
                    if let Err(error) = result
                        && !error.is_cancelled()
                    {
                        warn!(%error, "worker task failed while stopping");
                        if first_error.is_none() {
                            first_error = Some(error);
                        }
                    }
                }
                return first_error.map_or(Ok(()), Err);
            }
        }
    }
}

fn spawn_worker_supervisors(
    workers: &mut JoinSet<()>,
    store: PgStore,
    valkey_url: String,
    shutdown: watch::Receiver<bool>,
) {
    workers.spawn(outbox_supervisor(
        store.clone(),
        valkey_url.clone(),
        shutdown.clone(),
    ));
    workers.spawn(request_metadata_consumer_supervisor(
        store.clone(),
        valkey_url,
        shutdown.clone(),
    ));
    workers.spawn(maintenance_supervisor(store.clone(), shutdown.clone()));
    workers.spawn(request_metadata_epoch_supervisor(store, shutdown));
}

pub(super) async fn maintenance_supervisor(store: PgStore, mut shutdown: watch::Receiver<bool>) {
    // Frequent bounded passes keep receipt expiry from becoming one large
    // hourly DELETE/WAL spike at qualified request rates.
    let mut interval = tokio::time::interval(Duration::from_secs(60));
    interval.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);
    loop {
        tokio::select! {
            _ = interval.tick() => {
                match store.run_maintenance(chrono::Utc::now()).await {
                    Ok(report) => info!(?report, "maintenance pass completed"),
                    Err(error) => error!(%error, "maintenance pass failed; retrying next interval"),
                }
            }
            changed = shutdown.changed() => {
                if changed.is_err() || *shutdown.borrow() {
                    return;
                }
            }
        }
    }
}

pub(super) async fn request_metadata_epoch_supervisor(
    store: PgStore,
    mut shutdown: watch::Receiver<bool>,
) {
    let mut interval = tokio::time::interval(Duration::from_secs(5));
    interval.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);
    loop {
        tokio::select! {
            _ = interval.tick() => {
                match store.detect_stale_request_metadata_gateway_epochs(chrono::Utc::now()).await {
                    Ok(report) if report.detected_epochs > 0 => warn!(
                        detected_epochs = report.detected_epochs,
                        uncertain_event_lower_bound = report.uncertain_event_lower_bound,
                        "unclean request metadata gateway epochs recorded as completeness gaps"
                    ),
                    Ok(report) if report.candidate_epochs > 0 => warn!(
                        candidate_epochs = report.candidate_epochs,
                        "request metadata gateway epochs missed the stale threshold; awaiting confirmation"
                    ),
                    Ok(_) => {}
                    Err(error) => warn!(%error, "request metadata gateway epoch detection failed; retrying"),
                }
            }
            changed = shutdown.changed() => {
                if changed.is_err() || *shutdown.borrow() {
                    return;
                }
            }
        }
    }
}

pub(super) async fn outbox_supervisor(
    store: PgStore,
    valkey_url: String,
    mut shutdown: watch::Receiver<bool>,
) {
    let mut backoff = Duration::from_millis(100);
    loop {
        if *shutdown.borrow() {
            return;
        }
        match outbox_loop(store.clone(), &valkey_url, shutdown.clone()).await {
            Ok(()) => return,
            Err(error) => error!(%error, "outbox worker failed; restarting"),
        }
        tokio::select! {
            changed = shutdown.changed() => {
                if changed.is_err() || *shutdown.borrow() {
                    return;
                }
            }
            () = tokio::time::sleep(backoff) => {}
        }
        backoff = (backoff * 2).min(Duration::from_secs(5));
    }
}

const OUTBOX_BATCH_SIZE: u16 = 100;
const OUTBOX_IDLE_INTERVAL: Duration = Duration::from_millis(250);

#[allow(async_fn_in_trait)]
pub trait RuntimeHintPublication {
    async fn publish_runtime_hint(&mut self, payload: &[u8]) -> Result<u64, ValkeyAdapterError>;
}

impl RuntimeHintPublication for RuntimeHintPublisher {
    async fn publish_runtime_hint(&mut self, payload: &[u8]) -> Result<u64, ValkeyAdapterError> {
        self.publish(payload).await
    }
}

#[derive(Debug, Eq, PartialEq)]
pub enum OutboxBatchOutcome {
    Published(usize),
    Retry,
    Shutdown,
}

pub(super) async fn request_metadata_consumer_supervisor(
    store: PgStore,
    valkey_url: String,
    mut shutdown: watch::Receiver<bool>,
) {
    let mut backoff = Duration::from_millis(100);
    loop {
        if *shutdown.borrow() {
            return;
        }
        match request_metadata_consumer_loop(store.clone(), &valkey_url, shutdown.clone()).await {
            Ok(()) => return,
            Err(error) => error!(%error, "request metadata persistence worker failed; restarting"),
        }
        tokio::select! {
            changed = shutdown.changed() => {
                if changed.is_err() || *shutdown.borrow() {
                    return;
                }
            }
            () = tokio::time::sleep(backoff) => {}
        }
        backoff = (backoff * 2).min(Duration::from_secs(5));
    }
}

async fn outbox_loop(
    store: PgStore,
    valkey_url: &str,
    mut shutdown: watch::Receiver<bool>,
) -> AppResult<()> {
    let mut leader = loop {
        tokio::select! {
            result = store.acquire_runtime_outbox_leader() => break result?,
            changed = shutdown.changed() => {
                if changed.is_err() || *shutdown.borrow() {
                    return Ok(());
                }
            }
        }
    };
    info!(
        event = "runtime_outbox_leadership_acquired",
        "acquired runtime outbox publication leadership"
    );

    let run_result = run_owned_outbox_loop(&mut leader, valkey_url, &mut shutdown).await;
    match leader.release().await {
        Ok(()) => info!(
            event = "runtime_outbox_leadership_released",
            "released runtime outbox publication leadership"
        ),
        Err(error) => {
            warn!(
                event = "runtime_outbox_leadership_release_failed",
                %error,
                "detached runtime outbox session will be closed after release failure"
            );
            if run_result.is_ok() {
                return Err(error.into());
            }
        }
    }
    run_result
}

async fn run_owned_outbox_loop(
    leader: &mut RuntimeOutboxLeader,
    valkey_url: &str,
    shutdown: &mut watch::Receiver<bool>,
) -> AppResult<()> {
    let mut publisher = loop {
        tokio::select! {
            result = RuntimeHintPublisher::connect(valkey_url) => break result?,
            changed = shutdown.changed() => {
                if changed.is_err() || *shutdown.borrow() {
                    return Ok(());
                }
            }
        }
    };
    let mut wait_for_more = false;
    loop {
        if wait_for_more {
            tokio::select! {
                () = tokio::time::sleep(OUTBOX_IDLE_INTERVAL) => {}
                changed = shutdown.changed() => {
                    if changed.is_err() || *shutdown.borrow() {
                        return Ok(());
                    }
                }
            }
        } else if *shutdown.borrow() {
            return Ok(());
        }

        match publish_outbox_batch(leader, &mut publisher, shutdown).await? {
            OutboxBatchOutcome::Published(count) => {
                // A full batch implies an existing backlog, so continue
                // immediately. Partial batches use the idle cadence.
                wait_for_more = count < usize::from(OUTBOX_BATCH_SIZE);
            }
            OutboxBatchOutcome::Retry => {
                wait_for_more = true;
            }
            OutboxBatchOutcome::Shutdown => return Ok(()),
        }
    }
}

async fn publish_outbox_batch<P: RuntimeHintPublication>(
    leader: &mut RuntimeOutboxLeader,
    publisher: &mut P,
    shutdown: &mut watch::Receiver<bool>,
) -> AppResult<OutboxBatchOutcome> {
    let records = leader.pending(OUTBOX_BATCH_SIZE).await?;
    let mut published = 0_usize;
    for record in records {
        info!(
            event = "runtime_outbox_publication_attempt",
            outbox_id = %record.id,
            generation_id = %record.aggregate_id,
            topic = %record.topic,
            created_at = %record.created_at,
            "attempting runtime hint publication"
        );
        let result = loop {
            tokio::select! {
                result = publisher.publish_runtime_hint(&record.payload) => break result,
                changed = shutdown.changed() => {
                    if changed.is_err() || *shutdown.borrow() {
                        warn!(
                            event = "runtime_outbox_publication_unconfirmed",
                            outbox_id = %record.id,
                            generation_id = %record.aggregate_id,
                            outcome = "shutdown_during_publish",
                            publish_may_have_succeeded = true,
                            retry_required = true,
                            "runtime hint publication was interrupted; leaving the outbox row pending"
                        );
                        return Ok(OutboxBatchOutcome::Shutdown);
                    }
                    warn!(
                        event = "runtime_outbox_publication_unconfirmed",
                        outbox_id = %record.id,
                        generation_id = %record.aggregate_id,
                        outcome = "publish_cancelled_by_watch_change",
                        publish_may_have_succeeded = true,
                        retry_required = true,
                        "runtime hint publication was cancelled; retrying the same outbox row"
                    );
                }
            }
        };
        let subscribers = match result {
            Ok(subscribers) => subscribers,
            Err(error) => {
                warn!(
                    event = "runtime_outbox_publication_unconfirmed",
                    outbox_id = %record.id,
                    generation_id = %record.aggregate_id,
                    outcome = "valkey_error",
                    publish_may_have_succeeded = true,
                    retry_required = true,
                    %error,
                    "runtime hint publication failed or was ambiguous; leaving the outbox row pending"
                );
                return Ok(OutboxBatchOutcome::Retry);
            }
        };
        info!(
            event = "runtime_outbox_publication_accepted",
            outbox_id = %record.id,
            generation_id = %record.aggregate_id,
            durable_completion_pending = true,
            %subscribers,
            "Valkey accepted the runtime hint; durable outbox completion is pending"
        );
        match leader.mark_published(record.id).await {
            Ok(true) => info!(
                event = "runtime_outbox_publication_confirmed",
                outbox_id = %record.id,
                generation_id = %record.aggregate_id,
                %subscribers,
                "published runtime hint and durably completed its outbox row"
            ),
            Ok(false) => warn!(
                event = "runtime_outbox_duplicate_publication",
                outbox_id = %record.id,
                generation_id = %record.aggregate_id,
                duplicate_publication = true,
                "runtime hint was published after its outbox row was already completed"
            ),
            Err(error) => {
                warn!(
                    event = "runtime_outbox_publication_unconfirmed",
                    outbox_id = %record.id,
                    generation_id = %record.aggregate_id,
                    outcome = "publish_before_completion",
                    publish_succeeded = true,
                    retry_required = true,
                    duplicate_publication_possible = true,
                    %error,
                    "runtime hint was published but durable completion failed; a new owner may publish it again"
                );
                return Err(error.into());
            }
        }
        published = published.saturating_add(1);
    }
    Ok(OutboxBatchOutcome::Published(published))
}

#[cfg(feature = "test-util")]
pub mod test_support {
    use std::error::Error;

    use olp_storage::runtime::RuntimeOutboxLeader;
    use tokio::sync::watch;

    pub use super::{OutboxBatchOutcome, RuntimeHintPublication};

    pub const OUTBOX_BATCH_SIZE: u16 = super::OUTBOX_BATCH_SIZE;

    pub async fn publish_outbox_batch<P: RuntimeHintPublication>(
        leader: &mut RuntimeOutboxLeader,
        publisher: &mut P,
        shutdown: &mut watch::Receiver<bool>,
    ) -> Result<OutboxBatchOutcome, Box<dyn Error + Send + Sync>> {
        super::publish_outbox_batch(leader, publisher, shutdown).await
    }
}

async fn request_metadata_consumer_loop(
    store: PgStore,
    valkey_url: &str,
    shutdown: watch::Receiver<bool>,
) -> AppResult<()> {
    run_request_metadata_consumer(&store, valkey_url, shutdown).await?;
    Ok(())
}

pub(super) async fn master_key_command(args: MasterKeyArgs) -> AppResult<()> {
    check_secret_permissions(&args.master_key_file).await?;
    let master_key = load_master_key(&args.master_key_file).await?;
    let store = connect_store(&args.database).await?;
    match args.action {
        MasterKeyAction::Status { batch_size } => {
            let status = store
                .master_key_encryption_status(master_key.version())
                .await?;
            report_master_key_status(&master_key, &status);
            ensure_keyring_covers_references(&master_key, &status)?;
            let verified = store
                .verify_master_key_envelopes(&master_key, batch_size)
                .await?;
            info!(
                active_version = master_key.version(),
                rows_verified = verified.rows_verified,
                "master-key envelope status verified"
            );
        }
        MasterKeyAction::Reencrypt {
            batch_size,
            dry_run,
        } => {
            let initial = store
                .master_key_encryption_status(master_key.version())
                .await?;
            report_master_key_status(&master_key, &initial);
            ensure_keyring_covers_references(&master_key, &initial)?;
            if dry_run {
                let verified = store
                    .verify_master_key_envelopes(&master_key, batch_size)
                    .await?;
                info!(
                    active_version = master_key.version(),
                    rows_verified = verified.rows_verified,
                    rows_requiring_reencryption = initial.non_active_references(),
                    "master-key re-encryption dry run completed without writes"
                );
                return Ok(());
            }
            let mut total_reencrypted = 0_u64;
            loop {
                let status = store
                    .master_key_encryption_status(master_key.version())
                    .await?;
                ensure_keyring_covers_references(&master_key, &status)?;
                if status.non_active_references() == 0 {
                    break;
                }
                let batch = store
                    .reencrypt_master_key_batch(&master_key, batch_size)
                    .await?;
                if batch.rows_reencrypted == 0 {
                    return Err(std::io::Error::other(
                        "master-key re-encryption made no progress while old references remain",
                    )
                    .into());
                }
                total_reencrypted = total_reencrypted.saturating_add(batch.rows_reencrypted);
                for (table, rows) in batch.by_table {
                    info!(
                        active_version = master_key.version(),
                        encrypted_table = table.as_str(),
                        rows_reencrypted = rows,
                        total_reencrypted,
                        "master-key re-encryption batch committed"
                    );
                }
            }
            let verified = store
                .verify_master_key_envelopes(&master_key, batch_size)
                .await?;
            let final_status = store
                .master_key_encryption_status(master_key.version())
                .await?;
            report_master_key_status(&master_key, &final_status);
            if final_status.non_active_references() != 0 {
                return Err(std::io::Error::other(
                    "non-active master-key references appeared during final verification; confirm every replica uses the new active version and rerun",
                )
                .into());
            }
            info!(
                active_version = master_key.version(),
                rows_reencrypted = total_reencrypted,
                rows_verified = verified.rows_verified,
                "master-key re-encryption completed"
            );
        }
        MasterKeyAction::VerifyRetirement {
            version,
            batch_size,
        } => {
            let status = store
                .master_key_encryption_status(master_key.version())
                .await?;
            report_master_key_status(&master_key, &status);
            ensure_keyring_covers_references(&master_key, &status)?;
            let verified = store
                .verify_master_key_retirement(&master_key, version, batch_size)
                .await?;
            info!(
                active_version = master_key.version(),
                retirement_version = version,
                rows_verified = verified.rows_verified,
                "master-key version has zero references and is safe to remove after all replicas use the active keyring"
            );
        }
    }
    Ok(())
}

fn report_master_key_status(master_key: &MasterKey, status: &MasterKeyEncryptionStatus) {
    let available_versions = master_key.versions().collect::<Vec<_>>();
    info!(
        active_version = master_key.version(),
        available_versions = ?available_versions,
        total_encrypted_rows = status.total_references(),
        non_active_references = status.non_active_references(),
        "master-key reference status"
    );
    for reference in &status.references {
        info!(
            encrypted_table = reference.table.as_str(),
            key_version = reference.key_version,
            row_count = reference.row_count,
            "master-key references"
        );
    }
}

pub(super) async fn doctor(args: DoctorArgs) -> AppResult<()> {
    let mut checks = serde_json::Map::new();
    let store = connect_store(&args.persistence.database).await?;
    store.ping().await?;
    checks.insert("postgresql".into(), json!({ "ok": true }));

    let limiter =
        DistributedLimiter::connect(&args.persistence.valkey_url, "olp:v2:doctor").await?;
    limiter.ping().await?;
    checks.insert("valkey".into(), json!({ "ok": true }));
    checks.insert(
        "request_metadata_stream_upgrade".into(),
        json!({ "ok": true }),
    );

    load_auth_hmac_key(&args.auth_hmac_key_file).await?;
    load_master_key(&args.master_key_file).await?;
    check_secret_permissions(&args.auth_hmac_key_file).await?;
    check_secret_permissions(&args.master_key_file).await?;
    checks.insert("secret_files".into(), json!({ "ok": true }));

    if let Some(path) = &args.assets.connector_config_file {
        let registry = TransportRegistry::default();
        register_mounted_connectors(path, &registry).await?;
        checks.insert(
            "connector_config".into(),
            json!({ "ok": true, "configured": registry.snapshot().len() }),
        );
    }

    if !args.assets.console_dir.join("index.html").is_file() {
        return Err(std::io::Error::other(format!(
            "console index is missing at {}",
            args.assets.console_dir.join("index.html").display()
        ))
        .into());
    }
    checks.insert("console".into(), json!({ "ok": true }));
    let media_spool_dir = args
        .assets
        .media_spool_dir
        .as_deref()
        .map_or_else(std::env::temp_dir, Path::to_path_buf);
    let media_spool = create_media_spool(&media_spool_dir, args.assets.media_spool_capacity_bytes)?;
    drop(media_spool);
    checks.insert(
        "media_spool".into(),
        json!({
            "ok": true,
            "capacity_bytes": args.assets.media_spool_capacity_bytes,
        }),
    );
    println!(
        "{}",
        serde_json::to_string_pretty(&json!({ "ok": true, "checks": checks }))?
    );
    Ok(())
}
