use super::*;

const UTF8_BOM: &[u8] = b"\xef\xbb\xbf";

fn response_with_content_type(content_type: Option<&str>, body: &[u8]) -> Vec<u8> {
    let content_type = content_type
        .map(|value| format!("Content-Type: {value}\r\n"))
        .unwrap_or_default();
    let mut response = format!(
        "HTTP/1.1 200 OK\r\n{content_type}Content-Length: {}\r\nConnection: close\r\n\r\n",
        body.len()
    )
    .into_bytes();
    response.extend_from_slice(body);
    response
}

fn one_byte_chunked_response(content_type: &str, body: &[u8]) -> Vec<u8> {
    let mut response = format!(
        "HTTP/1.1 200 OK\r\nContent-Type: {content_type}\r\nTransfer-Encoding: chunked\r\nConnection: close\r\n\r\n"
    )
    .into_bytes();
    for byte in body {
        response.extend_from_slice(b"1\r\n");
        response.push(*byte);
        response.extend_from_slice(b"\r\n");
    }
    response.extend_from_slice(b"0\r\n\r\n");
    response
}

fn with_bom(body: &[u8]) -> Vec<u8> {
    let mut wire = UTF8_BOM.to_vec();
    wire.extend_from_slice(body);
    wire
}

fn full_chat_body(text: &str) -> Vec<u8> {
    serde_json::to_vec(&json!({
        "id": "chatcmpl-conformance",
        "object": "chat.completion",
        "created": 1,
        "model": MODEL,
        "choices": [{
            "index": 0,
            "message": {"role": "assistant", "content": text},
            "finish_reason": "stop"
        }],
        "usage": {"prompt_tokens": 2, "completion_tokens": 1, "total_tokens": 3}
    }))
    .unwrap()
}

fn compatible_chat_body(text: &str) -> Vec<u8> {
    serde_json::to_vec(&json!({
        "choices": [{
            "message": {"role": "assistant", "content": text},
            "finish_reason": "stop"
        }],
        "usage": {"prompt_tokens": 2, "completion_tokens": 1, "total_tokens": 3}
    }))
    .unwrap()
}

async fn execute_events(
    kind: ProviderKind,
    mode: TransportMode,
    response: Vec<u8>,
) -> Vec<CanonicalEvent> {
    let (transport, server) = transport_at(kind, response).await;
    let events = collect_events(
        transport
            .execute(generation_request(kind, Surface::OpenAi, mode))
            .await
            .unwrap(),
    )
    .await
    .unwrap();
    server.await.unwrap();
    events
}

async fn execute_protocol_error(
    kind: ProviderKind,
    mode: TransportMode,
    response: Vec<u8>,
) -> TransportError {
    let (transport, server) = transport_at(kind, response).await;
    let result = transport
        .execute(generation_request(kind, Surface::OpenAi, mode))
        .await;
    let error = match result {
        Err(error) => error,
        Ok(output) => collect_events(output)
            .await
            .expect_err("deviant response must fail closed"),
    };
    server.await.unwrap();
    assert_eq!(error.class, AttemptFailureClass::Protocol, "{error:?}");
    error
}

fn assert_chat_events(events: &[CanonicalEvent], expected_text: &str) {
    validate_event_sequence(events).unwrap();
    assert!(events.iter().any(|event| matches!(
        &event.kind,
        CanonicalEventKind::TextDelta { text, .. } if text == expected_text
    )));
    assert!(events.iter().any(|event| matches!(
        event.kind,
        CanonicalEventKind::Usage {
            usage: Usage {
                input_tokens: 2,
                output_tokens: 1,
                total_tokens: 3,
                ..
            }
        }
    )));
    assert!(matches!(
        events.last().map(|event| &event.kind),
        Some(CanonicalEventKind::Done)
    ));
}

#[tokio::test]
async fn compatible_unary_chat_accepts_bom_omitted_metadata_and_safe_content_types() {
    let body = with_bom(&compatible_chat_body("compatible unary"));
    for content_type in [
        Some("application/problem+json; charset=utf-8"),
        None,
        Some("application/octet-stream"),
        Some("text/plain; charset=utf-8"),
    ] {
        let events = execute_events(
            ProviderKind::OpenAiCompatible,
            TransportMode::Unary,
            response_with_content_type(content_type, &body),
        )
        .await;
        assert_chat_events(&events, "compatible unary");
    }
}

#[tokio::test]
async fn compatible_streaming_chat_accepts_a_bounded_unary_json_response() {
    let events = execute_events(
        ProviderKind::OpenAiCompatible,
        TransportMode::Streaming,
        response_with_content_type(
            Some("application/json; charset=utf-8"),
            &compatible_chat_body("unary fallback"),
        ),
    )
    .await;
    assert_chat_events(&events, "unary fallback");
}

#[tokio::test]
async fn compatible_sse_accepts_fragmented_bom_omitted_metadata_and_usage_only_chunk() {
    let body = with_bom(
        concat!(
            "data: {\"choices\":[{\"index\":7,\"delta\":{\"role\":\"assistant\",\"content\":\"fragmented\"},\"finish_reason\":null}]}\n\n",
            "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n",
            "data: {\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":1,\"total_tokens\":3}}\n\n",
            "data: [DONE]\n\n"
        )
        .as_bytes(),
    );
    let events = execute_events(
        ProviderKind::OpenAiCompatible,
        TransportMode::Streaming,
        one_byte_chunked_response("text/event-stream; charset=utf-8", &body),
    )
    .await;
    assert_chat_events(&events, "fragmented");
    assert!(events.iter().any(|event| matches!(
        event.kind,
        CanonicalEventKind::Finish {
            output_index: 7,
            ..
        }
    )));
}

#[tokio::test]
async fn native_openai_and_azure_reject_compatible_only_unary_deviations() {
    let full = full_chat_body("strict");
    let omitted_metadata = compatible_chat_body("strict");
    let cases = [
        (
            "leading BOM",
            response_with_content_type(Some("application/json"), &with_bom(&full)),
        ),
        (
            "omitted envelope metadata and choice index",
            response_with_content_type(Some("application/json"), &omitted_metadata),
        ),
        (
            "structured JSON content type",
            response_with_content_type(Some("application/problem+json"), &full),
        ),
        (
            "missing content type",
            response_with_content_type(None, &full),
        ),
        (
            "generic content type",
            response_with_content_type(Some("application/octet-stream"), &full),
        ),
    ];

    for kind in [ProviderKind::OpenAi, ProviderKind::AzureOpenAi] {
        for (case, response) in &cases {
            let error = execute_protocol_error(kind, TransportMode::Unary, response.clone()).await;
            assert!(!error.response_committed, "{kind:?} {case}: {error:?}");
        }
    }
}

#[tokio::test]
async fn native_openai_and_azure_reject_unary_fallback_for_streaming_chat() {
    for kind in [ProviderKind::OpenAi, ProviderKind::AzureOpenAi] {
        let error = execute_protocol_error(
            kind,
            TransportMode::Streaming,
            response_with_content_type(Some("application/json"), &full_chat_body("unary")),
        )
        .await;
        assert!(!error.response_committed, "{kind:?}: {error:?}");
    }
}

#[tokio::test]
async fn native_openai_and_azure_reject_compatible_only_sse_deviations() {
    let omitted_metadata = concat!(
        "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hello\"},\"finish_reason\":null}]}\n\n",
        "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n",
        "data: [DONE]\n\n"
    );
    let omitted_choice_index = concat!(
        "data: {\"id\":\"chatcmpl-strict\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"conformance-model\",\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"hello\"},\"finish_reason\":null}]}\n\n",
        "data: {\"id\":\"chatcmpl-strict\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"conformance-model\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n",
        "data: [DONE]\n\n"
    );
    let usage_only_terminal = concat!(
        "data: {\"id\":\"chatcmpl-strict\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"conformance-model\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hello\"},\"finish_reason\":null}]}\n\n",
        "data: {\"id\":\"chatcmpl-strict\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"conformance-model\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n",
        "data: {\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":1,\"total_tokens\":3}}\n\n",
        "data: [DONE]\n\n"
    );
    let valid = concat!(
        "data: {\"id\":\"chatcmpl-strict\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"conformance-model\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hello\"},\"finish_reason\":null}]}\n\n",
        "data: {\"id\":\"chatcmpl-strict\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"conformance-model\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n",
        "data: [DONE]\n\n"
    );
    let cases = [
        (
            "usage-only chunk with omitted envelope metadata",
            response_with_content_type(Some("text/event-stream"), usage_only_terminal.as_bytes()),
        ),
        (
            "omitted envelope metadata",
            response_with_content_type(Some("text/event-stream"), omitted_metadata.as_bytes()),
        ),
        (
            "omitted choice index",
            response_with_content_type(Some("text/event-stream"), omitted_choice_index.as_bytes()),
        ),
        (
            "leading BOM",
            one_byte_chunked_response("text/event-stream", &with_bom(valid.as_bytes())),
        ),
    ];

    for kind in [ProviderKind::OpenAi, ProviderKind::AzureOpenAi] {
        for (case, response) in &cases {
            let error =
                execute_protocol_error(kind, TransportMode::Streaming, response.clone()).await;
            assert_eq!(error.phase, TransportPhase::Body, "{kind:?} {case}");
        }
    }
}

#[tokio::test]
async fn compatible_content_type_fallback_still_requires_unambiguous_valid_json() {
    for mode in [TransportMode::Unary, TransportMode::Streaming] {
        for content_type in [None, Some("application/octet-stream")] {
            let error = execute_protocol_error(
                ProviderKind::OpenAiCompatible,
                mode,
                response_with_content_type(content_type, b"{"),
            )
            .await;
            assert!(
                !error.response_committed,
                "{mode:?} {content_type:?}: {error:?}"
            );
        }
    }

    for content_type in ["application/xml", "not a type/problem+json"] {
        execute_protocol_error(
            ProviderKind::OpenAiCompatible,
            TransportMode::Unary,
            response_with_content_type(Some(content_type), &full_chat_body("wrong type")),
        )
        .await;
    }

    let twice_bom = with_bom(&with_bom(&compatible_chat_body("double BOM")));
    execute_protocol_error(
        ProviderKind::OpenAiCompatible,
        TransportMode::Unary,
        response_with_content_type(Some("application/json"), &twice_bom),
    )
    .await;
}

#[tokio::test]
async fn compatible_streaming_chat_rejects_ambiguous_missing_choice_and_tool_indices() {
    let ambiguous_choices = concat!(
        "data: {\"choices\":[",
        "{\"delta\":{\"role\":\"assistant\",\"content\":\"one\"},\"finish_reason\":null},",
        "{\"delta\":{\"role\":\"assistant\",\"content\":\"two\"},\"finish_reason\":null}",
        "]}\n\n",
        "data: {\"choices\":[",
        "{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"},",
        "{\"index\":1,\"delta\":{},\"finish_reason\":\"stop\"}",
        "]}\n\n",
        "data: [DONE]\n\n"
    );
    let ambiguous_tools = concat!(
        "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\",\"tool_calls\":[",
        "{\"id\":\"call-a\",\"type\":\"function\",\"function\":{\"name\":\"a\",\"arguments\":\"{}\"}},",
        "{\"id\":\"call-b\",\"type\":\"function\",\"function\":{\"name\":\"b\",\"arguments\":\"{}\"}}",
        "]},\"finish_reason\":null}]}\n\n",
        "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n",
        "data: [DONE]\n\n"
    );

    for body in [ambiguous_choices, ambiguous_tools] {
        let error = execute_protocol_error(
            ProviderKind::OpenAiCompatible,
            TransportMode::Streaming,
            response_with_content_type(Some("text/event-stream"), body.as_bytes()),
        )
        .await;
        assert_eq!(error.phase, TransportPhase::Body, "{error:?}");
    }
}
