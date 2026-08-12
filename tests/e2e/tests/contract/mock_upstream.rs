//! Loopback mock upstream provider speaking the real OpenAI wire format.
//!
//! Serves both the OpenAI-compatible surface (`/v1/*`) and the Azure OpenAI
//! deployment surface (`/openai/deployments/{deployment}/*`). Every request
//! is recorded (method, path, query, credentials, body) so the journey can
//! assert exactly what the product sent upstream; unexpected paths are
//! recorded and answered 404 so contract gaps surface instead of hanging.

use std::sync::{Arc, Mutex};

use axum::{
    Router,
    extract::{Request, State},
    http::{StatusCode, header},
    response::{IntoResponse, Response},
    routing::any,
};
use serde_json::{Value, json};

pub const MODEL: &str = "e2e-model";
pub const DEPLOYMENT: &str = "e2e-deploy";
pub const API_VERSION: &str = "2024-10-21";
pub const COMPAT_CREDENTIAL: &str = "sk-e2e-compat";
pub const AZURE_CREDENTIAL: &str = "azure-e2e-secret";
/// Marker in the user text that switches the mock to the CR-fidelity reply.
pub const CR_MARKER: &str = "CR_FIDELITY";
/// Marker that makes the upstream answer 200 with no `usage` object at all —
/// the "missing upstream usage" case `docs/architecture.md` pins.
pub const NO_USAGE_MARKER: &str = "OMIT_USAGE";
/// Marker that makes the upstream answer 429 with a `Retry-After`, so the
/// gateway's own translation of that status can be observed.
pub const RATE_LIMITED_MARKER: &str = "UPSTREAM_429";
/// `Retry-After` seconds the mock sends with its 429.
pub const RETRY_AFTER_SECONDS: &str = "37";
/// Reply text for ordinary calls.
pub const PLAIN_TEXT: &str = "Hello, world";
pub const PLAIN_DELTAS: [&str; 3] = ["Hello", ", ", "world"];
/// Reply text containing a raw CR+LF inside a delta — exercises byte fidelity
/// through the product's SSE encode/decode path.
pub const CR_TEXT: &str = "line1\r\nline2 tail";
pub const CR_DELTAS: [&str; 2] = ["line1\r\nline2", " tail"];
pub const PROMPT_TOKENS: u64 = 7;
pub const COMPLETION_TOKENS: u64 = 5;
pub const TOTAL_TOKENS: u64 = 12;

#[derive(Clone, Debug)]
pub struct RecordedRequest {
    pub method: String,
    pub path: String,
    pub query: Option<String>,
    pub authorization: Option<String>,
    pub api_key_header: Option<String>,
    /// Every header, lowercased, in arrival order. The two credential fields
    /// above are conveniences over this list, not a filter applied before it:
    /// an assertion about what the product forwards upstream can only be made
    /// against the complete set.
    pub headers: Vec<(String, String)>,
    pub body: Value,
}

impl RecordedRequest {
    pub fn header(&self, name: &str) -> Option<&str> {
        let name = name.to_ascii_lowercase();
        self.headers
            .iter()
            .find(|(key, _)| *key == name)
            .map(|(_, value)| value.as_str())
    }
}

#[derive(Clone, Default)]
pub struct MockState {
    pub requests: Arc<Mutex<Vec<RecordedRequest>>>,
    pub unexpected: Arc<Mutex<Vec<String>>>,
}

pub struct MockUpstream {
    /// `http://127.0.0.1:{port}` — no trailing slash, no path.
    pub base: String,
    pub state: MockState,
}

impl MockUpstream {
    pub async fn spawn() -> Self {
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let address = listener.local_addr().unwrap();
        let state = MockState::default();
        let app = Router::new()
            .fallback(any(dispatch))
            .with_state(state.clone());
        tokio::spawn(async move {
            axum::serve(listener, app).await.unwrap();
        });
        Self {
            base: format!("http://{address}"),
            state,
        }
    }

    pub fn recorded(&self) -> Vec<RecordedRequest> {
        self.state.requests.lock().unwrap().clone()
    }

    /// The number of requests seen so far.
    ///
    /// Paired with [`Self::since`], this lets a test assert an *exact* upstream
    /// call count for its own request without the shared bootstrap traffic —
    /// probes, discovery, certification — inflating the total. `>=` assertions
    /// pass under a duplicate-send bug; these do not.
    pub fn checkpoint(&self) -> usize {
        self.state.requests.lock().unwrap().len()
    }

    /// Requests recorded after `checkpoint`.
    pub fn since(&self, checkpoint: usize) -> Vec<RecordedRequest> {
        self.state.requests.lock().unwrap()[checkpoint..].to_vec()
    }

    pub fn unexpected(&self) -> Vec<String> {
        self.state.unexpected.lock().unwrap().clone()
    }
}

async fn dispatch(State(state): State<MockState>, request: Request) -> Response {
    let method = request.method().to_string();
    let path = request.uri().path().to_owned();
    let query = request.uri().query().map(str::to_owned);
    let authorization = header_string(&request, header::AUTHORIZATION.as_str());
    let api_key_header = header_string(&request, "api-key");
    let headers = request
        .headers()
        .iter()
        .map(|(name, value)| {
            (
                name.as_str().to_ascii_lowercase(),
                String::from_utf8_lossy(value.as_bytes()).into_owned(),
            )
        })
        .collect();
    let body_bytes = match axum::body::to_bytes(request.into_body(), 1 << 20).await {
        Ok(body) => body,
        Err(error) => {
            state
                .unexpected
                .lock()
                .unwrap()
                .push(format!("{method} {path}: unreadable body ({error})"));
            return StatusCode::PAYLOAD_TOO_LARGE.into_response();
        }
    };
    let body: Value = serde_json::from_slice(&body_bytes).unwrap_or(Value::Null);

    state.requests.lock().unwrap().push(RecordedRequest {
        method: method.clone(),
        path: path.clone(),
        query: query.clone(),
        authorization,
        api_key_header,
        headers,
        body: body.clone(),
    });

    let azure_chat = format!("/openai/deployments/{DEPLOYMENT}/chat/completions");
    let azure_responses = format!("/openai/deployments/{DEPLOYMENT}/responses");
    match (method.as_str(), path.as_str()) {
        ("GET", "/v1/models") => models_response(MODEL).into_response(),
        ("POST", "/v1/chat/completions") => chat_response(&body, MODEL),
        ("POST", "/v1/responses") => responses_response(&body, MODEL),
        ("POST", p) if p == azure_chat => chat_response(&body, DEPLOYMENT),
        ("POST", p) if p == azure_responses => responses_response(&body, DEPLOYMENT),
        _ => {
            state
                .unexpected
                .lock()
                .unwrap()
                .push(format!("{method} {path}"));
            StatusCode::NOT_FOUND.into_response()
        }
    }
}

fn header_string(request: &Request, name: &str) -> Option<String> {
    request
        .headers()
        .get(name)
        .and_then(|value| value.to_str().ok())
        .map(str::to_owned)
}

fn models_response(model: &str) -> impl IntoResponse {
    axum::Json(json!({
        "object": "list",
        "data": [{"id": model, "object": "model"}]
    }))
}

/// Selects the reply text: the CR-fidelity variant when any string in the
/// request body contains the marker, the plain variant otherwise.
fn reply_deltas(body: &Value) -> (&'static str, Vec<&'static str>) {
    if body_contains_marker(body) {
        (CR_TEXT, CR_DELTAS.to_vec())
    } else {
        (PLAIN_TEXT, PLAIN_DELTAS.to_vec())
    }
}

fn body_contains_marker(value: &Value) -> bool {
    contains(value, CR_MARKER)
}

/// Whether any string anywhere in the request body contains `marker`.
///
/// The search is structural rather than a substring match on the serialised
/// body, so a marker only counts when the product actually carried it through
/// as text — not when it happens to appear in a field name or an escape.
fn contains(value: &Value, marker: &str) -> bool {
    match value {
        Value::String(text) => text.contains(marker),
        Value::Array(items) => items.iter().any(|item| contains(item, marker)),
        Value::Object(map) => map.values().any(|item| contains(item, marker)),
        _ => false,
    }
}

/// A 429 carrying `Retry-After`, exactly as a rate-limited upstream sends it.
fn rate_limited() -> Response {
    (
        StatusCode::TOO_MANY_REQUESTS,
        [
            (header::RETRY_AFTER, RETRY_AFTER_SECONDS),
            (header::CONTENT_TYPE, "application/json"),
        ],
        serde_json::to_string(&json!({
            "error": {
                "message": "Rate limit reached",
                "type": "rate_limit_error",
                "code": "rate_limit_exceeded"
            }
        }))
        .unwrap_or_default(),
    )
        .into_response()
}

fn is_stream(body: &Value) -> bool {
    body.get("stream").and_then(Value::as_bool).unwrap_or(false)
}

fn usage_chat() -> Value {
    json!({
        "prompt_tokens": PROMPT_TOKENS,
        "completion_tokens": COMPLETION_TOKENS,
        "total_tokens": TOTAL_TOKENS
    })
}

fn chat_response(body: &Value, model: &str) -> Response {
    if contains(body, RATE_LIMITED_MARKER) {
        return rate_limited();
    }
    let (text, deltas) = reply_deltas(body);
    let usage = (!contains(body, NO_USAGE_MARKER)).then(usage_chat);
    if !is_stream(body) {
        let mut payload = json!({
            "id": "chatcmpl-e2e",
            "object": "chat.completion",
            "created": 1,
            "model": model,
            "choices": [{
                "index": 0,
                "message": {"role": "assistant", "content": text},
                "finish_reason": "stop"
            }]
        });
        if let Some(usage) = usage {
            payload["usage"] = usage;
        }
        return axum::Json(payload).into_response();
    }

    let mut frames = Vec::new();
    frames.push(chat_chunk(model, json!({"role": "assistant"}), None, None));
    for delta in deltas {
        frames.push(chat_chunk(model, json!({"content": delta}), None, None));
    }
    frames.push(chat_chunk(model, json!({}), Some("stop"), usage));
    sse_body(frames, true)
}

fn chat_chunk(model: &str, delta: Value, finish: Option<&str>, usage: Option<Value>) -> String {
    let mut chunk = json!({
        "id": "chatcmpl-e2e",
        "object": "chat.completion.chunk",
        "created": 1,
        "model": model,
        "choices": [{
            "index": 0,
            "delta": delta,
            "finish_reason": finish
        }]
    });
    if let Some(usage) = usage {
        chunk["usage"] = usage;
    }
    format!("data: {chunk}")
}

fn responses_response(body: &Value, model: &str) -> Response {
    if contains(body, RATE_LIMITED_MARKER) {
        return rate_limited();
    }
    let (text, deltas) = reply_deltas(body);
    let usage = (!contains(body, NO_USAGE_MARKER)).then(|| {
        json!({
            "input_tokens": PROMPT_TOKENS,
            "output_tokens": COMPLETION_TOKENS,
            "total_tokens": TOTAL_TOKENS
        })
    });
    if !is_stream(body) {
        let mut payload = json!({
            "id": "resp_e2e",
            "object": "response",
            "created_at": 1,
            "status": "completed",
            "model": model,
            "output": [{
                "id": "msg_e2e",
                "type": "message",
                "role": "assistant",
                "status": "completed",
                "content": [{"type": "output_text", "text": text, "annotations": []}]
            }]
        });
        if let Some(usage) = usage {
            payload["usage"] = usage;
        }
        return axum::Json(payload).into_response();
    }

    let mut frames = Vec::new();
    frames.push(format!(
        "event: response.created\ndata: {}",
        json!({"type": "response.created", "response": {"id": "resp_e2e", "model": model}})
    ));
    frames.push(format!(
        "event: response.output_item.added\ndata: {}",
        json!({
            "type": "response.output_item.added",
            "output_index": 0,
            "item": {"type": "message", "role": "assistant", "status": "in_progress"}
        })
    ));
    for delta in deltas {
        frames.push(format!(
            "event: response.output_text.delta\ndata: {}",
            json!({"type": "response.output_text.delta", "output_index": 0, "delta": delta})
        ));
    }
    frames.push(format!(
        "event: response.output_item.done\ndata: {}",
        json!({
            "type": "response.output_item.done",
            "output_index": 0,
            "item": {"type": "message", "status": "completed"}
        })
    ));
    let mut completed = json!({
        "type": "response.completed",
        "response": {
            "status": "completed",
            "output": [{"type": "message", "status": "completed"}]
        }
    });
    if let Some(usage) = usage {
        completed["response"]["usage"] = usage;
    }
    frames.push(format!("event: response.completed\ndata: {completed}"));
    sse_body(frames, false)
}

fn sse_body(frames: Vec<String>, done_sentinel: bool) -> Response {
    let mut body = String::new();
    for frame in frames {
        body.push_str(&frame);
        body.push_str("\n\n");
    }
    if done_sentinel {
        body.push_str("data: [DONE]\n\n");
    }
    (
        StatusCode::OK,
        [(header::CONTENT_TYPE, "text/event-stream")],
        body,
    )
        .into_response()
}
