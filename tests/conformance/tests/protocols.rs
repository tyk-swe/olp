use olp_conformance_fixtures::read_json;
use olp_domain::{GenerationRequest, Operation, Surface};
use olp_protocols::{
    anthropic::{MessagesRequest, decode_messages_request, encode_messages_request},
    gemini::{
        GenerateContentRequest, decode_generate_content_request, encode_generate_content_request,
    },
    openai::{ChatCompletionRequest, decode_chat_completion, encode_chat_completion},
};
use serde::Deserialize;
use serde_json::Value;

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
    let wire: ChatCompletionRequest = read_json("protocols/openai-chat-request.json");
    let expected: ExpectedCanonical = read_json("protocols/openai-chat-request.expected.json");
    let request = generation(decode_chat_completion(wire).expect("OpenAI fixture must decode"));
    assert_expected(&request, &expected);

    let upstream_model = expected
        .upstream_model
        .as_deref()
        .expect("model is required");
    let encoded = serde_json::to_value(
        encode_chat_completion(&request, upstream_model).expect("OpenAI fixture must encode"),
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
    let request = generation(decode_messages_request(wire).expect("Anthropic fixture must decode"));
    assert_expected(&request, &expected);

    let upstream_model = expected
        .upstream_model
        .as_deref()
        .expect("model is required");
    let encoded = serde_json::to_value(
        encode_messages_request(&request, upstream_model).expect("Anthropic fixture must encode"),
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
    let request = generation(
        decode_generate_content_request(&expected.route, wire, true)
            .expect("Gemini fixture must decode"),
    );
    assert_expected(&request, &expected);

    let encoded = serde_json::to_value(
        encode_generate_content_request(&request).expect("Gemini fixture must encode"),
    )
    .expect("Gemini DTO must serialize");
    assert_eq!(encoded["safetySettings"][0]["threshold"], "BLOCK_ONLY_HIGH");
}

#[test]
fn protocol_fixture_files_are_vendor_json_objects() {
    for path in [
        "protocols/openai-chat-request.json",
        "protocols/anthropic-messages-request.json",
        "protocols/gemini-generate-content-request.json",
    ] {
        let value: Value = read_json(path);
        assert!(
            value.is_object(),
            "{path} must contain one vendor JSON object"
        );
    }
}
