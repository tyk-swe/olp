use super::*;
use crate::worker_recovery::{await_metadata_quiescence, metadata_processed, usage_fact_count};
pub(crate) async fn prove_shared_valkey_isolation(
    installation_a: &World,
    installation_b: &World,
) -> Result<Vec<String>, String> {
    let valkey_url_a = installation_a.valkey_url().await?;
    let valkey_url_b = installation_b.valkey_url().await?;
    crate::require!(
        valkey_url_a == valkey_url_b,
        "installations did not receive the exact same Valkey URL"
    );
    let prefix_a = installation_prefix(&installation_a.database_url).await?;
    let prefix_b = installation_prefix(&installation_b.database_url).await?;
    crate::require!(
        prefix_a != prefix_b,
        "durable installation namespaces collide"
    );

    let channel_a = format!("{prefix_a}:runtime");
    let channel_b = format!("{prefix_b}:runtime");
    let stream_a = format!("{prefix_a}:request-metadata");
    let stream_b = format!("{prefix_b}:request-metadata");
    crate::require!(channel_a != channel_b, "runtime hint channels collide");
    crate::require!(stream_a != stream_b, "request metadata streams collide");

    let client = redis::Client::open(valkey_url_a.clone())
        .map_err(|error| format!("invalid shared Valkey URL: {error}"))?;
    let mut pubsub = client
        .get_async_pubsub()
        .await
        .map_err(|error| format!("failed to connect shared Valkey Pub/Sub: {error}"))?;
    pubsub
        .subscribe(&channel_a)
        .await
        .map_err(|error| format!("failed to subscribe to installation A hints: {error}"))?;
    pubsub
        .subscribe(&channel_b)
        .await
        .map_err(|error| format!("failed to subscribe to installation B hints: {error}"))?;
    let mut hints = pubsub.on_message();

    let b_generation_before = latest_runtime_generation(&installation_b.database_url).await?;
    let b_processed_before =
        await_metadata_quiescence(installation_b, Duration::from_secs(30)).await?;
    let key_a = installation_a
        .issue_key("shared Valkey isolation mutation", json!({}))
        .await?;

    let hint_deadline = Instant::now() + Duration::from_secs(10);
    loop {
        let remaining = hint_deadline.saturating_duration_since(Instant::now());
        let message = tokio::time::timeout(remaining, hints.next())
            .await
            .map_err(|_| "installation A published no runtime hint within 10s".to_owned())?
            .ok_or_else(|| "shared Valkey Pub/Sub stream ended".to_owned())?;
        let channel = message.get_channel_name();
        let payload: Value = serde_json::from_slice(message.get_payload_bytes())
            .map_err(|error| format!("runtime hint payload was invalid: {error}"))?;
        let generation_id = payload["generation_id"]
            .as_str()
            .ok_or_else(|| format!("runtime hint lacks generation ID: {payload}"))?;
        if generation_id != key_a.generation_id {
            continue;
        }
        crate::require!(
            channel == channel_a,
            "installation A mutation published on installation B channel"
        );
        break;
    }

    let response = installation_a
        .gateway_post(
            "/openai/v1/chat/completions",
            json!({
                "model": OPENAI_ROUTE,
                "messages": [{"role": "user", "content": "shared Valkey isolation"}]
            }),
            &key_a.secret,
        )
        .await?;
    crate::require!(
        response.status == 200,
        "installation A inference failed with {}: {}",
        response.status,
        response.text
    );
    installation_a
        .await_request_rows(&key_a.id, &format!("&route={OPENAI_ROUTE}"), 1)
        .await?;

    crate::require!(
        latest_runtime_generation(&installation_b.database_url).await? == b_generation_before,
        "installation A mutation changed installation B runtime state"
    );
    let b_processed_after = metadata_processed(&installation_b.database_url).await?;
    crate::require!(
        b_processed_after == b_processed_before,
        "installation B consumed or acknowledged installation A request metadata \
         (processed {b_processed_before} -> {b_processed_after})"
    );
    crate::require!(
        usage_fact_count(&installation_b.database_url).await? == 0,
        "installation A usage was attributed to installation B"
    );

    let same_lookup = "identical_lookup_01";
    let limiter_a = DistributedLimiter::connect(&valkey_url_a, format!("{prefix_a}:limits"))
        .await
        .map_err(|error| format!("installation A limiter failed to connect: {error}"))?;
    let limiter_b = DistributedLimiter::connect(&valkey_url_b, format!("{prefix_b}:limits"))
        .await
        .map_err(|error| format!("installation B limiter failed to connect: {error}"))?;
    let limit_request = || LimitRequest {
        api_key_id: uuid::Uuid::nil(),
        lookup_id: same_lookup,
        requests_per_minute: Some(1),
        tokens_per_minute: Some(10),
        max_concurrency: Some(1),
        daily_cost_limit: None,
        monthly_cost_limit: None,
        requested_tokens: 10,
        lease_ttl: Duration::from_secs(60),
    };
    limiter_a
        .reserve(limit_request())
        .await
        .map_err(|error| format!("installation A initial reservation failed: {error}"))?;
    limiter_b
        .reserve(limit_request())
        .await
        .map_err(|error| format!("installation B state was contaminated by A: {error}"))?;
    crate::require!(
        matches!(
            limiter_a.reserve(limit_request()).await,
            Err(LimitError::Exceeded { .. })
        ),
        "installation A did not enforce its own exhausted RPM/TPM/concurrency state"
    );
    crate::require!(
        matches!(
            limiter_b.reserve(limit_request()).await,
            Err(LimitError::Exceeded { .. })
        ),
        "installation B did not enforce its own exhausted RPM/TPM/concurrency state"
    );

    let keys_b = durable_valkey_keys(&valkey_url_b, &format!("{prefix_b}:*")).await?;
    crate::require!(
        !keys_b.is_empty(),
        "installation B created no durable namespaced Valkey keys"
    );
    Ok(keys_b)
}

pub(crate) async fn installation_prefix(database_url: &str) -> Result<String, String> {
    let mut database = sqlx::PgConnection::connect(database_url)
        .await
        .map_err(|error| format!("failed to connect to installation database: {error}"))?;
    let id: String =
        sqlx::query_scalar("SELECT id::text FROM installation_identity WHERE singleton")
            .fetch_one(&mut database)
            .await
            .map_err(|error| format!("failed to read installation identity: {error}"))?;
    Ok(format!("olp:v3:{id}"))
}

pub(crate) async fn latest_runtime_generation(database_url: &str) -> Result<i64, String> {
    let mut database = sqlx::PgConnection::connect(database_url)
        .await
        .map_err(|error| format!("failed to connect to installation database: {error}"))?;
    sqlx::query_scalar::<_, Option<i64>>("SELECT max(sequence) FROM runtime_generations")
        .fetch_one(&mut database)
        .await
        .map_err(|error| format!("failed to read runtime generation: {error}"))?
        .ok_or_else(|| "installation has no runtime generation".to_owned())
}

pub(crate) async fn durable_valkey_keys(url: &str, pattern: &str) -> Result<Vec<String>, String> {
    let client =
        redis::Client::open(url).map_err(|error| format!("invalid shared Valkey URL: {error}"))?;
    let mut connection = client
        .get_multiplexed_async_connection()
        .await
        .map_err(|error| format!("failed to connect shared Valkey: {error}"))?;
    let keys: Vec<String> = connection
        .keys(pattern)
        .await
        .map_err(|error| format!("failed to list shared Valkey keys: {error}"))?;
    let mut keys_with_pttl = Vec::with_capacity(keys.len());
    for key in keys {
        let pttl: i64 = connection
            .pttl(&key)
            .await
            .map_err(|error| format!("failed to inspect installation B key {key}: {error}"))?;
        keys_with_pttl.push((key, pttl));
    }
    Ok(keys_with_pttl
        .into_iter()
        .filter_map(|(key, pttl)| (pttl == -1).then_some(key))
        .collect())
}

pub(crate) async fn assert_valkey_keys_exist(url: &str, keys: &[String]) -> Result<(), String> {
    let client =
        redis::Client::open(url).map_err(|error| format!("invalid shared Valkey URL: {error}"))?;
    let mut connection = client
        .get_multiplexed_async_connection()
        .await
        .map_err(|error| format!("failed to reconnect shared Valkey: {error}"))?;
    let checked_keys = keys.join(", ");
    for key in keys {
        let exists: bool = connection
            .exists(key)
            .await
            .map_err(|error| format!("failed to inspect installation B key {key}: {error}"))?;
        crate::require!(
            exists,
            "installation A teardown deleted installation B durable key {key}; \
             durable keys checked: {checked_keys}"
        );
    }
    Ok(())
}
