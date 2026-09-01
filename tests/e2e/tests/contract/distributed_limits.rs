use super::*;
use rust_decimal::Decimal;
use std::time::{Duration, Instant};

// ---------------------------------------------------------------------------
// Distributed limits
//
// docs/architecture.md "Distributed limit semantics": RPM, TPM and concurrency
// are decided by one atomic reservation against Valkey server time, which also
// derives `Retry-After`; "A rejection consumes no dimension".
// ---------------------------------------------------------------------------

#[test]
#[ignore = "end-to-end; run via make e2e"]
fn a_key_over_its_request_limit_is_refused_with_a_retry_after() {
    runtime().block_on(async {
        let world = world();
        let key = world
            .issue_key("rpm probe", json!({"requests_per_minute": 1}))
            .await
            .expect("rate-limited key");

        // Issuing the key already spent a request against the gateway's model
        // listing, so drive the limit from a known state: send until a 429
        // arrives, bounded, and assert the shape of the refusal.
        let checkpoint = world.mock.checkpoint();
        let mut refusal = None;
        let mut accepted = 0;
        for _ in 0..4 {
            let response = world
                .gateway_post(
                    "/openai/v1/chat/completions",
                    json!({
                        "model": world::OPENAI_ROUTE,
                        "messages": [{"role": "user", "content": nonce("rpm")}]
                    }),
                    &key.secret,
                )
                .await
                .expect("chat completion");
            if response.status == 429 {
                refusal = Some(response);
                break;
            }
            assert_eq!(
                response.status, 200,
                "an in-limit request failed with {}: {}",
                response.status, response.text
            );
            accepted += 1;
        }

        let refusal = refusal.unwrap_or_else(|| {
            panic!("a key limited to one request per minute served {accepted} requests without refusing any")
        });
        assert!(
            accepted <= 1,
            "a key limited to one request per minute served {accepted} before refusing"
        );

        let retry_after = refusal
            .header("retry-after")
            .unwrap_or_else(|| panic!("the 429 carries no Retry-After: {}", refusal.text));
        let seconds: u64 = retry_after.parse().unwrap_or_else(|_| {
            panic!("Retry-After must be a delay in seconds; got {retry_after:?}")
        });
        assert!(
            (1..=60).contains(&seconds),
            "Retry-After is derived from the remaining fixed minute window, so \
             it must fall in 1..=60; got {seconds}"
        );

        // "A rejection consumes no dimension" — and a refused request must not
        // reach the provider at all.
        let upstream = world.mock.since(checkpoint);
        assert_eq!(
            upstream.len(),
            accepted,
            "{accepted} admitted requests produced {} upstream calls, so a \
             refused request still reached the provider",
            upstream.len()
        );
    });
}

async fn await_budget(
    api_key_id: &str,
    minimum_unpriced_attempts: u64,
    require_accrued_cost: bool,
) -> Value {
    let deadline = Instant::now() + Duration::from_secs(30);
    loop {
        let response = world()
            .management
            .get(&format!("/api/v1/api-keys/{api_key_id}"))
            .await
            .expect("API-key budget read");
        assert_eq!(
            response.status, 200,
            "API-key budget read: {}",
            response.body
        );
        let accrued = response.body["budget"]["daily"]["accrued"]
            .as_str()
            .and_then(|value| Decimal::from_str_exact(value).ok());
        let unpriced_attempts = response.body["budget"]["unpriced_attempts"].as_u64();
        let accrued_ready = accrued.is_some_and(|value| {
            if require_accrued_cost {
                value > Decimal::ZERO
            } else {
                value == Decimal::ZERO
            }
        });
        if accrued_ready
            && unpriced_attempts.is_some_and(|value| value >= minimum_unpriced_attempts)
        {
            return response.body;
        }
        assert!(
            Instant::now() <= deadline,
            "budget did not converge within 30 seconds: {}",
            response.body
        );
        tokio::time::sleep(Duration::from_millis(100)).await;
    }
}

async fn await_daily_budget_rejection(api_key_id: &str, limit: Decimal) {
    use olp_db::{limits::DistributedLimiter, store::Store};
    use olp_engine::inference::limits::{LimitDimension, LimitError, LimitRequest};

    let world = world();
    let store = Store::connect(&world.database_url, 1)
        .await
        .expect("budget counter PostgreSQL connection");
    let namespace = store
        .valkey_keyspace()
        .await
        .expect("installation Valkey keyspace")
        .limits_namespace();
    let limiter = DistributedLimiter::connect(
        &world.valkey_url().await.expect("fixture Valkey URL"),
        namespace,
    )
    .await
    .expect("budget counter connection");
    let api_key_id = api_key_id.parse().expect("API key UUID");
    let deadline = Instant::now() + Duration::from_secs(30);
    loop {
        let result = limiter
            .reserve(LimitRequest {
                api_key_id,
                lookup_id: "budget_probe",
                requests_per_minute: None,
                tokens_per_minute: None,
                max_concurrency: None,
                daily_cost_limit: Some(limit),
                monthly_cost_limit: None,
                requested_tokens: 0,
                lease_ttl: Duration::from_secs(1),
            })
            .await;
        match result {
            Err(LimitError::Exceeded {
                dimension: LimitDimension::DailyCost,
                ..
            }) => return,
            Ok(_) => {}
            Err(error) => panic!("budget counter check failed: {error}"),
        }
        assert!(
            Instant::now() <= deadline,
            "daily budget counter did not converge within 30 seconds"
        );
        tokio::time::sleep(Duration::from_millis(10)).await;
    }
}

async fn await_unpriced_cost_counter(api_key_id: &str, minimum: u64) {
    use olp_db::store::Store;

    let world = world();
    let store = Store::connect(&world.database_url, 1)
        .await
        .expect("unpriced counter PostgreSQL connection");
    let namespace = store
        .valkey_keyspace()
        .await
        .expect("installation Valkey keyspace")
        .limits_namespace();
    let key = format!("{namespace}:{{{}}}:cost:month", api_key_id.replace('-', ""));
    let client = redis::Client::open(world.valkey_url().await.expect("fixture Valkey URL"))
        .expect("unpriced counter client");
    let mut connection = client
        .get_multiplexed_async_connection()
        .await
        .expect("unpriced counter connection");
    let deadline = Instant::now() + Duration::from_secs(30);
    loop {
        let count: Option<u64> = redis::cmd("HGET")
            .arg(&key)
            .arg("unpriced")
            .query_async(&mut connection)
            .await
            .expect("unpriced counter read");
        if count.is_some_and(|value| value >= minimum) {
            return;
        }
        assert!(
            Instant::now() <= deadline,
            "unpriced counter did not converge within 30 seconds"
        );
        tokio::time::sleep(Duration::from_millis(10)).await;
    }
}

#[test]
#[ignore = "end-to-end; run via make e2e"]
fn a_priced_request_exhausts_a_daily_budget_before_the_second_request() {
    runtime().block_on(async {
        let world = world();
        let key = world
            .issue_key("daily budget probe", json!({"daily_cost_limit": "0.01"}))
            .await
            .expect("budgeted key");
        let checkpoint = world.mock.checkpoint();
        let request = || {
            json!({
                "model": world::OPENAI_ROUTE,
                "messages": [{"role": "user", "content": nonce("daily-budget")}]
            })
        };

        let first = world
            .gateway_post("/openai/v1/chat/completions", request(), &key.secret)
            .await
            .expect("first budgeted request");
        assert_eq!(first.status, 200, "first request: {}", first.text);
        let budget = await_budget(&key.id, 0, true).await;
        let limit = budget["budget"]["daily"]["limit"]
            .as_str()
            .and_then(|value| Decimal::from_str_exact(value).ok());
        assert_eq!(limit, Some(Decimal::new(1, 2)));
        await_daily_budget_rejection(&key.id, Decimal::new(1, 2)).await;

        let second = world
            .gateway_post("/openai/v1/chat/completions", request(), &key.secret)
            .await
            .expect("second budgeted request");
        assert_eq!(second.status, 429, "second request: {}", second.text);
        let error = second.json();
        assert_eq!(error["error"]["code"], json!("budget_exhausted"));
        assert_eq!(
            error["error"]["message"],
            json!("The API key cost budget was exhausted. Unpriced attempts accrue 0.")
        );
        let retry_after = second
            .header("retry-after")
            .expect("budget rejection carries Retry-After")
            .parse::<u64>()
            .expect("Retry-After is a delay in seconds");
        assert!((1..=86_400).contains(&retry_after));
        assert_eq!(world.mock.since(checkpoint).len(), 1);
    });
}

#[test]
#[ignore = "end-to-end; run via make e2e"]
fn unpriced_attempts_accrue_zero_and_never_exhaust_the_budget() {
    runtime().block_on(async {
        let world = world();
        let key = world
            .issue_key(
                "unpriced budget probe",
                json!({"daily_cost_limit": "0.000000000001"}),
            )
            .await
            .expect("budgeted key");

        for attempt in 1..=2 {
            let response = world
                .gateway_post(
                    "/openai/v1/chat/completions",
                    json!({
                        "model": world::OPENAI_ROUTE,
                        "messages": [{
                            "role": "user",
                            "content": format!("{} {}", mock_upstream::NO_USAGE_MARKER, nonce("unpriced-budget"))
                        }]
                    }),
                    &key.secret,
                )
                .await
                .expect("unpriced request");
            assert_eq!(response.status, 200, "attempt {attempt}: {}", response.text);
            let budget = await_budget(&key.id, attempt, false).await;
            let accrued = budget["budget"]["daily"]["accrued"]
                .as_str()
                .and_then(|value| Decimal::from_str_exact(value).ok());
            assert_eq!(accrued, Some(Decimal::ZERO));
            await_unpriced_cost_counter(&key.id, attempt).await;
        }
    });
}
