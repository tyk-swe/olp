use std::sync::Arc;

use axum::{
    Json,
    body::{Body, Bytes},
    extract::{Extension, Multipart, State, rejection::JsonRejection},
    http::{HeaderMap, HeaderValue, StatusCode, header},
    response::{IntoResponse, Response},
};
use futures::{StreamExt, stream};
use olp_domain::{
    CanonicalEvent, CanonicalEventKind, CanonicalResult, MediaHandle, Surface, TransportMode,
};
use olp_protocols::openai::{
    EmbeddingRequest, OpenAiImageEditRequest, OpenAiImageGenerationRequest,
    OpenAiImageVariationRequest, OpenAiModerationRequest, OpenAiSpeechRequest,
    OpenAiTranscriptionRequest, decode_embedding_request, decode_image_edit,
    decode_image_generation, decode_image_variation, decode_moderation, decode_speech,
    decode_transcription, encode_embedding_response, encode_moderation_response,
    encode_speech_body, encode_transcription_response,
};
use tracing::warn;

use crate::{
    ApiState, MultipartRequestAdmission,
    image_response::streaming_image_json_response,
    streaming_response::{TerminalFrames, encode_sse_frame, sse_stream},
};

use super::{
    error::{InferenceError, valid_json},
    execution::{
        RoutedEventExecution, RoutedUnaryResult, execute_event_operation, execute_unary_result,
        incompatible_result,
    },
    limits::{CleanupMediaStream, release_limits},
    multipart::{media_spool_error, parse_multipart},
    openai_http::error_sse as openai_error_sse,
    telemetry::{UsageCapture, emit_event_execution_metadata},
};

pub(super) async fn embeddings(
    State(state): State<ApiState>,
    headers: HeaderMap,
    payload: Result<Json<EmbeddingRequest>, JsonRejection>,
) -> Result<Response, InferenceError> {
    let Json(request) = valid_json(payload)?;
    let encoding_format = request.encoding_format.clone();
    let operation = decode_embedding_request(request)
        .map_err(|error| InferenceError::invalid_request(error.to_string()))?;
    let mut executed = execute_unary_result(&state, &headers, operation).await?;
    let CanonicalResult::Embeddings(result) = executed.result.as_ref() else {
        return Err(incompatible_result("embeddings"));
    };
    let response = encode_embedding_response(
        result,
        executed.route_slug.as_str(),
        encoding_format.as_deref(),
    )
    .map_err(|error| InferenceError::bad_gateway("provider_protocol_error", error.to_string()))?;
    executed.mark_success();
    Ok((StatusCode::OK, Json(response)).into_response())
}

pub(super) async fn moderations(
    State(state): State<ApiState>,
    headers: HeaderMap,
    payload: Result<Json<OpenAiModerationRequest>, JsonRejection>,
) -> Result<Response, InferenceError> {
    let Json(request) = valid_json(payload)?;
    let operation = decode_moderation(request)
        .map_err(|error| InferenceError::invalid_request(error.to_string()))?;
    let mut executed = execute_unary_result(&state, &headers, operation).await?;
    let CanonicalResult::Moderation(result) = executed.result.as_ref() else {
        return Err(incompatible_result("moderation"));
    };
    let response = encode_moderation_response(
        result,
        executed.route_slug.as_str(),
        &format!("modr-{}", executed.request_id),
    )
    .map_err(|error| InferenceError::bad_gateway("provider_protocol_error", error.to_string()))?;
    executed.mark_success();
    Ok((StatusCode::OK, Json(response)).into_response())
}

pub(super) async fn image_generations(
    State(state): State<ApiState>,
    headers: HeaderMap,
    payload: Result<Json<OpenAiImageGenerationRequest>, JsonRejection>,
) -> Result<Response, InferenceError> {
    let Json(request) = valid_json(payload)?;
    let streaming = request.stream;
    let operation = decode_image_generation(request)
        .map_err(|error| InferenceError::invalid_request(error.to_string()))?;
    if streaming {
        let execution =
            execute_event_operation(&state, &headers, operation, TransportMode::Streaming).await?;
        return Ok(raw_media_streaming_response(state, execution));
    }
    let mut executed = execute_unary_result(&state, &headers, operation).await?;
    let CanonicalResult::Images(result) = executed.result.as_ref() else {
        return Err(incompatible_result("image generation"));
    };
    let outcome = streaming_image_json_response(Arc::clone(&state.media_spool), result).await;
    executed.mark_outcome(&outcome);
    outcome
}

pub(super) async fn image_edits(
    State(state): State<ApiState>,
    headers: HeaderMap,
    Extension(admission): Extension<MultipartRequestAdmission>,
    multipart: Multipart,
) -> Result<Response, InferenceError> {
    let mut form = parse_multipart(&state, multipart, 50 * 1024 * 1024, 32, admission).await?;
    let images = form.take_files_with_prefix("image");
    let mask = form.take_single_file("mask")?;
    let request = OpenAiImageEditRequest {
        model: form.required("model")?,
        images,
        mask,
        prompt: form.required("prompt")?,
        n: form.optional_parse("n")?,
        size: form.optional("size")?,
        stream: form.optional_parse("stream")?.unwrap_or(false),
        quality: form.optional("quality")?,
        response_format: form.optional("response_format")?,
        user: form.optional("user")?,
        background: form.optional("background")?,
        input_fidelity: form.optional("input_fidelity")?,
        output_compression: form.optional_parse("output_compression")?,
        output_format: form.optional("output_format")?,
        partial_images: form.optional_parse("partial_images")?,
        extra: form.take_extensions()?,
    };
    let streaming = request.stream;
    let operation = decode_image_edit(request)
        .map_err(|error| InferenceError::invalid_request(error.to_string()))?;
    form.disarm_cleanup();
    if streaming {
        let execution =
            execute_event_operation(&state, &headers, operation, TransportMode::Streaming).await?;
        return Ok(raw_media_streaming_response(state, execution));
    }
    encode_executed_images(
        &state,
        execute_unary_result(&state, &headers, operation).await?,
    )
    .await
}

pub(super) async fn image_variations(
    State(state): State<ApiState>,
    headers: HeaderMap,
    Extension(admission): Extension<MultipartRequestAdmission>,
    multipart: Multipart,
) -> Result<Response, InferenceError> {
    let mut form = parse_multipart(&state, multipart, 50 * 1024 * 1024, 1, admission).await?;
    let image = form
        .take_single_file("image")?
        .ok_or_else(|| InferenceError::invalid_request("The image file is required."))?;
    let request = OpenAiImageVariationRequest {
        model: form.required("model")?,
        image,
        n: form.optional_parse("n")?,
        size: form.optional("size")?,
        response_format: form.optional("response_format")?,
        user: form.optional("user")?,
        extra: form.take_extensions()?,
    };
    let operation = decode_image_variation(request)
        .map_err(|error| InferenceError::invalid_request(error.to_string()))?;
    form.disarm_cleanup();
    encode_executed_images(
        &state,
        execute_unary_result(&state, &headers, operation).await?,
    )
    .await
}

async fn encode_executed_images(
    state: &ApiState,
    mut executed: RoutedUnaryResult,
) -> Result<Response, InferenceError> {
    let CanonicalResult::Images(result) = executed.result.as_ref() else {
        return Err(incompatible_result("image"));
    };
    let outcome = streaming_image_json_response(Arc::clone(&state.media_spool), result).await;
    executed.mark_outcome(&outcome);
    outcome
}

pub(super) async fn speech(
    State(state): State<ApiState>,
    headers: HeaderMap,
    payload: Result<Json<OpenAiSpeechRequest>, JsonRejection>,
) -> Result<Response, InferenceError> {
    let Json(request) = valid_json(payload)?;
    let streaming = request.stream_format.as_deref() == Some("sse");
    let operation = decode_speech(request)
        .map_err(|error| InferenceError::invalid_request(error.to_string()))?;
    if streaming {
        let execution =
            execute_event_operation(&state, &headers, operation, TransportMode::Streaming).await?;
        return Ok(raw_media_streaming_response(state, execution));
    }
    let mut executed = execute_unary_result(&state, &headers, operation).await?;
    let CanonicalResult::Speech(result) = executed.result.as_ref() else {
        return Err(incompatible_result("speech"));
    };
    let outcome = async {
        let body = encode_speech_body(result).map_err(|error| {
            InferenceError::bad_gateway("provider_protocol_error", error.to_string())
        })?;
        let opened = open_response_media(&state, &body.media.handle).await?;
        let cleanup = CleanupMediaStream::new(
            opened.bytes,
            state.media_spool.clone(),
            opened.artifact.handle.clone(),
        );
        let mut response = Response::new(Body::from_stream(cleanup));
        if let Some(content_type) = opened.artifact.content_type {
            let content_type = HeaderValue::from_str(&content_type).map_err(|_| {
                InferenceError::bad_gateway(
                    "provider_protocol_error",
                    "The provider returned an invalid media content type.",
                )
            })?;
            response
                .headers_mut()
                .insert(header::CONTENT_TYPE, content_type);
        }
        if let Some(length) = opened.artifact.content_length {
            response.headers_mut().insert(
                header::CONTENT_LENGTH,
                HeaderValue::from_str(&length.to_string()).map_err(|_| {
                    InferenceError::bad_gateway(
                        "provider_protocol_error",
                        "The provider returned an invalid media length.",
                    )
                })?,
            );
        }
        Ok(response)
    }
    .await;
    executed.mark_outcome(&outcome);
    outcome
}

pub(super) async fn transcriptions(
    State(state): State<ApiState>,
    headers: HeaderMap,
    Extension(admission): Extension<MultipartRequestAdmission>,
    multipart: Multipart,
) -> Result<Response, InferenceError> {
    let mut form = parse_multipart(&state, multipart, 25 * 1024 * 1024, 1, admission).await?;
    let file = form
        .take_single_file("file")?
        .ok_or_else(|| InferenceError::invalid_request("The audio file is required."))?;
    let response_format = form.optional("response_format")?;
    let known_speaker_names = form.take_repeated("known_speaker_names");
    let known_speaker_references = form.take_repeated("known_speaker_references");
    let model = form.required("model")?;
    let language = form.optional("language")?;
    let prompt = form.optional("prompt")?;
    let temperature = form.optional_parse("temperature")?;
    let include = form.take_repeated("include");
    let timestamp_granularities = form.take_repeated("timestamp_granularities");
    let chunking_strategy = form
        .optional("chunking_strategy")?
        .map(|value| serde_json::from_str(&value))
        .transpose()
        .map_err(|_| InferenceError::invalid_request("chunking_strategy must be JSON"))?;
    let stream = form.optional_parse("stream")?.unwrap_or(false);
    let mut extra = form.take_extensions()?;
    if !known_speaker_names.is_empty() {
        extra.insert(
            "known_speaker_names".to_owned(),
            serde_json::to_value(known_speaker_names)
                .map_err(|_| InferenceError::invalid_request("known speaker names are invalid"))?,
        );
    }
    if !known_speaker_references.is_empty() {
        extra.insert(
            "known_speaker_references".to_owned(),
            serde_json::to_value(known_speaker_references).map_err(|_| {
                InferenceError::invalid_request("known speaker references are invalid")
            })?,
        );
    }
    let request = OpenAiTranscriptionRequest {
        model,
        file,
        language,
        prompt,
        response_format: response_format.clone(),
        temperature,
        include,
        timestamp_granularities,
        chunking_strategy,
        stream,
        extra,
    };
    let streaming = request.stream;
    let operation = decode_transcription(request)
        .map_err(|error| InferenceError::invalid_request(error.to_string()))?;
    form.disarm_cleanup();
    if streaming {
        let execution =
            execute_event_operation(&state, &headers, operation, TransportMode::Streaming).await?;
        return Ok(raw_media_streaming_response(state, execution));
    }
    let mut executed = execute_unary_result(&state, &headers, operation).await?;
    let CanonicalResult::Transcription(result) = executed.result.as_ref() else {
        return Err(incompatible_result("transcription"));
    };
    let outcome = if matches!(response_format.as_deref(), Some("text" | "srt" | "vtt")) {
        let mut response = Response::new(Body::from(result.text.clone()));
        let content_type = match response_format.as_deref() {
            Some("srt") => HeaderValue::from_static("application/x-subrip; charset=utf-8"),
            Some("vtt") => HeaderValue::from_static("text/vtt; charset=utf-8"),
            _ => HeaderValue::from_static("text/plain; charset=utf-8"),
        };
        response
            .headers_mut()
            .insert(header::CONTENT_TYPE, content_type);
        Ok(response)
    } else {
        encode_transcription_response(result)
            .map(|response| (StatusCode::OK, Json(response)).into_response())
            .map_err(|error| {
                InferenceError::bad_gateway("provider_protocol_error", error.to_string())
            })
    };
    executed.mark_outcome(&outcome);
    outcome
}

fn raw_media_streaming_response(state: ApiState, mut execution: RoutedEventExecution) -> Response {
    let (writer, response) = sse_stream();
    tokio::spawn(async move {
        let mut events = std::mem::replace(&mut execution.events, Box::pin(stream::empty()));
        let mut next = Some(Ok(execution.first.clone()));
        let mut usage = UsageCapture::default();
        let mut failure = None;
        let mut terminal = None;
        while let Some(item) = next {
            let event = match item {
                Ok(event) => event,
                Err(error) => {
                    failure = Some(InferenceError::from_transport(error));
                    break;
                }
            };
            usage.observe(&event);
            usage.observe_openai_media_event(&event);
            match raw_media_event_bytes(event) {
                Ok(Some(bytes)) => {
                    if let Err(error) = writer.send_or_fail(bytes, execution.deadline).await {
                        failure = Some(error);
                        break;
                    }
                }
                Ok(None) => {
                    terminal = Some(TerminalFrames::empty());
                    break;
                }
                Err(error) => {
                    failure = Some(error);
                    break;
                }
            }
            next = tokio::select! {
                () = writer.closed() => {
                    failure = Some(InferenceError::client_cancelled());
                    None
                }
                () = tokio::time::sleep_until(execution.deadline) => {
                    failure = Some(InferenceError::timeout());
                    None
                }
                next = events.next() => next,
            };
        }
        if terminal.is_none() && failure.is_none() {
            failure = Some(InferenceError::bad_gateway(
                "provider_protocol_error",
                "The provider media stream ended without a terminal event.",
            ));
        }
        drop(events);
        writer.finish_stream(terminal, &mut failure, |error| {
            TerminalFrames::one(openai_error_sse(error))
        });
        emit_event_execution_metadata(&state, &execution, &usage, failure.as_ref());
        release_limits(&state, execution.lease.as_ref()).await;
    });
    response
}

fn raw_media_event_bytes(event: CanonicalEvent) -> Result<Option<Bytes>, InferenceError> {
    match event.kind {
        CanonicalEventKind::SourceExtension { mut extensions } => {
            if extensions.source != Some(Surface::OpenAi) {
                return Err(InferenceError::bad_gateway(
                    "provider_protocol_error",
                    "A media stream contained extensions from another protocol.",
                ));
            }
            let data = extensions
                .values
                .remove("/__olp/raw_sse/data")
                .ok_or_else(|| {
                    InferenceError::bad_gateway(
                        "provider_protocol_error",
                        "A media stream event omitted its payload.",
                    )
                })?;
            let event_name = extensions
                .values
                .remove("/__olp/raw_sse/event")
                .and_then(|value| value.as_str().map(str::to_owned));
            if !extensions.values.is_empty() {
                return Err(InferenceError::bad_gateway(
                    "provider_protocol_error",
                    "A media stream event contained unrepresentable extensions.",
                ));
            }
            encode_sse_frame(&olp_protocols::sse::SseFrame {
                event: event_name,
                data: serde_json::to_string(&data).map_err(|_| {
                    InferenceError::bad_gateway(
                        "provider_protocol_error",
                        "A media stream event could not be encoded.",
                    )
                })?,
                id: None,
                retry_ms: None,
            })
            .map(Some)
            .map_err(|_| {
                InferenceError::bad_gateway(
                    "provider_protocol_error",
                    "A media stream event could not be encoded.",
                )
            })
        }
        CanonicalEventKind::Error { error } => Err(InferenceError::from_canonical(&error)),
        CanonicalEventKind::Done => Ok(None),
        _ => Err(InferenceError::bad_gateway(
            "provider_protocol_error",
            "A provider emitted a generation event in a media stream.",
        )),
    }
}

pub(super) async fn open_response_media(
    state: &ApiState,
    handle: &MediaHandle,
) -> Result<olp_domain::OpenedMedia, InferenceError> {
    match state.media_spool.open(handle).await {
        Ok(opened) => Ok(opened),
        Err(error) => {
            let mapped = media_spool_error(error);
            if let Err(cleanup_error) = state.media_spool.remove(handle).await
                && cleanup_error != olp_domain::MediaSpoolError::NotFound
            {
                warn!(%cleanup_error, "failed to remove unreadable response media");
            }
            Err(mapped)
        }
    }
}
