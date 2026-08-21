use std::collections::BTreeMap;

use serde_json::Value;
use thiserror::Error;

use crate::domain::{
    canonical::{
        identity::Surface,
        requests::{
            ContentPart as CanonicalContentPart, GenerationParameters, GenerationRequest,
            MediaSource, Message as CanonicalMessage, MessageRole, Operation,
            ResponseFormat as CanonicalResponseFormat, SourceExtensions,
            ToolCall as CanonicalToolCall, ToolChoice as CanonicalToolChoice, ToolDefinition,
            media_handle_from_inline_marker,
        },
    },
    ids::{RouteSlug, RouteSlugError},
};

use super::super::extensions::collect_extra;
use super::{
    CompletionRequest, ContentPart, Message, MessageContent, ResponseFormat, Role, StopSequences,
    Tool, ToolChoice,
};

pub fn chat_completion(request: CompletionRequest) -> Result<Operation, Error> {
    validate_request_parameters(&request)?;
    let route = RouteSlug::parse(request.model.clone())?;
    let mut extension_values = BTreeMap::new();
    collect_extra("", &request.extra, &mut extension_values);

    let messages = request
        .messages
        .into_iter()
        .enumerate()
        .map(|(index, message)| decode_message(index, message, &mut extension_values))
        .collect::<Result<Vec<_>, _>>()?;
    if messages.is_empty() {
        return Err(Error::EmptyMessages);
    }

    let tools = request
        .tools
        .into_iter()
        .enumerate()
        .map(|(index, tool)| decode_tool(index, tool, &mut extension_values))
        .collect::<Result<Vec<_>, _>>()?;
    let tool_choice = request
        .tool_choice
        .map(|choice| decode_tool_choice(choice, &mut extension_values))
        .transpose()?;
    let response_format = request
        .response_format
        .map(|format| decode_response_format(format, &mut extension_values))
        .transpose()?;

    Ok(Operation::Generation(GenerationRequest {
        route,
        messages,
        parameters: GenerationParameters {
            max_output_tokens: request.max_completion_tokens.or(request.max_tokens),
            temperature: request.temperature,
            top_p: request.top_p,
            stop_sequences: request
                .stop
                .map(StopSequences::into_vec)
                .unwrap_or_default(),
            candidate_count: request.n,
            seed: request.seed,
            parallel_tool_calls: request.parallel_tool_calls,
            stream: request.stream,
        },
        tools,
        tool_choice,
        response_format,
        extensions: SourceExtensions::new(Surface::OpenAi, extension_values),
    }))
}

fn validate_request_parameters(request: &CompletionRequest) -> Result<(), Error> {
    if request.max_completion_tokens.is_some() && request.max_tokens.is_some() {
        return Err(Error::ConflictingTokenLimits);
    }
    if request.n == Some(0) {
        return Err(Error::InvalidParameter {
            field: "n",
            reason: "must be greater than zero",
        });
    }
    if request
        .temperature
        .is_some_and(|value| !(0.0..=2.0).contains(&value))
    {
        return Err(Error::InvalidParameter {
            field: "temperature",
            reason: "must be between 0 and 2",
        });
    }
    if request
        .top_p
        .is_some_and(|value| !(0.0..=1.0).contains(&value))
    {
        return Err(Error::InvalidParameter {
            field: "top_p",
            reason: "must be between 0 and 1",
        });
    }
    Ok(())
}

fn decode_message(
    index: usize,
    message: Message,
    extensions: &mut BTreeMap<String, Value>,
) -> Result<CanonicalMessage, Error> {
    let prefix = format!("/messages/{index}");
    collect_extra(&prefix, &message.extra, extensions);

    let role = match message.role {
        Role::System => MessageRole::System,
        Role::Developer => MessageRole::Developer,
        Role::User => MessageRole::User,
        Role::Assistant => MessageRole::Assistant,
        Role::Tool => MessageRole::Tool,
    };
    if role == MessageRole::Tool && message.tool_call_id.is_none() {
        return Err(Error::MissingToolCallId {
            message_index: index,
        });
    }

    let content = match message.content {
        Some(MessageContent::Text(text)) => vec![CanonicalContentPart::Text { text }],
        Some(MessageContent::Parts(parts)) => parts
            .into_iter()
            .enumerate()
            .map(|(part_index, part)| decode_content_part(index, part_index, part, extensions))
            .collect::<Result<_, _>>()?,
        None => Vec::new(),
    };

    let tool_calls = message
        .tool_calls
        .into_iter()
        .enumerate()
        .map(|(tool_index, call)| {
            let call_prefix = format!("{prefix}/tool_calls/{tool_index}");
            collect_extra(&call_prefix, &call.extra, extensions);
            collect_extra(
                &format!("{call_prefix}/function"),
                &call.function.extra,
                extensions,
            );
            if call.kind != "function" {
                return Err(Error::UnsupportedToolType(call.kind));
            }
            Ok(CanonicalToolCall {
                id: call.id,
                name: call.function.name,
                arguments: call.function.arguments,
            })
        })
        .collect::<Result<Vec<_>, _>>()?;

    if content.is_empty() && tool_calls.is_empty() && role != MessageRole::Assistant {
        return Err(Error::EmptyMessage {
            message_index: index,
        });
    }

    Ok(CanonicalMessage {
        role,
        content,
        name: message.name,
        tool_call_id: message.tool_call_id,
        tool_calls,
    })
}

fn decode_content_part(
    message_index: usize,
    part_index: usize,
    part: ContentPart,
    extensions: &mut BTreeMap<String, Value>,
) -> Result<CanonicalContentPart, Error> {
    let prefix = format!("/messages/{message_index}/content/{part_index}");
    collect_extra(&prefix, part.extra(), extensions);
    match part {
        ContentPart::Text { text, .. } => Ok(CanonicalContentPart::Text { text }),
        ContentPart::ImageUrl { image_url, .. } => {
            collect_extra(&format!("{prefix}/image_url"), &image_url.extra, extensions);
            Ok(CanonicalContentPart::Image {
                source: MediaSource::Uri(image_url.url),
                detail: image_url.detail,
            })
        }
        ContentPart::InputAudio { input_audio, .. } => {
            collect_extra(
                &format!("{prefix}/input_audio"),
                &input_audio.extra,
                extensions,
            );
            let media = media_handle_from_inline_marker(&input_audio.data)
                .ok_or(Error::InlineMediaRequiresBoundedHandle)?;
            Ok(CanonicalContentPart::InputAudio {
                media,
                format: input_audio.format,
            })
        }
        ContentPart::Refusal { refusal, .. } => Ok(CanonicalContentPart::Refusal { text: refusal }),
    }
}

fn decode_tool(
    index: usize,
    tool: Tool,
    extensions: &mut BTreeMap<String, Value>,
) -> Result<ToolDefinition, Error> {
    if tool.kind != "function" {
        return Err(Error::UnsupportedToolType(tool.kind));
    }
    let prefix = format!("/tools/{index}");
    collect_extra(&prefix, &tool.extra, extensions);
    collect_extra(
        &format!("{prefix}/function"),
        &tool.function.extra,
        extensions,
    );
    Ok(ToolDefinition {
        name: tool.function.name,
        description: tool.function.description,
        input_schema: tool.function.parameters,
    })
}

fn decode_tool_choice(
    choice: ToolChoice,
    extensions: &mut BTreeMap<String, Value>,
) -> Result<CanonicalToolChoice, Error> {
    match choice {
        ToolChoice::Mode(mode) => match mode.as_str() {
            "auto" => Ok(CanonicalToolChoice::Auto),
            "none" => Ok(CanonicalToolChoice::None),
            "required" => Ok(CanonicalToolChoice::Required),
            _ => Err(Error::UnsupportedToolChoice(mode)),
        },
        ToolChoice::Named(named) => {
            if named.kind != "function" {
                return Err(Error::UnsupportedToolType(named.kind));
            }
            collect_extra("/tool_choice", &named.extra, extensions);
            collect_extra("/tool_choice/function", &named.function.extra, extensions);
            Ok(CanonicalToolChoice::Named(named.function.name))
        }
    }
}

fn decode_response_format(
    format: ResponseFormat,
    extensions: &mut BTreeMap<String, Value>,
) -> Result<CanonicalResponseFormat, Error> {
    collect_extra("/response_format", &format.extra, extensions);
    match format.kind.as_str() {
        "text" => Ok(CanonicalResponseFormat::Text),
        "json_object" => Ok(CanonicalResponseFormat::JsonObject),
        "json_schema" => {
            let schema = format.json_schema.ok_or(Error::MissingJsonSchema)?;
            collect_extra("/response_format/json_schema", &schema.extra, extensions);
            Ok(CanonicalResponseFormat::JsonSchema {
                name: schema.name,
                description: schema.description,
                schema: schema.schema,
                strict: schema.strict,
            })
        }
        kind => Err(Error::UnsupportedResponseFormat(kind.to_owned())),
    }
}

#[derive(Debug, Error)]
pub enum Error {
    #[error(transparent)]
    InvalidRoute(#[from] RouteSlugError),
    #[error("messages must contain at least one message")]
    EmptyMessages,
    #[error("message {message_index} must contain content or an assistant tool call")]
    EmptyMessage { message_index: usize },
    #[error("tool message {message_index} is missing tool_call_id")]
    MissingToolCallId { message_index: usize },
    #[error("max_tokens and max_completion_tokens cannot both be supplied")]
    ConflictingTokenLimits,
    #[error("{field} {reason}")]
    InvalidParameter {
        field: &'static str,
        reason: &'static str,
    },
    #[error("unsupported OpenAI tool type {0}")]
    UnsupportedToolType(String),
    #[error("unsupported OpenAI tool choice {0}")]
    UnsupportedToolChoice(String),
    #[error("unsupported OpenAI response format {0}")]
    UnsupportedResponseFormat(String),
    #[error("response_format type json_schema requires json_schema")]
    MissingJsonSchema,
    #[error("inline media must be admitted through a bounded media spool")]
    InlineMediaRequiresBoundedHandle,
}
