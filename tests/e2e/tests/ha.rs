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

use serde_json::{Value, json};
use sqlx::Connection as _;

use harness::GatewayProcess;
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
