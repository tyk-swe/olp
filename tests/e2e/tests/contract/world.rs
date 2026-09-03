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

#[path = "world/fixture.rs"]
mod fixture;
#[path = "world/management.rs"]
mod management;

#[allow(unused_imports)]
pub(crate) use fixture::{
    bootstrap, bootstrap_ha, bootstrap_sharing_valkey, bootstrap_worker_ha, resolve_model_row,
};
#[allow(unused_imports)]
pub(crate) use management::{Management, MgmtResponse};

use std::time::{Duration, Instant};

use reqwest::header::HeaderMap;
use serde_json::{Value, json};
use tokio::sync::Mutex;

use crate::harness::{Server, WorkerProcess};
use crate::mock_upstream::MockUpstream;
use crate::otlp::OtlpReceiver;

pub(crate) const OPENAI_ROUTE: &str = "e2e-openai";
pub(crate) const CROSS_ROUTE: &str = "e2e-cross";
pub(crate) const TRACE_ROUTE: &str = "e2e-trace-failover";
pub(crate) const OWNER_EMAIL: &str = "owner@e2e.test";
pub(crate) const OWNER_PASSWORD: &str = "correct horse battery staple";

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
    otlp: Option<OtlpReceiver>,
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

    pub(crate) fn otlp(&self) -> &OtlpReceiver {
        self.otlp
            .as_ref()
            .expect("this fixture was launched with tracing enabled")
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

        let budgeted = ["daily_cost_limit", "monthly_cost_limit"]
            .iter()
            .any(|field| body.get(*field).is_some_and(|value| !value.is_null()));
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
        let generation_id = created.body["runtime_generation"]["id"]
            .as_str()
            .ok_or_else(|| format!("API key response lacks generation ID: {}", created.body))?
            .to_owned();
        let generation = created.body["runtime_generation"]["sequence"]
            .as_i64()
            .ok_or_else(|| format!("API key response lacks generation: {}", created.body))?;
        if budgeted {
            // A new budget must wait for an authoritative snapshot, not seed zero.
            await_key_with_timeout(
                &self.http,
                &self.public_origin,
                &secret,
                Duration::from_secs(90),
            )
            .await?;
        } else {
            await_key(&self.http, &self.public_origin, &secret).await?;
        }
        Ok(IssuedKey {
            id,
            secret,
            etag,
            generation_id,
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
            let rows = response.body["items"].as_array().cloned().ok_or_else(|| {
                format!("request listing carries no data array: {}", response.body)
            })?;
            if rows.len() >= expected {
                tokio::time::sleep(Duration::from_millis(750)).await;
                let settled = self.management.get(&path).await?;
                return settled.body["items"].as_array().cloned().ok_or_else(|| {
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

pub(crate) struct IssuedKey {
    pub(crate) id: String,
    pub(crate) secret: String,
    pub(crate) etag: String,
    pub(crate) generation_id: String,
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
    await_key_with_timeout(http, origin, secret, Duration::from_secs(30)).await
}

async fn await_key_with_timeout(
    http: &reqwest::Client,
    origin: &str,
    secret: &str,
    timeout: Duration,
) -> Result<(), String> {
    let models_url = format!("{origin}/openai/v1/models");
    let deadline = Instant::now() + timeout;
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
