use std::collections::{BTreeMap, BTreeSet};

use olp_domain::{
    CanonicalError, CanonicalEvent, CanonicalEventKind, ContentPart, ErrorClass, FinishReason,
    GenerationParameters, GenerationRequest, MediaSource, Message, MessageRole, Operation,
    ResponseFormat, RouteSlug, RouteSlugError, SourceExtensions, Surface, TokenCountRequest,
    TokenCountResult, ToolCall, ToolChoice, ToolDefinition, Usage, inline_media_marker,
    media_handle_from_inline_marker,
};
use serde::{Deserialize, Serialize};
use serde_json::{Map, Value};
use thiserror::Error;

use crate::sse::{DEFAULT_MAX_EVENT_BYTES, SseDecodeError, SseDecoder, SseFrame};

use super::extensions::apply_pointer_extensions;
use super::extensions::{collect_extra, escape_json_pointer};

pub(crate) const OPENAI_RESPONSES_RAW_OUTPUT_PREFIX: &str = "/__olp/openai_responses_raw_output";

#[derive(Clone, Deserialize, PartialEq, Serialize)]
pub struct ResponseCreateRequest {
    pub model: String,
    pub input: ResponseInput,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub instructions: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub max_output_tokens: Option<u32>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub temperature: Option<f32>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub top_p: Option<f32>,
    #[serde(default, skip_serializing_if = "is_false")]
    pub stream: bool,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub tools: Vec<ResponseTool>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub tool_choice: Option<ResponseToolChoice>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub parallel_tool_calls: Option<bool>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub text: Option<ResponseTextConfig>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub background: Option<bool>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub previous_response_id: Option<String>,
    #[serde(flatten)]
    pub extra: BTreeMap<String, Value>,
}

const fn is_false(value: &bool) -> bool {
    !*value
}

#[derive(Clone, Deserialize, PartialEq, Serialize)]
#[serde(untagged)]
pub enum ResponseInput {
    Text(String),
    Items(Vec<Value>),
}

#[derive(Clone, Deserialize, PartialEq, Serialize)]
pub struct ResponseTool {
    #[serde(rename = "type")]
    pub kind: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub name: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub description: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub parameters: Option<Value>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub strict: Option<bool>,
    #[serde(flatten)]
    pub extra: BTreeMap<String, Value>,
}

#[derive(Clone, Deserialize, PartialEq, Serialize)]
#[serde(untagged)]
pub enum ResponseToolChoice {
    Mode(String),
    Named(ResponseNamedToolChoice),
}

#[derive(Clone, Deserialize, PartialEq, Serialize)]
pub struct ResponseNamedToolChoice {
    #[serde(rename = "type")]
    pub kind: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub name: Option<String>,
    #[serde(flatten)]
    pub extra: BTreeMap<String, Value>,
}

#[derive(Clone, Deserialize, PartialEq, Serialize)]
pub struct ResponseTextConfig {
    pub format: ResponseTextFormat,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub verbosity: Option<String>,
    #[serde(flatten)]
    pub extra: BTreeMap<String, Value>,
}

#[derive(Clone, Deserialize, PartialEq, Serialize)]
pub struct ResponseTextFormat {
    #[serde(rename = "type")]
    pub kind: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub name: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub description: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub schema: Option<Value>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub strict: Option<bool>,
    #[serde(flatten)]
    pub extra: BTreeMap<String, Value>,
}

pub fn decode_response_create(
    mut request: ResponseCreateRequest,
) -> Result<Operation, ResponsesCodecError> {
    if request.background == Some(true) {
        return Err(ResponsesCodecError::BackgroundUnsupported);
    }
    if let Some(value) = request.extra.remove("conversation") {
        return Err(ResponsesCodecError::StatefulField {
            field: "conversation",
            value: value.to_string(),
        });
    }
    validate_sampling(request.temperature, request.top_p)?;
    let route = RouteSlug::parse(request.model)?;
    let mut extensions = BTreeMap::new();
    collect_extra("", &request.extra, &mut extensions);
    if let Some(value) = request.background {
        extensions.insert("/background".into(), Value::Bool(value));
    }
    if let Some(value) = request.previous_response_id {
        // OLP intentionally does not implement upstream state. Keeping this as
        // an extension would make a retry/failover target-dependent, so reject.
        return Err(ResponsesCodecError::StatefulField {
            field: "previous_response_id",
            value,
        });
    }

    let mut messages = Vec::new();
    if let Some(instructions) = request.instructions {
        messages.push(Message {
            role: MessageRole::Developer,
            content: vec![ContentPart::Text { text: instructions }],
            name: None,
            tool_call_id: None,
            tool_calls: Vec::new(),
        });
    }
    messages.extend(decode_response_input(request.input, &mut extensions)?);
    if messages.is_empty() {
        return Err(ResponsesCodecError::EmptyInput);
    }

    let mut tools = Vec::new();
    for (index, tool) in request.tools.into_iter().enumerate() {
        if tool.kind != "function" {
            extensions.insert(format!("/tools/{index}"), serde_json::to_value(tool)?);
            continue;
        }
        let name = tool
            .name
            .ok_or(ResponsesCodecError::MissingToolField("name"))?;
        let parameters = tool
            .parameters
            .ok_or(ResponsesCodecError::MissingToolField("parameters"))?;
        collect_extra(&format!("/tools/{index}"), &tool.extra, &mut extensions);
        if let Some(strict) = tool.strict {
            extensions.insert(format!("/tools/{index}/strict"), Value::Bool(strict));
        }
        tools.push(ToolDefinition {
            name,
            description: tool.description,
            input_schema: parameters,
        });
    }
    let tool_choice = request
        .tool_choice
        .map(|choice| decode_response_tool_choice(choice, &mut extensions))
        .transpose()?
        .flatten();
    let response_format = request
        .text
        .map(|text| decode_response_text_config(text, &mut extensions))
        .transpose()?;

    Ok(Operation::Generation(GenerationRequest {
        route,
        messages,
        parameters: GenerationParameters {
            max_output_tokens: request.max_output_tokens,
            temperature: request.temperature,
            top_p: request.top_p,
            stop_sequences: Vec::new(),
            candidate_count: None,
            seed: None,
            parallel_tool_calls: request.parallel_tool_calls,
            stream: request.stream,
        },
        tools,
        tool_choice,
        response_format,
        extensions: SourceExtensions::new(Surface::OpenAi, extensions),
    }))
}

pub fn encode_response_create(
    request: &GenerationRequest,
    provider_model: &str,
) -> Result<ResponseCreateRequest, ResponsesCodecError> {
    request
        .extensions
        .ensure_representable_on(Surface::OpenAi)?;
    if !request.parameters.stop_sequences.is_empty() {
        return Err(ResponsesCodecError::UnrepresentableCanonicalField(
            "stop_sequences",
        ));
    }
    if request.parameters.candidate_count.is_some() {
        return Err(ResponsesCodecError::UnrepresentableCanonicalField(
            "candidate_count",
        ));
    }
    if request.parameters.seed.is_some() {
        return Err(ResponsesCodecError::UnrepresentableCanonicalField("seed"));
    }

    let (instructions, messages) = match request.messages.split_first() {
        Some((
            Message {
                role: MessageRole::Developer,
                content,
                name: None,
                tool_call_id: None,
                tool_calls,
            },
            rest,
        )) if tool_calls.is_empty() => match content.as_slice() {
            [ContentPart::Text { text }] => (Some(text.clone()), rest),
            _ => (None, request.messages.as_slice()),
        },
        _ => (None, request.messages.as_slice()),
    };
    let mut items = Vec::new();
    for message in messages {
        encode_response_message(message, &mut items)?;
    }
    if items.is_empty() {
        return Err(ResponsesCodecError::EmptyInput);
    }
    let mut tools = request
        .tools
        .iter()
        .map(|tool| ResponseTool {
            kind: "function".into(),
            name: Some(tool.name.clone()),
            description: tool.description.clone(),
            parameters: Some(tool.input_schema.clone()),
            strict: None,
            extra: BTreeMap::new(),
        })
        .collect();
    let mut tool_choice = request.tool_choice.as_ref().map(|choice| match choice {
        ToolChoice::Auto => ResponseToolChoice::Mode("auto".into()),
        ToolChoice::None => ResponseToolChoice::Mode("none".into()),
        ToolChoice::Required => ResponseToolChoice::Mode("required".into()),
        ToolChoice::Named(name) => ResponseToolChoice::Named(ResponseNamedToolChoice {
            kind: "function".into(),
            name: Some(name.clone()),
            extra: BTreeMap::new(),
        }),
    });
    let text = request
        .response_format
        .as_ref()
        .map(|format| ResponseTextConfig {
            format: match format {
                ResponseFormat::Text => ResponseTextFormat {
                    kind: "text".into(),
                    name: None,
                    description: None,
                    schema: None,
                    strict: None,
                    extra: BTreeMap::new(),
                },
                ResponseFormat::JsonObject => ResponseTextFormat {
                    kind: "json_object".into(),
                    name: None,
                    description: None,
                    schema: None,
                    strict: None,
                    extra: BTreeMap::new(),
                },
                ResponseFormat::JsonSchema {
                    name,
                    description,
                    schema,
                    strict,
                } => ResponseTextFormat {
                    kind: "json_schema".into(),
                    name: Some(name.clone()),
                    description: description.clone(),
                    schema: Some(schema.clone()),
                    strict: *strict,
                    extra: BTreeMap::new(),
                },
            },
            verbosity: None,
            extra: BTreeMap::new(),
        });
    let mut extension_values = request.extensions.values.clone();
    restore_raw_response_tools(&mut tools, &mut tool_choice, &mut extension_values)?;
    apply_pointer_extensions(
        ResponseCreateRequest {
            model: provider_model.into(),
            input: ResponseInput::Items(items),
            instructions,
            max_output_tokens: request.parameters.max_output_tokens,
            temperature: request.parameters.temperature,
            top_p: request.parameters.top_p,
            stream: request.parameters.stream,
            tools,
            tool_choice,
            parallel_tool_calls: request.parameters.parallel_tool_calls,
            text,
            background: None,
            previous_response_id: None,
            extra: BTreeMap::new(),
        },
        &extension_values,
    )
    .map_err(ResponsesCodecError::InvalidExtension)
}

fn restore_raw_response_tools(
    tools: &mut Vec<ResponseTool>,
    tool_choice: &mut Option<ResponseToolChoice>,
    extensions: &mut BTreeMap<String, Value>,
) -> Result<(), ResponsesCodecError> {
    let mut raw_tools = extensions
        .keys()
        .filter_map(|path| {
            path.strip_prefix("/tools/")
                .filter(|suffix| !suffix.contains('/'))
                .and_then(|index| index.parse::<usize>().ok())
                .map(|index| (index, path.clone()))
        })
        .collect::<Vec<_>>();
    raw_tools.sort_by_key(|(index, _)| *index);
    for (index, path) in raw_tools {
        if index > tools.len() {
            return Err(ResponsesCodecError::InvalidExtension(path));
        }
        let value = extensions
            .remove(&path)
            .ok_or_else(|| ResponsesCodecError::InvalidExtension(path.clone()))?;
        let tool = serde_json::from_value(value)?;
        tools.insert(index, tool);
    }
    if let Some(value) = extensions.remove("/tool_choice") {
        *tool_choice = Some(serde_json::from_value(value)?);
    }
    Ok(())
}

fn encode_response_message(
    message: &Message,
    items: &mut Vec<Value>,
) -> Result<(), ResponsesCodecError> {
    if message.role == MessageRole::Tool {
        let call_id = message
            .tool_call_id
            .as_ref()
            .ok_or(ResponsesCodecError::MissingCanonicalToolCallId)?;
        let [ContentPart::Text { text }] = message.content.as_slice() else {
            return Err(ResponsesCodecError::UnrepresentableCanonicalField(
                "tool output content",
            ));
        };
        items.push(serde_json::json!({
            "type": "function_call_output",
            "call_id": call_id,
            "output": text,
        }));
        return Ok(());
    }

    if !message.content.is_empty() {
        let role = match message.role {
            MessageRole::System => "system",
            MessageRole::Developer => "developer",
            MessageRole::User => "user",
            MessageRole::Assistant => "assistant",
            MessageRole::Tool => unreachable!("handled above"),
        };
        let content = message
            .content
            .iter()
            .map(encode_response_content_part)
            .collect::<Result<Vec<_>, _>>()?;
        items.push(serde_json::json!({
            "type": "message",
            "role": role,
            "content": content,
        }));
    }
    if !message.tool_calls.is_empty() && message.role != MessageRole::Assistant {
        return Err(ResponsesCodecError::UnrepresentableCanonicalField(
            "non-assistant tool_calls",
        ));
    }
    for call in &message.tool_calls {
        items.push(serde_json::json!({
            "type": "function_call",
            "call_id": call.id,
            "name": call.name,
            "arguments": call.arguments,
        }));
    }
    if message.content.is_empty() && message.tool_calls.is_empty() {
        return Err(ResponsesCodecError::UnrepresentableCanonicalField(
            "empty message",
        ));
    }
    Ok(())
}

fn encode_response_content_part(part: &ContentPart) -> Result<Value, ResponsesCodecError> {
    match part {
        ContentPart::Text { text } => Ok(serde_json::json!({"type": "input_text", "text": text})),
        ContentPart::Refusal { text } => {
            Ok(serde_json::json!({"type": "refusal", "refusal": text}))
        }
        ContentPart::Image {
            source: MediaSource::Uri(url),
            detail,
        } => Ok(serde_json::json!({
            "type": "input_image",
            "image_url": url,
            "detail": detail,
        })),
        ContentPart::Image {
            source: MediaSource::Handle(_),
            ..
        } => Err(ResponsesCodecError::UnrepresentableCanonicalField(
            "unresolved media handle",
        )),
        ContentPart::InputAudio { media, format } => Ok(serde_json::json!({
            "type": "input_audio",
            "input_audio": {
                "data": inline_media_marker(media),
                "format": format,
            }
        })),
        ContentPart::InputFile {
            media,
            mime_type,
            filename,
        } => {
            if mime_type != "application/pdf" {
                return Err(ResponsesCodecError::UnrepresentableCanonicalField(
                    "input_file MIME type",
                ));
            }
            Ok(serde_json::json!({
                "type": "input_file",
                "file_data": inline_media_marker(media),
                "filename": filename,
            }))
        }
    }
}

#[derive(Clone, Deserialize, PartialEq, Serialize)]
pub struct ResponseInputTokensRequest {
    pub model: String,
    pub input: ResponseInput,
    #[serde(flatten)]
    pub extra: BTreeMap<String, Value>,
}

/// Exact, source-scoped request retained for same-protocol forwarding. The
/// canonical token-count operation intentionally carries only countable media
/// and text parts; it cannot faithfully represent Responses roles, function
/// calls/results, or future stateless input item fields on its own.
pub const OPENAI_RESPONSES_INPUT_TOKENS_REQUEST_EXTENSION: &str =
    "/__olp/openai_responses_input_tokens_request";

pub fn decode_response_input_tokens(
    request: ResponseInputTokensRequest,
) -> Result<Operation, ResponsesCodecError> {
    let plain_text = request.extra.is_empty() && matches!(&request.input, ResponseInput::Text(_));
    let preserved = serde_json::to_value(&request)?;
    let route = RouteSlug::parse(request.model)?;
    reject_stateful_token_count_fields(&request.extra)?;

    // Reuse the complete Responses input validator so the count endpoint and
    // response creation cannot disagree about supported stateless item forms.
    // Granular unknown fields are deliberately discarded here because the
    // exact body below is the sole lossless source-scoped representation.
    let mut validation_extensions = BTreeMap::new();
    let messages = decode_response_input(request.input, &mut validation_extensions)?;
    let mut input = Vec::new();
    let mut has_tool_semantics = false;
    for message in messages {
        input.extend(message.content);
        for call in message.tool_calls {
            has_tool_semantics = true;
            // Include arguments in the conservative local TPM estimate while
            // the preserved body remains authoritative for upstream counting.
            input.push(ContentPart::Text {
                text: call.arguments,
            });
        }
        has_tool_semantics |= message.tool_call_id.is_some();
    }
    if input.is_empty() && !has_tool_semantics {
        return Err(ResponsesCodecError::EmptyInput);
    }
    let extensions = if plain_text && validation_extensions.is_empty() && !has_tool_semantics {
        SourceExtensions::new(Surface::OpenAi, BTreeMap::new())
    } else {
        SourceExtensions::new(
            Surface::OpenAi,
            BTreeMap::from([(
                OPENAI_RESPONSES_INPUT_TOKENS_REQUEST_EXTENSION.to_owned(),
                preserved,
            )]),
        )
    };
    Ok(Operation::TokenCount(TokenCountRequest {
        route,
        input,
        extensions,
    }))
}

pub fn encode_response_input_tokens(
    request: &TokenCountRequest,
    provider_model: &str,
) -> Result<ResponseInputTokensRequest, ResponsesCodecError> {
    request
        .extensions
        .ensure_representable_on(Surface::OpenAi)?;
    if let Some(preserved) = request
        .extensions
        .values
        .get(OPENAI_RESPONSES_INPUT_TOKENS_REQUEST_EXTENSION)
    {
        if request.extensions.values.len() != 1 {
            return Err(ResponsesCodecError::InvalidExtension(
                "Responses input-token preservation collided with another extension".into(),
            ));
        }
        let mut wire: ResponseInputTokensRequest = serde_json::from_value(preserved.clone())?;
        wire.model = provider_model.to_owned();
        return Ok(wire);
    }
    let parts = request
        .input
        .iter()
        .map(encode_response_content_part)
        .collect::<Result<Vec<_>, _>>()?;
    apply_pointer_extensions(
        ResponseInputTokensRequest {
            model: provider_model.into(),
            input: ResponseInput::Items(vec![serde_json::json!({
                "type": "message",
                "role": "user",
                "content": parts,
            })]),
            extra: BTreeMap::new(),
        },
        &request.extensions.values,
    )
    .map_err(ResponsesCodecError::InvalidExtension)
}

fn reject_stateful_token_count_fields(
    extra: &BTreeMap<String, Value>,
) -> Result<(), ResponsesCodecError> {
    for field in ["previous_response_id", "conversation"] {
        if let Some(value) = extra.get(field) {
            return Err(ResponsesCodecError::StatefulField {
                field,
                value: value.to_string(),
            });
        }
    }
    if extra.get("background") == Some(&Value::Bool(true)) {
        return Err(ResponsesCodecError::BackgroundUnsupported);
    }
    Ok(())
}

#[derive(Clone, Deserialize, PartialEq, Serialize)]
pub struct ResponseInputTokensResponse {
    pub input_tokens: u64,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub object: Option<String>,
    #[serde(flatten)]
    pub extra: BTreeMap<String, Value>,
}

pub fn decode_response_input_tokens_result(
    response: ResponseInputTokensResponse,
) -> TokenCountResult {
    let mut extensions = BTreeMap::new();
    collect_extra("", &response.extra, &mut extensions);
    if let Some(object) = response.object {
        extensions.insert("/object".into(), Value::String(object));
    }
    TokenCountResult {
        input_tokens: response.input_tokens,
        extensions: SourceExtensions::new(Surface::OpenAi, extensions),
    }
}

pub fn encode_response_input_tokens_result(
    result: &TokenCountResult,
) -> Result<ResponseInputTokensResponse, ResponsesCodecError> {
    result.extensions.ensure_representable_on(Surface::OpenAi)?;
    let mut extensions = result.extensions.values.clone();
    let object = extensions
        .remove("/object")
        .and_then(|value| value.as_str().map(str::to_owned))
        .or_else(|| Some("response.input_tokens".into()));
    apply_pointer_extensions(
        ResponseInputTokensResponse {
            input_tokens: result.input_tokens,
            object,
            extra: BTreeMap::new(),
        },
        &extensions,
    )
    .map_err(ResponsesCodecError::InvalidExtension)
}

#[derive(Clone, Deserialize, PartialEq, Serialize)]
pub struct ResponseObject {
    pub id: String,
    pub object: String,
    pub created_at: i64,
    pub status: String,
    pub model: String,
    #[serde(default)]
    pub output: Vec<Value>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub usage: Option<ResponseUsage>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub error: Option<ResponseErrorBody>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub incomplete_details: Option<Value>,
    #[serde(flatten)]
    pub extra: BTreeMap<String, Value>,
}

#[derive(Clone, Deserialize, PartialEq, Serialize)]
pub struct ResponseUsage {
    pub input_tokens: u64,
    pub output_tokens: u64,
    pub total_tokens: u64,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub input_tokens_details: Option<ResponseInputTokenDetails>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub output_tokens_details: Option<ResponseOutputTokenDetails>,
    #[serde(flatten)]
    pub extra: BTreeMap<String, Value>,
}

#[derive(Clone, Deserialize, PartialEq, Serialize)]
pub struct ResponseInputTokenDetails {
    #[serde(default)]
    pub cached_tokens: u64,
    #[serde(flatten)]
    pub extra: BTreeMap<String, Value>,
}

#[derive(Clone, Deserialize, PartialEq, Serialize)]
pub struct ResponseOutputTokenDetails {
    #[serde(default)]
    pub reasoning_tokens: u64,
    #[serde(flatten)]
    pub extra: BTreeMap<String, Value>,
}

#[derive(Clone, Deserialize, PartialEq, Serialize)]
pub struct ResponseErrorBody {
    pub code: String,
    pub message: String,
    #[serde(flatten)]
    pub extra: BTreeMap<String, Value>,
}

pub fn decode_response_object(
    response: ResponseObject,
) -> Result<Vec<CanonicalEvent>, ResponsesCodecError> {
    if response.object != "response" {
        return Err(ResponsesCodecError::InvalidResponse(response.object));
    }
    let mut builder = ResponsesEventBuilder::default();
    builder.push(CanonicalEventKind::ResponseStart {
        response_id: Some(response.id),
        provider_model: Some(response.model),
    });
    let mut extensions = BTreeMap::new();
    collect_extra("", &response.extra, &mut extensions);
    extensions.insert("/created_at".into(), Value::from(response.created_at));
    extensions.insert("/status".into(), Value::String(response.status.clone()));
    if let Some(details) = response.incomplete_details {
        extensions.insert("/incomplete_details".into(), details);
    }

    for (output_index, item) in response.output.into_iter().enumerate() {
        decode_response_output_item(
            output_index
                .try_into()
                .map_err(|_| ResponsesCodecError::TooManyOutputItems)?,
            item,
            &mut extensions,
            &mut builder,
        )?;
    }
    if let Some(usage) = response.usage {
        collect_response_usage_extensions(&usage, &mut extensions);
        builder.push(CanonicalEventKind::Usage {
            usage: canonical_response_usage(&usage),
        });
    }
    if let Some(error) = response.error {
        collect_extra("/error", &error.extra, &mut extensions);
        builder.push(CanonicalEventKind::Error {
            error: CanonicalError {
                class: ErrorClass::Upstream,
                message: error.message,
                provider_code: Some(error.code),
                retryable: false,
            },
        });
    }
    if !extensions.is_empty() {
        builder.push(CanonicalEventKind::SourceExtension {
            extensions: SourceExtensions::new(Surface::OpenAi, extensions),
        });
    }
    builder.push(CanonicalEventKind::Done);
    Ok(builder.events)
}

fn decode_response_output_item(
    output_index: u32,
    item: Value,
    extensions: &mut BTreeMap<String, Value>,
    builder: &mut ResponsesEventBuilder,
) -> Result<(), ResponsesCodecError> {
    let Value::Object(mut object) = item else {
        return Err(ResponsesCodecError::InvalidResponse(
            "output item is not an object".into(),
        ));
    };
    let kind = take_required_output_string(&mut object, "type")?;
    match kind.as_str() {
        "message" => {
            let role = match take_required_output_string(&mut object, "role")?.as_str() {
                "assistant" => MessageRole::Assistant,
                value => return Err(ResponsesCodecError::UnsupportedRole(value.into())),
            };
            let content = object
                .remove("content")
                .and_then(|value| value.as_array().cloned())
                .ok_or_else(|| ResponsesCodecError::InvalidResponse("message content".into()))?;
            builder.push(CanonicalEventKind::MessageStart { output_index, role });
            for (part_index, part) in content.into_iter().enumerate() {
                let Value::Object(mut part) = part else {
                    return Err(ResponsesCodecError::InvalidResponse(
                        "output content part".into(),
                    ));
                };
                let part_kind = take_required_output_string(&mut part, "type")?;
                match part_kind.as_str() {
                    "output_text" => builder.push(CanonicalEventKind::TextDelta {
                        output_index,
                        text: take_required_output_string(&mut part, "text")?,
                    }),
                    "refusal" => builder.push(CanonicalEventKind::RefusalDelta {
                        output_index,
                        text: take_required_output_string(&mut part, "refusal")?,
                    }),
                    _ => return Err(ResponsesCodecError::UnsupportedOutputItem(part_kind)),
                }
                collect_object_extra(
                    &format!("/output/{output_index}/content/{part_index}"),
                    part,
                    extensions,
                );
            }
            collect_object_extra(&format!("/output/{output_index}"), object, extensions);
            builder.push(CanonicalEventKind::Finish {
                output_index,
                reason: FinishReason::Stop,
            });
        }
        "function_call" => {
            let id = object
                .remove("call_id")
                .or_else(|| object.remove("id"))
                .and_then(|value| value.as_str().map(str::to_owned));
            let name = Some(take_required_output_string(&mut object, "name")?);
            let arguments_delta = take_required_output_string(&mut object, "arguments")?;
            builder.push(CanonicalEventKind::MessageStart {
                output_index,
                role: MessageRole::Assistant,
            });
            builder.push(CanonicalEventKind::ToolCallDelta {
                output_index,
                tool_index: 0,
                id,
                name,
                arguments_delta,
            });
            collect_object_extra(&format!("/output/{output_index}"), object, extensions);
            builder.push(CanonicalEventKind::Finish {
                output_index,
                reason: FinishReason::ToolCalls,
            });
        }
        _ => {
            object.insert("type".into(), Value::String(kind));
            extensions.insert(
                format!("{OPENAI_RESPONSES_RAW_OUTPUT_PREFIX}/{output_index}"),
                Value::Object(object),
            );
        }
    }
    Ok(())
}

fn take_required_output_string(
    object: &mut Map<String, Value>,
    field: &'static str,
) -> Result<String, ResponsesCodecError> {
    object
        .remove(field)
        .and_then(|value| value.as_str().map(str::to_owned))
        .ok_or_else(|| ResponsesCodecError::InvalidResponse(field.into()))
}

fn canonical_response_usage(usage: &ResponseUsage) -> Usage {
    Usage {
        input_tokens: usage.input_tokens,
        output_tokens: usage.output_tokens,
        total_tokens: usage.total_tokens,
        cached_input_tokens: usage
            .input_tokens_details
            .as_ref()
            .map(|details| details.cached_tokens),
        reasoning_tokens: usage
            .output_tokens_details
            .as_ref()
            .map(|details| details.reasoning_tokens),
    }
}

fn collect_response_usage_extensions(
    usage: &ResponseUsage,
    extensions: &mut BTreeMap<String, Value>,
) {
    collect_extra("/usage", &usage.extra, extensions);
    if let Some(details) = &usage.input_tokens_details {
        collect_extra("/usage/input_tokens_details", &details.extra, extensions);
    }
    if let Some(details) = &usage.output_tokens_details {
        collect_extra("/usage/output_tokens_details", &details.extra, extensions);
    }
}

#[derive(Default)]
struct ResponsesEventBuilder {
    events: Vec<CanonicalEvent>,
}

pub struct OpenAiResponsesStreamDecoder {
    sse: SseDecoder,
    sequence: u64,
    response_started: bool,
    started_outputs: BTreeSet<u32>,
    finished_outputs: BTreeSet<u32>,
    done: bool,
}

impl std::fmt::Debug for OpenAiResponsesStreamDecoder {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        formatter
            .debug_struct("OpenAiResponsesStreamDecoder")
            .field("next_sequence", &self.sequence)
            .field("response_started", &self.response_started)
            .field("started_output_count", &self.started_outputs.len())
            .field("finished_output_count", &self.finished_outputs.len())
            .field("done", &self.done)
            .finish_non_exhaustive()
    }
}

impl Default for OpenAiResponsesStreamDecoder {
    fn default() -> Self {
        Self::new()
    }
}

impl OpenAiResponsesStreamDecoder {
    #[must_use]
    pub fn new() -> Self {
        Self::with_max_event_bytes(DEFAULT_MAX_EVENT_BYTES)
    }

    #[must_use]
    pub fn with_max_event_bytes(max_event_bytes: usize) -> Self {
        Self {
            sse: SseDecoder::new(max_event_bytes),
            sequence: 0,
            response_started: false,
            started_outputs: BTreeSet::new(),
            finished_outputs: BTreeSet::new(),
            done: false,
        }
    }

    pub fn push(&mut self, bytes: &[u8]) -> Result<Vec<CanonicalEvent>, ResponsesCodecError> {
        let frames = self.sse.push(bytes)?;
        self.decode_frames(frames)
    }

    pub fn finish(&mut self) -> Result<Vec<CanonicalEvent>, ResponsesCodecError> {
        let frames = self.sse.finish()?;
        let events = self.decode_frames(frames)?;
        if !self.done {
            return Err(ResponsesCodecError::UnexpectedEof);
        }
        Ok(events)
    }

    #[must_use]
    pub const fn is_done(&self) -> bool {
        self.done
    }

    fn decode_frames(
        &mut self,
        frames: Vec<SseFrame>,
    ) -> Result<Vec<CanonicalEvent>, ResponsesCodecError> {
        let mut events = Vec::new();
        for frame in frames {
            if self.done {
                return Err(ResponsesCodecError::DataAfterDone);
            }
            if frame.data.trim() == "[DONE]" {
                self.finish_open_outputs(&mut events);
                self.emit(&mut events, CanonicalEventKind::Done);
                self.done = true;
                continue;
            }
            let mut value: Value = serde_json::from_str(&frame.data)?;
            let kind = value
                .get("type")
                .and_then(Value::as_str)
                .or(frame.event.as_deref())
                .ok_or_else(|| ResponsesCodecError::InvalidResponse("stream event type".into()))?
                .to_owned();
            self.decode_stream_event(&kind, &mut value, &mut events)?;
        }
        Ok(events)
    }

    fn decode_stream_event(
        &mut self,
        kind: &str,
        value: &mut Value,
        events: &mut Vec<CanonicalEvent>,
    ) -> Result<(), ResponsesCodecError> {
        match kind {
            "response.created" | "response.in_progress" => {
                let response = value.get("response").unwrap_or(value);
                self.ensure_response_started(response, events);
            }
            "response.output_item.added" => {
                self.ensure_response_started(value, events);
                let output_index = stream_index(value, "output_index")?;
                let item = value
                    .get("item")
                    .ok_or_else(|| ResponsesCodecError::InvalidResponse("stream item".into()))?;
                let role = item.get("role").and_then(Value::as_str).map_or(
                    MessageRole::Assistant,
                    |role| match role {
                        "assistant" => MessageRole::Assistant,
                        _ => MessageRole::Assistant,
                    },
                );
                self.ensure_output_started(output_index, role, events);
                if item.get("type").and_then(Value::as_str) == Some("function_call") {
                    self.emit(
                        events,
                        CanonicalEventKind::ToolCallDelta {
                            output_index,
                            tool_index: 0,
                            id: item
                                .get("call_id")
                                .or_else(|| item.get("id"))
                                .and_then(Value::as_str)
                                .map(str::to_owned),
                            name: item.get("name").and_then(Value::as_str).map(str::to_owned),
                            arguments_delta: item
                                .get("arguments")
                                .and_then(Value::as_str)
                                .unwrap_or_default()
                                .to_owned(),
                        },
                    );
                }
            }
            "response.output_text.delta" => {
                let output_index = stream_index(value, "output_index")?;
                self.ensure_output_started(output_index, MessageRole::Assistant, events);
                self.emit(
                    events,
                    CanonicalEventKind::TextDelta {
                        output_index,
                        text: stream_string(value, "delta")?,
                    },
                );
            }
            "response.refusal.delta" => {
                let output_index = stream_index(value, "output_index")?;
                self.ensure_output_started(output_index, MessageRole::Assistant, events);
                self.emit(
                    events,
                    CanonicalEventKind::RefusalDelta {
                        output_index,
                        text: stream_string(value, "delta")?,
                    },
                );
            }
            "response.function_call_arguments.delta" => {
                let output_index = stream_index(value, "output_index")?;
                self.ensure_output_started(output_index, MessageRole::Assistant, events);
                self.emit(
                    events,
                    CanonicalEventKind::ToolCallDelta {
                        output_index,
                        tool_index: 0,
                        id: value
                            .get("item_id")
                            .and_then(Value::as_str)
                            .map(str::to_owned),
                        name: None,
                        arguments_delta: stream_string(value, "delta")?,
                    },
                );
            }
            "response.output_item.done" => {
                let output_index = stream_index(value, "output_index")?;
                if self.finished_outputs.insert(output_index) {
                    let reason = if value
                        .get("item")
                        .and_then(|item| item.get("type"))
                        .and_then(Value::as_str)
                        == Some("function_call")
                    {
                        FinishReason::ToolCalls
                    } else {
                        FinishReason::Stop
                    };
                    self.emit(
                        events,
                        CanonicalEventKind::Finish {
                            output_index,
                            reason,
                        },
                    );
                }
            }
            "response.completed" | "response.incomplete" => {
                let response = value.get("response").unwrap_or(value);
                self.ensure_response_started(response, events);
                self.finish_open_outputs(events);
                let raw_output = raw_response_output_extensions(response)?;
                if !raw_output.is_empty() {
                    self.emit(
                        events,
                        CanonicalEventKind::SourceExtension {
                            extensions: SourceExtensions::new(Surface::OpenAi, raw_output),
                        },
                    );
                }
                if let Some(usage) = response.get("usage") {
                    let usage: ResponseUsage = serde_json::from_value(usage.clone())?;
                    self.emit(
                        events,
                        CanonicalEventKind::Usage {
                            usage: canonical_response_usage(&usage),
                        },
                    );
                }
                self.emit(events, CanonicalEventKind::Done);
                self.done = true;
            }
            "response.failed" | "error" => {
                let error = value
                    .get("response")
                    .and_then(|response| response.get("error"))
                    .or_else(|| value.get("error"))
                    .unwrap_or(value);
                self.emit(
                    events,
                    CanonicalEventKind::Error {
                        error: CanonicalError {
                            class: ErrorClass::Upstream,
                            message: error
                                .get("message")
                                .and_then(Value::as_str)
                                .unwrap_or("OpenAI Responses stream failed")
                                .to_owned(),
                            provider_code: error
                                .get("code")
                                .and_then(Value::as_str)
                                .map(str::to_owned),
                            retryable: false,
                        },
                    },
                );
                self.finish_open_outputs(events);
                self.emit(events, CanonicalEventKind::Done);
                self.done = true;
            }
            // Lifecycle events that contain no new semantic payload.
            "response.content_part.added"
            | "response.content_part.done"
            | "response.output_text.done"
            | "response.function_call_arguments.done" => {}
            _ => {
                self.emit(
                    events,
                    CanonicalEventKind::SourceExtension {
                        extensions: SourceExtensions::new(
                            Surface::OpenAi,
                            BTreeMap::from([(
                                format!("/stream/{}", escape_json_pointer(kind)),
                                value.clone(),
                            )]),
                        ),
                    },
                );
            }
        }
        Ok(())
    }

    fn ensure_response_started(&mut self, value: &Value, events: &mut Vec<CanonicalEvent>) {
        if self.response_started {
            return;
        }
        let response = value.get("response").unwrap_or(value);
        self.emit(
            events,
            CanonicalEventKind::ResponseStart {
                response_id: response
                    .get("id")
                    .and_then(Value::as_str)
                    .map(str::to_owned),
                provider_model: response
                    .get("model")
                    .and_then(Value::as_str)
                    .map(str::to_owned),
            },
        );
        self.response_started = true;
    }

    fn ensure_output_started(
        &mut self,
        output_index: u32,
        role: MessageRole,
        events: &mut Vec<CanonicalEvent>,
    ) {
        if self.started_outputs.insert(output_index) {
            self.emit(
                events,
                CanonicalEventKind::MessageStart { output_index, role },
            );
        }
    }

    fn finish_open_outputs(&mut self, events: &mut Vec<CanonicalEvent>) {
        let unfinished = self
            .started_outputs
            .difference(&self.finished_outputs)
            .copied()
            .collect::<Vec<_>>();
        for output_index in unfinished {
            self.finished_outputs.insert(output_index);
            self.emit(
                events,
                CanonicalEventKind::Finish {
                    output_index,
                    reason: FinishReason::Stop,
                },
            );
        }
    }

    fn emit(&mut self, events: &mut Vec<CanonicalEvent>, kind: CanonicalEventKind) {
        events.push(CanonicalEvent::new(self.sequence, kind));
        self.sequence = self.sequence.saturating_add(1);
    }
}

fn raw_response_output_extensions(
    response: &Value,
) -> Result<BTreeMap<String, Value>, ResponsesCodecError> {
    let Some(output) = response.get("output").and_then(Value::as_array) else {
        return Ok(BTreeMap::new());
    };
    let mut extensions = BTreeMap::new();
    for (index, item) in output.iter().enumerate() {
        let kind = item
            .get("type")
            .and_then(Value::as_str)
            .ok_or_else(|| ResponsesCodecError::InvalidResponse("output item type".to_owned()))?;
        if !matches!(kind, "message" | "function_call") {
            extensions.insert(
                format!("{OPENAI_RESPONSES_RAW_OUTPUT_PREFIX}/{index}"),
                item.clone(),
            );
        }
    }
    Ok(extensions)
}

fn stream_index(value: &Value, field: &'static str) -> Result<u32, ResponsesCodecError> {
    value
        .get(field)
        .and_then(Value::as_u64)
        .and_then(|value| value.try_into().ok())
        .ok_or_else(|| ResponsesCodecError::InvalidResponse(field.into()))
}

fn stream_string(value: &Value, field: &'static str) -> Result<String, ResponsesCodecError> {
    value
        .get(field)
        .and_then(Value::as_str)
        .map(str::to_owned)
        .ok_or_else(|| ResponsesCodecError::InvalidResponse(field.into()))
}

impl ResponsesEventBuilder {
    fn push(&mut self, kind: CanonicalEventKind) {
        let sequence = self.events.len().try_into().unwrap_or(u64::MAX);
        self.events.push(CanonicalEvent::new(sequence, kind));
    }
}

fn validate_sampling(
    temperature: Option<f32>,
    top_p: Option<f32>,
) -> Result<(), ResponsesCodecError> {
    if temperature.is_some_and(|value| !(0.0..=2.0).contains(&value)) {
        return Err(ResponsesCodecError::InvalidSampling("temperature"));
    }
    if top_p.is_some_and(|value| !(0.0..=1.0).contains(&value)) {
        return Err(ResponsesCodecError::InvalidSampling("top_p"));
    }
    Ok(())
}

fn decode_response_input(
    input: ResponseInput,
    extensions: &mut BTreeMap<String, Value>,
) -> Result<Vec<Message>, ResponsesCodecError> {
    match input {
        ResponseInput::Text(text) if text.is_empty() => Err(ResponsesCodecError::EmptyInput),
        ResponseInput::Text(text) => Ok(vec![Message {
            role: MessageRole::User,
            content: vec![ContentPart::Text { text }],
            name: None,
            tool_call_id: None,
            tool_calls: Vec::new(),
        }]),
        ResponseInput::Items(items) if items.is_empty() => Err(ResponsesCodecError::EmptyInput),
        ResponseInput::Items(items) => items
            .into_iter()
            .enumerate()
            .map(|(index, item)| decode_response_input_item(index, item, extensions))
            .collect(),
    }
}

fn decode_response_input_item(
    index: usize,
    item: Value,
    extensions: &mut BTreeMap<String, Value>,
) -> Result<Message, ResponsesCodecError> {
    let Value::Object(mut object) = item else {
        return Err(ResponsesCodecError::InvalidInputItem(index));
    };
    let kind =
        take_optional_string(&mut object, "type", index)?.unwrap_or_else(|| "message".into());
    match kind.as_str() {
        "message" => decode_response_message(index, object, extensions),
        "function_call" => decode_response_function_call(index, object, extensions),
        "function_call_output" => decode_response_function_output(index, object, extensions),
        _ => Err(ResponsesCodecError::UnsupportedInputItem(kind)),
    }
}

fn decode_response_message(
    index: usize,
    mut object: Map<String, Value>,
    extensions: &mut BTreeMap<String, Value>,
) -> Result<Message, ResponsesCodecError> {
    let role = match take_required_string(&mut object, "role", index)?.as_str() {
        "system" => MessageRole::System,
        "developer" => MessageRole::Developer,
        "user" => MessageRole::User,
        "assistant" => MessageRole::Assistant,
        value => return Err(ResponsesCodecError::UnsupportedRole(value.into())),
    };
    let content = object
        .remove("content")
        .ok_or(ResponsesCodecError::MissingInputField {
            index,
            field: "content",
        })?;
    let content = decode_response_content(index, content, extensions)?;
    collect_object_extra(&format!("/input/{index}"), object, extensions);
    Ok(Message {
        role,
        content,
        name: None,
        tool_call_id: None,
        tool_calls: Vec::new(),
    })
}

fn decode_response_content(
    item_index: usize,
    content: Value,
    extensions: &mut BTreeMap<String, Value>,
) -> Result<Vec<ContentPart>, ResponsesCodecError> {
    match content {
        Value::String(text) => Ok(vec![ContentPart::Text { text }]),
        Value::Array(parts) => parts
            .into_iter()
            .enumerate()
            .map(|(part_index, part)| {
                decode_response_content_part(item_index, part_index, part, extensions)
            })
            .collect(),
        _ => Err(ResponsesCodecError::InvalidInputItem(item_index)),
    }
}

fn decode_response_content_part(
    item_index: usize,
    part_index: usize,
    part: Value,
    extensions: &mut BTreeMap<String, Value>,
) -> Result<ContentPart, ResponsesCodecError> {
    let Value::Object(mut object) = part else {
        return Err(ResponsesCodecError::InvalidContentPart {
            item: item_index,
            part: part_index,
        });
    };
    let kind = take_required_string(&mut object, "type", item_index)?;
    let prefix = format!("/input/{item_index}/content/{part_index}");
    let decoded = match kind.as_str() {
        "input_text" | "output_text" => ContentPart::Text {
            text: take_required_string(&mut object, "text", item_index)?,
        },
        "refusal" => ContentPart::Refusal {
            text: take_required_string(&mut object, "refusal", item_index)?,
        },
        "input_image" => {
            let image_url = take_optional_string(&mut object, "image_url", item_index)?;
            let file_id = take_optional_string(&mut object, "file_id", item_index)?;
            let Some(image_url) = image_url else {
                return if file_id.is_some() {
                    Err(ResponsesCodecError::FileIdNeedsResolution)
                } else {
                    Err(ResponsesCodecError::MissingInputField {
                        index: item_index,
                        field: "image_url",
                    })
                };
            };
            if file_id.is_some() {
                return Err(ResponsesCodecError::AmbiguousImageSource);
            }
            ContentPart::Image {
                source: MediaSource::Uri(image_url),
                detail: take_optional_string(&mut object, "detail", item_index)?,
            }
        }
        "input_audio" => {
            let audio =
                object
                    .remove("input_audio")
                    .ok_or(ResponsesCodecError::MissingInputField {
                        index: item_index,
                        field: "input_audio",
                    })?;
            let Value::Object(mut audio) = audio else {
                return Err(ResponsesCodecError::InvalidContentPart {
                    item: item_index,
                    part: part_index,
                });
            };
            let data = take_required_string(&mut audio, "data", item_index)?;
            let format = take_required_string(&mut audio, "format", item_index)?;
            collect_object_extra(&format!("{prefix}/input_audio"), audio, extensions);
            let media = media_handle_from_inline_marker(&data)
                .ok_or_else(|| ResponsesCodecError::InlineMediaNeedsBoundedSpool(kind.clone()))?;
            ContentPart::InputAudio { media, format }
        }
        "input_file" => {
            let file_data = take_required_string(&mut object, "file_data", item_index)?;
            let filename = take_required_string(&mut object, "filename", item_index)?;
            if object.contains_key("file_id") || object.contains_key("file_url") {
                return Err(ResponsesCodecError::AmbiguousFileSource);
            }
            let media = media_handle_from_inline_marker(&file_data)
                .ok_or_else(|| ResponsesCodecError::InlineMediaNeedsBoundedSpool(kind.clone()))?;
            ContentPart::InputFile {
                media,
                mime_type: "application/pdf".to_owned(),
                filename,
            }
        }
        _ => return Err(ResponsesCodecError::UnsupportedContentPart(kind)),
    };
    collect_object_extra(&prefix, object, extensions);
    Ok(decoded)
}

fn decode_response_function_call(
    index: usize,
    mut object: Map<String, Value>,
    extensions: &mut BTreeMap<String, Value>,
) -> Result<Message, ResponsesCodecError> {
    let call_id = take_required_string(&mut object, "call_id", index)?;
    let name = take_required_string(&mut object, "name", index)?;
    let arguments = take_required_string(&mut object, "arguments", index)?;
    let id = take_optional_string(&mut object, "id", index)?.unwrap_or_else(|| call_id.clone());
    collect_object_extra(&format!("/input/{index}"), object, extensions);
    Ok(Message {
        role: MessageRole::Assistant,
        content: Vec::new(),
        name: None,
        tool_call_id: None,
        tool_calls: vec![ToolCall {
            id,
            name,
            arguments,
        }],
    })
}

fn decode_response_function_output(
    index: usize,
    mut object: Map<String, Value>,
    extensions: &mut BTreeMap<String, Value>,
) -> Result<Message, ResponsesCodecError> {
    let call_id = take_required_string(&mut object, "call_id", index)?;
    let output = take_required_string(&mut object, "output", index)?;
    collect_object_extra(&format!("/input/{index}"), object, extensions);
    Ok(Message {
        role: MessageRole::Tool,
        content: vec![ContentPart::Text { text: output }],
        name: None,
        tool_call_id: Some(call_id),
        tool_calls: Vec::new(),
    })
}

fn take_required_string(
    object: &mut Map<String, Value>,
    field: &'static str,
    index: usize,
) -> Result<String, ResponsesCodecError> {
    take_optional_string(object, field, index)?
        .ok_or(ResponsesCodecError::MissingInputField { index, field })
}

fn take_optional_string(
    object: &mut Map<String, Value>,
    field: &'static str,
    index: usize,
) -> Result<Option<String>, ResponsesCodecError> {
    match object.remove(field) {
        None | Some(Value::Null) => Ok(None),
        Some(Value::String(value)) => Ok(Some(value)),
        Some(_) => Err(ResponsesCodecError::InvalidInputField { index, field }),
    }
}

fn collect_object_extra(
    prefix: &str,
    object: Map<String, Value>,
    extensions: &mut BTreeMap<String, Value>,
) {
    for (field, value) in object {
        extensions.insert(format!("{prefix}/{}", escape_json_pointer(&field)), value);
    }
}

fn decode_response_tool_choice(
    choice: ResponseToolChoice,
    extensions: &mut BTreeMap<String, Value>,
) -> Result<Option<ToolChoice>, ResponsesCodecError> {
    match choice {
        ResponseToolChoice::Mode(mode) => match mode.as_str() {
            "auto" => Ok(Some(ToolChoice::Auto)),
            "none" => Ok(Some(ToolChoice::None)),
            "required" => Ok(Some(ToolChoice::Required)),
            _ => Err(ResponsesCodecError::UnsupportedToolChoice(mode)),
        },
        ResponseToolChoice::Named(choice) if choice.kind == "function" => {
            collect_extra("/tool_choice", &choice.extra, extensions);
            Ok(Some(ToolChoice::Named(choice.name.ok_or(
                ResponsesCodecError::MissingToolField("tool_choice.name"),
            )?)))
        }
        ResponseToolChoice::Named(choice) => {
            extensions.insert("/tool_choice".into(), serde_json::to_value(choice)?);
            Ok(None)
        }
    }
}

fn decode_response_text_config(
    text: ResponseTextConfig,
    extensions: &mut BTreeMap<String, Value>,
) -> Result<ResponseFormat, ResponsesCodecError> {
    collect_extra("/text", &text.extra, extensions);
    if let Some(verbosity) = text.verbosity {
        extensions.insert("/text/verbosity".into(), Value::String(verbosity));
    }
    collect_extra("/text/format", &text.format.extra, extensions);
    match text.format.kind.as_str() {
        "text" => Ok(ResponseFormat::Text),
        "json_object" => Ok(ResponseFormat::JsonObject),
        "json_schema" => Ok(ResponseFormat::JsonSchema {
            name: text
                .format
                .name
                .ok_or(ResponsesCodecError::MissingJsonSchemaField("name"))?,
            description: text.format.description,
            schema: text
                .format
                .schema
                .ok_or(ResponsesCodecError::MissingJsonSchemaField("schema"))?,
            strict: text.format.strict,
        }),
        kind => Err(ResponsesCodecError::UnsupportedResponseFormat(kind.into())),
    }
}

#[derive(Debug, Error)]
pub enum ResponsesCodecError {
    #[error(transparent)]
    InvalidRoute(#[from] RouteSlugError),
    #[error(transparent)]
    Extensions(#[from] olp_domain::ExtensionError),
    #[error("Responses background mode is outside the OLP contract")]
    BackgroundUnsupported,
    #[error("stateful Responses field {field} is unsupported")]
    StatefulField { field: &'static str, value: String },
    #[error("Responses input cannot be empty")]
    EmptyInput,
    #[error("invalid Responses input item at index {0}")]
    InvalidInputItem(usize),
    #[error("invalid Responses content part {part} in item {item}")]
    InvalidContentPart { item: usize, part: usize },
    #[error("missing field {field} in Responses input item {index}")]
    MissingInputField { index: usize, field: &'static str },
    #[error("invalid field {field} in Responses input item {index}")]
    InvalidInputField { index: usize, field: &'static str },
    #[error("unsupported Responses input item type: {0}")]
    UnsupportedInputItem(String),
    #[error("unsupported Responses content part type: {0}")]
    UnsupportedContentPart(String),
    #[error("unsupported Responses message role: {0}")]
    UnsupportedRole(String),
    #[error("unsupported Responses tool type: {0}")]
    UnsupportedTool(String),
    #[error("Responses function tool is missing {0}")]
    MissingToolField(&'static str),
    #[error("unsupported Responses tool choice: {0}")]
    UnsupportedToolChoice(String),
    #[error("unsupported Responses text format: {0}")]
    UnsupportedResponseFormat(String),
    #[error("Responses JSON schema is missing {0}")]
    MissingJsonSchemaField(&'static str),
    #[error("{0} must be within the supported range")]
    InvalidSampling(&'static str),
    #[error("OpenAI file_id input requires adapter-side bounded resolution")]
    FileIdNeedsResolution,
    #[error("Responses image input must contain exactly one source")]
    AmbiguousImageSource,
    #[error("Responses input_file cannot combine inline data with file_id or file_url")]
    AmbiguousFileSource,
    #[error("{0} input must be admitted through a bounded media spool")]
    InlineMediaNeedsBoundedSpool(String),
    #[error("invalid source extension path: {0}")]
    InvalidExtension(String),
    #[error("canonical field cannot be represented by the Responses API: {0}")]
    UnrepresentableCanonicalField(&'static str),
    #[error("canonical tool output is missing tool_call_id")]
    MissingCanonicalToolCallId,
    #[error("Responses input-token counting supports only one stateless user input")]
    TokenCountSemanticsUnsupported,
    #[error("invalid Responses response object: {0}")]
    InvalidResponse(String),
    #[error("unsupported Responses output item type: {0}")]
    UnsupportedOutputItem(String),
    #[error("Responses response contains too many output items")]
    TooManyOutputItems,
    #[error(transparent)]
    Json(#[from] serde_json::Error),
    #[error(transparent)]
    Sse(#[from] SseDecodeError),
    #[error("Responses stream ended before a terminal event")]
    UnexpectedEof,
    #[error("Responses stream contained data after its terminal event")]
    DataAfterDone,
}
