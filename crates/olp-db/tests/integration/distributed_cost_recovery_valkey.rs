use super::*;

#[tokio::test]
#[ignore = "requires Valkey in OLP_VALKEY_URL"]
async fn a_missing_budget_fails_closed_without_creating_any_limit_state() {
    let namespace = namespace("uninitialized");
    let api_key_id = Uuid::now_v7();
    let limiter = DistributedLimiter::connect(&valkey_url(), &namespace)
        .await
        .unwrap();
    let mut request = cost_request(api_key_id, Decimal::ONE, Decimal::ONE);
    request.requests_per_minute = Some(1);
    request.tokens_per_minute = Some(10);
    request.max_concurrency = Some(1);
    request.requested_tokens = 5;
    assert!(matches!(
        limiter.reserve(request.clone()).await,
        Err(LimitError::Service { .. })
    ));
    let mut connection = connection().await;
    let (daily, monthly) = cost_keys(&namespace, api_key_id);
    let (rate, concurrency) = rate_keys(&namespace, request.lookup_id);
    for key in [&daily, &monthly, &rate, &concurrency] {
        assert!(!connection.exists::<_, bool>(key).await.unwrap());
    }
    let now = Utc::now();
    limiter
        .apply_cost_snapshot(&snapshot(api_key_id, now, Decimal::ZERO, Decimal::ZERO, 0))
        .await
        .unwrap();
    let lease = limiter.reserve(request).await.unwrap();
    limiter.release(&lease).await.unwrap();
}

#[tokio::test]
#[ignore = "requires Valkey in OLP_VALKEY_URL"]
async fn full_and_individual_counter_loss_never_reopens_spend() {
    let namespace = namespace("counter-loss");
    let api_key_id = Uuid::now_v7();
    let limiter = DistributedLimiter::connect(&valkey_url(), &namespace)
        .await
        .unwrap();
    let now = Utc.with_ymd_and_hms(2026, 9, 15, 12, 0, 0).unwrap();
    let request = cost_request(api_key_id, Decimal::ONE, Decimal::from(10));
    let charged = snapshot(api_key_id, now, Decimal::ONE, Decimal::ONE, 0);
    let mut connection = connection().await;
    let (daily, monthly) = cost_keys(&namespace, api_key_id);

    for keys in [vec![&daily], vec![&monthly], vec![&daily, &monthly]] {
        limiter.apply_cost_snapshot_at(&charged, now).await.unwrap();
        let _: i64 = redis::cmd("DEL")
            .arg(&keys)
            .query_async(&mut connection)
            .await
            .unwrap();
        // An intact exhausted window may still return 429; neither outcome admits.
        assert!(matches!(
            limiter.reserve_cost_at(&request, now).await,
            Err(LimitError::Service { .. } | LimitError::Exceeded { .. })
        ));
        for key in keys {
            assert!(!connection.exists::<_, bool>(key).await.unwrap());
        }
        limiter.apply_cost_snapshot_at(&charged, now).await.unwrap();
        exceeded(
            limiter.reserve_cost_at(&request, now).await.unwrap_err(),
            LimitDimension::DailyCost,
        );
    }
}

#[tokio::test]
#[ignore = "requires Valkey in OLP_VALKEY_URL"]
async fn invalid_unpriced_values_are_repaired_even_when_authoritative_count_is_zero() {
    let namespace = namespace("repair-unpriced");
    let api_key_id = Uuid::now_v7();
    let limiter = DistributedLimiter::connect(&valkey_url(), &namespace)
        .await
        .unwrap();
    let now = Utc.with_ymd_and_hms(2026, 9, 15, 12, 0, 0).unwrap();
    let authoritative = snapshot(api_key_id, now, Decimal::new(1, 1), Decimal::new(1, 1), 0);
    let request = cost_request(api_key_id, Decimal::ONE, Decimal::ONE);
    let mut connection = connection().await;
    let (_, monthly) = cost_keys(&namespace, api_key_id);
    for malformed in ["garbage", "-1", "1.5", "9007199254740992", ""] {
        limiter.apply_cost_snapshot_at(&authoritative, now).await.unwrap();
        connection.hset::<_, _, _, ()>(&monthly, "unpriced", malformed).await.unwrap();
        assert!(matches!(
            limiter.reserve_cost_at(&request, now).await,
            Err(LimitError::MalformedState)
        ));
        limiter.apply_cost_snapshot_at(&authoritative, now).await.unwrap();
        assert_eq!(cost_state(&mut connection, &monthly).await["unpriced"], "0");
        limiter.reserve_cost_at(&request, now).await.unwrap();
    }
}

#[tokio::test]
#[ignore = "requires Valkey in OLP_VALKEY_URL"]
async fn wrong_type_keys_are_rebuilt_without_lowering_an_intact_window() {
    let namespace = namespace("repair-type");
    let api_key_id = Uuid::now_v7();
    let limiter = DistributedLimiter::connect(&valkey_url(), &namespace)
        .await
        .unwrap();
    let now = Utc.with_ymd_and_hms(2026, 9, 15, 12, 0, 0).unwrap();
    let older = snapshot(api_key_id, now, Decimal::new(1, 1), Decimal::new(1, 1), 0);
    let newer = snapshot(api_key_id, now, Decimal::new(2, 1), Decimal::new(2, 1), 0);
    let request = cost_request(api_key_id, Decimal::ONE, Decimal::ONE);
    let mut connection = connection().await;
    let (daily, monthly) = cost_keys(&namespace, api_key_id);
    for (broken, intact) in [(&daily, &monthly), (&monthly, &daily)] {
        limiter.apply_cost_snapshot_at(&newer, now).await.unwrap();
        connection.set::<_, _, ()>(broken, "wrong-type").await.unwrap();
        assert!(matches!(
            limiter.reserve_cost_at(&request, now).await,
            Err(LimitError::MalformedState)
        ));
        limiter.apply_cost_snapshot_at(&older, now).await.unwrap();
        assert_eq!(cost_state(&mut connection, broken).await["accrued"], "0.1");
        assert_eq!(cost_state(&mut connection, intact).await["accrued"], "0.2");
        limiter.reserve_cost_at(&request, now).await.unwrap();
    }
}
