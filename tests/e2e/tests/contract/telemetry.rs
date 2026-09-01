use super::*;

const REQUEST_TRACE_ATTRIBUTE_KEYS: &[&str] = &[
    "olp.request_id",
    "olp.surface",
    "olp.operation",
    "olp.route_slug",
    "olp.key_id",
    "olp.installation_id",
    "olp.generation",
    "olp.status",
    "olp.error_class",
    "olp.attempt_count",
    "olp.time_to_first_byte_ms",
    "olp.total_duration_ms",
    "olp.cancelled",
];
const ATTEMPT_TRACE_ATTRIBUTE_KEYS: &[&str] = &[
    "olp.provider_kind",
    "olp.provider_revision",
    "olp.model",
    "olp.outcome_class",
    "olp.upstream_status_class",
    "olp.usage.input_tokens",
    "olp.usage.output_tokens",
    "olp.usage.cached_input_tokens",
    "olp.usage.media_units",
    "olp.pricing_provenance",
];
// Deliberately restated rather than imported: `assert_trace_allowlist` is a
// subset check, so importing the engine constant would let a newly exported
// attribute pass unnoticed. `trace_allowlists_match_the_engine` keeps the two
// from drifting apart silently.
const TRACE_RESOURCE_ATTRIBUTE_KEYS: [&str; 3] =
    ["olp.process.mode", "service.name", "service.version"];

// ---------------------------------------------------------------------------
// Telemetry
//
// docs/architecture.md "Data-safety invariants": one bounded terminal metadata
// envelope per request, and "Missing upstream usage is incomplete and
// unpriced, never zero".
// ---------------------------------------------------------------------------

#[test]
#[ignore = "end-to-end; run via make e2e"]
fn every_request_is_recorded_exactly_once() {
    runtime().block_on(async {
        let world = world();
        let key = world
            .issue_key("telemetry probe", json!({}))
            .await
            .expect("dedicated key");
        const CALLS: usize = 3;

        for index in 0..CALLS {
            let response = world
                .gateway_post(
                    "/openai/v1/chat/completions",
                    json!({
                        "model": world::OPENAI_ROUTE,
                        "messages": [{"role": "user", "content": nonce(&format!("count-{index}"))}]
                    }),
                    &key.secret,
                )
                .await
                .expect("chat completion");
            assert_eq!(
                response.status, 200,
                "call {index} failed: {}",
                response.text
            );
        }

        let rows = world
            .await_request_rows(&key.id, &route_filter(), CALLS)
            .await
            .expect("requests are logged");
        assert_eq!(
            rows.len(),
            CALLS,
            "{CALLS} requests produced {} log rows",
            rows.len()
        );

        for row in &rows {
            assert_eq!(row["route"], json!(world::OPENAI_ROUTE), "row: {row}");
            assert_eq!(row["surface"], json!("openai"), "row: {row}");
            assert_eq!(
                row["attempt_count"],
                json!(1),
                "a request that succeeded first time recorded {} attempts: {row}",
                row["attempt_count"]
            );
            assert_eq!(row["status_code"], json!(200), "row: {row}");
            assert_eq!(
                row["usage_complete"],
                json!(true),
                "the upstream reported usage, so the record must be complete: {row}"
            );
            assert_eq!(
                row["input_tokens"],
                json!(mock_upstream::PROMPT_TOKENS),
                "row: {row}"
            );
            assert_eq!(
                row["output_tokens"],
                json!(mock_upstream::COMPLETION_TOKENS),
                "row: {row}"
            );
            assert!(
                row["first_byte_ms"].is_number(),
                "a completed request must record time to first byte: {row}"
            );
        }

        let (start, end) = usage_window();
        let summary = world
            .management
            .get(&format!(
                "/api/v1/usage/summary?start={start}&end={end}&api_key_id={}",
                key.id
            ))
            .await
            .expect("usage summary");
        assert_eq!(
            summary.status, 200,
            "GET /api/v1/usage/summary returned {}: {}",
            summary.status, summary.body
        );
        assert_eq!(
            summary.body["request_count"],
            json!(CALLS),
            "usage summary: {}",
            summary.body
        );
        assert_eq!(
            summary.body["incomplete_count"],
            json!(0),
            "every call reported usage, so none is incomplete: {}",
            summary.body
        );
        assert_eq!(
            summary.body["input_tokens"],
            json!((mock_upstream::PROMPT_TOKENS * CALLS as u64).to_string()),
            "usage summary: {}",
            summary.body
        );
        assert_eq!(
            summary.body["output_tokens"],
            json!((mock_upstream::COMPLETION_TOKENS * CALLS as u64).to_string()),
            "usage summary: {}",
            summary.body
        );
    });
}

#[test]
#[ignore = "end-to-end; run via make e2e"]
fn a_request_covered_by_a_pricing_revision_is_priced() {
    // The control for the assertion below. docs/architecture.md "Data-safety
    // invariants" says durable records carry "pricing provenance" and that
    // "Missing upstream usage is incomplete and unpriced, never zero" — a claim
    // about the *missing* case, which only means something if the present case
    // differs. Without this test, `unpriced` hard-wired to `true` would satisfy
    // the missing-usage assertion and nothing would notice that no request is
    // ever priced.
    runtime().block_on(async {
        let world = world();
        let key = world
            .issue_key("priced probe", json!({}))
            .await
            .expect("dedicated key");
        let response = world
            .gateway_post(
                "/openai/v1/chat/completions",
                json!({
                    "model": world::OPENAI_ROUTE,
                    "messages": [{"role": "user", "content": nonce("priced")}]
                }),
                &key.secret,
            )
            .await
            .expect("chat completion");
        assert_eq!(response.status, 200, "chat completion: {}", response.text);

        let rows = world
            .await_request_rows(&key.id, &route_filter(), 1)
            .await
            .expect("the request is logged");
        assert_eq!(rows.len(), 1, "expected one row, got {}", rows.len());
        let row = &rows[0];

        assert_eq!(
            row["usage_complete"],
            json!(true),
            "the upstream reported complete usage: {row}"
        );
        assert_eq!(
            row["unpriced"],
            json!(false),
            "usage arrived complete and a pricing revision covers this \
             provider kind, model and operation, so the record must not be \
             unpriced: {row}"
        );
        assert!(
            row["estimated_cost"].is_string(),
            "a priced request must carry its cost: {row}"
        );
        assert_eq!(
            row["currency"],
            json!("USD"),
            "the record must carry the currency its price was quoted in: {row}"
        );
    });
}

#[test]
#[ignore = "end-to-end; run via make e2e"]
fn missing_upstream_usage_is_incomplete_and_unpriced_never_zero() {
    // docs/architecture.md "Data-safety invariants", verbatim: "Missing
    // upstream usage is incomplete and unpriced, never zero." A record that
    // claims complete, priced, zero-token usage understates real spend and
    // cannot be told apart from a genuinely free request.
    runtime().block_on(async {
        let world = world();
        let key = world
            .issue_key("unpriced probe", json!({}))
            .await
            .expect("dedicated key");

        let response = world
            .gateway_post(
                "/openai/v1/chat/completions",
                json!({
                    "model": world::OPENAI_ROUTE,
                    "messages": [{
                        "role": "user",
                        "content": format!("{} {}", mock_upstream::NO_USAGE_MARKER, nonce("no-usage"))
                    }]
                }),
                &key.secret,
            )
            .await
            .expect("chat completion");
        assert_eq!(
            response.status, 200,
            "the upstream answered without usage, which is still a successful \
             completion: {}",
            response.text
        );

        let rows = world
            .await_request_rows(&key.id, &route_filter(), 1)
            .await
            .expect("the request is logged");
        assert_eq!(rows.len(), 1, "expected one row, got {}", rows.len());
        let row = &rows[0];

        assert_eq!(
            row["usage_complete"],
            json!(false),
            "the upstream reported no usage, so the record must be incomplete: {row}"
        );
        assert_eq!(
            row["unpriced"],
            json!(true),
            "incomplete usage must be recorded unpriced: {row}"
        );
        assert!(
            row.get("input_tokens").is_none_or(Value::is_null),
            "missing usage must leave input tokens absent/null: {row}"
        );
        assert!(
            row.get("output_tokens").is_none_or(Value::is_null),
            "missing usage must leave output tokens absent/null: {row}"
        );

        let (start, end) = usage_window();
        let completeness = world
            .management
            .get(&format!(
                "/api/v1/usage/completeness?start={start}&end={end}&api_key_id={}",
                key.id
            ))
            .await
            .expect("usage completeness");
        assert_eq!(
            completeness.status, 200,
            "GET /api/v1/usage/completeness returned {}: {}",
            completeness.status, completeness.body
        );
        assert_eq!(
            completeness.body["incomplete_count"],
            json!(1),
            "a request with no upstream usage must count as incomplete: {}",
            completeness.body
        );
        assert_eq!(
            completeness.body["unpriced_count"],
            json!(1),
            "a request with no upstream usage must count as unpriced: {}",
            completeness.body
        );
        assert_eq!(
            completeness.body["complete"],
            json!(false),
            "a range holding an incomplete request is not complete: {}",
            completeness.body
        );
    });
}

#[test]
#[ignore = "end-to-end; run via make e2e"]
fn streamed_failover_exports_one_content_free_linked_trace() {
    runtime().block_on(async {
        let proof = run_failover_trace(world()).await;
        assert_request_span(&proof);
        assert_attempt_spans(&proof);
        assert_upstream_propagation(&proof);
        assert_trace_has_no_content(&proof);
    });
}

struct FailoverTraceProof {
    inbound: otlp::InboundTrace,
    prompt_secret: String,
    malicious_request_id: String,
    request: otlp::CollectedSpan,
    attempts: [otlp::CollectedSpan; 2],
    upstream: [mock_upstream::RecordedRequest; 2],
}

async fn run_failover_trace(world: &world::World) -> FailoverTraceProof {
    let inbound = otlp::inbound_trace();
    let prompt_secret = nonce("secret-trace-prompt");
    let malicious_request_id = nonce("secret-trace-request-id");
    let prompt = format!("{} {prompt_secret}", mock_upstream::TRACE_FAILOVER_MARKER);
    let authorization = format!("Bearer {}", world.api_key);
    let checkpoint = world.mock.checkpoint();
    let response = world
        .gateway_send(
            reqwest::Method::POST,
            "/openai/v1/chat/completions",
            Some(json!({
                "model": world::TRACE_ROUTE,
                "stream": true,
                "messages": [{"role": "user", "content": prompt}]
            })),
            &[
                (reqwest::header::AUTHORIZATION.as_str(), &authorization),
                ("traceparent", &inbound.header),
                ("x-request-id", &malicious_request_id),
            ],
        )
        .await
        .expect("streamed failover request");
    assert_eq!(response.status, 200, "streamed failover: {}", response.text);
    assert!(
        response
            .header("content-type")
            .is_some_and(|value| value.starts_with("text/event-stream"))
    );
    assert!(response.text.contains("data: [DONE]"));

    let upstream = world
        .mock
        .since(checkpoint)
        .try_into()
        .unwrap_or_else(|calls: Vec<_>| panic!("expected two provider calls: {calls:#?}"));
    let mut spans = world
        .otlp()
        .await_trace(&inbound.trace_id, 3, std::time::Duration::from_secs(15))
        .await
        .expect("trace export");
    assert_eq!(spans.len(), 3, "unexpected trace span count");
    let request_index = spans
        .iter()
        .position(|span| span.string_attribute("olp.surface").is_some())
        .expect("request span");
    let request = spans.remove(request_index);
    spans.sort_by_key(|span| span.span.start_time_unix_nano);
    let attempts = spans
        .try_into()
        .unwrap_or_else(|spans: Vec<_>| panic!("expected two attempt spans, got {}", spans.len()));
    FailoverTraceProof {
        inbound,
        prompt_secret,
        malicious_request_id,
        request,
        attempts,
        upstream,
    }
}

fn assert_request_span(proof: &FailoverTraceProof) {
    let request = &proof.request;
    assert_eq!(request.span.name, "request");
    assert_eq!(request.span.trace_id, proof.inbound.trace_id);
    assert_eq!(request.span.parent_span_id, proof.inbound.parent_span_id);
    assert_eq!(request.string_attribute("olp.request_id"), None);
    assert_eq!(request.string_attribute("olp.surface"), Some("openai"));
    assert_eq!(
        request.string_attribute("olp.operation"),
        Some("generation")
    );
    assert_eq!(
        request.string_attribute("olp.route_slug"),
        Some(world::TRACE_ROUTE)
    );
    assert_eq!(request.integer_attribute("olp.status"), Some(200));
    assert_eq!(request.integer_attribute("olp.attempt_count"), Some(2));
    assert_eq!(
        request.resource_attribute("service.name"),
        Some("openllmproxy")
    );
    assert_trace_allowlist(request, REQUEST_TRACE_ATTRIBUTE_KEYS);
    assert!(
        request.span.end_time_unix_nano
            >= proof
                .attempts
                .iter()
                .map(|span| span.span.end_time_unix_nano)
                .max()
                .unwrap(),
        "request span ended before its final provider attempt"
    );
}

fn assert_attempt_spans(proof: &FailoverTraceProof) {
    for attempt in &proof.attempts {
        assert_eq!(attempt.span.name, "attempt");
        assert_eq!(attempt.span.trace_id, proof.inbound.trace_id);
        assert_eq!(attempt.span.parent_span_id, proof.request.span.span_id);
        assert_trace_allowlist(attempt, ATTEMPT_TRACE_ATTRIBUTE_KEYS);
    }
    assert_ne!(
        proof.attempts[0].span.span_id,
        proof.attempts[1].span.span_id
    );
    for (attempt, provider, model, outcome, status) in [
        (
            &proof.attempts[0],
            "openai_compatible",
            mock_upstream::MODEL,
            "rate_limit",
            "4xx",
        ),
        (
            &proof.attempts[1],
            "azure_openai",
            mock_upstream::DEPLOYMENT,
            "success",
            "2xx",
        ),
    ] {
        assert_eq!(
            attempt.string_attribute("olp.provider_kind"),
            Some(provider)
        );
        assert_eq!(attempt.string_attribute("olp.model"), Some(model));
        let revision = attempt
            .string_attribute("olp.provider_revision")
            .expect("attempt span provider revision");
        revision
            .parse::<olp_engine::domain::ids::ProviderId>()
            .expect("provider revision is a UUID");
        assert_eq!(attempt.string_attribute("olp.outcome_class"), Some(outcome));
        assert_eq!(
            attempt.string_attribute("olp.upstream_status_class"),
            Some(status)
        );
    }
    assert_eq!(
        proof.attempts[1].integer_attribute("olp.usage.input_tokens"),
        Some(i64::try_from(mock_upstream::PROMPT_TOKENS).unwrap())
    );
    assert_eq!(
        proof.attempts[1].integer_attribute("olp.usage.output_tokens"),
        Some(i64::try_from(mock_upstream::COMPLETION_TOKENS).unwrap())
    );
}

fn assert_upstream_propagation(proof: &FailoverTraceProof) {
    assert_eq!(proof.upstream[0].path, "/v1/chat/completions");
    assert_eq!(
        proof.upstream[1].path,
        format!(
            "/openai/deployments/{}/chat/completions",
            mock_upstream::DEPLOYMENT
        )
    );
    let trace_id = otlp::hex_id(&proof.inbound.trace_id);
    for (call, attempt) in proof.upstream.iter().zip(&proof.attempts) {
        let traceparent = call
            .header("traceparent")
            .unwrap_or_else(|| panic!("provider call lacks traceparent: {call:#?}"));
        let fields: Vec<&str> = traceparent.split('-').collect();
        assert_eq!(
            fields.len(),
            4,
            "invalid provider traceparent: {traceparent}"
        );
        assert_eq!(fields[0], "00");
        assert_eq!(fields[1], trace_id);
        assert_eq!(fields[2], otlp::hex_id(&attempt.span.span_id));
        assert_eq!(fields[3], "01");
        assert!(
            !call.headers.iter().any(|(_, value)| {
                value.contains(&proof.prompt_secret) || value.contains(&proof.malicious_request_id)
            }),
            "content reached provider headers: {:#?}",
            call.headers
        );
    }
}

fn assert_trace_has_no_content(proof: &FailoverTraceProof) {
    let forbidden = [
        proof.prompt_secret.as_str(),
        proof.malicious_request_id.as_str(),
        mock_upstream::PLAIN_TEXT,
        mock_upstream::PLAIN_DELTAS[0],
        mock_upstream::PLAIN_DELTAS[2],
    ];
    for span in std::iter::once(&proof.request).chain(&proof.attempts) {
        assert!(span.span.events.is_empty(), "trace span exported events");
        assert!(span.span.links.is_empty(), "trace span exported links");
        assert!(
            !span.contains_any_text(&forbidden),
            "request or response content reached exported span {}",
            span.span.name
        );
    }
}

/// The restated allowlists above are only trustworthy while they agree with
/// what the engine actually exports.
#[test]
fn trace_allowlists_match_the_engine() {
    assert_eq!(
        REQUEST_TRACE_ATTRIBUTE_KEYS,
        olp_engine::inference::tracing::REQUEST_ATTRIBUTE_KEYS
    );
    assert_eq!(
        ATTEMPT_TRACE_ATTRIBUTE_KEYS,
        olp_engine::inference::tracing::ATTEMPT_ATTRIBUTE_KEYS
    );
}

fn assert_trace_allowlist(span: &otlp::CollectedSpan, allowed_attributes: &[&str]) {
    let allowed: std::collections::BTreeSet<_> = allowed_attributes.iter().copied().collect();
    let actual = span.attribute_keys();
    let unexpected: Vec<_> = actual.difference(&allowed).copied().collect();
    assert!(
        unexpected.is_empty(),
        "span {} exported attributes outside its allowlist: {unexpected:?}",
        span.span.name
    );
    assert_eq!(
        span.resource_attribute_keys(),
        std::collections::BTreeSet::from(TRACE_RESOURCE_ATTRIBUTE_KEYS),
        "span {} exported unexpected resource attributes",
        span.span.name
    );
}
