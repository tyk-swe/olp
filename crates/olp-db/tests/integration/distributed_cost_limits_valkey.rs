use std::{collections::HashMap, time::Duration};

use chrono::{Datelike as _, TimeZone as _, Utc};
use olp_db::limits::{CostSnapshot, DistributedLimiter};
use olp_engine::inference::limits::{LimitDimension, LimitError, LimitRequest};
use redis::{AsyncCommands as _, aio::MultiplexedConnection};
use rust_decimal::Decimal;
use uuid::Uuid;

fn valkey_url() -> String {
    std::env::var("OLP_VALKEY_URL").expect("OLP_VALKEY_URL must point to a Valkey test endpoint")
}

fn namespace(label: &str) -> String {
    format!("olp:test:cost-limits:{label}:{}", Uuid::now_v7().simple())
}

fn rate_keys(namespace: &str, lookup_id: &str) -> (String, String) {
    (
        format!("{namespace}:{{{lookup_id}}}:rate"),
        format!("{namespace}:{{{lookup_id}}}:concurrency:v2"),
    )
}

fn cost_keys(namespace: &str, api_key_id: Uuid) -> (String, String) {
    let prefix = format!("{namespace}:{{{}}}:cost", api_key_id.simple());
    (format!("{prefix}:day"), format!("{prefix}:month"))
}

fn cost_request(api_key_id: Uuid, daily: Decimal, monthly: Decimal) -> LimitRequest<'static> {
    LimitRequest {
        api_key_id,
        lookup_id: "lookup_01",
        requests_per_minute: None,
        tokens_per_minute: None,
        max_concurrency: None,
        daily_cost_limit: Some(daily),
        monthly_cost_limit: Some(monthly),
        requested_tokens: 0,
        lease_ttl: Duration::from_secs(5),
    }
}

fn snapshot(
    api_key_id: Uuid,
    at: chrono::DateTime<Utc>,
    daily_accrued: Decimal,
    monthly_accrued: Decimal,
    unpriced_attempts: u64,
) -> CostSnapshot {
    CostSnapshot {
        api_key_id,
        daily_window_id: at.timestamp().div_euclid(86_400),
        daily_accrued,
        monthly_window_id: i64::from(at.year()) * 12 + i64::from(at.month0()),
        monthly_accrued,
        unpriced_attempts,
    }
}

async fn connection() -> MultiplexedConnection {
    redis::Client::open(valkey_url())
        .unwrap()
        .get_multiplexed_async_connection()
        .await
        .unwrap()
}

async fn cost_state(connection: &mut MultiplexedConnection, key: &str) -> HashMap<String, String> {
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

#[tokio::test]
#[ignore = "requires Valkey in OLP_VALKEY_URL"]
async fn cost_reservation_rolls_daily_without_resetting_the_month() {
    let namespace = namespace("daily-rollover");
    let api_key_id = Uuid::now_v7();
    let limiter = DistributedLimiter::connect(&valkey_url(), &namespace)
        .await
        .unwrap();
    let first_day = Utc.with_ymd_and_hms(2026, 10, 5, 12, 0, 0).unwrap();
    let next_day = Utc.with_ymd_and_hms(2026, 10, 6, 12, 0, 0).unwrap();
    limiter
        .apply_cost_snapshot_at(
            &snapshot(
                api_key_id,
                first_day,
                Decimal::new(1, 2),
                Decimal::new(1, 2),
                0,
            ),
            first_day,
        )
        .await
        .unwrap();

    let request = cost_request(api_key_id, Decimal::new(1, 2), Decimal::new(2, 2));
    let retry = exceeded(
        limiter
            .reserve_cost_at(&request, first_day)
            .await
            .unwrap_err(),
        LimitDimension::DailyCost,
    );
    assert_eq!(retry, Duration::from_secs(12 * 60 * 60));
    limiter.reserve_cost_at(&request, next_day).await.unwrap();

    let mut connection = connection().await;
    let (daily_key, monthly_key) = cost_keys(&namespace, api_key_id);
    assert_eq!(
        cost_state(&mut connection, &daily_key).await["accrued"],
        "0"
    );
    assert_eq!(
        cost_state(&mut connection, &monthly_key).await["accrued"],
        "0.01"
    );
}

#[tokio::test]
#[ignore = "requires Valkey in OLP_VALKEY_URL"]
async fn cost_reservation_rolls_monthly_at_the_injected_utc_boundary() {
    let namespace = namespace("month-rollover");
    let api_key_id = Uuid::now_v7();
    let limiter = DistributedLimiter::connect(&valkey_url(), &namespace)
        .await
        .unwrap();
    let january = Utc.with_ymd_and_hms(2026, 1, 31, 12, 0, 0).unwrap();
    let february = Utc.with_ymd_and_hms(2026, 2, 1, 0, 0, 0).unwrap();
    limiter
        .apply_cost_snapshot_at(
            &snapshot(
                api_key_id,
                january,
                Decimal::new(3, 1),
                Decimal::new(3, 1),
                2,
            ),
            january,
        )
        .await
        .unwrap();
    let request = cost_request(api_key_id, Decimal::ONE, Decimal::new(3, 1));

    let retry = exceeded(
        limiter
            .reserve_cost_at(&request, january)
            .await
            .unwrap_err(),
        LimitDimension::MonthlyCost,
    );
    assert_eq!(retry, Duration::from_secs(12 * 60 * 60));
    limiter.reserve_cost_at(&request, february).await.unwrap();

    let mut connection = connection().await;
    let (_, monthly_key) = cost_keys(&namespace, api_key_id);
    let state = cost_state(&mut connection, &monthly_key).await;
    assert_eq!(state["accrued"], "0");
    assert_eq!(state["unpriced"], "0");
}

#[tokio::test]
#[ignore = "requires Valkey in OLP_VALKEY_URL"]
async fn future_window_snapshot_cannot_replace_current_spend_early() {
    let namespace = namespace("future-window");
    let api_key_id = Uuid::now_v7();
    let limiter = DistributedLimiter::connect(&valkey_url(), &namespace)
        .await
        .unwrap();
    let current = Utc.with_ymd_and_hms(2026, 1, 31, 23, 55, 0).unwrap();
    let future = Utc.with_ymd_and_hms(2026, 2, 1, 0, 0, 0).unwrap();
    let current_snapshot = snapshot(
        api_key_id,
        current,
        Decimal::new(5, 1),
        Decimal::new(5, 1),
        1,
    );
    let future_snapshot = snapshot(
        api_key_id,
        future,
        Decimal::new(1, 1),
        Decimal::new(1, 1),
        0,
    );
    limiter
        .apply_cost_snapshot_at(&current_snapshot, current)
        .await
        .unwrap();
    limiter
        .apply_cost_snapshot_at(&future_snapshot, current)
        .await
        .unwrap();

    let mut connection = connection().await;
    let (daily_key, monthly_key) = cost_keys(&namespace, api_key_id);
    assert_eq!(
        cost_state(&mut connection, &daily_key).await["accrued"],
        "0.5"
    );
    assert_eq!(
        cost_state(&mut connection, &monthly_key).await["accrued"],
        "0.5"
    );

    limiter
        .apply_cost_snapshot_at(&future_snapshot, future)
        .await
        .unwrap();
    assert_eq!(
        cost_state(&mut connection, &daily_key).await["accrued"],
        "0.1"
    );
    assert_eq!(
        cost_state(&mut connection, &monthly_key).await["accrued"],
        "0.1"
    );
}

#[tokio::test]
#[ignore = "requires Valkey in OLP_VALKEY_URL"]
async fn cost_arithmetic_is_exact_and_rotation_keeps_the_uuid_counter() {
    let namespace = namespace("exact");
    let api_key_id = Uuid::now_v7();
    let limiter = DistributedLimiter::connect(&valkey_url(), &namespace)
        .await
        .unwrap();
    let now = Utc.with_ymd_and_hms(2026, 10, 5, 12, 0, 0).unwrap();
    for cost in [Decimal::new(1, 1), Decimal::new(3, 1), Decimal::new(1, 1)] {
        limiter
            .apply_cost_snapshot_at(&snapshot(api_key_id, now, cost, cost, 0), now)
            .await
            .unwrap();
    }
    let mut rotated = cost_request(api_key_id, Decimal::new(3, 1), Decimal::ONE);
    rotated.lookup_id = "lookup_02";
    exceeded(
        limiter.reserve_cost_at(&rotated, now).await.unwrap_err(),
        LimitDimension::DailyCost,
    );

    let mut connection = connection().await;
    let (daily_key, _) = cost_keys(&namespace, api_key_id);
    assert_eq!(
        cost_state(&mut connection, &daily_key).await["accrued"],
        "0.3"
    );
}

#[tokio::test]
#[ignore = "requires Valkey in OLP_VALKEY_URL"]
async fn exhausted_cost_rejects_before_rate_or_concurrency_mutation() {
    let namespace = namespace("cost-first");
    let api_key_id = Uuid::now_v7();
    let lookup_id = "lookup_01";
    let limiter = DistributedLimiter::connect(&valkey_url(), &namespace)
        .await
        .unwrap();
    limiter
        .apply_cost_snapshot(&snapshot(
            api_key_id,
            Utc::now(),
            Decimal::new(1, 2),
            Decimal::new(1, 2),
            0,
        ))
        .await
        .unwrap();

    exceeded(
        limiter
            .reserve(LimitRequest {
                api_key_id,
                lookup_id,
                requests_per_minute: Some(1),
                tokens_per_minute: Some(10),
                max_concurrency: Some(1),
                daily_cost_limit: Some(Decimal::new(1, 2)),
                monthly_cost_limit: Some(Decimal::ONE),
                requested_tokens: 5,
                lease_ttl: Duration::from_secs(5),
            })
            .await
            .unwrap_err(),
        LimitDimension::DailyCost,
    );
    let mut connection = connection().await;
    let (rate_key, concurrency_key) = rate_keys(&namespace, lookup_id);
    assert!(!connection.exists::<_, bool>(rate_key).await.unwrap());
    assert!(!connection.exists::<_, bool>(concurrency_key).await.unwrap());
}
