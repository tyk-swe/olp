use std::collections::BTreeMap;

use olp_domain::{
    ContentPart, GenerationParameters, GenerationRequest, MediaSource, Message, MessageRole,
    Operation, ResponseFormat, RouteSlug, RouteSlugError, SourceExtensions, Surface, ToolCall,
    ToolChoice, ToolDefinition, inline_media_marker, media_handle_from_inline_marker,
};
use serde::{Deserialize, Serialize};
use serde_json::Value;
use thiserror::Error;

#[derive(Clone, Debug, Deserialize, PartialEq, Serialize)]
pub struct ChatCompletionRequest {
    pub model: String,
    pub messages: Vec<ChatMessage>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub max_completion_tokens: Option<u32>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub max_tokens: Option<u32>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub temperature: Option<f32>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub top_p: Option<f32>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub stop: Option<StopSequences>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub n: Option<u16>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub seed: Option<i64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub parallel_tool_calls: Option<bool>,
    #[serde(default, skip_serializing_if = "is_false")]
    pub stream: bool,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub tools: Vec<ChatTool>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub tool_choice: Option<ChatToolChoice>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub response_format: Option<ChatResponseFormat>,
    #[serde(flatten)]
    pub extra: BTreeMap<String, Value>,
}

const fn is_false(value: &bool) -> bool {
    !*value
}

#[derive(Clone, Debug, Deserialize, PartialEq, Serialize)]
pub struct ChatMessage {
    pub role: ChatRole,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub content: Option<ChatMessageContent>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub name: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub tool_call_id: Option<String>,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub tool_calls: Vec<ChatToolCall>,
    #[serde(flatten)]
    pub extra: BTreeMap<String, Value>,
}

#[derive(Clone, Copy, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(rename_all = "lowercase")]
pub enum ChatRole {
    System,
    Developer,
    User,
    Assistant,
    Tool,
}

#[derive(Clone, Debug, Deserialize, PartialEq, Serialize)]
#[serde(untagged)]
pub enum ChatMessageContent {
    Text(String),
    Parts(Vec<ChatContentPart>),
}

#[derive(Clone, Debug, Deserialize, PartialEq, Serialize)]
#[serde(tag = "type", rename_all = "snake_case")]
pub enum ChatContentPart {
    Text {
        text: String,
        #[serde(flatten)]
        extra: BTreeMap<String, Value>,
    },
    ImageUrl {
        image_url: ChatImageUrl,
        #[serde(flatten)]
        extra: BTreeMap<String, Value>,
    },
    InputAudio {
        input_audio: ChatInputAudio,
        #[serde(flatten)]
        extra: BTreeMap<String, Value>,
    },
    Refusal {
        refusal: String,
        #[serde(flatten)]
        extra: BTreeMap<String, Value>,
    },
}

impl ChatContentPart {
    fn extra(&self) -> &BTreeMap<String, Value> {
        match self {
            Self::Text { extra, .. }
            | Self::ImageUrl { extra, .. }
            | Self::InputAudio { extra, .. }
            | Self::Refusal { extra, .. } => extra,
        }
    }

    fn extra_mut(&mut self) -> &mut BTreeMap<String, Value> {
        match self {
            Self::Text { extra, .. }
            | Self::ImageUrl { extra, .. }
            | Self::InputAudio { extra, .. }
            | Self::Refusal { extra, .. } => extra,
        }
    }
}

#[derive(Clone, Debug, Deserialize, PartialEq, Serialize)]
pub struct ChatImageUrl {
    pub url: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub detail: Option<String>,
    #[serde(flatten)]
    pub extra: BTreeMap<String, Value>,
}

#[derive(Clone, Debug, Deserialize, PartialEq, Serialize)]
pub struct ChatInputAudio {
    pub data: String,
    pub format: String,
    #[serde(flatten)]
    pub extra: BTreeMap<String, Value>,
}

#[derive(Clone, Debug, Deserialize, PartialEq, Serialize)]
pub struct ChatToolCall {
    pub id: String,
    #[serde(rename = "type")]
    pub kind: String,
    pub function: ChatFunctionCall,
    #[serde(flatten)]
    pub extra: BTreeMap<String, Value>,
}

#[derive(Clone, Debug, Deserialize, PartialEq, Serialize)]
pub struct ChatFunctionCall {
    pub name: String,
    pub arguments: String,
    #[serde(flatten)]
    pub extra: BTreeMap<String, Value>,
}

#[derive(Clone, Debug, Deserialize, PartialEq, Serialize)]
pub struct ChatTool {
    #[serde(rename = "type")]
    pub kind: String,
    pub function: ChatFunctionDefinition,
    #[serde(flatten)]
    pub extra: BTreeMap<String, Value>,
}

#[derive(Clone, Debug, Deserialize, PartialEq, Serialize)]
pub struct ChatFunctionDefinition {
    pub name: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub description: Option<String>,
    #[serde(default = "empty_object")]
    pub parameters: Value,
    #[serde(flatten)]
    pub extra: BTreeMap<String, Value>,
}

fn empty_object() -> Value {
    Value::Object(Default::default())
}

#[derive(Clone, Debug, Deserialize, PartialEq, Serialize)]
#[serde(untagged)]
pub enum ChatToolChoice {
    Mode(String),
    Named(ChatNamedToolChoice),
}

#[derive(Clone, Debug, Deserialize, PartialEq, Serialize)]
pub struct ChatNamedToolChoice {
    #[serde(rename = "type")]
    pub kind: String,
    pub function: ChatNamedFunction,
    #[serde(flatten)]
    pub extra: BTreeMap<String, Value>,
}

#[derive(Clone, Debug, Deserialize, PartialEq, Serialize)]
pub struct ChatNamedFunction {
    pub name: String,
    #[serde(flatten)]
    pub extra: BTreeMap<String, Value>,
}

#[derive(Clone, Debug, Deserialize, PartialEq, Serialize)]
pub struct ChatResponseFormat {
    #[serde(rename = "type")]
    pub kind: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub json_schema: Option<ChatJsonSchema>,
    #[serde(flatten)]
    pub extra: BTreeMap<String, Value>,
}

#[derive(Clone, Debug, Deserialize, PartialEq, Serialize)]
pub struct ChatJsonSchema {
    pub name: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub description: Option<String>,
    pub schema: Value,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub strict: Option<bool>,
    #[serde(flatten)]
    pub extra: BTreeMap<String, Value>,
}

#[derive(Clone, Debug, Deserialize, PartialEq, Serialize)]
#[serde(untagged)]
pub enum StopSequences {
    One(String),
    Many(Vec<String>),
}

impl StopSequences {
    fn into_vec(self) -> Vec<String> {
        match self {
            Self::One(value) => vec![value],
            Self::Many(values) => values,
        }
    }

    fn from_vec(values: &[String]) -> Option<Self> {
        match values {
            [] => None,
            [value] => Some(Self::One(value.clone())),
            values => Some(Self::Many(values.to_vec())),
        }
    }
}

pub fn decode_chat_completion(
    request: ChatCompletionRequest,
) -> Result<Operation, OpenAiDecodeError> {
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
        return Err(OpenAiDecodeError::EmptyMessages);
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

fn validate_request_parameters(request: &ChatCompletionRequest) -> Result<(), OpenAiDecodeError> {
    if request.max_completion_tokens.is_some() && request.max_tokens.is_some() {
        return Err(OpenAiDecodeError::ConflictingTokenLimits);
    }
    if request.n == Some(0) {
        return Err(OpenAiDecodeError::InvalidParameter {
            field: "n",
            reason: "must be greater than zero",
        });
    }
    if request
        .temperature
        .is_some_and(|value| !(0.0..=2.0).contains(&value))
    {
        return Err(OpenAiDecodeError::InvalidParameter {
            field: "temperature",
            reason: "must be between 0 and 2",
        });
    }
    if request
        .top_p
        .is_some_and(|value| !(0.0..=1.0).contains(&value))
    {
        return Err(OpenAiDecodeError::InvalidParameter {
            field: "top_p",
            reason: "must be between 0 and 1",
        });
    }
    Ok(())
}

fn decode_message(
    index: usize,
    message: ChatMessage,
    extensions: &mut BTreeMap<String, Value>,
) -> Result<Message, OpenAiDecodeError> {
    let prefix = format!("/messages/{index}");
    collect_extra(&prefix, &message.extra, extensions);

    let role = match message.role {
        ChatRole::System => MessageRole::System,
        ChatRole::Developer => MessageRole::Developer,
        ChatRole::User => MessageRole::User,
        ChatRole::Assistant => MessageRole::Assistant,
        ChatRole::Tool => MessageRole::Tool,
    };
    if role == MessageRole::Tool && message.tool_call_id.is_none() {
        return Err(OpenAiDecodeError::MissingToolCallId {
            message_index: index,
        });
    }

    let content = match message.content {
        Some(ChatMessageContent::Text(text)) => vec![ContentPart::Text { text }],
        Some(ChatMessageContent::Parts(parts)) => parts
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
                return Err(OpenAiDecodeError::UnsupportedToolType(call.kind));
            }
            Ok(ToolCall {
                id: call.id,
                name: call.function.name,
                arguments: call.function.arguments,
            })
        })
        .collect::<Result<Vec<_>, _>>()?;

    if content.is_empty() && tool_calls.is_empty() && role != MessageRole::Assistant {
        return Err(OpenAiDecodeError::EmptyMessage {
            message_index: index,
        });
    }

    Ok(Message {
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
    part: ChatContentPart,
    extensions: &mut BTreeMap<String, Value>,
) -> Result<ContentPart, OpenAiDecodeError> {
    let prefix = format!("/messages/{message_index}/content/{part_index}");
    collect_extra(&prefix, part.extra(), extensions);
    match part {
        ChatContentPart::Text { text, .. } => Ok(ContentPart::Text { text }),
        ChatContentPart::ImageUrl { image_url, .. } => {
            collect_extra(&format!("{prefix}/image_url"), &image_url.extra, extensions);
            Ok(ContentPart::Image {
                source: MediaSource::Uri(image_url.url),
                detail: image_url.detail,
            })
        }
        ChatContentPart::InputAudio { input_audio, .. } => {
            collect_extra(
                &format!("{prefix}/input_audio"),
                &input_audio.extra,
                extensions,
            );
            let media = media_handle_from_inline_marker(&input_audio.data)
                .ok_or(OpenAiDecodeError::InlineMediaRequiresBoundedHandle)?;
            Ok(ContentPart::InputAudio {
                media,
                format: input_audio.format,
            })
        }
        ChatContentPart::Refusal { refusal, .. } => Ok(ContentPart::Refusal { text: refusal }),
    }
}

fn decode_tool(
    index: usize,
    tool: ChatTool,
    extensions: &mut BTreeMap<String, Value>,
) -> Result<ToolDefinition, OpenAiDecodeError> {
    if tool.kind != "function" {
        return Err(OpenAiDecodeError::UnsupportedToolType(tool.kind));
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
    choice: ChatToolChoice,
    extensions: &mut BTreeMap<String, Value>,
) -> Result<ToolChoice, OpenAiDecodeError> {
    match choice {
        ChatToolChoice::Mode(mode) => match mode.as_str() {
            "auto" => Ok(ToolChoice::Auto),
            "none" => Ok(ToolChoice::None),
            "required" => Ok(ToolChoice::Required),
            _ => Err(OpenAiDecodeError::UnsupportedToolChoice(mode)),
        },
        ChatToolChoice::Named(named) => {
            if named.kind != "function" {
                return Err(OpenAiDecodeError::UnsupportedToolType(named.kind));
            }
            collect_extra("/tool_choice", &named.extra, extensions);
            collect_extra("/tool_choice/function", &named.function.extra, extensions);
            Ok(ToolChoice::Named(named.function.name))
        }
    }
}

fn decode_response_format(
    format: ChatResponseFormat,
    extensions: &mut BTreeMap<String, Value>,
) -> Result<ResponseFormat, OpenAiDecodeError> {
    collect_extra("/response_format", &format.extra, extensions);
    match format.kind.as_str() {
        "text" => Ok(ResponseFormat::Text),
        "json_object" => Ok(ResponseFormat::JsonObject),
        "json_schema" => {
            let schema = format
                .json_schema
                .ok_or(OpenAiDecodeError::MissingJsonSchema)?;
            collect_extra("/response_format/json_schema", &schema.extra, extensions);
            Ok(ResponseFormat::JsonSchema {
                name: schema.name,
                description: schema.description,
                schema: schema.schema,
                strict: schema.strict,
            })
        }
        kind => Err(OpenAiDecodeError::UnsupportedResponseFormat(
            kind.to_owned(),
        )),
    }
}

fn collect_extra(
    prefix: &str,
    extra: &BTreeMap<String, Value>,
    extensions: &mut BTreeMap<String, Value>,
) {
    for (key, value) in extra {
        extensions.insert(
            format!("{prefix}/{}", escape_json_pointer(key)),
            value.clone(),
        );
    }
}

fn escape_json_pointer(value: &str) -> String {
    value.replace('~', "~0").replace('/', "~1")
}

fn unescape_json_pointer(value: &str) -> String {
    value.replace("~1", "/").replace("~0", "~")
}

pub fn encode_chat_completion(
    request: &GenerationRequest,
    upstream_model: &str,
) -> Result<ChatCompletionRequest, OpenAiEncodeError> {
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
        .map(|tool| ChatTool {
            kind: "function".into(),
            function: ChatFunctionDefinition {
                name: tool.name.clone(),
                description: tool.description.clone(),
                parameters: tool.input_schema.clone(),
                extra: BTreeMap::new(),
            },
            extra: BTreeMap::new(),
        })
        .collect();
    let tool_choice = request.tool_choice.as_ref().map(|choice| match choice {
        ToolChoice::Auto => ChatToolChoice::Mode("auto".into()),
        ToolChoice::None => ChatToolChoice::Mode("none".into()),
        ToolChoice::Required => ChatToolChoice::Mode("required".into()),
        ToolChoice::Named(name) => ChatToolChoice::Named(ChatNamedToolChoice {
            kind: "function".into(),
            function: ChatNamedFunction {
                name: name.clone(),
                extra: BTreeMap::new(),
            },
            extra: BTreeMap::new(),
        }),
    });
    let response_format = request.response_format.as_ref().map(|format| match format {
        ResponseFormat::Text => ChatResponseFormat {
            kind: "text".into(),
            json_schema: None,
            extra: BTreeMap::new(),
        },
        ResponseFormat::JsonObject => ChatResponseFormat {
            kind: "json_object".into(),
            json_schema: None,
            extra: BTreeMap::new(),
        },
        ResponseFormat::JsonSchema {
            name,
            description,
            schema,
            strict,
        } => ChatResponseFormat {
            kind: "json_schema".into(),
            json_schema: Some(ChatJsonSchema {
                name: name.clone(),
                description: description.clone(),
                schema: schema.clone(),
                strict: *strict,
                extra: BTreeMap::new(),
            }),
            extra: BTreeMap::new(),
        },
    });

    let mut encoded = ChatCompletionRequest {
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

fn encode_message(
    message: &Message,
    force_content_parts: bool,
) -> Result<ChatMessage, OpenAiEncodeError> {
    let role = match message.role {
        MessageRole::System => ChatRole::System,
        MessageRole::Developer => ChatRole::Developer,
        MessageRole::User => ChatRole::User,
        MessageRole::Assistant => ChatRole::Assistant,
        MessageRole::Tool => ChatRole::Tool,
    };
    let mut parts = Vec::with_capacity(message.content.len());
    for part in &message.content {
        parts.push(match part {
            ContentPart::Text { text } => ChatContentPart::Text {
                text: text.clone(),
                extra: BTreeMap::new(),
            },
            ContentPart::Image { source, detail } => {
                let MediaSource::Uri(url) = source else {
                    return Err(OpenAiEncodeError::MediaHandleCannotBeEncoded);
                };
                ChatContentPart::ImageUrl {
                    image_url: ChatImageUrl {
                        url: url.clone(),
                        detail: detail.clone(),
                        extra: BTreeMap::new(),
                    },
                    extra: BTreeMap::new(),
                }
            }
            ContentPart::Refusal { text } => ChatContentPart::Refusal {
                refusal: text.clone(),
                extra: BTreeMap::new(),
            },
            ContentPart::InputAudio { media, format } => {
                if !matches!(format.as_str(), "wav" | "mp3") {
                    return Err(OpenAiEncodeError::InvalidInputAudioFormat);
                }
                ChatContentPart::InputAudio {
                    input_audio: ChatInputAudio {
                        data: inline_media_marker(media),
                        format: format.clone(),
                        extra: BTreeMap::new(),
                    },
                    extra: BTreeMap::new(),
                }
            }
            ContentPart::InputFile { .. } => return Err(OpenAiEncodeError::InputFileUnsupported),
        });
    }
    let content = match parts.as_slice() {
        [] => None,
        [ChatContentPart::Text { text, extra }] if extra.is_empty() && !force_content_parts => {
            Some(ChatMessageContent::Text(text.clone()))
        }
        _ => Some(ChatMessageContent::Parts(parts)),
    };
    Ok(ChatMessage {
        role,
        content,
        name: message.name.clone(),
        tool_call_id: message.tool_call_id.clone(),
        tool_calls: message
            .tool_calls
            .iter()
            .map(|call| ChatToolCall {
                id: call.id.clone(),
                kind: "function".into(),
                function: ChatFunctionCall {
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
    request: &mut ChatCompletionRequest,
    extensions: &BTreeMap<String, Value>,
) -> Result<(), OpenAiEncodeError> {
    for (pointer, value) in extensions {
        let segments = pointer
            .strip_prefix('/')
            .ok_or_else(|| OpenAiEncodeError::InvalidExtensionPath(pointer.clone()))?
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
                let ChatContentPart::ImageUrl { image_url, .. } = part else {
                    return Err(OpenAiEncodeError::InvalidExtensionPath(pointer.clone()));
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
                let Some(ChatToolChoice::Named(choice)) = &mut request.tool_choice else {
                    return Err(OpenAiEncodeError::InvalidExtensionPath(pointer.clone()));
                };
                choice.extra.insert(field.clone(), value.clone());
            }
            [choice, function, field] if choice == "tool_choice" && function == "function" => {
                let Some(ChatToolChoice::Named(choice)) = &mut request.tool_choice else {
                    return Err(OpenAiEncodeError::InvalidExtensionPath(pointer.clone()));
                };
                choice.function.extra.insert(field.clone(), value.clone());
            }
            [format, field] if format == "response_format" => {
                let Some(format) = &mut request.response_format else {
                    return Err(OpenAiEncodeError::InvalidExtensionPath(pointer.clone()));
                };
                format.extra.insert(field.clone(), value.clone());
            }
            [format, schema, field] if format == "response_format" && schema == "json_schema" => {
                let Some(ChatResponseFormat {
                    json_schema: Some(schema),
                    ..
                }) = &mut request.response_format
                else {
                    return Err(OpenAiEncodeError::InvalidExtensionPath(pointer.clone()));
                };
                schema.extra.insert(field.clone(), value.clone());
            }
            _ => return Err(OpenAiEncodeError::InvalidExtensionPath(pointer.clone())),
        }
    }
    Ok(())
}

fn message_mut<'a>(
    request: &'a mut ChatCompletionRequest,
    index: &str,
    pointer: &str,
) -> Result<&'a mut ChatMessage, OpenAiEncodeError> {
    indexed_mut(&mut request.messages, index, pointer)
}

fn content_part_mut<'a>(
    request: &'a mut ChatCompletionRequest,
    message_index: &str,
    part_index: &str,
    pointer: &str,
) -> Result<&'a mut ChatContentPart, OpenAiEncodeError> {
    let message = message_mut(request, message_index, pointer)?;
    let Some(ChatMessageContent::Parts(parts)) = &mut message.content else {
        return Err(OpenAiEncodeError::InvalidExtensionPath(pointer.to_owned()));
    };
    indexed_mut(parts, part_index, pointer)
}

fn tool_call_mut<'a>(
    request: &'a mut ChatCompletionRequest,
    message_index: &str,
    tool_index: &str,
    pointer: &str,
) -> Result<&'a mut ChatToolCall, OpenAiEncodeError> {
    let message = message_mut(request, message_index, pointer)?;
    indexed_mut(&mut message.tool_calls, tool_index, pointer)
}

fn indexed_mut<'a, T>(
    values: &'a mut [T],
    index: &str,
    pointer: &str,
) -> Result<&'a mut T, OpenAiEncodeError> {
    index
        .parse::<usize>()
        .ok()
        .and_then(|index| values.get_mut(index))
        .ok_or_else(|| OpenAiEncodeError::InvalidExtensionPath(pointer.to_owned()))
}

#[derive(Debug, Error)]
pub enum OpenAiDecodeError {
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

#[derive(Debug, Error)]
pub enum OpenAiEncodeError {
    #[error(transparent)]
    Extensions(#[from] olp_domain::ExtensionError),
    #[error("a media handle cannot be encoded as an OpenAI image URL")]
    MediaHandleCannotBeEncoded,
    #[error("canonical input file is not supported by Chat Completions")]
    InputFileUnsupported,
    #[error("OpenAI input_audio supports only wav or mp3")]
    InvalidInputAudioFormat,
    #[error("source extension path cannot be applied: {0}")]
    InvalidExtensionPath(String),
}
