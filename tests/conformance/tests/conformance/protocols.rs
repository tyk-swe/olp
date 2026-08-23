use olp_conformance::read_json;
use olp_engine::domain::canonical::{
    identity::Surface,
    requests::{GenerationRequest, Operation},
};
use olp_engine::protocols::{
    anthropic::{
        dto::MessagesRequest,
        translate::{decode::request as decode_anthropic, encode::request as encode_anthropic},
    },
    gemini::{
        dto::GenerateContentRequest,
        translate::{decode::request as decode_gemini, encode::request as encode_gemini},
    },
    openai::chat::{CompletionRequest, decode, encode},
};
use serde::Deserialize;

#[derive(Debug, Deserialize)]
struct ExpectedCanonical {
    route: String,
    message_count: usize,
    tool_count: usize,
    source: Surface,
    extension_paths: Vec<String>,
    upstream_model: Option<String>,
}

fn generation(operation: Operation) -> GenerationRequest {
    let Operation::Generation(request) = operation else {
        panic!("generation fixture decoded to a different operation")
    };
    request
}

fn assert_expected(request: &GenerationRequest, expected: &ExpectedCanonical) {
    assert_eq!(request.route.as_str(), expected.route);
    assert_eq!(request.messages.len(), expected.message_count);
    assert_eq!(request.tools.len(), expected.tool_count);
    assert_eq!(request.extensions.source, Some(expected.source));
    for path in &expected.extension_paths {
        assert!(
            request.extensions.values.contains_key(path),
            "missing expected extension path {path}"
        );
    }
}

#[test]
fn openai_request_fixture_translates_and_round_trips_extensions() {
    let wire: CompletionRequest = read_json("protocols/openai-chat-request.json");
    let expected: ExpectedCanonical = read_json("protocols/openai-chat-request.expected.json");
    let request = generation(decode::chat_completion(wire).expect("OpenAI fixture must decode"));
    assert_expected(&request, &expected);

    let upstream_model = expected
        .upstream_model
        .as_deref()
        .expect("model is required");
    let encoded = serde_json::to_value(
        encode::chat_completion(&request, upstream_model).expect("OpenAI fixture must encode"),
    )
    .expect("OpenAI DTO must serialize");
    assert_eq!(encoded["model"], upstream_model);
    assert_eq!(encoded["service_tier"], "priority");
}

#[test]
fn anthropic_request_fixture_translates_and_round_trips_extensions() {
    let wire: MessagesRequest = read_json("protocols/anthropic-messages-request.json");
    let expected: ExpectedCanonical =
        read_json("protocols/anthropic-messages-request.expected.json");
    let request = generation(decode_anthropic(wire).expect("Anthropic fixture must decode"));
    assert_expected(&request, &expected);

    let upstream_model = expected
        .upstream_model
        .as_deref()
        .expect("model is required");
    let encoded = serde_json::to_value(
        encode_anthropic(&request, upstream_model).expect("Anthropic fixture must encode"),
    )
    .expect("Anthropic DTO must serialize");
    assert_eq!(encoded["model"], upstream_model);
    assert_eq!(encoded["metadata"]["user_id"], "fixture-user");
}

#[test]
fn gemini_request_fixture_translates_and_round_trips_extensions() {
    let wire: GenerateContentRequest = read_json("protocols/gemini-generate-content-request.json");
    let expected: ExpectedCanonical =
        read_json("protocols/gemini-generate-content-request.expected.json");
    let request =
        generation(decode_gemini(&expected.route, wire, true).expect("Gemini fixture must decode"));
    assert_expected(&request, &expected);

    let encoded =
        serde_json::to_value(encode_gemini(&request).expect("Gemini fixture must encode"))
            .expect("Gemini DTO must serialize");
    assert_eq!(encoded["safetySettings"][0]["threshold"], "BLOCK_ONLY_HIGH");
}
