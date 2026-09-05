use crate::application::error::AppResult;
use olp_db::{
    runtime::outbox::{RuntimeOutboxLeader, RuntimeOutboxLeadershipProbe},
    valkey::{Error, RuntimeHintPublisher},
};
use olp_db::{
    store::Store,
    worker_health::{WorkerTask, WorkerTaskCheckpointOutcome},
};
use std::time::Duration;
use tokio::sync::watch;
use tracing::{error, info, warn};
pub(crate) async fn outbox_supervisor(
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
    for record in records {
        match publish_outbox_record(leader, publisher, shutdown, &record).await? {
            OutboxBatchOutcome::Published(count) => published = published.saturating_add(count),
            outcome => return Ok(outcome),
        }
    }
    Ok(OutboxBatchOutcome::Published(published))
}

async fn publish_outbox_record<P: RuntimeHintPublication>(
    leader: &mut RuntimeOutboxLeader,
    publisher: &mut P,
    shutdown: &mut watch::Receiver<bool>,
    record: &olp_db::runtime::OutboxRecord,
) -> AppResult<OutboxBatchOutcome> {
    let result = 'attempts: loop {
        let Some(attempt) = leader.begin_publication(record.id).await? else {
            warn!(
                event = "runtime_outbox_claim_disappeared",
                outbox_id = %record.id,
                generation_id = %record.aggregate_id,
                "runtime outbox row was completed before its publication attempt"
            );
            return Ok(OutboxBatchOutcome::Published(0));
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
    complete_outbox_publication(leader, record, subscribers).await?;
    Ok(OutboxBatchOutcome::Published(1))
}

async fn complete_outbox_publication(
    leader: &mut RuntimeOutboxLeader,
    record: &olp_db::runtime::OutboxRecord,
    subscribers: u64,
) -> AppResult<()> {
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
