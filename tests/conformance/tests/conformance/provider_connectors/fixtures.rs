use std::sync::Arc;

use olp_engine::domain::{
    canonical::{
        identity::TransportMode,
        requests::{Operation, ToolChoice, ToolDefinition},
    },
    ports::ProviderRequest,
    routing::provider::ProviderKind,
};
use olp_engine::protocols::sse::DEFAULT_MAX_EVENT_BYTES;
use serde_json::json;

use super::support::*;

pub(super) fn unary_response(kind: ProviderKind) -> Vec<u8> {
    let body = match kind {
        ProviderKind::OpenAi | ProviderKind::OpenAiCompatible | ProviderKind::AzureOpenAi => {
            json!({
                "id": "provider-response-id",
                "object": "chat.completion",
                "created": 1,
                "model": MODEL,
                "choices": [{
                    "index": 0,
                    "message": {"role": "assistant", "content": "hello back"},
                    "finish_reason": "stop"
                }],
                "usage": {
                    "prompt_tokens": 5,
                    "completion_tokens": 2,
                    "total_tokens": 7,
                    "prompt_tokens_details": {"cached_tokens": 3}
                }
            })
        }
        ProviderKind::Anthropic => json!({
            "id": "provider-response-id",
            "type": "message",
            "role": "assistant",
            "content": [{"type": "text", "text": "hello back"}],
            "model": MODEL,
            "stop_reason": "end_turn",
            "stop_sequence": null,
            "usage": {
                "input_tokens": 2,
                "output_tokens": 2,
                "cache_read_input_tokens": 3
            }
        }),
        ProviderKind::Gemini | ProviderKind::VertexAi => json!({
            "candidates": [{
                "content": {"role": "model", "parts": [{"text": "hello back"}]},
                "finishReason": "STOP",
                "index": 0
            }],
            "usageMetadata": {
                "promptTokenCount": 5,
                "candidatesTokenCount": 2,
                "totalTokenCount": 7,
                "cachedContentTokenCount": 3
            },
            "modelVersion": MODEL,
            "responseId": "provider-response-id"
        }),
        ProviderKind::Bedrock => json!({
            "output": {"message": {"role": "assistant", "content": [{"text": "hello back"}]}},
            "stopReason": "end_turn",
            "usage": {"inputTokens": 5, "outputTokens": 2, "totalTokens": 7},
            "metrics": {"latencyMs": 1}
        }),
    };
    http_response(
        "200 OK",
        "application/json",
        serde_json::to_vec(&body).unwrap(),
    )
}

pub(super) fn streaming_response(kind: ProviderKind) -> Vec<u8> {
    match kind {
        ProviderKind::OpenAi | ProviderKind::OpenAiCompatible | ProviderKind::AzureOpenAi => {
            let body = concat!(
                "data: {\"id\":\"provider-response-id\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"conformance-model\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hello back\"},\"finish_reason\":null}]}\n\n",
                "data: {\"id\":\"provider-response-id\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"conformance-model\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":2,\"total_tokens\":7,\"prompt_tokens_details\":{\"cached_tokens\":3}}}\n\n",
                "data: [DONE]\n\n"
            );
            http_response("200 OK", "text/event-stream", body)
        }
        ProviderKind::Anthropic => {
            let body = concat!(
                "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"provider-response-id\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"conformance-model\",\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":2,\"output_tokens\":0,\"cache_read_input_tokens\":3}}}\n\n",
                "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
                "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello back\"}}\n\n",
                "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
                "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":2}}\n\n",
                "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
            );
            http_response("200 OK", "text/event-stream", body)
        }
        ProviderKind::Gemini | ProviderKind::VertexAi => {
            let body = concat!(
                "data: {\"responseId\":\"provider-response-id\",\"modelVersion\":\"conformance-model\",\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"hello back\"}]},\"index\":0}]}\n\n",
                "data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[]},\"finishReason\":\"STOP\",\"index\":0}],\"usageMetadata\":{\"promptTokenCount\":5,\"candidatesTokenCount\":2,\"totalTokenCount\":7,\"cachedContentTokenCount\":3}}\n\n"
            );
            http_response("200 OK", "text/event-stream", body)
        }
        ProviderKind::Bedrock => {
            let mut body = Vec::new();
            for (event, payload) in [
                ("messageStart", r#"{"role":"assistant"}"#),
                (
                    "contentBlockDelta",
                    r#"{"delta":{"text":"hello back"},"contentBlockIndex":0}"#,
                ),
                ("contentBlockStop", r#"{"contentBlockIndex":0}"#),
                ("messageStop", r#"{"stopReason":"end_turn"}"#),
                (
                    "metadata",
                    r#"{"usage":{"inputTokens":5,"outputTokens":2,"totalTokens":7},"metrics":{"latencyMs":1}}"#,
                ),
            ] {
                body.extend(event_frame(event, payload));
            }
            http_response("200 OK", "application/vnd.amazon.eventstream", body)
        }
    }
}

pub(super) fn token_count_response(kind: ProviderKind) -> Vec<u8> {
    let body = match kind {
        ProviderKind::OpenAi | ProviderKind::OpenAiCompatible | ProviderKind::AzureOpenAi => {
            r#"{"object":"response.input_tokens","input_tokens":7}"#
        }
        ProviderKind::Anthropic => r#"{"input_tokens":7}"#,
        ProviderKind::Gemini | ProviderKind::VertexAi => {
            r#"{"totalTokens":7,"cachedContentTokenCount":2}"#
        }
        ProviderKind::Bedrock => r#"{"inputTokens":7}"#,
    };
    http_response("200 OK", "application/json", body)
}

pub(super) fn certification_response(kind: ProviderKind, request: &CapturedRequest) -> Vec<u8> {
    let path = request.path();
    if path.ends_with("/models") {
        return http_response(
            "200 OK",
            "application/json",
            json!({
                "object": "list",
                "data": [{
                    "id": MODEL,
                    "object": "model",
                    "created": 1,
                    "owned_by": "openai"
                }]
            })
            .to_string(),
        );
    }
    if path.contains("/chat/completions") {
        return if request.body_text().contains("\"stream\":true") {
            streaming_response(kind)
        } else {
            unary_response(kind)
        };
    }
    if path.contains("/responses") && !path.contains("/responses/input_tokens") {
        if request.body_text().contains("\"stream\":true") {
            return http_response(
                "200 OK",
                "text/event-stream",
                concat!(
                    "event: response.created\n",
                    "data: {\"type\":\"response.created\",\"response\":{\"id\":\"provider-response-id\",\"model\":\"conformance-model\"}}\n\n",
                    "event: response.output_text.delta\n",
                    "data: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"delta\":\"OK\"}\n\n",
                    "event: response.completed\n",
                    "data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":3,\"output_tokens\":1,\"total_tokens\":4}}}\n\n"
                ),
            );
        }
        return http_response(
            "200 OK",
            "application/json",
            json!({
                "id": "provider-response-id",
                "object": "response",
                "created_at": 1,
                "status": "completed",
                "model": MODEL,
                "output": [{
                    "id": "message-id",
                    "type": "message",
                    "role": "assistant",
                    "status": "completed",
                    "content": [{"type": "output_text", "text": "OK", "annotations": []}]
                }],
                "usage": {"input_tokens": 3, "output_tokens": 1, "total_tokens": 4}
            })
            .to_string(),
        );
    }
    if path.contains("/embeddings") {
        return http_response(
            "200 OK",
            "application/json",
            json!({
                "object": "list",
                "model": MODEL,
                "data": [{"object": "embedding", "index": 0, "embedding": [0.25]}],
                "usage": {"prompt_tokens": 1, "total_tokens": 1}
            })
            .to_string(),
        );
    }
    if path.contains("/moderations") {
        return http_response(
            "200 OK",
            "application/json",
            json!({
                "id": "moderation-id",
                "model": MODEL,
                "results": [{
                    "flagged": false,
                    "categories": {"violence": false},
                    "category_scores": {"violence": 0.0}
                }]
            })
            .to_string(),
        );
    }
    if path.contains("input_tokens")
        || path.contains("count_tokens")
        || path.contains("countTokens")
        || path.contains("count-tokens")
    {
        return token_count_response(kind);
    }
    if path.contains("streamGenerateContent")
        || path.contains("converse-stream")
        || (path.contains("/messages") && request.body_text().contains("\"stream\":true"))
    {
        return streaming_response(kind);
    }
    if path.contains("generateContent") || path.contains("/converse") || path.contains("/messages")
    {
        return unary_response(kind);
    }
    panic!("unhandled certification request for {kind:?}: {path}");
}

pub(super) fn tool_response(kind: ProviderKind) -> Vec<u8> {
    let body = match kind {
        ProviderKind::OpenAi | ProviderKind::OpenAiCompatible | ProviderKind::AzureOpenAi => {
            json!({
                "id": "tool-response",
                "object": "chat.completion",
                "created": 1,
                "model": MODEL,
                "choices": [{
                    "index": 0,
                    "message": {
                        "role": "assistant",
                        "content": null,
                        "tool_calls": [{
                            "id": "call-1",
                            "type": "function",
                            "function": {"name": "weather", "arguments": "{\"city\":\"Paris\"}"}
                        }]
                    },
                    "finish_reason": "tool_calls"
                }],
                "usage": {"prompt_tokens": 2, "completion_tokens": 1, "total_tokens": 3}
            })
        }
        ProviderKind::Anthropic => json!({
            "id": "tool-response",
            "type": "message",
            "role": "assistant",
            "content": [{
                "type": "tool_use",
                "id": "call-1",
                "name": "weather",
                "input": {"city": "Paris"}
            }],
            "model": MODEL,
            "stop_reason": "tool_use",
            "stop_sequence": null,
            "usage": {"input_tokens": 2, "output_tokens": 1}
        }),
        ProviderKind::Gemini | ProviderKind::VertexAi => json!({
            "candidates": [{
                "content": {"role": "model", "parts": [{
                    "functionCall": {"name": "weather", "args": {"city": "Paris"}}
                }]},
                "finishReason": "STOP",
                "index": 0
            }],
            "usageMetadata": {"promptTokenCount": 2, "candidatesTokenCount": 1, "totalTokenCount": 3},
            "modelVersion": MODEL,
            "responseId": "tool-response"
        }),
        ProviderKind::Bedrock => json!({
            "output": {"message": {"role": "assistant", "content": [{
                "toolUse": {"toolUseId": "call-1", "name": "weather", "input": {"city": "Paris"}}
            }]}},
            "stopReason": "tool_use",
            "usage": {"inputTokens": 2, "outputTokens": 1, "totalTokens": 3},
            "metrics": {"latencyMs": 1}
        }),
    };
    http_response(
        "200 OK",
        "application/json",
        serde_json::to_vec(&body).unwrap(),
    )
}

pub(super) fn tool_request(kind: ProviderKind) -> ProviderRequest {
    let surface = native_surface(kind);
    let mut request = generation_request(kind, surface, TransportMode::Unary);
    let Operation::Generation(generation) = Arc::make_mut(&mut request.operation) else {
        unreachable!()
    };
    generation.tools = vec![ToolDefinition {
        name: "weather".to_owned(),
        description: Some("Look up weather".to_owned()),
        input_schema: json!({
            "type": "object",
            "properties": {"city": {"type": "string"}},
            "required": ["city"]
        }),
    }];
    generation.tool_choice = Some(ToolChoice::Auto);
    request
}

pub(super) fn classified_error_response(kind: ProviderKind, status: &str, secret: &str) -> Vec<u8> {
    let error_type = match status.as_bytes().first() {
        Some(b'4') if status.starts_with("429") => "ThrottlingException",
        Some(b'4') => "ValidationException",
        _ => "ServiceUnavailableException",
    };
    let body = json!({"__type": error_type, "message": format!("reflected {secret}")});
    let headers = [("x-amzn-errortype", error_type), ("retry-after", "7")];
    if kind == ProviderKind::Bedrock {
        http_response_with_headers(status, "application/json", &headers, body.to_string())
    } else {
        http_response_with_headers(
            status,
            "application/json",
            &[("retry-after", "7")],
            body.to_string(),
        )
    }
}

pub(super) fn stream_handoff_response(kind: ProviderKind) -> Vec<u8> {
    match kind {
        ProviderKind::OpenAi | ProviderKind::OpenAiCompatible | ProviderKind::AzureOpenAi => {
            http_stream_response(
                "text/event-stream",
                format!(
                    "data: {{\"id\":\"handoff\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"{MODEL}\",\"choices\":[{{\"index\":0,\"delta\":{{\"role\":\"assistant\",\"content\":\"partial\"}},\"finish_reason\":null}}]}}\n\n"
                ),
            )
        }
        ProviderKind::Anthropic => http_stream_response(
            "text/event-stream",
            format!(
                "event: message_start\ndata: {{\"type\":\"message_start\",\"message\":{{\"id\":\"handoff\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"{MODEL}\",\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{{\"input_tokens\":1,\"output_tokens\":0}}}}}}\n\n"
            ),
        ),
        ProviderKind::Gemini | ProviderKind::VertexAi => http_stream_response(
            "text/event-stream",
            r#"data: {"candidates":[{"content":{"role":"model","parts":[{"text":"partial"}]},"index":0}]}

"#,
        ),
        ProviderKind::Bedrock => http_stream_response(
            "application/vnd.amazon.eventstream",
            event_frame("messageStart", &json!({"role": "assistant"}).to_string()),
        ),
    }
}

pub(super) fn truncated_stream_response(kind: ProviderKind) -> Vec<u8> {
    match kind {
        ProviderKind::OpenAi | ProviderKind::OpenAiCompatible | ProviderKind::AzureOpenAi => {
            http_response(
                "200 OK",
                "text/event-stream",
                format!(
                    "data: {{\"id\":\"truncated\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"{MODEL}\",\"choices\":[{{\"index\":0,\"delta\":{{\"role\":\"assistant\",\"content\":\"partial\"}},\"finish_reason\":null}}]}}\n\n"
                ),
            )
        }
        ProviderKind::Anthropic => http_response(
            "200 OK",
            "text/event-stream",
            format!(
                "event: message_start\ndata: {{\"type\":\"message_start\",\"message\":{{\"id\":\"truncated\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"{MODEL}\",\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{{\"input_tokens\":1,\"output_tokens\":0}}}}}}\n\n"
            ),
        ),
        ProviderKind::Gemini | ProviderKind::VertexAi => http_response(
            "200 OK",
            "text/event-stream",
            r#"data: {"candidates":[{"content":{"role":"model","parts":[{"text":"partial"}]},"index":0}]}

"#,
        ),
        ProviderKind::Bedrock => http_response(
            "200 OK",
            "application/vnd.amazon.eventstream",
            event_frame("messageStart", &json!({"role": "assistant"}).to_string()),
        ),
    }
}

pub(super) fn invalid_ordered_stream_response(kind: ProviderKind) -> Vec<u8> {
    match kind {
        ProviderKind::OpenAi | ProviderKind::OpenAiCompatible | ProviderKind::AzureOpenAi => {
            let finished = format!(
                "data: {{\"id\":\"invalid-order\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"{MODEL}\",\"choices\":[{{\"index\":0,\"delta\":{{}},\"finish_reason\":\"stop\"}}]}}\n\n"
            );
            let late_data = format!(
                "data: {{\"id\":\"invalid-order\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"{MODEL}\",\"choices\":[{{\"index\":0,\"delta\":{{\"content\":\"late\"}},\"finish_reason\":null}}]}}\n\n"
            );
            http_response(
                "200 OK",
                "text/event-stream",
                [finished, late_data].concat(),
            )
        }
        ProviderKind::Anthropic => http_response(
            "200 OK",
            "text/event-stream",
            format!(
                "event: message_start\ndata: {{\"type\":\"message_start\",\"message\":{{\"id\":\"invalid-order\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"{MODEL}\",\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{{\"input_tokens\":1,\"output_tokens\":0}}}}}}\n\n\
                 event: message_delta\ndata: {{\"type\":\"message_delta\",\"delta\":{{\"stop_reason\":\"end_turn\",\"stop_sequence\":null}},\"usage\":{{\"output_tokens\":1}}}}\n\n\
                 event: content_block_delta\ndata: {{\"type\":\"content_block_delta\",\"index\":0,\"delta\":{{\"type\":\"text_delta\",\"text\":\"late\"}}}}\n\n"
            ),
        ),
        ProviderKind::Gemini | ProviderKind::VertexAi => http_response(
            "200 OK",
            "text/event-stream",
            concat!(
                "data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[]},\"finishReason\":\"STOP\",\"index\":0}]}\n\n",
                "data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"late\"}]},\"index\":0}]}\n\n"
            ),
        ),
        ProviderKind::Bedrock => http_response(
            "200 OK",
            "application/vnd.amazon.eventstream",
            event_frame(
                "contentBlockDelta",
                r#"{"delta":{"text":"late"},"contentBlockIndex":0}"#,
            ),
        ),
    }
}

pub(super) fn invalid_body_response(case: &str) -> Vec<u8> {
    match case {
        "empty" => http_response("200 OK", "application/json", []),
        "malformed" => http_response("200 OK", "application/json", b"{"),
        "truncated" => {
            b"HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 20\r\nConnection: close\r\n\r\n{}".to_vec()
        }
        _ => unreachable!(),
    }
}

pub(super) fn oversized_stream_response() -> Vec<u8> {
    let event = format!("data: {}\n\n", "x".repeat(DEFAULT_MAX_EVENT_BYTES + 1));
    http_response("200 OK", "text/event-stream", event)
}
