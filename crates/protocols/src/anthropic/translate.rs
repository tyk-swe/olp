use std::collections::BTreeMap;

use olp_domain::{
    CanonicalEvent, CanonicalEventKind, ContentPart, FinishReason, GenerationParameters,
    GenerationRequest, MediaSource as CanonicalMediaSource, Message as CanonicalMessage,
    MessageRole, Operation, RouteSlug, RouteSlugError, SourceExtensions, Surface, ToolCall,
    ToolChoice as CanonicalToolChoice, ToolDefinition, Usage as CanonicalUsage,
    inline_media_marker, media_handle_from_inline_marker,
};
use serde_json::Value;
use thiserror::Error;

use super::dto::{
    ContentBlock, ImageBlock, MediaSource, Message, MessageContent, MessagesRequest,
    MessagesResponse, Role, SystemPrompt, TextBlock, Tool, ToolChoice, ToolResultBlock,
    ToolResultContent, ToolUseBlock,
};

pub fn decode_messages_request(request: MessagesRequest) -> Result<Operation, DecodeError> {
    validate_request(&request)?;
    let route = RouteSlug::parse(request.model.clone())?;
    let mut extensions = BTreeMap::new();
    collect_extra("", &request.extra, &mut extensions);

    let mut messages = Vec::new();
    if let Some(system) = request.system {
        let mut content = Vec::new();
        match system {
            SystemPrompt::Text(text) => content.push(ContentPart::Text { text }),
            SystemPrompt::Blocks(blocks) => {
                for (index, block) in blocks.into_iter().enumerate() {
                    if block.kind != "text" {
                        return Err(DecodeError::UnexpectedType {
                            expected: "text",
                            actual: block.kind,
                        });
                    }
                    collect_extra(&format!("/system/{index}"), &block.extra, &mut extensions);
                    content.push(ContentPart::Text { text: block.text });
                }
            }
        }
        if !content.is_empty() {
            messages.push(CanonicalMessage {
                role: MessageRole::System,
                content,
                name: None,
                tool_call_id: None,
                tool_calls: Vec::new(),
            });
        }
    }

    let mut wire_message_index = 0_usize;
    for message in request.messages {
        let decoded = decode_message(message)?;
        for (segment_index, segment) in decoded.into_iter().enumerate() {
            let prefix = format!("/messages/{wire_message_index}");
            if segment_index == 0 {
                collect_extra(&prefix, &segment.message_extra, &mut extensions);
            }
            for (path, value) in segment.extensions {
                extensions.insert(format!("{prefix}{path}"), value);
            }
            messages.push(segment.message);
            wire_message_index += 1;
        }
    }
    if messages
        .iter()
        .all(|message| message.role == MessageRole::System)
    {
        return Err(DecodeError::EmptyMessages);
    }

    let mut tools = Vec::new();
    for (wire_tool_index, tool) in request.tools.into_iter().enumerate() {
        if let Some(input_schema) = tool.input_schema.clone()
            && tool.kind.is_none()
        {
            let prefix = format!("/tools/{wire_tool_index}");
            collect_extra(&prefix, &tool.extra, &mut extensions);
            tools.push(ToolDefinition {
                name: tool.name,
                description: tool.description,
                input_schema,
            });
        } else {
            extensions.insert(
                format!("/tools/{wire_tool_index}"),
                serde_json::to_value(tool).map_err(DecodeError::Json)?,
            );
        }
    }

    let (tool_choice, parallel_tool_calls) = request
        .tool_choice
        .map(|choice| decode_tool_choice(choice, &mut extensions))
        .transpose()?
        .unwrap_or((None, None));

    Ok(Operation::Generation(GenerationRequest {
        route,
        messages,
        parameters: GenerationParameters {
            max_output_tokens: Some(request.max_tokens),
            temperature: request.temperature,
            top_p: request.top_p,
            stop_sequences: request.stop_sequences,
            candidate_count: Some(1),
            seed: None,
            parallel_tool_calls,
            stream: request.stream,
        },
        tools,
        tool_choice,
        response_format: None,
        extensions: SourceExtensions::new(Surface::Anthropic, extensions),
    }))
}

fn validate_request(request: &MessagesRequest) -> Result<(), DecodeError> {
    if request.max_tokens == 0 {
        return Err(DecodeError::InvalidParameter {
            field: "max_tokens",
            reason: "must be greater than zero",
        });
    }
    if request
        .temperature
        .is_some_and(|value| !(0.0..=1.0).contains(&value))
    {
        return Err(DecodeError::InvalidParameter {
            field: "temperature",
            reason: "must be between 0 and 1",
        });
    }
    if request
        .top_p
        .is_some_and(|value| !(0.0..=1.0).contains(&value))
    {
        return Err(DecodeError::InvalidParameter {
            field: "top_p",
            reason: "must be between 0 and 1",
        });
    }
    Ok(())
}

struct DecodedSegment {
    message: CanonicalMessage,
    message_extra: BTreeMap<String, Value>,
    extensions: BTreeMap<String, Value>,
}

fn decode_message(message: Message) -> Result<Vec<DecodedSegment>, DecodeError> {
    let role = match message.role {
        Role::User => MessageRole::User,
        Role::Assistant => MessageRole::Assistant,
    };
    match message.content {
        MessageContent::Text(text) => Ok(vec![DecodedSegment {
            message: CanonicalMessage {
                role,
                content: vec![ContentPart::Text { text }],
                name: None,
                tool_call_id: None,
                tool_calls: Vec::new(),
            },
            message_extra: message.extra,
            extensions: BTreeMap::new(),
        }]),
        MessageContent::Blocks(blocks) => decode_blocks(role, blocks, message.extra),
    }
}

fn decode_blocks(
    role: MessageRole,
    blocks: Vec<ContentBlock>,
    message_extra: BTreeMap<String, Value>,
) -> Result<Vec<DecodedSegment>, DecodeError> {
    let mut segments = Vec::new();
    let mut content = Vec::new();
    let mut tool_calls = Vec::new();
    let mut content_extensions = Vec::new();
    let mut tool_extensions = Vec::new();
    let mut seen_tool_use = false;

    for block in blocks {
        match block {
            ContentBlock::ToolResult(result) => {
                if role != MessageRole::User {
                    return Err(DecodeError::ToolResultRole);
                }
                flush_regular_segment(
                    role,
                    &mut segments,
                    &mut content,
                    &mut tool_calls,
                    &mut content_extensions,
                    &mut tool_extensions,
                );
                segments.push(decode_tool_result(result)?);
                seen_tool_use = false;
            }
            ContentBlock::Text(block) => {
                require_kind(&block.kind, "text")?;
                if seen_tool_use {
                    return Err(DecodeError::InterleavedToolUse);
                }
                let index = content.len();
                content_extensions.push((format!("/content/{index}"), block.extra));
                content.push(ContentPart::Text { text: block.text });
            }
            ContentBlock::Image(block) => {
                require_kind(&block.kind, "image")?;
                if seen_tool_use {
                    return Err(DecodeError::InterleavedToolUse);
                }
                let index = content.len();
                let image = decode_image(block)?;
                content_extensions.push((format!("/content/{index}"), image.block_extra));
                content_extensions.push((format!("/content/{index}/source"), image.source_extra));
                content.push(image.part);
            }
            ContentBlock::ToolUse(block) => {
                require_kind(&block.kind, "tool_use")?;
                if role != MessageRole::Assistant {
                    return Err(DecodeError::ToolUseRole);
                }
                seen_tool_use = true;
                let index = tool_calls.len();
                tool_extensions.push((index, block.extra));
                tool_calls.push(ToolCall {
                    id: block.id,
                    name: block.name,
                    arguments: serde_json::to_string(&block.input).map_err(DecodeError::Json)?,
                });
            }
            ContentBlock::Thinking(block) => {
                return Err(DecodeError::UnsupportedContentBlock(block.kind));
            }
            ContentBlock::RedactedThinking(block) => {
                return Err(DecodeError::UnsupportedContentBlock(block.kind));
            }
            ContentBlock::Unknown(value) => {
                return Err(DecodeError::UnsupportedContentBlock(
                    value
                        .get("type")
                        .and_then(Value::as_str)
                        .unwrap_or("unknown")
                        .to_owned(),
                ));
            }
        }
    }
    flush_regular_segment(
        role,
        &mut segments,
        &mut content,
        &mut tool_calls,
        &mut content_extensions,
        &mut tool_extensions,
    );
    if segments.is_empty() {
        return Err(DecodeError::EmptyMessage);
    }
    if let Some(first) = segments.first_mut() {
        first.message_extra = message_extra;
    }
    Ok(segments)
}

fn flush_regular_segment(
    role: MessageRole,
    segments: &mut Vec<DecodedSegment>,
    content: &mut Vec<ContentPart>,
    tool_calls: &mut Vec<ToolCall>,
    content_extensions: &mut Vec<(String, BTreeMap<String, Value>)>,
    tool_extensions: &mut Vec<(usize, BTreeMap<String, Value>)>,
) {
    if content.is_empty() && tool_calls.is_empty() {
        return;
    }
    let content_count = content.len();
    let mut extensions = BTreeMap::new();
    for (prefix, extra) in content_extensions.drain(..) {
        collect_extra(&prefix, &extra, &mut extensions);
    }
    for (index, extra) in tool_extensions.drain(..) {
        collect_extra(
            &format!("/content/{}", content_count + index),
            &extra,
            &mut extensions,
        );
    }
    segments.push(DecodedSegment {
        message: CanonicalMessage {
            role,
            content: std::mem::take(content),
            name: None,
            tool_call_id: None,
            tool_calls: std::mem::take(tool_calls),
        },
        message_extra: BTreeMap::new(),
        extensions,
    });
}

struct DecodedImage {
    part: ContentPart,
    block_extra: BTreeMap<String, Value>,
    source_extra: BTreeMap<String, Value>,
}

fn decode_image(block: ImageBlock) -> Result<DecodedImage, DecodeError> {
    let MediaSource {
        kind,
        media_type,
        data,
        url,
        extra: mut source_extra,
    } = block.source;
    if kind == "base64" {
        let data = data.ok_or(DecodeError::InlineMediaRequiresBoundedHandle)?;
        let handle = media_handle_from_inline_marker(&data)
            .ok_or(DecodeError::InlineMediaRequiresBoundedHandle)?;
        let media_type = media_type.ok_or(DecodeError::InlineMediaRequiresBoundedHandle)?;
        source_extra.insert("media_type".into(), Value::String(media_type));
        return Ok(DecodedImage {
            part: ContentPart::Image {
                source: CanonicalMediaSource::Handle(handle),
                detail: None,
            },
            block_extra: block.extra,
            source_extra,
        });
    }
    if data.is_some() {
        return Err(DecodeError::UnsupportedMediaSource(kind));
    }
    if kind != "url" {
        return Err(DecodeError::UnsupportedMediaSource(kind));
    }
    let url = url.ok_or(DecodeError::MissingMediaUrl)?;
    if let Some(media_type) = media_type {
        source_extra.insert("media_type".into(), Value::String(media_type));
    }
    Ok(DecodedImage {
        part: ContentPart::Image {
            source: CanonicalMediaSource::Uri(url),
            detail: None,
        },
        block_extra: block.extra,
        source_extra,
    })
}

fn decode_tool_result(result: ToolResultBlock) -> Result<DecodedSegment, DecodeError> {
    require_kind(&result.kind, "tool_result")?;
    let mut extensions = BTreeMap::new();
    collect_extra("/content/0", &result.extra, &mut extensions);
    if let Some(is_error) = result.is_error {
        extensions.insert("/content/0/is_error".into(), Value::Bool(is_error));
    }
    let content = match result.content {
        None => Vec::new(),
        Some(ToolResultContent::Text(text)) => vec![ContentPart::Text { text }],
        Some(ToolResultContent::Blocks(blocks)) => {
            let mut content = Vec::new();
            for (index, block) in blocks.into_iter().enumerate() {
                match block {
                    ContentBlock::Text(block) => {
                        require_kind(&block.kind, "text")?;
                        collect_extra(
                            &format!("/content/0/content/{index}"),
                            &block.extra,
                            &mut extensions,
                        );
                        content.push(ContentPart::Text { text: block.text });
                    }
                    ContentBlock::Image(block) => {
                        require_kind(&block.kind, "image")?;
                        let image = decode_image(block)?;
                        collect_extra(
                            &format!("/content/0/content/{index}"),
                            &image.block_extra,
                            &mut extensions,
                        );
                        collect_extra(
                            &format!("/content/0/content/{index}/source"),
                            &image.source_extra,
                            &mut extensions,
                        );
                        content.push(image.part);
                    }
                    block => {
                        return Err(DecodeError::UnsupportedContentBlock(
                            block.kind().unwrap_or("unknown").to_owned(),
                        ));
                    }
                }
            }
            content
        }
    };
    Ok(DecodedSegment {
        message: CanonicalMessage {
            role: MessageRole::Tool,
            content,
            name: None,
            tool_call_id: Some(result.tool_use_id),
            tool_calls: Vec::new(),
        },
        message_extra: BTreeMap::new(),
        extensions,
    })
}

fn decode_tool_choice(
    choice: ToolChoice,
    extensions: &mut BTreeMap<String, Value>,
) -> Result<(Option<CanonicalToolChoice>, Option<bool>), DecodeError> {
    collect_extra("/tool_choice", &choice.extra, extensions);
    let canonical = match choice.kind.as_str() {
        "auto" => CanonicalToolChoice::Auto,
        "none" => CanonicalToolChoice::None,
        "any" => CanonicalToolChoice::Required,
        "tool" => CanonicalToolChoice::Named(choice.name.ok_or(DecodeError::MissingToolName)?),
        other => return Err(DecodeError::UnsupportedToolChoice(other.to_owned())),
    };
    Ok((
        Some(canonical),
        choice.disable_parallel_tool_use.map(|disabled| !disabled),
    ))
}

pub fn encode_messages_request(
    request: &GenerationRequest,
    provider_model: &str,
) -> Result<MessagesRequest, EncodeError> {
    request
        .extensions
        .ensure_representable_on(Surface::Anthropic)?;
    let max_tokens = request
        .parameters
        .max_output_tokens
        .ok_or(EncodeError::MissingMaxOutputTokens)?;
    if request
        .parameters
        .candidate_count
        .is_some_and(|count| count != 1)
    {
        return Err(EncodeError::CandidateCountUnsupported);
    }
    if request.parameters.seed.is_some() {
        return Err(EncodeError::SeedUnsupported);
    }
    if request.response_format.is_some() {
        return Err(EncodeError::ResponseFormatUnsupported);
    }
    let mut system_blocks = Vec::new();
    let mut messages = Vec::new();
    let mut conversation_started = false;
    for message in &request.messages {
        match message.role {
            MessageRole::System | MessageRole::Developer => {
                if conversation_started {
                    return Err(EncodeError::SystemMessageAfterConversation);
                }
                if message.name.is_some()
                    || message.tool_call_id.is_some()
                    || !message.tool_calls.is_empty()
                {
                    return Err(EncodeError::UnsupportedSystemContent);
                }
                for part in &message.content {
                    let ContentPart::Text { text } = part else {
                        return Err(EncodeError::UnsupportedSystemContent);
                    };
                    system_blocks.push(TextBlock {
                        kind: "text".into(),
                        text: text.clone(),
                        extra: BTreeMap::new(),
                    });
                }
            }
            _ => {
                conversation_started = true;
                let message_index = messages.len();
                let content_prefix = format!("/messages/{message_index}/content/");
                let force_blocks = request
                    .extensions
                    .values
                    .keys()
                    .any(|path| path.starts_with(&content_prefix));
                messages.push(encode_message(message, force_blocks)?);
            }
        }
    }
    if messages.is_empty() {
        return Err(EncodeError::EmptyMessages);
    }

    let tools = request
        .tools
        .iter()
        .map(|tool| Tool {
            name: tool.name.clone(),
            description: tool.description.clone(),
            input_schema: Some(tool.input_schema.clone()),
            kind: None,
            extra: BTreeMap::new(),
        })
        .collect();
    let tool_choice = request
        .tool_choice
        .as_ref()
        .map(|choice| ToolChoice {
            kind: match choice {
                CanonicalToolChoice::Auto => "auto",
                CanonicalToolChoice::None => "none",
                CanonicalToolChoice::Required => "any",
                CanonicalToolChoice::Named(_) => "tool",
            }
            .into(),
            name: match choice {
                CanonicalToolChoice::Named(name) => Some(name.clone()),
                _ => None,
            },
            disable_parallel_tool_use: request
                .parameters
                .parallel_tool_calls
                .map(|enabled| !enabled),
            extra: BTreeMap::new(),
        })
        .or_else(|| {
            request
                .parameters
                .parallel_tool_calls
                .map(|enabled| ToolChoice {
                    kind: "auto".into(),
                    name: None,
                    disable_parallel_tool_use: Some(!enabled),
                    extra: BTreeMap::new(),
                })
        });
    let force_system_blocks = request
        .extensions
        .values
        .keys()
        .any(|path| path.starts_with("/system/"));
    let system = match system_blocks.as_slice() {
        [] => None,
        [block] if block.extra.is_empty() && !force_system_blocks => {
            Some(SystemPrompt::Text(block.text.clone()))
        }
        _ => Some(SystemPrompt::Blocks(system_blocks)),
    };
    let mut encoded = MessagesRequest {
        model: provider_model.to_owned(),
        messages,
        max_tokens,
        system,
        stop_sequences: request.parameters.stop_sequences.clone(),
        temperature: request.parameters.temperature,
        top_p: request.parameters.top_p,
        tools,
        tool_choice,
        stream: request.parameters.stream,
        extra: BTreeMap::new(),
    };
    apply_extensions(&mut encoded, &request.extensions.values)?;
    Ok(encoded)
}

fn encode_message(
    message: &CanonicalMessage,
    force_content_blocks: bool,
) -> Result<Message, EncodeError> {
    if message.name.is_some() {
        return Err(EncodeError::MessageNameUnsupported);
    }
    if message.role == MessageRole::Tool {
        let tool_use_id = message
            .tool_call_id
            .clone()
            .ok_or(EncodeError::MissingToolCallId)?;
        let blocks = encode_content(&message.content)?;
        let content = match blocks.as_slice() {
            [ContentBlock::Text(block)] if block.extra.is_empty() => {
                Some(ToolResultContent::Text(block.text.clone()))
            }
            _ if blocks.is_empty() => None,
            _ => Some(ToolResultContent::Blocks(blocks)),
        };
        return Ok(Message {
            role: Role::User,
            content: MessageContent::Blocks(vec![ContentBlock::ToolResult(ToolResultBlock {
                kind: "tool_result".into(),
                tool_use_id,
                content,
                is_error: None,
                extra: BTreeMap::new(),
            })]),
            extra: BTreeMap::new(),
        });
    }
    let role = match message.role {
        MessageRole::User => Role::User,
        MessageRole::Assistant => Role::Assistant,
        MessageRole::System | MessageRole::Developer | MessageRole::Tool => unreachable!(),
    };
    if message.tool_call_id.is_some() {
        return Err(EncodeError::UnexpectedToolCallId);
    }
    if role == Role::User && !message.tool_calls.is_empty() {
        return Err(EncodeError::ToolUseRole);
    }
    let mut blocks = encode_content(&message.content)?;
    for call in &message.tool_calls {
        let input = serde_json::from_str(&call.arguments).map_err(|source| {
            EncodeError::InvalidToolArguments {
                tool: call.name.clone(),
                source,
            }
        })?;
        blocks.push(ContentBlock::ToolUse(ToolUseBlock {
            kind: "tool_use".into(),
            id: call.id.clone(),
            name: call.name.clone(),
            input,
            extra: BTreeMap::new(),
        }));
    }
    let content = match blocks.as_slice() {
        [ContentBlock::Text(block)] if block.extra.is_empty() && !force_content_blocks => {
            MessageContent::Text(block.text.clone())
        }
        _ => MessageContent::Blocks(blocks),
    };
    Ok(Message {
        role,
        content,
        extra: BTreeMap::new(),
    })
}

fn encode_content(content: &[ContentPart]) -> Result<Vec<ContentBlock>, EncodeError> {
    content
        .iter()
        .map(|part| match part {
            ContentPart::Text { text } => Ok(ContentBlock::Text(TextBlock {
                kind: "text".into(),
                text: text.clone(),
                extra: BTreeMap::new(),
            })),
            ContentPart::Image { source, detail } => {
                if detail.is_some() {
                    return Err(EncodeError::ImageDetailUnsupported);
                }
                let (kind, data, url) = match source {
                    CanonicalMediaSource::Uri(url) => ("url", None, Some(url.clone())),
                    CanonicalMediaSource::Handle(handle) => {
                        ("base64", Some(inline_media_marker(handle)), None)
                    }
                };
                Ok(ContentBlock::Image(ImageBlock {
                    kind: "image".into(),
                    source: MediaSource {
                        kind: kind.into(),
                        media_type: None,
                        data,
                        url,
                        extra: BTreeMap::new(),
                    },
                    extra: BTreeMap::new(),
                }))
            }
            ContentPart::InputAudio { .. } => Err(EncodeError::InputAudioUnsupported),
            ContentPart::InputFile { .. } => Err(EncodeError::InputFileUnsupported),
            ContentPart::Refusal { .. } => Err(EncodeError::RefusalUnsupported),
        })
        .collect()
}

pub fn decode_messages_response(
    response: MessagesResponse,
) -> Result<Vec<CanonicalEvent>, ResponseError> {
    if response.role != Role::Assistant {
        return Err(ResponseError::UnexpectedRole);
    }
    if response.kind != "message" {
        return Err(ResponseError::UnexpectedType(response.kind));
    }
    let mut builder = EventBuilder::default();
    builder.push(CanonicalEventKind::ResponseStart {
        response_id: Some(response.id),
        provider_model: Some(response.model),
    });
    builder.push(CanonicalEventKind::MessageStart {
        output_index: 0,
        role: MessageRole::Assistant,
    });
    let mut extensions = BTreeMap::new();
    collect_extra("", &response.extra, &mut extensions);
    let mut tool_index = 0_u32;
    for (index, block) in response.content.into_iter().enumerate() {
        match block {
            ContentBlock::Text(block) => {
                require_response_kind(&block.kind, "text")?;
                collect_extra(&format!("/content/{index}"), &block.extra, &mut extensions);
                builder.push(CanonicalEventKind::TextDelta {
                    output_index: 0,
                    text: block.text,
                });
            }
            ContentBlock::ToolUse(block) => {
                require_response_kind(&block.kind, "tool_use")?;
                collect_extra(&format!("/content/{index}"), &block.extra, &mut extensions);
                builder.push(CanonicalEventKind::ToolCallDelta {
                    output_index: 0,
                    tool_index,
                    id: Some(block.id),
                    name: Some(block.name),
                    arguments_delta: serde_json::to_string(&block.input)
                        .map_err(ResponseError::Json)?,
                });
                tool_index = tool_index
                    .checked_add(1)
                    .ok_or(ResponseError::TooManyContentBlocks)?;
            }
            other => {
                extensions.insert(format!("/content/{index}"), other.as_value());
            }
        }
    }
    collect_usage_extensions(&response.usage, &mut extensions);
    if let Some(stop_sequence) = response.stop_sequence {
        extensions.insert("/stop_sequence".into(), Value::String(stop_sequence));
    }
    if !extensions.is_empty() {
        builder.push(CanonicalEventKind::SourceExtension {
            extensions: SourceExtensions::new(Surface::Anthropic, extensions),
        });
    }
    builder.push(CanonicalEventKind::Usage {
        usage: canonical_usage(&response.usage),
    });
    let stop_reason = response
        .stop_reason
        .ok_or(ResponseError::MissingStopReason)?;
    builder.push(CanonicalEventKind::Finish {
        output_index: 0,
        reason: anthropic_finish_reason(&stop_reason),
    });
    builder.push(CanonicalEventKind::Done);
    Ok(builder.events)
}

pub(crate) fn canonical_usage(usage: &super::dto::Usage) -> CanonicalUsage {
    let input_tokens = usage
        .input_tokens
        .saturating_add(usage.cache_creation_input_tokens.unwrap_or(0))
        .saturating_add(usage.cache_read_input_tokens.unwrap_or(0));
    CanonicalUsage {
        input_tokens,
        output_tokens: usage.output_tokens,
        total_tokens: input_tokens.saturating_add(usage.output_tokens),
        cached_input_tokens: usage.cache_read_input_tokens,
        reasoning_tokens: None,
    }
}

pub(crate) fn collect_usage_extensions(
    usage: &super::dto::Usage,
    extensions: &mut BTreeMap<String, Value>,
) {
    collect_extra("/usage", &usage.extra, extensions);
    if let Some(tokens) = usage.cache_creation_input_tokens {
        extensions.insert(
            "/usage/cache_creation_input_tokens".into(),
            Value::from(tokens),
        );
    }
}

pub(crate) fn anthropic_finish_reason(reason: &str) -> FinishReason {
    match reason {
        "end_turn" | "stop_sequence" => FinishReason::Stop,
        "max_tokens" | "model_context_window_exceeded" => FinishReason::Length,
        "tool_use" => FinishReason::ToolCalls,
        "refusal" => FinishReason::ContentFilter,
        other => FinishReason::Other(other.to_owned()),
    }
}

#[derive(Default)]
pub(crate) struct EventBuilder {
    pub(crate) events: Vec<CanonicalEvent>,
}

impl EventBuilder {
    pub(crate) fn push(&mut self, kind: CanonicalEventKind) {
        let sequence = self.events.len().try_into().unwrap_or(u64::MAX);
        self.events.push(CanonicalEvent::new(sequence, kind));
    }
}

fn require_kind(actual: &str, expected: &'static str) -> Result<(), DecodeError> {
    if actual == expected {
        Ok(())
    } else {
        Err(DecodeError::UnexpectedType {
            expected,
            actual: actual.to_owned(),
        })
    }
}

fn require_response_kind(actual: &str, expected: &'static str) -> Result<(), ResponseError> {
    if actual == expected {
        Ok(())
    } else {
        Err(ResponseError::UnexpectedType(actual.to_owned()))
    }
}

pub(crate) fn collect_extra(
    prefix: &str,
    extra: &BTreeMap<String, Value>,
    extensions: &mut BTreeMap<String, Value>,
) {
    for (key, value) in extra {
        extensions.insert(format!("{prefix}/{}", escape_pointer(key)), value.clone());
    }
}

fn escape_pointer(value: &str) -> String {
    value.replace('~', "~0").replace('/', "~1")
}

fn unescape_pointer(value: &str) -> String {
    value.replace("~1", "/").replace("~0", "~")
}

fn apply_extensions(
    request: &mut MessagesRequest,
    extensions: &BTreeMap<String, Value>,
) -> Result<(), EncodeError> {
    if extensions.is_empty() {
        return Ok(());
    }
    let mut value = serde_json::to_value(&*request).map_err(EncodeError::Json)?;
    let mut insertions = extensions
        .iter()
        .filter(|(path, _)| is_array_item_path(path))
        .collect::<Vec<_>>();
    insertions.sort_by_key(|(path, _)| array_path_key(path));
    for (path, extension) in insertions {
        set_pointer(&mut value, path, extension.clone(), true)?;
    }
    for (path, extension) in extensions {
        if !is_array_item_path(path) {
            set_pointer(&mut value, path, extension.clone(), false)?;
        }
    }
    *request = serde_json::from_value(value).map_err(EncodeError::Json)?;
    Ok(())
}

fn is_array_item_path(path: &str) -> bool {
    let segments = path.trim_start_matches('/').split('/').collect::<Vec<_>>();
    matches!(segments.as_slice(), ["tools", index] if index.parse::<usize>().is_ok())
}

fn array_path_key(path: &str) -> (String, usize) {
    let (parent, index) = path.rsplit_once('/').unwrap_or((path, "0"));
    (parent.to_owned(), index.parse().unwrap_or(0))
}

fn set_pointer(
    root: &mut Value,
    pointer: &str,
    value: Value,
    insert_array_item: bool,
) -> Result<(), EncodeError> {
    let segments = pointer
        .strip_prefix('/')
        .ok_or_else(|| EncodeError::InvalidExtensionPath(pointer.to_owned()))?
        .split('/')
        .map(unescape_pointer)
        .collect::<Vec<_>>();
    if segments.is_empty() || segments.len() > 16 {
        return Err(EncodeError::InvalidExtensionPath(pointer.to_owned()));
    }
    let mut current = root;
    for (position, segment) in segments.iter().enumerate() {
        let terminal = position + 1 == segments.len();
        match current {
            Value::Object(object) if terminal => {
                object.insert(segment.clone(), value);
                return Ok(());
            }
            Value::Array(array) if terminal => {
                let index = segment
                    .parse::<usize>()
                    .map_err(|_| EncodeError::InvalidExtensionPath(pointer.to_owned()))?;
                if insert_array_item && index <= array.len() {
                    array.insert(index, value);
                    return Ok(());
                }
                let slot = array
                    .get_mut(index)
                    .ok_or_else(|| EncodeError::InvalidExtensionPath(pointer.to_owned()))?;
                *slot = value;
                return Ok(());
            }
            Value::Object(object) => {
                current = object
                    .get_mut(segment)
                    .ok_or_else(|| EncodeError::InvalidExtensionPath(pointer.to_owned()))?;
            }
            Value::Array(array) => {
                let index = segment
                    .parse::<usize>()
                    .map_err(|_| EncodeError::InvalidExtensionPath(pointer.to_owned()))?;
                current = array
                    .get_mut(index)
                    .ok_or_else(|| EncodeError::InvalidExtensionPath(pointer.to_owned()))?;
            }
            _ => return Err(EncodeError::InvalidExtensionPath(pointer.to_owned())),
        }
    }
    Err(EncodeError::InvalidExtensionPath(pointer.to_owned()))
}

#[derive(Debug, Error)]
pub enum DecodeError {
    #[error(transparent)]
    InvalidRoute(#[from] RouteSlugError),
    #[error("messages must contain at least one non-system message")]
    EmptyMessages,
    #[error("message content cannot be empty")]
    EmptyMessage,
    #[error("{field} {reason}")]
    InvalidParameter {
        field: &'static str,
        reason: &'static str,
    },
    #[error("expected Anthropic content type {expected}, got {actual}")]
    UnexpectedType {
        expected: &'static str,
        actual: String,
    },
    #[error("Anthropic tool_use blocks are valid only in assistant messages")]
    ToolUseRole,
    #[error("Anthropic tool_result blocks are valid only in user messages")]
    ToolResultRole,
    #[error("text or image content after tool_use cannot be reordered canonically")]
    InterleavedToolUse,
    #[error("inline base64 media must be replaced by a bounded media handle before translation")]
    InlineMediaRequiresBoundedHandle,
    #[error("unsupported Anthropic media source type {0}")]
    UnsupportedMediaSource(String),
    #[error("Anthropic URL media source is missing url")]
    MissingMediaUrl,
    #[error("unsupported Anthropic content block {0}")]
    UnsupportedContentBlock(String),
    #[error("unsupported Anthropic tool choice {0}")]
    UnsupportedToolChoice(String),
    #[error("Anthropic named tool choice is missing name")]
    MissingToolName,
    #[error("Anthropic JSON value is invalid: {0}")]
    Json(serde_json::Error),
}

#[derive(Debug, Error)]
pub enum EncodeError {
    #[error(transparent)]
    Extensions(#[from] olp_domain::ExtensionError),
    #[error("Anthropic Messages requires max_output_tokens")]
    MissingMaxOutputTokens,
    #[error("Anthropic Messages requires at least one conversation message")]
    EmptyMessages,
    #[error("system or developer messages cannot appear after conversation content")]
    SystemMessageAfterConversation,
    #[error("Anthropic system prompts support text content only")]
    UnsupportedSystemContent,
    #[error("canonical message name is not representable by Anthropic Messages")]
    MessageNameUnsupported,
    #[error("tool_call_id is valid only on a canonical tool result message")]
    UnexpectedToolCallId,
    #[error("tool result message is missing tool_call_id")]
    MissingToolCallId,
    #[error("tool calls are valid only in assistant messages")]
    ToolUseRole,
    #[error("media handle cannot be encoded as an Anthropic URL source")]
    MediaHandleCannotBeEncoded,
    #[error("input audio is not representable by the launch Anthropic Messages surface")]
    InputAudioUnsupported,
    #[error("input files are not representable by the launch Anthropic Messages surface")]
    InputFileUnsupported,
    #[error("OpenAI-style image detail is not representable by Anthropic Messages")]
    ImageDetailUnsupported,
    #[error("a canonical refusal marker is not representable by Anthropic request content")]
    RefusalUnsupported,
    #[error("Anthropic Messages supports exactly one candidate")]
    CandidateCountUnsupported,
    #[error("Anthropic Messages does not support a deterministic seed")]
    SeedUnsupported,
    #[error("canonical response format requires an explicit Anthropic output-config translation")]
    ResponseFormatUnsupported,
    #[error("tool {tool} arguments are not valid JSON: {source}")]
    InvalidToolArguments {
        tool: String,
        source: serde_json::Error,
    },
    #[error("source extension path cannot be applied: {0}")]
    InvalidExtensionPath(String),
    #[error("Anthropic JSON value is invalid: {0}")]
    Json(serde_json::Error),
}

#[derive(Debug, Error)]
pub enum ResponseError {
    #[error("Anthropic response role is not assistant")]
    UnexpectedRole,
    #[error("unexpected Anthropic response content type {0}")]
    UnexpectedType(String),
    #[error("Anthropic response is missing stop_reason")]
    MissingStopReason,
    #[error("Anthropic response has too many content blocks")]
    TooManyContentBlocks,
    #[error("Anthropic response JSON is invalid: {0}")]
    Json(serde_json::Error),
}
