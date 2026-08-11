use std::{path::Path, time::Duration};

use base64::Engine as _;
use olp_db::{
    PgStore,
    limits::DistributedLimiter,
    runtime::{RuntimeOutboxLeader, RuntimeOutboxLeadershipProbe},
    security::MasterKey,
    security::MasterKeyEncryptionStatus,
    valkey::{RuntimeHintPublisher, ValkeyAdapterError, run_request_metadata_consumer},
    worker_health::{WorkerTask, WorkerTaskCheckpointOutcome},
};
use serde_json::json;
use sha2::{Digest, Sha256};
use tokio::{sync::watch, task::JoinSet};
use tracing::{error, info, warn};

use crate::{
    TransportRegistry, bootstrap::connectors::register_mounted_connectors, create_media_spool,
};

const DATABASE_IDENTITY_QUERY_PARAMS: &[&str] = &["dbname", "host", "hostaddr", "port"];

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
        olp_db::MIGRATOR.run_to(target, store.pool()).await?;
        info!(target, "PostgreSQL migrations reached test target");
    } else {
        let legacy_stream_claim_token =
            legacy_request_metadata_stream_claim_token(&args.persistence.database.database_url)?;
        let should_claim_legacy_stream =
            store.should_claim_legacy_request_metadata_stream().await?;
        let legacy_stream_claim_prepared = if should_claim_legacy_stream {
            olp_db::valkey::mark_legacy_request_metadata_stream_claim(
                &args.persistence.valkey_url,
                &legacy_stream_claim_token,
            )
            .await?
        } else {
            false
        };
        store.migrate().await?;
        info!("PostgreSQL migrations are current");
        let keyspace = store.valkey_keyspace().await?;
        let migrated = olp_db::valkey::migrate_claimed_legacy_request_metadata_stream(
            &args.persistence.valkey_url,
            &keyspace.request_metadata_stream(),
            &legacy_stream_claim_token,
        )
        .await?;
        if migrated || legacy_stream_claim_prepared {
            info!(
                migrated,
                stream = %keyspace.request_metadata_stream(),
                "legacy request metadata stream transition is complete"
            );
        } else {
            olp_db::valkey::verify_request_metadata_stream_upgrade(&args.persistence.valkey_url)
                .await?;
            info!("legacy request metadata stream claim skipped for non-upgrade database");
        }
    }
    Ok(())
}

pub(super) fn legacy_request_metadata_stream_claim_token(database_url: &str) -> AppResult<String> {
    let mut identity_url = url::Url::parse(database_url).map_err(|error| {
        std::io::Error::other(format!(
            "invalid database URL for legacy request metadata stream claim: {error}"
        ))
    })?;
    identity_url.set_username("").map_err(|()| {
        std::io::Error::other("database URL cannot be normalized for legacy stream claim")
    })?;
    identity_url.set_password(None).map_err(|()| {
        std::io::Error::other("database URL cannot be normalized for legacy stream claim")
    })?;
    let mut identity_query_params = identity_url
        .query_pairs()
        .filter(|(key, _)| DATABASE_IDENTITY_QUERY_PARAMS.contains(&key.as_ref()))
        .map(|(key, value)| (key.into_owned(), value.into_owned()))
        .collect::<Vec<_>>();
    identity_query_params.sort_unstable();
    identity_url.set_query(None);
    if !identity_query_params.is_empty() {
        let mut query_pairs = identity_url.query_pairs_mut();
        for (key, value) in identity_query_params {
            query_pairs.append_pair(&key, &value);
        }
    }
    identity_url.set_fragment(None);
    let digest = Sha256::digest(identity_url.as_str().as_bytes());
    Ok(format!(
        "database-url-sha256-v1:{}",
        base64::engine::general_purpose::URL_SAFE_NO_PAD.encode(digest)
    ))
}

pub(super) async fn run_worker(args: PersistenceArgs) -> AppResult<()> {
    let store = connect_store(&args.database).await?;
    let keyspace = store.valkey_keyspace().await?;
    test_worker_start_barrier().await?;
    let (sender, receiver) = watch::channel(false);
    let mut workers = JoinSet::new();
    spawn_worker_supervisors(
        &mut workers,
        store,
        args.valkey_url,
        keyspace.runtime_hint_channel(),
        keyspace.request_metadata_stream(),
        request_metadata_consumer_name(),
        receiver,
    );
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

#[cfg(all(feature = "test-util", debug_assertions))]
async fn test_worker_start_barrier() -> AppResult<()> {
    let Ok(marker) = std::env::var("OLP_TEST_WORKER_START_MARKER") else {
        return Ok(());
    };
    let release = format!("{marker}.release");
    std::fs::write(&marker, b"ready\n")?;
    while !std::path::Path::new(&release).exists() {
        tokio::time::sleep(Duration::from_millis(25)).await;
    }
    Ok(())
}

#[cfg(not(all(feature = "test-util", debug_assertions)))]
async fn test_worker_start_barrier() -> AppResult<()> {
    Ok(())
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
    runtime_hint_channel: String,
    request_metadata_stream: String,
    request_metadata_consumer: String,
    shutdown: watch::Receiver<bool>,
) {
    workers.spawn(outbox_supervisor(
        store.clone(),
        valkey_url.clone(),
        runtime_hint_channel,
        shutdown.clone(),
    ));
    workers.spawn(request_metadata_consumer_supervisor(
        store.clone(),
        valkey_url,
        request_metadata_stream,
        request_metadata_consumer,
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
                    Ok(report) => {
                        let outcome = if report.lock_acquired {
                            WorkerTaskCheckpointOutcome::Success
                        } else {
                            WorkerTaskCheckpointOutcome::Skipped
                        };
                        if let Err(error) = store
                            .report_worker_task_checkpoint(
                                WorkerTask::Maintenance,
                                outcome,
                                report.lock_acquired,
                            )
                            .await
                        {
                            warn!(%error, "maintenance health checkpoint failed");
                        }
                        if report.lock_acquired {
                            info!(?report, "maintenance pass completed");
                        }
                    }
                    Err(error) => {
                        if let Err(checkpoint_error) = store
                            .report_worker_task_checkpoint(
                                WorkerTask::Maintenance,
                                WorkerTaskCheckpointOutcome::Failure,
                                false,
                            )
                            .await
                        {
                            warn!(%checkpoint_error, "maintenance failure checkpoint failed");
                        }
                        error!(%error, "maintenance pass failed; retrying next interval");
                    }
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
                    Ok(report) => {
                        if let Err(error) = store
                            .report_worker_task_checkpoint(
                                WorkerTask::RequestMetadataGatewayEpochDetection,
                                WorkerTaskCheckpointOutcome::Success,
                                report.candidate_epochs > 0 || report.detected_epochs > 0,
                            )
                            .await
                        {
                            warn!(%error, "request metadata gateway epoch health checkpoint failed");
                        }
                        if report.detected_epochs > 0 {
                            warn!(
                                detected_epochs = report.detected_epochs,
                                uncertain_event_lower_bound = report.uncertain_event_lower_bound,
                                "unclean request metadata gateway epochs recorded as completeness gaps"
                            );
                        } else if report.candidate_epochs > 0 {
                            warn!(
                                candidate_epochs = report.candidate_epochs,
                                "request metadata gateway epochs missed the stale threshold; awaiting confirmation"
                            );
                        }
                    }
                    Err(error) => {
                        if let Err(checkpoint_error) = store
                            .report_worker_task_checkpoint(
                                WorkerTask::RequestMetadataGatewayEpochDetection,
                                WorkerTaskCheckpointOutcome::Failure,
                                false,
                            )
                            .await
                        {
                            warn!(%checkpoint_error, "request metadata gateway epoch failure checkpoint failed");
                        }
                        warn!(%error, "request metadata gateway epoch detection failed; retrying");
                    }
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
    runtime_hint_channel: String,
    mut shutdown: watch::Receiver<bool>,
) {
    let mut backoff = Duration::from_millis(100);
    loop {
        if *shutdown.borrow() {
            return;
        }
        match outbox_loop(
            store.clone(),
            &valkey_url,
            &runtime_hint_channel,
            shutdown.clone(),
        )
        .await
        {
            Ok(()) => return,
            Err(error) => {
                if let Err(checkpoint_error) = store
                    .report_worker_task_checkpoint(
                        WorkerTask::RuntimeOutbox,
                        WorkerTaskCheckpointOutcome::Failure,
                        false,
                    )
                    .await
                {
                    warn!(%checkpoint_error, "outbox failure checkpoint failed");
                }
                error!(%error, "outbox worker failed; restarting");
            }
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
const OUTBOX_HEARTBEAT_INTERVAL: Duration = Duration::from_secs(5);
const OUTBOX_LEADERSHIP_RETRY_INTERVAL: Duration = Duration::from_secs(5);

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
    request_metadata_stream: String,
    consumer: String,
    mut shutdown: watch::Receiver<bool>,
) {
    let mut backoff = Duration::from_millis(100);
    loop {
        if *shutdown.borrow() {
            return;
        }
        match request_metadata_consumer_loop(
            store.clone(),
            &valkey_url,
            &request_metadata_stream,
            &consumer,
            shutdown.clone(),
        )
        .await
        {
            Ok(()) => return,
            Err(error) => {
                if let Err(checkpoint_error) = store
                    .report_worker_task_checkpoint(
                        WorkerTask::RequestMetadataConsumer,
                        WorkerTaskCheckpointOutcome::Failure,
                        false,
                    )
                    .await
                {
                    warn!(%checkpoint_error, "request metadata consumer failure checkpoint failed");
                }
                error!(%error, "request metadata persistence worker failed; restarting");
            }
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
    runtime_hint_channel: &str,
    mut shutdown: watch::Receiver<bool>,
) -> AppResult<()> {
    let mut contender = Some(store.runtime_outbox_leader_contender().await?);
    let mut leader = loop {
        tokio::select! {
            result = contender
                .take()
                .expect("runtime outbox contender must be present before each probe")
                .try_acquire(&store) => {
                match result? {
                    RuntimeOutboxLeadershipProbe::Acquired(leader) => break leader,
                    RuntimeOutboxLeadershipProbe::Pending(pending) => contender = Some(pending),
                }
            }
            changed = shutdown.changed() => {
                if changed.is_err() || *shutdown.borrow() {
                    return Ok(());
                }
            }
        }
        tokio::select! {
            () = tokio::time::sleep(OUTBOX_LEADERSHIP_RETRY_INTERVAL) => {}
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

    let run_result =
        run_owned_outbox_loop(&mut leader, valkey_url, runtime_hint_channel, &mut shutdown).await;
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
    runtime_hint_channel: &str,
    shutdown: &mut watch::Receiver<bool>,
) -> AppResult<()> {
    let publisher = RuntimeHintPublisher::connect(valkey_url, runtime_hint_channel);
    tokio::pin!(publisher);
    let mut connect_heartbeat = tokio::time::interval(OUTBOX_HEARTBEAT_INTERVAL);
    connect_heartbeat.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);
    let mut publisher = loop {
        tokio::select! {
            result = &mut publisher => break result?,
            _ = connect_heartbeat.tick() => leader.heartbeat().await?,
            changed = shutdown.changed() => {
                if changed.is_err() || *shutdown.borrow() {
                    return Ok(());
                }
            }
        }
    };
    let mut wait_for_more = false;
    let mut heartbeat_due = tokio::time::Instant::now() + OUTBOX_HEARTBEAT_INTERVAL;
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

        if tokio::time::Instant::now() >= heartbeat_due {
            leader.heartbeat().await?;
            heartbeat_due = tokio::time::Instant::now() + OUTBOX_HEARTBEAT_INTERVAL;
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
    'records: for record in records {
        let result = 'attempts: loop {
            let Some(attempt) = leader.begin_publication(record.id).await? else {
                warn!(
                    event = "runtime_outbox_claim_disappeared",
                    outbox_id = %record.id,
                    generation_id = %record.aggregate_id,
                    "runtime outbox row was completed before its publication attempt"
                );
                continue 'records;
            };
            #[cfg(all(feature = "test-util", debug_assertions))]
            block_after_test_outbox_claim().await;
            info!(
                event = "runtime_outbox_publication_attempt",
                outbox_id = %record.id,
                generation_id = %record.aggregate_id,
                topic = %record.topic,
                created_at = %record.created_at,
                attempt,
                repeated_attempt = attempt > 1,
                "attempting runtime hint publication"
            );
            let publication = publisher.publish_runtime_hint(&record.payload);
            tokio::pin!(publication);
            let mut heartbeat = tokio::time::interval(OUTBOX_HEARTBEAT_INTERVAL);
            heartbeat.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);
            heartbeat.tick().await;
            loop {
                tokio::select! {
                    result = &mut publication => break 'attempts result,
                    _ = heartbeat.tick() => leader.heartbeat().await?,
                    changed = shutdown.changed() => {
                        leader.record_publication_retry().await?;
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
                        break;
                    }
                }
            }
        };
        let subscribers = match result {
            Ok(subscribers) => subscribers,
            Err(error) => {
                leader.record_publication_retry().await?;
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

    use olp_db::runtime::RuntimeOutboxLeader;
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
    request_metadata_stream: &str,
    consumer: &str,
    shutdown: watch::Receiver<bool>,
) -> AppResult<()> {
    run_request_metadata_consumer(
        &store,
        valkey_url,
        request_metadata_stream,
        consumer,
        shutdown,
    )
    .await?;
    Ok(())
}

#[cfg(all(feature = "test-util", debug_assertions))]
async fn block_after_test_outbox_claim() {
    let Ok(marker) = std::env::var("OLP_TEST_OUTBOX_OWNED_MARKER") else {
        return;
    };
    let marker = std::path::Path::new(&marker);
    match std::fs::OpenOptions::new()
        .write(true)
        .create_new(true)
        .open(marker)
    {
        Ok(_) => std::future::pending::<()>().await,
        Err(error) if error.kind() == std::io::ErrorKind::AlreadyExists => {}
        Err(error) => panic!("failed to create outbox failpoint marker: {error}"),
    }
}

/// Combines a bounded, log-safe host label with the OS process ID and a UUIDv7
/// process epoch. The supervisor retains this name across transient reconnects.
pub(super) fn request_metadata_consumer_name() -> String {
    let host = std::env::var("HOSTNAME").unwrap_or_else(|_| "olp".to_owned());
    request_metadata_consumer_name_from(&host, std::process::id(), uuid::Uuid::now_v7())
}

pub(super) fn request_metadata_consumer_name_from(
    host: &str,
    process_id: u32,
    epoch: uuid::Uuid,
) -> String {
    let mut host = host
        .chars()
        .map(|character| {
            if character.is_ascii_alphanumeric() || character == '-' || character == '_' {
                character.to_ascii_lowercase()
            } else {
                '-'
            }
        })
        .take(48)
        .collect::<String>();
    if host.is_empty() {
        host.push_str("olp");
    }
    format!("{host}-{process_id}-{}", epoch.simple())
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

    let keyspace = store.valkey_keyspace().await?;
    let limiter = DistributedLimiter::connect(
        &args.persistence.valkey_url,
        &format!("{}:doctor", keyspace.prefix()),
    )
    .await?;
    limiter.ping().await?;
    checks.insert("valkey".into(), json!({ "ok": true }));
    olp_db::valkey::verify_request_metadata_stream_upgrade(&args.persistence.valkey_url).await?;
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
