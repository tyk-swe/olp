use std::collections::BTreeMap;

use crate::domain::{
    canonical::requests::{
        GenerationParameters, Message, SourceExtensions, ToolCall, ToolDefinition,
    },
    ids::RouteSlug,
};
use aws_sdk_bedrockruntime::types::{
    ConverseOutput as BedrockConverseOutput, TokenUsage, ToolUseBlock,
};

use super::*;

fn request() -> GenerationRequest {
    GenerationRequest {
        route: RouteSlug::parse("chat").unwrap(),
        messages: vec![Message {
            role: MessageRole::User,
            content: vec![ContentPart::Text {
                text: "hello".to_owned(),
            }],
            name: None,
            tool_call_id: None,
            tool_calls: vec![],
        }],
        parameters: GenerationParameters::default(),
        tools: vec![],
        tool_choice: None,
        response_format: None,
        extensions: SourceExtensions::default(),
    }
}

fn tool(name: &str) -> ToolDefinition {
    ToolDefinition {
        name: name.to_owned(),
        description: None,
        input_schema: serde_json::json!({"type": "object"}),
    }
}

fn assert_protocol(result: Result<EncodedConverse, TransportError>, message: &str) {
    let error = result.err().unwrap();
    assert_eq!(
        error.class,
        crate::domain::ports::AttemptFailureClass::Protocol
    );
    assert!(error.message.contains(message), "{}", error.message);
}

#[test]
fn encodes_text_and_tool_configuration() {
    let mut request = request();
    request.tools.push(ToolDefinition {
        name: "weather".to_owned(),
        description: Some("Get weather".to_owned()),
        input_schema: serde_json::json!({"type":"object"}),
    });
    request.tool_choice = Some(ToolChoice::Named("weather".to_owned()));
    let encoded = encode_generation(&request).unwrap();
    assert_eq!(encoded.messages.len(), 1);
    assert!(encoded.tool_config.is_some());
}

#[test]
fn rejects_unrepresentable_semantics() {
    let mut seed_request = request();
    seed_request.parameters.seed = Some(7);
    assert!(encode_generation(&seed_request).is_err());
    seed_request.parameters.seed = None;
    seed_request.extensions.values = BTreeMap::from([("reasoning".to_owned(), Value::Bool(true))]);
    assert!(encode_generation(&seed_request).is_err());

    let mut empty_message_request = request();
    empty_message_request.messages[0].content.clear();
    assert!(encode_generation(&empty_message_request).is_err());

    let mut user_tool_request = request();
    user_tool_request.messages[0].tool_calls.push(ToolCall {
        id: "call-1".to_owned(),
        name: "weather".to_owned(),
        arguments: "{}".to_owned(),
    });
    assert!(encode_generation(&user_tool_request).is_err());
}

#[test]
fn encodes_prior_tool_call() {
    let mut request = request();
    request.messages.push(Message {
        role: MessageRole::Assistant,
        content: vec![],
        name: None,
        tool_call_id: None,
        tool_calls: vec![ToolCall {
            id: "call-1".to_owned(),
            name: "weather".to_owned(),
            arguments: "{\"city\":\"Paris\"}".to_owned(),
        }],
    });
    assert_eq!(encode_generation(&request).unwrap().messages.len(), 2);
}

#[test]
fn decodes_text_tools_usage_and_finish() {
    let tool = ToolUseBlock::builder()
        .tool_use_id("call-1")
        .name("weather")
        .input(Document::Object(HashMap::from([(
            "city".to_owned(),
            Document::String("Paris".to_owned()),
        )])))
        .build()
        .unwrap();
    let message = BedrockMessage::builder()
        .role(ConversationRole::Assistant)
        .content(ContentBlock::Text("answer".to_owned()))
        .content(ContentBlock::ToolUse(tool))
        .build()
        .unwrap();
    let response = ConverseOutput::builder()
        .output(BedrockConverseOutput::Message(message))
        .stop_reason(StopReason::ToolUse)
        .usage(
            TokenUsage::builder()
                .input_tokens(4)
                .output_tokens(2)
                .total_tokens(6)
                .build()
                .unwrap(),
        )
        .build()
        .unwrap();
    let events = decode_converse(response, "anthropic.claude-test").unwrap();
    assert!(
        events
            .iter()
            .any(|event| matches!(event.kind, Kind::ToolCallDelta { .. }))
    );
    assert!(matches!(events.last().unwrap().kind, Kind::Done));
}

#[test]
fn prompt_cache_usage_follows_the_canonical_inclusive_input_contract() {
    let usage = TokenUsage::builder()
        .input_tokens(2)
        .output_tokens(3)
        .total_tokens(5)
        .cache_read_input_tokens(7)
        .cache_write_input_tokens(11)
        .build()
        .unwrap();

    assert_eq!(
        decode_usage(&usage).unwrap(),
        Usage {
            input_tokens: 20,
            output_tokens: 3,
            total_tokens: 23,
            cached_input_tokens: Some(7),
            reasoning_tokens: None,
        }
    );
}

#[test]
fn generation_validation_rejects_lossy_request_shapes() {
    type RequestMutator = fn(&mut GenerationRequest);
    let cases: [(&str, RequestMutator); 9] = [
        ("exactly one candidate", |r| {
            r.parameters.candidate_count = Some(2)
        }),
        ("parallel tool-call", |r| {
            r.parameters.parallel_tool_calls = Some(false)
        }),
        ("structured response", |r| {
            r.response_format = Some(ResponseFormat::JsonObject)
        }),
        ("names", |r| {
            r.messages[0].role = MessageRole::System;
            r.messages[0].name = Some("alice".into());
        }),
        ("at least one", |r| r.messages[0].role = MessageRole::System),
        ("text only", |r| {
            r.messages[0].role = MessageRole::System;
            r.messages[0].content = vec![ContentPart::Refusal { text: "no".into() }];
        }),
        ("output token", |r| {
            r.parameters.max_output_tokens = Some(i32::MAX as u32 + 1)
        }),
        ("finite", |r| r.parameters.temperature = Some(f32::NAN)),
        ("text message parts", |r| {
            r.messages[0].content = vec![ContentPart::Refusal { text: "no".into() }]
        }),
    ];
    for (message, mutate) in cases {
        let mut candidate = request();
        mutate(&mut candidate);
        assert_protocol(encode_generation(&candidate), message);
    }
}

#[test]
fn tool_configuration_and_calls_are_strictly_validated() {
    type RequestMutator = fn(&mut GenerationRequest);
    let cases: [(&str, RequestMutator); 5] = [
        ("requires at least one tool", |r| {
            r.tool_choice = Some(ToolChoice::Required)
        }),
        ("unique", |r| r.tools = vec![tool("same"), tool("same")]),
        ("tool name", |r| r.tools = vec![tool("bad name")]),
        ("does not exist", |r| {
            r.tools = vec![tool("known")];
            r.tool_choice = Some(ToolChoice::Named("missing".into()));
        }),
        ("cannot disable", |r| {
            r.tools = vec![tool("known")];
            r.tool_choice = Some(ToolChoice::None);
        }),
    ];
    for (message, mutate) in cases {
        let mut candidate = request();
        mutate(&mut candidate);
        assert_protocol(encode_generation(&candidate), message);
    }

    for (id, name, arguments, message) in [
        ("", "tool", "{}", "tool call ID"),
        ("call", "bad name", "{}", "tool name"),
        ("call", "tool", "{", "valid JSON"),
    ] {
        let mut candidate = request();
        candidate.messages[0].role = MessageRole::Assistant;
        candidate.messages[0].content.clear();
        candidate.messages[0].tool_calls = vec![ToolCall {
            id: id.into(),
            name: name.into(),
            arguments: arguments.into(),
        }];
        assert_protocol(encode_generation(&candidate), message);
    }
}

#[test]
fn tool_results_and_token_counting_enforce_content_invariants() {
    let tool_result = || Message {
        role: MessageRole::Tool,
        content: vec![ContentPart::Text {
            text: "sunny".into(),
        }],
        name: None,
        tool_call_id: Some("call-1".into()),
        tool_calls: vec![],
    };
    let mut valid = request();
    valid.messages.push(tool_result());
    assert_eq!(encode_generation(&valid).unwrap().messages.len(), 2);

    type MessageMutator = fn(&mut Message);
    let cases: [(&str, MessageMutator); 4] = [
        ("tool call ID", |m| m.tool_call_id = None),
        ("requires content", |m| m.content.clear()),
        ("text only", |m| {
            m.content = vec![ContentPart::Refusal { text: "no".into() }]
        }),
        ("cannot contain tool calls", |m| {
            m.tool_calls.push(ToolCall {
                id: "nested".into(),
                name: "tool".into(),
                arguments: "{}".into(),
            })
        }),
    ];
    for (message, mutate) in cases {
        let mut candidate = request();
        let mut result = tool_result();
        mutate(&mut result);
        candidate.messages.push(result);
        assert_protocol(encode_generation(&candidate), message);
    }

    let count = |input| TokenCountRequest {
        route: RouteSlug::parse("chat").unwrap(),
        input,
        extensions: SourceExtensions::default(),
    };
    assert_eq!(
        encode_token_count(&count(vec![
            ContentPart::Text { text: "one".into() },
            ContentPart::Text { text: "two".into() },
        ]))
        .unwrap()
        .messages()
        .len(),
        1
    );
    for input in [vec![], vec![ContentPart::Refusal { text: "no".into() }]] {
        assert!(encode_token_count(&count(input)).is_err());
    }
    let mut extended = count(vec![ContentPart::Text { text: "one".into() }]);
    extended
        .extensions
        .values
        .insert("vendor".into(), Value::Null);
    assert!(encode_token_count(&extended).is_err());
}

#[test]
fn wire_value_and_finish_reason_mappings_are_total_and_checked() {
    let value = serde_json::json!({
        "null": null, "bool": true, "text": "x", "array": [1, -2, 1.5]
    });
    assert_eq!(
        document_to_json(json_to_document(&value).unwrap()).unwrap(),
        value
    );
    assert!(document_to_json(Document::Number(Number::Float(f64::NAN))).is_err());

    for (reason, expected) in [
        (StopReason::EndTurn, FinishReason::Stop),
        (StopReason::StopSequence, FinishReason::Stop),
        (StopReason::MaxTokens, FinishReason::Length),
        (StopReason::ModelContextWindowExceeded, FinishReason::Length),
        (StopReason::ToolUse, FinishReason::ToolCalls),
        (StopReason::ContentFiltered, FinishReason::ContentFilter),
        (StopReason::GuardrailIntervened, FinishReason::ContentFilter),
        (StopReason::MalformedToolUse, FinishReason::Error),
        (
            StopReason::from("future_reason"),
            FinishReason::Other("future_reason".into()),
        ),
    ] {
        assert_eq!(decode_stop_reason(&reason), expected);
    }

    for usage in [
        TokenUsage::builder()
            .input_tokens(-1)
            .output_tokens(0)
            .total_tokens(0)
            .build(),
        TokenUsage::builder()
            .input_tokens(0)
            .output_tokens(-1)
            .total_tokens(0)
            .build(),
        TokenUsage::builder()
            .input_tokens(0)
            .output_tokens(0)
            .total_tokens(-1)
            .build(),
        TokenUsage::builder()
            .input_tokens(0)
            .output_tokens(0)
            .total_tokens(0)
            .cache_read_input_tokens(-1)
            .build(),
        TokenUsage::builder()
            .input_tokens(0)
            .output_tokens(0)
            .total_tokens(0)
            .cache_write_input_tokens(-1)
            .build(),
    ] {
        assert!(decode_usage(&usage.unwrap()).is_err());
    }
}
