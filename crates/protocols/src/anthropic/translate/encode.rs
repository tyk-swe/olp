use std::collections::BTreeMap;

use olp_domain::{
    ContentPart, GenerationRequest, MediaSource as CanonicalMediaSource,
    Message as CanonicalMessage, MessageRole, Surface, ToolChoice as CanonicalToolChoice,
    inline_media_marker,
};

use super::super::dto::{
    ContentBlock, ImageBlock, MediaSource, Message, MessageContent, MessagesRequest, Role,
    SystemPrompt, TextBlock, Tool, ToolChoice, ToolResultBlock, ToolResultContent, ToolUseBlock,
};
use super::errors::EncodeError;
use super::extensions::apply_extensions;

pub fn encode_messages_request(
    request: &GenerationRequest,
    upstream_model: &str,
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
        model: upstream_model.to_owned(),
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
