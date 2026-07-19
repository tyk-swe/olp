use std::collections::BTreeMap;

use olp_domain::{
    CanonicalEvent, CanonicalEventKind, ContentPart, FinishReason, GenerationParameters,
    GenerationRequest, MediaSource, Message as CanonicalMessage, MessageRole, Operation,
    ResponseFormat, RouteSlug, RouteSlugError, SourceExtensions, Surface, ToolCall,
    ToolChoice as CanonicalToolChoice, ToolDefinition, Usage, inline_media_marker,
    media_handle_from_inline_marker,
};
use serde_json::{Value, json};
use thiserror::Error;

use super::dto::{
    Blob, Candidate, Content, FileData, FileDataPart, FunctionCall, FunctionCallPart,
    FunctionCallingConfig, FunctionDeclaration, FunctionResponse, FunctionResponsePart,
    GenerateContentRequest, GenerateContentResponse, GenerationConfig, InlineDataPart, Part,
    TextPart, Tool, ToolConfig, UsageMetadata,
};

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

pub fn encode_generate_content_request(
    request: &GenerationRequest,
) -> Result<GenerateContentRequest, EncodeError> {
    request
        .extensions
        .ensure_representable_on(Surface::Gemini)?;
    if request.parameters.parallel_tool_calls.is_some() {
        return Err(EncodeError::ParallelToolCallsUnsupported);
    }
    let mut system_parts = Vec::new();
    let mut contents = Vec::new();
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
                    system_parts.push(Part::Text(TextPart {
                        text: text.clone(),
                        thought: None,
                        thought_signature: None,
                        extra: BTreeMap::new(),
                    }));
                }
            }
            _ => {
                conversation_started = true;
                let index = contents.len();
                contents.push(encode_content(message, index, &request.extensions.values)?);
            }
        }
    }
    if contents.is_empty() {
        return Err(EncodeError::EmptyContents);
    }
    let tools = request
        .tools
        .iter()
        .map(|tool| Tool {
            function_declarations: vec![FunctionDeclaration {
                name: tool.name.clone(),
                description: tool.description.clone(),
                parameters: tool.input_schema.clone(),
                extra: BTreeMap::new(),
            }],
            extra: BTreeMap::new(),
        })
        .collect();
    let tool_config = request.tool_choice.as_ref().map(|choice| ToolConfig {
        function_calling_config: Some(FunctionCallingConfig {
            mode: match choice {
                CanonicalToolChoice::Auto => "AUTO",
                CanonicalToolChoice::None => "NONE",
                CanonicalToolChoice::Required | CanonicalToolChoice::Named(_) => "ANY",
            }
            .into(),
            allowed_function_names: match choice {
                CanonicalToolChoice::Named(name) => vec![name.clone()],
                _ => Vec::new(),
            },
            extra: BTreeMap::new(),
        }),
        extra: BTreeMap::new(),
    });
    let (response_mime_type, response_schema) = match &request.response_format {
        None | Some(ResponseFormat::Text) => (None, None),
        Some(ResponseFormat::JsonObject) => (Some("application/json".into()), None),
        Some(ResponseFormat::JsonSchema { schema, .. }) => {
            (Some("application/json".into()), Some(schema.clone()))
        }
    };
    let generation_config = GenerationConfig {
        candidate_count: request.parameters.candidate_count,
        stop_sequences: request.parameters.stop_sequences.clone(),
        max_output_tokens: request.parameters.max_output_tokens,
        temperature: request.parameters.temperature,
        top_p: request.parameters.top_p,
        seed: request.parameters.seed,
        response_mime_type,
        response_schema,
        extra: BTreeMap::new(),
    };
    let mut encoded = GenerateContentRequest {
        model: None,
        contents,
        system_instruction: (!system_parts.is_empty()).then_some(Content {
            role: None,
            parts: system_parts,
            extra: BTreeMap::new(),
        }),
        tools,
        tool_config,
        generation_config: Some(generation_config),
        extra: BTreeMap::new(),
    };
    apply_extensions(&mut encoded, &request.extensions.values)?;
    Ok(encoded)
}

fn encode_content(
    message: &CanonicalMessage,
    content_index: usize,
    extensions: &BTreeMap<String, Value>,
) -> Result<Content, EncodeError> {
    if message.role == MessageRole::Tool {
        let name = message.name.clone().ok_or(EncodeError::MissingToolName)?;
        let id = message
            .tool_call_id
            .clone()
            .ok_or(EncodeError::MissingToolCallId)?;
        let response = match message.content.as_slice() {
            [ContentPart::Text { text }] => {
                serde_json::from_str(text).unwrap_or_else(|_| json!({ "result": text }))
            }
            [] => Value::Object(Default::default()),
            _ => return Err(EncodeError::UnsupportedToolResultContent),
        };
        return Ok(Content {
            role: Some("user".into()),
            parts: vec![Part::FunctionResponse(FunctionResponsePart {
                function_response: FunctionResponse {
                    name,
                    response,
                    id: Some(id),
                    extra: BTreeMap::new(),
                },
                extra: BTreeMap::new(),
            })],
            extra: BTreeMap::new(),
        });
    }
    if message.name.is_some() {
        return Err(EncodeError::MessageNameUnsupported);
    }
    if message.tool_call_id.is_some() {
        return Err(EncodeError::UnexpectedToolCallId);
    }
    let role = match message.role {
        MessageRole::User => "user",
        MessageRole::Assistant => "model",
        MessageRole::System | MessageRole::Developer | MessageRole::Tool => unreachable!(),
    };
    if role == "user" && !message.tool_calls.is_empty() {
        return Err(EncodeError::FunctionCallRole);
    }
    let mut parts = Vec::new();
    for part in &message.content {
        let part_index = parts.len();
        parts.push(match part {
            ContentPart::Text { text } => Part::Text(TextPart {
                text: text.clone(),
                thought: None,
                thought_signature: None,
                extra: BTreeMap::new(),
            }),
            ContentPart::Image { source, detail } => {
                if detail.is_some() {
                    return Err(EncodeError::ImageDetailUnsupported);
                }
                let inline = matches!(source, MediaSource::Handle(_));
                let mime_path = format!(
                    "/contents/{content_index}/parts/{part_index}/{}/mimeType",
                    if inline { "inlineData" } else { "fileData" }
                );
                let mime_type = extensions
                    .get(&mime_path)
                    .and_then(Value::as_str)
                    .ok_or_else(|| EncodeError::ImageMimeTypeRequired(mime_path.clone()))?;
                match source {
                    MediaSource::Uri(file_uri) => Part::FileData(FileDataPart {
                        file_data: FileData {
                            mime_type: mime_type.to_owned(),
                            file_uri: file_uri.clone(),
                            extra: BTreeMap::new(),
                        },
                        extra: BTreeMap::new(),
                    }),
                    MediaSource::Handle(handle) => Part::InlineData(InlineDataPart {
                        inline_data: Blob {
                            mime_type: mime_type.to_owned(),
                            data: inline_media_marker(handle),
                            extra: BTreeMap::new(),
                        },
                        extra: BTreeMap::new(),
                    }),
                }
            }
            ContentPart::InputAudio { media, format } => {
                if !format.starts_with("audio/") {
                    return Err(EncodeError::InvalidInputAudioMimeType);
                }
                Part::InlineData(InlineDataPart {
                    inline_data: Blob {
                        mime_type: format.clone(),
                        data: inline_media_marker(media),
                        extra: BTreeMap::new(),
                    },
                    extra: BTreeMap::new(),
                })
            }
            ContentPart::InputFile { .. } => return Err(EncodeError::InputFileUnsupported),
            ContentPart::Refusal { .. } => return Err(EncodeError::RefusalUnsupported),
        });
    }
    for call in &message.tool_calls {
        parts.push(Part::FunctionCall(FunctionCallPart {
            function_call: FunctionCall {
                name: call.name.clone(),
                args: serde_json::from_str(&call.arguments).map_err(|source| {
                    EncodeError::InvalidToolArguments {
                        tool: call.name.clone(),
                        source,
                    }
                })?,
                id: Some(call.id.clone()),
                extra: BTreeMap::new(),
            },
            extra: BTreeMap::new(),
        }));
    }
    Ok(Content {
        role: Some(role.into()),
        parts,
        extra: BTreeMap::new(),
    })
}

pub fn decode_generate_content_response(
    response: GenerateContentResponse,
) -> Result<Vec<CanonicalEvent>, ResponseError> {
    decode_response(response, true)
}

pub(crate) fn decode_generate_content_chunk(
    response: GenerateContentResponse,
) -> Result<Vec<CanonicalEvent>, ResponseError> {
    decode_response(response, false)
}

fn decode_response(
    response: GenerateContentResponse,
    require_finish: bool,
) -> Result<Vec<CanonicalEvent>, ResponseError> {
    let mut builder = EventBuilder::default();
    builder.push(CanonicalEventKind::ResponseStart {
        response_id: response.response_id,
        provider_model: response.model_version,
    });
    let mut extensions = BTreeMap::new();
    collect_extra("", &response.extra, &mut extensions);
    let metadata_only = !require_finish
        && response.candidates.is_empty()
        && (response.usage_metadata.is_some() || !extensions.is_empty());
    let prompt_blocked = response.candidates.is_empty()
        && (extensions.contains_key("/promptFeedback")
            || extensions.contains_key("/prompt_feedback"));
    if response.candidates.is_empty() && !prompt_blocked && !metadata_only {
        return Err(ResponseError::EmptyResponse);
    }
    let candidate_count = response.candidates.len();
    let mut finished_count = 0_usize;
    let mut candidate_indexes = std::collections::BTreeSet::new();
    for (position, candidate) in response.candidates.iter().enumerate() {
        let index = candidate.index.unwrap_or(
            position
                .try_into()
                .map_err(|_| ResponseError::TooManyCandidates)?,
        );
        if !candidate_indexes.insert(index) {
            return Err(ResponseError::DuplicateCandidateIndex(index));
        }
    }
    for (position, candidate) in response.candidates.into_iter().enumerate() {
        if decode_candidate(candidate, position, &mut builder, &mut extensions)? {
            finished_count += 1;
        }
    }
    if require_finish && !prompt_blocked && finished_count != candidate_count {
        return Err(ResponseError::MissingFinishReason);
    }
    if let Some(usage) = response.usage_metadata {
        collect_extra("/usageMetadata", &usage.extra, &mut extensions);
        builder.push(CanonicalEventKind::Usage {
            usage: canonical_usage(&usage),
        });
    }
    if !extensions.is_empty() {
        builder.push(CanonicalEventKind::SourceExtension {
            extensions: SourceExtensions::new(Surface::Gemini, extensions),
        });
    }
    if prompt_blocked {
        builder.push(CanonicalEventKind::Finish {
            output_index: 0,
            reason: FinishReason::ContentFilter,
        });
    }
    builder.push(CanonicalEventKind::Done);
    Ok(builder.events)
}

fn decode_candidate(
    candidate: Candidate,
    position: usize,
    builder: &mut EventBuilder,
    extensions: &mut BTreeMap<String, Value>,
) -> Result<bool, ResponseError> {
    let output_index = candidate.index.unwrap_or(
        position
            .try_into()
            .map_err(|_| ResponseError::TooManyCandidates)?,
    );
    let prefix = format!("/candidates/{output_index}");
    collect_extra(&prefix, &candidate.extra, extensions);
    builder.push(CanonicalEventKind::MessageStart {
        output_index,
        role: MessageRole::Assistant,
    });
    let mut tool_index = 0_u32;
    if let Some(content) = candidate.content {
        if content.role.as_deref().is_some_and(|role| role != "model") {
            return Err(ResponseError::UnexpectedRole(
                content.role.unwrap_or_default(),
            ));
        }
        collect_extra(&format!("{prefix}/content"), &content.extra, extensions);
        for (part_index, part) in content.parts.into_iter().enumerate() {
            match part {
                Part::Text(part)
                    if part.thought != Some(true) && part.thought_signature.is_none() =>
                {
                    collect_extra(
                        &format!("{prefix}/content/parts/{part_index}"),
                        &part.extra,
                        extensions,
                    );
                    if let Some(thought) = part.thought {
                        extensions.insert(
                            format!("{prefix}/content/parts/{part_index}/thought"),
                            Value::Bool(thought),
                        );
                    }
                    builder.push(CanonicalEventKind::TextDelta {
                        output_index,
                        text: part.text,
                    });
                }
                Part::FunctionCall(part) => {
                    collect_extra(
                        &format!("{prefix}/content/parts/{part_index}"),
                        &part.extra,
                        extensions,
                    );
                    collect_extra(
                        &format!("{prefix}/content/parts/{part_index}/functionCall"),
                        &part.function_call.extra,
                        extensions,
                    );
                    builder.push(CanonicalEventKind::ToolCallDelta {
                        output_index,
                        tool_index,
                        id: part.function_call.id,
                        name: Some(part.function_call.name),
                        arguments_delta: serde_json::to_string(&part.function_call.args)
                            .map_err(ResponseError::Json)?,
                    });
                    tool_index = tool_index
                        .checked_add(1)
                        .ok_or(ResponseError::TooManyToolCalls)?;
                }
                part => {
                    extensions.insert(
                        format!("{prefix}/content/parts/{part_index}"),
                        part.as_value(),
                    );
                }
            }
        }
    }
    let finished = candidate.finish_reason.is_some();
    if let Some(reason) = candidate.finish_reason {
        let canonical = gemini_finish_reason(&reason);
        if !matches!(reason.as_str(), "STOP" | "MAX_TOKENS") {
            extensions.insert(format!("{prefix}/finishReason"), Value::String(reason));
        }
        builder.push(CanonicalEventKind::Finish {
            output_index,
            reason: canonical,
        });
    }
    Ok(finished)
}

pub(crate) fn canonical_usage(usage: &UsageMetadata) -> Usage {
    let total_tokens = if usage.total_token_count == 0 {
        usage
            .prompt_token_count
            .saturating_add(usage.candidates_token_count)
            .saturating_add(usage.thoughts_token_count.unwrap_or(0))
    } else {
        usage.total_token_count
    };
    Usage {
        input_tokens: usage.prompt_token_count,
        output_tokens: usage.candidates_token_count,
        total_tokens,
        cached_input_tokens: usage.cached_content_token_count,
        reasoning_tokens: usage.thoughts_token_count,
    }
}

pub(crate) fn gemini_finish_reason(reason: &str) -> FinishReason {
    match reason {
        "STOP" => FinishReason::Stop,
        "MAX_TOKENS" => FinishReason::Length,
        "SAFETY"
        | "RECITATION"
        | "BLOCKLIST"
        | "PROHIBITED_CONTENT"
        | "SPII"
        | "IMAGE_SAFETY"
        | "IMAGE_PROHIBITED_CONTENT" => FinishReason::ContentFilter,
        "MALFORMED_FUNCTION_CALL" | "UNEXPECTED_TOOL_CALL" => FinishReason::Error,
        other => FinishReason::Other(other.to_owned()),
    }
}

pub fn validate_count_tokens_request(
    request: &super::dto::CountTokensRequest,
) -> Result<(), CountTokensError> {
    let has_contents = !request.contents.is_empty();
    let has_generate_request = request.generate_content_request.is_some();
    if has_contents == has_generate_request {
        return Err(CountTokensError::ExactlyOneInput);
    }
    Ok(())
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

pub(crate) fn collect_extra(
    prefix: &str,
    extra: &BTreeMap<String, Value>,
    extensions: &mut BTreeMap<String, Value>,
) {
    for (key, value) in extra {
        let key = key.replace('~', "~0").replace('/', "~1");
        extensions.insert(format!("{prefix}/{key}"), value.clone());
    }
}

fn apply_extensions(
    request: &mut GenerateContentRequest,
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
        .ok_or_else(|| EncodeError::InvalidExtensionPath(pointer.into()))?
        .split('/')
        .map(|segment| segment.replace("~1", "/").replace("~0", "~"))
        .collect::<Vec<_>>();
    if segments.is_empty() || segments.len() > 16 {
        return Err(EncodeError::InvalidExtensionPath(pointer.into()));
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
                    .map_err(|_| EncodeError::InvalidExtensionPath(pointer.into()))?;
                if insert_array_item && index <= array.len() {
                    array.insert(index, value);
                    return Ok(());
                }
                let slot = array
                    .get_mut(index)
                    .ok_or_else(|| EncodeError::InvalidExtensionPath(pointer.into()))?;
                *slot = value;
                return Ok(());
            }
            Value::Object(object) => {
                current = object
                    .get_mut(segment)
                    .ok_or_else(|| EncodeError::InvalidExtensionPath(pointer.into()))?;
            }
            Value::Array(array) => {
                let index = segment
                    .parse::<usize>()
                    .map_err(|_| EncodeError::InvalidExtensionPath(pointer.into()))?;
                current = array
                    .get_mut(index)
                    .ok_or_else(|| EncodeError::InvalidExtensionPath(pointer.into()))?;
            }
            _ => return Err(EncodeError::InvalidExtensionPath(pointer.into())),
        }
    }
    Err(EncodeError::InvalidExtensionPath(pointer.into()))
}

#[derive(Debug, Error)]
pub enum DecodeError {
    #[error(transparent)]
    InvalidRoute(#[from] RouteSlugError),
    #[error("body model {0} conflicts with the model route")]
    ConflictingModel(String),
    #[error("generateContent requires at least one non-system content")]
    EmptyContents,
    #[error("Gemini content parts cannot be empty")]
    EmptyContent,
    #[error("unsupported Gemini content role {0}")]
    UnsupportedRole(String),
    #[error("Gemini functionCall parts are valid only for model content")]
    FunctionCallRole,
    #[error("Gemini functionResponse parts are valid only for user content")]
    FunctionResponseRole,
    #[error("content after functionCall cannot be reordered canonically")]
    InterleavedFunctionCall,
    #[error("inline base64 media must be replaced by a bounded media handle before translation")]
    InlineMediaRequiresBoundedHandle,
    #[error("Gemini fileData media type {0} is not an image generation input")]
    UnsupportedFileMediaType(String),
    #[error("Gemini thought parts require source-protocol passthrough")]
    ThoughtPartUnsupported,
    #[error("unsupported Gemini part {0}")]
    UnsupportedPart(String),
    #[error("unsupported Gemini system part {0}")]
    UnsupportedSystemPart(String),
    #[error("{field} {reason}")]
    InvalidParameter {
        field: &'static str,
        reason: &'static str,
    },
    #[error("responseSchema requires responseMimeType application/json")]
    SchemaWithoutJsonMimeType,
    #[error("Gemini JSON value is invalid: {0}")]
    Json(serde_json::Error),
}

#[derive(Debug, Error)]
pub enum EncodeError {
    #[error(transparent)]
    Extensions(#[from] olp_domain::ExtensionError),
    #[error("Gemini tool configuration cannot represent parallel_tool_calls")]
    ParallelToolCallsUnsupported,
    #[error("system or developer messages cannot appear after conversation content")]
    SystemMessageAfterConversation,
    #[error("Gemini system instructions support text content only")]
    UnsupportedSystemContent,
    #[error("generateContent requires at least one conversation content")]
    EmptyContents,
    #[error("canonical message name is not representable on regular Gemini content")]
    MessageNameUnsupported,
    #[error("tool_call_id is valid only on a canonical tool result message")]
    UnexpectedToolCallId,
    #[error("Gemini function calls are valid only in model content")]
    FunctionCallRole,
    #[error("Gemini function response is missing the function name")]
    MissingToolName,
    #[error("Gemini function response is missing tool_call_id")]
    MissingToolCallId,
    #[error("Gemini function response supports one JSON-compatible text result")]
    UnsupportedToolResultContent,
    #[error("media handle cannot be encoded as Gemini fileData")]
    MediaHandleCannotBeEncoded,
    #[error("input audio requires a bounded Gemini media adapter")]
    InputAudioUnsupported,
    #[error("input files are not supported by the launch Gemini surface")]
    InputFileUnsupported,
    #[error("Gemini inline audio requires an audio MIME type")]
    InvalidInputAudioMimeType,
    #[error("canonical refusal marker is not representable by Gemini request content")]
    RefusalUnsupported,
    #[error("OpenAI-style image detail is not representable by Gemini")]
    ImageDetailUnsupported,
    #[error("Gemini image MIME type extension is required at {0}")]
    ImageMimeTypeRequired(String),
    #[error("tool {tool} arguments are not valid JSON: {source}")]
    InvalidToolArguments {
        tool: String,
        source: serde_json::Error,
    },
    #[error("source extension path cannot be applied: {0}")]
    InvalidExtensionPath(String),
    #[error("Gemini JSON value is invalid: {0}")]
    Json(serde_json::Error),
}

#[derive(Debug, Error)]
pub enum ResponseError {
    #[error("Gemini response has no candidates or prompt feedback")]
    EmptyResponse,
    #[error("Gemini unary response candidate is missing finishReason")]
    MissingFinishReason,
    #[error("Gemini response contains too many candidates")]
    TooManyCandidates,
    #[error("Gemini response repeats candidate index {0}")]
    DuplicateCandidateIndex(u32),
    #[error("Gemini candidate role is not model: {0}")]
    UnexpectedRole(String),
    #[error("Gemini response has too many tool calls")]
    TooManyToolCalls,
    #[error("Gemini response JSON is invalid: {0}")]
    Json(serde_json::Error),
}

#[derive(Clone, Copy, Debug, Error, Eq, PartialEq)]
pub enum CountTokensError {
    #[error("countTokens requires exactly one of contents or generateContentRequest")]
    ExactlyOneInput,
}
