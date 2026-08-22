use std::{
    collections::BTreeSet,
    sync::{Arc, Mutex},
    time::Duration,
};

use chrono::{TimeDelta, Utc};
use olp::bootstrap::cli::worker::{
    OUTBOX_BATCH_SIZE, OutboxBatchOutcome, RuntimeHintPublication, publish_outbox_batch,
};
use olp_db::{
    runtime::outbox::RuntimeOutboxState, store::Store, test_support::TestDb, valkey::Error,
};
use tokio::sync::{Barrier, oneshot, watch};
use uuid::Uuid;

#[derive(Clone, Default)]
struct RecordingPublisher {
    state: Arc<Mutex<RecordingPublisherState>>,
}

#[derive(Default)]
struct RecordingPublisherState {
    attempts: Vec<Vec<u8>>,
    failures_remaining: usize,
}

impl RecordingPublisher {
    fn failing_once() -> Self {
        Self {
            state: Arc::new(Mutex::new(RecordingPublisherState {
                attempts: Vec::new(),
                failures_remaining: 1,
            })),
        }
    }

    fn attempts(&self) -> Vec<Vec<u8>> {
        self.state.lock().unwrap().attempts.clone()
    }
}

impl RuntimeHintPublication for RecordingPublisher {
    async fn publish_runtime_hint(&mut self, payload: &[u8]) -> Result<u64, Error> {
        let mut state = self.state.lock().unwrap();
        state.attempts.push(payload.to_vec());
        if state.failures_remaining > 0 {
            state.failures_remaining -= 1;
            return Err(Error::InvalidState("injected ambiguous publication result"));
        }
        Ok(1)
    }
}

struct BlockingPublisher {
    started: Option<oneshot::Sender<()>>,
}

impl RuntimeHintPublication for BlockingPublisher {
    async fn publish_runtime_hint(&mut self, _payload: &[u8]) -> Result<u64, Error> {
        if let Some(started) = self.started.take() {
            let _ = started.send(());
        }
        std::future::pending().await
    }
}

struct RetryAfterWatchChangePublisher {
    state: Arc<Mutex<RetryAfterWatchChangeState>>,
    first_started: Option<oneshot::Sender<()>>,
}

#[derive(Default)]
struct RetryAfterWatchChangeState {
    attempts: Vec<Vec<u8>>,
}

impl RetryAfterWatchChangePublisher {
    fn new(first_started: oneshot::Sender<()>) -> Self {
        Self {
            state: Arc::new(Mutex::new(RetryAfterWatchChangeState::default())),
            first_started: Some(first_started),
        }
    }
}

impl RuntimeHintPublication for RetryAfterWatchChangePublisher {
    async fn publish_runtime_hint(&mut self, payload: &[u8]) -> Result<u64, Error> {
        let first_attempt = {
            let mut state = self.state.lock().unwrap();
            state.attempts.push(payload.to_vec());
            state.attempts.len() == 1
        };
        if first_attempt {
            if let Some(started) = self.first_started.take() {
                let _ = started.send(());
            }
            std::future::pending().await
        } else {
            Ok(1)
        }
    }
}

#[derive(Clone)]
struct TestOutboxRow {
    id: Uuid,
    payload: Vec<u8>,
}

async fn insert_outbox_rows(store: &Store, count: usize) -> Vec<TestOutboxRow> {
    let first_created_at = Utc::now() - TimeDelta::minutes(1);
    let mut rows = Vec::with_capacity(count);
    for index in 0..count {
        let id = Uuid::now_v7();
        let aggregate_id = Uuid::now_v7();
        let payload = u64::try_from(index).unwrap().to_be_bytes().to_vec();
        let created_at = first_created_at + TimeDelta::milliseconds(i64::try_from(index).unwrap());
        sqlx::query(
            "INSERT INTO transactional_outbox \
             (id, topic, aggregate_id, payload, created_at) \
             VALUES ($1, 'runtime.generation.activated', $2, $3, $4)",
        )
        .bind(id)
        .bind(aggregate_id)
        .bind(&payload)
        .bind(created_at)
        .execute(store.pool())
        .await
        .unwrap();
        rows.push(TestOutboxRow { id, payload });
    }
    rows
}

async fn unpublished_count(store: &Store) -> i64 {
    sqlx::query_scalar("SELECT count(*) FROM transactional_outbox WHERE published_at IS NULL")
        .fetch_one(store.pool())
        .await
        .unwrap()
}

#[tokio::test]
#[ignore = "requires PostgreSQL via make db-test"]
async fn three_workers_publish_each_outbox_row_once_during_ordinary_operation() {
    let db = TestDb::create_migrated("outbox_three_workers").await;
    let store = db.store(8).await;
    let rows = insert_outbox_rows(&store, 12).await;
    let publisher = RecordingPublisher::default();
    let (_shutdown_sender, shutdown) = watch::channel(false);
    let barrier = Arc::new(Barrier::new(4));
    let mut workers = tokio::task::JoinSet::new();

    for _ in 0..3 {
        let store = store.clone();
        let mut publisher = publisher.clone();
        let mut shutdown = shutdown.clone();
        let barrier = Arc::clone(&barrier);
        workers.spawn(async move {
            barrier.wait().await;
            let mut leader = store.acquire_runtime_outbox_leader().await.unwrap();
            let outcome = publish_outbox_batch(&mut leader, &mut publisher, &mut shutdown)
                .await
                .unwrap();
            leader.release().await.unwrap();
            outcome
        });
    }
    barrier.wait().await;

    let mut outcomes = Vec::new();
    while let Some(result) = workers.join_next().await {
        outcomes.push(result.unwrap());
    }
    outcomes.sort_by_key(|outcome| match outcome {
        OutboxBatchOutcome::Published(count) => *count,
        OutboxBatchOutcome::Retry => usize::MAX - 1,
        OutboxBatchOutcome::Shutdown => usize::MAX,
    });

    assert_eq!(
        outcomes,
        vec![
            OutboxBatchOutcome::Published(0),
            OutboxBatchOutcome::Published(0),
            OutboxBatchOutcome::Published(12),
        ]
    );
    let attempts = publisher.attempts();
    assert_eq!(attempts.len(), rows.len());
    assert_eq!(
        attempts.iter().cloned().collect::<BTreeSet<_>>().len(),
        rows.len()
    );
    assert_eq!(unpublished_count(&store).await, 0);
}

#[tokio::test]
#[ignore = "requires PostgreSQL via make db-test"]
async fn runtime_outbox_owner_death_before_reading_a_batch_allows_takeover() {
    let db = TestDb::create_migrated("outbox_death_before_read").await;
    let store = db.store(1).await;
    let mut leader = store.acquire_runtime_outbox_leader().await.unwrap();
    let owner_backend_pid = leader.backend_pid().await.unwrap();
    // Leadership uses a detached session and must not consume the ordinary
    // pool's sole configured connection.
    store.ping().await.unwrap();

    drop(leader);

    let mut takeover = tokio::time::timeout(
        Duration::from_secs(2),
        store.acquire_runtime_outbox_leader(),
    )
    .await
    .expect("a dropped owning session must release its advisory lock")
    .unwrap();
    let takeover_backend_pid = takeover.backend_pid().await.unwrap();
    assert_ne!(takeover_backend_pid, owner_backend_pid);
    takeover.release().await.unwrap();

    let mut after_clean_release = store.acquire_runtime_outbox_leader().await.unwrap();
    assert_ne!(
        after_clean_release.backend_pid().await.unwrap(),
        takeover_backend_pid
    );
    after_clean_release.release().await.unwrap();
}

#[tokio::test]
#[ignore = "requires PostgreSQL via make db-test"]
async fn runtime_outbox_owner_panic_drops_its_session_and_allows_takeover() {
    let db = TestDb::create_migrated("outbox_owner_panic").await;
    let store = db.store(1).await;
    let owner_store = store.clone();
    let (acquired_sender, acquired_receiver) = oneshot::channel();
    let owner: tokio::task::JoinHandle<()> = tokio::spawn(async move {
        let mut leader = owner_store.acquire_runtime_outbox_leader().await.unwrap();
        acquired_sender
            .send(leader.backend_pid().await.unwrap())
            .unwrap();
        panic!("injected panic while holding runtime outbox leadership");
    });
    let owner_backend_pid = acquired_receiver.await.unwrap();

    let panic = owner.await.expect_err("the owning task must panic");
    assert!(panic.is_panic());

    let mut takeover = tokio::time::timeout(
        Duration::from_secs(2),
        store.acquire_runtime_outbox_leader(),
    )
    .await
    .expect("dropping the detached session during panic must release leadership")
    .unwrap();
    assert_ne!(takeover.backend_pid().await.unwrap(), owner_backend_pid);
    takeover.release().await.unwrap();
    store.ping().await.unwrap();
}

#[tokio::test]
#[ignore = "requires PostgreSQL via make db-test"]
async fn runtime_outbox_owner_death_after_read_before_publish_leaves_the_row_available() {
    let db = TestDb::create_migrated("outbox_death_before_publish").await;
    let store = db.store(4).await;
    let rows = insert_outbox_rows(&store, 1).await;
    let mut old_owner = store.acquire_runtime_outbox_leader().await.unwrap();
    assert_eq!(old_owner.pending(OUTBOX_BATCH_SIZE).await.unwrap().len(), 1);

    drop(old_owner);

    let mut takeover = tokio::time::timeout(
        Duration::from_secs(2),
        store.acquire_runtime_outbox_leader(),
    )
    .await
    .expect("a replacement owner must acquire after process death")
    .unwrap();
    let mut publisher = RecordingPublisher::default();
    let (_shutdown_sender, mut shutdown) = watch::channel(false);
    assert_eq!(
        publish_outbox_batch(&mut takeover, &mut publisher, &mut shutdown)
            .await
            .unwrap(),
        OutboxBatchOutcome::Published(1)
    );
    takeover.release().await.unwrap();
    assert_eq!(publisher.attempts(), vec![rows[0].payload.clone()]);
    assert_eq!(unpublished_count(&store).await, 0);
}

#[tokio::test]
#[ignore = "requires PostgreSQL via make db-test"]
async fn runtime_outbox_death_after_publish_before_mark_causes_only_an_idempotent_hint_retry() {
    let db = TestDb::create_migrated("outbox_death_after_publish").await;
    let store = db.store(4).await;
    let rows = insert_outbox_rows(&store, 1).await;
    let mut old_owner = store.acquire_runtime_outbox_leader().await.unwrap();
    let record = old_owner
        .pending(OUTBOX_BATCH_SIZE)
        .await
        .unwrap()
        .remove(0);
    assert_eq!(
        old_owner.begin_publication(record.id).await.unwrap(),
        Some(1)
    );
    let mut publisher = RecordingPublisher::default();
    publisher
        .publish_runtime_hint(&record.payload)
        .await
        .unwrap();

    drop(old_owner);

    let mut takeover = tokio::time::timeout(
        Duration::from_secs(2),
        store.acquire_runtime_outbox_leader(),
    )
    .await
    .expect("the publish-before-mark crash must not strand the row")
    .unwrap();
    let (_shutdown_sender, mut shutdown) = watch::channel(false);
    assert_eq!(
        publish_outbox_batch(&mut takeover, &mut publisher, &mut shutdown)
            .await
            .unwrap(),
        OutboxBatchOutcome::Published(1)
    );
    takeover.release().await.unwrap();

    assert_eq!(
        publisher.attempts(),
        vec![rows[0].payload.clone(), rows[0].payload.clone()]
    );
    assert_eq!(unpublished_count(&store).await, 0);
}

#[tokio::test]
#[ignore = "requires PostgreSQL via make db-test"]
async fn runtime_outbox_ambiguous_valkey_error_keeps_the_row_pending_for_retry() {
    let db = TestDb::create_migrated("outbox_ambiguous_valkey").await;
    let store = db.store(4).await;
    let rows = insert_outbox_rows(&store, 1).await;
    let mut publisher = RecordingPublisher::failing_once();
    let (_shutdown_sender, mut shutdown) = watch::channel(false);
    let mut first_owner = store.acquire_runtime_outbox_leader().await.unwrap();

    assert_eq!(
        publish_outbox_batch(&mut first_owner, &mut publisher, &mut shutdown)
            .await
            .unwrap(),
        OutboxBatchOutcome::Retry
    );
    assert_eq!(unpublished_count(&store).await, 1);
    let pending = store.runtime_outbox_status().await.unwrap();
    assert_eq!(pending.state, RuntimeOutboxState::Backlogged);
    assert_eq!(pending.pending_rows, 1);
    assert_eq!(pending.claimed_rows, 0);
    let counters = store.worker_recovery_counters().await.unwrap();
    assert_eq!(counters.runtime_outbox_attempts, 1);
    assert_eq!(counters.runtime_outbox_retry_scheduled, 1);
    assert_eq!(counters.runtime_outbox_repeated_attempts, 0);

    assert_eq!(
        publish_outbox_batch(&mut first_owner, &mut publisher, &mut shutdown)
            .await
            .unwrap(),
        OutboxBatchOutcome::Published(1)
    );
    first_owner.release().await.unwrap();
    assert_eq!(
        publisher.attempts(),
        vec![rows[0].payload.clone(), rows[0].payload.clone()]
    );
    assert_eq!(unpublished_count(&store).await, 0);
    let counters = store.worker_recovery_counters().await.unwrap();
    assert_eq!(counters.runtime_outbox_attempts, 2);
    assert_eq!(counters.runtime_outbox_retry_scheduled, 1);
    assert_eq!(counters.runtime_outbox_repeated_attempts, 1);
    assert_eq!(counters.runtime_outbox_published, 1);
}

#[tokio::test]
#[ignore = "requires PostgreSQL via make db-test"]
async fn stale_outbox_owner_failed_takeover_and_abandoned_claim_are_durable() {
    let db = TestDb::create_migrated("outbox_failed_takeover_visibility").await;
    let store = db.store(6).await;
    let rows = insert_outbox_rows(&store, 1).await;
    let mut owner = store.acquire_runtime_outbox_leader().await.unwrap();
    assert_eq!(owner.begin_publication(rows[0].id).await.unwrap(), Some(1));
    sqlx::query(
        "UPDATE runtime_outbox_health SET checked_at = now() - interval '1 minute' \
         WHERE singleton",
    )
    .execute(store.pool())
    .await
    .unwrap();

    assert!(
        store
            .try_acquire_runtime_outbox_leader()
            .await
            .unwrap()
            .is_none()
    );
    let stale = store.runtime_outbox_status().await.unwrap();
    assert_eq!(stale.state, RuntimeOutboxState::Stale);
    assert_eq!(stale.pending_rows, 1);
    assert_eq!(stale.claimed_rows, 1);
    assert!(stale.ownership_abandoned());
    assert_eq!(
        store
            .worker_recovery_counters()
            .await
            .unwrap()
            .runtime_outbox_failed_takeovers,
        1
    );

    drop(owner);
    let takeover = store
        .try_acquire_runtime_outbox_leader()
        .await
        .unwrap()
        .expect("the abandoned PostgreSQL session lock must be recoverable");
    let counters = store.worker_recovery_counters().await.unwrap();
    assert_eq!(counters.runtime_outbox_abandoned_ownership, 1);
    assert_eq!(counters.runtime_outbox_abandoned_claims, 1);
    takeover.release().await.unwrap();

    let handoff = store.runtime_outbox_status().await.unwrap();
    assert!(!handoff.owner_active);
    assert!(!handoff.ownership_abandoned());
    let clean_owner = store
        .try_acquire_runtime_outbox_leader()
        .await
        .unwrap()
        .expect("cleanly released leadership must be immediately available");
    let after_clean_handoff = store.worker_recovery_counters().await.unwrap();
    assert_eq!(after_clean_handoff.runtime_outbox_abandoned_ownership, 1);
    assert_eq!(after_clean_handoff.runtime_outbox_abandoned_claims, 1);
    clean_owner.release().await.unwrap();
}

#[tokio::test]
#[ignore = "requires PostgreSQL via make db-test"]
async fn runtime_outbox_stale_owner_cannot_complete_after_postgres_fences_its_session() {
    let db = TestDb::create_migrated("outbox_stale_owner").await;
    let store = db.store(5).await;
    let rows = insert_outbox_rows(&store, 1).await;
    let mut old_owner = store.acquire_runtime_outbox_leader().await.unwrap();
    let backend_pid = old_owner.backend_pid().await.unwrap();
    let terminated: bool = sqlx::query_scalar("SELECT pg_terminate_backend($1)")
        .bind(backend_pid)
        .fetch_one(store.pool())
        .await
        .unwrap();
    assert!(terminated);

    let mut takeover = tokio::time::timeout(
        Duration::from_secs(2),
        store.acquire_runtime_outbox_leader(),
    )
    .await
    .expect("PostgreSQL must release leadership with the terminated session")
    .unwrap();
    assert!(old_owner.mark_published(rows[0].id).await.is_err());

    let mut publisher = RecordingPublisher::default();
    let (_shutdown_sender, mut shutdown) = watch::channel(false);
    assert_eq!(
        publish_outbox_batch(&mut takeover, &mut publisher, &mut shutdown)
            .await
            .unwrap(),
        OutboxBatchOutcome::Published(1)
    );
    takeover.release().await.unwrap();
    assert_eq!(unpublished_count(&store).await, 0);
}

#[tokio::test]
#[ignore = "requires PostgreSQL via make db-test"]
async fn runtime_outbox_backlog_drains_in_bounded_oldest_first_batches() {
    let db = TestDb::create_migrated("outbox_bounded_backlog").await;
    let store = db.store(4).await;
    let rows = insert_outbox_rows(&store, 205).await;
    let mut leader = store.acquire_runtime_outbox_leader().await.unwrap();
    let mut publisher = RecordingPublisher::default();
    let (_shutdown_sender, mut shutdown) = watch::channel(false);

    for expected in [100, 100, 5] {
        assert_eq!(
            publish_outbox_batch(&mut leader, &mut publisher, &mut shutdown)
                .await
                .unwrap(),
            OutboxBatchOutcome::Published(expected)
        );
    }
    leader.release().await.unwrap();

    assert_eq!(
        publisher.attempts(),
        rows.into_iter().map(|row| row.payload).collect::<Vec<_>>()
    );
    assert_eq!(unpublished_count(&store).await, 0);
}

#[tokio::test]
#[ignore = "requires PostgreSQL via make db-test"]
async fn runtime_outbox_shutdown_during_publish_releases_ownership_without_marking_the_row() {
    let db = TestDb::create_migrated("outbox_shutdown_during_publish").await;
    let store = db.store(4).await;
    insert_outbox_rows(&store, 1).await;
    let (shutdown_sender, mut shutdown) = watch::channel(false);
    let (started, started_receiver) = oneshot::channel();
    let mut leader = store.acquire_runtime_outbox_leader().await.unwrap();
    let publication = tokio::spawn(async move {
        let mut publisher = BlockingPublisher {
            started: Some(started),
        };
        let outcome = publish_outbox_batch(&mut leader, &mut publisher, &mut shutdown)
            .await
            .unwrap();
        leader.release().await.unwrap();
        outcome
    });
    started_receiver.await.unwrap();

    shutdown_sender.send(true).unwrap();

    assert_eq!(publication.await.unwrap(), OutboxBatchOutcome::Shutdown);
    assert_eq!(unpublished_count(&store).await, 1);
    let takeover = tokio::time::timeout(
        Duration::from_secs(2),
        store.acquire_runtime_outbox_leader(),
    )
    .await
    .expect("clean shutdown must release leadership")
    .unwrap();
    takeover.release().await.unwrap();
}

#[tokio::test]
#[ignore = "requires PostgreSQL via make db-test"]
async fn runtime_outbox_non_shutdown_watch_change_retries_the_same_row() {
    let db = TestDb::create_migrated("outbox_watch_change_retry").await;
    let store = db.store(4).await;
    let rows = insert_outbox_rows(&store, 2).await;
    let (shutdown_sender, mut shutdown) = watch::channel(false);
    let (started, started_receiver) = oneshot::channel();
    let mut leader = store.acquire_runtime_outbox_leader().await.unwrap();
    let publisher = RetryAfterWatchChangePublisher::new(started);
    let publisher_state = Arc::clone(&publisher.state);
    let publication = tokio::spawn(async move {
        let mut publisher = publisher;
        let outcome = publish_outbox_batch(&mut leader, &mut publisher, &mut shutdown)
            .await
            .unwrap();
        leader.release().await.unwrap();
        outcome
    });
    started_receiver.await.unwrap();

    shutdown_sender.send(false).unwrap();

    assert_eq!(publication.await.unwrap(), OutboxBatchOutcome::Published(2));
    assert_eq!(
        publisher_state.lock().unwrap().attempts.clone(),
        vec![
            rows[0].payload.clone(),
            rows[0].payload.clone(),
            rows[1].payload.clone(),
        ]
    );
    assert_eq!(unpublished_count(&store).await, 0);
}
