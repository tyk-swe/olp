use chrono::Duration;
use chrono::TimeZone as _;
use chrono::Utc;
use olp::access::identity::InstallationSetupInput;
use olp::crypto::password::hash;
use olp::limits::distributed::DistributedLimiter;
use olp::test_support::TestDb;
use redis::AsyncCommands as _;
use redis::aio::MultiplexedConnection;
use rust_decimal::Decimal;
use sqlx::PgPool;
use uuid::Uuid;

fn valkey_url() -> String {
    std::env::var("OLP_VALKEY_URL").expect("OLP_VALKEY_URL must point to a Valkey test endpoint")
}

fn namespace(label: &str) -> String {
    format!("olp:test:spend:{label}:{}", Uuid::now_v7().simple())
}

fn cost_keys(namespace: &str, api_key_id: Uuid) -> (String, String) {
    let prefix = format!("{namespace}:{{{}}}:cost", api_key_id.simple());
    (format!("{prefix}:day"), format!("{prefix}:month"))
}

async fn valkey_connection() -> MultiplexedConnection {
    redis::Client::open(valkey_url())
        .unwrap()
        .get_multiplexed_async_connection()
        .await
        .unwrap()
}

async fn setup_authority(pool: &PgPool, label: &str) -> (Uuid, Uuid) {
    let owner = olp::access::identity::setup::setup_installation(
        pool,
        &olp::database::RequestProvenance::default(),
        InstallationSetupInput {
            installation_name: format!("Spend {label}"),
            email: format!("owner-{label}@example.test"),
            display_name: "Owner".to_owned(),
            password_hash: hash("correct horse battery staple").unwrap(),
        },
    )
    .await
    .unwrap();
    let provider_id = Uuid::now_v7();
    sqlx::query(
        "INSERT INTO providers (id, name, kind, auth_mode, etag, created_by) \
         VALUES ($1, $2, 'openai', 'api_key', $3, $4)",
    )
    .bind(provider_id)
    .bind(format!("provider-{label}"))
    .bind(Uuid::now_v7())
    .bind(owner.user_id)
    .execute(pool)
    .await
    .unwrap();
    (owner.user_id, provider_id)
}

async fn insert_api_key(pool: &PgPool, owner: Uuid, api_key_id: Uuid, lookup_id: &str) {
    sqlx::query(
        "INSERT INTO api_keys \
         (id, lookup_id, secret_digest, name, created_by, daily_cost_limit, monthly_cost_limit) \
         VALUES ($1, $2, $3, 'spend test', $4, 1, 10)",
    )
    .bind(api_key_id)
    .bind(lookup_id)
    .bind([7_u8; 32].as_slice())
    .bind(owner)
    .execute(pool)
    .await
    .unwrap();
}

async fn set_usage_retention_to_one_day(pool: &PgPool, owner: Uuid) {
    sqlx::query(
        "INSERT INTO settings (key, value, etag, updated_by) \
         VALUES ('retention.usage_days', '1', $1, $2) \
         ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value",
    )
    .bind(Uuid::now_v7())
    .bind(owner)
    .execute(pool)
    .await
    .unwrap();
}

struct AttemptFact {
    request_id: Uuid,
    ordinal: i16,
    observed_at: chrono::DateTime<Utc>,
    estimated_cost: Option<Decimal>,
    unpriced: bool,
}

async fn insert_attempt(pool: &PgPool, api_key_id: Uuid, provider_id: Uuid, fact: AttemptFact) {
    sqlx::query(
        "INSERT INTO attempt_usage_facts \
         (attempt_id, event_id, request_id, request_started_at, attempt_ordinal, api_key_id, \
          provider_id, route_slug, upstream_model, operation, surface, observed_at, \
          charge_status, usage_observed, usage_complete, input_tokens, output_tokens, \
          cached_input_tokens, media_units, estimated_cost, unpriced, pricing_revision_id, \
          currency, request_counted, provider_request_counted, model_request_counted, \
          target_request_counted, request_unpriced_counted, provider_unpriced_counted, \
          model_unpriced_counted, target_unpriced_counted, request_incomplete_counted, \
          provider_incomplete_counted, model_incomplete_counted, target_incomplete_counted) \
         VALUES ($1, $2, $3, $4, $5, $6, $7, 'spend-route', 'spend-model', 'generation', \
                 'openai', $4, 'billable', true, true, 1, 1, 0, NULL, $8, $9, NULL, \
                 CASE WHEN $8::numeric IS NULL THEN NULL ELSE 'USD' END, \
                 $5 = 1, $5 = 1, $5 = 1, $5 = 1, $5 = 1 AND $9, $5 = 1 AND $9, \
                 $5 = 1 AND $9, $5 = 1 AND $9, false, false, false, false)",
    )
    .bind(Uuid::now_v7())
    .bind(Uuid::now_v7())
    .bind(fact.request_id)
    .bind(fact.observed_at)
    .bind(fact.ordinal)
    .bind(api_key_id)
    .bind(provider_id)
    .bind(fact.estimated_cost)
    .bind(fact.unpriced)
    .execute(pool)
    .await
    .unwrap();
}

async fn insert_request_attempts(
    pool: &PgPool,
    api_key_id: Uuid,
    provider_id: Uuid,
    observed_at: chrono::DateTime<Utc>,
) {
    let request_id = Uuid::now_v7();
    sqlx::query(
        "INSERT INTO usage_request_anchors (request_id, request_started_at) VALUES ($1, $2)",
    )
    .bind(request_id)
    .bind(observed_at)
    .execute(pool)
    .await
    .unwrap();
    insert_attempt(
        pool,
        api_key_id,
        provider_id,
        AttemptFact {
            request_id,
            ordinal: 1,
            observed_at,
            estimated_cost: None,
            unpriced: true,
        },
    )
    .await;
    insert_attempt(
        pool,
        api_key_id,
        provider_id,
        AttemptFact {
            request_id,
            ordinal: 2,
            observed_at,
            estimated_cost: None,
            unpriced: true,
        },
    )
    .await;
    insert_attempt(
        pool,
        api_key_id,
        provider_id,
        AttemptFact {
            request_id,
            ordinal: 3,
            observed_at,
            estimated_cost: Some(Decimal::new(3, 2)),
            unpriced: false,
        },
    )
    .await;
}

#[tokio::test]
#[ignore = "requires PostgreSQL and Valkey"]
async fn status_and_reconciliation_include_raw_and_exact_hourly_attempts() {
    let db = TestDb::create_migrated("spend_status_reconciliation").await;
    let pool = db.pool(4).await;
    let (owner, provider_id) = setup_authority(&pool, "status").await;
    let api_key_id = Uuid::now_v7();
    insert_api_key(&pool, owner, api_key_id, "spend_status_01").await;
    let now = Utc.with_ymd_and_hms(2026, 10, 5, 12, 0, 0).unwrap();
    insert_request_attempts(&pool, api_key_id, provider_id, now - Duration::days(2)).await;
    set_usage_retention_to_one_day(&pool, owner).await;

    olp::usage::retention::run_maintenance(&pool, now)
        .await
        .unwrap();
    let rolled: (i64, i64) = sqlx::query_as(
        "SELECT target_unpriced_count, unpriced_attempt_count FROM attempt_usage_hourly \
         WHERE api_key_id = $1",
    )
    .bind(api_key_id)
    .fetch_one(&pool)
    .await
    .unwrap();
    assert_eq!(rolled, (1, 2));

    let current_request = Uuid::now_v7();
    sqlx::query(
        "INSERT INTO usage_request_anchors (request_id, request_started_at) VALUES ($1, $2)",
    )
    .bind(current_request)
    .bind(now)
    .execute(&pool)
    .await
    .unwrap();
    insert_attempt(
        &pool,
        api_key_id,
        provider_id,
        AttemptFact {
            request_id: current_request,
            ordinal: 1,
            observed_at: now,
            estimated_cost: Some(Decimal::new(1, 2)),
            unpriced: false,
        },
    )
    .await;

    let status = olp::limits::budgets::api_key_budget_status(&pool, api_key_id, now)
        .await
        .unwrap();
    assert_eq!(status.daily.accrued, Decimal::new(1, 2));
    assert_eq!(status.monthly.accrued, Decimal::new(4, 2));
    assert_eq!(status.unpriced_attempts, 2);

    let namespace = namespace("loss");
    let limiter = DistributedLimiter::connect(&valkey_url(), &namespace)
        .await
        .unwrap();
    assert_eq!(
        limiter
            .reconcile_costs_at(&pool, now)
            .await
            .unwrap()
            .keys_reconciled,
        1
    );
    let mut connection = valkey_connection().await;
    let (daily_key, monthly_key) = cost_keys(&namespace, api_key_id);
    let _: i64 = redis::cmd("DEL")
        .arg(&daily_key)
        .arg(&monthly_key)
        .query_async(&mut connection)
        .await
        .unwrap();
    assert!(!connection.exists::<_, bool>(&daily_key).await.unwrap());
    let report = limiter.reconcile_costs_at(&pool, now).await.unwrap();
    assert_eq!(report.keys_reconciled, 1);
    assert_eq!(
        connection
            .hget::<_, _, String>(&daily_key, "accrued")
            .await
            .unwrap(),
        "0.01"
    );
    assert_eq!(
        connection
            .hget::<_, _, String>(&monthly_key, "accrued")
            .await
            .unwrap(),
        "0.04"
    );
    assert_eq!(
        connection
            .hget::<_, _, i64>(&monthly_key, "unpriced")
            .await
            .unwrap(),
        2
    );
}

#[tokio::test]
#[ignore = "requires PostgreSQL and Valkey"]
async fn reconciliation_repairs_malformed_state_and_continues_to_later_keys() {
    let db = TestDb::create_migrated("spend_reconciliation_continues").await;
    let pool = db.pool(4).await;
    let (owner, _) = setup_authority(&pool, "continues").await;
    let first_key = Uuid::from_u128(1);
    let second_key = Uuid::from_u128(2);
    insert_api_key(&pool, owner, first_key, "spend_first_01").await;
    insert_api_key(&pool, owner, second_key, "spend_second_01").await;
    let namespace = namespace("continues");
    let limiter = DistributedLimiter::connect(&valkey_url(), &namespace)
        .await
        .unwrap();
    let mut connection = valkey_connection().await;
    let (first_daily, _) = cost_keys(&namespace, first_key);
    connection
        .hset::<_, _, _, ()>(&first_daily, "window", "malformed")
        .await
        .unwrap();
    let now = Utc.with_ymd_and_hms(2026, 10, 5, 12, 0, 0).unwrap();

    let report = limiter.reconcile_costs_at(&pool, now).await.unwrap();
    assert_eq!(report.keys_reconciled, 2);
    assert_eq!(
        connection
            .hget::<_, _, String>(&first_daily, "accrued")
            .await
            .unwrap(),
        "0"
    );
    let (second_daily, second_monthly) = cost_keys(&namespace, second_key);
    assert!(connection.exists::<_, bool>(second_daily).await.unwrap());
    assert!(connection.exists::<_, bool>(second_monthly).await.unwrap());
}

#[tokio::test]
#[ignore = "requires PostgreSQL"]
async fn future_window_delta_cannot_replace_the_current_durable_window() {
    let db = TestDb::create_migrated("spend_future_window").await;
    let pool = db.pool(2).await;
    let (owner, _) = setup_authority(&pool, "future-window").await;
    let api_key_id = Uuid::now_v7();
    insert_api_key(&pool, owner, api_key_id, "spend_future_01").await;
    let current = Utc.with_ymd_and_hms(2026, 1, 31, 23, 55, 0).unwrap();
    let future = Utc.with_ymd_and_hms(2026, 2, 1, 0, 0, 0).unwrap();
    olp::limits::budgets::add_cost_delta_for_test(
        &pool,
        api_key_id,
        current,
        Decimal::new(5, 1),
        1,
    )
    .await
    .unwrap();
    olp::limits::budgets::add_cost_delta_for_test(&pool, api_key_id, future, Decimal::new(1, 1), 0)
        .await
        .unwrap();

    let rows: Vec<(String, i64, String)> = sqlx::query_as(
        "SELECT window_kind, window_id, accrued::text FROM api_key_cost_windows \
         WHERE api_key_id = $1 ORDER BY window_kind, window_id",
    )
    .bind(api_key_id)
    .fetch_all(&pool)
    .await
    .unwrap();
    assert_eq!(rows.len(), 4);
    assert!(
        rows.iter()
            .any(|(kind, _, cost)| kind == "day" && cost == "0.500000000000")
    );
    assert!(
        rows.iter()
            .any(|(kind, _, cost)| kind == "day" && cost == "0.100000000000")
    );
    assert!(
        rows.iter()
            .any(|(kind, _, cost)| kind == "month" && cost == "0.500000000000")
    );
    assert!(
        rows.iter()
            .any(|(kind, _, cost)| kind == "month" && cost == "0.100000000000")
    );
}
