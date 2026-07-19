use std::collections::{HashMap, HashSet};

use aws_sdk_bedrockruntime::{
    operation::converse::ConverseOutput,
    types::{
        AnyToolChoice, AutoToolChoice, ContentBlock, ConversationRole,
        ConverseOutput as BedrockConverseOutput, ConverseTokensRequest, InferenceConfiguration,
        Message as BedrockMessage, SpecificToolChoice, StopReason, SystemContentBlock, Tool,
        ToolChoice as BedrockToolChoice, ToolConfiguration, ToolInputSchema, ToolResultBlock,
        ToolResultContentBlock, ToolSpecification, ToolUseBlock,
    },
};
use aws_smithy_types::{Document, Number};
use olp_domain::{
    CanonicalEvent, CanonicalEventKind, ContentPart, FinishReason, GenerationRequest, Message,
    MessageRole, ResponseFormat, TokenCountRequest, ToolChoice, TransportError, Usage,
};
use serde_json::Value;

pub(crate) struct EncodedConverse {
    pub messages: Vec<BedrockMessage>,
    pub system: Vec<SystemContentBlock>,
    pub inference_config: InferenceConfiguration,
    pub tool_config: Option<ToolConfiguration>,
}

pub(crate) fn encode_generation(
    request: &GenerationRequest,
) -> Result<EncodedConverse, TransportError> {
    reject_extensions(&request.extensions)?;
    if request
        .parameters
        .candidate_count
        .is_some_and(|count| count != 1)
    {
        return Err(protocol_error(
            "Bedrock Converse returns exactly one candidate",
        ));
    }
    if request.parameters.seed.is_some() {
        return Err(protocol_error(
            "Bedrock Converse does not represent a deterministic seed",
        ));
    }
    if request.parameters.parallel_tool_calls.is_some() {
        return Err(protocol_error(
            "Bedrock Converse does not represent parallel tool-call selection",
        ));
    }
    if request
        .response_format
        .as_ref()
        .is_some_and(|format| !matches!(format, ResponseFormat::Text))
    {
        return Err(protocol_error(
            "Bedrock Converse cannot represent the requested structured response format",
        ));
    }

    let mut system = Vec::new();
    let mut messages = Vec::new();
    for message in &request.messages {
        if matches!(message.role, MessageRole::System | MessageRole::Developer) {
            if message.name.is_some()
                || message.tool_call_id.is_some()
                || !message.tool_calls.is_empty()
            {
                return Err(protocol_error(
                    "Bedrock system instructions cannot represent names or tool-call metadata",
                ));
            }
            for part in &message.content {
                match part {
                    ContentPart::Text { text } => {
                        system.push(SystemContentBlock::Text(text.clone()));
                    }
                    _ => {
                        return Err(protocol_error(
                            "Bedrock system instructions support canonical text only",
                        ));
                    }
                }
            }
        } else {
            messages.push(encode_message(message)?);
        }
    }
    if messages.is_empty() {
        return Err(protocol_error(
            "Bedrock Converse requires at least one user or assistant message",
        ));
    }

    let max_tokens = request
        .parameters
        .max_output_tokens
        .map(i32::try_from)
        .transpose()
        .map_err(|_| protocol_error("maximum output token count exceeds Bedrock limits"))?;
    for value in [request.parameters.temperature, request.parameters.top_p]
        .into_iter()
        .flatten()
    {
        if !value.is_finite() {
            return Err(protocol_error(
                "Bedrock inference parameters must be finite",
            ));
        }
    }
    let inference_config = InferenceConfiguration::builder()
        .set_max_tokens(max_tokens)
        .set_temperature(request.parameters.temperature)
        .set_top_p(request.parameters.top_p)
        .set_stop_sequences(
            (!request.parameters.stop_sequences.is_empty())
                .then(|| request.parameters.stop_sequences.clone()),
        )
        .build();
    let tool_config = encode_tools(request)?;
    Ok(EncodedConverse {
        messages,
        system,
        inference_config,
        tool_config,
    })
}

pub(crate) fn encode_token_count(
    request: &TokenCountRequest,
) -> Result<ConverseTokensRequest, TransportError> {
    reject_extensions(&request.extensions)?;
    let mut builder = BedrockMessage::builder().role(ConversationRole::User);
    if request.input.is_empty() {
        return Err(protocol_error(
            "Bedrock token counting requires nonempty input",
        ));
    }
    for part in &request.input {
        let ContentPart::Text { text } = part else {
            return Err(protocol_error(
                "Bedrock token counting currently supports canonical text only",
            ));
        };
        builder = builder.content(ContentBlock::Text(text.clone()));
    }
    let message = builder
        .build()
        .map_err(|_| protocol_error("cannot build Bedrock token-count message"))?;
    Ok(ConverseTokensRequest::builder().messages(message).build())
}

fn encode_message(message: &Message) -> Result<BedrockMessage, TransportError> {
    if message.name.is_some() {
        return Err(protocol_error(
            "Bedrock messages do not represent participant names",
        ));
    }
    let (role, tool_result) = match message.role {
        MessageRole::User => (ConversationRole::User, false),
        MessageRole::Assistant => (ConversationRole::Assistant, false),
        MessageRole::Tool => (ConversationRole::User, true),
        MessageRole::System | MessageRole::Developer => {
            return Err(protocol_error("system messages must be encoded separately"));
        }
    };
    let mut builder = BedrockMessage::builder().role(role);
    if tool_result {
        if !message.tool_calls.is_empty() {
            return Err(protocol_error("a tool result cannot contain tool calls"));
        }
        let tool_use_id = message
            .tool_call_id
            .as_deref()
            .filter(|value| !value.is_empty())
            .ok_or_else(|| protocol_error("a Bedrock tool result requires a tool call ID"))?;
        let mut result = ToolResultBlock::builder().tool_use_id(tool_use_id);
        if message.content.is_empty() {
            return Err(protocol_error("a Bedrock tool result requires content"));
        }
        for part in &message.content {
            let ContentPart::Text { text } = part else {
                return Err(protocol_error(
                    "Bedrock tool results currently support canonical text only",
                ));
            };
            result = result.content(ToolResultContentBlock::Text(text.clone()));
        }
        builder = builder.content(ContentBlock::ToolResult(
            result
                .build()
                .map_err(|_| protocol_error("cannot build Bedrock tool result"))?,
        ));
    } else {
        if message.tool_call_id.is_some() {
            return Err(protocol_error(
                "only canonical tool-result messages may carry a tool call ID",
            ));
        }
        if !message.tool_calls.is_empty() && message.role != MessageRole::Assistant {
            return Err(protocol_error(
                "only assistant messages may contain Bedrock tool calls",
            ));
        }
        if message.content.is_empty() && message.tool_calls.is_empty() {
            return Err(protocol_error(
                "Bedrock messages require text content or an assistant tool call",
            ));
        }
        for part in &message.content {
            match part {
                ContentPart::Text { text } => {
                    builder = builder.content(ContentBlock::Text(text.clone()));
                }
                _ => {
                    return Err(protocol_error(
                        "Bedrock generation currently supports canonical text message parts only",
                    ));
                }
            }
        }
        for call in &message.tool_calls {
            validate_identifier(&call.id, "tool call ID")?;
            validate_tool_name(&call.name)?;
            let input: Value = serde_json::from_str(&call.arguments)
                .map_err(|_| protocol_error("tool call arguments must be valid JSON"))?;
            builder = builder.content(ContentBlock::ToolUse(
                ToolUseBlock::builder()
                    .tool_use_id(&call.id)
                    .name(&call.name)
                    .input(json_to_document(&input)?)
                    .build()
                    .map_err(|_| protocol_error("cannot build Bedrock tool call"))?,
            ));
        }
    }
    builder
        .build()
        .map_err(|_| protocol_error("cannot build Bedrock message"))
}

fn encode_tools(request: &GenerationRequest) -> Result<Option<ToolConfiguration>, TransportError> {
    if request.tools.is_empty() {
        return match request.tool_choice.as_ref() {
            None | Some(ToolChoice::None) => Ok(None),
            _ => Err(protocol_error("tool choice requires at least one tool")),
        };
    }
    let mut names = HashSet::new();
    let mut builder = ToolConfiguration::builder();
    for tool in &request.tools {
        validate_tool_name(&tool.name)?;
        if !names.insert(tool.name.as_str()) {
            return Err(protocol_error("Bedrock tool names must be unique"));
        }
        let specification = ToolSpecification::builder()
            .name(&tool.name)
            .set_description(tool.description.clone())
            .input_schema(ToolInputSchema::Json(json_to_document(&tool.input_schema)?))
            .build()
            .map_err(|_| protocol_error("cannot build Bedrock tool specification"))?;
        builder = builder.tools(Tool::ToolSpec(specification));
    }
    let choice = match request.tool_choice.as_ref() {
        None | Some(ToolChoice::Auto) => {
            Some(BedrockToolChoice::Auto(AutoToolChoice::builder().build()))
        }
        Some(ToolChoice::Required) => {
            Some(BedrockToolChoice::Any(AnyToolChoice::builder().build()))
        }
        Some(ToolChoice::Named(name)) => {
            if !names.contains(name.as_str()) {
                return Err(protocol_error("named tool choice does not exist"));
            }
            Some(BedrockToolChoice::Tool(
                SpecificToolChoice::builder()
                    .name(name)
                    .build()
                    .map_err(|_| protocol_error("cannot build Bedrock named tool choice"))?,
            ))
        }
        Some(ToolChoice::None) => {
            return Err(protocol_error(
                "Bedrock Converse cannot disable a nonempty tool list",
            ));
        }
    };
    builder = builder.set_tool_choice(choice);
    builder
        .build()
        .map(Some)
        .map_err(|_| protocol_error("cannot build Bedrock tool configuration"))
}

pub(crate) fn decode_converse(
    response: ConverseOutput,
    provider_model: &str,
) -> Result<Vec<CanonicalEvent>, TransportError> {
    if response.trace.is_some() || response.additional_model_response_fields.is_some() {
        return Err(protocol_body_error(
            "Bedrock returned guardrail or vendor semantics that cannot be represented canonically",
        ));
    }
    let mut kinds = vec![
        CanonicalEventKind::ResponseStart {
            response_id: None,
            provider_model: Some(provider_model.to_owned()),
        },
        CanonicalEventKind::MessageStart {
            output_index: 0,
            role: MessageRole::Assistant,
        },
    ];
    let output = response
        .output
        .ok_or_else(|| protocol_body_error("Bedrock response omitted output"))?;
    let BedrockConverseOutput::Message(message) = output else {
        return Err(protocol_body_error(
            "Bedrock returned an unknown Converse output variant",
        ));
    };
    let mut tool_index = 0_u32;
    for block in message.content {
        match block {
            ContentBlock::Text(text) => kinds.push(CanonicalEventKind::TextDelta {
                output_index: 0,
                text,
            }),
            ContentBlock::ToolUse(tool) => {
                let arguments = document_to_json(tool.input)?;
                kinds.push(CanonicalEventKind::ToolCallDelta {
                    output_index: 0,
                    tool_index,
                    id: Some(tool.tool_use_id),
                    name: Some(tool.name),
                    arguments_delta: serde_json::to_string(&arguments).map_err(|_| {
                        protocol_body_error("Bedrock tool input cannot be serialized")
                    })?,
                });
                tool_index = tool_index.saturating_add(1);
            }
            _ => {
                return Err(protocol_body_error(
                    "Bedrock returned content that cannot be represented canonically",
                ));
            }
        }
    }
    if let Some(usage) = response.usage {
        kinds.push(CanonicalEventKind::Usage {
            usage: decode_usage(&usage)?,
        });
    }
    kinds.push(CanonicalEventKind::Finish {
        output_index: 0,
        reason: decode_stop_reason(&response.stop_reason),
    });
    kinds.push(CanonicalEventKind::Done);
    Ok(kinds
        .into_iter()
        .enumerate()
        .map(|(sequence, kind)| CanonicalEvent::new(sequence as u64, kind))
        .collect())
}

pub(crate) fn decode_usage(
    usage: &aws_sdk_bedrockruntime::types::TokenUsage,
) -> Result<Usage, TransportError> {
    Ok(Usage {
        input_tokens: nonnegative_tokens(usage.input_tokens, "input")?,
        output_tokens: nonnegative_tokens(usage.output_tokens, "output")?,
        total_tokens: nonnegative_tokens(usage.total_tokens, "total")?,
        cached_input_tokens: None,
        reasoning_tokens: None,
    })
}

pub(crate) fn decode_stop_reason(reason: &StopReason) -> FinishReason {
    match reason {
        StopReason::EndTurn | StopReason::StopSequence => FinishReason::Stop,
        StopReason::MaxTokens | StopReason::ModelContextWindowExceeded => FinishReason::Length,
        StopReason::ToolUse => FinishReason::ToolCalls,
        StopReason::ContentFiltered | StopReason::GuardrailIntervened => {
            FinishReason::ContentFilter
        }
        StopReason::MalformedModelOutput | StopReason::MalformedToolUse => FinishReason::Error,
        other => FinishReason::Other(other.as_str().to_owned()),
    }
}

fn reject_extensions(extensions: &olp_domain::SourceExtensions) -> Result<(), TransportError> {
    if extensions.is_empty() {
        Ok(())
    } else {
        Err(protocol_error(
            "source-scoped vendor fields cannot be represented by Bedrock Converse",
        ))
    }
}

fn validate_identifier(value: &str, label: &str) -> Result<(), TransportError> {
    if value.is_empty() || value.len() > 256 || value.chars().any(char::is_control) {
        return Err(protocol_error(format!("Bedrock {label} is invalid")));
    }
    Ok(())
}

fn validate_tool_name(name: &str) -> Result<(), TransportError> {
    if name.is_empty()
        || name.len() > 64
        || name
            .bytes()
            .any(|byte| !byte.is_ascii_alphanumeric() && byte != b'_' && byte != b'-')
    {
        return Err(protocol_error("Bedrock tool name is invalid"));
    }
    Ok(())
}

fn json_to_document(value: &Value) -> Result<Document, TransportError> {
    match value {
        Value::Null => Ok(Document::Null),
        Value::Bool(value) => Ok(Document::Bool(*value)),
        Value::String(value) => Ok(Document::String(value.clone())),
        Value::Array(values) => values
            .iter()
            .map(json_to_document)
            .collect::<Result<Vec<_>, _>>()
            .map(Document::Array),
        Value::Object(values) => values
            .iter()
            .map(|(key, value)| Ok((key.clone(), json_to_document(value)?)))
            .collect::<Result<HashMap<_, _>, _>>()
            .map(Document::Object),
        Value::Number(value) => {
            if let Some(value) = value.as_u64() {
                Ok(Document::Number(Number::PosInt(value)))
            } else if let Some(value) = value.as_i64() {
                Ok(Document::Number(Number::NegInt(value)))
            } else {
                value
                    .as_f64()
                    .filter(|value| value.is_finite())
                    .map(|value| Document::Number(Number::Float(value)))
                    .ok_or_else(|| protocol_error("Bedrock JSON number is not finite"))
            }
        }
    }
}

fn document_to_json(document: Document) -> Result<Value, TransportError> {
    match document {
        Document::Null => Ok(Value::Null),
        Document::Bool(value) => Ok(Value::Bool(value)),
        Document::String(value) => Ok(Value::String(value)),
        Document::Array(values) => values
            .into_iter()
            .map(document_to_json)
            .collect::<Result<Vec<_>, _>>()
            .map(Value::Array),
        Document::Object(values) => values
            .into_iter()
            .map(|(key, value)| Ok((key, document_to_json(value)?)))
            .collect::<Result<serde_json::Map<_, _>, _>>()
            .map(Value::Object),
        Document::Number(Number::PosInt(value)) => Ok(Value::Number(value.into())),
        Document::Number(Number::NegInt(value)) => Ok(Value::Number(value.into())),
        Document::Number(Number::Float(value)) => serde_json::Number::from_f64(value)
            .map(Value::Number)
            .ok_or_else(|| protocol_body_error("Bedrock returned a non-finite number")),
    }
}

fn nonnegative_tokens(value: i32, label: &str) -> Result<u64, TransportError> {
    u64::try_from(value).map_err(|_| {
        protocol_body_error(format!("Bedrock returned a negative {label} token count"))
    })
}

pub(crate) fn protocol_error(message: impl Into<String>) -> TransportError {
    TransportError {
        phase: olp_domain::TransportPhase::Connect,
        class: olp_domain::AttemptFailureClass::Protocol,
        response_committed: false,
        message: message.into(),
    }
}

pub(crate) fn protocol_body_error(message: impl Into<String>) -> TransportError {
    TransportError {
        phase: olp_domain::TransportPhase::Body,
        class: olp_domain::AttemptFailureClass::Protocol,
        response_committed: true,
        message: message.into(),
    }
}

#[cfg(test)]
mod tests {
    use std::collections::BTreeMap;

    use aws_sdk_bedrockruntime::types::{
        ConverseOutput as BedrockConverseOutput, TokenUsage, ToolUseBlock,
    };
    use olp_domain::{
        GenerationParameters, Message, RouteSlug, SourceExtensions, ToolCall, ToolDefinition,
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
        seed_request.extensions.values =
            BTreeMap::from([("reasoning".to_owned(), Value::Bool(true))]);
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
                .any(|event| matches!(event.kind, CanonicalEventKind::ToolCallDelta { .. }))
        );
        assert!(matches!(
            events.last().unwrap().kind,
            CanonicalEventKind::Done
        ));
    }
}
