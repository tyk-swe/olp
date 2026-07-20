use std::collections::BTreeMap;

use olp_domain::{
    ContentPart, GenerationParameters, GenerationRequest, MediaSource as CanonicalMediaSource,
    Message as CanonicalMessage, MessageRole, Operation, RouteSlug, SourceExtensions, Surface,
    ToolCall, ToolChoice as CanonicalToolChoice, ToolDefinition, media_handle_from_inline_marker,
};
use serde_json::Value;

use super::super::dto::{
    ContentBlock, ImageBlock, MediaSource, Message, MessageContent, MessagesRequest, Role,
    SystemPrompt, ToolChoice, ToolResultBlock, ToolResultContent,
};
use super::errors::DecodeError;
use super::extensions::{collect_extra, require_kind};

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
