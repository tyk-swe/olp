use std::collections::BTreeMap;

use crate::protocols::canonical::events::Error;
use crate::protocols::canonical::events::ErrorClass;
use crate::protocols::canonical::events::Event;
use crate::protocols::canonical::events::FinishReason;
use crate::protocols::canonical::events::Kind;
use crate::protocols::canonical::events::Usage;
use crate::protocols::canonical::events::validate_event_sequence;
use crate::protocols::canonical::identity::Surface;
use crate::protocols::canonical::requests::MessageRole;
use crate::protocols::canonical::requests::SourceExtensions;
use crate::protocols::openai::response::Decoder;
use crate::protocols::openai::response::OpenAiStreamError;
use axum::body::Bytes;
use serde_json::Value;
use serde_json::json;

use crate::inference::http::openai_chat_response::OpenAiChatCompletionStreamEncoder;
use crate::inference::http::openai_chat_response::aggregate_chat_completion_response;
use crate::inference::http::openai_http::unix_seconds;
use crate::protocols::extensions::materialize_response_pointer;

fn only_frame(mut frames: Vec<Bytes>) -> Bytes {
    assert_eq!(frames.len(), 1);
    frames.pop().unwrap()
}

fn sse_json_value(frame: &Bytes) -> Value {
    let bytes = frame.as_ref();
    assert!(bytes.starts_with(b"data: "));
    assert!(bytes.ends_with(b"\n\n"));
    serde_json::from_slice(&bytes[b"data: ".len()..bytes.len() - b"\n\n".len()]).unwrap()
}

fn assert_created_within_window_and_remove(value: &mut Value, before: i64, after: i64) -> i64 {
    let created = value
        .get("created")
        .and_then(Value::as_i64)
        .expect("OpenAI response must include an integer created timestamp");
    assert!(
        (before..=after).contains(&created),
        "created timestamp {created} was outside [{before}, {after}]"
    );
    value
        .as_object_mut()
        .expect("OpenAI response must be a JSON object")
        .remove("created");
    created
}

fn assert_sse_chunk(frame: &Bytes, before: i64, after: i64, expected: Value) -> i64 {
    let mut actual = sse_json_value(frame);
    let created = assert_created_within_window_and_remove(&mut actual, before, after);
    assert_eq!(actual, expected);
    created
}

fn join_sse_frames(frames: &[Bytes]) -> Vec<u8> {
    frames
        .iter()
        .flat_map(|frame| frame.iter().copied())
        .collect()
}

#[test]
fn source_extension_pointer_materializes_nested_arrays_without_loss() {
    let mut value = json!({ "choices": [] });
    materialize_response_pointer(
        &mut value,
        "/choices/2/delta/vendor_field",
        json!({ "preserved": true }),
    )
    .unwrap();
    assert_eq!(value["choices"][2]["index"], 2);
    assert_eq!(
        value["choices"][2]["delta"]["vendor_field"]["preserved"],
        true
    );
}

pub mod streaming;

pub mod unary;
