//! The live fixture the contract assertions run against.
//!
//! This replaces the previous design, in which one `run()` recorded a frozen
//! report and every test read fields off it. That shape made whole classes of
//! assertion unreachable: a test could not issue a request of its own, so no
//! error path, negative case, or documented failure mode could be exercised.
//! Here `bootstrap` brings up a real server and leaves it running, and each
//! test drives it directly.
//!
//! `bootstrap` deliberately does whatever it takes to reach a working
//! installation, including the extra re-probes the provider lifecycle
//! currently demands. Those workarounds are *not* presented as the contract:
//! the documented flow is asserted on its own in `provider_lifecycle`, so a
//! defect there fails one named test instead of collapsing the whole suite.

use std::sync::Mutex;
use std::sync::atomic::{AtomicU32, Ordering};
use std::time::{Duration, Instant};

use reqwest::header::HeaderMap;
use serde_json::{Value, json};

use crate::harness::Server;
use crate::mock_upstream::{self, MockUpstream};

pub const OPENAI_ROUTE: &str = "e2e-openai";
pub const CROSS_ROUTE: &str = "e2e-cross";
pub const OWNER_EMAIL: &str = "owner@e2e.test";
pub const OWNER_PASSWORD: &str = "correct horse battery staple";

pub struct MgmtResponse {
    pub status: u16,
    pub headers: HeaderMap,
    pub body: Value,
}

impl MgmtResponse {
    pub fn header(&self, name: &str) -> Option<String> {
        self.headers
            .get(name)
            .and_then(|value| value.to_str().ok())
            .map(str::to_owned)
    }

    pub fn etag(&self) -> Option<String> {
        self.header("etag")
    }

    pub fn require_etag(&self, what: &str) -> Result<String, String> {
        self.etag()
            .ok_or_else(|| format!("{what} response carries no ETag header"))
    }
}

pub struct Management {
    http: reqwest::Client,
    origin: String,
    cookie: String,
    csrf: String,
    sequence: AtomicU32,
}

impl Management {
    pub fn new(origin: &str) -> Self {
        Self {
            http: reqwest::Client::builder()
                .timeout(Duration::from_secs(30))
                .build()
                .expect("reqwest client builds"),
            origin: origin.to_owned(),
            cookie: String::new(),
            csrf: String::new(),
            sequence: AtomicU32::new(0),
        }
    }

    pub fn next_idempotency_key(&self) -> String {
        let next = self.sequence.fetch_add(1, Ordering::Relaxed) + 1;
        format!("e2e-contract-{next:04}")
    }

    /// Performs first-run setup and retains the resulting session.
    ///
    /// `POST /api/v1/setup` requires an `Origin` header matching
    /// `OLP_PUBLIC_ORIGIN`; the setup token is single-use.
    pub async fn setup(&mut self, setup_token: &str) -> Result<MgmtResponse, String> {
        let response = self
            .http
            .post(format!("{}/api/v1/setup", self.origin))
            .header("x-olp-setup-token", setup_token)
            .header(reqwest::header::ORIGIN, &self.origin)
            .json(&json!({
                "email": OWNER_EMAIL,
                "password": OWNER_PASSWORD,
                "display_name": "E2E Owner",
                "installation_name": "E2E contract"
            }))
            .send()
            .await
            .map_err(|error| format!("setup request failed: {error}"))?;
        let cookie = response
            .headers()
            .get_all(reqwest::header::SET_COOKIE)
            .iter()
            .filter_map(|value| value.to_str().ok())
            .filter_map(|value| value.split(';').next())
            .collect::<Vec<_>>()
            .join("; ");
        let parsed = read_response(response).await?;
        if parsed.status != 201 {
            return Err(format!(
                "setup returned {} instead of 201: {}",
                parsed.status, parsed.body
            ));
        }
        self.cookie = cookie;
        self.csrf = parsed.body["csrf_token"]
            .as_str()
            .ok_or_else(|| format!("setup response lacks csrf_token: {}", parsed.body))?
            .to_owned();
        Ok(parsed)
    }

    pub async fn send(
        &self,
        method: reqwest::Method,
        path: &str,
        body: Option<Value>,
        idempotency_key: Option<&str>,
        if_match: Option<&str>,
    ) -> Result<MgmtResponse, String> {
        let mut request = self
            .http
            .request(method.clone(), format!("{}{path}", self.origin))
            .header(reqwest::header::COOKIE, &self.cookie);
        if method != reqwest::Method::GET {
            request = request
                .header("x-csrf-token", &self.csrf)
                .header(reqwest::header::ORIGIN, &self.origin);
        }
        // Mutating management operations require an Idempotency-Key. Callers
        // that do not pin one get a fresh key, so no call site has to restate a
        // header the API demands of every mutation.
        let generated_key;
        let key = match idempotency_key {
            Some(key) => key,
            None => {
                generated_key = self.next_idempotency_key();
                &generated_key
            }
        };
        request = request.header("idempotency-key", key);
        if let Some(etag) = if_match {
            request = request.header(reqwest::header::IF_MATCH, etag);
        }
        if let Some(body) = body {
            request = request.json(&body);
        }
        let response = request
            .send()
            .await
            .map_err(|error| format!("{method} {path} failed: {error}"))?;
        read_response(response).await
    }

    pub async fn get(&self, path: &str) -> Result<MgmtResponse, String> {
        self.send(reqwest::Method::GET, path, None, None, None).await
    }

    pub async fn post(&self, path: &str, body: Value) -> Result<MgmtResponse, String> {
        let key = self.next_idempotency_key();
        self.send(reqwest::Method::POST, path, Some(body), Some(&key), None)
            .await
    }

    pub async fn expect(
        &self,
        method: reqwest::Method,
        path: &str,
        body: Option<Value>,
        idempotency_key: Option<&str>,
        if_match: Option<&str>,
        expected_status: u16,
    ) -> Result<MgmtResponse, String> {
        let response = self
            .send(method.clone(), path, body, idempotency_key, if_match)
            .await?;
        if response.status != expected_status {
            return Err(format!(
                "{method} {path} returned {} instead of {expected_status}: {}",
                response.status, response.body
            ));
        }
        Ok(response)
    }
}

async fn read_response(response: reqwest::Response) -> Result<MgmtResponse, String> {
    let status = response.status().as_u16();
    let headers = response.headers().clone();
    let bytes = response
        .bytes()
        .await
        .map_err(|error| format!("failed to read response body: {error}"))?;
    let body = serde_json::from_slice(&bytes).unwrap_or(Value::Null);
    Ok(MgmtResponse {
        status,
        headers,
        body,
    })
}

pub struct World {
    /// Held so teardown can consume it; tests read the copied origins below.
    server: Mutex<Option<Server>>,
    pub public_origin: String,
    pub observability_base: String,
    pub setup_token: String,
    pub mock: MockUpstream,
    pub management: Management,
    pub http: reqwest::Client,
    pub api_key: String,
    pub compat_provider: String,
    pub azure_provider: String,
}

impl World {
    pub fn origin(&self) -> &str {
        &self.public_origin
    }

    /// Stops the server and releases the per-run database. Returns the tail of
    /// its stderr. Idempotent, so a second call is harmless.
    pub async fn shutdown(&self) -> String {
        let server = self.server.lock().expect("world lock is not poisoned").take();
        match server {
            Some(server) => server.shutdown().await,
            None => String::new(),
        }
    }
}

/// Brings up a server, two providers, two routes and an API key, and waits
/// until the gateway serves the key.
pub async fn bootstrap() -> Result<World, String> {
    let mock = MockUpstream::spawn().await;
    let server = Server::launch().await?;
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
        &compat_provider,
        mock_upstream::MODEL,
    )
    .await?;
    configure_route(
        &management,
        CROSS_ROUTE,
        &azure_provider,
        mock_upstream::DEPLOYMENT,
    )
    .await?;

    let key = management.next_idempotency_key();
    let api_key = management
        .expect(
            reqwest::Method::POST,
            "/api/v1/api-keys",
            Some(json!({
                "name": "e2e contract key",
                "scopes": ["inference", "models_read"],
                "allowed_routes": [OPENAI_ROUTE, CROSS_ROUTE]
            })),
            Some(&key),
            None,
            201,
        )
        .await?;
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
    let models_url = format!("{}/openai/v1/models", server.public_origin);
    let deadline = Instant::now() + Duration::from_secs(30);
    loop {
        let status = http
            .get(&models_url)
            .bearer_auth(&secret)
            .send()
            .await
            .map_err(|error| format!("gateway convergence poll failed: {error}"))?
            .status();
        if status.is_success() {
            break;
        }
        if Instant::now() > deadline {
            return Err(format!(
                "gateway never accepted the new API key (last status {status})"
            ));
        }
        tokio::time::sleep(Duration::from_millis(250)).await;
    }

    Ok(World {
        public_origin: server.public_origin.clone(),
        observability_base: server.observability_base.clone(),
        setup_token: server.setup_token.clone(),
        server: Mutex::new(Some(server)),
        mock,
        management,
        http,
        api_key: secret,
        compat_provider,
        azure_provider,
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
pub async fn resolve_model_row(
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

    let rows = ["items", "data", "models"]
        .iter()
        .find_map(|key| listing.body.get(*key).and_then(Value::as_array))
        .ok_or_else(|| format!("model listing carries no array of rows: {}", listing.body))?;

    let row = rows
        .iter()
        .find(|row| {
            ["model", "name", "model_id", "upstream_model"]
                .iter()
                .any(|key| row.get(*key).and_then(Value::as_str) == Some(model_name))
        })
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

async fn configure_route(
    management: &Management,
    route: &str,
    provider_id: &str,
    model_id: &str,
) -> Result<(), String> {
    let key = management.next_idempotency_key();
    let created = management
        .expect(
            reqwest::Method::POST,
            "/api/v1/routes",
            Some(json!({
                "name": route,
                "targets": [{"provider_id": provider_id, "model_id": model_id, "weight": 1}]
            })),
            Some(&key),
            None,
            201,
        )
        .await?;
    let etag = created.require_etag("route create")?;
    management
        .expect(
            reqwest::Method::POST,
            &format!("/api/v1/routes/{route}/activate"),
            None,
            None,
            Some(&etag),
            200,
        )
        .await?;
    Ok(())
}
