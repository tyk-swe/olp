use olp_engine::domain::canonical::{
    events::{FinishReason, Kind, validate_event_sequence},
    requests::{MessageRole, Operation},
};
use olp_engine::protocols::openai::{
    client::{Encoder as ResponseEncoder, encode_response_object},
    embeddings::{EmbeddingRequest, decode_embedding_request},
    responses::{
        request::{Create, decode_response_create, encode_response_create},
        response::{Object, decode_response_object},
        stream::Decoder as ResponseDecoder,
    },
};
use serde_json::json;

use super::common::responses_stream_frames;

#[test]
fn streamed_tool_call_keeps_the_call_id_not_the_item_id() {
    let wire = concat!(
        "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"model\":\"gpt-upstream\"}}\n\n",
        "data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"function_call\",\"id\":\"fc_abc\",\"call_id\":\"call_xyz\",\"name\":\"lookup\",\"arguments\":\"\"}}\n\n",
        "data: {\"type\":\"response.function_call_arguments.delta\",\"output_index\":0,\"item_id\":\"fc_abc\",\"delta\":\"{\\\"city\\\":\\\"Paris\\\"}\"}\n\n",
        "data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"function_call\"}}\n\n",
        "data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":4,\"output_tokens\":6,\"total_tokens\":10}}}\n\n"
    );
    let mut decoder = ResponseDecoder::new();
    let mut events = decoder.push(wire.as_bytes()).unwrap();
    events.extend(decoder.finish().unwrap());

    let ids = events
        .iter()
        .filter_map(|event| match &event.kind {
            Kind::ToolCallDelta { id, .. } => Some(id.clone()),
            _ => None,
        })
        .collect::<Vec<_>>();
    assert_eq!(ids, vec![Some("call_xyz".to_owned()), None]);

    let object = encode_response_object(&events, "team-route", "fallback", 0).unwrap();
    let wire = serde_json::to_value(&object).unwrap();
    assert_eq!(wire["output"][0]["call_id"], "call_xyz");
}
#[test]
fn a_truncated_unary_response_reports_length_not_stop() {
    for (reason, expected) in [
        ("max_output_tokens", FinishReason::Length),
        ("content_filter", FinishReason::ContentFilter),
    ] {
        let response: Object = serde_json::from_value(json!({
            "id": "resp_truncated",
            "object": "response",
            "created_at": 1_800_000_000_i64,
            "status": "incomplete",
            "model": "gpt-upstream",
            "output": [{
                "id": "msg_1", "type": "message", "role": "assistant", "status": "incomplete",
                "content": [{"type": "output_text", "text": "partial", "annotations": []}]
            }],
            "incomplete_details": {"reason": reason}
        }))
        .unwrap();
        let events = decode_response_object(response).unwrap();
        let finish = events
            .iter()
            .find_map(|event| match &event.kind {
                Kind::Finish { reason, .. } => Some(reason.clone()),
                _ => None,
            })
            .expect("a finish reason must be decoded");
        assert_eq!(finish, expected, "incomplete_details.reason = {reason}");
    }
}
#[test]
fn parallel_tool_calls_stay_in_one_turn_with_distinct_item_ids() {
    let response: Object = serde_json::from_value(json!({
        "id": "resp_parallel",
        "object": "response",
        "created_at": 1_800_000_000_i64,
        "status": "completed",
        "model": "gpt-upstream",
        "output": [
            {"id": "fc_1", "type": "function_call", "call_id": "call_1",
             "name": "weather", "arguments": "{\"city\":\"Paris\"}", "status": "completed"},
            {"id": "fc_2", "type": "function_call", "call_id": "call_2",
             "name": "lookup", "arguments": "{\"q\":\"rust\"}", "status": "completed"}
        ]
    }))
    .unwrap();
    let events = decode_response_object(response).unwrap();
    validate_event_sequence(&events).unwrap();

    let outputs = events
        .iter()
        .filter_map(|event| match &event.kind {
            Kind::ToolCallDelta {
                output_index,
                tool_index,
                ..
            } => Some((*output_index, *tool_index)),
            _ => None,
        })
        .collect::<Vec<_>>();
    assert_eq!(outputs, vec![(0, 0), (0, 1)]);
    assert_eq!(
        events
            .iter()
            .filter(|event| matches!(event.kind, Kind::MessageStart { .. }))
            .count(),
        1
    );

    let wire =
        serde_json::to_value(encode_response_object(&events, "team-route", "fallback", 0).unwrap())
            .unwrap();
    assert_eq!(wire["output"][0]["id"], "fc_1");
    assert_eq!(wire["output"][0]["call_id"], "call_1");
    assert_eq!(wire["output"][1]["id"], "fc_2");
    assert_eq!(wire["output"][1]["call_id"], "call_2");
}
#[test]
fn the_responses_stream_encoder_emits_a_complete_numbered_lifecycle() {
    let wire = concat!(
        "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"model\":\"gpt-upstream\"}}\n\n",
        "data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"function_call\",\"id\":\"fc_abc\",\"call_id\":\"call_xyz\",\"name\":\"lookup\",\"arguments\":\"\"}}\n\n",
        "data: {\"type\":\"response.function_call_arguments.delta\",\"output_index\":0,\"item_id\":\"fc_abc\",\"delta\":\"{}\"}\n\n",
        "data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"function_call\"}}\n\n",
        "data: {\"type\":\"response.completed\",\"response\":{}}\n\n"
    );
    let frames = responses_stream_frames(wire, "team-route");
    let kinds = frames
        .iter()
        .map(|frame| frame["type"].as_str().unwrap().to_owned())
        .collect::<Vec<_>>();
    assert_eq!(
        kinds,
        vec![
            "response.created",
            "response.in_progress",
            "response.output_item.added",
            "response.function_call_arguments.delta",
            "response.function_call_arguments.done",
            "response.output_item.done",
            "response.completed",
        ]
    );
    let sequence_numbers = frames
        .iter()
        .map(|frame| frame["sequence_number"].as_u64().unwrap())
        .collect::<Vec<_>>();
    assert_eq!(
        sequence_numbers,
        (0..frames.len() as u64).collect::<Vec<_>>()
    );

    let added = &frames[2];
    assert_eq!(added["item"]["name"], "lookup");
    assert_eq!(added["item"]["call_id"], "call_xyz");
    assert_ne!(added["item"]["name"], "function");
    assert_eq!(frames[3]["item_id"], added["item"]["id"]);
    assert_eq!(frames[5]["item"]["arguments"], "{}");
}
#[test]
fn a_streamed_text_turn_carries_item_ids_and_content_part_events() {
    let wire = concat!(
        "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"model\":\"gpt-upstream\"}}\n\n",
        "data: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"delta\":\"hello\"}\n\n",
        "data: {\"type\":\"response.completed\",\"response\":{}}\n\n"
    );
    let frames = responses_stream_frames(wire, "team-route");
    let kinds = frames
        .iter()
        .map(|frame| frame["type"].as_str().unwrap().to_owned())
        .collect::<Vec<_>>();
    assert_eq!(
        kinds,
        vec![
            "response.created",
            "response.in_progress",
            "response.output_item.added",
            "response.content_part.added",
            "response.output_text.delta",
            "response.output_text.done",
            "response.content_part.done",
            "response.output_item.done",
            "response.completed",
        ]
    );
    let item_id = frames[2]["item"]["id"].as_str().unwrap().to_owned();
    assert_eq!(frames[4]["item_id"], item_id);
    assert_eq!(frames[5]["text"], "hello");
    assert_eq!(frames[7]["item"]["content"][0]["text"], "hello");
    assert_eq!(frames[7]["item"]["status"], "completed");
}
#[test]
fn a_non_responses_upstream_still_reports_created_at_and_truncation() {
    use olp_engine::domain::canonical::events::{Event, FinishReason};

    let events = vec![
        Event::new(
            0,
            Kind::ResponseStart {
                response_id: Some("gen-1".into()),
                provider_model: Some("upstream".into()),
            },
        ),
        Event::new(
            1,
            Kind::MessageStart {
                output_index: 0,
                role: MessageRole::Assistant,
            },
        ),
        Event::new(
            2,
            Kind::TextDelta {
                output_index: 0,
                text: "partial".into(),
            },
        ),
        Event::new(
            3,
            Kind::Finish {
                output_index: 0,
                reason: FinishReason::Length,
            },
        ),
        Event::new(4, Kind::Done),
    ];

    let object = encode_response_object(&events, "team-route", "fallback", 1_800_000_000).unwrap();
    assert_eq!(object.created_at, 1_800_000_000);
    assert_eq!(object.status, "incomplete");
    assert_eq!(
        object
            .incomplete_details
            .as_ref()
            .and_then(|details| details["reason"].as_str()),
        Some("max_output_tokens")
    );

    let mut encoder = ResponseEncoder::new("team-route", "fallback", 1_800_000_000);
    let frames = events
        .into_iter()
        .flat_map(|event| encoder.push(event).unwrap())
        .map(|frame| serde_json::from_str::<serde_json::Value>(&frame.data).unwrap())
        .collect::<Vec<_>>();
    let created = frames.first().unwrap();
    let terminal = frames.last().unwrap();
    assert_eq!(terminal["type"], "response.incomplete");
    assert_eq!(
        terminal["response"]["created_at"],
        created["response"]["created_at"]
    );
    assert_eq!(terminal["response"]["created_at"], 1_800_000_000);
}
#[test]
fn assistant_history_is_re_encoded_as_output_text() {
    let wire: Create = serde_json::from_value(json!({
        "model": "team-responses",
        "input": [
            {"type": "message", "role": "user",
             "content": [{"type": "input_text", "text": "hi"}]},
            {"type": "message", "role": "assistant",
             "content": [{"type": "output_text", "text": "hello back"}]},
            {"type": "message", "role": "user",
             "content": [{"type": "input_text", "text": "and again"}]}
        ]
    }))
    .unwrap();
    let Operation::Generation(canonical) = decode_response_create(wire).unwrap() else {
        panic!("wrong operation");
    };
    let encoded =
        serde_json::to_value(encode_response_create(&canonical, "gpt-upstream").unwrap()).unwrap();
    assert_eq!(encoded["input"][0]["content"][0]["type"], "input_text");
    assert_eq!(encoded["input"][1]["role"], "assistant");
    assert_eq!(encoded["input"][1]["content"][0]["type"], "output_text");
    assert_eq!(encoded["input"][1]["content"][0]["text"], "hello back");
    assert_eq!(encoded["input"][2]["content"][0]["type"], "input_text");
}
#[test]
fn an_invalid_encoding_format_is_rejected_before_dispatch() {
    let request: EmbeddingRequest = serde_json::from_value(json!({
        "model": "team-embed",
        "input": "hello",
        "encoding_format": "float16"
    }))
    .unwrap();
    let error = decode_embedding_request(request).unwrap_err();
    assert!(
        error.to_string().contains("float16"),
        "unexpected error: {error}"
    );

    for format in ["float", "base64"] {
        let request: EmbeddingRequest = serde_json::from_value(json!({
            "model": "team-embed",
            "input": "hello",
            "encoding_format": format
        }))
        .unwrap();
        assert!(decode_embedding_request(request).is_ok());
    }
}
