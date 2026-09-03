use olp_engine::domain::canonical::{
    events::{FinishReason, Kind, validate_event_sequence},
    identity::Surface,
    requests::Operation,
};
use olp_engine::protocols::openai::{
    client::{Encoder as ResponseEncoder, encode_response_object},
    responses::{
        request::{Create, decode_response_create, encode_response_create},
        response::{Object, decode_response_object},
        stream::Decoder as ResponseDecoder,
    },
};
use serde_json::json;

#[test]
fn responses_request_round_trips_supported_semantics_and_extensions() {
    let wire: Create = serde_json::from_value(json!({
        "model": "team-responses",
        "instructions": "Be concise",
        "input": [{
            "type": "message",
            "role": "user",
            "content": [
                {"type": "input_text", "text": "describe this", "cache_hint": "short"},
                {"type": "input_image", "image_url": "https://example.test/a.png", "detail": "low"}
            ],
            "vendor_message": true
        }],
        "max_output_tokens": 80,
        "parallel_tool_calls": false,
        "tools": [{
            "type": "function",
            "name": "lookup",
            "description": "lookup",
            "parameters": {"type": "object"},
            "strict": true,
            "vendor_tool": 3
        }],
        "tool_choice": {"type": "function", "name": "lookup"},
        "text": {
            "format": {"type": "json_schema", "name": "answer", "schema": {"type": "object"}, "strict": true},
            "verbosity": "low"
        },
        "service_tier": "priority"
    }))
    .unwrap();
    let Operation::Generation(canonical) = decode_response_create(wire).unwrap() else {
        panic!("wrong operation")
    };
    assert_eq!(canonical.route.as_str(), "team-responses");
    assert_eq!(canonical.messages.len(), 2);
    assert_eq!(canonical.tools[0].name, "lookup");
    assert_eq!(canonical.extensions.values["/service_tier"], "priority");
    assert_eq!(canonical.extensions.values["/input/0/vendor_message"], true);
    assert_eq!(
        canonical.extensions.values["/input/0/content/0/cache_hint"],
        "short"
    );

    let encoded = encode_response_create(&canonical, "gpt-upstream").unwrap();
    let encoded = serde_json::to_value(encoded).unwrap();
    assert_eq!(encoded["model"], "gpt-upstream");
    assert_eq!(encoded["instructions"], "Be concise");
    assert_eq!(encoded["input"][0]["vendor_message"], true);
    assert_eq!(encoded["input"][0]["content"][0]["cache_hint"], "short");
    assert_eq!(encoded["tools"][0]["strict"], true);
    assert_eq!(encoded["service_tier"], "priority");
}
#[test]
fn responses_rejects_stateful_and_unspooled_media_semantics() {
    let stateful: Create = serde_json::from_value(json!({
        "model": "default",
        "input": "hello",
        "previous_response_id": "resp_previous"
    }))
    .unwrap();
    assert!(decode_response_create(stateful).is_err());

    let conversation: Create = serde_json::from_value(json!({
        "model": "default",
        "input": "hello",
        "conversation": {"id": "conv_stateful"}
    }))
    .unwrap();
    assert!(decode_response_create(conversation).is_err());

    let inline_file: Create = serde_json::from_value(json!({
        "model": "default",
        "input": [{"type": "message", "role": "user", "content": [{
            "type": "input_file", "file_data": "large-inline-payload"
        }]}]
    }))
    .unwrap();
    assert!(decode_response_create(inline_file).is_err());
}
#[test]
fn responses_preserves_builtin_tools_only_for_same_protocol() {
    let wire: Create = serde_json::from_value(json!({
        "model": "team-responses",
        "input": "search",
        "tools": [{
            "type": "web_search_preview",
            "search_context_size": "low",
            "user_location": {"type": "approximate", "country": "FR"}
        }],
        "tool_choice": {"type": "web_search_preview"}
    }))
    .unwrap();
    let Operation::Generation(canonical) = decode_response_create(wire).unwrap() else {
        panic!("wrong operation")
    };
    assert!(canonical.tools.is_empty());
    assert_eq!(canonical.extensions.source, Some(Surface::OpenAi));
    let encoded =
        serde_json::to_value(encode_response_create(&canonical, "gpt-upstream").unwrap()).unwrap();
    assert_eq!(encoded["tools"][0]["type"], "web_search_preview");
    assert_eq!(encoded["tools"][0]["user_location"]["country"], "FR");
    assert_eq!(encoded["tool_choice"]["type"], "web_search_preview");
    assert!(
        canonical
            .extensions
            .ensure_representable_on(Surface::Gemini)
            .is_err()
    );
}
#[test]
fn responses_unary_and_fragmented_stream_become_ordered_events() {
    let response: Object = serde_json::from_value(json!({
        "id": "resp_1",
        "object": "response",
        "created_at": 1800000000,
        "status": "completed",
        "model": "gpt-upstream",
        "output": [{
            "id": "msg_1",
            "type": "message",
            "role": "assistant",
            "status": "completed",
            "content": [{"type": "output_text", "text": "hello", "annotations": []}]
        }],
        "usage": {"input_tokens": 3, "output_tokens": 2, "total_tokens": 5}
    }))
    .unwrap();
    let events = decode_response_object(response).unwrap();
    validate_event_sequence(&events).unwrap();
    assert!(events.iter().any(|event| matches!(
        &event.kind,
        Kind::TextDelta { text, .. } if text == "hello"
    )));
    let client = encode_response_object(&events, "team-route", "fallback", 1_800_000_000).unwrap();
    assert_eq!(client.model, "team-route");
    validate_event_sequence(&decode_response_object(client).unwrap()).unwrap();

    let mut encoder = ResponseEncoder::new("team-route", "fallback", 1_800_000_000);
    let mut client_frames = Vec::new();
    for event in events.clone() {
        client_frames.extend(encoder.push(event).unwrap());
    }
    assert_eq!(
        client_frames.last().unwrap().event.as_deref(),
        Some("response.completed")
    );

    let frames = [
        json!({"type":"response.created","response":{"id":"resp_s","model":"gpt-upstream"}}),
        json!({"type":"response.output_text.delta","output_index":0,"delta":"hé 🌍"}),
        json!({"type":"response.completed","response":{"usage":{"input_tokens":2,"output_tokens":2,"total_tokens":4}}}),
    ];
    let wire = frames
        .iter()
        .map(|frame| {
            format!(
                "event: {}\ndata: {frame}\n\n",
                frame["type"].as_str().unwrap()
            )
        })
        .collect::<String>();
    let mut decoder = ResponseDecoder::new();
    let mut streamed = Vec::new();
    for byte in wire.as_bytes() {
        streamed.extend(decoder.push(std::slice::from_ref(byte)).unwrap());
    }
    streamed.extend(decoder.finish().unwrap());
    validate_event_sequence(&streamed).unwrap();
    assert!(streamed.iter().any(|event| matches!(
        &event.kind,
        Kind::TextDelta { text, .. } if text == "hé 🌍"
    )));
}
#[test]
fn a_provider_error_finish_is_a_failed_response_not_a_completed_one() {
    let response: Object = serde_json::from_value(json!({
        "id": "resp_err",
        "object": "response",
        "created_at": 1800000000,
        "status": "completed",
        "model": "gpt-upstream",
        "output": [{
            "id": "msg_1",
            "type": "message",
            "role": "assistant",
            "status": "completed",
            "content": [{"type": "output_text", "text": "partial", "annotations": []}]
        }]
    }))
    .unwrap();
    let events = decode_response_object(response)
        .unwrap()
        .into_iter()
        .map(|mut event| {
            if let Kind::Finish { reason, .. } = &mut event.kind {
                *reason = FinishReason::Error;
            }
            event
        })
        .collect::<Vec<_>>();

    let unary = encode_response_object(&events, "team-route", "fallback", 1_800_000_000).unwrap();
    assert_eq!(unary.status, "failed");
    assert_eq!(
        unary.error.as_ref().map(|error| error.code.as_str()),
        Some("server_error")
    );

    let mut encoder = ResponseEncoder::new("team-route", "fallback", 1_800_000_000);
    let mut frames = Vec::new();
    for event in events {
        frames.extend(encoder.push(event).unwrap());
    }
    let terminal = frames.last().unwrap();
    assert_eq!(terminal.event.as_deref(), Some("response.failed"));
    let payload: serde_json::Value = serde_json::from_str(&terminal.data).unwrap();
    assert_eq!(payload["response"]["status"], "failed");
    assert_eq!(payload["response"]["error"]["code"], "server_error");
}
#[test]
fn unary_incomplete_response_remains_incomplete_when_streamed_to_the_client() {
    let response: Object = serde_json::from_value(json!({
        "id": "resp_incomplete",
        "object": "response",
        "created_at": 1800000000,
        "status": "incomplete",
        "model": "gpt-upstream",
        "output": [{
            "id": "msg_1",
            "type": "message",
            "role": "assistant",
            "status": "incomplete",
            "content": [{"type": "output_text", "text": "partial", "annotations": []}]
        }],
        "incomplete_details": {"reason": "max_output_tokens"}
    }))
    .unwrap();
    let events = decode_response_object(response).unwrap();
    let mut encoder = ResponseEncoder::new("team-route", "fallback", 1_800_000_000);
    let mut frames = Vec::new();
    for event in events {
        frames.extend(encoder.push(event).unwrap());
    }

    let terminal = frames.last().unwrap();
    assert_eq!(terminal.event.as_deref(), Some("response.incomplete"));
    let payload: serde_json::Value = serde_json::from_str(&terminal.data).unwrap();
    assert_eq!(payload["response"]["status"], "incomplete");
    assert_eq!(
        payload["response"]["incomplete_details"]["reason"],
        "max_output_tokens"
    );
}
#[test]
fn responses_reasoning_output_round_trips_without_becoming_message_content() {
    let response: Object = serde_json::from_value(json!({
        "id": "resp_reasoning",
        "object": "response",
        "created_at": 1800000000,
        "status": "completed",
        "model": "gpt-upstream",
        "output": [
            {
                "id": "rs_1", "type": "reasoning", "status": "completed",
                "summary": [{"type": "summary_text", "text": "checked constraints"}],
                "encrypted_content": "opaque"
            },
            {
                "id": "msg_1", "type": "message", "role": "assistant", "status": "completed",
                "content": [{"type": "output_text", "text": "answer", "annotations": []}]
            }
        ]
    }))
    .unwrap();
    let events = decode_response_object(response).unwrap();
    let encoded = serde_json::to_value(
        encode_response_object(&events, "team-route", "fallback", 1_800_000_000).unwrap(),
    )
    .unwrap();
    assert_eq!(encoded["output"][0]["type"], "reasoning");
    assert_eq!(encoded["output"][0]["encrypted_content"], "opaque");
    assert_eq!(encoded["output"][1]["content"][0]["text"], "answer");
}
