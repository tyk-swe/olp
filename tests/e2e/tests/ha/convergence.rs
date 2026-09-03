use super::*;
pub(crate) async fn exercise(world: &World, gateway: &GatewayProcess) -> Result<(), String> {
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

    prove_trace_continuity(world, &http, &public).await?;

    for origin in public {
        for path in ["/health/live", "/health/ready", "/metrics"] {
            let status = http
                .get(format!("{origin}{path}"))
                .send()
                .await
                .map_err(|error| format!("public observability probe failed: {error}"))?
                .status();
            crate::require!(status.as_u16() == 404, "{origin}{path} returned {status}");
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
    let budgeted = issue_key(
        world,
        &http,
        public[1],
        "HA cost budget",
        json!({
            "allowed_routes": [OPENAI_ROUTE],
            "daily_cost_limit": "1000.00"
        }),
    )
    .await?;
    set_limits_fail_open(&world.management).await?;

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
    crate::require!(
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
        crate::require!(
            status != 429,
            "shared RPM denied before the fourth admitted request"
        );
    }
    let status = gateway_status(&http, public[0], &hard.secret, Some(&chat)).await?;
    crate::require!(
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
    crate::require!(
        corrupt_sequence > lkg.generation,
        "corrupt generation did not advance the runtime sequence"
    );
    tokio::time::sleep(Duration::from_millis(5_200)).await;
    for public in &public {
        let status = gateway_status(&http, public, &lkg.secret, None).await?;
        crate::require!(
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
    crate::require!(
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
                crate::require!(
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
        await_keys(&http, &[public[0]], &hard, 200, Duration::from_secs(20))
            .await
            .map_err(|error| {
                format!("rate-limited key did not follow fail-open policy: {error}")
            })?;
        let budgeted_status =
            gateway_status(&http, public[0], &budgeted.secret, Some(&chat)).await?;
        crate::require!(
            budgeted_status == 503,
            "cost-budgeted key did not remain fail-closed under the general fail-open policy \
             ({budgeted_status})"
        );
        let soft_status = gateway_status(&http, public[0], &soft.secret, Some(&chat)).await?;
        crate::require!(
            soft_status != 503,
            "unlimited key failed closed during Valkey outage"
        );

        let started = Instant::now();
        revoke_key(&world.management, &soft).await?;
        await_keys(&http, &public, &soft, 401, Duration::from_millis(5_500)).await?;
        crate::require!(
            started.elapsed() <= Duration::from_millis(5_500),
            "missed-hint revocation convergence exceeded 5.5 seconds"
        );
        Ok(())
    }
    .await;
    set_proxy(&http, &toxiproxy, &valkey_proxy, true).await?;
    outage
}

pub(crate) async fn set_limits_fail_open(management: &Management) -> Result<(), String> {
    let path = "/api/v1/settings/limits.valkey_unavailable";
    let setting = management.get(path).await?;
    crate::require!(
        setting.status == 200,
        "limits outage setting read returned {}: {}",
        setting.status,
        setting.body
    );
    let etag = setting.require_etag("limits outage setting")?;
    let updated = management
        .expect(
            reqwest::Method::PUT,
            path,
            Some(json!({"value": "fail_open"})),
            None,
            Some(&etag),
            200,
        )
        .await?;
    crate::require!(
        updated.body["value"] == "fail_open",
        "limits outage setting did not retain fail_open: {}",
        updated.body
    );
    Ok(())
}

pub(crate) async fn prove_trace_continuity(
    world: &World,
    http: &reqwest::Client,
    origins: &[&str; 2],
) -> Result<(), String> {
    let inbound = otlp::inbound_trace();
    let chat = json!({
        "model": OPENAI_ROUTE,
        "messages": [{"role": "user", "content": "HA trace continuity"}],
        "max_tokens": 1
    });
    for origin in origins {
        let response = http
            .post(format!("{origin}/openai/v1/chat/completions"))
            .bearer_auth(&world.api_key)
            .header("traceparent", &inbound.header)
            .json(&chat)
            .send()
            .await
            .map_err(|error| format!("HA trace request to {origin} failed: {error}"))?;
        let status = response.status();
        let body = response.text().await.unwrap_or_default();
        crate::require!(
            status.is_success(),
            "HA trace request to {origin} returned {status}: {body}"
        );
    }

    let spans = world
        .otlp()
        .await_trace(&inbound.trace_id, 4, Duration::from_secs(15))
        .await?;
    assert_ha_trace(&spans, &inbound)
}

pub(crate) fn assert_ha_trace(
    spans: &[otlp::CollectedSpan],
    inbound: &otlp::InboundTrace,
) -> Result<(), String> {
    crate::require!(
        spans.len() == 4,
        "two traced gateway requests exported {} spans: {:?}",
        spans.len(),
        spans
            .iter()
            .map(|span| span.span.name.as_str())
            .collect::<Vec<_>>()
    );
    let requests: Vec<_> = spans
        .iter()
        .filter(|span| span.string_attribute("olp.surface").is_some())
        .collect();
    let attempts: Vec<_> = spans
        .iter()
        .filter(|span| span.string_attribute("olp.provider_kind").is_some())
        .collect();
    crate::require!(
        requests.len() == 2 && attempts.len() == 2,
        "HA trace did not contain two request/attempt pairs"
    );
    let request_ids: Vec<&[u8]> = requests
        .iter()
        .map(|request| request.span.span_id.as_slice())
        .collect();
    for request in &requests {
        crate::require!(
            request.span.name == "request",
            "unexpected request span name"
        );
        crate::require!(
            request.span.trace_id == inbound.trace_id,
            "gateway changed the inbound trace ID"
        );
        crate::require!(
            request.span.parent_span_id == inbound.parent_span_id,
            "gateway changed the inbound parent span ID"
        );
    }
    crate::require!(
        requests[0].span.span_id != requests[1].span.span_id,
        "two gateways reused one request span ID"
    );
    let process_modes: std::collections::BTreeSet<_> = requests
        .iter()
        .filter_map(|request| request.resource_attribute("olp.process.mode"))
        .collect();
    crate::require!(
        process_modes == std::collections::BTreeSet::from(["all", "gateway"]),
        "trace did not traverse both gateway process modes: {process_modes:?}"
    );
    for attempt in attempts {
        crate::require!(
            attempt.span.name == "attempt",
            "unexpected attempt span name"
        );
        crate::require!(
            attempt.span.trace_id == inbound.trace_id,
            "provider attempt changed the inbound trace ID"
        );
        crate::require!(
            request_ids.contains(&attempt.span.parent_span_id.as_slice()),
            "provider attempt is not parented by either gateway request span"
        );
    }
    Ok(())
}

pub(crate) async fn issue_key(
    world: &World,
    http: &reqwest::Client,
    second_gateway: &str,
    name: &str,
    overrides: Value,
) -> Result<IssuedKey, String> {
    let budgeted = ["daily_cost_limit", "monthly_cost_limit"]
        .iter()
        .any(|field| overrides.get(*field).is_some_and(|value| !value.is_null()));
    let key = world.issue_key(name, overrides).await?;
    // Budget readiness includes the authoritative snapshot bootstrap interval.
    let timeout = Duration::from_secs(if budgeted { 90 } else { 5 });
    await_keys(http, &[second_gateway], &key, 200, timeout).await?;
    crate::require!(
        key.published_at.elapsed() <= timeout,
        "runtime generation {} did not become ready within {timeout:?}",
        key.generation
    );
    Ok(key)
}

pub(crate) async fn revoke_key(management: &Management, key: &IssuedKey) -> Result<(), String> {
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

pub(crate) async fn await_keys(
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

pub(crate) async fn gateway_status(
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

pub(crate) async fn publish_corrupt_generation(database: &str) -> Result<i64, String> {
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

pub(crate) async fn set_proxy(
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

pub(crate) async fn wait_readiness(
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
