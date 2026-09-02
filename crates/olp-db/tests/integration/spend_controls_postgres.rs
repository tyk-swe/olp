use chrono::{Duration, TimeZone as _, Utc};
use olp_db::{
    identity::InstallationSetupInput, limits::DistributedLimiter, security::password::hash,
    store::Store, test_support::TestDb,
};
use redis::{AsyncCommands as _, aio::MultiplexedConnection};
use rust_decimal::Decimal;
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

async fn setup_authority(store: &Store, label: &str) -> (Uuid, Uuid) {
    let owner = store
        .setup_installation(InstallationSetupInput {
            installation_name: format!("Spend {label}"),
            email: format!("owner-{label}@example.test"),
            display_name: "Owner".to_owned(),
            password_hash: hash("correct horse battery staple").unwrap(),
        })
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
    .execute(store.pool())
    .await
    .unwrap();
    (owner.user_id, provider_id)
}

async fn insert_api_key(store: &Store, owner: Uuid, api_key_id: Uuid, lookup_id: &str) {
    sqlx::query(
        "INSERT INTO api_keys \
         (id, lookup_id, secret_digest, name, created_by, daily_cost_limit, monthly_cost_limit) \
         VALUES ($1, $2, $3, 'spend test', $4, 1, 10)",
    )
    .bind(api_key_id)
    .bind(lookup_id)
    .bind([7_u8; 32].as_slice())
    .bind(owner)
    .execute(store.pool())
    .await
    .unwrap();
}

async fn set_usage_retention_to_one_day(store: &Store, owner: Uuid) {
    sqlx::query(
        "INSERT INTO settings (key, value, etag, updated_by) \
         VALUES ('retention.usage_days', '1', $1, $2) \
         ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value",
    )
    .bind(Uuid::now_v7())
    .bind(owner)
    .execute(store.pool())
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

async fn insert_attempt(store: &Store, api_key_id: Uuid, provider_id: Uuid, fact: AttemptFact) {
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
    .execute(store.pool())
    .await
    .unwrap();
}

async fn insert_request_attempts(
    store: &Store,
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
    .execute(store.pool())
    .await
    .unwrap();
    insert_attempt(
        store,
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
        store,
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
        store,
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
    let store = db.store(4).await;
    let (owner, provider_id) = setup_authority(&store, "status").await;
    let api_key_id = Uuid::now_v7();
    insert_api_key(&store, owner, api_key_id, "spend_status_01").await;
    let now = Utc.with_ymd_and_hms(2026, 10, 5, 12, 0, 0).unwrap();
    insert_request_attempts(&store, api_key_id, provider_id, now - Duration::days(2)).await;
    set_usage_retention_to_one_day(&store, owner).await;

    store.run_maintenance(now).await.unwrap();
    let rolled: (i64, i64) = sqlx::query_as(
        "SELECT target_unpriced_count, unpriced_attempt_count FROM attempt_usage_hourly \
         WHERE api_key_id = $1",
    )
    .bind(api_key_id)
    .fetch_one(store.pool())
    .await
    .unwrap();
    assert_eq!(rolled, (1, 2));

    let current_request = Uuid::now_v7();
    sqlx::query(
        "INSERT INTO usage_request_anchors (request_id, request_started_at) VALUES ($1, $2)",
    )
    .bind(current_request)
    .bind(now)
    .execute(store.pool())
    .await
    .unwrap();
    insert_attempt(
        &store,
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

    let status = store.api_key_budget_status(api_key_id, now).await.unwrap();
    assert_eq!(status.daily.accrued, Decimal::new(1, 2));
    assert_eq!(status.monthly.accrued, Decimal::new(4, 2));
    assert_eq!(status.unpriced_attempts, 2);

    let namespace = namespace("loss");
    let limiter = DistributedLimiter::connect(&valkey_url(), &namespace)
        .await
        .unwrap();
    assert_eq!(
        limiter
            .reconcile_costs_at(&store, now)
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
    let report = limiter.reconcile_costs_at(&store, now).await.unwrap();
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
    let store = db.store(4).await;
    let (owner, _) = setup_authority(&store, "continues").await;
    let first_key = Uuid::from_u128(1);
    let second_key = Uuid::from_u128(2);
    insert_api_key(&store, owner, first_key, "spend_first_01").await;
    insert_api_key(&store, owner, second_key, "spend_second_01").await;
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

    let report = limiter.reconcile_costs_at(&store, now).await.unwrap();
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
    let store = db.store(2).await;
    let (owner, _) = setup_authority(&store, "future-window").await;
    let api_key_id = Uuid::now_v7();
    insert_api_key(&store, owner, api_key_id, "spend_future_01").await;
    let current = Utc.with_ymd_and_hms(2026, 1, 31, 23, 55, 0).unwrap();
    let future = Utc.with_ymd_and_hms(2026, 2, 1, 0, 0, 0).unwrap();
    store
        .add_cost_delta_for_test(api_key_id, current, Decimal::new(5, 1), 1)
        .await
        .unwrap();
    store
        .add_cost_delta_for_test(api_key_id, future, Decimal::new(1, 1), 0)
        .await
        .unwrap();

    let rows: Vec<(String, i64, String)> = sqlx::query_as(
        "SELECT window_kind, window_id, accrued::text FROM api_key_cost_windows \
         WHERE api_key_id = $1 ORDER BY window_kind, window_id",
    )
    .bind(api_key_id)
    .fetch_all(store.pool())
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

#[tokio::test]
#[ignore = "requires PostgreSQL"]
async fn migration_fences_n_minus_one_rollup_before_raw_fact_deletion() {
    let db = TestDb::create_empty("spend_n_minus_one_fence").await;
    let store = db.store(2).await;
    store.migrate_to(48).await.unwrap();
    let (owner, provider_id) = setup_authority(&store, "n-minus-one").await;
    let api_key_id = Uuid::now_v7();
    sqlx::query(
        "INSERT INTO api_keys (id, lookup_id, secret_digest, name, created_by) \
         VALUES ($1, 'spend_upgrade_01', $2, 'spend upgrade test', $3)",
    )
    .bind(api_key_id)
    .bind([7_u8; 32].as_slice())
    .bind(owner)
    .execute(store.pool())
    .await
    .unwrap();
    let now = Utc.with_ymd_and_hms(2026, 10, 5, 12, 0, 0).unwrap();
    insert_request_attempts(&store, api_key_id, provider_id, now - Duration::days(2)).await;
    store.migrate().await.unwrap();

    let mut transaction = store.pool().begin().await.unwrap();
    sqlx::query("SELECT set_config('olp.usage_rollup_writer', 'additive-v2', true)")
        .execute(&mut *transaction)
        .await
        .unwrap();
    sqlx::query("DELETE FROM attempt_usage_facts WHERE api_key_id = $1")
        .bind(api_key_id)
        .execute(&mut *transaction)
        .await
        .unwrap();
    let old_insert = sqlx::query(
        "INSERT INTO attempt_usage_hourly \
         (bucket, route_slug, provider_id, upstream_model, operation, surface, api_key_id, \
          request_count, provider_request_count, model_request_count, target_request_count, \
          input_tokens, output_tokens, cached_input_tokens, media_units, request_unpriced_count, \
          provider_unpriced_count, model_unpriced_count, target_unpriced_count, \
          request_incomplete_count, provider_incomplete_count, model_incomplete_count, \
          target_incomplete_count) \
         VALUES ($1, 'spend-route', $2, 'spend-model', 'generation', 'openai', $3, \
                 1, 1, 1, 1, 0, 0, 0, 0, 1, 1, 1, 1, 0, 0, 0, 0)",
    )
    .bind(now - Duration::days(2))
    .bind(provider_id)
    .bind(api_key_id)
    .execute(&mut *transaction)
    .await;
    assert!(old_insert.is_err());
    transaction.rollback().await.unwrap();
    let retained: i64 =
        sqlx::query_scalar("SELECT count(*) FROM attempt_usage_facts WHERE api_key_id = $1")
            .bind(api_key_id)
            .fetch_one(store.pool())
            .await
            .unwrap();
    assert_eq!(retained, 3);

    set_usage_retention_to_one_day(&store, owner).await;
    store.run_maintenance(now).await.unwrap();
    let exact: i64 = sqlx::query_scalar(
        "SELECT unpriced_attempt_count FROM attempt_usage_hourly WHERE api_key_id = $1",
    )
    .bind(api_key_id)
    .fetch_one(store.pool())
    .await
    .unwrap();
    assert_eq!(exact, 2);
}
