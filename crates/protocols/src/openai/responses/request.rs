use std::collections::BTreeMap;

use olp_domain::{
    ContentPart, GenerationParameters, GenerationRequest, MediaSource, Message, MessageRole,
    Operation, ResponseFormat, RouteSlug, SourceExtensions, Surface, ToolCall, ToolChoice,
    ToolDefinition, inline_media_marker, media_handle_from_inline_marker,
};
use serde::{Deserialize, Serialize};
use serde_json::{Map, Value};

use super::super::extensions::{apply_pointer_extensions, collect_extra};
use super::errors::ResponsesCodecError;
use super::helpers::collect_object_extra;

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

pub(super) fn encode_response_content_part(
    part: &ContentPart,
) -> Result<Value, ResponsesCodecError> {
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

pub(super) fn decode_response_input(
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
