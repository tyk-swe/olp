use std::collections::BTreeMap;

use olp_domain::{
    ContentPart, GenerationParameters, GenerationRequest, MediaSource, Message as CanonicalMessage,
    MessageRole, Operation, ResponseFormat, RouteSlug, SourceExtensions, Surface, ToolCall,
    ToolChoice as CanonicalToolChoice, ToolDefinition, media_handle_from_inline_marker,
};
use serde_json::Value;

use super::super::dto::{Content, GenerateContentRequest, GenerationConfig, Part, ToolConfig};
use super::errors::DecodeError;
use super::extensions::collect_extra;

pub fn decode_generate_content_request(
    route_model: &str,
    request: GenerateContentRequest,
    stream: bool,
) -> Result<Operation, DecodeError> {
    let route = RouteSlug::parse(route_model)?;
    let mut extensions = BTreeMap::new();
    collect_extra("", &request.extra, &mut extensions);
    if let Some(body_model) = request.model {
        if body_model != route_model && body_model != format!("models/{route_model}") {
            return Err(DecodeError::ConflictingModel(body_model));
        }
        extensions.insert("/model".into(), Value::String(body_model));
    }

    let mut messages = Vec::new();
    if let Some(system) = request.system_instruction {
        let mut content = Vec::new();
        collect_extra("/systemInstruction", &system.extra, &mut extensions);
        for (index, part) in system.parts.into_iter().enumerate() {
            match part {
                Part::Text(part)
                    if part.thought != Some(true) && part.thought_signature.is_none() =>
                {
                    collect_extra(
                        &format!("/systemInstruction/parts/{index}"),
                        &part.extra,
                        &mut extensions,
                    );
                    if let Some(thought) = part.thought {
                        extensions.insert(
                            format!("/systemInstruction/parts/{index}/thought"),
                            Value::Bool(thought),
                        );
                    }
                    content.push(ContentPart::Text { text: part.text });
                }
                part => return Err(DecodeError::UnsupportedSystemPart(part.kind().into())),
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

    let mut wire_content_index = 0_usize;
    for content in request.contents {
        let decoded = decode_content(content, wire_content_index)?;
        for segment in decoded {
            let prefix = format!("/contents/{wire_content_index}");
            collect_extra(&prefix, &segment.content_extra, &mut extensions);
            for (path, value) in segment.extensions {
                extensions.insert(format!("{prefix}{path}"), value);
            }
            messages.push(segment.message);
            wire_content_index += 1;
        }
    }
    if messages
        .iter()
        .all(|message| message.role == MessageRole::System)
    {
        return Err(DecodeError::EmptyContents);
    }

    let mut tools = Vec::new();
    let mut output_tool_index = 0_usize;
    for tool in request.tools {
        if tool.function_declarations.is_empty() {
            extensions.insert(
                format!("/tools/{output_tool_index}"),
                serde_json::to_value(tool).map_err(DecodeError::Json)?,
            );
            output_tool_index += 1;
            continue;
        }
        let tool_extra = tool.extra;
        for (declaration_index, declaration) in tool.function_declarations.into_iter().enumerate() {
            let prefix = format!("/tools/{output_tool_index}");
            if declaration_index == 0 {
                collect_extra(&prefix, &tool_extra, &mut extensions);
            }
            collect_extra(
                &format!("{prefix}/functionDeclarations/0"),
                &declaration.extra,
                &mut extensions,
            );
            tools.push(ToolDefinition {
                name: declaration.name,
                description: declaration.description,
                input_schema: declaration.parameters,
            });
            output_tool_index += 1;
        }
    }

    let tool_choice = request
        .tool_config
        .map(|config| decode_tool_config(config, &mut extensions))
        .transpose()?
        .flatten();
    let (parameters, response_format) = decode_generation_config(
        request.generation_config.unwrap_or_default(),
        stream,
        &mut extensions,
    )?;

    Ok(Operation::Generation(GenerationRequest {
        route,
        messages,
        parameters,
        tools,
        tool_choice,
        response_format,
        extensions: SourceExtensions::new(Surface::Gemini, extensions),
    }))
}

struct DecodedContent {
    message: CanonicalMessage,
    content_extra: BTreeMap<String, Value>,
    extensions: BTreeMap<String, Value>,
}

fn decode_content(
    content: Content,
    source_index: usize,
) -> Result<Vec<DecodedContent>, DecodeError> {
    let role = match content.role.as_deref().unwrap_or("user") {
        "user" => MessageRole::User,
        "model" => MessageRole::Assistant,
        other => return Err(DecodeError::UnsupportedRole(other.to_owned())),
    };
    let mut segments = Vec::new();
    let mut parts = Vec::new();
    let mut tool_calls = Vec::new();
    let mut extensions = BTreeMap::new();
    let mut seen_function_call = false;

    for part in content.parts {
        match part {
            Part::FunctionResponse(part) => {
                if role != MessageRole::User {
                    return Err(DecodeError::FunctionResponseRole);
                }
                flush_regular(
                    role,
                    &mut segments,
                    &mut parts,
                    &mut tool_calls,
                    &mut extensions,
                );
                let mut local_extensions = BTreeMap::new();
                collect_extra("/parts/0", &part.extra, &mut local_extensions);
                collect_extra(
                    "/parts/0/functionResponse",
                    &part.function_response.extra,
                    &mut local_extensions,
                );
                let response = serde_json::to_string(&part.function_response.response)
                    .map_err(DecodeError::Json)?;
                let id = match part.function_response.id {
                    Some(id) => id,
                    None => {
                        local_extensions.insert("/parts/0/functionResponse/id".into(), Value::Null);
                        part.function_response.name.clone()
                    }
                };
                segments.push(DecodedContent {
                    message: CanonicalMessage {
                        role: MessageRole::Tool,
                        content: vec![ContentPart::Text { text: response }],
                        name: Some(part.function_response.name),
                        tool_call_id: Some(id),
                        tool_calls: Vec::new(),
                    },
                    content_extra: BTreeMap::new(),
                    extensions: local_extensions,
                });
                seen_function_call = false;
            }
            Part::Text(part) => {
                if part.thought == Some(true) || part.thought_signature.is_some() {
                    return Err(DecodeError::ThoughtPartUnsupported);
                }
                if seen_function_call {
                    return Err(DecodeError::InterleavedFunctionCall);
                }
                let index = parts.len();
                collect_extra(&format!("/parts/{index}"), &part.extra, &mut extensions);
                if let Some(thought) = part.thought {
                    extensions.insert(format!("/parts/{index}/thought"), Value::Bool(thought));
                }
                parts.push(ContentPart::Text { text: part.text });
            }
            Part::FileData(part) => {
                if seen_function_call {
                    return Err(DecodeError::InterleavedFunctionCall);
                }
                if !part.file_data.mime_type.starts_with("image/") {
                    return Err(DecodeError::UnsupportedFileMediaType(
                        part.file_data.mime_type,
                    ));
                }
                let index = parts.len();
                collect_extra(&format!("/parts/{index}"), &part.extra, &mut extensions);
                collect_extra(
                    &format!("/parts/{index}/fileData"),
                    &part.file_data.extra,
                    &mut extensions,
                );
                extensions.insert(
                    format!("/parts/{index}/fileData/mimeType"),
                    Value::String(part.file_data.mime_type),
                );
                parts.push(ContentPart::Image {
                    source: MediaSource::Uri(part.file_data.file_uri),
                    detail: None,
                });
            }
            Part::InlineData(part) => {
                if seen_function_call {
                    return Err(DecodeError::InterleavedFunctionCall);
                }
                let index = parts.len();
                collect_extra(&format!("/parts/{index}"), &part.extra, &mut extensions);
                collect_extra(
                    &format!("/parts/{index}/inlineData"),
                    &part.inline_data.extra,
                    &mut extensions,
                );
                let mime_type = part.inline_data.mime_type;
                extensions.insert(
                    format!("/parts/{index}/inlineData/mimeType"),
                    Value::String(mime_type.clone()),
                );
                let handle = media_handle_from_inline_marker(&part.inline_data.data)
                    .ok_or(DecodeError::InlineMediaRequiresBoundedHandle)?;
                if mime_type.starts_with("image/") {
                    parts.push(ContentPart::Image {
                        source: MediaSource::Handle(handle),
                        detail: None,
                    });
                } else if mime_type.starts_with("audio/") {
                    parts.push(ContentPart::InputAudio {
                        media: handle,
                        format: mime_type,
                    });
                } else {
                    return Err(DecodeError::UnsupportedFileMediaType(mime_type));
                }
            }
            Part::FunctionCall(part) => {
                if role != MessageRole::Assistant {
                    return Err(DecodeError::FunctionCallRole);
                }
                seen_function_call = true;
                let part_index = parts.len() + tool_calls.len();
                collect_extra(
                    &format!("/parts/{part_index}"),
                    &part.extra,
                    &mut extensions,
                );
                collect_extra(
                    &format!("/parts/{part_index}/functionCall"),
                    &part.function_call.extra,
                    &mut extensions,
                );
                let id = match part.function_call.id {
                    Some(id) => id,
                    None => {
                        extensions
                            .insert(format!("/parts/{part_index}/functionCall/id"), Value::Null);
                        format!("gemini-call-{source_index}-{}", tool_calls.len())
                    }
                };
                tool_calls.push(ToolCall {
                    id,
                    name: part.function_call.name,
                    arguments: serde_json::to_string(&part.function_call.args)
                        .map_err(DecodeError::Json)?,
                });
            }
            Part::Unknown(value) => {
                return Err(DecodeError::UnsupportedPart(
                    value
                        .as_object()
                        .and_then(|object| object.keys().next())
                        .cloned()
                        .unwrap_or_else(|| "unknown".into()),
                ));
            }
        }
    }
    flush_regular(
        role,
        &mut segments,
        &mut parts,
        &mut tool_calls,
        &mut extensions,
    );
    if segments.is_empty() {
        return Err(DecodeError::EmptyContent);
    }
    if let Some(first) = segments.first_mut() {
        first.content_extra = content.extra;
    }
    Ok(segments)
}

fn flush_regular(
    role: MessageRole,
    segments: &mut Vec<DecodedContent>,
    parts: &mut Vec<ContentPart>,
    tool_calls: &mut Vec<ToolCall>,
    extensions: &mut BTreeMap<String, Value>,
) {
    if parts.is_empty() && tool_calls.is_empty() {
        return;
    }
    segments.push(DecodedContent {
        message: CanonicalMessage {
            role,
            content: std::mem::take(parts),
            name: None,
            tool_call_id: None,
            tool_calls: std::mem::take(tool_calls),
        },
        content_extra: BTreeMap::new(),
        extensions: std::mem::take(extensions),
    });
}

fn decode_tool_config(
    config: ToolConfig,
    extensions: &mut BTreeMap<String, Value>,
) -> Result<Option<CanonicalToolChoice>, DecodeError> {
    let raw_config = serde_json::to_value(&config).map_err(DecodeError::Json)?;
    collect_extra("/toolConfig", &config.extra, extensions);
    let Some(function) = config.function_calling_config else {
        if !config.extra.is_empty() {
            extensions.insert("/toolConfig".into(), raw_config);
        }
        return Ok(None);
    };
    collect_extra(
        "/toolConfig/functionCallingConfig",
        &function.extra,
        extensions,
    );
    match function.mode.as_str() {
        "AUTO" | "MODE_UNSPECIFIED" => Ok(Some(CanonicalToolChoice::Auto)),
        "NONE" => Ok(Some(CanonicalToolChoice::None)),
        "ANY" if function.allowed_function_names.is_empty() => {
            Ok(Some(CanonicalToolChoice::Required))
        }
        "ANY" if function.allowed_function_names.len() == 1 => Ok(Some(
            CanonicalToolChoice::Named(function.allowed_function_names[0].clone()),
        )),
        "ANY" => {
            extensions.insert(
                "/toolConfig/functionCallingConfig/allowedFunctionNames".into(),
                serde_json::to_value(function.allowed_function_names).map_err(DecodeError::Json)?,
            );
            Ok(Some(CanonicalToolChoice::Required))
        }
        _ => {
            extensions.insert("/toolConfig".into(), raw_config);
            Ok(None)
        }
    }
}

fn decode_generation_config(
    config: GenerationConfig,
    stream: bool,
    extensions: &mut BTreeMap<String, Value>,
) -> Result<(GenerationParameters, Option<ResponseFormat>), DecodeError> {
    collect_extra("/generationConfig", &config.extra, extensions);
    if config
        .temperature
        .is_some_and(|value| !(0.0..=2.0).contains(&value))
    {
        return Err(DecodeError::InvalidParameter {
            field: "temperature",
            reason: "must be between 0 and 2",
        });
    }
    if config
        .top_p
        .is_some_and(|value| !(0.0..=1.0).contains(&value))
    {
        return Err(DecodeError::InvalidParameter {
            field: "topP",
            reason: "must be between 0 and 1",
        });
    }
    if config.candidate_count == Some(0) {
        return Err(DecodeError::InvalidParameter {
            field: "candidateCount",
            reason: "must be greater than zero",
        });
    }
    let response_format = match (config.response_mime_type, config.response_schema) {
        (None, None) => None,
        (Some(mime), None) if mime == "text/plain" => None,
        (Some(mime), None) if mime == "application/json" => Some(ResponseFormat::JsonObject),
        (Some(mime), Some(schema)) if mime == "application/json" => {
            Some(ResponseFormat::JsonSchema {
                name: "response".into(),
                description: None,
                schema,
                strict: None,
            })
        }
        (Some(mime), schema) => {
            extensions.insert(
                "/generationConfig/responseMimeType".into(),
                Value::String(mime),
            );
            if let Some(schema) = schema {
                extensions.insert("/generationConfig/responseSchema".into(), schema);
            }
            None
        }
        (None, Some(_)) => return Err(DecodeError::SchemaWithoutJsonMimeType),
    };
    Ok((
        GenerationParameters {
            max_output_tokens: config.max_output_tokens,
            temperature: config.temperature,
            top_p: config.top_p,
            stop_sequences: config.stop_sequences,
            candidate_count: config.candidate_count,
            seed: config.seed,
            parallel_tool_calls: None,
            stream,
        },
        response_format,
    ))
}
