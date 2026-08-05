use super::*;

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
