//! True end-to-end journey assertions.
//!
//! The journey (`steps::run`) executes exactly once per test binary run: it
//! drives the real `olp all` binary against real PostgreSQL, real Valkey,
//! and a loopback mock upstream, then records every outcome. Each `#[test]`
//! below asserts one documented contract point against that record.
//!
//! HONESTY CONTRACT: assertions encode the documented behavior, never the
//! current implementation. A red test here is a product bug; it gets an
//! entry in tests/e2e/known-failures.txt (see scripts/run-e2e-tests.sh),
//! not a weakened assertion.
//!
//! All tests are #[ignore]d (repo convention for suites needing external
//! services) and must run with --test-threads=1.

use std::sync::OnceLock;

use serde_json::Value;

mod journey {
    pub mod harness;
    pub mod mock_upstream;
    pub mod sse;
    pub mod steps;
}

use journey::mock_upstream;
use journey::steps::{self, CROSS_ROUTE, JourneyReport, OPENAI_ROUTE};

fn report() -> &'static JourneyReport {
    static REPORT: OnceLock<JourneyReport> = OnceLock::new();
    REPORT.get_or_init(|| {
        tokio::runtime::Builder::new_multi_thread()
            .enable_all()
            .build()
            .expect("tokio runtime builds")
            .block_on(steps::run())
    })
}

/// OpenAI-surface stream: JSON chunks from default-typed events, excluding
/// the `[DONE]` sentinel.
fn openai_chunks(call: &steps::InferenceCall) -> Vec<Value> {
    let stream = call.sse().expect("response is not a decodable SSE stream");
    stream
        .events
        .iter()
        .filter(|event| event.data != "[DONE]")
        .map(|event| {
            serde_json::from_str(&event.data).unwrap_or_else(|error| {
                panic!("stream data is not JSON ({error}): {:?}", event.data)
            })
        })
        .collect()
}

fn openai_stream_text(call: &steps::InferenceCall) -> String {
    openai_chunks(call)
        .iter()
        .filter_map(|chunk| chunk["choices"][0]["delta"]["content"].as_str())
        .collect()
}

// ---------------------------------------------------------------------------
// Journey and configuration phase
// ---------------------------------------------------------------------------

#[test]
#[ignore = "needs PostgreSQL, Valkey, and the olp binary; run via make e2e"]
fn journey_completes_without_fatal_error() {
    let report = report();
    assert!(
        report.abort.is_none(),
        "journey aborted: {}\nserver stderr tail:\n{}",
        report.abort.as_deref().unwrap_or_default(),
        report.server_stderr_tail
    );
}

#[test]
#[ignore = "needs PostgreSQL, Valkey, and the olp binary; run via make e2e"]
fn provider_probe_succeeds_for_openai_compatible() {
    let report = report();
    let probe = report.require(&report.probe_compat, "openai_compatible probe");
    assert_eq!(probe["succeeded"], true, "probe response: {probe}");
}

#[test]
#[ignore = "needs PostgreSQL, Valkey, and the olp binary; run via make e2e"]
fn provider_probe_succeeds_for_azure_openai() {
    let report = report();
    let probe = report.require(&report.probe_azure, "azure_openai probe");
    assert_eq!(probe["succeeded"], true, "probe response: {probe}");
}

#[test]
#[ignore = "needs PostgreSQL, Valkey, and the olp binary; run via make e2e"]
fn certification_certifies_every_compat_tuple() {
    let report = report();
    let certification = report.require(&report.certification_compat, "compat certification");
    assert_eq!(
        certification["status"], "succeeded",
        "certification response: {certification}"
    );
    assert_eq!(
        certification["certified_count"], 2,
        "expected both openai-surface generation tuples certified: {certification}"
    );
}

#[test]
#[ignore = "needs PostgreSQL, Valkey, and the olp binary; run via make e2e"]
fn certification_certifies_every_azure_tuple() {
    let report = report();
    let certification = report.require(&report.certification_azure, "azure certification");
    assert_eq!(
        certification["status"], "succeeded",
        "certification response: {certification}"
    );
    assert_eq!(
        certification["certified_count"], 4,
        "expected anthropic+gemini generation tuples certified: {certification}"
    );
}

#[test]
#[ignore = "needs PostgreSQL, Valkey, and the olp binary; run via make e2e"]
fn readiness_reports_ok_after_activation() {
    let report = report();
    let (status, body) = report.require(&report.ready_after_activation, "readiness poll");
    assert_eq!(
        *status, 200,
        "/health/ready stayed unready after activation: {body}"
    );
}

#[test]
#[ignore = "needs PostgreSQL, Valkey, and the olp binary; run via make e2e"]
fn api_key_secret_has_documented_format() {
    let report = report();
    let secret = report.require(&report.api_key_secret, "api key creation");
    let remainder = secret
        .strip_prefix("olp_v2_")
        .unwrap_or_else(|| panic!("api key lacks olp_v2_ prefix: {secret}"));
    let (lookup, secret_part) = remainder
        .split_once('_')
        .unwrap_or_else(|| panic!("api key lacks lookup separator: {secret}"));
    assert_eq!(lookup.len(), 12, "lookup id must be 12 hex chars: {secret}");
    assert!(
        lookup.bytes().all(|byte| byte.is_ascii_hexdigit()),
        "lookup id must be hex: {secret}"
    );
    assert_eq!(
        secret_part.len(),
        43,
        "secret must be 43 chars of URL-safe base64: {secret}"
    );
}

// ---------------------------------------------------------------------------
// OpenAI surface
// ---------------------------------------------------------------------------

#[test]
#[ignore = "needs PostgreSQL, Valkey, and the olp binary; run via make e2e"]
fn openai_unary_returns_chat_completion() {
    let call = report().call("openai_chat_unary");
    assert_eq!(
        call.status,
        200,
        "body: {}",
        String::from_utf8_lossy(&call.body)
    );
    let body = call.json();
    assert_eq!(body["object"], "chat.completion", "body: {body}");
    assert_eq!(
        body["choices"][0]["message"]["content"],
        mock_upstream::PLAIN_TEXT,
        "body: {body}"
    );
    assert_eq!(body["choices"][0]["finish_reason"], "stop", "body: {body}");
}

#[test]
#[ignore = "needs PostgreSQL, Valkey, and the olp binary; run via make e2e"]
fn openai_unary_passes_usage_through_exactly() {
    let body = report().call("openai_chat_unary").json();
    assert_eq!(
        body["usage"]["prompt_tokens"], 7,
        "usage: {}",
        body["usage"]
    );
    assert_eq!(
        body["usage"]["completion_tokens"], 5,
        "usage: {}",
        body["usage"]
    );
    assert_eq!(
        body["usage"]["total_tokens"], 12,
        "usage: {}",
        body["usage"]
    );
}

#[test]
#[ignore = "needs PostgreSQL, Valkey, and the olp binary; run via make e2e"]
fn openai_stream_is_served_as_event_stream() {
    let call = report().call("openai_chat_stream");
    assert_eq!(
        call.status,
        200,
        "body: {}",
        String::from_utf8_lossy(&call.body)
    );
    assert!(
        call.content_type.starts_with("text/event-stream"),
        "content-type: {}",
        call.content_type
    );
}

#[test]
#[ignore = "needs PostgreSQL, Valkey, and the olp binary; run via make e2e"]
fn openai_stream_terminates_with_done_sentinel() {
    let call = report().call("openai_chat_stream");
    let stream = call.sse().expect("stream must decode as SSE");
    let last = stream
        .events
        .last()
        .unwrap_or_else(|| panic!("stream has no events"));
    assert_eq!(last.data, "[DONE]", "final event: {last:?}");
    assert!(
        stream.undispatched_tail.is_empty(),
        "stream ended mid-event: {:?}",
        stream.undispatched_tail
    );
}

#[test]
#[ignore = "needs PostgreSQL, Valkey, and the olp binary; run via make e2e"]
fn openai_stream_preserves_text_exactly() {
    let text = openai_stream_text(report().call("openai_chat_stream"));
    assert_eq!(text, mock_upstream::PLAIN_TEXT);
}

#[test]
#[ignore = "needs PostgreSQL, Valkey, and the olp binary; run via make e2e"]
fn openai_stream_reports_requested_usage() {
    let chunks = openai_chunks(report().call("openai_chat_stream"));
    let usage = chunks
        .iter()
        .filter_map(|chunk| chunk.get("usage"))
        .find(|usage| !usage.is_null())
        .unwrap_or_else(|| panic!("no chunk carried usage despite include_usage: {chunks:?}"));
    assert_eq!(usage["prompt_tokens"], 7, "usage: {usage}");
    assert_eq!(usage["completion_tokens"], 5, "usage: {usage}");
}

#[test]
#[ignore = "needs PostgreSQL, Valkey, and the olp binary; run via make e2e"]
fn openai_stream_preserves_carriage_returns_in_text() {
    let text = openai_stream_text(report().call("openai_chat_stream_cr"));
    assert_eq!(
        text,
        mock_upstream::CR_TEXT,
        "text with embedded CRLF must survive the full proxy path byte-exactly"
    );
}

// ---------------------------------------------------------------------------
// Anthropic surface (translated through the Azure deployment)
// ---------------------------------------------------------------------------

#[test]
#[ignore = "needs PostgreSQL, Valkey, and the olp binary; run via make e2e"]
fn anthropic_unary_returns_message() {
    let call = report().call("anthropic_unary");
    assert_eq!(
        call.status,
        200,
        "body: {}",
        String::from_utf8_lossy(&call.body)
    );
    let body = call.json();
    assert_eq!(body["type"], "message", "body: {body}");
    assert_eq!(body["role"], "assistant", "body: {body}");
    assert_eq!(
        body["content"][0]["text"],
        mock_upstream::PLAIN_TEXT,
        "body: {body}"
    );
}

#[test]
#[ignore = "needs PostgreSQL, Valkey, and the olp binary; run via make e2e"]
fn anthropic_unary_passes_usage_through_exactly() {
    let body = report().call("anthropic_unary").json();
    assert_eq!(body["usage"]["input_tokens"], 7, "usage: {}", body["usage"]);
    assert_eq!(
        body["usage"]["output_tokens"], 5,
        "usage: {}",
        body["usage"]
    );
}

#[test]
#[ignore = "needs PostgreSQL, Valkey, and the olp binary; run via make e2e"]
fn anthropic_stream_follows_documented_event_order() {
    let call = report().call("anthropic_stream");
    assert_eq!(
        call.status,
        200,
        "body: {}",
        String::from_utf8_lossy(&call.body)
    );
    let stream = call.sse().expect("stream must decode as SSE");
    let names: Vec<&str> = stream
        .events
        .iter()
        .map(|event| event.event.as_str())
        .filter(|name| *name != "ping")
        .collect();
    assert!(
        names.first() == Some(&"message_start"),
        "stream must open with message_start: {names:?}"
    );
    assert!(
        names.last() == Some(&"message_stop"),
        "stream must close with message_stop: {names:?}"
    );
    let start_index = names.iter().position(|name| *name == "content_block_start");
    let delta_index = names.iter().position(|name| *name == "content_block_delta");
    let stop_index = names.iter().position(|name| *name == "content_block_stop");
    match (start_index, delta_index, stop_index) {
        (Some(start), Some(delta), Some(stop)) => {
            assert!(
                start < delta && delta < stop,
                "content block events out of order: {names:?}"
            );
        }
        _ => panic!("missing content block events: {names:?}"),
    }
}

#[test]
#[ignore = "needs PostgreSQL, Valkey, and the olp binary; run via make e2e"]
fn anthropic_stream_event_names_match_payload_types() {
    let stream = report()
        .call("anthropic_stream")
        .sse()
        .expect("stream must decode as SSE");
    for event in &stream.events {
        let payload: Value = serde_json::from_str(&event.data)
            .unwrap_or_else(|error| panic!("event data is not JSON ({error}): {event:?}"));
        assert_eq!(
            payload["type"], *event.event,
            "event name and payload type diverge: {event:?}"
        );
    }
}

#[test]
#[ignore = "needs PostgreSQL, Valkey, and the olp binary; run via make e2e"]
fn anthropic_stream_preserves_text_exactly() {
    let stream = report()
        .call("anthropic_stream")
        .sse()
        .expect("stream must decode as SSE");
    let text: String = stream
        .events
        .iter()
        .filter(|event| event.event == "content_block_delta")
        .filter_map(|event| serde_json::from_str::<Value>(&event.data).ok())
        .filter_map(|payload| payload["delta"]["text"].as_str().map(str::to_owned))
        .collect();
    assert_eq!(text, mock_upstream::PLAIN_TEXT);
}

// ---------------------------------------------------------------------------
// Gemini surface (translated through the Azure deployment)
// ---------------------------------------------------------------------------

#[test]
#[ignore = "needs PostgreSQL, Valkey, and the olp binary; run via make e2e"]
fn gemini_unary_returns_candidates() {
    let call = report().call("gemini_unary");
    assert_eq!(
        call.status,
        200,
        "body: {}",
        String::from_utf8_lossy(&call.body)
    );
    let body = call.json();
    assert_eq!(
        body["candidates"][0]["content"]["parts"][0]["text"],
        mock_upstream::PLAIN_TEXT,
        "body: {body}"
    );
}

#[test]
#[ignore = "needs PostgreSQL, Valkey, and the olp binary; run via make e2e"]
fn gemini_unary_reports_usage_metadata() {
    let body = report().call("gemini_unary").json();
    let usage = &body["usageMetadata"];
    assert_eq!(usage["promptTokenCount"], 7, "usageMetadata: {usage}");
    assert_eq!(usage["candidatesTokenCount"], 5, "usageMetadata: {usage}");
    assert_eq!(usage["totalTokenCount"], 12, "usageMetadata: {usage}");
}

#[test]
#[ignore = "needs PostgreSQL, Valkey, and the olp binary; run via make e2e"]
fn gemini_stream_is_valid_sse() {
    let call = report().call("gemini_stream");
    assert_eq!(
        call.status,
        200,
        "body: {}",
        String::from_utf8_lossy(&call.body)
    );
    assert!(
        call.content_type.starts_with("text/event-stream"),
        "content-type: {}",
        call.content_type
    );
    let stream = call.sse().expect("stream must decode as SSE");
    assert!(!stream.events.is_empty(), "stream produced no events");
    assert!(
        stream.undispatched_tail.is_empty(),
        "stream ended mid-event: {:?}",
        stream.undispatched_tail
    );
}

#[test]
#[ignore = "needs PostgreSQL, Valkey, and the olp binary; run via make e2e"]
fn gemini_stream_preserves_text_exactly() {
    let stream = report()
        .call("gemini_stream")
        .sse()
        .expect("stream must decode as SSE");
    let text: String = stream
        .events
        .iter()
        .filter_map(|event| serde_json::from_str::<Value>(&event.data).ok())
        .filter_map(|payload| {
            payload["candidates"][0]["content"]["parts"][0]["text"]
                .as_str()
                .map(str::to_owned)
        })
        .collect();
    assert_eq!(text, mock_upstream::PLAIN_TEXT);
}

// ---------------------------------------------------------------------------
// Upstream fidelity
// ---------------------------------------------------------------------------

#[test]
#[ignore = "needs PostgreSQL, Valkey, and the olp binary; run via make e2e"]
fn upstream_receives_real_compat_credential() {
    let report = report();
    let chat_requests: Vec<_> = report
        .upstream_requests
        .iter()
        .filter(|request| request.path == "/v1/chat/completions")
        .collect();
    assert!(
        !chat_requests.is_empty(),
        "compat upstream never saw a chat call"
    );
    for request in chat_requests {
        assert_eq!(
            request.authorization.as_deref(),
            Some(&*format!("Bearer {}", mock_upstream::COMPAT_CREDENTIAL)),
            "upstream authorization: {request:?}"
        );
    }
}

#[test]
#[ignore = "needs PostgreSQL, Valkey, and the olp binary; run via make e2e"]
fn upstream_receives_azure_api_key_and_api_version() {
    let report = report();
    let azure_path = format!(
        "/openai/deployments/{}/chat/completions",
        mock_upstream::DEPLOYMENT
    );
    let azure_requests: Vec<_> = report
        .upstream_requests
        .iter()
        .filter(|request| request.path == azure_path)
        .collect();
    assert!(
        !azure_requests.is_empty(),
        "azure upstream never saw a chat call"
    );
    for request in azure_requests {
        assert_eq!(
            request.api_key_header.as_deref(),
            Some(mock_upstream::AZURE_CREDENTIAL),
            "azure api-key header: {request:?}"
        );
        let query = request.query.as_deref().unwrap_or_default();
        assert!(
            query.contains(&format!("api-version={}", mock_upstream::API_VERSION)),
            "azure api-version query: {request:?}"
        );
    }
}

#[test]
#[ignore = "needs PostgreSQL, Valkey, and the olp binary; run via make e2e"]
fn upstream_receives_provider_model_not_route_slug() {
    let report = report();
    for request in report
        .upstream_requests
        .iter()
        .filter(|request| request.method == "POST" && request.body.get("model").is_some())
    {
        let model = request.body["model"].as_str().unwrap_or_default();
        assert!(
            model != OPENAI_ROUTE && model != CROSS_ROUTE,
            "route slug leaked upstream: {request:?}"
        );
    }
    let compat_chats = report
        .upstream_requests
        .iter()
        .filter(|request| request.path == "/v1/chat/completions")
        .filter(|request| {
            request.body.get("stream").is_some() || request.body.get("model").is_some()
        });
    for request in compat_chats {
        assert_eq!(
            request.body["model"],
            mock_upstream::MODEL,
            "compat upstream must receive the provider model: {request:?}"
        );
    }
}

#[test]
#[ignore = "needs PostgreSQL, Valkey, and the olp binary; run via make e2e"]
fn upstream_sees_no_unexpected_requests() {
    let report = report();
    assert!(
        report.upstream_unexpected.is_empty(),
        "unexpected upstream requests: {:?}",
        report.upstream_unexpected
    );
}

// ---------------------------------------------------------------------------
// Negative paths
// ---------------------------------------------------------------------------

#[test]
#[ignore = "needs PostgreSQL, Valkey, and the olp binary; run via make e2e"]
fn invalid_api_key_yields_openai_shaped_unauthorized() {
    // Inference surfaces speak each vendor's error dialect by design (the
    // management API is the RFC 7807 plane), so an OpenAI-surface auth
    // failure must be an OpenAI error envelope.
    let call = report().call("negative_bad_key");
    assert_eq!(
        call.status,
        401,
        "body: {}",
        String::from_utf8_lossy(&call.body)
    );
    let body = call.json();
    assert!(
        body["error"]["message"].is_string(),
        "expected an OpenAI error envelope: {body}"
    );
    assert!(
        body["error"]["type"].is_string(),
        "expected an OpenAI error envelope: {body}"
    );
}

#[test]
#[ignore = "needs PostgreSQL, Valkey, and the olp binary; run via make e2e"]
fn route_outside_key_allowlist_is_denied() {
    // The journey key allowlists only the two journey routes; a slug outside
    // the allowlist must be denied with an OpenAI-shaped permission error.
    let call = report().call("negative_unknown_route");
    assert_eq!(
        call.status,
        403,
        "body: {}",
        String::from_utf8_lossy(&call.body)
    );
    let body = call.json();
    assert_eq!(
        body["error"]["code"], "permission_denied",
        "expected a permission error envelope: {body}"
    );
}

// ---------------------------------------------------------------------------
// Operations plane
// ---------------------------------------------------------------------------

fn request_rows(report: &JourneyReport) -> Vec<Value> {
    report.require(&report.requests_api, "GET /api/v1/requests")["data"]
        .as_array()
        .cloned()
        .unwrap_or_default()
}

#[test]
#[ignore = "needs PostgreSQL, Valkey, and the olp binary; run via make e2e"]
fn request_log_records_every_successful_call() {
    let report = report();
    let rows = request_rows(report);
    // The convergence poll also logs `model_list` rows; the contract under
    // test is the seven generation calls.
    let successful: Vec<_> = rows
        .iter()
        .filter(|row| row["status_code"] == 200 && row["operation"] == "generation")
        .collect();
    assert!(
        successful.len() >= 7,
        "expected the 7 successful inference calls in /api/v1/requests, got {} rows: {rows:?}",
        successful.len()
    );
    let openai_rows = successful
        .iter()
        .filter(|row| row["route"] == OPENAI_ROUTE)
        .count();
    let cross_rows = successful
        .iter()
        .filter(|row| row["route"] == CROSS_ROUTE)
        .count();
    assert_eq!(openai_rows, 3, "rows for {OPENAI_ROUTE}: {rows:?}");
    assert_eq!(cross_rows, 4, "rows for {CROSS_ROUTE}: {rows:?}");
    for row in &successful {
        assert_eq!(row["operation"], "generation", "row: {row}");
    }
}

#[test]
#[ignore = "needs PostgreSQL, Valkey, and the olp binary; run via make e2e"]
fn request_log_usage_matches_upstream_usage() {
    let report = report();
    for row in request_rows(report)
        .iter()
        .filter(|row| row["status_code"] == 200 && row["operation"] == "generation")
    {
        assert_eq!(row["input_tokens"], 7, "row: {row}");
        assert_eq!(row["output_tokens"], 5, "row: {row}");
        assert_eq!(row["usage_complete"], true, "row: {row}");
    }
}

#[test]
#[ignore = "needs PostgreSQL, Valkey, and the olp binary; run via make e2e"]
fn usage_summary_totals_match_mock_usage() {
    let report = report();
    let summary = report.require(&report.usage_summary, "GET /api/v1/usage/summary");
    let input: i64 = summary["input_tokens"]
        .as_str()
        .and_then(|value| value.parse().ok())
        .unwrap_or_else(|| panic!("input_tokens is not a decimal string: {summary}"));
    let output: i64 = summary["output_tokens"]
        .as_str()
        .and_then(|value| value.parse().ok())
        .unwrap_or_else(|| panic!("output_tokens is not a decimal string: {summary}"));
    assert_eq!(input, 7 * 7, "summary: {summary}");
    assert_eq!(output, 5 * 7, "summary: {summary}");
    assert!(
        summary["request_count"].as_i64().unwrap_or(0) >= 7,
        "summary: {summary}"
    );
}

#[test]
#[ignore = "needs PostgreSQL, Valkey, and the olp binary; run via make e2e"]
fn audit_trail_records_configuration_lifecycle() {
    let report = report();
    let audit = report.require(&report.audit, "GET /api/v1/audit");
    let entries = audit["data"].as_array().cloned().unwrap_or_default();
    assert!(!entries.is_empty(), "audit trail is empty: {audit}");
    let mentions = |needle: &str| {
        entries.iter().any(|entry| {
            entry["action"]
                .as_str()
                .unwrap_or_default()
                .contains(needle)
                || entry["resource_type"]
                    .as_str()
                    .unwrap_or_default()
                    .contains(needle)
        })
    };
    assert!(mentions("provider"), "no provider audit entry: {entries:?}");
    assert!(mentions("route"), "no route audit entry: {entries:?}");
    assert!(mentions("key"), "no api-key audit entry: {entries:?}");
    for entry in &entries {
        assert!(
            entry.get("source_ip").is_none() && entry.get("user_agent_family").is_none(),
            "audit entries must not expose client fingerprints: {entry}"
        );
    }
}
