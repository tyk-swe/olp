use olp_engine::domain::canonical::requests::Operation;
use olp_engine::protocols::{
    anthropic::{
        dto::MessagesRequest,
        translate::{
            decode::request as decode_anthropic_request,
            encode::request as encode_anthropic_request,
        },
    },
    gemini::{
        dto::GenerateContentRequest,
        translate::{
            decode::request as decode_gemini_request, encode::request as encode_gemini_request,
        },
    },
    openai::chat::{CompletionRequest, decode, encode},
};
use proptest::prelude::*;
use serde_json::{Map, Value, json};

fn extension_value() -> impl Strategy<Value = Value> {
    prop_oneof![
        any::<bool>().prop_map(Value::Bool),
        any::<i32>().prop_map(|value| Value::from(i64::from(value))),
        "[^\\p{C}]{0,96}".prop_map(Value::String),
    ]
}

fn with_extension(mut request: Value, name: &str, value: Value) -> Value {
    request
        .as_object_mut()
        .expect("the test request is an object")
        .insert(name.to_owned(), value);
    request
}

fn object(value: impl serde::Serialize) -> Map<String, Value> {
    serde_json::to_value(value)
        .expect("the encoded DTO serializes")
        .as_object()
        .expect("the encoded DTO is an object")
        .clone()
}

proptest! {
    #![proptest_config(ProptestConfig::with_cases(64))]

    /// Source-scoped vendor fields must survive an OpenAI -> canonical ->
    /// OpenAI translation. This is deliberately property-based: the codec
    /// cannot rely on a small allow-list of extension names or scalar values.
    #[test]
    fn openai_chat_preserves_arbitrary_source_extension(
        suffix in "[a-z][a-z0-9_]{0,31}",
        value in extension_value(),
    ) {
        let name = format!("x_olp_property_{suffix}");
        let request = with_extension(json!({
            "model": "public-route",
            "messages": [{"role": "user", "content": "hello"}]
        }), &name, value.clone());
        let request: CompletionRequest = serde_json::from_value(request).unwrap();
        let Operation::Generation(canonical) = decode::chat_completion(request).unwrap() else {
            unreachable!("chat decoding always creates generation")
        };
        let encoded = encode::chat_completion(&canonical, "provider-model").unwrap();
        let encoded = object(encoded);
        prop_assert_eq!(encoded.get(&name), Some(&value));
    }

    /// Anthropic extensions have the same lossless same-surface guarantee.
    #[test]
    fn anthropic_messages_preserves_arbitrary_source_extension(
        suffix in "[a-z][a-z0-9_]{0,31}",
        value in extension_value(),
    ) {
        let name = format!("x_olp_property_{suffix}");
        let request = with_extension(json!({
            "model": "public-route",
            "max_tokens": 32,
            "messages": [{"role": "user", "content": "hello"}]
        }), &name, value.clone());
        let request: MessagesRequest = serde_json::from_value(request).unwrap();
        let Operation::Generation(canonical) = decode_anthropic_request(request).unwrap() else {
            unreachable!("message decoding always creates generation")
        };
        let encoded = encode_anthropic_request(&canonical, "provider-model").unwrap();
        let encoded = object(encoded);
        prop_assert_eq!(encoded.get(&name), Some(&value));
    }

    /// Gemini uses camelCase DTOs but must retain unknown top-level fields
    /// exactly when it is also the destination surface.
    #[test]
    fn gemini_generate_content_preserves_arbitrary_source_extension(
        suffix in "[a-z][a-z0-9_]{0,31}",
        value in extension_value(),
    ) {
        let name = format!("x_olp_property_{suffix}");
        let request = with_extension(json!({
            "contents": [{"role": "user", "parts": [{"text": "hello"}]}]
        }), &name, value.clone());
        let request: GenerateContentRequest = serde_json::from_value(request).unwrap();
        let Operation::Generation(canonical) =
            decode_gemini_request("public-route", request, false).unwrap()
        else {
            unreachable!("generateContent decoding always creates generation")
        };
        let encoded = encode_gemini_request(&canonical).unwrap();
        let encoded = object(encoded);
        prop_assert_eq!(encoded.get(&name), Some(&value));
    }
}
