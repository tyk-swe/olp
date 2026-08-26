use std::collections::BTreeMap;

use axum::body::Bytes;
use olp_engine::domain::canonical::{
    events::{Error, ErrorClass, Event, FinishReason, Kind, Usage, validate_event_sequence},
    identity::Surface,
    requests::{MessageRole, SourceExtensions},
};
use olp_engine::protocols::openai::response::{Decoder, OpenAiStreamError};
use serde_json::{Value, json};

use super::{
    OpenAiChatCompletionStreamEncoder, aggregate_chat_completion_response, set_json_pointer,
};
use crate::gateway::openai_http::unix_seconds;

mod streaming;
mod unary;

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
    set_json_pointer(
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
