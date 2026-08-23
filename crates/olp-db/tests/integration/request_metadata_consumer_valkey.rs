#![cfg(all(feature = "test-util", debug_assertions))]

use std::{sync::Arc, time::Duration};

use chrono::Utc;
use olp_db::{
    request_metadata::ingestion::Outcome,
    store::Store,
    valkey::request_metadata::test_support::{
        RequestMetadataConsumerTestPolicy, run_request_metadata_consumer,
    },
};
use olp_engine::{
    domain::canonical::identity::{OperationKind, Surface},
    inference::request_metadata::{Event, RequestAttemptMetadata},
};
use redis::{
    AsyncCommands,
    aio::MultiplexedConnection,
    streams::{
        StreamAutoClaimOptions, StreamAutoClaimReply, StreamInfoConsumersReply,
        StreamInfoGroupsReply, StreamPendingCountReply, StreamReadOptions, StreamReadReply,
    },
};
use tokio::{sync::watch, task::JoinHandle};
use uuid::Uuid;

const GROUP: &str = "olp:persistence";
const WAIT_TIMEOUT: Duration = Duration::from_secs(10);

struct Fixture {
    provider_id: Uuid,
    api_key_id: Uuid,
    generation_id: Uuid,
}

fn valkey_url() -> String {
    std::env::var("OLP_VALKEY_URL").expect("OLP_VALKEY_URL must point to a Valkey test endpoint")
}

fn stream(label: &str) -> String {
    format!(
        "olp:test:request-metadata:{label}:{}",
        Uuid::now_v7().simple()
    )
}

async fn valkey_connection() -> MultiplexedConnection {
    redis::Client::open(valkey_url())
        .unwrap()
        .get_multiplexed_async_connection()
        .await
        .unwrap()
}

async fn fixture(store: &Store, label: &str) -> Fixture {
    let owner = store
        .setup_installation(crate::support::owner_setup(&format!(
            "request-metadata-{label}"
        )))
        .await
        .unwrap();
    let provider_id = Uuid::now_v7();
    let api_key_id = Uuid::now_v7();
    let generation_id = Uuid::now_v7();
    sqlx::query(
        "INSERT INTO providers (id, name, kind, auth_mode, etag, created_by) \
         VALUES ($1, $2, 'openai', 'api_key', $3, $4)",
    )
    .bind(provider_id)
    .bind(format!("provider-{label}"))
    .bind(Uuid::now_v7())
    .bind(owner.user_id)
    .execute(store.pool())
    .await
    .unwrap();
    sqlx::query(
        "INSERT INTO api_keys (id, lookup_id, secret_digest, name, created_by) \
         VALUES ($1, $2, $3, 'consumer test', $4)",
    )
    .bind(api_key_id)
    .bind(format!("olpv2{}", Uuid::now_v7().simple()))
    .bind([7_u8; 32].as_slice())
    .bind(owner.user_id)
    .execute(store.pool())
    .await
    .unwrap();
    sqlx::query(
        "INSERT INTO runtime_generations \
         (id, compiled_release, release_sha256, created_by) VALUES ($1, $2, $3, $4)",
    )
    .bind(generation_id)
    .bind([1_u8].as_slice())
    .bind([2_u8; 32].as_slice())
    .bind(owner.user_id)
    .execute(store.pool())
    .await
    .unwrap();
    Fixture {
        provider_id,
        api_key_id,
        generation_id,
    }
}

fn event(fixture: &Fixture) -> Event {
    let observed_at = Utc::now();
    Event {
        event_id: Uuid::now_v7(),
        request_id: Uuid::now_v7(),
        runtime_generation_id: fixture.generation_id,
        api_key_id: fixture.api_key_id,
        provider_id: Some(fixture.provider_id),
        route_slug: "default".to_owned(),
        upstream_model: Some("mock-model".to_owned()),
        operation: OperationKind::Generation,
        surface: Surface::OpenAi,
        request_started_at: observed_at - chrono::Duration::milliseconds(10),
        request_completed_at: observed_at,
        observed_at,
        status_code: Some(200),
        error_class: None,
        committed: true,
        latency_ms: 10,
        first_byte_ms: Some(3),
        input_tokens: Some(1),
        output_tokens: Some(2),
        cached_input_tokens: None,
        media_units: None,
        usage_complete: true,
        unpriced: true,
        attempts: vec![RequestAttemptMetadata {
            id: Uuid::now_v7(),
            ordinal: 1,
            provider_id: fixture.provider_id,
            upstream_model: "mock-model".to_owned(),
            started_at: observed_at - chrono::Duration::milliseconds(10),
            completed_at: observed_at,
            status_code: Some(200),
            error_class: None,
            committed: true,
            latency_ms: 10,
            first_byte_ms: Some(3),
            usage: None,
        }],
    }
}

async fn create_group(connection: &mut MultiplexedConnection, stream: &str) {
    let result: Result<String, redis::RedisError> = redis::cmd("XGROUP")
        .arg("CREATE")
        .arg(stream)
        .arg(GROUP)
        .arg("0")
        .arg("MKSTREAM")
        .query_async(connection)
        .await;
    match result {
        Ok(reply) => assert_eq!(reply, "OK"),
        Err(error) => assert_eq!(error.code(), Some("BUSYGROUP")),
    }
}

async fn add_payload(
    connection: &mut MultiplexedConnection,
    stream: &str,
    payload: &[u8],
) -> String {
    redis::cmd("XADD")
        .arg(stream)
        .arg("*")
        .arg("event")
        .arg(payload)
        .query_async(connection)
        .await
        .unwrap()
}

async fn add_event(
    connection: &mut MultiplexedConnection,
    stream: &str,
    event: &Event,
) -> (String, Vec<u8>) {
    let payload = serde_json::to_vec(event).unwrap();
    let id = add_payload(connection, stream, &payload).await;
    (id, payload)
}

async fn deliver(
    connection: &mut MultiplexedConnection,
    stream: &str,
    consumer: &str,
    count: usize,
) -> Vec<String> {
    let options = StreamReadOptions::default()
        .group(GROUP, consumer)
        .count(count);
    let reply: StreamReadReply = connection
        .xread_options(&[stream], &[">"], &options)
        .await
        .unwrap();
    reply
        .keys
        .into_iter()
        .flat_map(|stream| stream.ids)
        .map(|entry| entry.id)
        .collect()
}

fn test_policy(reclaim_idle: Duration) -> RequestMetadataConsumerTestPolicy {
    RequestMetadataConsumerTestPolicy {
        reclaim_idle,
        ..RequestMetadataConsumerTestPolicy::default()
    }
}

fn spawn_consumer(
    store: Store,
    stream: String,
    consumer: &'static str,
    shutdown: watch::Receiver<bool>,
    policy: RequestMetadataConsumerTestPolicy,
) -> JoinHandle<Result<(), olp_db::valkey::Error>> {
    tokio::spawn(async move {
        run_request_metadata_consumer(&store, &valkey_url(), &stream, consumer, shutdown, policy)
            .await
    })
}

async fn stop_consumers(
    shutdown: watch::Sender<bool>,
    consumers: Vec<JoinHandle<Result<(), olp_db::valkey::Error>>>,
) {
    shutdown.send(true).unwrap();
    for consumer in consumers {
        tokio::time::timeout(WAIT_TIMEOUT, consumer)
            .await
            .expect("consumer must stop within the blocking-read bound")
            .unwrap()
            .unwrap();
    }
}

/// Polls `check` every 10ms until it returns true, failing with `expectation`
/// after WAIT_TIMEOUT.
async fn poll_until<F>(expectation: &str, mut check: F)
where
    F: AsyncFnMut() -> bool,
{
    tokio::time::timeout(WAIT_TIMEOUT, async {
        let mut interval = tokio::time::interval(Duration::from_millis(10));
        loop {
            interval.tick().await;
            if check().await {
                return;
            }
        }
    })
    .await
    .expect(expectation);
}

async fn wait_for_consumers(
    connection: &mut MultiplexedConnection,
    stream: &str,
    expected: &[&str],
) {
    poll_until("all consumers must join the group", async || {
        let Ok(reply): Result<StreamInfoConsumersReply, _> =
            connection.xinfo_consumers(stream, GROUP).await
        else {
            return false;
        };
        expected.iter().all(|name| {
            reply
                .consumers
                .iter()
                .any(|consumer| consumer.name == *name)
        })
    })
    .await;
}

async fn wait_for_usage_facts(store: &Store, expected: i64) {
    poll_until("expected request metadata must be persisted", async || {
        let count: i64 = sqlx::query_scalar("SELECT count(*) FROM usage_facts")
            .fetch_one(store.pool())
            .await
            .unwrap();
        count == expected
    })
    .await;
}

async fn group_drained(connection: &mut MultiplexedConnection, stream: &str) -> bool {
    let groups: StreamInfoGroupsReply = connection.xinfo_groups(stream).await.unwrap();
    let group = groups
        .groups
        .into_iter()
        .find(|group| group.name == GROUP)
        .unwrap();
    group.pending == 0 && group.lag == Some(0)
}

async fn wait_for_group_drain(store: &Store, connection: &mut MultiplexedConnection, stream: &str) {
    poll_until(
        "group pending, lag, and durable health must converge to zero",
        async || {
            group_drained(connection, stream).await
                && store
                    .request_metadata_consumer_health()
                    .await
                    .unwrap()
                    .is_some_and(|health| {
                        health.pending_events == 0
                            && health.lag_events == 0
                            && health.oldest_pending_at.is_none()
                    })
        },
    )
    .await;
}

async fn wait_for_valkey_drain(connection: &mut MultiplexedConnection, stream: &str) {
    poll_until(
        "Valkey group pending and lag must converge to zero",
        async || group_drained(connection, stream).await,
    )
    .await;
}

async fn pending_owner(
    connection: &mut MultiplexedConnection,
    stream: &str,
    id: &str,
) -> Option<String> {
    let reply: StreamPendingCountReply = connection
        .xpending_count(stream, GROUP, id, id, 1)
        .await
        .unwrap();
    reply.ids.into_iter().next().map(|pending| pending.consumer)
}

async fn wait_for_pending_owner(
    connection: &mut MultiplexedConnection,
    stream: &str,
    id: &str,
    consumer: &str,
) {
    poll_until(
        "the entry must be pending for the expected consumer",
        async || pending_owner(connection, stream, id).await.as_deref() == Some(consumer),
    )
    .await;
}

async fn kill_consumer_connection(connection: &mut MultiplexedConnection, consumer: &str) {
    let clients: String = redis::cmd("CLIENT")
        .arg("LIST")
        .arg("TYPE")
        .arg("normal")
        .query_async(connection)
        .await
        .unwrap();
    let expected_name = format!("name=olp-test-request-metadata-{consumer}");
    let client_id = clients
        .lines()
        .find(|line| {
            line.split_ascii_whitespace()
                .any(|field| field == expected_name.as_str())
        })
        .and_then(|line| {
            line.split_ascii_whitespace()
                .find_map(|field| field.strip_prefix("id="))
        })
        .expect("the test consumer connection must have a client ID");
    let killed: usize = redis::cmd("CLIENT")
        .arg("KILL")
        .arg("ID")
        .arg(client_id)
        .query_async(connection)
        .await
        .unwrap();
    assert_eq!(killed, 1);
}

#[tokio::test]
#[ignore = "requires PostgreSQL 18 and Valkey in OLP_VALKEY_URL"]
async fn three_workers_distribute_new_events_and_checkpoint_group_wide_drain() {
    let db = olp_db::test_support::TestDb::create_migrated("metadata_ha").await;
    let store = db.store(12).await;
    let fixture = fixture(&store, "three-workers").await;
    let stream = stream("three-workers");
    let mut connection = valkey_connection().await;
    let mut persistence_lock = store.pool().begin().await.unwrap();
    sqlx::query("LOCK TABLE request_metadata_event_receipts IN ACCESS EXCLUSIVE MODE")
        .execute(&mut *persistence_lock)
        .await
        .unwrap();
    let (shutdown, receiver) = watch::channel(false);
    let policy = RequestMetadataConsumerTestPolicy {
        batch_size: 1,
        ..test_policy(Duration::from_secs(60))
    };
    let consumers = ["worker-a", "worker-b", "worker-c"]
        .into_iter()
        .map(|consumer| {
            spawn_consumer(
                store.clone(),
                stream.clone(),
                consumer,
                receiver.clone(),
                policy,
            )
        })
        .collect::<Vec<_>>();
    wait_for_consumers(
        &mut connection,
        &stream,
        &["worker-a", "worker-b", "worker-c"],
    )
    .await;

    for _ in 0..3 {
        add_event(&mut connection, &stream, &event(&fixture)).await;
    }
    poll_until(
        "one blocked delivery must be distributed to each live worker",
        async || {
            let pending: StreamPendingCountReply = connection
                .xpending_count(&stream, GROUP, "-", "+", 10)
                .await
                .unwrap();
            let owners = pending
                .ids
                .into_iter()
                .map(|pending| pending.consumer)
                .collect::<std::collections::HashSet<_>>();
            owners
                == ["worker-a", "worker-b", "worker-c"]
                    .map(str::to_owned)
                    .into()
        },
    )
    .await;
    for _ in 3..24 {
        add_event(&mut connection, &stream, &event(&fixture)).await;
    }
    persistence_lock.rollback().await.unwrap();
    wait_for_usage_facts(&store, 24).await;
    wait_for_group_drain(&store, &mut connection, &stream).await;

    let requests: i64 = sqlx::query_scalar("SELECT count(*) FROM requests")
        .fetch_one(store.pool())
        .await
        .unwrap();
    let receipts: i64 = sqlx::query_scalar("SELECT count(*) FROM request_metadata_event_receipts")
        .fetch_one(store.pool())
        .await
        .unwrap();
    assert_eq!(requests, 24);
    assert_eq!(receipts, 24);
    stop_consumers(shutdown, consumers).await;
}

#[tokio::test]
#[ignore = "requires PostgreSQL 18 and Valkey in OLP_VALKEY_URL"]
async fn reconnect_resumes_own_pending_before_stale_idle_and_survivor_recovers_dead_owner() {
    let db = olp_db::test_support::TestDb::create_migrated("metadata_reconnect").await;
    let store = db.store(8).await;
    let fixture = fixture(&store, "reconnect").await;
    let stream = stream("reconnect");
    let mut connection = valkey_connection().await;
    create_group(&mut connection, &stream).await;

    let first = event(&fixture);
    let (first_id, _) = add_event(&mut connection, &stream, &first).await;
    assert_eq!(
        deliver(&mut connection, &stream, "same-process", 1).await,
        [first_id]
    );
    drop(connection); // The delivered entry remains in this consumer's PEL.

    let (shutdown, receiver) = watch::channel(false);
    let consumer = spawn_consumer(
        store.clone(),
        stream.clone(),
        "same-process",
        receiver,
        test_policy(Duration::from_secs(60)),
    );
    wait_for_usage_facts(&store, 1).await;
    stop_consumers(shutdown, vec![consumer]).await;

    let mut connection = valkey_connection().await;
    let second = event(&fixture);
    let (second_id, _) = add_event(&mut connection, &stream, &second).await;
    assert_eq!(
        deliver(&mut connection, &stream, "dead-process", 1).await,
        [second_id]
    );
    let (shutdown, receiver) = watch::channel(false);
    let survivor = spawn_consumer(
        store.clone(),
        stream.clone(),
        "survivor",
        receiver,
        test_policy(Duration::ZERO),
    );
    wait_for_usage_facts(&store, 2).await;
    wait_for_group_drain(&store, &mut connection, &stream).await;
    stop_consumers(shutdown, vec![survivor]).await;

    // Hold the first receipt insert so the real consumer is known to be
    // between stream delivery and PostgreSQL persistence, then hard-abort it.
    // The survivor must claim the abandoned PEL entry and commit it once.
    let mut persistence_lock = store.pool().begin().await.unwrap();
    sqlx::query("LOCK TABLE request_metadata_event_receipts IN ACCESS EXCLUSIVE MODE")
        .execute(&mut *persistence_lock)
        .await
        .unwrap();
    let (doomed_shutdown, doomed_receiver) = watch::channel(false);
    let doomed = spawn_consumer(
        store.clone(),
        stream.clone(),
        "hard-killed-process",
        doomed_receiver,
        test_policy(Duration::from_secs(60)),
    );
    wait_for_consumers(&mut connection, &stream, &["hard-killed-process"]).await;
    let third = event(&fixture);
    let (third_id, _) = add_event(&mut connection, &stream, &third).await;
    wait_for_pending_owner(&mut connection, &stream, &third_id, "hard-killed-process").await;
    doomed.abort();
    assert!(
        tokio::time::timeout(WAIT_TIMEOUT, doomed)
            .await
            .unwrap()
            .unwrap_err()
            .is_cancelled()
    );
    drop(doomed_shutdown);
    persistence_lock.rollback().await.unwrap();

    let (shutdown, receiver) = watch::channel(false);
    let survivor = spawn_consumer(
        store.clone(),
        stream.clone(),
        "hard-kill-survivor",
        receiver,
        test_policy(Duration::ZERO),
    );
    wait_for_usage_facts(&store, 3).await;
    wait_for_group_drain(&store, &mut connection, &stream).await;
    stop_consumers(shutdown, vec![survivor]).await;

    // Kill the worker's Valkey socket while its PostgreSQL insert is blocked.
    // It may reconnect transparently or restart with this stable identity, but
    // it must not acknowledge before the durable commit.
    let mut persistence_lock = store.pool().begin().await.unwrap();
    sqlx::query("LOCK TABLE request_metadata_event_receipts IN ACCESS EXCLUSIVE MODE")
        .execute(&mut *persistence_lock)
        .await
        .unwrap();
    let (shutdown, receiver) = watch::channel(false);
    let mut connection_loss_worker = spawn_consumer(
        store.clone(),
        stream.clone(),
        "connection-loss-process",
        receiver,
        test_policy(Duration::from_secs(60)),
    );
    wait_for_consumers(&mut connection, &stream, &["connection-loss-process"]).await;
    let fourth = event(&fixture);
    let (fourth_id, _) = add_event(&mut connection, &stream, &fourth).await;
    wait_for_pending_owner(
        &mut connection,
        &stream,
        &fourth_id,
        "connection-loss-process",
    )
    .await;
    kill_consumer_connection(&mut connection, "connection-loss-process").await;
    persistence_lock.rollback().await.unwrap();
    wait_for_usage_facts(&store, 4).await;
    let consumers = tokio::select! {
        biased;
        result = &mut connection_loss_worker => {
            assert!(result.unwrap().is_err(), "a consumer may only return successfully on shutdown");
            vec![spawn_consumer(
                store.clone(),
                stream.clone(),
                "connection-loss-process",
                shutdown.subscribe(),
                test_policy(Duration::from_secs(60)),
            )]
        }
        () = wait_for_valkey_drain(&mut connection, &stream) => {
            vec![connection_loss_worker]
        }
    };
    wait_for_group_drain(&store, &mut connection, &stream).await;
    stop_consumers(shutdown, consumers).await;
}

#[tokio::test]
#[ignore = "requires PostgreSQL 18 and Valkey in OLP_VALKEY_URL"]
async fn commit_before_ack_replays_as_duplicate_under_two_concurrent_claimants() {
    let db = olp_db::test_support::TestDb::create_migrated("metadata_commit").await;
    let store = db.store(10).await;
    let fixture = fixture(&store, "commit-replay").await;
    let stream = stream("commit-replay");
    let mut connection = valkey_connection().await;
    create_group(&mut connection, &stream).await;
    let event = event(&fixture);
    let (id, payload) = add_event(&mut connection, &stream, &event).await;
    assert_eq!(
        deliver(&mut connection, &stream, "committed-owner", 1).await,
        [id]
    );

    assert_eq!(
        store
            .persist_request_metadata_stream_event(&event, &payload)
            .await
            .unwrap(),
        Outcome::Persisted
    );

    let (shutdown, receiver) = watch::channel(false);
    let barrier = Arc::new(tokio::sync::Barrier::new(3));
    let mut consumers = Vec::new();
    for consumer in ["claimant-a", "claimant-b"] {
        let store = store.clone();
        let stream = stream.clone();
        let receiver = receiver.clone();
        let barrier = Arc::clone(&barrier);
        consumers.push(tokio::spawn(async move {
            barrier.wait().await;
            run_request_metadata_consumer(
                &store,
                &valkey_url(),
                &stream,
                consumer,
                receiver,
                test_policy(Duration::ZERO),
            )
            .await
        }));
    }
    barrier.wait().await;
    wait_for_group_drain(&store, &mut connection, &stream).await;

    assert_eq!(
        store
            .persist_request_metadata_stream_event(&event, &payload)
            .await
            .unwrap(),
        Outcome::Duplicate
    );
    let counts: (i64, i64, i64) = sqlx::query_as(
        "SELECT (SELECT count(*) FROM requests), \
                (SELECT count(*) FROM usage_facts), \
                (SELECT count(*) FROM request_metadata_event_receipts)",
    )
    .fetch_one(store.pool())
    .await
    .unwrap();
    assert_eq!(counts, (1, 1, 1));
    stop_consumers(shutdown, consumers).await;
    let counters = store.worker_recovery_counters().await.unwrap();
    // With a zero-idle test policy both contenders may transfer the same PEL
    // entry before the winner acknowledges it. The counter records recovery
    // activity, not unique event identity.
    assert!(counters.request_metadata_reclaimed >= 1);
    assert!(counters.request_metadata_recovered >= 1);
    assert_eq!(
        counters.request_metadata_duplicates,
        counters.request_metadata_recovered
    );
    assert_eq!(
        counters.request_metadata_processed,
        counters.request_metadata_recovered
    );
}

#[tokio::test]
#[ignore = "requires PostgreSQL 18 and Valkey in OLP_VALKEY_URL"]
async fn slow_active_entry_is_not_stolen_below_idle_threshold() {
    let db = olp_db::test_support::TestDb::create_migrated("metadata_idle").await;
    let store = db.store(8).await;
    let fixture = fixture(&store, "idle-threshold").await;
    let stream = stream("idle-threshold");
    let mut connection = valkey_connection().await;
    create_group(&mut connection, &stream).await;
    let mut persistence_lock = store.pool().begin().await.unwrap();
    sqlx::query("LOCK TABLE request_metadata_event_receipts IN ACCESS EXCLUSIVE MODE")
        .execute(&mut *persistence_lock)
        .await
        .unwrap();
    let (shutdown, receiver) = watch::channel(false);
    let slow = spawn_consumer(
        store.clone(),
        stream.clone(),
        "slow-active",
        receiver,
        test_policy(Duration::from_secs(60)),
    );
    wait_for_consumers(&mut connection, &stream, &["slow-active"]).await;
    let event = event(&fixture);
    let (id, _) = add_event(&mut connection, &stream, &event).await;
    wait_for_pending_owner(&mut connection, &stream, &id, "slow-active").await;

    let claim: StreamAutoClaimReply = connection
        .xautoclaim_options(
            &stream,
            GROUP,
            "premature-claimant",
            60_000,
            "0-0",
            StreamAutoClaimOptions::default().count(10),
        )
        .await
        .unwrap();
    assert!(claim.claimed.is_empty());
    assert_eq!(
        pending_owner(&mut connection, &stream, &id)
            .await
            .as_deref(),
        Some("slow-active")
    );
    let fact_count: i64 = sqlx::query_scalar("SELECT count(*) FROM usage_facts")
        .fetch_one(store.pool())
        .await
        .unwrap();
    assert_eq!(fact_count, 0);
    persistence_lock.rollback().await.unwrap();
    wait_for_usage_facts(&store, 1).await;
    wait_for_group_drain(&store, &mut connection, &stream).await;
    stop_consumers(shutdown, vec![slow]).await;
}

#[tokio::test]
#[ignore = "requires PostgreSQL 18 and Valkey in OLP_VALKEY_URL"]
async fn concurrent_recovery_records_malformed_and_invalid_entries_once_then_drains() {
    let db = olp_db::test_support::TestDb::create_migrated("metadata_invalid").await;
    let store = db.store(10).await;
    let fixture = fixture(&store, "invalid-recovery").await;
    let stream = stream("invalid-recovery");
    let mut connection = valkey_connection().await;
    create_group(&mut connection, &stream).await;

    add_payload(&mut connection, &stream, b"not-json").await;
    let mut invalid = event(&fixture);
    invalid.route_slug.clear();
    add_event(&mut connection, &stream, &invalid).await;
    assert_eq!(
        deliver(&mut connection, &stream, "dead-invalid-owner", 2)
            .await
            .len(),
        2
    );

    let (shutdown, receiver) = watch::channel(false);
    let barrier = Arc::new(tokio::sync::Barrier::new(3));
    let mut consumers = Vec::new();
    for consumer in ["invalid-claimant-a", "invalid-claimant-b"] {
        let store = store.clone();
        let stream = stream.clone();
        let receiver = receiver.clone();
        let barrier = Arc::clone(&barrier);
        consumers.push(tokio::spawn(async move {
            barrier.wait().await;
            run_request_metadata_consumer(
                &store,
                &valkey_url(),
                &stream,
                consumer,
                receiver,
                test_policy(Duration::ZERO),
            )
            .await
        }));
    }
    barrier.wait().await;
    wait_for_group_drain(&store, &mut connection, &stream).await;

    let gaps: Vec<(String, i64, i64)> = sqlx::query_as(
        "SELECT reason, count(*), sum(event_count)::bigint \
         FROM request_metadata_ingestion_gaps \
         WHERE reason IN ('malformed_stream_event', 'invalid_request_metadata_event') \
         GROUP BY reason ORDER BY reason",
    )
    .fetch_all(store.pool())
    .await
    .unwrap();
    assert_eq!(
        gaps,
        [
            ("invalid_request_metadata_event".to_owned(), 1, 1),
            ("malformed_stream_event".to_owned(), 1, 1),
        ]
    );
    stop_consumers(shutdown, consumers).await;
}
