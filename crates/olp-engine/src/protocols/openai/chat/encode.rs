use std::collections::BTreeMap;

use serde_json::Value;
use thiserror::Error;

use crate::domain::canonical::{
    identity::Surface,
    requests::{
        ContentPart as CanonicalContentPart, GenerationRequest, MediaSource,
        Message as CanonicalMessage, MessageRole, ResponseFormat as CanonicalResponseFormat,
        ToolChoice as CanonicalToolChoice, inline_media_marker, is_delivery_only_extension,
    },
};

use super::super::extensions::unescape_json_pointer;
use super::{
    CompletionRequest, ContentPart, FunctionCall, FunctionDefinition, ImageUrl, InputAudio,
    JsonSchema, Message, MessageContent, NamedFunction, NamedToolChoice, ResponseFormat, Role,
    StopSequences, Tool, ToolCall, ToolChoice,
};

pub fn chat_completion(
    request: &GenerationRequest,
    upstream_model: &str,
) -> Result<CompletionRequest, Error> {
    request
        .extensions
        .ensure_representable_on(Surface::OpenAi)?;
    let messages = request
        .messages
        .iter()
        .enumerate()
        .map(|(index, message)| {
            let content_prefix = format!("/messages/{index}/content/");
            let force_parts = request
                .extensions
                .values
                .keys()
                .any(|path| path.starts_with(&content_prefix));
            encode_message(message, force_parts)
        })
        .collect::<Result<Vec<_>, _>>()?;
    let tools = request
        .tools
        .iter()
        .map(|tool| Tool {
            kind: "function".into(),
            function: FunctionDefinition {
                name: tool.name.clone(),
                description: tool.description.clone(),
                parameters: tool.input_schema.clone(),
                extra: BTreeMap::new(),
            },
            extra: BTreeMap::new(),
        })
        .collect();
    let tool_choice = request.tool_choice.as_ref().map(|choice| match choice {
        CanonicalToolChoice::Auto => ToolChoice::Mode("auto".into()),
        CanonicalToolChoice::None => ToolChoice::Mode("none".into()),
        CanonicalToolChoice::Required => ToolChoice::Mode("required".into()),
        CanonicalToolChoice::Named(name) => ToolChoice::Named(NamedToolChoice {
            kind: "function".into(),
            function: NamedFunction {
                name: name.clone(),
                extra: BTreeMap::new(),
            },
            extra: BTreeMap::new(),
        }),
    });
    let response_format = request.response_format.as_ref().map(|format| match format {
        CanonicalResponseFormat::Text => ResponseFormat {
            kind: "text".into(),
            json_schema: None,
            extra: BTreeMap::new(),
        },
        CanonicalResponseFormat::JsonObject => ResponseFormat {
            kind: "json_object".into(),
            json_schema: None,
            extra: BTreeMap::new(),
        },
        CanonicalResponseFormat::JsonSchema {
            name,
            description,
            schema,
            strict,
        } => ResponseFormat {
            kind: "json_schema".into(),
            json_schema: Some(JsonSchema {
                name: name.clone(),
                description: description.clone(),
                schema: schema.clone(),
                strict: *strict,
                extra: BTreeMap::new(),
            }),
            extra: BTreeMap::new(),
        },
    });

    let mut encoded = CompletionRequest {
        model: upstream_model.to_owned(),
        messages,
        max_completion_tokens: request.parameters.max_output_tokens,
        max_tokens: None,
        temperature: request.parameters.temperature,
        top_p: request.parameters.top_p,
        stop: StopSequences::from_vec(&request.parameters.stop_sequences),
        n: request.parameters.candidate_count,
        seed: request.parameters.seed,
        parallel_tool_calls: request.parameters.parallel_tool_calls,
        stream: request.parameters.stream,
        tools,
        tool_choice,
        response_format,
        extra: BTreeMap::new(),
    };
    apply_extensions(&mut encoded, &request.extensions.values)?;
    Ok(encoded)
}

fn encode_message(message: &CanonicalMessage, force_content_parts: bool) -> Result<Message, Error> {
    let role = match message.role {
        MessageRole::System => Role::System,
        MessageRole::Developer => Role::Developer,
        MessageRole::User => Role::User,
        MessageRole::Assistant => Role::Assistant,
        MessageRole::Tool => Role::Tool,
    };
    let mut parts = Vec::with_capacity(message.content.len());
    for part in &message.content {
        parts.push(match part {
            CanonicalContentPart::Text { text } => ContentPart::Text {
                text: text.clone(),
                extra: BTreeMap::new(),
            },
            CanonicalContentPart::Image { source, detail, .. } => {
                let MediaSource::Uri(url) = source else {
                    return Err(Error::MediaHandleCannotBeEncoded);
                };
                ContentPart::ImageUrl {
                    image_url: ImageUrl {
                        url: url.clone(),
                        detail: detail.clone(),
                        extra: BTreeMap::new(),
                    },
                    extra: BTreeMap::new(),
                }
            }
            CanonicalContentPart::Refusal { text } => ContentPart::Refusal {
                refusal: text.clone(),
                extra: BTreeMap::new(),
            },
            CanonicalContentPart::InputAudio { media, format } => {
                if !matches!(format.as_str(), "wav" | "mp3") {
                    return Err(Error::InvalidInputAudioFormat);
                }
                ContentPart::InputAudio {
                    input_audio: InputAudio {
                        data: inline_media_marker(media),
                        format: format.clone(),
                        extra: BTreeMap::new(),
                    },
                    extra: BTreeMap::new(),
                }
            }
            CanonicalContentPart::InputFile { .. } => {
                return Err(Error::InputFileUnsupported);
            }
        });
    }
    let content = match parts.as_slice() {
        [] => None,
        [ContentPart::Text { text, extra }] if extra.is_empty() && !force_content_parts => {
            Some(MessageContent::Text(text.clone()))
        }
        _ => Some(MessageContent::Parts(parts)),
    };
    Ok(Message {
        role,
        content,
        name: message.name.clone(),
        tool_call_id: message.tool_call_id.clone(),
        tool_calls: message
            .tool_calls
            .iter()
            .map(|call| ToolCall {
                id: call.id.clone(),
                kind: "function".into(),
                function: FunctionCall {
                    name: call.name.clone(),
                    arguments: call.arguments.clone(),
                    extra: BTreeMap::new(),
                },
                extra: BTreeMap::new(),
            })
            .collect(),
        extra: BTreeMap::new(),
    })
}

fn apply_extensions(
    request: &mut CompletionRequest,
    extensions: &BTreeMap<String, Value>,
) -> Result<(), Error> {
    for (pointer, value) in extensions
        .iter()
        .filter(|(pointer, _)| !is_delivery_only_extension(pointer))
    {
        let segments = pointer
            .strip_prefix('/')
            .ok_or_else(|| Error::InvalidExtensionPath(pointer.clone()))?
            .split('/')
            .map(unescape_json_pointer)
            .collect::<Vec<_>>();
        match segments.as_slice() {
            [field] => {
                request.extra.insert(field.clone(), value.clone());
            }
            [messages, index, field] if messages == "messages" => {
                let message = message_mut(request, index, pointer)?;
                message.extra.insert(field.clone(), value.clone());
            }
            [messages, message_index, content, part_index, field]
                if messages == "messages" && content == "content" =>
            {
                let part = content_part_mut(request, message_index, part_index, pointer)?;
                part.extra_mut().insert(field.clone(), value.clone());
            }
            [
                messages,
                message_index,
                content,
                part_index,
                image_url,
                field,
            ] if messages == "messages" && content == "content" && image_url == "image_url" => {
                let part = content_part_mut(request, message_index, part_index, pointer)?;
                let ContentPart::ImageUrl { image_url, .. } = part else {
                    return Err(Error::InvalidExtensionPath(pointer.clone()));
                };
                image_url.extra.insert(field.clone(), value.clone());
            }
            [messages, message_index, tool_calls, tool_index, field]
                if messages == "messages" && tool_calls == "tool_calls" =>
            {
                let call = tool_call_mut(request, message_index, tool_index, pointer)?;
                call.extra.insert(field.clone(), value.clone());
            }
            [
                messages,
                message_index,
                tool_calls,
                tool_index,
                function,
                field,
            ] if messages == "messages" && tool_calls == "tool_calls" && function == "function" => {
                let call = tool_call_mut(request, message_index, tool_index, pointer)?;
                call.function.extra.insert(field.clone(), value.clone());
            }
            [tools, index, field] if tools == "tools" => {
                let tool = indexed_mut(&mut request.tools, index, pointer)?;
                tool.extra.insert(field.clone(), value.clone());
            }
            [tools, index, function, field] if tools == "tools" && function == "function" => {
                let tool = indexed_mut(&mut request.tools, index, pointer)?;
                tool.function.extra.insert(field.clone(), value.clone());
            }
            [choice, field] if choice == "tool_choice" => {
                let Some(ToolChoice::Named(choice)) = &mut request.tool_choice else {
                    return Err(Error::InvalidExtensionPath(pointer.clone()));
                };
                choice.extra.insert(field.clone(), value.clone());
            }
            [choice, function, field] if choice == "tool_choice" && function == "function" => {
                let Some(ToolChoice::Named(choice)) = &mut request.tool_choice else {
                    return Err(Error::InvalidExtensionPath(pointer.clone()));
                };
                choice.function.extra.insert(field.clone(), value.clone());
            }
            [format, field] if format == "response_format" => {
                let Some(format) = &mut request.response_format else {
                    return Err(Error::InvalidExtensionPath(pointer.clone()));
                };
                format.extra.insert(field.clone(), value.clone());
            }
            [format, schema, field] if format == "response_format" && schema == "json_schema" => {
                let Some(ResponseFormat {
                    json_schema: Some(schema),
                    ..
                }) = &mut request.response_format
                else {
                    return Err(Error::InvalidExtensionPath(pointer.clone()));
                };
                schema.extra.insert(field.clone(), value.clone());
            }
            _ => return Err(Error::InvalidExtensionPath(pointer.clone())),
        }
    }
    Ok(())
}

fn message_mut<'a>(
    request: &'a mut CompletionRequest,
    index: &str,
    pointer: &str,
) -> Result<&'a mut Message, Error> {
    indexed_mut(&mut request.messages, index, pointer)
}

fn content_part_mut<'a>(
    request: &'a mut CompletionRequest,
    message_index: &str,
    part_index: &str,
    pointer: &str,
) -> Result<&'a mut ContentPart, Error> {
    let message = message_mut(request, message_index, pointer)?;
    let Some(MessageContent::Parts(parts)) = &mut message.content else {
        return Err(Error::InvalidExtensionPath(pointer.to_owned()));
    };
    indexed_mut(parts, part_index, pointer)
}

fn tool_call_mut<'a>(
    request: &'a mut CompletionRequest,
    message_index: &str,
    tool_index: &str,
    pointer: &str,
) -> Result<&'a mut ToolCall, Error> {
    let message = message_mut(request, message_index, pointer)?;
    indexed_mut(&mut message.tool_calls, tool_index, pointer)
}

fn indexed_mut<'a, T>(values: &'a mut [T], index: &str, pointer: &str) -> Result<&'a mut T, Error> {
    index
        .parse::<usize>()
        .ok()
        .and_then(|index| values.get_mut(index))
        .ok_or_else(|| Error::InvalidExtensionPath(pointer.to_owned()))
}

#[derive(Debug, Error)]
pub enum Error {
    #[error(transparent)]
    Extensions(#[from] crate::domain::canonical::requests::ExtensionError),
    #[error("a media handle cannot be encoded as an OpenAI image URL")]
    MediaHandleCannotBeEncoded,
    #[error("canonical input file is not supported by Chat Completions")]
    InputFileUnsupported,
    #[error("OpenAI input_audio supports only wav or mp3")]
    InvalidInputAudioFormat,
    #[error("source extension path cannot be applied: {0}")]
    InvalidExtensionPath(String),
}
