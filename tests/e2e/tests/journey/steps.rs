//! The end-to-end journey: bootstrap, configure two providers and two
//! routes through the documented management API, issue inference on all
//! three surfaces, and record everything for the assertion tests.
//!
//! Assertions do NOT live here — the journey only records outcomes. It
//! aborts on failures that make later steps meaningless (recording the
//! reason), so every assertion test can report either a contract violation
//! or the precise step that prevented verification.

use std::time::{Duration, Instant};

use serde_json::{Value, json};

use super::harness::Server;
use super::mock_upstream::{self, MockUpstream, RecordedRequest};
use super::sse::{self, SseStream};

pub const OPENAI_ROUTE: &str = "e2e-openai";
pub const CROSS_ROUTE: &str = "e2e-cross";

#[derive(Debug, Default)]
pub struct JourneyReport {
    /// First fatal error; steps after it were not attempted.
    pub abort: Option<String>,
    pub server_stderr_tail: String,
    pub probe_compat: Option<Value>,
    pub probe_azure: Option<Value>,
    pub certification_compat: Option<Value>,
    pub certification_azure: Option<Value>,
    pub ready_after_activation: Option<(u16, Value)>,
    pub api_key_secret: Option<String>,
    pub inference: Vec<InferenceCall>,
    pub upstream_requests: Vec<RecordedRequest>,
    pub upstream_unexpected: Vec<String>,
    pub requests_api: Option<Value>,
    pub usage_summary: Option<Value>,
    pub audit: Option<Value>,
}

#[derive(Debug)]
pub struct InferenceCall {
    pub name: &'static str,
    pub status: u16,
    pub content_type: String,
    pub body: Vec<u8>,
}

impl InferenceCall {
    pub fn json(&self) -> Value {
        serde_json::from_slice(&self.body).unwrap_or(Value::Null)
    }

    pub fn sse(&self) -> Result<SseStream, String> {
        sse::decode(&self.body)
    }
}

impl JourneyReport {
    pub fn call(&self, name: &str) -> &InferenceCall {
        self.inference
            .iter()
            .find(|call| call.name == name)
            .unwrap_or_else(|| {
                panic!(
                    "inference call {name} was never issued; journey abort: {:?}",
                    self.abort
                )
            })
    }

    pub fn require<'a, T>(&'a self, field: &'a Option<T>, what: &str) -> &'a T {
        field
            .as_ref()
            .unwrap_or_else(|| panic!("journey never reached `{what}`; abort: {:?}", self.abort))
    }
}

struct Management {
    http: reqwest::Client,
    origin: String,
    cookie: String,
    csrf: String,
    idempotency_sequence: u32,
}

struct MgmtResponse {
    status: u16,
    etag: Option<String>,
    body: Value,
}

impl Management {
    fn new(origin: &str) -> Self {
        Self {
            http: reqwest::Client::builder()
                .timeout(Duration::from_secs(30))
                .build()
                .expect("reqwest client builds"),
            origin: origin.to_owned(),
            cookie: String::new(),
            csrf: String::new(),
            idempotency_sequence: 0,
        }
    }

    fn next_idempotency_key(&mut self) -> String {
        self.idempotency_sequence += 1;
        format!("e2e-journey-{:04}", self.idempotency_sequence)
    }

    async fn setup(&mut self, setup_token: &str) -> Result<MgmtResponse, String> {
        let response = self
            .http
            .post(format!("{}/api/v1/setup", self.origin))
            .header("x-olp-setup-token", setup_token)
            .header(reqwest::header::ORIGIN, &self.origin)
            .json(&json!({
                "email": "owner@e2e.test",
                "password": "correct horse battery staple",
                "display_name": "E2E Owner",
                "installation_name": "E2E journey"
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

    async fn send(
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
        let mutation = method != reqwest::Method::GET;
        if mutation {
            request = request
                .header("x-csrf-token", &self.csrf)
                .header(reqwest::header::ORIGIN, &self.origin);
        }
        if let Some(key) = idempotency_key {
            request = request.header("idempotency-key", key);
        }
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

    async fn expect(
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
    let etag = response
        .headers()
        .get(reqwest::header::ETAG)
        .and_then(|value| value.to_str().ok())
        .map(str::to_owned);
    let bytes = response
        .bytes()
        .await
        .map_err(|error| format!("failed to read response body: {error}"))?;
    let body = serde_json::from_slice(&bytes).unwrap_or(Value::Null);
    Ok(MgmtResponse { status, etag, body })
}

fn required_etag(response: &MgmtResponse, what: &str) -> Result<String, String> {
    response
        .etag
        .clone()
        .ok_or_else(|| format!("{what} response carries no ETag header"))
}

/// Configures one provider from draft to active and returns its id. Probe
/// and certification bodies are written into the report slots as soon as
/// they exist so an abort later in the flow cannot discard them.
async fn configure_provider(
    management: &mut Management,
    create_body: Value,
    upstream_model: &str,
    capabilities: Value,
    probe_slot: &mut Option<Value>,
    certification_slot: &mut Option<Value>,
) -> Result<String, String> {
    let name = create_body["name"]
        .as_str()
        .unwrap_or("provider")
        .to_owned();
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
        .ok_or_else(|| format!("{name}: create response lacks id: {}", created.body))?
        .to_owned();
    let mut etag = required_etag(&created, &format!("{name} create"))?;

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
    *probe_slot = Some(probe.body.clone());

    let discovery = management
        .expect(
            reqwest::Method::POST,
            &format!("/api/v1/providers/{provider_id}/discovery"),
            Some(json!({
                "models": [{"upstream_model": upstream_model, "display_name": upstream_model}]
            })),
            None,
            Some(&etag),
            200,
        )
        .await?;
    etag = required_etag(&discovery, &format!("{name} discovery"))?;

    let models = management
        .expect(
            reqwest::Method::GET,
            &format!("/api/v1/providers/{provider_id}/models?limit=100"),
            None,
            None,
            None,
            200,
        )
        .await?;
    let model_id = models.body["items"]
        .as_array()
        .and_then(|items| {
            items
                .iter()
                .find(|item| item["upstream_model"] == upstream_model)
        })
        .and_then(|item| item["id"].as_str())
        .ok_or_else(|| format!("{name}: model {upstream_model} not listed: {}", models.body))?
        .to_owned();

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
    etag = required_etag(&reviewed, &format!("{name} capability review"))?;

    // Certification and activation each require a probe newer than the
    // draft's latest change, so connectivity is re-verified before both.
    management
        .expect(
            reqwest::Method::POST,
            &format!("/api/v1/providers/{provider_id}/probe"),
            None,
            None,
            Some(&etag),
            200,
        )
        .await?;

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
    *certification_slot = Some(certification.body.clone());
    etag = required_etag(&certification, &format!("{name} certification"))?;

    management
        .expect(
            reqwest::Method::POST,
            &format!("/api/v1/providers/{provider_id}/probe"),
            None,
            None,
            Some(&etag),
            200,
        )
        .await?;

    let key = management.next_idempotency_key();
    management
        .expect(
            reqwest::Method::POST,
            &format!("/api/v1/providers/{provider_id}/activate"),
            None,
            Some(&key),
            Some(&etag),
            200,
        )
        .await?;

    Ok(provider_id)
}

async fn configure_route(
    management: &mut Management,
    slug: &str,
    provider_id: &str,
    provider_model: &str,
) -> Result<(), String> {
    let key = management.next_idempotency_key();
    let created = management
        .expect(
            reqwest::Method::POST,
            "/api/v1/route-drafts",
            Some(json!({
                "slug": slug,
                "operations": ["generation"],
                "overall_timeout_ms": 30000,
                "max_attempts": 1,
                "targets": [{
                    "provider_id": provider_id,
                    "provider_model": provider_model,
                    "priority": 0,
                    "weight": 1,
                    "timeout_ms": 20000
                }]
            })),
            Some(&key),
            None,
            201,
        )
        .await?;
    let draft_id = created.body["id"]
        .as_str()
        .ok_or_else(|| format!("route draft {slug} lacks id: {}", created.body))?
        .to_owned();
    let etag = required_etag(&created, &format!("route draft {slug}"))?;

    let validated = management
        .expect(
            reqwest::Method::POST,
            &format!("/api/v1/route-drafts/{draft_id}/validate"),
            None,
            None,
            Some(&etag),
            200,
        )
        .await?;
    let etag = required_etag(&validated, &format!("route draft {slug} validation"))?;

    let key = management.next_idempotency_key();
    management
        .expect(
            reqwest::Method::POST,
            &format!("/api/v1/route-drafts/{draft_id}/activate"),
            None,
            Some(&key),
            Some(&etag),
            200,
        )
        .await?;
    Ok(())
}

async fn record_inference(
    report: &mut JourneyReport,
    http: &reqwest::Client,
    name: &'static str,
    request: reqwest::RequestBuilder,
) -> Result<(), String> {
    let response = request
        .send()
        .await
        .map_err(|error| format!("inference call {name} failed to send: {error}"))?;
    let status = response.status().as_u16();
    let content_type = response
        .headers()
        .get(reqwest::header::CONTENT_TYPE)
        .and_then(|value| value.to_str().ok())
        .unwrap_or_default()
        .to_owned();
    let body = response
        .bytes()
        .await
        .map_err(|error| format!("inference call {name} failed mid-body: {error}"))?
        .to_vec();
    let _ = http;
    report.inference.push(InferenceCall {
        name,
        status,
        content_type,
        body,
    });
    Ok(())
}

pub async fn run() -> JourneyReport {
    let mut report = JourneyReport::default();

    let mock = MockUpstream::spawn().await;
    let server = match Server::launch().await {
        Ok(server) => server,
        Err(error) => {
            report.abort = Some(error);
            return report;
        }
    };

    let outcome = drive(&mut report, &server, &mock).await;
    if let Err(error) = outcome {
        report.abort = Some(error);
    }
    report.upstream_requests = mock.recorded();
    report.upstream_unexpected = mock.unexpected();
    report.server_stderr_tail = server.shutdown().await;
    report
}

async fn drive(
    report: &mut JourneyReport,
    server: &Server,
    mock: &MockUpstream,
) -> Result<(), String> {
    let journey_start = std::time::SystemTime::now();
    let mut management = Management::new(&server.public_origin);
    management.setup(&server.setup_token).await?;

    // Provider 1: openai_compatible serving the OpenAI surface.
    let mut probe_compat = None;
    let mut certification_compat = None;
    let compat_result = configure_provider(
        &mut management,
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
        &mut probe_compat,
        &mut certification_compat,
    )
    .await;
    report.probe_compat = probe_compat;
    report.certification_compat = certification_compat;
    let compat_id = compat_result?;

    // Provider 2: azure_openai serving the translated Anthropic and Gemini
    // surfaces through its deployment.
    let mut probe_azure = None;
    let mut certification_azure = None;
    let azure_result = configure_provider(
        &mut management,
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
        &mut probe_azure,
        &mut certification_azure,
    )
    .await;
    report.probe_azure = probe_azure;
    report.certification_azure = certification_azure;
    let azure_id = azure_result?;

    configure_route(
        &mut management,
        OPENAI_ROUTE,
        &compat_id,
        mock_upstream::MODEL,
    )
    .await?;
    configure_route(
        &mut management,
        CROSS_ROUTE,
        &azure_id,
        mock_upstream::DEPLOYMENT,
    )
    .await?;

    let key = management.next_idempotency_key();
    let api_key = management
        .expect(
            reqwest::Method::POST,
            "/api/v1/api-keys",
            Some(json!({
                "name": "e2e journey key",
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
    report.api_key_secret = Some(secret.clone());

    // Readiness on the observability listener flips to 200 once the runtime
    // generation with providers, routes, and the key is live.
    let http = reqwest::Client::builder()
        .timeout(Duration::from_secs(30))
        .build()
        .expect("reqwest client builds");
    let ready_url = format!("{}/health/ready", server.observability_base);
    let deadline = Instant::now() + Duration::from_secs(30);
    loop {
        let response = http
            .get(&ready_url)
            .send()
            .await
            .map_err(|error| format!("readiness poll failed: {error}"))?;
        let status = response.status().as_u16();
        let body: Value = response.json().await.unwrap_or(Value::Null);
        if status == 200 || Instant::now() > deadline {
            report.ready_after_activation = Some((status, body));
            break;
        }
        tokio::time::sleep(Duration::from_millis(500)).await;
    }

    // The key exists only inside the activated runtime generation; poll an
    // authenticated read until the gateway converges.
    let models_url = format!("{}/openai/v1/models", server.public_origin);
    let deadline = Instant::now() + Duration::from_secs(20);
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

    let base = &server.public_origin;
    record_inference(
        report,
        &http,
        "openai_chat_unary",
        http.post(format!("{base}/openai/v1/chat/completions"))
            .bearer_auth(&secret)
            .json(&json!({
                "model": OPENAI_ROUTE,
                "messages": [{"role": "user", "content": "hello"}],
                "max_tokens": 32
            })),
    )
    .await?;
    record_inference(
        report,
        &http,
        "openai_chat_stream",
        http.post(format!("{base}/openai/v1/chat/completions"))
            .bearer_auth(&secret)
            .json(&json!({
                "model": OPENAI_ROUTE,
                "messages": [{"role": "user", "content": "hello"}],
                "max_tokens": 32,
                "stream": true,
                "stream_options": {"include_usage": true}
            })),
    )
    .await?;
    record_inference(
        report,
        &http,
        "openai_chat_stream_cr",
        http.post(format!("{base}/openai/v1/chat/completions"))
            .bearer_auth(&secret)
            .json(&json!({
                "model": OPENAI_ROUTE,
                "messages": [{"role": "user", "content": mock_upstream::CR_MARKER}],
                "max_tokens": 32,
                "stream": true
            })),
    )
    .await?;
    record_inference(
        report,
        &http,
        "anthropic_unary",
        http.post(format!("{base}/anthropic/v1/messages"))
            .header("x-api-key", &secret)
            .json(&json!({
                "model": CROSS_ROUTE,
                "max_tokens": 32,
                "messages": [{"role": "user", "content": "hello"}]
            })),
    )
    .await?;
    record_inference(
        report,
        &http,
        "anthropic_stream",
        http.post(format!("{base}/anthropic/v1/messages"))
            .header("x-api-key", &secret)
            .json(&json!({
                "model": CROSS_ROUTE,
                "max_tokens": 32,
                "stream": true,
                "messages": [{"role": "user", "content": "hello"}]
            })),
    )
    .await?;
    record_inference(
        report,
        &http,
        "gemini_unary",
        http.post(format!(
            "{base}/gemini/v1beta/models/{CROSS_ROUTE}:generateContent"
        ))
        .header("x-goog-api-key", &secret)
        .json(&json!({
            "contents": [{"role": "user", "parts": [{"text": "hello"}]}]
        })),
    )
    .await?;
    record_inference(
        report,
        &http,
        "gemini_stream",
        http.post(format!(
            "{base}/gemini/v1beta/models/{CROSS_ROUTE}:streamGenerateContent?alt=sse"
        ))
        .header("x-goog-api-key", &secret)
        .json(&json!({
            "contents": [{"role": "user", "parts": [{"text": "hello"}]}]
        })),
    )
    .await?;
    record_inference(
        report,
        &http,
        "negative_bad_key",
        http.post(format!("{base}/openai/v1/chat/completions"))
            .bearer_auth("olp_v2_000000000000_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
            .json(&json!({
                "model": OPENAI_ROUTE,
                "messages": [{"role": "user", "content": "hello"}]
            })),
    )
    .await?;
    record_inference(
        report,
        &http,
        "negative_unknown_route",
        http.post(format!("{base}/openai/v1/chat/completions"))
            .bearer_auth(&secret)
            .json(&json!({
                "model": "no-such-route",
                "messages": [{"role": "user", "content": "hello"}]
            })),
    )
    .await?;

    // Request metadata lands asynchronously (gateway -> Valkey stream ->
    // in-process worker -> PostgreSQL); poll until the successful calls are
    // all visible or the deadline documents the shortfall.
    let deadline = Instant::now() + Duration::from_secs(30);
    loop {
        let requests = management
            .send(
                reqwest::Method::GET,
                "/api/v1/requests?limit=100",
                None,
                None,
                None,
            )
            .await?;
        let count = requests.body["data"].as_array().map_or(0, |rows| {
            rows.iter()
                .filter(|row| row["operation"] == "generation")
                .count()
        });
        let done = count >= 7 || Instant::now() > deadline;
        if done {
            report.requests_api = Some(requests.body);
            break;
        }
        tokio::time::sleep(Duration::from_millis(500)).await;
    }

    let start = chrono_like(journey_start - Duration::from_secs(3600));
    let end = chrono_like(std::time::SystemTime::now() + Duration::from_secs(3600));
    let usage = management
        .send(
            reqwest::Method::GET,
            &format!("/api/v1/usage/summary?start={start}&end={end}"),
            None,
            None,
            None,
        )
        .await?;
    report.usage_summary = Some(usage.body);

    let audit = management
        .send(
            reqwest::Method::GET,
            "/api/v1/audit?limit=100",
            None,
            None,
            None,
        )
        .await?;
    report.audit = Some(audit.body);

    Ok(())
}

/// RFC 3339 UTC timestamp without a chrono dependency.
fn chrono_like(time: std::time::SystemTime) -> String {
    let seconds = time
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap_or_default()
        .as_secs() as i64;
    let days = seconds.div_euclid(86_400);
    let secs_of_day = seconds.rem_euclid(86_400);
    let (hour, minute, second) = (
        secs_of_day / 3600,
        (secs_of_day % 3600) / 60,
        secs_of_day % 60,
    );
    // Civil-from-days algorithm (Howard Hinnant), valid for the test's era.
    let z = days + 719_468;
    let era = z.div_euclid(146_097);
    let doe = z.rem_euclid(146_097);
    let yoe = (doe - doe / 1460 + doe / 36_524 - doe / 146_096) / 365;
    let year = yoe + era * 400;
    let doy = doe - (365 * yoe + yoe / 4 - yoe / 100);
    let mp = (5 * doy + 2) / 153;
    let day = doy - (153 * mp + 2) / 5 + 1;
    let month = if mp < 10 { mp + 3 } else { mp - 9 };
    let year = if month <= 2 { year + 1 } else { year };
    format!("{year:04}-{month:02}-{day:02}T{hour:02}:{minute:02}:{second:02}Z")
}
