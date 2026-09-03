use olp_engine::domain::canonical::{
    identity::Surface,
    requests::{MessageRole, Operation},
};
use olp_engine::protocols::anthropic::{
    count::decode_count_tokens_request,
    dto::{CountTokensRequest, MessagesRequest},
    translate::{decode::request as decode_request, encode::request as encode_request},
};
use serde_json::json;

#[test]
fn request_translation_round_trips_tools_results_and_source_extensions() {
    let wire = json!({
        "model": "team-claude",
        "max_tokens": 512,
        "stream": true,
        "system": [{"type": "text", "text": "Be concise", "cache_control": {"type": "ephemeral"}}],
        "messages": [
            {"role": "user", "content": [{"type": "text", "text": "Weather?", "vendor_text": 7}], "vendor_turn": true},
            {"role": "assistant", "content": [
                {"type": "text", "text": "I'll check."},
                {"type": "tool_use", "id": "toolu_1", "name": "weather", "input": {"city": "Paris"}, "eager_input_streaming": true}
            ]},
            {"role": "user", "content": [
                {"type": "tool_result", "tool_use_id": "toolu_1", "content": "sunny", "is_error": false},
                {"type": "tool_result", "tool_use_id": "toolu_2", "content": [{"type": "text", "text": "extra"}], "is_error": true}
            ]}
        ],
        "tools": [
            {"name": "weather", "description": "Weather lookup", "input_schema": {"type": "object"}, "cache_control": {"type": "ephemeral"}},
            {"type": "web_search_20250305", "name": "web_search", "max_uses": 2}
        ],
        "tool_choice": {"type": "any", "disable_parallel_tool_use": true, "vendor_choice": "kept"},
        "metadata": {"user_id": "opaque-user"}
    });
    let dto: MessagesRequest = serde_json::from_value(wire).unwrap();
    let Operation::Generation(canonical) = decode_request(dto).unwrap() else {
        panic!("wrong operation");
    };

    assert_eq!(canonical.route.as_str(), "team-claude");
    assert_eq!(canonical.messages[0].role, MessageRole::System);
    assert_eq!(canonical.messages.len(), 5);
    assert_eq!(canonical.messages[3].role, MessageRole::Tool);
    assert_eq!(
        canonical.messages[3].tool_call_id.as_deref(),
        Some("toolu_1")
    );
    assert_eq!(
        canonical.messages[4].tool_call_id.as_deref(),
        Some("toolu_2")
    );
    assert_eq!(canonical.tools.len(), 1);
    assert_eq!(canonical.parameters.parallel_tool_calls, Some(false));
    assert_eq!(canonical.extensions.source, Some(Surface::Anthropic));
    assert_eq!(
        canonical.extensions.values["/metadata"]["user_id"],
        "opaque-user"
    );
    assert_eq!(
        canonical.extensions.values["/messages/0/content/0/vendor_text"],
        7
    );
    assert_eq!(
        canonical.extensions.values["/messages/2/content/0/is_error"],
        false
    );
    assert_eq!(
        canonical.extensions.values["/messages/3/content/0/is_error"],
        true
    );

    let encoded = encode_request(&canonical, "claude-upstream").unwrap();
    let encoded = serde_json::to_value(encoded).unwrap();
    assert_eq!(encoded["model"], "claude-upstream");
    assert_eq!(encoded["system"][0]["cache_control"]["type"], "ephemeral");
    assert_eq!(encoded["messages"][0]["content"][0]["vendor_text"], 7);
    assert_eq!(encoded["messages"][2]["content"][0]["is_error"], false);
    assert_eq!(encoded["messages"][3]["content"][0]["is_error"], true);
    assert_eq!(encoded["tools"].as_array().unwrap().len(), 2);
    assert_eq!(encoded["tools"][1]["type"], "web_search_20250305");
    assert_eq!(encoded["tool_choice"]["vendor_choice"], "kept");
    assert_eq!(encoded["metadata"]["user_id"], "opaque-user");
}
#[test]
fn inline_media_and_cross_protocol_loss_are_rejected() {
    let inline: MessagesRequest = serde_json::from_value(json!({
        "model": "default",
        "max_tokens": 32,
        "messages": [{"role": "user", "content": [{
            "type": "image",
            "source": {"type": "base64", "media_type": "image/png", "data": "AAAA"}
        }]}]
    }))
    .unwrap();
    assert!(decode_request(inline).is_err());

    let request: MessagesRequest = serde_json::from_value(json!({
        "model": "default",
        "max_tokens": 32,
        "messages": [{"role": "user", "content": "hello"}],
        "thinking": {"type": "adaptive"}
    }))
    .unwrap();
    let Operation::Generation(mut canonical) = decode_request(request).unwrap() else {
        unreachable!();
    };
    canonical.extensions.source = Some(Surface::Gemini);
    assert!(encode_request(&canonical, "claude-upstream").is_err());
}
#[test]
fn server_side_only_tools_survive_the_request_round_trip() {
    let dto: MessagesRequest = serde_json::from_value(json!({
        "model": "team-claude",
        "max_tokens": 256,
        "messages": [{"role": "user", "content": "search please"}],
        "tools": [{"type": "web_search_20250305", "name": "web_search", "max_uses": 2}]
    }))
    .unwrap();
    let Operation::Generation(canonical) = decode_request(dto).unwrap() else {
        panic!("wrong operation");
    };
    assert!(canonical.tools.is_empty());
    assert_eq!(
        canonical.extensions.values["/tools/0"]["type"],
        "web_search_20250305"
    );

    let encoded = encode_request(&canonical, "claude-upstream").unwrap();
    let wire = serde_json::to_value(&encoded).unwrap();
    assert_eq!(wire["tools"][0]["type"], "web_search_20250305");
    assert_eq!(wire["tools"][0]["name"], "web_search");
    assert_eq!(wire["tools"][0]["max_uses"], 2);
    assert_eq!(wire["tools"].as_array().unwrap().len(), 1);
}
#[test]
fn thinking_and_unmodelled_assistant_blocks_round_trip_through_canonical() {
    let dto: MessagesRequest = serde_json::from_value(json!({
        "model": "team-claude",
        "max_tokens": 256,
        "messages": [
            {"role": "user", "content": "weather?"},
            {"role": "assistant", "content": [
                {"type": "thinking", "thinking": "let me check", "signature": "sig-abc"},
                {"type": "text", "text": "checking", "vendor_text": "kept"},
                {"type": "tool_use", "id": "toolu_1", "name": "weather", "input": {}}
            ]},
            {"role": "user", "content": [
                {"type": "tool_result", "tool_use_id": "toolu_1", "content": "sunny"}
            ]}
        ]
    }))
    .unwrap();
    let Operation::Generation(canonical) = decode_request(dto).unwrap() else {
        panic!("wrong operation");
    };
    assert_eq!(
        canonical.extensions.values["/messages/1/content/0"]["signature"],
        "sig-abc"
    );

    let wire =
        serde_json::to_value(encode_request(&canonical, "claude-upstream").unwrap()).unwrap();
    let assistant = &wire["messages"][1]["content"];
    assert_eq!(assistant[0]["type"], "thinking");
    assert_eq!(assistant[0]["signature"], "sig-abc");
    assert_eq!(assistant[0]["thinking"], "let me check");
    assert_eq!(assistant[1]["type"], "text");
    assert_eq!(assistant[1]["text"], "checking");
    assert_eq!(assistant[1]["vendor_text"], "kept");
    assert_eq!(assistant[0].get("vendor_text"), None);
    assert_eq!(assistant[2]["type"], "tool_use");
    assert_eq!(assistant[2]["id"], "toolu_1");
}
#[test]
fn a_message_of_only_unmodelled_blocks_is_not_dropped() {
    let dto: MessagesRequest = serde_json::from_value(json!({
        "model": "team-claude",
        "max_tokens": 256,
        "messages": [
            {"role": "user", "content": "hi"},
            {"role": "assistant", "content": [
                {"type": "redacted_thinking", "data": "opaque"}
            ]},
            {"role": "user", "content": "continue"}
        ]
    }))
    .unwrap();
    let Operation::Generation(canonical) = decode_request(dto).unwrap() else {
        panic!("wrong operation");
    };
    let wire =
        serde_json::to_value(encode_request(&canonical, "claude-upstream").unwrap()).unwrap();
    assert_eq!(
        wire["messages"][1]["content"][0]["type"],
        "redacted_thinking"
    );
    assert_eq!(wire["messages"][1]["content"][0]["data"], "opaque");
}
#[test]
fn thinking_block_with_tool_use_fields_round_trips_idempotently() {
    // Found by the protocol_json fuzz target. Block classification must follow
    // `type`; guessing from field shape turned this thinking block into a tool
    // use on the second decode and rejected the encoder's own output.
    let document = json!({
        "max_tokens": 1024,
        "model": "claude",
        "messages": [
            { "role": "user", "content": "weather?" },
            { "role": "assistant", "content": [
                { "type": "thinking", "thinking": "check the tool", "signature": "sig-abc",
                  "id": "toolu_1", "name": "weather", "input": {} },
                { "type": "text", "text": "checking" }
            ] },
            { "role": "user", "content": [
                { "type": "tool_result", "tool_use_id": "toolu_1", "content": "sunny" }
            ] }
        ]
    });
    let request: MessagesRequest = serde_json::from_value(document).unwrap();
    let Operation::Generation(first) = decode_request(request).unwrap() else {
        panic!("expected a generation");
    };
    let encoded = serde_json::to_value(encode_request(&first, "upstream").unwrap()).unwrap();
    let reparsed: MessagesRequest = serde_json::from_value(encoded.clone()).unwrap();
    let Operation::Generation(second) = decode_request(reparsed).unwrap() else {
        panic!("the encoder's own output must decode to the same operation");
    };
    let re_encoded = serde_json::to_value(encode_request(&second, "upstream").unwrap()).unwrap();
    assert_eq!(encoded, re_encoded);
    assert_eq!(encoded["messages"][1]["content"][0]["type"], "thinking");
    assert_eq!(encoded["messages"][1]["content"][0]["id"], "toolu_1");
}
#[test]
fn document_block_with_base64_source_is_rejected_by_decoder() {
    let dto: MessagesRequest = serde_json::from_value(json!({
        "model": "team-claude",
        "max_tokens": 256,
        "messages": [
            {
                "role": "user",
                "content": [
                    {
                        "type": "document",
                        "source": {
                            "type": "base64",
                            "media_type": "application/pdf",
                            "data": "JVBERi0xLjQK"
                        }
                    }
                ]
            }
        ]
    }))
    .unwrap();
    assert!(decode_request(dto).is_err());
}
#[test]
fn document_block_with_url_source_round_trips_through_canonical() {
    let dto: MessagesRequest = serde_json::from_value(json!({
        "model": "team-claude",
        "max_tokens": 256,
        "messages": [
            {
                "role": "user",
                "content": [
                    {
                        "type": "document",
                        "source": {
                            "type": "url",
                            "url": "https://example.com/spec.pdf"
                        }
                    }
                ]
            }
        ]
    }))
    .unwrap();
    let Operation::Generation(canonical) = decode_request(dto).unwrap() else {
        panic!("wrong operation");
    };
    let wire =
        serde_json::to_value(encode_request(&canonical, "claude-upstream").unwrap()).unwrap();
    let document = &wire["messages"][0]["content"][0];
    assert_eq!(document["type"], "document");
    assert_eq!(document["source"]["type"], "url");
    assert_eq!(document["source"]["url"], "https://example.com/spec.pdf");
}
#[test]
fn unrecognised_content_block_type_is_rejected() {
    let dto: MessagesRequest = serde_json::from_value(json!({
        "model": "team-claude",
        "max_tokens": 256,
        "messages": [
            {
                "role": "user",
                "content": [
                    {
                        "type": "container_upload",
                        "container_id": "cnt_123"
                    }
                ]
            }
        ]
    }))
    .unwrap();
    assert!(decode_request(dto).is_err());
}
#[test]
fn document_block_nesting_base64_content_is_rejected() {
    let dto: MessagesRequest = serde_json::from_value(json!({
        "model": "team-claude",
        "max_tokens": 256,
        "messages": [
            {
                "role": "user",
                "content": [
                    {
                        "type": "document",
                        "source": {
                            "type": "content",
                            "content": [
                                {
                                    "type": "image",
                                    "source": {
                                        "type": "base64",
                                        "media_type": "image/png",
                                        "data": "AAAA"
                                    }
                                }
                            ]
                        }
                    }
                ]
            }
        ]
    }))
    .unwrap();
    assert!(decode_request(dto).is_err());
}
#[test]
fn count_tokens_rejects_a_base64_document_like_the_messages_surface() {
    let dto: CountTokensRequest = serde_json::from_value(json!({
        "model": "team-claude",
        "messages": [
            {
                "role": "user",
                "content": [
                    {
                        "type": "document",
                        "source": {
                            "type": "base64",
                            "media_type": "application/pdf",
                            "data": "JVBERi0xLjQK"
                        }
                    }
                ]
            }
        ]
    }))
    .unwrap();
    assert!(decode_count_tokens_request(dto).is_err());
}
