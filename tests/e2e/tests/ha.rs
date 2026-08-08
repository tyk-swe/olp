//! Two-gateway convergence and degraded-dependency proof.

#[allow(dead_code)]
#[path = "contract/harness.rs"]
mod harness;
#[allow(dead_code)]
#[path = "contract/mock_upstream.rs"]
mod mock_upstream;
#[allow(dead_code)]
#[path = "contract/world.rs"]
mod world;

use std::time::{Duration, Instant};

use futures::StreamExt as _;
use olp_storage::limits::{DistributedLimiter, LimitError, LimitRequest};
use redis::{
    AsyncCommands as _,
    streams::{StreamInfoGroupsReply, StreamPendingCountReply, StreamReadOptions, StreamReadReply},
};
use serde_json::{Value, json};
use sqlx::Connection as _;

use harness::{GatewayProcess, Server, SharedValkey};
use world::{IssuedKey, Management, OPENAI_ROUTE, World};

macro_rules! require {
    ($condition:expr, $($message:tt)*) => {
        if !$condition {
            return Err(format!($($message)*));
        }
    };
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
#[ignore = "high-availability; run through scripts/run-e2e-tests.sh"]
async fn two_gateways_converge_and_degrade_safely() -> Result<(), String> {
    let (world, gateway) = world::bootstrap_ha().await?;
    let result = exercise(&world, &gateway).await;
    let logs = world.shutdown().await;
    result.map_err(|error| format!("{error}\nserver logs:\n{logs}"))
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
#[ignore = "shared-Valkey qualification; run through scripts/run-e2e-tests.sh"]
async fn worker_ha_shared_valkey_installations_are_isolated() -> Result<(), String> {
    let valkey = SharedValkey::reserve().await?;
    let installation_a = match world::bootstrap_sharing_valkey(valkey.url()).await {
        Ok(world) => world,
        Err(error) => {
            valkey.release().await;
            return Err(error);
        }
    };
    let installation_b = match world::bootstrap_sharing_valkey(valkey.url()).await {
        Ok(world) => world,
        Err(error) => {
            let logs = installation_a.shutdown().await;
            valkey.release().await;
            return Err(format!("{error}\ninstallation A logs:\n{logs}"));
        }
    };

    let result = prove_shared_valkey_isolation(&installation_a, &installation_b).await;
    let logs_a = installation_a.shutdown().await;
    let teardown_result = match &result {
        Ok(keys_b) => assert_valkey_keys_exist(valkey.url(), keys_b).await,
        Err(_) => Ok(()),
    };
    let logs_b = installation_b.shutdown().await;
    valkey.release().await;

    result.map(|_| ()).and(teardown_result).map_err(|error| {
        format!("{error}\ninstallation A logs:\n{logs_a}\ninstallation B logs:\n{logs_b}")
    })
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
#[ignore = "legacy namespace qualification; run through scripts/run-e2e-tests.sh"]
async fn worker_ha_migrate_preserves_legacy_stream_ownership() -> Result<(), String> {
    const LEGACY: &str = "olp:v2:request-metadata";
    const GROUP: &str = "olp:persistence";
    let valkey = SharedValkey::reserve().await?;
    let client = redis::Client::open(valkey.url())
        .map_err(|error| format!("invalid shared Valkey URL: {error}"))?;
    let mut connection = client
        .get_multiplexed_async_connection()
        .await
        .map_err(|error| format!("failed to connect shared Valkey: {error}"))?;
    let event_id: String = redis::cmd("XADD")
        .arg(LEGACY)
        .arg("*")
        .arg("event")
        .arg("legacy-pending-event")
        .query_async(&mut connection)
        .await
        .map_err(|error| format!("failed to seed legacy stream: {error}"))?;
    let _: String = redis::cmd("XGROUP")
        .arg("CREATE")
        .arg(LEGACY)
        .arg(GROUP)
        .arg("0")
        .query_async(&mut connection)
        .await
        .map_err(|error| format!("failed to seed legacy consumer group: {error}"))?;
    let delivered: StreamReadReply = connection
        .xread_options(
            &[LEGACY],
            &[">"],
            &StreamReadOptions::default().group(GROUP, "legacy-owner"),
        )
        .await
        .map_err(|error| format!("failed to establish legacy ownership: {error}"))?;
    require!(
        delivered
            .keys
            .iter()
            .flat_map(|stream| &stream.ids)
            .any(|entry| entry.id == event_id),
        "legacy event was not delivered into the pending-entry list"
    );

    let server = match Server::launch_control_sharing_valkey(valkey.url()).await {
        Ok(server) => server,
        Err(error) => {
            valkey.release().await;
            return Err(error);
        }
    };
    let result = async {
        let target = format!(
            "{}:request-metadata",
            installation_prefix(&server.database_url).await?
        );
        let legacy_exists: bool = connection
            .exists(LEGACY)
            .await
            .map_err(|error| format!("failed to inspect legacy stream: {error}"))?;
        let target_exists: bool = connection
            .exists(&target)
            .await
            .map_err(|error| format!("failed to inspect namespaced stream: {error}"))?;
        require!(!legacy_exists, "migration left the legacy stream present");
        require!(
            target_exists,
            "migration did not create the namespaced stream"
        );
        let groups: StreamInfoGroupsReply = connection
            .xinfo_groups(&target)
            .await
            .map_err(|error| format!("failed to inspect migrated consumer group: {error}"))?;
        let group = groups
            .groups
            .iter()
            .find(|group| group.name == GROUP)
            .ok_or_else(|| "migration lost the legacy consumer group".to_owned())?;
        require!(group.pending == 1, "migration lost pending-entry state");
        let pending: StreamPendingCountReply = connection
            .xpending_count(&target, GROUP, "-", "+", 10)
            .await
            .map_err(|error| format!("failed to inspect migrated ownership: {error}"))?;
        require!(
            pending
                .ids
                .iter()
                .any(|entry| { entry.id == event_id && entry.consumer == "legacy-owner" }),
            "migration changed pending event identity or ownership"
        );
        Ok::<(), String>(())
    }
    .await;
    let logs = server.shutdown().await;
    valkey.release().await;
    result.map_err(|error| format!("{error}\ncontrol logs:\n{logs}"))
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
#[ignore = "worker HA qualification; run through scripts/run-e2e-tests.sh"]
async fn worker_ha_three_workers_recover_owned_metadata_and_outbox_work() -> Result<(), String> {
    let (world, workers) = world::bootstrap_worker_ha().await?;
    let result = prove_three_worker_recovery(&world, &workers).await;
    let logs = world.shutdown().await;
    result.map_err(|error| format!("{error}\nworker HA process logs:\n{logs}"))
}

async fn prove_three_worker_recovery(
    world: &World,
    workers: &[harness::WorkerProcess; 3],
) -> Result<(), String> {
    let response = world
        .gateway_post(
            "/openai/v1/chat/completions",
            json!({
                "model": OPENAI_ROUTE,
                "messages": [{"role": "user", "content": "owned metadata replay"}]
            }),
            &world.api_key,
        )
        .await?;
    require!(
        response.status == 200,
        "request used to create owned metadata failed with {}: {}",
        response.status,
        response.text
    );
    await_file(&workers[0].ownership_marker, Duration::from_secs(30)).await?;
    await_outbox_pending(world, 0, Duration::from_secs(15)).await?;
    world.hard_kill_worker(&workers[0]).await?;

    world.release_worker(&workers[1])?;
    await_metadata_recovery(world, &world.api_key_id, Duration::from_secs(45)).await?;

    let crash_second = async {
        await_file(&workers[1].ownership_marker, Duration::from_secs(30)).await?;
        world.hard_kill_worker(&workers[1]).await?;
        world.release_worker(&workers[2])?;
        Ok::<(), String>(())
    };
    let (takeover_key, crash_result) = tokio::join!(
        world.issue_key("outbox takeover after hard termination", json!({})),
        crash_second
    );
    crash_result?;
    let takeover_key = takeover_key?;

    await_healthy_recovered_workers(world, Duration::from_secs(30)).await?;
    let replayed_rows = world
        .await_request_rows(&world.api_key_id, &format!("&route={OPENAI_ROUTE}"), 1)
        .await?;
    require!(
        replayed_rows.len() == 1,
        "replay created {} logical request rows instead of one",
        replayed_rows.len()
    );
    require!(
        usage_facts_for_key(&world.database_url, &world.api_key_id).await? == 1,
        "replay did not preserve exactly one logical usage fact"
    );

    let continued = world
        .gateway_post(
            "/openai/v1/chat/completions",
            json!({
                "model": OPENAI_ROUTE,
                "messages": [{"role": "user", "content": "work after recovery"}]
            }),
            &takeover_key.secret,
        )
        .await?;
    require!(
        continued.status == 200,
        "surviving worker topology stopped serving new work: {} {}",
        continued.status,
        continued.text
    );
    world
        .await_request_rows(&takeover_key.id, &format!("&route={OPENAI_ROUTE}"), 1)
        .await?;
    require!(
        usage_facts_for_key(&world.database_url, &takeover_key.id).await? == 1,
        "new work after recovery did not produce exactly one usage fact"
    );
    await_healthy_recovered_workers(world, Duration::from_secs(15)).await
}

async fn await_file(path: &std::path::Path, timeout: Duration) -> Result<(), String> {
    let deadline = Instant::now() + timeout;
    loop {
        if path.exists() {
            return Ok(());
        }
        if Instant::now() >= deadline {
            return Err(format!(
                "worker did not reach ownership boundary {} within {timeout:?}",
                path.display()
            ));
        }
        tokio::time::sleep(Duration::from_millis(25)).await;
    }
}

async fn await_outbox_pending(
    world: &World,
    expected: i64,
    timeout: Duration,
) -> Result<(), String> {
    let deadline = Instant::now() + timeout;
    loop {
        let pending = database_scalar(
            &world.database_url,
            "SELECT count(*)::bigint FROM transactional_outbox WHERE published_at IS NULL",
        )
        .await?;
        if pending == expected {
            return Ok(());
        }
        if Instant::now() >= deadline {
            return Err(format!(
                "runtime outbox pending count stayed at {pending}, expected {expected}"
            ));
        }
        tokio::time::sleep(Duration::from_millis(100)).await;
    }
}

async fn await_metadata_recovery(
    world: &World,
    api_key_id: &str,
    timeout: Duration,
) -> Result<(), String> {
    let deadline = Instant::now() + timeout;
    loop {
        let mut database = sqlx::PgConnection::connect(&world.database_url)
            .await
            .map_err(|error| format!("failed to inspect metadata recovery: {error}"))?;
        let (usage_facts, recovered, pending, lag): (i64, i64, Option<i64>, Option<i64>) =
            sqlx::query_as(
                "SELECT \
                   (SELECT count(*)::bigint FROM usage_facts WHERE api_key_id = $1::uuid), \
                   request_metadata_recovered_total, \
                   (SELECT pending_events FROM request_metadata_consumer_health WHERE singleton), \
                   (SELECT lag_events FROM request_metadata_consumer_health WHERE singleton) \
                 FROM async_worker_counters WHERE singleton",
            )
            .bind(api_key_id)
            .fetch_one(&mut database)
            .await
            .map_err(|error| format!("failed to read metadata recovery state: {error}"))?;
        if usage_facts == 1 && recovered >= 1 && pending == Some(0) && lag == Some(0) {
            return Ok(());
        }
        if Instant::now() >= deadline {
            return Err(format!(
                "metadata recovery did not converge: usage={usage_facts}, recovered={recovered}, pending={pending:?}, lag={lag:?}"
            ));
        }
        tokio::time::sleep(Duration::from_millis(200)).await;
    }
}

async fn await_healthy_recovered_workers(world: &World, timeout: Duration) -> Result<(), String> {
    let deadline = Instant::now() + timeout;
    loop {
        let mut database = sqlx::PgConnection::connect(&world.database_url)
            .await
            .map_err(|error| format!("failed to inspect worker health: {error}"))?;
        let state: (i64, i64, i64, i64, i64, bool, i64, Option<i64>, Option<i64>) = sqlx::query_as(
            "SELECT \
                   (SELECT count(*)::bigint FROM transactional_outbox WHERE published_at IS NULL), \
                   runtime_outbox_repeated_attempts_total, \
                   runtime_outbox_abandoned_ownership_total, \
                   runtime_outbox_abandoned_claims_total, \
                   runtime_outbox_published_total, \
                   (SELECT owner_active FROM runtime_outbox_health WHERE singleton), \
                   (SELECT claimed_rows FROM runtime_outbox_health WHERE singleton), \
                   (SELECT pending_events FROM request_metadata_consumer_health WHERE singleton), \
                   (SELECT lag_events FROM request_metadata_consumer_health WHERE singleton) \
                 FROM async_worker_counters WHERE singleton",
        )
        .fetch_one(&mut database)
        .await
        .map_err(|error| format!("failed to read worker recovery state: {error}"))?;
        let healthy_tasks: i64 = sqlx::query_scalar(
            "SELECT count(*)::bigint FROM worker_task_health \
             WHERE last_success_at IS NOT NULL AND \
               last_success_at >= clock_timestamp() - \
                 CASE WHEN task = 'maintenance' THEN interval '180 seconds' ELSE interval '20 seconds' END",
        )
        .fetch_one(&mut database)
        .await
        .map_err(|error| format!("failed to read worker checkpoints: {error}"))?;
        if state.0 == 0
            && state.1 >= 1
            && state.2 >= 1
            && state.3 >= 1
            && state.4 >= 1
            && state.5
            && state.6 == 0
            && state.7 == Some(0)
            && state.8 == Some(0)
            && healthy_tasks == 4
        {
            return Ok(());
        }
        if Instant::now() >= deadline {
            return Err(format!(
                "worker recovery did not converge: state={state:?}, healthy_tasks={healthy_tasks}"
            ));
        }
        tokio::time::sleep(Duration::from_millis(200)).await;
    }
}

async fn usage_facts_for_key(database_url: &str, api_key_id: &str) -> Result<i64, String> {
    let mut database = sqlx::PgConnection::connect(database_url)
        .await
        .map_err(|error| format!("failed to connect for usage assertion: {error}"))?;
    sqlx::query_scalar("SELECT count(*)::bigint FROM usage_facts WHERE api_key_id = $1::uuid")
        .bind(api_key_id)
        .fetch_one(&mut database)
        .await
        .map_err(|error| format!("failed to count logical usage facts: {error}"))
}

async fn prove_shared_valkey_isolation(
    installation_a: &World,
    installation_b: &World,
) -> Result<Vec<String>, String> {
    require!(
        installation_a.valkey_url()? == installation_b.valkey_url()?,
        "installations did not receive the exact same Valkey URL"
    );
    let prefix_a = installation_prefix(&installation_a.database_url).await?;
    let prefix_b = installation_prefix(&installation_b.database_url).await?;
    require!(
        prefix_a != prefix_b,
        "durable installation namespaces collide"
    );

    let channel_a = format!("{prefix_a}:runtime");
    let channel_b = format!("{prefix_b}:runtime");
    let stream_a = format!("{prefix_a}:request-metadata");
    let stream_b = format!("{prefix_b}:request-metadata");
    require!(channel_a != channel_b, "runtime hint channels collide");
    require!(stream_a != stream_b, "request metadata streams collide");

    let client = redis::Client::open(installation_a.valkey_url()?)
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
    while tokio::time::timeout(Duration::from_millis(100), hints.next())
        .await
        .is_ok()
    {}

    let b_generation_before = latest_runtime_generation(&installation_b.database_url).await?;
    let b_processed_before = metadata_processed(&installation_b.database_url).await?;
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
        require!(
            channel != channel_b,
            "installation A mutation published on installation B channel"
        );
        if channel == channel_a {
            break;
        }
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
    require!(
        response.status == 200,
        "installation A inference failed with {}: {}",
        response.status,
        response.text
    );
    installation_a
        .await_request_rows(&key_a.id, &format!("&route={OPENAI_ROUTE}"), 1)
        .await?;

    require!(
        latest_runtime_generation(&installation_b.database_url).await? == b_generation_before,
        "installation A mutation changed installation B runtime state"
    );
    require!(
        metadata_processed(&installation_b.database_url).await? == b_processed_before,
        "installation B consumed or acknowledged installation A request metadata"
    );
    require!(
        usage_fact_count(&installation_b.database_url).await? == 0,
        "installation A usage was attributed to installation B"
    );

    let same_lookup = "identical_lookup_01";
    let limiter_a =
        DistributedLimiter::connect(&installation_a.valkey_url()?, format!("{prefix_a}:limits"))
            .await
            .map_err(|error| format!("installation A limiter failed to connect: {error}"))?;
    let limiter_b =
        DistributedLimiter::connect(&installation_b.valkey_url()?, format!("{prefix_b}:limits"))
            .await
            .map_err(|error| format!("installation B limiter failed to connect: {error}"))?;
    let limit_request = || LimitRequest {
        lookup_id: same_lookup,
        requests_per_minute: Some(1),
        tokens_per_minute: Some(10),
        max_concurrency: Some(1),
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
    require!(
        matches!(
            limiter_a.reserve(limit_request()).await,
            Err(LimitError::Exceeded { .. })
        ),
        "installation A did not enforce its own exhausted RPM/TPM/concurrency state"
    );
    require!(
        matches!(
            limiter_b.reserve(limit_request()).await,
            Err(LimitError::Exceeded { .. })
        ),
        "installation B did not enforce its own exhausted RPM/TPM/concurrency state"
    );

    let keys_b = valkey_keys(
        installation_b.valkey_url()?.as_str(),
        &format!("{prefix_b}:*"),
    )
    .await?;
    require!(
        !keys_b.is_empty(),
        "installation B created no namespaced Valkey keys"
    );
    Ok(keys_b)
}

async fn installation_prefix(database_url: &str) -> Result<String, String> {
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

async fn latest_runtime_generation(database_url: &str) -> Result<i64, String> {
    let mut database = sqlx::PgConnection::connect(database_url)
        .await
        .map_err(|error| format!("failed to connect to installation database: {error}"))?;
    sqlx::query_scalar::<_, Option<i64>>("SELECT max(sequence) FROM runtime_generations")
        .fetch_one(&mut database)
        .await
        .map_err(|error| format!("failed to read runtime generation: {error}"))?
        .ok_or_else(|| "installation has no runtime generation".to_owned())
}

async fn metadata_processed(database_url: &str) -> Result<i64, String> {
    database_scalar(
        database_url,
        "SELECT request_metadata_processed_total FROM async_worker_counters WHERE singleton",
    )
    .await
}

async fn usage_fact_count(database_url: &str) -> Result<i64, String> {
    database_scalar(database_url, "SELECT count(*)::bigint FROM usage_facts").await
}

async fn database_scalar(database_url: &str, query: &'static str) -> Result<i64, String> {
    let mut database = sqlx::PgConnection::connect(database_url)
        .await
        .map_err(|error| format!("failed to connect to installation database: {error}"))?;
    sqlx::query_scalar(query)
        .fetch_one(&mut database)
        .await
        .map_err(|error| format!("database assertion failed: {error}"))
}

async fn valkey_keys(url: &str, pattern: &str) -> Result<Vec<String>, String> {
    let client =
        redis::Client::open(url).map_err(|error| format!("invalid shared Valkey URL: {error}"))?;
    let mut connection = client
        .get_multiplexed_async_connection()
        .await
        .map_err(|error| format!("failed to connect shared Valkey: {error}"))?;
    connection
        .keys(pattern)
        .await
        .map_err(|error| format!("failed to list shared Valkey keys: {error}"))
}

async fn assert_valkey_keys_exist(url: &str, keys: &[String]) -> Result<(), String> {
    let client =
        redis::Client::open(url).map_err(|error| format!("invalid shared Valkey URL: {error}"))?;
    let mut connection = client
        .get_multiplexed_async_connection()
        .await
        .map_err(|error| format!("failed to reconnect shared Valkey: {error}"))?;
    for key in keys {
        let exists: bool = connection
            .exists(key)
            .await
            .map_err(|error| format!("failed to inspect installation B key {key}: {error}"))?;
        require!(
            exists,
            "installation A teardown deleted installation B key {key}"
        );
    }
    Ok(())
}

async fn exercise(world: &World, gateway: &GatewayProcess) -> Result<(), String> {
    let endpoints = [
        (&world.public_origin, &world.observability_base),
        (&gateway.public_origin, &gateway.observability_base),
    ];
    let public = [endpoints[0].0.as_str(), endpoints[1].0.as_str()];
    let observability = [endpoints[0].1.as_str(), endpoints[1].1.as_str()];
    let five_seconds = Duration::from_secs(5);
    let http = reqwest::Client::builder()
        .timeout(Duration::from_secs(5))
        .build()
        .map_err(|error| format!("failed to build HA client: {error}"))?;

    for origin in public {
        for path in ["/health/live", "/health/ready", "/metrics"] {
            let status = http
                .get(format!("{origin}{path}"))
                .send()
                .await
                .map_err(|error| format!("public observability probe failed: {error}"))?
                .status();
            require!(status.as_u16() == 404, "{origin}{path} returned {status}");
        }
    }

    let hard = issue_key(
        world,
        &http,
        public[1],
        "HA hard limit",
        json!({
            "allowed_routes": [OPENAI_ROUTE],
            "requests_per_minute": 4
        }),
    )
    .await?;

    let fast = issue_key(
        world,
        &http,
        public[1],
        "HA convergence probe",
        json!({
            "scopes": ["models_read"],
            "allowed_routes": [OPENAI_ROUTE]
        }),
    )
    .await?;
    let started = Instant::now();
    revoke_key(&world.management, &fast).await?;
    await_keys(&http, &public, &fast, 401, five_seconds).await?;
    require!(
        started.elapsed() <= five_seconds,
        "healthy revocation convergence exceeded five seconds"
    );

    let chat = json!({
        "model": OPENAI_ROUTE,
        "messages": [{"role": "user", "content": "HA limiter probe"}],
        "max_tokens": 1
    });
    for public in &public {
        let status = gateway_status(&http, public, &hard.secret, Some(&chat)).await?;
        require!(
            status != 429,
            "shared RPM denied before the fourth admitted request"
        );
    }
    let status = gateway_status(&http, public[0], &hard.secret, Some(&chat)).await?;
    require!(
        status == 429,
        "shared RPM was not atomic across gateways (last status {status})"
    );

    let lkg = issue_key(
        world,
        &http,
        public[1],
        "HA last-known-good probe",
        json!({
            "scopes": ["models_read"],
            "allowed_routes": [OPENAI_ROUTE]
        }),
    )
    .await?;
    let corrupt_sequence = publish_corrupt_generation(&world.database_url).await?;
    require!(
        corrupt_sequence > lkg.generation,
        "corrupt generation did not advance the runtime sequence"
    );
    tokio::time::sleep(Duration::from_millis(5_200)).await;
    for public in &public {
        let status = gateway_status(&http, public, &lkg.secret, None).await?;
        require!(
            status == 200,
            "{public} abandoned its last-known-good generation (status {status})"
        );
    }
    wait_readiness(&http, &observability, &[], five_seconds).await?;

    let soft = issue_key(
        world,
        &http,
        public[1],
        "HA no hard limits",
        json!({"allowed_routes": [OPENAI_ROUTE]}),
    )
    .await?;
    require!(
        soft.generation > corrupt_sequence,
        "valid generation did not supersede the corrupt release"
    );

    let toxiproxy = std::env::var("OLP_E2E_TOXIPROXY_API")
        .map_err(|_| "OLP_E2E_TOXIPROXY_API is required by the HA target".to_owned())?;
    if let Ok(database_proxy) = std::env::var("OLP_E2E_DATABASE_PROXY_NAME") {
        set_proxy(&http, &toxiproxy, &database_proxy, false).await?;
        let outage = async {
            wait_readiness(
                &http,
                &observability,
                &[("database", "unavailable_lkg")],
                Duration::from_secs(30),
            )
            .await?;
            for public in &public {
                require!(
                    gateway_status(&http, public, &soft.secret, None).await? == 200,
                    "{public} stopped LKG traffic during database outage"
                );
            }
            Ok(())
        }
        .await;
        set_proxy(&http, &toxiproxy, &database_proxy, true).await?;
        outage?;
        wait_readiness(
            &http,
            &observability,
            &[("database", "ok")],
            Duration::from_secs(20),
        )
        .await?;
    }

    let valkey_proxy = std::env::var("OLP_E2E_VALKEY_PROXY_NAME")
        .map_err(|_| "OLP_E2E_VALKEY_PROXY_NAME is required by the HA target".to_owned())?;
    set_proxy(&http, &toxiproxy, &valkey_proxy, false).await?;
    let outage = async {
        wait_readiness(
            &http,
            &observability,
            &[("status", "degraded"), ("limits", "unavailable")],
            Duration::from_secs(15),
        )
        .await?;
        let hard_status = gateway_status(&http, public[0], &hard.secret, Some(&chat)).await?;
        require!(
            hard_status == 503,
            "hard-limited key did not fail closed during Valkey outage ({hard_status})"
        );
        let soft_status = gateway_status(&http, public[0], &soft.secret, Some(&chat)).await?;
        require!(
            soft_status != 503,
            "unlimited key failed closed during Valkey outage"
        );

        let started = Instant::now();
        revoke_key(&world.management, &soft).await?;
        await_keys(&http, &public, &soft, 401, Duration::from_millis(5_500)).await?;
        require!(
            started.elapsed() <= Duration::from_millis(5_500),
            "missed-hint revocation convergence exceeded 5.5 seconds"
        );
        Ok(())
    }
    .await;
    set_proxy(&http, &toxiproxy, &valkey_proxy, true).await?;
    outage
}

async fn issue_key(
    world: &World,
    http: &reqwest::Client,
    second_gateway: &str,
    name: &str,
    overrides: Value,
) -> Result<IssuedKey, String> {
    let key = world.issue_key(name, overrides).await?;
    let timeout = Duration::from_secs(5);
    await_keys(http, &[second_gateway], &key, 200, timeout).await?;
    require!(
        key.published_at.elapsed() <= timeout,
        "runtime generation {} did not converge within five seconds",
        key.generation
    );
    Ok(key)
}

async fn revoke_key(management: &Management, key: &IssuedKey) -> Result<(), String> {
    management
        .expect(
            reqwest::Method::POST,
            &format!("/api/v1/api-keys/{}/revoke", key.id),
            None,
            None,
            Some(&key.etag),
            200,
        )
        .await?;
    Ok(())
}

async fn await_keys(
    http: &reqwest::Client,
    origins: &[&str],
    key: &IssuedKey,
    expected: u16,
    timeout: Duration,
) -> Result<(), String> {
    for origin in origins {
        let deadline = Instant::now() + timeout;
        loop {
            let status = gateway_status(http, origin, &key.secret, None).await?;
            if status == expected {
                break;
            }
            if Instant::now() >= deadline {
                return Err(format!(
                    "{origin} did not reach {expected} for generation {} (last {status})",
                    key.generation
                ));
            }
            tokio::time::sleep(Duration::from_millis(50)).await;
        }
    }
    Ok(())
}

async fn gateway_status(
    http: &reqwest::Client,
    origin: &str,
    secret: &str,
    body: Option<&Value>,
) -> Result<u16, String> {
    let request = match body {
        Some(body) => http
            .post(format!("{origin}/openai/v1/chat/completions"))
            .json(body),
        None => http.get(format!("{origin}/openai/v1/models")),
    };
    request
        .bearer_auth(secret)
        .send()
        .await
        .map(|response| response.status().as_u16())
        .map_err(|error| format!("gateway request failed: {error}"))
}

async fn publish_corrupt_generation(database: &str) -> Result<i64, String> {
    let mut connection = sqlx::postgres::PgConnection::connect(database)
        .await
        .map_err(|error| format!("failed to connect for corrupt generation proof: {error}"))?;
    let sequence = sqlx::query_scalar(
        "INSERT INTO runtime_generations \
         (id, compiled_release, release_sha256, created_by) \
         SELECT uuidv7(), decode('00','hex'), decode(repeat('00',32),'hex'), id FROM users LIMIT 1 \
         RETURNING sequence",
    )
    .fetch_one(&mut connection)
    .await
    .map_err(|error| format!("failed to publish corrupt generation: {error}"))?;
    connection.close().await.ok();
    Ok(sequence)
}

async fn set_proxy(
    http: &reqwest::Client,
    api: &str,
    proxy: &str,
    available: bool,
) -> Result<(), String> {
    let toxic = format!("{proxy}-reset");
    let url = format!("{api}/proxies/{proxy}/toxics/{toxic}");
    let response = if available {
        http.delete(url).send().await
    } else {
        http.post(format!("{api}/proxies/{proxy}/toxics"))
            .json(&json!({
                "name": toxic,
                "type": "reset_peer",
                "stream": "downstream",
                "toxicity": 1,
                "attributes": {"timeout": 0}
            }))
            .send()
            .await
    }
    .map_err(|error| format!("failed to change Toxiproxy {proxy}: {error}"))?;
    match response.error_for_status() {
        Ok(_) => Ok(()),
        Err(error) if available && error.status() == Some(reqwest::StatusCode::NOT_FOUND) => Ok(()),
        Err(error) => Err(format!("failed to change Toxiproxy {proxy}: {error}")),
    }
}

async fn wait_readiness(
    http: &reqwest::Client,
    origins: &[&str],
    expected: &[(&str, &str)],
    timeout: Duration,
) -> Result<(), String> {
    for origin in origins {
        let deadline = Instant::now() + timeout;
        let url = format!("{origin}/health/ready");
        loop {
            if let Ok(response) = http.get(&url).send().await
                && response.status().is_success()
                && let Ok(body) = response.json::<Value>().await
                && expected
                    .iter()
                    .all(|(field, value)| body[*field].as_str() == Some(*value))
            {
                break;
            }
            if Instant::now() >= deadline {
                return Err(format!(
                    "{url} did not report {expected:?} within {timeout:?}"
                ));
            }
            tokio::time::sleep(Duration::from_millis(100)).await;
        }
    }
    Ok(())
}
