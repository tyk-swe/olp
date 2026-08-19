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

use std::sync::atomic::{AtomicU32, Ordering};
use std::time::{Duration, Instant};

use reqwest::header::HeaderMap;
use serde_json::{Value, json};
use tokio::sync::Mutex;

use crate::harness::{GatewayProcess, Server, WorkerBoundary, WorkerProcess};
use crate::mock_upstream::{self, MockUpstream};

pub(crate) const OPENAI_ROUTE: &str = "e2e-openai";
pub(crate) const CROSS_ROUTE: &str = "e2e-cross";
pub(crate) const OWNER_EMAIL: &str = "owner@e2e.test";
pub(crate) const OWNER_PASSWORD: &str = "correct horse battery staple";

pub(crate) struct MgmtResponse {
    pub(crate) status: u16,
    pub(crate) headers: HeaderMap,
    pub(crate) body: Value,
}

impl MgmtResponse {
    pub(crate) fn header(&self, name: &str) -> Option<String> {
        self.headers
            .get(name)
            .and_then(|value| value.to_str().ok())
            .map(str::to_owned)
    }

    pub(crate) fn etag(&self) -> Option<String> {
        self.header("etag")
    }

    pub(crate) fn require_etag(&self, what: &str) -> Result<String, String> {
        self.etag()
            .ok_or_else(|| format!("{what} response carries no ETag header"))
    }
}

pub(crate) struct Management {
    http: reqwest::Client,
    origin: String,
    cookie: String,
    csrf: String,
    sequence: AtomicU32,
}

impl Management {
    pub(crate) fn new(origin: &str) -> Self {
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

    pub(crate) fn next_idempotency_key(&self) -> String {
        let next = self.sequence.fetch_add(1, Ordering::Relaxed) + 1;
        format!("e2e-contract-{next:04}")
    }

    /// Performs first-run setup and retains the resulting session.
    ///
    /// `POST /api/v1/setup` requires an `Origin` header matching
    /// `OLP_PUBLIC_ORIGIN`; the setup token is single-use.
    pub(crate) async fn setup(&mut self, setup_token: &str) -> Result<MgmtResponse, String> {
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

    pub(crate) async fn send(
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

    /// Sends a management request with exactly the headers given and no others.
    ///
    /// `send` supplies the session cookie, the CSRF token, `Origin` and an
    /// idempotency key because almost every call needs them. The documented
    /// failure modes are precisely the cases where one of those is absent or
    /// wrong, so they need a builder that adds nothing on the caller's behalf.
    pub(crate) async fn raw(
        &self,
        method: reqwest::Method,
        path: &str,
        body: Option<Value>,
        headers: &[(&str, &str)],
    ) -> Result<MgmtResponse, String> {
        let mut request = self
            .http
            .request(method.clone(), format!("{}{path}", self.origin));
        for (name, value) in headers {
            request = request.header(*name, *value);
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

    pub(crate) fn cookie(&self) -> &str {
        &self.cookie
    }

    pub(crate) fn csrf(&self) -> &str {
        &self.csrf
    }

    pub(crate) fn origin(&self) -> &str {
        &self.origin
    }

    pub(crate) async fn get(&self, path: &str) -> Result<MgmtResponse, String> {
        self.send(reqwest::Method::GET, path, None, None, None)
            .await
    }

    pub(crate) async fn post(&self, path: &str, body: Value) -> Result<MgmtResponse, String> {
        let key = self.next_idempotency_key();
        self.send(reqwest::Method::POST, path, Some(body), Some(&key), None)
            .await
    }

    pub(crate) async fn expect(
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
    let body = serde_json::from_slice(&bytes)
        .unwrap_or_else(|_| Value::String(String::from_utf8_lossy(&bytes).into_owned()));
    Ok(MgmtResponse {
        status,
        headers,
        body,
    })
}

pub(crate) struct World {
    /// Held so teardown can consume it; tests read the copied origins below.
    server: Mutex<Option<Server>>,
    pub(crate) public_origin: String,
    pub(crate) observability_base: String,
    pub(crate) setup_token: String,
    pub(crate) database_url: String,
    pub(crate) mock: MockUpstream,
    pub(crate) management: Management,
    pub(crate) http: reqwest::Client,
    pub(crate) api_key_id: String,
    pub(crate) api_key: String,
    pub(crate) compat_provider: String,
    pub(crate) azure_provider: String,
}

/// A gateway response kept whole: assertions need the status line, the headers
/// and the raw body, and a body that failed to parse must stay readable in the
/// failure message rather than becoming `null`.
pub(crate) struct GatewayResponse {
    pub(crate) status: u16,
    pub(crate) headers: HeaderMap,
    pub(crate) text: String,
}

impl GatewayResponse {
    pub(crate) fn header(&self, name: &str) -> Option<String> {
        self.headers
            .get(name)
            .and_then(|value| value.to_str().ok())
            .map(str::to_owned)
    }

    pub(crate) fn json(&self) -> Value {
        serde_json::from_str(&self.text).unwrap_or(Value::Null)
    }
}

impl World {
    pub(crate) async fn valkey_url(&self) -> Result<String, String> {
        self.server
            .lock()
            .await
            .as_ref()
            .map(|server| server.valkey_url().to_owned())
            .ok_or_else(|| "server has already shut down".to_owned())
    }

    pub(crate) async fn release_worker(&self, worker: &WorkerProcess) -> Result<(), String> {
        self.server
            .lock()
            .await
            .as_ref()
            .ok_or_else(|| "server has already shut down".to_owned())?
            .release_worker(worker)
    }

    pub(crate) async fn hard_kill_worker(&self, worker: &WorkerProcess) -> Result<(), String> {
        let mut guard = self.server.lock().await;
        let server = guard
            .as_mut()
            .ok_or_else(|| "server has already shut down".to_owned())?;
        server.hard_kill_worker(worker).await
    }

    pub(crate) fn origin(&self) -> &str {
        &self.public_origin
    }

    /// Posts to a gateway surface with a bearer credential.
    pub(crate) async fn gateway_post(
        &self,
        path: &str,
        body: Value,
        credential: &str,
    ) -> Result<GatewayResponse, String> {
        self.gateway_send(
            reqwest::Method::POST,
            path,
            Some(body),
            &[(
                reqwest::header::AUTHORIZATION.as_str(),
                &format!("Bearer {credential}"),
            )],
        )
        .await
    }

    /// Posts to a gateway surface with whatever credential headers the caller
    /// names, so a test can pin the header each vendor dialect documents
    /// (`Authorization`, `x-api-key`, `x-goog-api-key`) or send none at all.
    pub(crate) async fn gateway_send(
        &self,
        method: reqwest::Method,
        path: &str,
        body: Option<Value>,
        headers: &[(&str, &str)],
    ) -> Result<GatewayResponse, String> {
        let mut request = self
            .http
            .request(method.clone(), format!("{}{path}", self.public_origin));
        for (name, value) in headers {
            request = request.header(*name, *value);
        }
        if let Some(body) = body {
            request = request.json(&body);
        }
        let response = request
            .send()
            .await
            .map_err(|error| format!("{method} {path} failed: {error}"))?;
        let status = response.status().as_u16();
        let headers = response.headers().clone();
        let text = response
            .text()
            .await
            .map_err(|error| format!("failed to read the {method} {path} body: {error}"))?;
        Ok(GatewayResponse {
            status,
            headers,
            text,
        })
    }

    /// Issues an API key and waits until the gateway serves it.
    ///
    /// `overrides` is merged into the create body, so a test can pin the limit
    /// fields `CreateApiKeyRequest` documents without restating the rest.
    pub(crate) async fn issue_key(
        &self,
        name: &str,
        overrides: Value,
    ) -> Result<IssuedKey, String> {
        let mut body = json!({
            "name": name,
            "scopes": ["inference", "models_read"],
            "allowed_routes": [OPENAI_ROUTE, CROSS_ROUTE]
        });
        if let Some(fields) = overrides.as_object() {
            for (key, value) in fields {
                body[key] = value.clone();
            }
        }

        let created = self
            .management
            .expect(
                reqwest::Method::POST,
                "/api/v1/api-keys",
                Some(body),
                None,
                None,
                201,
            )
            .await?;
        let published_at = Instant::now();
        let secret = created.body["secret"]
            .as_str()
            .ok_or_else(|| format!("api key response lacks secret: {}", created.body))?
            .to_owned();
        let id = created.body["id"]
            .as_str()
            .ok_or_else(|| format!("api key response lacks id: {}", created.body))?
            .to_owned();
        let etag = created.require_etag("API key create")?;
        let generation = created.body["runtime_generation"]["sequence"]
            .as_i64()
            .ok_or_else(|| format!("API key response lacks generation: {}", created.body))?;
        await_key(&self.http, &self.public_origin, &secret).await?;
        Ok(IssuedKey {
            id,
            secret,
            etag,
            generation,
            published_at,
        })
    }

    /// Waits until the request log holds `expected` rows for `api_key_id`.
    ///
    /// Metadata ingestion is asynchronous — the gateway emits a terminal
    /// envelope and the worker persists it — so a count read immediately after
    /// a response is a race. This waits for the expected count and then *keeps
    /// waiting briefly*, so a duplicate row still fails the caller's exact
    /// assertion instead of being read before it lands.
    pub(crate) async fn await_request_rows(
        &self,
        api_key_id: &str,
        filter: &str,
        expected: usize,
    ) -> Result<Vec<Value>, String> {
        // Issuing a key costs one gateway request of its own — the convergence
        // poll against the model listing — and that request is logged like any
        // other. `filter` narrows the query to the traffic the caller made, so
        // the count can stay exact instead of becoming a lower bound.
        let path = format!("/api/v1/requests?api_key_id={api_key_id}&limit=200{filter}");
        let deadline = Instant::now() + Duration::from_secs(30);
        loop {
            let response = self.management.get(&path).await?;
            if response.status != 200 {
                return Err(format!(
                    "GET {path} returned {}: {}",
                    response.status, response.body
                ));
            }
            let rows = response.body["data"].as_array().cloned().ok_or_else(|| {
                format!("request listing carries no data array: {}", response.body)
            })?;
            if rows.len() >= expected {
                tokio::time::sleep(Duration::from_millis(750)).await;
                let settled = self.management.get(&path).await?;
                return settled.body["data"].as_array().cloned().ok_or_else(|| {
                    format!("request listing carries no data array: {}", settled.body)
                });
            }
            if Instant::now() > deadline {
                return Err(format!(
                    "only {} of {expected} request rows were persisted within 30s",
                    rows.len()
                ));
            }
            tokio::time::sleep(Duration::from_millis(250)).await;
        }
    }

    /// Stops the server and releases the per-run database. Returns the tail of
    /// its stderr. Idempotent, so a second call is harmless.
    pub(crate) async fn shutdown(&self) -> String {
        let server = self.server.lock().await.take();
        match server {
            Some(server) => server.shutdown().await,
            None => String::new(),
        }
    }
}

/// Brings up a server, two providers, two routes and an API key, and waits
/// until the gateway serves the key.
pub(crate) async fn bootstrap() -> Result<World, String> {
    bootstrap_server(Server::launch().await?).await
}

pub(crate) async fn bootstrap_sharing_valkey(valkey_url: &str) -> Result<World, String> {
    bootstrap_server(Server::launch_sharing_valkey(valkey_url).await?).await
}

pub(crate) async fn bootstrap_ha() -> Result<(World, GatewayProcess), String> {
    let mut server = Server::launch().await?;
    let gateway = server.launch_gateway().await?;
    Ok((bootstrap_server(server).await?, gateway))
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
    let world = bootstrap_server_at_gateway(server, Some(gateway.public_origin)).await?;
    Ok((world, [first, second, third]))
}

async fn bootstrap_server(server: Server) -> Result<World, String> {
    bootstrap_server_at_gateway(server, None).await
}

async fn bootstrap_server_at_gateway(
    server: Server,
    gateway_origin: Option<String>,
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

    configure_pricing(&management).await?;

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
    })
}

pub(crate) struct IssuedKey {
    pub(crate) id: String,
    pub(crate) secret: String,
    pub(crate) etag: String,
    pub(crate) generation: i64,
    pub(crate) published_at: Instant,
}

/// Blocks until the gateway accepts `secret`.
///
/// A newly published key exists only inside a runtime generation the gateways
/// have not yet loaded; `docs/architecture.md` "Runtime publication" makes that
/// propagation explicit, so waiting for it is part of using the API, not a
/// workaround for flakiness.
async fn await_key(http: &reqwest::Client, origin: &str, secret: &str) -> Result<(), String> {
    let models_url = format!("{origin}/openai/v1/models");
    let deadline = Instant::now() + Duration::from_secs(30);
    loop {
        let status = http
            .get(&models_url)
            .bearer_auth(secret)
            .send()
            .await
            .map_err(|error| format!("gateway convergence poll failed: {error}"))?
            .status();
        if status.is_success() {
            return Ok(());
        }
        if Instant::now() > deadline {
            return Err(format!(
                "gateway never accepted the new API key (last status {status})"
            ));
        }
        tokio::time::sleep(Duration::from_millis(250)).await;
    }
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
            Some(json!({})),
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
            "input_per_million": "1.00",
            "output_per_million": "2.00"
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

async fn configure_route(
    management: &Management,
    route: &str,
    provider_id: &str,
    model_id: &str,
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
                // Bounded by the target count, so a single-target route may
                // attempt exactly once.
                "max_attempts": 1,
                "targets": [{
                    "provider_id": provider_id,
                    "provider_model": model_id,
                    "priority": 0,
                    "weight": 1,
                    "timeout_ms": 30_000
                }]
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
