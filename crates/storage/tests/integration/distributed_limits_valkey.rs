use std::{collections::HashMap, time::Duration};

use futures::future::join_all;
use olp_storage::{
    limits::DistributedLimiter, limits::LimitDimension, limits::LimitError, limits::LimitLease,
    limits::LimitRequest,
};
use redis::{AsyncCommands, aio::MultiplexedConnection};
use uuid::Uuid;

const MAX_LUA_INTEGER: i64 = (1_i64 << 53) - 1;
const RESERVE_SCRIPT: &str = include_str!("../../scripts/reserve_limits.lua");

fn valkey_url() -> String {
    std::env::var("OLP_VALKEY_URL").expect("OLP_VALKEY_URL must point to a Valkey test endpoint")
}

fn namespace(label: &str) -> String {
    format!("olp:test:limits:{label}:{}", Uuid::now_v7().simple())
}

fn keys(namespace: &str, lookup_id: &str) -> (String, String) {
    (
        format!("{namespace}:{{{lookup_id}}}:rate"),
        format!("{namespace}:{{{lookup_id}}}:concurrency:v2"),
    )
}

fn request(lookup_id: &str) -> LimitRequest<'_> {
    LimitRequest {
        lookup_id,
        requests_per_minute: Some(10),
        tokens_per_minute: Some(1_000),
        max_concurrency: Some(10),
        requested_tokens: 5,
        lease_ttl: Duration::from_secs(5),
    }
}

async fn connection() -> MultiplexedConnection {
    redis::Client::open(valkey_url())
        .unwrap()
        .get_multiplexed_async_connection()
        .await
        .unwrap()
}

async fn server_time_ms(connection: &mut MultiplexedConnection) -> i64 {
    let (seconds, microseconds): (i64, i64) =
        redis::cmd("TIME").query_async(connection).await.unwrap();
    seconds * 1_000 + microseconds / 1_000
}

async fn settle_in_minute(connection: &mut MultiplexedConnection) -> i64 {
    let now_ms = server_time_ms(connection).await;
    let remaining_ms = 60_000 - now_ms.rem_euclid(60_000);
    if remaining_ms < 2_000 {
        tokio::time::sleep(Duration::from_millis((remaining_ms + 20) as u64)).await;
        return server_time_ms(connection).await;
    }
    now_ms
}

async fn rate_state(connection: &mut MultiplexedConnection, key: &str) -> HashMap<String, i64> {
    connection.hgetall(key).await.unwrap()
}

fn exceeded(error: LimitError, expected: LimitDimension) -> Duration {
    match error {
        LimitError::Exceeded {
            dimension,
            retry_after,
        } => {
            assert_eq!(dimension, expected);
            retry_after
        }
        other => panic!("expected {expected:?} rejection, got {other:?}"),
    }
}

async fn release_twice(limiter: &DistributedLimiter, lease: &LimitLease) {
    limiter.release(lease).await.unwrap();
    limiter.release(lease).await.unwrap();
}

#[tokio::test]
#[ignore = "requires Valkey in OLP_VALKEY_URL"]
async fn server_time_unifies_callers() {
    let namespace = namespace("server_time");
    let lookup_id = "lookup_01";
    let other_lookup_id = "lookup_02";
    let limiter_a = DistributedLimiter::connect(&valkey_url(), &namespace)
        .await
        .unwrap();
    let limiter_b = DistributedLimiter::connect(&valkey_url(), &namespace)
        .await
        .unwrap();
    let mut connection = connection().await;
    let before_ms = settle_in_minute(&mut connection).await;
    let first = limiter_a
        .reserve(LimitRequest {
            requests_per_minute: Some(2),
            tokens_per_minute: None,
            max_concurrency: Some(2),
            ..request(lookup_id)
        })
        .await
        .unwrap();
    let second = limiter_b
        .reserve(LimitRequest {
            requests_per_minute: Some(2),
            tokens_per_minute: None,
            max_concurrency: Some(2),
            ..request(lookup_id)
        })
        .await
        .unwrap();
    exceeded(
        limiter_a
            .reserve(LimitRequest {
                requests_per_minute: Some(2),
                tokens_per_minute: None,
                max_concurrency: Some(2),
                ..request(lookup_id)
            })
            .await
            .unwrap_err(),
        LimitDimension::Requests,
    );
    let after_ms = server_time_ms(&mut connection).await;

    let (rate_key, _) = keys(&namespace, lookup_id);
    let state = rate_state(&mut connection, &rate_key).await;
    let window = state["window"];
    assert!((before_ms / 60_000..=after_ms / 60_000).contains(&window));
    assert_eq!(state["rpm"], 2);
    assert_eq!(state["tpm"], 0);

    limiter_a
        .reserve(LimitRequest {
            requests_per_minute: Some(1),
            tokens_per_minute: None,
            max_concurrency: None,
            ..request(other_lookup_id)
        })
        .await
        .unwrap();
    let (other_rate_key, _) = keys(&namespace, other_lookup_id);
    assert_eq!(rate_state(&mut connection, &other_rate_key).await["rpm"], 1);

    release_twice(&limiter_a, &first).await;
    release_twice(&limiter_b, &second).await;
}

#[tokio::test]
#[ignore = "requires Valkey in OLP_VALKEY_URL"]
async fn minute_rollover_resets_once_and_retry_matches_server_window() {
    let namespace = namespace("rollover");
    let lookup_id = "lookup_01";
    let limiter = DistributedLimiter::connect(&valkey_url(), &namespace)
        .await
        .unwrap();
    let mut connection = connection().await;
    let now_ms = settle_in_minute(&mut connection).await;
    let current_window = now_ms / 60_000;
    let (rate_key, _) = keys(&namespace, lookup_id);
    let _: () = redis::pipe()
        .hset(&rate_key, "window", current_window - 1)
        .hset(&rate_key, "rpm", 9)
        .hset(&rate_key, "tpm", 900)
        .hset(&rate_key, "reconciled:stale-lease", 1)
        .pexpire(&rate_key, 120_000)
        .query_async(&mut connection)
        .await
        .unwrap();

    let mut previous_ttl_ms = i64::MAX;
    for expected_rpm in 1..=2 {
        limiter
            .reserve(LimitRequest {
                requests_per_minute: Some(2),
                tokens_per_minute: Some(20),
                max_concurrency: None,
                requested_tokens: 7,
                ..request(lookup_id)
            })
            .await
            .unwrap();
        let state = rate_state(&mut connection, &rate_key).await;
        assert_eq!(connection.hlen::<_, i64>(&rate_key).await.unwrap(), 3);
        assert_eq!(state["window"], current_window);
        assert_eq!(state["rpm"], expected_rpm);
        assert_eq!(state["tpm"], expected_rpm * 7);
        let ttl_ms: i64 = connection.pttl(&rate_key).await.unwrap();
        assert!(ttl_ms > 0);
        assert!(ttl_ms <= previous_ttl_ms);
        previous_ttl_ms = ttl_ms;
    }

    let before_rejection_ms = server_time_ms(&mut connection).await;
    let retry = exceeded(
        limiter
            .reserve(LimitRequest {
                requests_per_minute: Some(2),
                tokens_per_minute: Some(20),
                max_concurrency: None,
                requested_tokens: 1,
                ..request(lookup_id)
            })
            .await
            .unwrap_err(),
        LimitDimension::Requests,
    );
    let after_rejection_ms = server_time_ms(&mut connection).await;
    assert!(!retry.is_zero());
    assert!(retry <= Duration::from_secs(60));
    if before_rejection_ms / 60_000 == after_rejection_ms / 60_000 {
        let retry_ms = i64::try_from(retry.as_millis()).unwrap();
        let remaining_before = 60_000 - before_rejection_ms.rem_euclid(60_000);
        let remaining_after = 60_000 - after_rejection_ms.rem_euclid(60_000);
        assert!((remaining_after..=remaining_before).contains(&retry_ms));
    }
}

#[tokio::test]
#[ignore = "requires Valkey in OLP_VALKEY_URL and waits for a UTC-minute boundary"]
async fn request_near_minute_end_gets_positive_server_derived_retry() {
    let namespace = namespace("near_boundary");
    let lookup_id = "lookup_01";
    let limiter = DistributedLimiter::connect(&valkey_url(), &namespace)
        .await
        .unwrap();
    let mut connection = connection().await;

    loop {
        let now_ms = server_time_ms(&mut connection).await;
        let remaining_ms = 60_000 - now_ms.rem_euclid(60_000);
        if (800..=1_500).contains(&remaining_ms) {
            break;
        }
        let wait_ms = if remaining_ms < 800 {
            remaining_ms + 20
        } else {
            (remaining_ms - 1_500).min(500)
        };
        tokio::time::sleep(Duration::from_millis(wait_ms as u64)).await;
    }

    limiter
        .reserve(LimitRequest {
            requests_per_minute: Some(1),
            tokens_per_minute: None,
            max_concurrency: None,
            ..request(lookup_id)
        })
        .await
        .unwrap();
    let before_rejection_ms = server_time_ms(&mut connection).await;
    let retry = exceeded(
        limiter
            .reserve(LimitRequest {
                requests_per_minute: Some(1),
                tokens_per_minute: None,
                max_concurrency: None,
                ..request(lookup_id)
            })
            .await
            .unwrap_err(),
        LimitDimension::Requests,
    );
    let after_rejection_ms = server_time_ms(&mut connection).await;
    let retry_ms = i64::try_from(retry.as_millis()).unwrap();
    assert!((1..=1_500).contains(&retry_ms));
    assert_eq!(before_rejection_ms / 60_000, after_rejection_ms / 60_000);
    assert!(
        (60_000 - after_rejection_ms.rem_euclid(60_000)
            ..=60_000 - before_rejection_ms.rem_euclid(60_000))
            .contains(&retry_ms)
    );
}

#[tokio::test]
#[ignore = "requires Valkey in OLP_VALKEY_URL"]
async fn rpm_rejection_consumes_neither_tokens_nor_concurrency() {
    let namespace = namespace("rpm_atomic");
    let lookup_id = "lookup_01";
    let limiter = DistributedLimiter::connect(&valkey_url(), &namespace)
        .await
        .unwrap();
    let mut connection = connection().await;
    settle_in_minute(&mut connection).await;
    let (rate_key, concurrency_key) = keys(&namespace, lookup_id);

    let lease = limiter
        .reserve(LimitRequest {
            requests_per_minute: Some(1),
            tokens_per_minute: Some(100),
            max_concurrency: Some(1),
            ..request(lookup_id)
        })
        .await
        .unwrap();
    release_twice(&limiter, &lease).await;
    exceeded(
        limiter
            .reserve(LimitRequest {
                requests_per_minute: Some(1),
                tokens_per_minute: Some(100),
                max_concurrency: Some(1),
                ..request(lookup_id)
            })
            .await
            .unwrap_err(),
        LimitDimension::Requests,
    );

    let state = rate_state(&mut connection, &rate_key).await;
    assert_eq!(state["rpm"], 1);
    assert_eq!(state["tpm"], 5);
    assert_eq!(
        connection.zcard::<_, i64>(&concurrency_key).await.unwrap(),
        0
    );
}

#[tokio::test]
#[ignore = "requires Valkey in OLP_VALKEY_URL"]
async fn tpm_rejection_consumes_neither_requests_nor_concurrency() {
    let namespace = namespace("tpm_atomic");
    let lookup_id = "lookup_01";
    let limiter = DistributedLimiter::connect(&valkey_url(), &namespace)
        .await
        .unwrap();
    let mut connection = connection().await;
    settle_in_minute(&mut connection).await;
    let (rate_key, concurrency_key) = keys(&namespace, lookup_id);

    let lease = limiter
        .reserve(LimitRequest {
            requests_per_minute: Some(3),
            tokens_per_minute: Some(5),
            max_concurrency: Some(1),
            ..request(lookup_id)
        })
        .await
        .unwrap();
    release_twice(&limiter, &lease).await;
    exceeded(
        limiter
            .reserve(LimitRequest {
                requests_per_minute: Some(3),
                tokens_per_minute: Some(5),
                max_concurrency: Some(1),
                requested_tokens: 1,
                ..request(lookup_id)
            })
            .await
            .unwrap_err(),
        LimitDimension::Tokens,
    );

    let state = rate_state(&mut connection, &rate_key).await;
    assert_eq!(state["rpm"], 1);
    assert_eq!(state["tpm"], 5);
    assert_eq!(
        connection.zcard::<_, i64>(&concurrency_key).await.unwrap(),
        0
    );
}

#[tokio::test]
#[ignore = "requires Valkey in OLP_VALKEY_URL"]
async fn token_reconciliation_refunds_only_unused_reservation() {
    let namespace = namespace("token_refund");
    let lookup_id = "lookup_01";
    let limiter = DistributedLimiter::connect(&valkey_url(), &namespace)
        .await
        .unwrap();
    let mut connection = connection().await;
    settle_in_minute(&mut connection).await;
    let (rate_key, _) = keys(&namespace, lookup_id);

    let lease = limiter
        .reserve(LimitRequest {
            requests_per_minute: Some(10),
            tokens_per_minute: Some(100),
            max_concurrency: None,
            requested_tokens: 8,
            ..request(lookup_id)
        })
        .await
        .unwrap();
    limiter.reconcile(&lease, 3).await.unwrap();
    limiter.reconcile(&lease, 3).await.unwrap();
    let state = rate_state(&mut connection, &rate_key).await;
    assert_eq!(state["rpm"], 1);
    assert_eq!(state["tpm"], 3);

    let lease = limiter
        .reserve(LimitRequest {
            requests_per_minute: Some(10),
            tokens_per_minute: Some(100),
            max_concurrency: None,
            requested_tokens: 4,
            ..request(lookup_id)
        })
        .await
        .unwrap();
    limiter.reconcile(&lease, 7).await.unwrap();
    let state = rate_state(&mut connection, &rate_key).await;
    assert_eq!(state["rpm"], 2);
    assert_eq!(state["tpm"], 7);
}

#[tokio::test]
#[ignore = "requires Valkey in OLP_VALKEY_URL"]
async fn token_reconciliation_does_not_touch_a_new_window() {
    let namespace = namespace("token_refund_window");
    let lookup_id = "lookup_01";
    let limiter = DistributedLimiter::connect(&valkey_url(), &namespace)
        .await
        .unwrap();
    let mut connection = connection().await;
    settle_in_minute(&mut connection).await;
    let (rate_key, _) = keys(&namespace, lookup_id);

    let lease = limiter
        .reserve(LimitRequest {
            requests_per_minute: Some(10),
            tokens_per_minute: Some(100),
            max_concurrency: None,
            requested_tokens: 8,
            ..request(lookup_id)
        })
        .await
        .unwrap();
    let reservation_window = rate_state(&mut connection, &rate_key).await["window"];
    let _: () = redis::pipe()
        .hset(&rate_key, "window", reservation_window + 1)
        .hset(&rate_key, "rpm", 1)
        .hset(&rate_key, "tpm", 11)
        .query_async(&mut connection)
        .await
        .unwrap();

    limiter.reconcile(&lease, 0).await.unwrap();
    let state = rate_state(&mut connection, &rate_key).await;
    assert_eq!(state["window"], reservation_window + 1);
    assert_eq!(state["rpm"], 1);
    assert_eq!(state["tpm"], 11);
}

#[tokio::test]
#[ignore = "requires Valkey in OLP_VALKEY_URL"]
async fn concurrency_rejection_consumes_neither_requests_nor_tokens() {
    let namespace = namespace("concurrency_atomic");
    let lookup_id = "lookup_01";
    let limiter = DistributedLimiter::connect(&valkey_url(), &namespace)
        .await
        .unwrap();
    let mut connection = connection().await;
    settle_in_minute(&mut connection).await;
    let (rate_key, _) = keys(&namespace, lookup_id);

    let lease = limiter
        .reserve(LimitRequest {
            requests_per_minute: Some(3),
            tokens_per_minute: Some(100),
            max_concurrency: Some(1),
            ..request(lookup_id)
        })
        .await
        .unwrap();
    exceeded(
        limiter
            .reserve(LimitRequest {
                requests_per_minute: Some(3),
                tokens_per_minute: Some(100),
                max_concurrency: Some(1),
                ..request(lookup_id)
            })
            .await
            .unwrap_err(),
        LimitDimension::Concurrency,
    );

    let state = rate_state(&mut connection, &rate_key).await;
    assert_eq!(state["rpm"], 1);
    assert_eq!(state["tpm"], 5);
    release_twice(&limiter, &lease).await;
}

#[tokio::test]
#[ignore = "requires Valkey in OLP_VALKEY_URL"]
async fn concurrency_expiry_cleanup_scores_and_release_use_server_state() {
    let namespace = namespace("lease_time");
    let lookup_id = "lookup_01";
    let limiter = DistributedLimiter::connect(&valkey_url(), &namespace)
        .await
        .unwrap();
    let mut connection = connection().await;
    let (_, concurrency_key) = keys(&namespace, lookup_id);
    let before_ms = server_time_ms(&mut connection).await;
    connection
        .zadd::<_, _, _, ()>(&concurrency_key, "expired-fixture", before_ms - 1)
        .await
        .unwrap();

    let lease = limiter
        .reserve(LimitRequest {
            requests_per_minute: None,
            tokens_per_minute: None,
            max_concurrency: Some(1),
            ..request(lookup_id)
        })
        .await
        .unwrap();
    let after_ms = server_time_ms(&mut connection).await;
    let entries: Vec<(String, i64)> = redis::cmd("ZRANGE")
        .arg(&concurrency_key)
        .arg(0)
        .arg(-1)
        .arg("WITHSCORES")
        .query_async(&mut connection)
        .await
        .unwrap();
    assert_eq!(entries.len(), 1);
    assert_ne!(entries[0].0, "expired-fixture");
    assert!((before_ms + 5_000..=after_ms + 5_000).contains(&entries[0].1));
    let ttl_ms: i64 = connection.pttl(&concurrency_key).await.unwrap();
    assert!((1..=5_000).contains(&ttl_ms));

    release_twice(&limiter, &lease).await;
    assert_eq!(
        connection.zcard::<_, i64>(&concurrency_key).await.unwrap(),
        0
    );
}

#[tokio::test]
#[ignore = "requires Valkey in OLP_VALKEY_URL"]
async fn unlimited_dimensions_create_no_state() {
    let namespace = namespace("unlimited");
    let lookup_id = "lookup_01";
    let limiter = DistributedLimiter::connect(&valkey_url(), &namespace)
        .await
        .unwrap();
    let mut connection = connection().await;
    let (rate_key, concurrency_key) = keys(&namespace, lookup_id);

    let lease = limiter
        .reserve(LimitRequest {
            requests_per_minute: None,
            tokens_per_minute: None,
            max_concurrency: None,
            requested_tokens: 0,
            ..request(lookup_id)
        })
        .await
        .unwrap();
    limiter.release(&lease).await.unwrap();
    assert!(!connection.exists::<_, bool>(&rate_key).await.unwrap());
    assert!(
        !connection
            .exists::<_, bool>(&concurrency_key)
            .await
            .unwrap()
    );
}

#[tokio::test]
#[ignore = "requires Valkey in OLP_VALKEY_URL"]
async fn malformed_rate_state_fails_closed_before_capacity_mutation() {
    let namespace = namespace("malformed");
    let lookup_id = "lookup_01";
    let limiter = DistributedLimiter::connect(&valkey_url(), &namespace)
        .await
        .unwrap();
    let mut connection = connection().await;
    let (rate_key, concurrency_key) = keys(&namespace, lookup_id);
    connection
        .hset::<_, _, _, ()>(&rate_key, "window", "not-an-integer")
        .await
        .unwrap();

    assert!(matches!(
        limiter.reserve(request(lookup_id)).await,
        Err(LimitError::MalformedState)
    ));
    assert_eq!(connection.hlen::<_, i64>(&rate_key).await.unwrap(), 1);
    assert!(
        !connection
            .exists::<_, bool>(&concurrency_key)
            .await
            .unwrap()
    );

    let raw: (i64, i64, String, i64, i64, i64) = redis::Script::new(RESERVE_SCRIPT)
        .key(format!("{rate_key}:invalid-args"))
        .key(format!("{concurrency_key}:invalid-args"))
        .arg("9007199254740992")
        .arg(0)
        .arg(0)
        .arg(0)
        .arg("lease")
        .arg(1)
        .invoke_async(&mut connection)
        .await
        .unwrap();
    assert_eq!((raw.0, raw.1, raw.2.as_str()), (1, -1, "invalid_arguments"));
}

#[tokio::test]
#[ignore = "requires Valkey in OLP_VALKEY_URL"]
async fn counters_retain_exact_behavior_at_lua_safe_maximum() {
    let namespace = namespace("exact_integer");
    let lookup_id = "lookup_01";
    let limiter = DistributedLimiter::connect(&valkey_url(), &namespace)
        .await
        .unwrap();
    let mut connection = connection().await;
    let (rate_key, _) = keys(&namespace, lookup_id);
    let window = settle_in_minute(&mut connection).await / 60_000;
    let _: () = redis::pipe()
        .hset(&rate_key, "window", window)
        .hset(&rate_key, "rpm", MAX_LUA_INTEGER - 1)
        .hset(&rate_key, "tpm", MAX_LUA_INTEGER - 1)
        .pexpire(&rate_key, 60_000)
        .query_async(&mut connection)
        .await
        .unwrap();

    limiter
        .reserve(LimitRequest {
            requests_per_minute: Some(MAX_LUA_INTEGER),
            tokens_per_minute: Some(MAX_LUA_INTEGER),
            max_concurrency: None,
            requested_tokens: 1,
            ..request(lookup_id)
        })
        .await
        .unwrap();
    let state = rate_state(&mut connection, &rate_key).await;
    assert_eq!(state["rpm"], MAX_LUA_INTEGER);
    assert_eq!(state["tpm"], MAX_LUA_INTEGER);
    exceeded(
        limiter
            .reserve(LimitRequest {
                requests_per_minute: Some(MAX_LUA_INTEGER),
                tokens_per_minute: Some(MAX_LUA_INTEGER),
                max_concurrency: None,
                requested_tokens: 1,
                ..request(lookup_id)
            })
            .await
            .unwrap_err(),
        LimitDimension::Requests,
    );
    assert_eq!(
        rate_state(&mut connection, &rate_key).await["tpm"],
        MAX_LUA_INTEGER
    );
}

#[tokio::test]
#[ignore = "requires Valkey in OLP_VALKEY_URL"]
async fn concurrent_replicas_enforce_one_atomic_limit() {
    for attempt in 0..3 {
        let namespace = namespace(&format!("concurrent_{attempt}"));
        let lookup_id = "lookup_01";
        let limiter = DistributedLimiter::connect(&valkey_url(), &namespace)
            .await
            .unwrap();
        let mut connection = connection().await;
        let before_window = server_time_ms(&mut connection).await / 60_000;
        let reservations = (0..40).map(|_| {
            let limiter = limiter.clone();
            async move {
                limiter
                    .reserve(LimitRequest {
                        requests_per_minute: Some(10),
                        tokens_per_minute: None,
                        max_concurrency: None,
                        ..request(lookup_id)
                    })
                    .await
            }
        });
        let results = join_all(reservations).await;
        let after_window = server_time_ms(&mut connection).await / 60_000;
        if before_window != after_window {
            continue;
        }

        let granted = results.iter().filter(|result| result.is_ok()).count();
        let rejected = results
            .into_iter()
            .filter(|result| {
                matches!(
                    result,
                    Err(LimitError::Exceeded {
                        dimension: LimitDimension::Requests,
                        ..
                    })
                )
            })
            .count();
        assert_eq!(granted, 10);
        assert_eq!(rejected, 30);
        return;
    }
    panic!("all concurrency attempts crossed a UTC minute boundary");
}
