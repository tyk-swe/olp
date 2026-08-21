use std::time::Duration;

use olp_db::{
    runtime::outbox::{RuntimeOutboxLeader, RuntimeOutboxLeadershipProbe},
    store::Store,
    valkey::{Error, RuntimeHintPublisher, request_metadata::run_request_metadata_consumer},
    worker_health::{WorkerTask, WorkerTaskCheckpointOutcome},
};
use tokio::{sync::watch, task::JoinSet};
use tracing::{error, info, warn};

use super::{
    AppResult, BACKGROUND_SHUTDOWN_TIMEOUT, config::PersistenceArgs, lifecycle::shutdown_signal,
    validation::connect_store,
};

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
    store: Store,
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

pub(super) async fn maintenance_supervisor(store: Store, mut shutdown: watch::Receiver<bool>) {
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
    store: Store,
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
    store: Store,
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

pub const OUTBOX_BATCH_SIZE: u16 = 100;
const OUTBOX_IDLE_INTERVAL: Duration = Duration::from_millis(250);
const OUTBOX_HEARTBEAT_INTERVAL: Duration = Duration::from_secs(5);
const OUTBOX_LEADERSHIP_RETRY_INTERVAL: Duration = Duration::from_secs(5);

#[allow(async_fn_in_trait)]
pub trait RuntimeHintPublication {
    async fn publish_runtime_hint(&mut self, payload: &[u8]) -> Result<u64, Error>;
}

impl RuntimeHintPublication for RuntimeHintPublisher {
    async fn publish_runtime_hint(&mut self, payload: &[u8]) -> Result<u64, Error> {
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
    store: Store,
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
    store: Store,
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

pub async fn publish_outbox_batch<P: RuntimeHintPublication>(
    leader: &mut RuntimeOutboxLeader,
    publisher: &mut P,
    shutdown: &mut watch::Receiver<bool>,
) -> Result<OutboxBatchOutcome, Box<dyn std::error::Error + Send + Sync>> {
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

async fn request_metadata_consumer_loop(
    store: Store,
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
