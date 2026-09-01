use std::time::Duration;

use chrono::{DateTime, TimeZone as _, Utc};
use olp_db::{
    identity::InstallationSetupInput,
    limits::{CostReconciliationLeader, DistributedLimiter},
    security::password::hash,
    store::Store,
    test_support::TestDb,
};
use rust_decimal::Decimal;
use uuid::Uuid;

async fn authority(store: &Store) -> (Uuid, Uuid) {
    let owner = store.setup_installation(InstallationSetupInput {
        installation_name: "Spend recovery".to_owned(),
        email: "owner@recovery.test".to_owned(),
        display_name: "Owner".to_owned(),
        password_hash: hash("correct horse battery staple").unwrap(),
    }).await.unwrap();
    let key = Uuid::now_v7();
    let provider = Uuid::now_v7();
    sqlx::query(
        "INSERT INTO providers (id, name, kind, auth_mode, etag, created_by) \
         VALUES ($1, 'spend recovery', 'openai', 'api_key', $2, $3)",
    ).bind(provider).bind(Uuid::now_v7()).bind(owner.user_id)
        .execute(store.pool()).await.unwrap();
    sqlx::query(
        "INSERT INTO api_keys (id, lookup_id, secret_digest, name, created_by, \
                              daily_cost_limit, monthly_cost_limit) \
         VALUES ($1, 'spend_recovery_01', $2, 'spend recovery', $3, 1, 10)",
    ).bind(key).bind([7_u8; 32].as_slice()).bind(owner.user_id)
        .execute(store.pool()).await.unwrap();
    (key, provider)
}

async fn priced_attempt(
    store: &Store,
    key: Uuid,
    provider: Uuid,
    observed_at: DateTime<Utc>,
    cost: Decimal,
) {
    let request = Uuid::now_v7();
    sqlx::query(
        "INSERT INTO usage_request_anchors (request_id, request_started_at) VALUES ($1, $2)",
    ).bind(request).bind(observed_at).execute(store.pool()).await.unwrap();
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
         VALUES ($1, $2, $3, $4, 1, $5, $6, 'spend-route', 'spend-model', 'generation', \
                 'openai', $4, 'billable', true, true, 1, 1, 0, NULL, $7, false, NULL, \
                 'USD', true, true, true, true, false, false, false, false, \
                 false, false, false, false)",
    ).bind(Uuid::now_v7()).bind(Uuid::now_v7()).bind(request).bind(observed_at)
        .bind(key).bind(provider).bind(cost).execute(store.pool()).await.unwrap();
    store.add_cost_delta_for_test(key, observed_at, cost, 0).await.unwrap();
}

#[tokio::test]
#[ignore = "requires PostgreSQL and Valkey"]
async fn future_skew_is_excluded_from_today_but_retained_in_its_own_window() {
    let db = TestDb::create_migrated("spend_day_end").await;
    let store = db.store(2).await;
    let (key, provider) = authority(&store).await;
    let now = Utc.with_ymd_and_hms(2026, 9, 15, 23, 59, 0).unwrap();
    let tomorrow = Utc.with_ymd_and_hms(2026, 9, 16, 0, 0, 0).unwrap();
    priced_attempt(&store, key, provider, now, Decimal::new(25, 2)).await;
    priced_attempt(&store, key, provider, tomorrow, Decimal::new(75, 2)).await;
    priced_attempt(
        &store, key, provider, tomorrow + chrono::Duration::minutes(1), Decimal::new(5, 1),
    ).await;
    let status = store.api_key_budget_status(key, now).await.unwrap();
    assert_eq!(status.daily.accrued, Decimal::new(25, 2));
    assert_eq!(status.monthly.accrued, Decimal::new(15, 1));

    let url = std::env::var("OLP_VALKEY_URL").expect("Valkey test URL");
    let namespace = format!("olp:test:spend-recovery:{}", Uuid::now_v7().simple());
    let limiter = DistributedLimiter::connect(&url, namespace).await.unwrap();
    let report = limiter.reconcile_costs_at(&store, now).await.unwrap();
    assert!(report.lock_acquired);
    assert_eq!(report.daily_windows_reconciled, 1);
    assert_eq!(report.monthly_windows_reconciled, 1);
    let daily: Vec<(i64, String)> = sqlx::query_as(
        "SELECT window_id, accrued::text FROM api_key_cost_windows \
         WHERE api_key_id = $1 AND window_kind = 'day' ORDER BY window_id",
    ).bind(key).fetch_all(store.pool()).await.unwrap();
    assert_eq!(daily.len(), 2);
    assert_eq!(daily[0].0, now.timestamp().div_euclid(86_400));
    assert_eq!(daily[0].1.parse::<Decimal>().unwrap(), Decimal::new(25, 2));
    assert_eq!(daily[1].1.parse::<Decimal>().unwrap(), Decimal::new(125, 2));
    let status = store.api_key_budget_status(key, tomorrow).await.unwrap();
    assert_eq!(status.daily.accrued, Decimal::new(125, 2));
    assert_eq!(status.monthly.accrued, Decimal::new(15, 1));
}

async fn await_leadership(store: &Store) -> CostReconciliationLeader {
    tokio::time::timeout(Duration::from_secs(5), async {
        loop {
            if let Some(leader) = store.try_acquire_cost_reconciliation_leader().await.unwrap() {
                return leader;
            }
            tokio::task::yield_now().await;
        }
    }).await.expect("released PostgreSQL session lock becomes available")
}

#[tokio::test]
#[ignore = "requires PostgreSQL"]
async fn leadership_is_retained_across_follower_ticks_and_released_on_drop() {
    let db = TestDb::create_migrated("spend_leader").await;
    let first = db.store(1).await;
    let second = db.store(1).await;
    let third = db.store(1).await;
    let leader = first.try_acquire_cost_reconciliation_leader().await.unwrap().unwrap();
    for _ in 0..3 {
        assert!(second.try_acquire_cost_reconciliation_leader().await.unwrap().is_none());
        assert!(third.try_acquire_cost_reconciliation_leader().await.unwrap().is_none());
    }
    drop(leader);
    let successor = await_leadership(&second).await;
    assert!(first.try_acquire_cost_reconciliation_leader().await.unwrap().is_none());
    drop(successor);
    drop(await_leadership(&third).await);
}

#[tokio::test]
#[ignore = "requires PostgreSQL"]
async fn cancelling_the_owner_drops_the_detached_lock_session() {
    let db = TestDb::create_migrated("spend_cancel").await;
    let store = db.store(1).await;
    let owned_store = store.clone();
    let (ready, receiver) = tokio::sync::oneshot::channel();
    let task = tokio::spawn(async move {
        let _leader = owned_store.try_acquire_cost_reconciliation_leader().await.unwrap().unwrap();
        ready.send(()).unwrap();
        std::future::pending::<()>().await;
    });
    receiver.await.unwrap();
    assert!(store.try_acquire_cost_reconciliation_leader().await.unwrap().is_none());
    task.abort();
    assert!(task.await.unwrap_err().is_cancelled());
    drop(await_leadership(&store).await);
}
