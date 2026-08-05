use super::*;

// ---------------------------------------------------------------------------
// Gateway journey
//
// README.md "Interfaces" lists three client surfaces on one origin;
// docs/architecture.md "Canonical endpoint and provider policy" binds each to
// a typed operation, so a request on any surface must reach the same upstream
// provider and come back in that surface's own dialect.
// ---------------------------------------------------------------------------

#[test]
#[ignore = "end-to-end; run via make e2e"]
fn the_openai_surface_answers_and_translates_upstream() {
    runtime().block_on(async {
        let world = world();
        let checkpoint = world.mock.checkpoint();
        let prompt = nonce("openai-unary");

        let response = world
            .gateway_post(
                "/openai/v1/chat/completions",
                json!({
                    "model": world::OPENAI_ROUTE,
                    "messages": [{"role": "user", "content": prompt}]
                }),
                &world.api_key,
            )
            .await
            .expect("chat completion");
        assert_eq!(
            response.status, 200,
            "POST /openai/v1/chat/completions returned {}: {}",
            response.status, response.text
        );

        let body = response.json();
        assert_eq!(
            body["choices"][0]["message"]["content"],
            json!(mock_upstream::PLAIN_TEXT),
            "the upstream reply did not reach the client unchanged: {body}"
        );
        assert_eq!(body["choices"][0]["finish_reason"], json!("stop"));
        assert_eq!(
            body["usage"]["prompt_tokens"],
            json!(mock_upstream::PROMPT_TOKENS)
        );
        assert_eq!(
            body["usage"]["completion_tokens"],
            json!(mock_upstream::COMPLETION_TOKENS)
        );

        let upstream = world.mock.since(checkpoint);
        assert_eq!(
            upstream.len(),
            1,
            "one client request produced {} upstream calls: {upstream:#?}",
            upstream.len()
        );
        let call = &upstream[0];
        assert_eq!(call.method, "POST");
        assert_eq!(call.path, "/v1/chat/completions");
        assert_eq!(
            call.body["model"],
            json!(mock_upstream::MODEL),
            "the gateway sent the route slug upstream instead of the \
             provider's own model name: {}",
            call.body
        );
        assert_eq!(
            call.authorization.as_deref(),
            Some(format!("Bearer {}", mock_upstream::COMPAT_CREDENTIAL).as_str()),
            "the provider credential did not reach the upstream unchanged"
        );
        assert!(
            !call
                .headers
                .iter()
                .any(|(_, value)| value.contains(&world.api_key)),
            "the client's own API key was forwarded upstream: {:#?}",
            call.headers
        );
    });
}

#[test]
#[ignore = "end-to-end; run via make e2e"]
fn the_anthropic_surface_answers_in_the_anthropic_dialect() {
    // README.md "Interfaces": an Anthropic-compatible API at /anthropic/v1. A
    // client of that API reads `content[].text`, `stop_reason` and
    // `usage.input_tokens`; answering in another dialect is not compatibility.
    runtime().block_on(async {
        let world = world();
        let checkpoint = world.mock.checkpoint();
        let prompt = nonce("anthropic-unary");

        let response = world
            .gateway_send(
                reqwest::Method::POST,
                "/anthropic/v1/messages",
                Some(json!({
                    "model": world::CROSS_ROUTE,
                    "max_tokens": 64,
                    "messages": [{"role": "user", "content": prompt}]
                })),
                &[
                    ("x-api-key", &world.api_key),
                    ("anthropic-version", "2023-06-01"),
                ],
            )
            .await
            .expect("anthropic message");
        assert_eq!(
            response.status, 200,
            "POST /anthropic/v1/messages returned {}: {}",
            response.status, response.text
        );

        let body = response.json();
        assert_eq!(body["type"], json!("message"));
        assert_eq!(body["role"], json!("assistant"));
        assert_eq!(
            body["content"][0]["text"],
            json!(mock_upstream::PLAIN_TEXT),
            "the reply did not reach the Anthropic client: {body}"
        );
        assert_eq!(
            body["stop_reason"],
            json!("end_turn"),
            "an ordinary completion must stop with `end_turn` in the Anthropic \
             dialect: {body}"
        );
        assert_eq!(
            body["usage"]["input_tokens"],
            json!(mock_upstream::PROMPT_TOKENS)
        );
        assert_eq!(
            body["usage"]["output_tokens"],
            json!(mock_upstream::COMPLETION_TOKENS)
        );

        let upstream = world.mock.since(checkpoint);
        assert_eq!(
            upstream.len(),
            1,
            "one client request produced {} upstream calls: {upstream:#?}",
            upstream.len()
        );
        assert_eq!(
            upstream[0].api_key_header.as_deref(),
            Some(mock_upstream::AZURE_CREDENTIAL),
            "the Azure provider credential is sent in the `api-key` header"
        );
    });
}

#[test]
#[ignore = "end-to-end; run via make e2e"]
fn the_gemini_surface_answers_in_the_gemini_dialect() {
    runtime().block_on(async {
        let world = world();
        let checkpoint = world.mock.checkpoint();
        let prompt = nonce("gemini-unary");

        let response = world
            .gateway_send(
                reqwest::Method::POST,
                &format!(
                    "/gemini/v1beta/models/{}:generateContent",
                    world::CROSS_ROUTE
                ),
                Some(json!({
                    "contents": [{"role": "user", "parts": [{"text": prompt}]}]
                })),
                &[("x-goog-api-key", &world.api_key)],
            )
            .await
            .expect("gemini generateContent");
        assert_eq!(
            response.status,
            200,
            "POST /gemini/v1beta/models/{}:generateContent returned {}: {}",
            world::CROSS_ROUTE,
            response.status,
            response.text
        );

        let body = response.json();
        assert_eq!(
            body["candidates"][0]["content"]["parts"][0]["text"],
            json!(mock_upstream::PLAIN_TEXT),
            "the reply did not reach the Gemini client: {body}"
        );
        assert_eq!(body["candidates"][0]["finishReason"], json!("STOP"));
        assert_eq!(
            body["usageMetadata"]["promptTokenCount"],
            json!(mock_upstream::PROMPT_TOKENS)
        );
        assert_eq!(
            body["usageMetadata"]["candidatesTokenCount"],
            json!(mock_upstream::COMPLETION_TOKENS)
        );
        assert_eq!(
            body["usageMetadata"]["totalTokenCount"],
            json!(mock_upstream::TOTAL_TOKENS)
        );

        assert_eq!(
            world.mock.since(checkpoint).len(),
            1,
            "one client request must produce exactly one upstream call"
        );
    });
}

#[test]
#[ignore = "end-to-end; run via make e2e"]
fn a_streamed_completion_ends_once_and_carries_the_whole_reply() {
    // docs/architecture.md "Runtime publication": a stream cannot cross a
    // generation, so one client stream is one upstream call. The event stream
    // itself is decoded with the independent WHATWG decoder in
    // `contract/sse.rs`, so a product decoder bug cannot mask a product
    // encoder bug.
    runtime().block_on(async {
        let world = world();
        let checkpoint = world.mock.checkpoint();
        let prompt = nonce("openai-stream");

        let response = world
            .gateway_post(
                "/openai/v1/chat/completions",
                json!({
                    "model": world::OPENAI_ROUTE,
                    "stream": true,
                    "messages": [{"role": "user", "content": prompt}]
                }),
                &world.api_key,
            )
            .await
            .expect("streamed chat completion");
        assert_eq!(
            response.status, 200,
            "a streaming request returned {}: {}",
            response.status, response.text
        );
        let content_type = response.header("content-type").unwrap_or_default();
        assert!(
            content_type.starts_with("text/event-stream"),
            "a streaming response must be text/event-stream; got {content_type:?}"
        );

        let stream = sse::decode(response.text.as_bytes()).expect("stream decodes");
        assert!(
            stream.undispatched_tail.is_empty(),
            "the stream ended mid-event, leaving {:?} undispatched",
            stream.undispatched_tail
        );
        let data: Vec<&str> = stream
            .events
            .iter()
            .map(|event| event.data.as_str())
            .collect();
        assert_eq!(
            data.last(),
            Some(&"[DONE]"),
            "an OpenAI-compatible stream ends with the [DONE] sentinel: {data:?}"
        );

        let chunks: Vec<Value> = data[..data.len() - 1]
            .iter()
            .map(|payload| {
                serde_json::from_str(payload)
                    .unwrap_or_else(|error| panic!("chunk {payload:?} is not JSON: {error}"))
            })
            .collect();
        assert!(!chunks.is_empty(), "the stream carried no chunks");

        let finishes: Vec<&Value> = chunks
            .iter()
            .map(|chunk| &chunk["choices"][0]["finish_reason"])
            .filter(|reason| !reason.is_null())
            .collect();
        assert_eq!(
            finishes.len(),
            1,
            "a stream must terminate exactly once; saw {finishes:?}"
        );
        assert_eq!(*finishes[0], json!("stop"));

        let text: String = chunks
            .iter()
            .filter_map(|chunk| chunk["choices"][0]["delta"]["content"].as_str())
            .collect();
        assert_eq!(
            text,
            mock_upstream::PLAIN_TEXT,
            "the concatenated deltas do not reconstruct the upstream reply"
        );

        let ids: Vec<&str> = chunks
            .iter()
            .filter_map(|chunk| chunk["id"].as_str())
            .collect();
        assert!(
            ids.windows(2).all(|pair| pair[0] == pair[1]),
            "chunk ids are not stable across one stream: {ids:?}"
        );

        assert_eq!(
            world.mock.since(checkpoint).len(),
            1,
            "one client stream must produce exactly one upstream call"
        );
    });
}
