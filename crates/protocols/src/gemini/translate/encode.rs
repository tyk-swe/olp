use std::collections::BTreeMap;

use olp_domain::{
    ContentPart, GenerationRequest, MediaSource, Message as CanonicalMessage, MessageRole,
    ResponseFormat, Surface, ToolChoice as CanonicalToolChoice, inline_media_marker,
};
use serde_json::{Value, json};

use super::super::dto::{
    Blob, Content, FileData, FileDataPart, FunctionCall, FunctionCallPart, FunctionCallingConfig,
    FunctionDeclaration, FunctionResponse, FunctionResponsePart, GenerateContentRequest,
    GenerationConfig, InlineDataPart, Part, TextPart, Tool, ToolConfig,
};
use super::errors::EncodeError;
use super::extensions::apply_extensions;

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
