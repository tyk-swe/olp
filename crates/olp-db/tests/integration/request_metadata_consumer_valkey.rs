#![cfg(all(feature = "test-util", debug_assertions))]

use std::{sync::Arc, time::Duration};

use chrono::Utc;
use olp_db::{
    identity::InstallationSetupInput,
    limits::DistributedLimiter,
    request_metadata::ingestion::Outcome,
    security::password::hash,
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

fn limits_namespace(stream: &str) -> String {
    format!("{stream}:limits")
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
        .setup_installation(InstallationSetupInput {
            installation_name: format!("Request metadata {label}"),
            email: format!("owner-{label}@example.test"),
            display_name: "Owner".to_owned(),
            password_hash: hash("correct horse battery staple").unwrap(),
        })
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
        run_request_metadata_consumer(
            &store,
            &valkey_url(),
            &stream,
            consumer,
            &limits_namespace(&stream),
            shutdown,
            policy,
        )
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

async fn wait_for_consumers(
    connection: &mut MultiplexedConnection,
    stream: &str,
    expected: &[&str],
) {
    tokio::time::timeout(WAIT_TIMEOUT, async {
        let mut interval = tokio::time::interval(Duration::from_millis(10));
        loop {
            interval.tick().await;
            let Ok(reply): Result<StreamInfoConsumersReply, _> =
                connection.xinfo_consumers(stream, GROUP).await
            else {
                continue;
            };
            if expected.iter().all(|name| {
                reply
                    .consumers
                    .iter()
                    .any(|consumer| consumer.name == *name)
            }) {
                return;
            }
        }
    })
    .await
    .expect("all consumers must join the group");
}

async fn wait_for_usage_facts(store: &Store, expected: i64) {
    tokio::time::timeout(WAIT_TIMEOUT, async {
        let mut interval = tokio::time::interval(Duration::from_millis(10));
        loop {
            interval.tick().await;
            let count: i64 = sqlx::query_scalar("SELECT count(*) FROM usage_facts")
                .fetch_one(store.pool())
                .await
                .unwrap();
            if count == expected {
                return;
            }
        }
    })
    .await
    .expect("expected request metadata must be persisted");
}

async fn wait_for_group_drain(store: &Store, connection: &mut MultiplexedConnection, stream: &str) {
    tokio::time::timeout(WAIT_TIMEOUT, async {
        let mut interval = tokio::time::interval(Duration::from_millis(10));
        loop {
            interval.tick().await;
            let groups: StreamInfoGroupsReply = connection.xinfo_groups(stream).await.unwrap();
            let group = groups
                .groups
                .into_iter()
                .find(|group| group.name == GROUP)
                .unwrap();
            let health = store.request_metadata_consumer_health().await.unwrap();
            if group.pending == 0
                && group.lag == Some(0)
                && health.is_some_and(|health| {
                    health.pending_events == 0
                        && health.lag_events == 0
                        && health.oldest_pending_at.is_none()
                })
            {
                return;
            }
        }
    })
    .await
    .expect("group pending, lag, and durable health must converge to zero");
}

async fn wait_for_valkey_drain(connection: &mut MultiplexedConnection, stream: &str) {
    tokio::time::timeout(WAIT_TIMEOUT, async {
        let mut interval = tokio::time::interval(Duration::from_millis(10));
        loop {
            interval.tick().await;
            let groups: StreamInfoGroupsReply = connection.xinfo_groups(stream).await.unwrap();
            let group = groups
                .groups
                .into_iter()
                .find(|group| group.name == GROUP)
                .unwrap();
            if group.pending == 0 && group.lag == Some(0) {
                return;
            }
        }
    })
    .await
    .expect("Valkey group pending and lag must converge to zero");
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

mod distribution;
mod gaps;
mod recovery;

async fn wait_for_gap(store: &Store, reason: &str) {
    tokio::time::timeout(WAIT_TIMEOUT, async {
        let mut interval = tokio::time::interval(Duration::from_millis(10));
        loop {
            interval.tick().await;
            let count: i64 = sqlx::query_scalar(
                "SELECT count(*)::bigint FROM request_metadata_ingestion_gaps WHERE reason = $1",
            )
            .bind(reason)
            .fetch_one(store.pool())
            .await
            .unwrap();
            if count > 0 {
                return;
            }
        }
    })
    .await
    .unwrap_or_else(|_| panic!("a {reason} gap must be recorded"));
}
