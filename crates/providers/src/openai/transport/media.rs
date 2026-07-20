use std::{
    collections::VecDeque,
    future::ready,
    sync::{Arc, Mutex},
    time::Duration,
};

use base64::{Engine as _, engine::general_purpose::STANDARD};
use bytes::Bytes;
use futures::{StreamExt, stream};
use olp_domain::{
    MediaArtifact, MediaSpool, MediaSpoolError, MediaUpload, ProviderRequest, TransportError,
    media_handle_from_inline_marker,
};
use olp_protocols::openai::{
    BoundedMediaPart, ChatCompletionRequest, ChatContentPart, ChatMessageContent,
    OpenAiImageResponse, ResponseInput, decode_image_response,
};
use reqwest::multipart;
use serde_json::Value;

use super::{OpenAiConnector, errors::*, streams::*};

const MAX_INLINE_REQUEST_MEDIA_BYTES: usize = 1024 * 1024;

pub(super) async fn hydrate_chat_media(
    request: &mut ChatCompletionRequest,
    spool: Option<&Arc<dyn MediaSpool>>,
) -> Result<(), TransportError> {
    for message in &mut request.messages {
        let Some(ChatMessageContent::Parts(parts)) = &mut message.content else {
            continue;
        };
        for part in parts {
            let ChatContentPart::InputAudio { input_audio, .. } = part else {
                continue;
            };
            if media_handle_from_inline_marker(&input_audio.data).is_some() {
                input_audio.data = read_inline_request_media(&input_audio.data, spool).await?;
            }
        }
    }
    Ok(())
}

pub(super) async fn hydrate_responses_media(
    input: &mut ResponseInput,
    spool: Option<&Arc<dyn MediaSpool>>,
) -> Result<(), TransportError> {
    let ResponseInput::Items(items) = input else {
        return Ok(());
    };
    for item in items {
        let Some(content) = item.get_mut("content").and_then(Value::as_array_mut) else {
            continue;
        };
        for part in content {
            let Some(object) = part.as_object_mut() else {
                continue;
            };
            match object.get("type").and_then(Value::as_str) {
                Some("input_audio") => {
                    let Some(audio) = object.get_mut("input_audio").and_then(Value::as_object_mut)
                    else {
                        return Err(protocol_body_error(
                            "OpenAI Responses input_audio is malformed",
                        ));
                    };
                    let Some(marker) = audio.get("data").and_then(Value::as_str) else {
                        return Err(protocol_body_error(
                            "OpenAI Responses input_audio omitted data",
                        ));
                    };
                    if media_handle_from_inline_marker(marker).is_some() {
                        let encoded = read_inline_request_media(marker, spool).await?;
                        audio.insert("data".to_owned(), Value::String(encoded));
                    }
                }
                Some("input_file") => {
                    let Some(marker) = object.get("file_data").and_then(Value::as_str) else {
                        return Err(protocol_body_error(
                            "OpenAI Responses input_file omitted file_data",
                        ));
                    };
                    if media_handle_from_inline_marker(marker).is_some() {
                        let encoded = read_inline_request_media(marker, spool).await?;
                        object.insert(
                            "file_data".to_owned(),
                            Value::String(format!("data:application/pdf;base64,{encoded}")),
                        );
                    }
                }
                _ => {}
            }
        }
    }
    Ok(())
}

async fn read_inline_request_media(
    marker: &str,
    spool: Option<&Arc<dyn MediaSpool>>,
) -> Result<String, TransportError> {
    let handle = media_handle_from_inline_marker(marker)
        .ok_or_else(|| protocol_body_error("invalid bounded inline-media handle"))?;
    let spool =
        spool.ok_or_else(|| protocol_body_error("bounded inline-media spool is unavailable"))?;
    let opened = spool.open(&handle).await.map_err(map_spool_error)?;
    if opened
        .artifact
        .content_length
        .is_none_or(|length| length > MAX_INLINE_REQUEST_MEDIA_BYTES as u64)
    {
        return Err(protocol_body_error(
            "bounded inline request media exceeded its limit",
        ));
    }
    let mut stream = opened.bytes;
    let mut bytes = Vec::new();
    while let Some(chunk) = stream.next().await {
        let chunk = chunk.map_err(map_spool_error)?;
        if bytes.len().saturating_add(chunk.len()) > MAX_INLINE_REQUEST_MEDIA_BYTES {
            return Err(protocol_body_error(
                "bounded inline request media exceeded its limit",
            ));
        }
        bytes.extend_from_slice(&chunk);
    }
    Ok(STANDARD.encode(bytes))
}

impl OpenAiConnector {
    pub(super) async fn decode_image_result(
        &self,
        request: &ProviderRequest,
        mut wire: OpenAiImageResponse,
    ) -> Result<olp_domain::ImagesResult, TransportError> {
        let mut handles = VecDeque::new();
        let mut staged = Vec::new();
        let decoded = async {
            for (index, image) in wire.data.iter_mut().enumerate() {
                let Some(encoded) = image.b64_json.take() else {
                    continue;
                };
                let spool = request.media.as_ref().ok_or_else(|| {
                    protocol_body_error("the OpenAI image response requires a bounded media spool")
                })?;
                let bytes = STANDARD
                    .decode(encoded)
                    .map_err(|error| protocol_decode_error("image base64", error))?;
                if bytes.len() > self.config.max_response_bytes {
                    return Err(protocol_body_error(
                        "OpenAI image payload exceeded the configured response bound",
                    ));
                }
                let artifact = spool
                    .put(MediaUpload {
                        filename: format!("image-{index}.bin"),
                        content_type: Some("application/octet-stream".into()),
                        maximum_length: u64::try_from(self.config.max_response_bytes)
                            .unwrap_or(u64::MAX),
                        bytes: Box::pin(stream::once(ready(Ok(Bytes::from(bytes))))),
                    })
                    .await
                    .map_err(map_spool_error)?;
                staged.push(artifact.handle.clone());
                handles.push_back(artifact.handle);
                image.b64_json = Some(String::new());
            }
            decode_image_response(wire, |_| {
                handles.pop_front().ok_or_else(|| {
                    olp_protocols::openai::ImageCodecError::Staging(
                        "image spool handle was unavailable".into(),
                    )
                })
            })
            .map_err(|error| protocol_decode_error("image", error))
        }
        .await;
        if decoded.is_err()
            && let Some(spool) = request.media.as_ref()
        {
            for handle in staged {
                let _ = spool.remove(&handle).await;
            }
        }
        decoded
    }
}

pub(super) async fn bounded_part(
    spool: &dyn olp_domain::MediaSpool,
    handle: &olp_domain::MediaHandle,
    maximum: u64,
) -> Result<BoundedMediaPart, TransportError> {
    let opened = spool.open(handle).await.map_err(map_spool_error)?;
    let length = opened.artifact.content_length.ok_or_else(|| {
        protocol_body_error("bounded media spool omitted the admitted content length")
    })?;
    BoundedMediaPart::new(
        handle.clone(),
        opened.filename,
        opened.artifact.content_type,
        length,
        maximum,
    )
    .map_err(|error| protocol_body_error(error.to_string()))
}

pub(super) fn multipart_part(
    opened: olp_domain::OpenedMedia,
) -> Result<multipart::Part, TransportError> {
    let length = opened.artifact.content_length.ok_or_else(|| {
        protocol_body_error("bounded media spool omitted the admitted content length")
    })?;
    let mut part =
        multipart::Part::stream_with_length(reqwest::Body::wrap_stream(opened.bytes), length)
            .file_name(opened.filename);
    if let Some(content_type) = opened.artifact.content_type {
        part = part.mime_str(&content_type).map_err(|_| {
            protocol_body_error("bounded media spool returned an invalid content type")
        })?;
    }
    Ok(part)
}

pub(super) fn add_optional_text(
    form: multipart::Form,
    name: &'static str,
    value: Option<String>,
) -> multipart::Form {
    match value {
        Some(value) => form.text(name, value),
        None => form,
    }
}

pub(super) fn add_extra_fields(
    mut form: multipart::Form,
    extra: std::collections::BTreeMap<String, serde_json::Value>,
) -> multipart::Form {
    for (name, value) in extra {
        if let serde_json::Value::Array(values) = value {
            let name = if name.ends_with("[]") {
                name
            } else {
                format!("{name}[]")
            };
            for value in values {
                let value = value
                    .as_str()
                    .map_or_else(|| value.to_string(), str::to_owned);
                form = form.text(name.clone(), value);
            }
        } else {
            let value = value
                .as_str()
                .map_or_else(|| value.to_string(), str::to_owned);
            form = form.text(name, value);
        }
    }
    form
}

pub(super) fn add_image_edit_fields(
    mut form: multipart::Form,
    wire: &olp_protocols::openai::OpenAiImageEditRequest,
) -> multipart::Form {
    form = add_optional_text(form, "n", wire.n.map(|value| value.to_string()));
    form = add_optional_text(form, "size", wire.size.clone());
    form = form.text("stream", wire.stream.to_string());
    form = add_optional_text(form, "quality", wire.quality.clone());
    form = add_optional_text(form, "response_format", wire.response_format.clone());
    form = add_optional_text(form, "user", wire.user.clone());
    form = add_optional_text(form, "background", wire.background.clone());
    form = add_optional_text(form, "input_fidelity", wire.input_fidelity.clone());
    form = add_optional_text(
        form,
        "output_compression",
        wire.output_compression.map(|value| value.to_string()),
    );
    form = add_optional_text(form, "output_format", wire.output_format.clone());
    form = add_optional_text(
        form,
        "partial_images",
        wire.partial_images.map(|value| value.to_string()),
    );
    add_extra_fields(form, wire.extra.clone())
}

pub(super) async fn spool_response_body(
    response: DeadlineResponse,
    spool: &Arc<dyn MediaSpool>,
    filename: String,
    content_type: Option<String>,
    maximum_length: u64,
    idle_timeout: Duration,
) -> Result<MediaArtifact, TransportError> {
    let source: ReqwestByteStream = Box::pin(response.response.bytes_stream());
    let failures = Arc::new(Mutex::new(None::<TransportError>));
    let failure_sink = Arc::clone(&failures);
    let bytes = DeadlineByteStream::new(
        source,
        response.first_body_deadline,
        idle_timeout,
        response.attempt_deadline,
    )
    .map(move |item| {
        item.map_err(|error| {
            if let Ok(mut failure) = failure_sink.lock() {
                *failure = Some(error);
            }
            MediaSpoolError::Unavailable
        })
    });
    match spool
        .put(MediaUpload {
            filename,
            content_type,
            maximum_length,
            bytes: Box::pin(bytes),
        })
        .await
    {
        Ok(artifact) => Ok(artifact),
        Err(error) => {
            let transport = failures.lock().ok().and_then(|failure| failure.clone());
            Err(transport.unwrap_or_else(|| map_spool_error(error)))
        }
    }
}
