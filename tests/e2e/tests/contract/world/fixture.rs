use std::time::Duration;

use serde_json::{Value, json};
use tokio::sync::Mutex;

use crate::harness::{GatewayProcess, Server, WorkerBoundary, WorkerProcess};
use crate::mock_upstream::{self, MockUpstream};
use crate::otlp::OtlpReceiver;

use super::{CROSS_ROUTE, Management, OPENAI_ROUTE, TRACE_ROUTE, World, await_key};

/// Brings up a server, two providers, three routes and an API key, and waits
/// until the gateway serves the key.
pub(crate) async fn bootstrap() -> Result<World, String> {
    let otlp = OtlpReceiver::spawn().await?;
    let server = Server::launch_traced(otlp.endpoint()).await?;
    bootstrap_server_at_gateway(server, None, Some(otlp)).await
}

pub(crate) async fn bootstrap_sharing_valkey(valkey_url: &str) -> Result<World, String> {
    bootstrap_server(Server::launch_sharing_valkey(valkey_url).await?).await
}

pub(crate) async fn bootstrap_ha() -> Result<(World, GatewayProcess), String> {
    let otlp = OtlpReceiver::spawn().await?;
    let mut server = Server::launch_traced(otlp.endpoint()).await?;
    let gateway = server.launch_gateway().await?;
    Ok((
        bootstrap_server_at_gateway(server, None, Some(otlp)).await?,
        gateway,
    ))
}

pub(crate) async fn bootstrap_worker_ha() -> Result<(World, [WorkerProcess; 3]), String> {
    let mut server = Server::launch_control().await?;
    let gateway = server.launch_gateway().await?;
    let first = server
        .launch_worker("worker-1", WorkerBoundary::RequestMetadata)
        .await?;
    let second = server
        .launch_worker("worker-2", WorkerBoundary::RuntimeOutbox)
        .await?;
    let third = server
        .launch_worker("worker-3", WorkerBoundary::None)
        .await?;
    server.release_worker(&first)?;
    let world = bootstrap_server_at_gateway(server, Some(gateway.public_origin), None).await?;
    Ok((world, [first, second, third]))
}

async fn bootstrap_server(server: Server) -> Result<World, String> {
    bootstrap_server_at_gateway(server, None, None).await
}

async fn bootstrap_server_at_gateway(
    server: Server,
    gateway_origin: Option<String>,
    otlp: Option<OtlpReceiver>,
) -> Result<World, String> {
    let mock = MockUpstream::spawn().await;
    let mut management = Management::new(&server.public_origin);
    management.setup(&server.setup_token).await?;

    let compat_provider = configure_provider(
        &management,
        json!({
            "name": "e2e-compat",
            "kind": "openai_compatible",
            "endpoint": format!("{}/v1/", mock.base),
            "auth_mode": "api_key",
            "credential": mock_upstream::COMPAT_CREDENTIAL
        }),
        mock_upstream::MODEL,
        json!([
            {"operation": "generation", "surface": "openai", "mode": "unary"},
            {"operation": "generation", "surface": "openai", "mode": "streaming"}
        ]),
    )
    .await?;

    let azure_provider = configure_provider(
        &management,
        json!({
            "name": "e2e-azure",
            "kind": "azure_openai",
            "endpoint": mock.base,
            "deployment": mock_upstream::DEPLOYMENT,
            "api_version": mock_upstream::API_VERSION,
            "credential": mock_upstream::AZURE_CREDENTIAL
        }),
        mock_upstream::DEPLOYMENT,
        json!([
            {"operation": "generation", "surface": "openai", "mode": "unary"},
            {"operation": "generation", "surface": "openai", "mode": "streaming"},
            {"operation": "generation", "surface": "anthropic", "mode": "unary"},
            {"operation": "generation", "surface": "anthropic", "mode": "streaming"},
            {"operation": "generation", "surface": "gemini", "mode": "unary"},
            {"operation": "generation", "surface": "gemini", "mode": "streaming"}
        ]),
    )
    .await?;

    configure_route(
        &management,
        OPENAI_ROUTE,
        vec![route_target(&compat_provider, mock_upstream::MODEL, 0)],
        1,
    )
    .await?;
    configure_route(
        &management,
        CROSS_ROUTE,
        vec![route_target(&azure_provider, mock_upstream::DEPLOYMENT, 0)],
        1,
    )
    .await?;
    configure_route(
        &management,
        TRACE_ROUTE,
        vec![
            route_target(&compat_provider, mock_upstream::MODEL, 0),
            route_target(&azure_provider, mock_upstream::DEPLOYMENT, 1),
        ],
        2,
    )
    .await?;

    configure_pricing(&management).await?;

    let key = management.next_idempotency_key();
    let api_key = management
        .expect(
            reqwest::Method::POST,
            "/api/v1/api-keys",
            Some(json!({
                "name": "e2e contract key",
                "scopes": ["inference", "models_read"],
                "allowed_routes": [OPENAI_ROUTE, CROSS_ROUTE, TRACE_ROUTE]
            })),
            Some(&key),
            None,
            201,
        )
        .await?;
    let api_key_id = api_key.body["id"]
        .as_str()
        .ok_or_else(|| format!("api key response lacks id: {}", api_key.body))?
        .to_owned();
    let secret = api_key.body["secret"]
        .as_str()
        .ok_or_else(|| format!("api key response lacks secret: {}", api_key.body))?
        .to_owned();

    let http = reqwest::Client::builder()
        .timeout(Duration::from_secs(30))
        .build()
        .expect("reqwest client builds");

    // The key exists only inside the activated runtime generation, so wait for
    // the gateway to converge before handing the fixture to any test.
    let public_origin = gateway_origin.unwrap_or_else(|| server.public_origin.clone());
    await_key(&http, &public_origin, &secret).await?;

    Ok(World {
        public_origin,
        observability_base: server.observability_base.clone(),
        setup_token: server.setup_token.clone(),
        database_url: server.database_url.clone(),
        server: Mutex::new(Some(server)),
        mock,
        management,
        http,
        api_key_id,
        api_key: secret,
        compat_provider,
        azure_provider,
        otlp,
    })
}

/// Drives one provider from draft to active and returns its id.
///
/// The repeated probes before certify and before activate are a workaround,
/// not the contract. See `provider_lifecycle` for the assertion that pins the
/// documented flow.
async fn configure_provider(
    management: &Management,
    create_body: Value,
    model_id: &str,
    capabilities: Value,
) -> Result<String, String> {
    let key = management.next_idempotency_key();
    let created = management
        .expect(
            reqwest::Method::POST,
            "/api/v1/providers",
            Some(create_body),
            Some(&key),
            None,
            201,
        )
        .await?;
    let provider_id = created.body["id"]
        .as_str()
        .ok_or_else(|| format!("provider create response lacks id: {}", created.body))?
        .to_owned();
    let mut etag = created.require_etag("provider create")?;

    let probe = management
        .expect(
            reqwest::Method::POST,
            &format!("/api/v1/providers/{provider_id}/probe"),
            None,
            None,
            Some(&etag),
            200,
        )
        .await?;
    if let Some(fresh) = probe.etag() {
        etag = fresh;
    }

    let discovery = management
        .expect(
            reqwest::Method::POST,
            &format!("/api/v1/providers/{provider_id}/discovery"),
            Some(json!({"mode": "live"})),
            None,
            Some(&etag),
            200,
        )
        .await?;
    etag = discovery.require_etag("discovery")?;

    let model_id = resolve_model_row(management, &provider_id, model_id).await?;

    let reviewed = management
        .expect(
            reqwest::Method::PATCH,
            &format!("/api/v1/providers/{provider_id}/models/{model_id}"),
            Some(json!({"enabled": true, "capabilities": capabilities})),
            None,
            Some(&etag),
            200,
        )
        .await?;
    etag = reviewed.require_etag("capability review")?;

    etag = reprobe(management, &provider_id, etag).await?;
    let certification = management
        .expect(
            reqwest::Method::POST,
            &format!("/api/v1/providers/{provider_id}/models/{model_id}/certify"),
            None,
            None,
            Some(&etag),
            200,
        )
        .await?;
    etag = certification.require_etag("certification")?;

    etag = reprobe(management, &provider_id, etag).await?;
    management
        .expect(
            reqwest::Method::POST,
            &format!("/api/v1/providers/{provider_id}/activate"),
            None,
            None,
            Some(&etag),
            200,
        )
        .await?;

    Ok(provider_id)
}

/// Resolves an upstream model name to the row id the management API uses in
/// `/models/{model_id}` paths. Discovery assigns each model a row of its own,
/// and the capability-review and certify paths address that row, not the name
/// the provider reports.
pub(crate) async fn resolve_model_row(
    management: &Management,
    provider_id: &str,
    model_name: &str,
) -> Result<String, String> {
    let listing = management
        .expect(
            reqwest::Method::GET,
            &format!("/api/v1/providers/{provider_id}/models?limit=100"),
            None,
            None,
            None,
            200,
        )
        .await?;

    let rows = listing.body["items"]
        .as_array()
        .ok_or_else(|| format!("model listing carries no items array: {}", listing.body))?;

    let row = rows
        .iter()
        .find(|row| row["upstream_model"].as_str() == Some(model_name))
        .ok_or_else(|| format!("discovery did not surface {model_name}: {}", listing.body))?;

    row.get("id")
        .and_then(Value::as_str)
        .map(str::to_owned)
        .ok_or_else(|| format!("model row carries no id: {row}"))
}

async fn reprobe(
    management: &Management,
    provider_id: &str,
    etag: String,
) -> Result<String, String> {
    let probe = management
        .expect(
            reqwest::Method::POST,
            &format!("/api/v1/providers/{provider_id}/probe"),
            None,
            None,
            Some(&etag),
            200,
        )
        .await?;
    Ok(probe.etag().unwrap_or(etag))
}

/// Puts a pricing revision in force for the mock's models.
///
/// Part of the fixture rather than of one test, because an installation with no
/// pricing prices nothing, and in that installation every assertion about
/// `unpriced` holds vacuously — a record hard-wired to "unpriced" would satisfy
/// the missing-usage contract and no test would notice that nothing is ever
/// priced. Every telemetry assertion is written against an installation that
/// can price, which is also the one operators run.
async fn configure_pricing(management: &Management) -> Result<(), String> {
    // An hour ago, so the revision is already in force for the first request
    // any test issues.
    let effective_at = (chrono::Utc::now() - chrono::Duration::hours(1))
        .format("%Y-%m-%dT%H:%M:%SZ")
        .to_string();
    let prices: Vec<Value> = [
        ("openai_compatible", mock_upstream::MODEL),
        ("azure_openai", mock_upstream::DEPLOYMENT),
    ]
    .into_iter()
    .map(|(kind, model)| {
        json!({
            "provider_kind": kind,
            "model": model,
            "operation": "generation",
            "currency": "USD",
            "input_per_million": "1000.00",
            "output_per_million": "1000.00"
        })
    })
    .collect();

    management
        .expect(
            reqwest::Method::POST,
            "/api/v1/pricing/revisions",
            Some(json!({"effective_at": effective_at, "prices": prices})),
            None,
            None,
            201,
        )
        .await?;
    Ok(())
}

fn route_target(provider_id: &str, model_id: &str, priority: u32) -> Value {
    json!({
        "provider_id": provider_id,
        "provider_model": model_id,
        "priority": priority,
        "weight": 1,
        "timeout_ms": 30_000
    })
}

async fn configure_route(
    management: &Management,
    route: &str,
    targets: Vec<Value>,
    max_attempts: u16,
) -> Result<(), String> {
    // Routes are drafts-first: `/api/v1/routes` is read-only and a route comes
    // into being by creating a draft and activating it. Field names and the
    // required set come from CreateRouteDraftRequest and RouteTargetRequest in
    // openapi/management.json.
    let created = management
        .send(
            reqwest::Method::POST,
            "/api/v1/route-drafts",
            Some(json!({
                "slug": route,
                "operations": ["generation"],
                "overall_timeout_ms": 30_000,
                "max_attempts": max_attempts,
                "targets": targets
            })),
            None,
            None,
        )
        .await?;
    if !(200..300).contains(&created.status) {
        return Err(format!(
            "POST /api/v1/route-drafts returned {}: {}",
            created.status, created.body
        ));
    }

    let draft_id = created
        .body
        .get("id")
        .and_then(Value::as_str)
        .ok_or_else(|| format!("route draft carries no id: {}", created.body))?
        .to_owned();
    let etag = created.require_etag("route draft create")?;

    // A draft must be validated before it can be activated.
    let validated = management
        .send(
            reqwest::Method::POST,
            &format!("/api/v1/route-drafts/{draft_id}/validate"),
            None,
            None,
            Some(&etag),
        )
        .await?;
    if !(200..300).contains(&validated.status) {
        return Err(format!(
            "POST /api/v1/route-drafts/{draft_id}/validate returned {}: {}",
            validated.status, validated.body
        ));
    }
    let etag = validated.etag().unwrap_or(etag);

    let activated = management
        .send(
            reqwest::Method::POST,
            &format!("/api/v1/route-drafts/{draft_id}/activate"),
            None,
            None,
            Some(&etag),
        )
        .await?;
    if !(200..300).contains(&activated.status) {
        return Err(format!(
            "POST /api/v1/route-drafts/{draft_id}/activate returned {}: {}",
            activated.status, activated.body
        ));
    }
    Ok(())
}
