use std::{collections::BTreeMap, fmt, sync::Arc, time::Duration};

use axum::{
    Json, Router,
    body::{Body, Bytes},
    extract::{
        DefaultBodyLimit, Extension, Multipart, Path, Query, State, rejection::JsonRejection,
    },
    http::{HeaderMap, HeaderValue, StatusCode, header},
    response::{IntoResponse, Response},
    routing::{get, post},
};
use chrono::Utc;
use encoding_rs::{Encoding, UTF_8};
use futures::{StreamExt, stream};
use olp_domain::{
    ApiKey, ApiKeyLookupId, AttemptFailureClass, AttemptPlan, CanonicalError, CanonicalEvent,
    CanonicalEventKind, CanonicalResult, ErrorClass, EventSequenceError, EventSequenceValidator,
    MEDIA_DELETE_MISSING_IS_SUCCESS_EXTENSION, MediaByteStream, MediaHandle, MediaSpool, Operation,
    OperationKind, ProviderOutput, ProviderRequest, RequestId, RequestMetadata, RouteSlug, Surface,
    TargetId, TransportError, TransportMode, authorize_api_key,
};
use olp_protocols::openai::{
    BoundedMediaPart, ChatCompletionRequest, EmbeddingRequest, OpenAiImageEditRequest,
    OpenAiImageGenerationRequest, OpenAiImageVariationRequest, OpenAiModerationRequest,
    OpenAiResponsesStreamEncoder, OpenAiSpeechRequest, OpenAiTranscriptionRequest,
    OpenAiVideoContentQuery, OpenAiVideoCreateRequest, OpenAiVideoListQuery, ResponseCreateRequest,
    ResponseInputTokensRequest, decode_chat_completion, decode_embedding_request,
    decode_image_edit, decode_image_generation, decode_image_variation, decode_moderation,
    decode_response_create, decode_response_input_tokens, decode_speech, decode_transcription,
    decode_video_content_with_query, decode_video_create, decode_video_delete, decode_video_get,
    encode_embedding_response, encode_moderation_response, encode_response_input_tokens_result,
    encode_response_object, encode_speech_body, encode_transcription_response,
    encode_video_delete_response, encode_video_list_response, encode_video_object,
};
use olp_storage::{
    LimitDimension, LimitError, LimitLease, LimitRequest, MediaJobError, MediaJobFilters,
    MediaJobLifecycle, MediaJobOrder, MediaJobRecord, MediaJobState, MediaJobUpdate,
    MediaReconciliationPass, NewMediaJobReservation, UsageAttempt, UsageEvent,
};
use rust_decimal::{Decimal, prelude::FromPrimitive as _};
use serde::Serialize;
use serde_json::{Value, json};
use tracing::{error, warn};

use crate::{
    ApiState, IMAGE_VARIATION_BODY_BYTES, MAX_MEDIA_BODY_BYTES, MultipartRequestAdmission,
    MultipartRouteAdmission, Problem, TRANSCRIPTION_BODY_BYTES, VIDEO_CREATE_BODY_BYTES,
    event_completion::collect_provider_events,
    image_response::streaming_image_json_response,
    json_media::{
        admit_openai_chat, admit_openai_response_input_tokens, admit_openai_responses,
        cleanup_admitted,
    },
    openai_response::{
        OpenAiStreamEncoder, aggregate_openai_response, error_sse as openai_error_sse, unix_seconds,
    },
    semantic_validation::{operation_for_provider, select_representable_attempts_filtered},
    streaming_response::{
        ProtocolStreamEncoder, TerminalFrames, encode_server_sse_frame, encode_sse_frame,
        protocol_streaming_response, sse_stream,
    },
};

pub fn router() -> Router<ApiState> {
    Router::new()
        .route("/openai/v1/chat/completions", post(chat_completions))
        .route("/openai/v1/responses", post(responses))
        .route(
            "/openai/v1/responses/input_tokens",
            post(response_input_tokens),
        )
        .route("/openai/v1/embeddings", post(embeddings))
        .route("/openai/v1/moderations", post(moderations))
        .route("/openai/v1/images/generations", post(image_generations))
        .route(
            "/openai/v1/images/edits",
            post(image_edits).layer(DefaultBodyLimit::max(MAX_MEDIA_BODY_BYTES)),
        )
        .route(
            "/openai/v1/images/variations",
            post(image_variations).layer(DefaultBodyLimit::max(IMAGE_VARIATION_BODY_BYTES)),
        )
        .route("/openai/v1/audio/speech", post(speech))
        .route(
            "/openai/v1/audio/transcriptions",
            post(transcriptions).layer(DefaultBodyLimit::max(TRANSCRIPTION_BODY_BYTES)),
        )
        .route(
            "/openai/v1/videos",
            post(video_create)
                .get(video_list)
                .layer(DefaultBodyLimit::max(VIDEO_CREATE_BODY_BYTES)),
        )
        .route(
            "/openai/v1/videos/{video_id}",
            get(video_get).delete(video_delete),
        )
        .route("/openai/v1/videos/{video_id}/content", get(video_content))
        .merge(crate::openai_models::router())
}

async fn responses(
    State(state): State<ApiState>,
    headers: HeaderMap,
    payload: Result<Json<ResponseCreateRequest>, JsonRejection>,
) -> Result<Response, InferenceError> {
    let _ = authenticate_key(&state, &headers, OperationKind::Generation, None)?;
    let Json(mut request) = valid_json(payload)?;
    let streaming = request.stream;
    let admitted = admit_openai_responses(&state, &mut request).await?;
    let mut operation = match decode_response_create(request) {
        Ok(operation) => operation,
        Err(error) => {
            cleanup_admitted(&state, admitted).await;
            return Err(InferenceError::invalid_request(error.to_string()));
        }
    };
    let Operation::Generation(generation) = &mut operation else {
        unreachable!("the Responses codec always produces generation")
    };
    generation.extensions.values.insert(
        "/__olp/openai_endpoint".into(),
        Value::String("responses".into()),
    );
    let mode = if streaming {
        TransportMode::Streaming
    } else {
        TransportMode::Unary
    };
    let execution = execute_event_operation(&state, &headers, operation, mode).await?;
    if streaming {
        Ok(responses_streaming_response(state, execution))
    } else {
        responses_unary_response(&state, execution).await
    }
}

pub(crate) struct RoutedEventExecution {
    pub(crate) first: CanonicalEvent,
    pub(crate) events: olp_domain::ProviderEventStream,
    pub(crate) deadline: tokio::time::Instant,
    pub(crate) lease: Option<LimitLease>,
    generation_id: uuid::Uuid,
    api_key_id: uuid::Uuid,
    pub request_id: uuid::Uuid,
    pub route_slug: RouteSlug,
    surface: Surface,
    operation_kind: OperationKind,
    request_started_at: chrono::DateTime<Utc>,
    request_started: tokio::time::Instant,
    attempt_started: tokio::time::Instant,
    attempts: Vec<UsageAttempt>,
    first_byte_ms: u64,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) struct RequiredTarget {
    pub provider_id: uuid::Uuid,
    pub provider_model: String,
}

struct AuthenticatedProxyKey {
    runtime: Arc<crate::RuntimeBundle>,
    key: ApiKey,
    lookup_id: ApiKeyLookupId,
}

fn authenticate_proxy_key(
    state: &ApiState,
    plaintext_key: &str,
) -> Result<AuthenticatedProxyKey, InferenceError> {
    let runtime = crate::pin_inference_runtime(state);
    let key_hasher = state
        .key_hasher
        .as_ref()
        .ok_or_else(|| InferenceError::unavailable("api_key_authentication_unavailable"))?;
    let lookup = key_hasher
        .lookup_id(plaintext_key)
        .map_err(|_| InferenceError::unauthorized())?;
    let lookup_id = ApiKeyLookupId::parse(lookup).map_err(|_| InferenceError::unauthorized())?;
    let key = runtime
        .api_keys
        .get(&lookup_id)
        .ok_or_else(InferenceError::unauthorized)?;
    key_hasher
        .parse_and_verify(plaintext_key, key.digest.as_bytes())
        .map_err(|_| InferenceError::unauthorized())?;
    let key = key.clone();
    Ok(AuthenticatedProxyKey {
        runtime,
        key,
        lookup_id,
    })
}

async fn execute_event_operation(
    state: &ApiState,
    headers: &HeaderMap,
    operation: Operation,
    mode: TransportMode,
) -> Result<RoutedEventExecution, InferenceError> {
    let plaintext_key = bearer_token(headers)?;
    execute_event_operation_for_surface(state, plaintext_key, operation, Surface::OpenAi, mode)
        .await
}

pub(crate) async fn execute_event_operation_for_surface(
    state: &ApiState,
    plaintext_key: &str,
    operation: Operation,
    surface: Surface,
    mode: TransportMode,
) -> Result<RoutedEventExecution, InferenceError> {
    let request_media = RequestMediaGuard::new(
        state.media_spool.clone(),
        operation_media_handles(&operation),
    );
    let result =
        execute_event_operation_for_surface_inner(state, plaintext_key, operation, surface, mode)
            .await;
    request_media.cleanup().await;
    result
}

async fn execute_event_operation_for_surface_inner(
    state: &ApiState,
    plaintext_key: &str,
    operation: Operation,
    surface: Surface,
    mode: TransportMode,
) -> Result<RoutedEventExecution, InferenceError> {
    let AuthenticatedProxyKey {
        runtime: snapshot,
        key,
        lookup_id,
    } = authenticate_proxy_key(state, plaintext_key)?;
    let route_slug = operation
        .route()
        .cloned()
        .ok_or_else(|| InferenceError::invalid_request("A route model is required."))?;
    let operation_kind = operation.kind();
    authorize_api_key(&key, Some(&route_slug), operation_kind, Utc::now())
        .map_err(|error| InferenceError::forbidden(error.to_string()))?;
    let request_id = RequestId::new();
    let request_started_at = Utc::now();
    let request_started = tokio::time::Instant::now();
    let lease = reserve_limits(
        state,
        &key,
        &operation,
        lookup_id.as_str(),
        snapshot
            .routes
            .get(&route_slug)
            .map(|route| route.overall_timeout.as_duration())
            .unwrap_or(Duration::from_secs(30))
            .saturating_add(Duration::from_secs(30)),
    )
    .await?;
    let attempts = match select_representable_attempts_filtered(
        &snapshot,
        &route_slug,
        &operation,
        surface,
        mode,
        request_id.as_uuid().as_bytes(),
        |_, target| state.circuits.is_selectable(target.id),
    ) {
        Ok(attempts) => attempts,
        Err(failure) => {
            emit_request_event(
                state,
                snapshot.generation.id.as_uuid(),
                key.id.as_uuid(),
                request_id.as_uuid(),
                &route_slug,
                &[],
                request_started_at,
                request_started,
                None,
                None,
                Some(failure.status.as_u16()),
                Some(failure.code.to_owned()),
                false,
                &UsageCapture::default(),
                surface,
                operation_kind.as_str(),
            );
            release_limits(state, lease.as_ref()).await;
            return Err(failure);
        }
    };
    let route = snapshot
        .routes
        .get(&route_slug)
        .expect("attempt selection returned a known route");
    let result = execute_with_failover(
        &snapshot,
        attempts,
        RequestMetadata {
            request_id,
            operation: operation_kind,
            surface,
            mode,
        },
        operation,
        route.overall_timeout.as_duration(),
        state.media_spool.clone(),
        &state.circuits,
    )
    .await;
    let success = match result {
        Ok(success) => success,
        Err(failure) => {
            emit_request_event(
                state,
                snapshot.generation.id.as_uuid(),
                key.id.as_uuid(),
                request_id.as_uuid(),
                &route_slug,
                &failure.attempts,
                request_started_at,
                request_started,
                None,
                None,
                Some(failure.error.status.as_u16()),
                Some(failure.error.code.to_owned()),
                false,
                &UsageCapture::default(),
                surface,
                operation_kind.as_str(),
            );
            release_limits(state, lease.as_ref()).await;
            return Err(failure.error);
        }
    };
    let ExecutionOutput::Events { first, events } = success.output else {
        release_limits(state, lease.as_ref()).await;
        return Err(incompatible_result("generation"));
    };
    crate::claim_http_inference_metadata();
    Ok(RoutedEventExecution {
        first,
        events,
        deadline: success.deadline,
        lease,
        generation_id: snapshot.generation.id.as_uuid(),
        api_key_id: key.id.as_uuid(),
        request_id: request_id.as_uuid(),
        route_slug,
        surface,
        operation_kind,
        request_started_at,
        request_started,
        attempt_started: success.attempt_started,
        attempts: success.attempts,
        first_byte_ms: elapsed_ms(request_started.elapsed()),
    })
}

async fn responses_unary_response(
    state: &ApiState,
    mut execution: RoutedEventExecution,
) -> Result<Response, InferenceError> {
    let events = collect_provider_events(
        execution.first.clone(),
        &mut execution.events,
        execution.deadline,
    )
    .await;
    let (events, failure) = match events {
        Ok(events) => (events, None),
        Err(failure) => (Vec::new(), Some(failure)),
    };
    if let Some(failure) = failure {
        emit_event_execution(state, &execution, &UsageCapture::default(), Some(&failure));
        release_limits(state, execution.lease.as_ref()).await;
        return Err(failure);
    }
    let mut usage = UsageCapture::default();
    for event in &events {
        usage.observe(event);
    }
    let response = encode_response_object(
        &events,
        execution.route_slug.as_str(),
        &format!("resp_{}", execution.request_id.simple()),
    )
    .map_err(|error| InferenceError::bad_gateway("provider_protocol_error", error.to_string()));
    match response {
        Ok(response) => {
            emit_event_execution(state, &execution, &usage, None);
            release_limits(state, execution.lease.as_ref()).await;
            Ok((StatusCode::OK, Json(response)).into_response())
        }
        Err(failure) => {
            emit_event_execution(state, &execution, &usage, Some(&failure));
            release_limits(state, execution.lease.as_ref()).await;
            Err(failure)
        }
    }
}

pub(crate) fn emit_event_execution(
    state: &ApiState,
    execution: &RoutedEventExecution,
    usage: &UsageCapture,
    failure: Option<&InferenceError>,
) {
    emit_request_event(
        state,
        execution.generation_id,
        execution.api_key_id,
        execution.request_id,
        &execution.route_slug,
        &execution.attempts,
        execution.request_started_at,
        execution.request_started,
        Some(execution.attempt_started),
        Some(execution.first_byte_ms),
        Some(
            failure
                .map_or(StatusCode::OK, |error| error.status)
                .as_u16(),
        ),
        failure.map(|error| error.code.to_owned()),
        true,
        usage,
        execution.surface,
        execution.operation_kind.as_str(),
    );
}

fn responses_streaming_response(state: ApiState, execution: RoutedEventExecution) -> Response {
    let encoder = OpenAiResponsesHttpStreamEncoder(OpenAiResponsesStreamEncoder::new(
        execution.route_slug.as_str(),
        format!("resp_{}", execution.request_id.simple()),
        unix_seconds(),
    ));
    protocol_streaming_response(state, execution, encoder)
}

struct OpenAiResponsesHttpStreamEncoder(OpenAiResponsesStreamEncoder);

impl ProtocolStreamEncoder for OpenAiResponsesHttpStreamEncoder {
    fn push(&mut self, event: CanonicalEvent) -> Result<Vec<Bytes>, String> {
        self.0
            .push(event)
            .map_err(|error| error.to_string())
            .and_then(|frames| {
                frames
                    .iter()
                    .map(encode_sse_frame)
                    .collect::<Result<Vec<_>, _>>()
                    .map_err(|error| error.to_string())
            })
    }

    fn encode_error(&self, error: &InferenceError) -> Bytes {
        responses_error_sse(error)
    }
}

fn responses_error_sse(error: &InferenceError) -> Bytes {
    encode_server_sse_frame(&olp_protocols::sse::SseFrame {
        event: Some("error".to_owned()),
        data: json!({
            "type": "error",
            "code": error.code,
            "message": error.message,
            "param": null
        })
        .to_string(),
        id: None,
        retry_ms: None,
    })
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
        emit_event_execution(&state, &execution, &usage, failure.as_ref());
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

async fn response_input_tokens(
    State(state): State<ApiState>,
    headers: HeaderMap,
    payload: Result<Json<ResponseInputTokensRequest>, JsonRejection>,
) -> Result<Response, InferenceError> {
    let _ = authenticate_key(&state, &headers, OperationKind::TokenCount, None)?;
    let Json(mut request) = valid_json(payload)?;
    let admitted = admit_openai_response_input_tokens(&state, &mut request).await?;
    let operation = match decode_response_input_tokens(request) {
        Ok(operation) => operation,
        Err(error) => {
            cleanup_admitted(&state, admitted).await;
            return Err(InferenceError::invalid_request(error.to_string()));
        }
    };
    // Once decoded, the canonical token-count operation owns every admitted
    // handle. execute_unary_result installs a cancellation-safe guard before
    // its first suspension and removes the handles after transport completes.
    let mut executed = execute_unary_result(&state, &headers, operation).await?;
    let CanonicalResult::TokenCount(result) = executed.result.as_ref() else {
        return Err(incompatible_result("token count"));
    };
    let response = encode_response_input_tokens_result(result).map_err(|error| {
        InferenceError::bad_gateway("provider_protocol_error", error.to_string())
    })?;
    executed.mark_success();
    Ok((StatusCode::OK, Json(response)).into_response())
}

async fn embeddings(
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

async fn moderations(
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

async fn image_generations(
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

async fn image_edits(
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

async fn image_variations(
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

async fn speech(
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

async fn transcriptions(
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

async fn video_create(
    State(state): State<ApiState>,
    headers: HeaderMap,
    Extension(admission): Extension<MultipartRequestAdmission>,
    multipart: Multipart,
) -> Result<Response, InferenceError> {
    let mut form = parse_multipart(
        &state,
        multipart,
        olp_protocols::openai::DEFAULT_VIDEO_REFERENCE_LIMIT,
        1,
        admission,
    )
    .await?;
    let request = OpenAiVideoCreateRequest {
        model: form.required("model")?,
        prompt: form.required("prompt")?,
        input_reference: form.take_single_file("input_reference")?,
        seconds: form.optional("seconds")?,
        size: form.optional("size")?,
        extra: form.take_extensions()?,
    };
    let operation = decode_video_create(request)
        .map_err(|error| InferenceError::invalid_request(error.to_string()))?;
    let local_job_id = uuid::Uuid::now_v7();
    let (key, route_slug, required_target) =
        select_video_create_target(&state, &headers, &operation, local_job_id)?;
    let reserved = require_inference_store(&state)?
        .reserve_media_job(NewMediaJobReservation {
            id: local_job_id,
            runtime_generation_id: crate::pin_inference_runtime(&state).generation.id.as_uuid(),
            api_key_id: key.id.as_uuid(),
            provider_id: required_target.provider_id,
            provider_model: required_target.provider_model.clone(),
            route_slug: route_slug.to_string(),
            operation: OperationKind::VideoCreate,
            surface: Surface::OpenAi,
        })
        .await
        .map_err(media_job_error)?;
    // From this point execution owns cleanup of every bounded request-media
    // handle. Until the durable reservation succeeds, the multipart guard
    // remains armed so selection or PostgreSQL failures cannot leak uploads.
    form.disarm_cleanup();
    // The accepted upstream create must outlive client disconnects. Capture
    // the HTTP inference context before spawning so it keeps the original
    // runtime generation, limits reservation, and metadata ownership.
    let task = crate::spawn_http_inference_task(
        &state,
        complete_video_create(state.clone(), headers, operation, reserved, required_target),
    );
    match task.await {
        Ok(result) => result,
        Err(error) => {
            error!(%error, "video create completion task stopped unexpectedly");
            Err(InferenceError::unavailable(
                "video_create_completion_unavailable",
            ))
        }
    }
}

async fn complete_video_create(
    state: ApiState,
    headers: HeaderMap,
    operation: Operation,
    reserved: MediaJobRecord,
    required_target: RequiredTarget,
) -> Result<Response, InferenceError> {
    let mut executed = match execute_routed_result(
        &state,
        &headers,
        operation,
        TransportMode::Async,
        Some(required_target.clone()),
    )
    .await
    {
        Ok(executed) => executed,
        Err(error) => {
            if error.code == "ambiguous_upstream_result" {
                if let Err(persistence_error) = require_inference_store(&state)?
                    .mark_media_job_create_ambiguous(
                        reserved.id,
                        "upstream_create_result_ambiguous",
                    )
                    .await
                {
                    error!(job_id = %reserved.id, %persistence_error, "failed to mark ambiguous video creation");
                }
            } else {
                match media_job_deletion_finalized(require_inference_store(&state)?, reserved.id)
                    .await
                {
                    Ok(true) => {}
                    Ok(false) => {
                        state.record_media_reconciliation_gap();
                        error!(job_id = %reserved.id, "abandoned video reservation was not finalized");
                    }
                    Err(persistence_error) => {
                        state.record_media_reconciliation_gap();
                        error!(job_id = %reserved.id, %persistence_error, "failed to retire abandoned video reservation");
                    }
                }
            }
            return Err(error);
        }
    };
    let mut result = match executed.result.as_ref() {
        CanonicalResult::VideoJob(result) => result.clone(),
        _ => {
            let failure = incompatible_result("video creation");
            if let Err(error) = require_inference_store(&state)?
                .mark_media_job_create_ambiguous(
                    reserved.id,
                    "upstream_create_response_missing_job_identity",
                )
                .await
            {
                state.record_media_reconciliation_gap();
                error!(job_id = %reserved.id, %error, "failed to retire malformed video reservation");
            }
            executed.mark_failure(&failure);
            return Err(failure);
        }
    };
    let upstream_job_id = result.id.clone();
    if !valid_upstream_media_job_id(&upstream_job_id) {
        let failure = InferenceError::bad_gateway(
            "provider_protocol_error",
            "The provider returned an invalid video job identity.",
        );
        if let Err(error) = require_inference_store(&state)?
            .mark_media_job_create_ambiguous(
                reserved.id,
                "upstream_create_response_invalid_job_identity",
            )
            .await
        {
            state.record_media_reconciliation_gap();
            error!(job_id = %reserved.id, %error, "failed to retire invalid video reservation");
        }
        executed.mark_failure(&failure);
        return Err(failure);
    }
    debug_assert_eq!(executed.provider_id, required_target.provider_id);
    debug_assert_eq!(executed.provider_model, required_target.provider_model);
    let state_update = match media_job_state(&result.status) {
        Ok(state_update) => state_update,
        Err(failure) => {
            if let Err(error) = require_inference_store(&state)?
                .mark_media_job_create_cleanup_pending(
                    reserved.id,
                    &upstream_job_id,
                    "upstream_create_response_invalid_status",
                )
                .await
            {
                state.record_media_reconciliation_gap();
                error!(job_id = %reserved.id, %error, "failed to schedule malformed video cleanup");
            }
            executed.mark_failure(&failure);
            return Err(failure);
        }
    };
    let update = MediaJobUpdate {
        state: state_update,
        progress_percent: result.progress_percent,
        content_available: matches!(result.status, olp_domain::VideoStatus::Completed),
        expires_at: result
            .expires_at
            .and_then(chrono::DateTime::from_timestamp_secs),
        error_class: result
            .error
            .as_ref()
            .map(|error| format!("{:?}", error.class).to_lowercase()),
        last_polled_at: Utc::now(),
    };
    let record = attach_media_job_with_retry(&state, reserved.id, &upstream_job_id, update).await;
    let record = match record {
        Ok(record) => record,
        Err(error) => {
            let identity_conflict = matches!(error, MediaJobError::UpstreamIdentityConflict);
            // A compensation DELETE is only safe after PostgreSQL records the
            // upstream identity and cleanup intent. An ambiguous attachment
            // outcome can already have committed the active row.
            let cleanup_intent_persisted = if identity_conflict {
                false
            } else {
                match require_inference_store(&state)?
                    .mark_media_job_create_cleanup_pending(
                        reserved.id,
                        &upstream_job_id,
                        "upstream_created_local_attach_failed",
                    )
                    .await
                {
                    Ok(record)
                        if record.lifecycle == MediaJobLifecycle::CreateCleanupPending
                            && record.upstream_job_id.as_deref()
                                == Some(upstream_job_id.as_str()) =>
                    {
                        true
                    }
                    Ok(record) => {
                        error!(
                            job_id = %reserved.id,
                            lifecycle = record.lifecycle.as_str(),
                            "video cleanup intent did not retain the upstream identity"
                        );
                        false
                    }
                    Err(persistence_error) => {
                        error!(job_id = %reserved.id, %persistence_error, "failed to persist video cleanup reconciliation metadata");
                        false
                    }
                }
            };
            let compensation_confirmed = if cleanup_intent_persisted {
                let mut cleanup = decode_video_delete(upstream_job_id.clone());
                set_video_route(&mut cleanup, executed.route_slug.as_str())?;
                mark_missing_delete_as_success(&mut cleanup)?;
                let mut compensation = execute_routed_result(
                    &state,
                    &headers,
                    cleanup,
                    TransportMode::Unary,
                    Some(required_target),
                )
                .await;
                match &mut compensation {
                    Ok(compensation)
                        if matches!(
                            compensation.result.as_ref(),
                            CanonicalResult::VideoDelete(deleted) if deleted.deleted
                        ) =>
                    {
                        compensation.mark_success();
                        true
                    }
                    Ok(compensation) => {
                        let failure = incompatible_result("video deletion");
                        compensation.mark_failure(&failure);
                        false
                    }
                    Err(_) => false,
                }
            } else {
                false
            };
            if compensation_confirmed {
                match media_job_deletion_finalized(require_inference_store(&state)?, reserved.id)
                    .await
                {
                    Ok(true) => {}
                    Ok(false) => {
                        state.record_media_reconciliation_gap();
                        error!(job_id = %reserved.id, "upstream cleanup succeeded but reconciliation tombstone was not finalized");
                    }
                    Err(persistence_error) => {
                        state.record_media_reconciliation_gap();
                        error!(job_id = %reserved.id, %persistence_error, "upstream cleanup succeeded but reconciliation tombstone failed");
                    }
                }
            } else {
                state.record_media_reconciliation_gap();
                error!(
                    job_id = %reserved.id,
                    upstream_job_id = %upstream_job_id,
                    provider_id = %executed.provider_id,
                    route = %executed.route_slug,
                    "video create reconciliation gap requires operator attention"
                );
            }
            let failure = InferenceError::unavailable("media_job_create_reconciliation_pending");
            executed.mark_failure(&failure);
            return Err(failure);
        }
    };
    result.id = record.id.to_string();
    result.model = Some(executed.route_slug.to_string());
    let response = encode_video_object(&result, executed.route_slug.as_str()).map_err(|error| {
        InferenceError::bad_gateway("provider_protocol_error", error.to_string())
    })?;
    executed.mark_success();
    Ok((StatusCode::CREATED, Json(response)).into_response())
}

async fn video_list(
    State(state): State<ApiState>,
    headers: HeaderMap,
    Query(query): Query<OpenAiVideoListQuery>,
) -> Result<Response, InferenceError> {
    let key = authenticate_key(&state, &headers, OperationKind::VideoList, None)?;
    if !query.extra.is_empty() {
        return Err(InferenceError::invalid_request(
            "Video list contains unsupported query parameters.",
        ));
    }
    if query.limit == Some(0) || query.limit.is_some_and(|limit| limit > 100) {
        return Err(InferenceError::invalid_request(
            "Video list limit must be between 1 and 100.",
        ));
    }
    if query
        .order
        .as_deref()
        .is_some_and(|value| !matches!(value, "asc" | "desc"))
    {
        return Err(InferenceError::invalid_request(
            "Video list order must be asc or desc.",
        ));
    }
    let cursor = query
        .after
        .as_deref()
        .map(uuid::Uuid::parse_str)
        .transpose()
        .map_err(|_| InferenceError::invalid_request("The video cursor is invalid."))?;
    let order = if query.order.as_deref() == Some("asc") {
        MediaJobOrder::Ascending
    } else {
        MediaJobOrder::Descending
    };
    let allowed_routes = key
        .allowed_routes
        .iter()
        .map(ToString::to_string)
        .collect::<Vec<_>>();
    let page = require_inference_store(&state)?
        .media_jobs_after_id(
            &MediaJobFilters {
                api_key_id: Some(key.id.as_uuid()),
                route_slugs: allowed_routes,
                operation: Some(OperationKind::VideoCreate),
                surface: Some(Surface::OpenAi),
                ..MediaJobFilters::default()
            },
            cursor,
            order,
            query.limit.unwrap_or(20),
        )
        .await
        .map_err(media_job_error)?;
    let refreshed = stream::iter(page.items)
        .map(|record| refresh_video_list_record(&state, &headers, record))
        .buffered(4)
        .collect::<Vec<_>>()
        .await;
    let jobs = refreshed.iter().map(media_job_result).collect::<Vec<_>>();
    let result = olp_domain::VideoListResult {
        first_id: jobs.first().map(|job| job.id.clone()),
        last_id: jobs.last().map(|job| job.id.clone()),
        jobs,
        has_more: page.next_cursor.is_some(),
        extensions: olp_domain::SourceExtensions::new(Surface::OpenAi, BTreeMap::new()),
    };
    let response = encode_video_list_response(&result, "video").map_err(|error| {
        InferenceError::bad_gateway("provider_protocol_error", error.to_string())
    })?;
    Ok((StatusCode::OK, Json(response)).into_response())
}

async fn video_get(
    State(state): State<ApiState>,
    headers: HeaderMap,
    Path(video_id): Path<String>,
) -> Result<Response, InferenceError> {
    let (key, record) =
        owned_media_job(&state, &headers, &video_id, OperationKind::VideoGet).await?;
    let upstream_id = record
        .upstream_job_id
        .clone()
        .ok_or_else(|| InferenceError::unavailable("media_job_upstream_id_unavailable"))?;
    let mut operation = decode_video_get(upstream_id);
    set_video_route(&mut operation, &record.route_slug)?;
    let mut executed = execute_routed_result(
        &state,
        &headers,
        operation,
        TransportMode::Unary,
        Some(RequiredTarget {
            provider_id: record.provider_id,
            provider_model: record.provider_model.clone(),
        }),
    )
    .await?;
    debug_assert_eq!(executed.api_key_id, key.id.as_uuid());
    let mut result = match executed.result.as_ref() {
        CanonicalResult::VideoJob(result) => result.clone(),
        _ => return Err(incompatible_result("video status")),
    };
    let update = MediaJobUpdate {
        state: media_job_state(&result.status)?,
        progress_percent: result.progress_percent,
        content_available: matches!(result.status, olp_domain::VideoStatus::Completed),
        expires_at: result
            .expires_at
            .and_then(chrono::DateTime::from_timestamp_secs),
        error_class: result
            .error
            .as_ref()
            .map(|error| format!("{:?}", error.class).to_lowercase()),
        last_polled_at: Utc::now(),
    };
    let updated = require_inference_store(&state)?
        .refresh_media_job(record.id, update)
        .await
        .map_err(media_job_error)?;
    result.id = updated.id.to_string();
    result.model = Some(updated.route_slug.clone());
    let response = encode_video_object(&result, &updated.route_slug).map_err(|error| {
        InferenceError::bad_gateway("provider_protocol_error", error.to_string())
    })?;
    executed.mark_success();
    Ok((StatusCode::OK, Json(response)).into_response())
}

async fn video_content(
    State(state): State<ApiState>,
    headers: HeaderMap,
    Path(video_id): Path<String>,
    Query(query): Query<OpenAiVideoContentQuery>,
) -> Result<Response, InferenceError> {
    let (_, record) =
        owned_media_job(&state, &headers, &video_id, OperationKind::VideoContent).await?;
    let upstream_id = record
        .upstream_job_id
        .clone()
        .ok_or_else(|| InferenceError::unavailable("media_job_upstream_id_unavailable"))?;
    let mut operation = decode_video_content_with_query(upstream_id, query)
        .map_err(|error| InferenceError::invalid_request(error.to_string()))?;
    set_video_route(&mut operation, &record.route_slug)?;
    let mut executed = execute_routed_result(
        &state,
        &headers,
        operation,
        TransportMode::Unary,
        Some(RequiredTarget {
            provider_id: record.provider_id,
            provider_model: record.provider_model.clone(),
        }),
    )
    .await?;
    let result = match executed.result.as_ref() {
        CanonicalResult::VideoContent(result) => result.clone(),
        _ => return Err(incompatible_result("video content")),
    };
    let opened = open_response_media(&state, &result.media.handle).await?;
    let cleanup = CleanupMediaStream::new(
        opened.bytes,
        state.media_spool.clone(),
        opened.artifact.handle.clone(),
    );
    let mut response = Response::new(Body::from_stream(cleanup));
    if let Some(content_type) = opened.artifact.content_type {
        response.headers_mut().insert(
            header::CONTENT_TYPE,
            HeaderValue::from_str(&content_type).map_err(|_| {
                InferenceError::bad_gateway(
                    "provider_protocol_error",
                    "The provider returned an invalid video content type.",
                )
            })?,
        );
    }
    if let Some(length) = opened.artifact.content_length {
        response.headers_mut().insert(
            header::CONTENT_LENGTH,
            HeaderValue::from_str(&length.to_string()).map_err(|_| {
                InferenceError::bad_gateway(
                    "provider_protocol_error",
                    "The provider returned an invalid video length.",
                )
            })?,
        );
    }
    executed.mark_success();
    Ok(response)
}

async fn video_delete(
    State(state): State<ApiState>,
    headers: HeaderMap,
    Path(video_id): Path<String>,
) -> Result<Response, InferenceError> {
    let (_, loaded) =
        owned_media_job(&state, &headers, &video_id, OperationKind::VideoDelete).await?;
    let record = require_inference_store(&state)?
        .begin_media_job_deletion(loaded.id)
        .await
        .map_err(media_job_error)?;
    if record.lifecycle == MediaJobLifecycle::Deleted {
        let response = encode_video_delete_response(&olp_domain::VideoDeleteResult {
            id: record.id.to_string(),
            deleted: true,
            extensions: olp_domain::SourceExtensions::new(Surface::OpenAi, BTreeMap::new()),
        })
        .map_err(|error| {
            InferenceError::bad_gateway("provider_protocol_error", error.to_string())
        })?;
        return Ok((StatusCode::OK, Json(response)).into_response());
    }
    let upstream_id = record
        .upstream_job_id
        .clone()
        .ok_or_else(|| InferenceError::unavailable("media_job_upstream_id_unavailable"))?;
    let mut operation = decode_video_delete(upstream_id);
    set_video_route(&mut operation, &record.route_slug)?;
    mark_missing_delete_as_success(&mut operation)?;
    let mut executed = execute_routed_result(
        &state,
        &headers,
        operation,
        TransportMode::Unary,
        Some(RequiredTarget {
            provider_id: record.provider_id,
            provider_model: record.provider_model.clone(),
        }),
    )
    .await?;
    let mut result = match executed.result.as_ref() {
        CanonicalResult::VideoDelete(result) => result.clone(),
        _ => return Err(incompatible_result("video deletion")),
    };
    if !result.deleted {
        let failure = InferenceError::bad_gateway(
            "video_delete_not_confirmed",
            "The provider did not confirm video deletion.",
        );
        executed.mark_failure(&failure);
        return Err(failure);
    }
    let finalized = media_job_deletion_finalized(require_inference_store(&state)?, record.id)
        .await
        .map_err(media_job_error)?;
    if !finalized {
        state.record_media_reconciliation_gap();
        let failure = InferenceError::unavailable("media_job_delete_reconciliation_pending");
        executed.mark_failure(&failure);
        return Err(failure);
    }
    result.id = record.id.to_string();
    let response = encode_video_delete_response(&result).map_err(|error| {
        InferenceError::bad_gateway("provider_protocol_error", error.to_string())
    })?;
    executed.mark_success();
    Ok((StatusCode::OK, Json(response)).into_response())
}

fn select_video_create_target(
    state: &ApiState,
    headers: &HeaderMap,
    operation: &Operation,
    local_job_id: uuid::Uuid,
) -> Result<(ApiKey, RouteSlug, RequiredTarget), InferenceError> {
    let route_slug = operation
        .route()
        .cloned()
        .ok_or_else(|| InferenceError::invalid_request("A route model is required."))?;
    let key = authenticate_key(
        state,
        headers,
        OperationKind::VideoCreate,
        Some(&route_slug),
    )?;
    let snapshot = crate::pin_inference_runtime(state);
    let attempt = select_representable_attempts_filtered(
        &snapshot,
        &route_slug,
        operation,
        Surface::OpenAi,
        TransportMode::Async,
        local_job_id.as_bytes(),
        |_, target| state.circuits.is_selectable(target.id),
    )?
    .into_iter()
    .next()
    .ok_or_else(|| InferenceError::unavailable("no_eligible_provider"))?;
    Ok((
        key,
        route_slug,
        RequiredTarget {
            provider_id: attempt.provider_id.as_uuid(),
            provider_model: attempt.provider_model,
        },
    ))
}

async fn attach_media_job_with_retry(
    state: &ApiState,
    id: uuid::Uuid,
    upstream_job_id: &str,
    update: MediaJobUpdate,
) -> Result<MediaJobRecord, MediaJobError> {
    let store = require_inference_store(state)
        .map_err(|_| MediaJobError::Invalid("media persistence is not configured".to_owned()))?;
    for attempt in 0..3 {
        match store
            .attach_media_job_upstream(id, upstream_job_id, update.clone())
            .await
        {
            Ok(record) => return Ok(record),
            Err(MediaJobError::Database(_)) if attempt < 2 => {
                tokio::time::sleep(Duration::from_millis(25 * (attempt + 1))).await;
            }
            Err(error) => return Err(error),
        }
    }
    unreachable!("bounded attach retry returns on every final attempt")
}

async fn media_job_deletion_finalized(
    store: &olp_storage::PgStore,
    id: uuid::Uuid,
) -> Result<bool, MediaJobError> {
    if store.finalize_media_job_deletion(id).await? {
        return Ok(true);
    }
    Ok(store.media_job(id).await?.lifecycle == MediaJobLifecycle::Deleted)
}

fn mark_missing_delete_as_success(operation: &mut Operation) -> Result<(), InferenceError> {
    let Operation::Video(olp_domain::VideoOperation::Delete(request)) = operation else {
        return Err(InferenceError::unavailable("media_job_operation_invalid"));
    };
    request.extensions.source = Some(Surface::OpenAi);
    request.extensions.values.insert(
        MEDIA_DELETE_MISSING_IS_SUCCESS_EXTENSION.to_owned(),
        Value::Bool(true),
    );
    Ok(())
}

fn valid_upstream_media_job_id(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= 1_024
        && value.trim() == value
        && !value.chars().any(char::is_control)
}

/// Claims and reconciles a bounded metadata-only batch without authenticating
/// the API key that originally created each job. This is intentionally public
/// for the single-binary process supervisor; it is not an HTTP endpoint.
pub async fn reconcile_media_jobs_once(
    state: &ApiState,
    limit: u16,
) -> Result<MediaReconciliationPass, MediaJobError> {
    let records = require_inference_store(state)
        .map_err(|_| MediaJobError::Invalid("media persistence is not configured".to_owned()))?
        .claim_media_reconciliation_jobs(Utc::now(), limit)
        .await?;
    let claimed = u16::try_from(records.len()).unwrap_or(u16::MAX);
    let outcomes = stream::iter(records)
        .map(|record| reconcile_claimed_media_job(state, record))
        .buffer_unordered(4)
        .collect::<Vec<_>>()
        .await;
    let completed =
        u16::try_from(outcomes.iter().filter(|value| **value).count()).unwrap_or(u16::MAX);
    Ok(MediaReconciliationPass {
        claimed,
        completed,
        failed: claimed.saturating_sub(completed),
    })
}

async fn reconcile_claimed_media_job(state: &ApiState, mut record: MediaJobRecord) -> bool {
    let Some(claim_id) = record.reconciliation_claim_id else {
        state.record_media_reconciliation_gap();
        return false;
    };
    let store = require_inference_store(state)
        .expect("claimed media reconciliation always has a configured store");
    let outcome = reconcile_media_job_operation(state, &mut record).await;
    let now = Utc::now();
    let (next_attempt_at, error_class) = match outcome {
        Ok(()) => {
            let next = if matches!(record.state, MediaJobState::Queued | MediaJobState::Running)
                && record.lifecycle == MediaJobLifecycle::Active
            {
                now + chrono::Duration::seconds(5)
            } else {
                now + chrono::Duration::hours(24)
            };
            (next, None)
        }
        Err(code) => {
            let exponent = record.reconciliation_attempts.min(6);
            let seconds = 5_i64.saturating_mul(1_i64 << exponent).min(300);
            (now + chrono::Duration::seconds(seconds), Some(code))
        }
    };
    if let Err(error) = store
        .finish_media_reconciliation(record.id, claim_id, next_attempt_at, error_class)
        .await
    {
        state.record_media_reconciliation_gap();
        error!(job_id = %record.id, %error, "failed to checkpoint autonomous media reconciliation");
        return false;
    }
    if let Some(code) = error_class {
        warn!(job_id = %record.id, error_class = code, "autonomous media reconciliation will retry");
        false
    } else {
        true
    }
}

async fn reconcile_media_job_operation(
    state: &ApiState,
    record: &mut MediaJobRecord,
) -> Result<(), &'static str> {
    let store = require_inference_store(state).map_err(|_| "persistence_unavailable")?;
    match record.lifecycle {
        MediaJobLifecycle::Creating => {
            if let Some(upstream_id) = record.upstream_job_id.as_deref() {
                *record = store
                    .mark_media_job_create_cleanup_pending(
                        record.id,
                        upstream_id,
                        "stale_post_create_reservation",
                    )
                    .await
                    .map_err(|_| "persistence_unavailable")?;
            } else {
                *record = store
                    .mark_media_job_create_ambiguous(
                        record.id,
                        "upstream_create_outcome_unknown_after_restart",
                    )
                    .await
                    .map_err(|_| "persistence_unavailable")?;
                return Err("upstream_create_outcome_unknown");
            }
        }
        MediaJobLifecycle::CreateAmbiguous => {
            let Some(upstream_id) = record.upstream_job_id.as_deref() else {
                return Err("upstream_create_outcome_unknown");
            };
            *record = store
                .mark_media_job_create_cleanup_pending(
                    record.id,
                    upstream_id,
                    "ambiguous_create_has_cleanup_identity",
                )
                .await
                .map_err(|_| "persistence_unavailable")?;
        }
        MediaJobLifecycle::Deleted => return Ok(()),
        MediaJobLifecycle::Active
        | MediaJobLifecycle::CreateCleanupPending
        | MediaJobLifecycle::DeletePending => {}
    }

    if record.lifecycle == MediaJobLifecycle::Active
        && (record
            .expires_at
            .is_some_and(|expires_at| expires_at <= Utc::now())
            || record.created_at <= Utc::now() - chrono::Duration::days(30))
    {
        *record = store
            .begin_media_job_deletion(record.id)
            .await
            .map_err(|_| "persistence_unavailable")?;
    }

    let upstream_id = record
        .upstream_job_id
        .clone()
        .filter(|value| valid_upstream_media_job_id(value))
        .ok_or("media_job_upstream_id_unavailable")?;
    if record.lifecycle == MediaJobLifecycle::Active {
        let mut operation = decode_video_get(upstream_id);
        set_video_route(&mut operation, &record.route_slug).map_err(|error| error.code)?;
        let result = execute_media_reconciliation_result(state, record, operation).await?;
        let CanonicalResult::VideoJob(result) = result.as_ref() else {
            return Err("provider_protocol_error");
        };
        let state_update = media_job_state(&result.status).map_err(|error| error.code)?;
        *record = store
            .refresh_media_job(
                record.id,
                MediaJobUpdate {
                    state: state_update,
                    progress_percent: result.progress_percent,
                    content_available: matches!(result.status, olp_domain::VideoStatus::Completed),
                    expires_at: result
                        .expires_at
                        .and_then(chrono::DateTime::from_timestamp_secs),
                    error_class: result
                        .error
                        .as_ref()
                        .map(|error| format!("{:?}", error.class).to_lowercase()),
                    last_polled_at: Utc::now(),
                },
            )
            .await
            .map_err(|_| "persistence_unavailable")?;
        return Ok(());
    }

    let mut operation = decode_video_delete(upstream_id);
    set_video_route(&mut operation, &record.route_slug).map_err(|error| error.code)?;
    mark_missing_delete_as_success(&mut operation).map_err(|error| error.code)?;
    let result = execute_media_reconciliation_result(state, record, operation).await?;
    if !matches!(
        result.as_ref(),
        CanonicalResult::VideoDelete(deleted) if deleted.deleted
    ) {
        return Err("video_delete_not_confirmed");
    }
    let finalized = media_job_deletion_finalized(store, record.id)
        .await
        .map_err(|_| "persistence_unavailable")?;
    if !finalized {
        state.record_media_reconciliation_gap();
        return Err("persistence_unavailable");
    }
    record.lifecycle = MediaJobLifecycle::Deleted;
    Ok(())
}

async fn execute_media_reconciliation_result(
    state: &ApiState,
    record: &MediaJobRecord,
    operation: Operation,
) -> Result<Box<CanonicalResult>, &'static str> {
    let snapshot = state.runtime.pin();
    let route_slug = operation
        .route()
        .cloned()
        .ok_or("media_job_route_invalid")?;
    let request_id = RequestId::new();
    let request_started_at = Utc::now();
    let request_started = tokio::time::Instant::now();
    let operation_kind = operation.kind();
    let attempts = match select_representable_attempts_filtered(
        &snapshot,
        &route_slug,
        &operation,
        Surface::OpenAi,
        TransportMode::Unary,
        request_id.as_uuid().as_bytes(),
        |_, target| {
            target.provider_id.as_uuid() == record.provider_id
                && target.provider_model == record.provider_model
                && state.circuits.is_selectable(target.id)
        },
    ) {
        Ok(attempts) => attempts,
        Err(failure) => {
            emit_request_event(
                state,
                snapshot.generation.id.as_uuid(),
                record.api_key_id,
                request_id.as_uuid(),
                &route_slug,
                &[],
                request_started_at,
                request_started,
                None,
                None,
                Some(failure.status.as_u16()),
                Some(failure.code.to_owned()),
                false,
                &UsageCapture::default(),
                Surface::OpenAi,
                operation_kind.as_str(),
            );
            return Err(failure.code);
        }
    };
    let route = snapshot
        .routes
        .get(&route_slug)
        .ok_or("media_job_route_invalid")?;
    let execution = execute_with_failover(
        &snapshot,
        attempts,
        RequestMetadata {
            request_id,
            operation: operation_kind,
            surface: Surface::OpenAi,
            mode: TransportMode::Unary,
        },
        operation,
        route.overall_timeout.as_duration(),
        state.media_spool.clone(),
        &state.circuits,
    )
    .await;
    let success = match execution {
        Ok(success) => success,
        Err(failure) => {
            emit_request_event(
                state,
                snapshot.generation.id.as_uuid(),
                record.api_key_id,
                request_id.as_uuid(),
                &route_slug,
                &failure.attempts,
                request_started_at,
                request_started,
                None,
                None,
                Some(failure.error.status.as_u16()),
                Some(failure.error.code.to_owned()),
                false,
                &UsageCapture::default(),
                Surface::OpenAi,
                operation_kind.as_str(),
            );
            return Err(failure.error.code);
        }
    };
    let ExecutionOutput::Result(result) = success.output else {
        emit_request_event(
            state,
            snapshot.generation.id.as_uuid(),
            record.api_key_id,
            request_id.as_uuid(),
            &route_slug,
            &success.attempts,
            request_started_at,
            request_started,
            Some(success.attempt_started),
            Some(elapsed_ms(request_started.elapsed())),
            Some(StatusCode::BAD_GATEWAY.as_u16()),
            Some("provider_protocol_error".to_owned()),
            true,
            &UsageCapture::default(),
            Surface::OpenAi,
            operation_kind.as_str(),
        );
        return Err("provider_protocol_error");
    };
    let first_byte_ms = elapsed_ms(request_started.elapsed());
    emit_request_event(
        state,
        snapshot.generation.id.as_uuid(),
        record.api_key_id,
        request_id.as_uuid(),
        &route_slug,
        &success.attempts,
        request_started_at,
        request_started,
        Some(success.attempt_started),
        Some(first_byte_ms),
        Some(StatusCode::OK.as_u16()),
        None,
        true,
        &usage_from_result(&result),
        Surface::OpenAi,
        operation_kind.as_str(),
    );
    Ok(result)
}

async fn refresh_video_list_record(
    state: &ApiState,
    headers: &HeaderMap,
    record: MediaJobRecord,
) -> MediaJobRecord {
    if !matches!(record.state, MediaJobState::Queued | MediaJobState::Running) {
        return record;
    }
    let Some(upstream_id) = record.upstream_job_id.clone() else {
        return record;
    };
    let mut operation = decode_video_get(upstream_id);
    if set_video_route(&mut operation, &record.route_slug).is_err() {
        return record;
    }
    let Ok(mut executed) = execute_routed_result(
        state,
        headers,
        operation,
        TransportMode::Unary,
        Some(RequiredTarget {
            provider_id: record.provider_id,
            provider_model: record.provider_model.clone(),
        }),
    )
    .await
    else {
        return record;
    };
    let result = match executed.result.as_ref() {
        CanonicalResult::VideoJob(result) => result.clone(),
        _ => return record,
    };
    let Ok(state_update) = media_job_state(&result.status) else {
        return record;
    };
    let update = MediaJobUpdate {
        state: state_update,
        progress_percent: result.progress_percent,
        content_available: matches!(result.status, olp_domain::VideoStatus::Completed),
        expires_at: result
            .expires_at
            .and_then(chrono::DateTime::from_timestamp_secs),
        error_class: result
            .error
            .as_ref()
            .map(|error| format!("{:?}", error.class).to_lowercase()),
        last_polled_at: Utc::now(),
    };
    let updated = require_inference_store(state)
        .expect("list refresh runs only with a configured store")
        .refresh_media_job(record.id, update)
        .await
        .unwrap_or(record);
    executed.mark_success();
    updated
}

fn authenticate_key(
    state: &ApiState,
    headers: &HeaderMap,
    operation: OperationKind,
    route: Option<&RouteSlug>,
) -> Result<ApiKey, InferenceError> {
    let plaintext = bearer_token(headers)?;
    let authenticated = authenticate_proxy_key(state, plaintext)?;
    authorize_api_key(&authenticated.key, route, operation, Utc::now())
        .map_err(|error| InferenceError::forbidden(error.to_string()))?;
    Ok(authenticated.key)
}

async fn owned_media_job(
    state: &ApiState,
    headers: &HeaderMap,
    video_id: &str,
    operation: OperationKind,
) -> Result<(ApiKey, MediaJobRecord), InferenceError> {
    let key = authenticate_key(state, headers, operation, None)?;
    let id = uuid::Uuid::parse_str(video_id)
        .map_err(|_| InferenceError::resource_not_found("video_not_found"))?;
    let record = require_inference_store(state)?
        .media_job(id)
        .await
        .map_err(media_job_error)?;
    if record.api_key_id != key.id.as_uuid() {
        return Err(InferenceError::resource_not_found("video_not_found"));
    }
    if record.lifecycle == MediaJobLifecycle::Deleted && operation != OperationKind::VideoDelete {
        return Err(InferenceError::resource_not_found("video_not_found"));
    }
    if !matches!(
        record.lifecycle,
        MediaJobLifecycle::Active | MediaJobLifecycle::DeletePending | MediaJobLifecycle::Deleted
    ) {
        return Err(InferenceError::unavailable(
            "media_job_reconciliation_pending",
        ));
    }
    let route = RouteSlug::parse(&record.route_slug)
        .map_err(|_| InferenceError::unavailable("media_job_route_invalid"))?;
    authorize_api_key(&key, Some(&route), operation, Utc::now())
        .map_err(|error| InferenceError::forbidden(error.to_string()))?;
    Ok((key, record))
}

fn set_video_route(operation: &mut Operation, route: &str) -> Result<(), InferenceError> {
    let route = RouteSlug::parse(route)
        .map_err(|_| InferenceError::unavailable("media_job_route_invalid"))?;
    let Operation::Video(operation) = operation else {
        return Err(InferenceError::unavailable("media_job_operation_invalid"));
    };
    match operation {
        olp_domain::VideoOperation::Get(request)
        | olp_domain::VideoOperation::Content(request)
        | olp_domain::VideoOperation::Delete(request) => request.route = Some(route),
        _ => return Err(InferenceError::unavailable("media_job_operation_invalid")),
    }
    Ok(())
}

fn require_inference_store(state: &ApiState) -> Result<&olp_storage::PgStore, InferenceError> {
    state
        .store
        .as_ref()
        .ok_or_else(|| InferenceError::unavailable("persistence_unavailable"))
}

fn media_job_error(error: MediaJobError) -> InferenceError {
    match error {
        MediaJobError::NotFound => InferenceError::resource_not_found("video_not_found"),
        MediaJobError::PreconditionFailed => InferenceError {
            status: StatusCode::CONFLICT,
            code: "video_changed",
            kind: "conflict_error",
            message: "The video job changed; retry the request.".into(),
            retry_after: None,
        },
        MediaJobError::UpstreamIdentityConflict => {
            InferenceError::unavailable("media_job_upstream_identity_conflict")
        }
        MediaJobError::Invalid(message) => InferenceError::invalid_request(message),
        MediaJobError::Database(_) => InferenceError::unavailable("persistence_unavailable"),
    }
}

fn media_job_state(status: &olp_domain::VideoStatus) -> Result<MediaJobState, InferenceError> {
    match status {
        olp_domain::VideoStatus::Queued => Ok(MediaJobState::Queued),
        olp_domain::VideoStatus::InProgress => Ok(MediaJobState::Running),
        olp_domain::VideoStatus::Completed => Ok(MediaJobState::Succeeded),
        olp_domain::VideoStatus::Failed => Ok(MediaJobState::Failed),
        olp_domain::VideoStatus::Other(status) => Err(InferenceError::bad_gateway(
            "provider_protocol_error",
            format!("The provider returned an unsupported video status: {status}."),
        )),
    }
}

fn media_job_result(record: &MediaJobRecord) -> olp_domain::VideoJobResult {
    let status = match record.state {
        MediaJobState::Queued => olp_domain::VideoStatus::Queued,
        MediaJobState::Running => olp_domain::VideoStatus::InProgress,
        MediaJobState::Succeeded => olp_domain::VideoStatus::Completed,
        MediaJobState::Failed => olp_domain::VideoStatus::Failed,
        MediaJobState::Cancelled => olp_domain::VideoStatus::Other("cancelled".into()),
    };
    olp_domain::VideoJobResult {
        id: record.id.to_string(),
        model: Some(record.route_slug.clone()),
        status,
        progress_percent: record.progress_percent,
        created_at: Some(record.created_at.timestamp()),
        completed_at: record.completed_at.map(|value| value.timestamp()),
        expires_at: record.expires_at.map(|value| value.timestamp()),
        prompt: None,
        seconds: None,
        size: None,
        error: None,
        extensions: olp_domain::SourceExtensions::new(Surface::OpenAi, BTreeMap::new()),
    }
}

struct MultipartFormData {
    text: BTreeMap<String, Vec<String>>,
    files: BTreeMap<String, Vec<BoundedMediaPart>>,
    cleanup_spool: Arc<dyn MediaSpool>,
    cleanup_handles: Vec<MediaHandle>,
    cleanup_armed: bool,
    // The parser reservation stays attached to the staged media until it is
    // either handed to request execution or deleted. This prevents a failed
    // validation or cancelled request from freeing fixed upload capacity
    // while its temporary files still consume spool space.
    cleanup_admission: Option<MultipartRequestAdmission>,
}

impl MultipartFormData {
    fn new(
        cleanup_spool: Arc<dyn MediaSpool>,
        cleanup_admission: MultipartRequestAdmission,
    ) -> Self {
        Self {
            text: BTreeMap::new(),
            files: BTreeMap::new(),
            cleanup_spool,
            cleanup_handles: Vec::new(),
            cleanup_armed: true,
            cleanup_admission: Some(cleanup_admission),
        }
    }

    fn disarm_cleanup(&mut self) {
        self.cleanup_armed = false;
        // Execution now owns every request-media handle. Its reservation no
        // longer needs to cover parser cleanup.
        if let Some(admission) = self.cleanup_admission.take() {
            admission.release();
        }
    }

    /// Remove staged request media before returning a parser failure. This is
    /// deliberately cancellation-safe: a handle remains in the vector until
    /// its removal attempt returns, so `Drop` can retry any work interrupted
    /// by request cancellation.
    async fn cleanup(&mut self) {
        if !self.cleanup_armed {
            if let Some(admission) = self.cleanup_admission.take() {
                admission.release();
            }
            return;
        }
        while let Some(handle) = self.cleanup_handles.last().cloned() {
            match self.cleanup_spool.remove(&handle).await {
                Ok(()) | Err(olp_domain::MediaSpoolError::NotFound) => {
                    self.cleanup_handles.pop();
                }
                Err(_) => {
                    // Leave the handle and reservation armed. `Drop` will
                    // schedule a final best-effort deletion while retaining
                    // capacity until that task completes.
                    return;
                }
            }
        }
        self.cleanup_armed = false;
        if let Some(admission) = self.cleanup_admission.take() {
            admission.release();
        }
    }

    fn required(&mut self, name: &str) -> Result<String, InferenceError> {
        self.optional(name)?.ok_or_else(|| {
            InferenceError::invalid_request(format!("The {name} field is required."))
        })
    }

    fn optional(&mut self, name: &str) -> Result<Option<String>, InferenceError> {
        let Some(mut values) = self.text.remove(name) else {
            return Ok(None);
        };
        if values.len() != 1 {
            return Err(InferenceError::invalid_request(format!(
                "The {name} field must appear at most once."
            )));
        }
        Ok(values.pop())
    }

    fn optional_parse<T>(&mut self, name: &str) -> Result<Option<T>, InferenceError>
    where
        T: std::str::FromStr,
    {
        self.optional(name)?
            .map(|value| {
                value.parse().map_err(|_| {
                    InferenceError::invalid_request(format!("The {name} field is invalid."))
                })
            })
            .transpose()
    }

    fn take_repeated(&mut self, name: &str) -> Vec<String> {
        self.text
            .remove(name)
            .or_else(|| self.text.remove(&format!("{name}[]")))
            .unwrap_or_default()
    }

    fn take_single_file(&mut self, name: &str) -> Result<Option<BoundedMediaPart>, InferenceError> {
        let Some(mut values) = self.files.remove(name) else {
            return Ok(None);
        };
        if values.len() != 1 {
            return Err(InferenceError::invalid_request(format!(
                "The {name} file must appear at most once."
            )));
        }
        Ok(values.pop())
    }

    fn take_files_with_prefix(&mut self, prefix: &str) -> Vec<BoundedMediaPart> {
        let keys = self
            .files
            .keys()
            .filter(|name| *name == prefix || name.starts_with(&format!("{prefix}[")))
            .cloned()
            .collect::<Vec<_>>();
        keys.into_iter()
            .flat_map(|name| self.files.remove(&name).unwrap_or_default())
            .collect()
    }

    fn take_extensions(&mut self) -> Result<BTreeMap<String, Value>, InferenceError> {
        if !self.files.is_empty() {
            return Err(InferenceError::invalid_request(
                "The multipart request contains an unsupported file field.",
            ));
        }
        std::mem::take(&mut self.text)
            .into_iter()
            .map(|(name, values)| {
                if values.len() != 1 {
                    return Err(InferenceError::invalid_request(format!(
                        "The unsupported {name} field cannot be repeated."
                    )));
                }
                Ok((
                    name,
                    Value::String(values.into_iter().next().unwrap_or_default()),
                ))
            })
            .collect()
    }
}

impl Drop for MultipartFormData {
    fn drop(&mut self) {
        if !self.cleanup_armed || self.cleanup_handles.is_empty() {
            return;
        }
        let spool = Arc::clone(&self.cleanup_spool);
        let handles = std::mem::take(&mut self.cleanup_handles);
        // Move the final lease owner into the detached cleanup task. On
        // cancellation, request-owned copies of the extension can disappear
        // immediately, but the semaphore reservation remains until these
        // staged artifacts have had their deletion attempts.
        let admission = self.cleanup_admission.take();
        if let Ok(runtime) = tokio::runtime::Handle::try_current() {
            runtime.spawn(async move {
                for handle in handles {
                    let _ = spool.remove(&handle).await;
                }
                if let Some(admission) = admission {
                    admission.release();
                }
            });
        }
    }
}

const MULTIPART_TOTAL_DEADLINE: Duration = Duration::from_secs(5 * 60);
const MAX_MULTIPART_TEXT_FIELD_BYTES: usize = 64 * 1024;
const MAX_MULTIPART_TEXT_TOTAL_BYTES: usize = 512 * 1024;

async fn parse_multipart(
    state: &ApiState,
    multipart: Multipart,
    maximum_file_bytes: u64,
    maximum_files: usize,
    admission: MultipartRequestAdmission,
) -> Result<MultipartFormData, InferenceError> {
    // This deadline deliberately covers the entire parser lifetime. The
    // existing request-body timeout protects stalled reads; without this
    // non-resetting cap, a peer that continues to trickle valid frames could
    // occupy an admission reservation indefinitely.
    // Keep ownership of the cleanup guard outside the timed parser future.
    // That lets timeout and parser-error paths synchronously remove any
    // completed staged files before their fixed admission reservation is
    // released back to another untrusted upload. On success it transfers the
    // reservation to the form, where it remains until execution takes the
    // media or cleanup finishes.
    let route_admission = admission.route.clone();
    let mut output = MultipartFormData::new(state.media_spool.clone(), admission);
    let result = tokio::time::timeout(
        MULTIPART_TOTAL_DEADLINE,
        parse_multipart_fields(
            state,
            multipart,
            maximum_file_bytes,
            maximum_files,
            &route_admission,
            &mut output,
        ),
    )
    .await;
    match result {
        Ok(Ok(())) => Ok(output),
        Ok(Err(error)) => {
            output.cleanup().await;
            Err(error)
        }
        Err(_) => {
            output.cleanup().await;
            Err(InferenceError::multipart_parser_timeout())
        }
    }
}

async fn parse_multipart_fields(
    state: &ApiState,
    mut multipart: Multipart,
    maximum_file_bytes: u64,
    maximum_files: usize,
    admission: &MultipartRouteAdmission,
    output: &mut MultipartFormData,
) -> Result<(), InferenceError> {
    let mut field_count = 0_usize;
    let mut file_count = 0_usize;
    let mut text_bytes = 0_usize;
    let mut authorized_model_seen = false;
    while let Some(mut field) = multipart.next_field().await.map_err(|error| {
        InferenceError::invalid_request(format!("The multipart request is invalid: {error}"))
    })? {
        field_count = field_count.saturating_add(1);
        if field_count > 128 {
            return Err(InferenceError::invalid_request(
                "The multipart request contains too many fields.",
            ));
        }
        let name = field
            .name()
            .filter(|name| !name.is_empty())
            .ok_or_else(|| InferenceError::invalid_request("A multipart field has no name."))?
            .to_owned();
        if let Some(filename) = field.file_name().map(str::to_owned) {
            if admission.requires_model_before_file() && !authorized_model_seen {
                return Err(InferenceError::invalid_request(
                    "A route-restricted multipart request must send model before any file part.",
                ));
            }
            file_count = file_count.saturating_add(1);
            if file_count > maximum_files {
                return Err(InferenceError::invalid_request(
                    "The multipart request contains too many files.",
                ));
            }
            let content_type = field.content_type().map(str::to_owned);
            let (sender, receiver) = tokio::sync::mpsc::channel(8);
            let stream = stream::unfold(receiver, |mut receiver| async move {
                receiver.recv().await.map(|item| (item, receiver))
            });
            let put = state.media_spool.put(olp_domain::MediaUpload {
                filename: filename.clone(),
                content_type: content_type.clone(),
                maximum_length: maximum_file_bytes,
                bytes: Box::pin(stream),
            });
            let produce = async move {
                while let Some(chunk) = field.chunk().await.transpose() {
                    match chunk {
                        Ok(chunk) => {
                            if sender.send(Ok(chunk)).await.is_err() {
                                return Ok::<(), InferenceError>(());
                            }
                        }
                        Err(error) => {
                            let _ = sender
                                .send(Err(olp_domain::MediaSpoolError::Unavailable))
                                .await;
                            return Err(InferenceError::invalid_request(format!(
                                "The multipart file is invalid: {error}"
                            )));
                        }
                    }
                }
                Ok(())
            };
            let (artifact, produced) = tokio::join!(put, produce);
            let artifact = match (artifact, produced) {
                (Ok(artifact), Ok(())) => artifact,
                (Ok(artifact), Err(error)) => {
                    // The spool may have completed while the producer noticed
                    // malformed input. Register it before returning so the
                    // outer parser cleanup (and cancellation-safe `Drop`)
                    // owns it even if this request is aborted immediately.
                    output.cleanup_handles.push(artifact.handle);
                    return Err(error);
                }
                // A malformed multipart body is a client error even if the
                // spool was told to stop by the producer. Prefer that
                // original parser error over the expected receiver-side
                // `Unavailable` result from `put`.
                (Err(_), Err(error)) => return Err(error),
                (Err(error), Ok(())) => return Err(media_spool_error(error)),
            };
            output.cleanup_handles.push(artifact.handle.clone());
            let part = BoundedMediaPart::new(
                artifact.handle,
                filename,
                content_type,
                artifact.content_length.unwrap_or_default(),
                maximum_file_bytes,
            )
            .map_err(|error| InferenceError::invalid_request(error.to_string()))?;
            output.files.entry(name).or_default().push(part);
        } else {
            // Match multer's documented `text()` behavior (charset selection,
            // BOM sniffing, replacement of malformed sequences), but bound
            // the raw bytes before growing an allocation.
            let charset = field
                .content_type()
                .and_then(|content_type| content_type.parse::<mime::Mime>().ok())
                .and_then(|content_type| {
                    content_type
                        .get_param("charset")
                        .map(|value| value.as_str().to_owned())
                });
            let mut bytes = Vec::new();
            while let Some(chunk) = field.chunk().await.map_err(|error| {
                InferenceError::invalid_request(format!("The multipart field is invalid: {error}"))
            })? {
                let next_field = bytes
                    .len()
                    .checked_add(chunk.len())
                    .filter(|length| *length <= MAX_MULTIPART_TEXT_FIELD_BYTES)
                    .ok_or_else(|| {
                        InferenceError::invalid_request("A multipart text field exceeded 64 KiB.")
                    })?;
                let next_total = text_bytes
                    .checked_add(chunk.len())
                    .filter(|length| *length <= MAX_MULTIPART_TEXT_TOTAL_BYTES)
                    .ok_or_else(|| {
                        InferenceError::invalid_request(
                            "Multipart text fields exceeded the 512 KiB aggregate limit.",
                        )
                    })?;
                bytes.try_reserve(chunk.len()).map_err(|_| {
                    InferenceError::unavailable("multipart_text_allocation_unavailable")
                })?;
                bytes.extend_from_slice(&chunk);
                debug_assert_eq!(bytes.len(), next_field);
                text_bytes = next_total;
            }
            let encoding = charset
                .as_deref()
                .and_then(|label| Encoding::for_label(label.as_bytes()))
                .unwrap_or(UTF_8);
            let text = encoding.decode(&bytes).0.into_owned();
            if name == "model" {
                match admission {
                    MultipartRouteAdmission::Expected(expected) if text != expected.as_str() => {
                        return Err(InferenceError::invalid_request(
                            "X-OLP-Route must match the multipart model field.",
                        ));
                    }
                    MultipartRouteAdmission::RequireModelBeforeFile(allowed_routes) => {
                        let route = RouteSlug::parse(text.as_str()).map_err(|_| {
                            InferenceError::invalid_request(
                                "The model field must contain a valid authorized route before file parts.",
                            )
                        })?;
                        if !allowed_routes.contains(&route) {
                            return Err(InferenceError::forbidden(
                                "The API key is not authorized for the multipart model route."
                                    .to_owned(),
                            ));
                        }
                        authorized_model_seen = true;
                    }
                    MultipartRouteAdmission::Expected(_)
                    | MultipartRouteAdmission::Unrestricted => {
                        authorized_model_seen = true;
                    }
                }
            }
            output.text.entry(name).or_default().push(text);
        }
    }
    Ok(())
}

fn valid_json<T>(payload: Result<Json<T>, JsonRejection>) -> Result<Json<T>, InferenceError> {
    payload.map_err(|error| {
        InferenceError::invalid_request(format!("The JSON request is invalid: {error}"))
    })
}

fn incompatible_result(operation: &'static str) -> InferenceError {
    InferenceError::bad_gateway(
        "provider_protocol_error",
        format!("The provider returned an incompatible {operation} response."),
    )
}

fn media_spool_error(error: olp_domain::MediaSpoolError) -> InferenceError {
    match error {
        olp_domain::MediaSpoolError::TooLarge { .. } => {
            InferenceError::payload_too_large("media_too_large")
        }
        olp_domain::MediaSpoolError::InvalidFilename
        | olp_domain::MediaSpoolError::InvalidHandle
        | olp_domain::MediaSpoolError::ZeroLimit => {
            InferenceError::invalid_request(error.to_string())
        }
        olp_domain::MediaSpoolError::NotFound | olp_domain::MediaSpoolError::Unavailable => {
            InferenceError::unavailable("media_spool_unavailable")
        }
    }
}

async fn open_response_media(
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

async fn chat_completions(
    State(state): State<ApiState>,
    headers: HeaderMap,
    payload: Result<Json<ChatCompletionRequest>, JsonRejection>,
) -> Result<Response, InferenceError> {
    let plaintext_key = bearer_token(&headers)?;
    let AuthenticatedProxyKey {
        runtime: snapshot,
        key,
        lookup_id,
    } = authenticate_proxy_key(&state, plaintext_key)?;
    let request_id = RequestId::new();
    let request_started_at = Utc::now();
    let request_started = tokio::time::Instant::now();
    let invalid_route = RouteSlug::parse("invalid-request")
        .expect("the internal invalid-request route slug is valid");

    let Json(mut wire_request) = match payload {
        Ok(payload) => payload,
        Err(error) => {
            let failure =
                InferenceError::invalid_request(format!("The JSON request is invalid: {error}"));
            emit_request_event(
                &state,
                snapshot.generation.id.as_uuid(),
                key.id.as_uuid(),
                request_id.as_uuid(),
                &invalid_route,
                &[],
                request_started_at,
                request_started,
                None,
                None,
                Some(failure.status.as_u16()),
                Some(failure.code.to_owned()),
                false,
                &UsageCapture::default(),
                Surface::OpenAi,
                "generation",
            );
            return Err(failure);
        }
    };
    let admitted = admit_openai_chat(&state, &mut wire_request).await?;
    let streaming = wire_request.stream;
    let operation = match decode_chat_completion(wire_request) {
        Ok(operation) => operation,
        Err(error) => {
            cleanup_admitted(&state, admitted).await;
            let failure = InferenceError::invalid_request(error.to_string());
            emit_request_event(
                &state,
                snapshot.generation.id.as_uuid(),
                key.id.as_uuid(),
                request_id.as_uuid(),
                &invalid_route,
                &[],
                request_started_at,
                request_started,
                None,
                None,
                Some(failure.status.as_u16()),
                Some(failure.code.to_owned()),
                false,
                &UsageCapture::default(),
                Surface::OpenAi,
                "generation",
            );
            return Err(failure);
        }
    };
    let request_media = RequestMediaGuard::new(
        state.media_spool.clone(),
        operation_media_handles(&operation),
    );
    let route_slug = operation
        .route()
        .cloned()
        .ok_or_else(|| InferenceError::invalid_request("A route model is required."))?;
    if let Err(error) = authorize_api_key(&key, Some(&route_slug), operation.kind(), Utc::now()) {
        let failure = InferenceError::forbidden(error.to_string());
        emit_request_event(
            &state,
            snapshot.generation.id.as_uuid(),
            key.id.as_uuid(),
            request_id.as_uuid(),
            &route_slug,
            &[],
            request_started_at,
            request_started,
            None,
            None,
            Some(failure.status.as_u16()),
            Some(failure.code.to_owned()),
            false,
            &UsageCapture::default(),
            Surface::OpenAi,
            "generation",
        );
        return Err(failure);
    }
    let mode = if streaming {
        TransportMode::Streaming
    } else {
        TransportMode::Unary
    };
    let lease = reserve_limits(
        &state,
        &key,
        &operation,
        lookup_id.as_str(),
        snapshot
            .routes
            .get(&route_slug)
            .map(|route| route.overall_timeout.as_duration())
            .unwrap_or(Duration::from_secs(30))
            .saturating_add(Duration::from_secs(30)),
    )
    .await?;
    let attempts = match select_representable_attempts_filtered(
        &snapshot,
        &route_slug,
        &operation,
        Surface::OpenAi,
        mode,
        request_id.as_uuid().as_bytes(),
        |_, target| state.circuits.is_selectable(target.id),
    ) {
        Ok(attempts) => attempts,
        Err(failure) => {
            emit_request_event(
                &state,
                snapshot.generation.id.as_uuid(),
                key.id.as_uuid(),
                request_id.as_uuid(),
                &route_slug,
                &[],
                request_started_at,
                request_started,
                None,
                None,
                Some(failure.status.as_u16()),
                Some(failure.code.to_owned()),
                false,
                &UsageCapture::default(),
                Surface::OpenAi,
                "generation",
            );
            release_limits(&state, lease.as_ref()).await;
            return Err(failure);
        }
    };
    let route = snapshot
        .routes
        .get(&route_slug)
        .expect("attempt selection returned a known route");

    let metadata = RequestMetadata {
        request_id,
        operation: OperationKind::Generation,
        surface: Surface::OpenAi,
        mode,
    };
    let execution = execute_with_failover(
        &snapshot,
        attempts,
        metadata,
        operation,
        route.overall_timeout.as_duration(),
        state.media_spool.clone(),
        &state.circuits,
    )
    .await;
    request_media.cleanup().await;
    let ExecutionSuccess {
        output,
        deadline,
        attempts,
        attempt_started,
    } = match execution {
        Ok(execution) => execution,
        Err(failure) => {
            let status = failure.error.status.as_u16();
            let code = failure.error.code.to_owned();
            emit_request_event(
                &state,
                snapshot.generation.id.as_uuid(),
                key.id.as_uuid(),
                request_id.as_uuid(),
                &route_slug,
                &failure.attempts,
                request_started_at,
                request_started,
                None,
                None,
                Some(status),
                Some(code),
                false,
                &UsageCapture::default(),
                Surface::OpenAi,
                "generation",
            );
            release_limits(&state, lease.as_ref()).await;
            return Err(failure.error);
        }
    };
    let first_byte_ms = elapsed_ms(request_started.elapsed());
    let ExecutionOutput::Events { first, events } = output else {
        release_limits(&state, lease.as_ref()).await;
        return Err(InferenceError::bad_gateway(
            "provider_protocol_error",
            "The provider returned an incompatible unary result.",
        ));
    };

    if streaming {
        crate::claim_http_inference_metadata();
        Ok(streaming_response(
            state,
            snapshot.generation.id.as_uuid(),
            key.id.as_uuid(),
            request_id.as_uuid(),
            route_slug,
            first,
            events,
            lease,
            deadline,
            request_started_at,
            request_started,
            first_byte_ms,
            attempts,
            attempt_started,
        ))
    } else {
        let result = unary_response(
            &state,
            snapshot.generation.id.as_uuid(),
            key.id.as_uuid(),
            request_id.as_uuid(),
            &route_slug,
            first,
            events,
            deadline,
            request_started_at,
            request_started,
            first_byte_ms,
            &attempts,
            attempt_started,
        )
        .await;
        release_limits(&state, lease.as_ref()).await;
        result
    }
}

async fn reserve_limits(
    state: &ApiState,
    key: &ApiKey,
    operation: &Operation,
    lookup_id: &str,
    lease_ttl: Duration,
) -> Result<Option<LimitLease>, InferenceError> {
    if let Some(reserved_tokens) = crate::http_inference_reserved_tokens() {
        let Some(tokens_per_minute) = key.limits.tokens_per_minute else {
            return Ok(None);
        };
        let delta = estimate_tokens(operation).saturating_sub(reserved_tokens);
        if delta <= 0 {
            return Ok(None);
        }
        let limiter = state
            .limiter
            .get()
            .ok_or_else(|| InferenceError::unavailable("distributed_limits_unavailable"))?;
        let tokens_per_minute = i64::try_from(tokens_per_minute.get())
            .map_err(|_| InferenceError::unavailable("limit_configuration_invalid"))?;
        let result = tokio::time::timeout(
            Duration::from_secs(1),
            limiter.reserve(LimitRequest {
                lookup_id,
                requests_per_minute: None,
                tokens_per_minute: Some(tokens_per_minute),
                max_concurrency: None,
                requested_tokens: delta,
                lease_ttl,
            }),
        )
        .await
        .map_err(|_| InferenceError::unavailable("distributed_limits_unavailable"))?;
        return match result {
            Ok(lease) => Ok(Some(lease)),
            Err(LimitError::Exceeded {
                dimension,
                retry_after,
            }) => Err(InferenceError::rate_limited(dimension, retry_after)),
            Err(error) => {
                error!(%error, "HTTP TPM reconciliation failed closed");
                Err(InferenceError::unavailable(
                    "distributed_limits_unavailable",
                ))
            }
        };
    }
    if !key.limits.has_hard_limits() {
        return Ok(None);
    }
    let limiter = state
        .limiter
        .get()
        .ok_or_else(|| InferenceError::unavailable("distributed_limits_unavailable"))?;
    let tokens_per_minute = key
        .limits
        .tokens_per_minute
        .map(|value| i64::try_from(value.get()))
        .transpose()
        .map_err(|_| InferenceError::unavailable("limit_configuration_invalid"))?;
    let result = tokio::time::timeout(
        Duration::from_secs(1),
        limiter.reserve(LimitRequest {
            lookup_id,
            requests_per_minute: key
                .limits
                .requests_per_minute
                .map(|value| i64::from(value.get())),
            tokens_per_minute,
            max_concurrency: key.limits.concurrency.map(|value| i64::from(value.get())),
            requested_tokens: estimate_tokens(operation),
            lease_ttl,
        }),
    )
    .await
    .map_err(|_| InferenceError::unavailable("distributed_limits_unavailable"))?;
    match result {
        Ok(lease) => Ok(Some(lease)),
        Err(LimitError::Exceeded {
            dimension,
            retry_after,
        }) => Err(InferenceError::rate_limited(dimension, retry_after)),
        Err(error) => {
            error!(%error, "hard distributed limit reservation failed closed");
            Err(InferenceError::unavailable(
                "distributed_limits_unavailable",
            ))
        }
    }
}

fn estimate_tokens(operation: &Operation) -> i64 {
    let estimate = match operation {
        Operation::Generation(request) => {
            let messages = request
                .messages
                .iter()
                .map(|message| {
                    estimated_content_tokens(&message.content)
                        .saturating_add(message.name.as_deref().map_or(0, estimated_text_tokens))
                        .saturating_add(
                            message
                                .tool_call_id
                                .as_deref()
                                .map_or(0, estimated_text_tokens),
                        )
                        .saturating_add(
                            message
                                .tool_calls
                                .iter()
                                .map(|call| {
                                    estimated_text_tokens(&call.name)
                                        .saturating_add(estimated_text_tokens(&call.arguments))
                                })
                                .sum::<usize>(),
                        )
                })
                .sum::<usize>();
            let tools = request
                .tools
                .iter()
                .map(|tool| {
                    estimated_text_tokens(&tool.name)
                        .saturating_add(
                            tool.description.as_deref().map_or(0, estimated_text_tokens),
                        )
                        .saturating_add(estimated_text_tokens(&tool.input_schema.to_string()))
                })
                .sum::<usize>();
            // Omitting the output cap must not make TPM effectively input-only.
            // 4k is a conservative portable default across launch connectors.
            let output = usize::try_from(request.parameters.max_output_tokens.unwrap_or(4_096))
                .unwrap_or(usize::MAX)
                .saturating_mul(usize::from(request.parameters.candidate_count.unwrap_or(1)));
            messages.saturating_add(tools).saturating_add(output)
        }
        Operation::Embeddings(request) => request
            .input
            .iter()
            .map(|input| match input {
                olp_domain::EmbeddingInput::Text(text) => estimated_text_tokens(text),
                olp_domain::EmbeddingInput::Tokens(tokens) => tokens.len(),
            })
            .sum(),
        Operation::TokenCount(request) => estimated_content_tokens(&request.input),
        Operation::Images(olp_domain::ImageOperation::Generation(request)) => {
            estimated_text_tokens(&request.prompt)
        }
        Operation::Images(olp_domain::ImageOperation::Edit(request)) => {
            estimated_text_tokens(&request.prompt)
                .saturating_add(request.images.len().saturating_mul(1_000))
                .saturating_add(usize::from(request.mask.is_some()) * 1_000)
        }
        Operation::Images(olp_domain::ImageOperation::Variation(_)) => 1_000,
        Operation::Speech(request) => estimated_text_tokens(&request.input),
        Operation::Transcription(request) => request.prompt.as_deref().map_or(1_500, |prompt| {
            1_500_usize.saturating_add(estimated_text_tokens(prompt))
        }),
        Operation::Video(olp_domain::VideoOperation::Create(request)) => {
            estimated_text_tokens(&request.prompt)
                .saturating_add(usize::from(request.input.is_some()) * 2_000)
        }
        Operation::Moderation(request) => estimated_content_tokens(&request.input),
        Operation::Video(_) | Operation::Models(_) => 1,
    };
    i64::try_from(estimate.max(1)).unwrap_or(i64::MAX)
}

fn estimated_text_tokens(text: &str) -> usize {
    text.chars().count().saturating_add(3) / 4
}

fn estimated_content_tokens(parts: &[olp_domain::ContentPart]) -> usize {
    parts
        .iter()
        .map(|part| match part {
            olp_domain::ContentPart::Text { text } | olp_domain::ContentPart::Refusal { text } => {
                estimated_text_tokens(text)
            }
            olp_domain::ContentPart::Image { .. } => 1_000,
            olp_domain::ContentPart::InputAudio { .. } => 2_000,
            olp_domain::ContentPart::InputFile { .. } => 2_000,
        })
        .sum()
}

pub(crate) async fn release_limits(state: &ApiState, lease: Option<&LimitLease>) {
    if let (Some(limiter), Some(lease)) = (state.limiter.get(), lease) {
        match tokio::time::timeout(Duration::from_millis(250), limiter.release(lease)).await {
            Ok(Ok(())) => {}
            Ok(Err(error)) => warn!(%error, "failed to release concurrency lease"),
            Err(_) => warn!("timed out releasing concurrency lease"),
        }
    }
}

fn operation_media_handles(operation: &Operation) -> Vec<MediaHandle> {
    let mut handles = Vec::new();
    match operation {
        Operation::Generation(request) => {
            for message in &request.messages {
                capture_content_handles(&message.content, &mut handles);
            }
        }
        Operation::TokenCount(request) => capture_content_handles(&request.input, &mut handles),
        Operation::Images(olp_domain::ImageOperation::Edit(request)) => {
            handles.extend(request.images.iter().cloned());
            handles.extend(request.mask.iter().cloned());
        }
        Operation::Images(olp_domain::ImageOperation::Variation(request)) => {
            handles.push(request.image.clone());
        }
        Operation::Transcription(request) => handles.push(request.audio.clone()),
        Operation::Video(olp_domain::VideoOperation::Create(request)) => {
            handles.extend(request.input.iter().cloned());
        }
        Operation::Moderation(request) => capture_content_handles(&request.input, &mut handles),
        Operation::Embeddings(_)
        | Operation::Images(olp_domain::ImageOperation::Generation(_))
        | Operation::Speech(_)
        | Operation::Video(_)
        | Operation::Models(_) => {}
    }
    handles.sort_by(|left, right| left.as_str().cmp(right.as_str()));
    handles.dedup_by(|left, right| left.as_str() == right.as_str());
    handles
}

fn capture_content_handles(parts: &[olp_domain::ContentPart], handles: &mut Vec<MediaHandle>) {
    for part in parts {
        match part {
            olp_domain::ContentPart::Image {
                source: olp_domain::MediaSource::Handle(handle),
                ..
            }
            | olp_domain::ContentPart::InputAudio { media: handle, .. }
            | olp_domain::ContentPart::InputFile { media: handle, .. } => {
                handles.push(handle.clone());
            }
            _ => {}
        }
    }
}

async fn cleanup_request_media(spool: &Arc<dyn MediaSpool>, handles: Vec<MediaHandle>) {
    for handle in handles {
        match spool.remove(&handle).await {
            Ok(()) | Err(olp_domain::MediaSpoolError::NotFound) => {}
            Err(error) => warn!(%error, "failed to remove request media from the bounded spool"),
        }
    }
}

struct RequestMediaGuard {
    spool: Arc<dyn MediaSpool>,
    handles: Vec<MediaHandle>,
}

impl RequestMediaGuard {
    fn new(spool: Arc<dyn MediaSpool>, handles: Vec<MediaHandle>) -> Self {
        Self { spool, handles }
    }

    async fn cleanup(mut self) {
        if self.handles.is_empty() {
            return;
        }
        let spool = self.spool.clone();
        let handles = std::mem::take(&mut self.handles);
        let cleanup = tokio::spawn(async move {
            cleanup_request_media(&spool, handles).await;
        });
        let _ = cleanup.await;
    }
}

impl Drop for RequestMediaGuard {
    fn drop(&mut self) {
        if self.handles.is_empty() {
            return;
        }
        let spool = self.spool.clone();
        let handles = std::mem::take(&mut self.handles);
        if let Ok(runtime) = tokio::runtime::Handle::try_current() {
            runtime.spawn(async move {
                cleanup_request_media(&spool, handles).await;
            });
        }
    }
}

pub(crate) struct RoutedUnaryResult {
    pub result: Box<CanonicalResult>,
    pub request_id: RequestId,
    pub api_key_id: uuid::Uuid,
    pub route_slug: RouteSlug,
    pub provider_id: uuid::Uuid,
    pub provider_model: String,
    completion: Option<UnaryExecutionCompletion>,
}

struct UnaryExecutionCompletion {
    state: ApiState,
    generation_id: uuid::Uuid,
    api_key_id: uuid::Uuid,
    request_id: uuid::Uuid,
    route_slug: RouteSlug,
    attempts: Vec<UsageAttempt>,
    request_started_at: chrono::DateTime<Utc>,
    request_started: tokio::time::Instant,
    attempt_started: tokio::time::Instant,
    first_byte_ms: u64,
    usage: UsageCapture,
    surface: Surface,
    operation: &'static str,
}

impl UnaryExecutionCompletion {
    fn emit(self, failure: Option<&InferenceError>) {
        emit_request_event(
            &self.state,
            self.generation_id,
            self.api_key_id,
            self.request_id,
            &self.route_slug,
            &self.attempts,
            self.request_started_at,
            self.request_started,
            Some(self.attempt_started),
            Some(self.first_byte_ms),
            Some(
                failure
                    .map_or(StatusCode::OK, |error| error.status)
                    .as_u16(),
            ),
            failure.map(|error| error.code.to_owned()),
            true,
            &self.usage,
            self.surface,
            self.operation,
        );
    }
}

impl RoutedUnaryResult {
    pub(crate) fn mark_success(&mut self) {
        if let Some(completion) = self.completion.take() {
            completion.emit(None);
        }
    }

    pub(crate) fn mark_failure(&mut self, failure: &InferenceError) {
        if let Some(completion) = self.completion.take() {
            completion.emit(Some(failure));
        }
    }

    fn mark_outcome<T>(&mut self, outcome: &Result<T, InferenceError>) {
        match outcome {
            Ok(_) => self.mark_success(),
            Err(failure) => self.mark_failure(failure),
        }
    }
}

impl Drop for RoutedUnaryResult {
    fn drop(&mut self) {
        let Some(completion) = self.completion.take() else {
            return;
        };
        let failure = InferenceError::bad_gateway(
            "provider_protocol_error",
            "The provider result was not representable on the client protocol.",
        );
        completion.emit(Some(&failure));
    }
}

async fn execute_unary_result(
    state: &ApiState,
    headers: &HeaderMap,
    operation: Operation,
) -> Result<RoutedUnaryResult, InferenceError> {
    execute_routed_result(state, headers, operation, TransportMode::Unary, None).await
}

async fn execute_routed_result(
    state: &ApiState,
    headers: &HeaderMap,
    operation: Operation,
    mode: TransportMode,
    required_target: Option<RequiredTarget>,
) -> Result<RoutedUnaryResult, InferenceError> {
    let plaintext_key = bearer_token(headers)?;
    execute_routed_result_for_surface(
        state,
        plaintext_key,
        operation,
        Surface::OpenAi,
        mode,
        required_target,
    )
    .await
}

pub(crate) async fn execute_routed_result_for_surface(
    state: &ApiState,
    plaintext_key: &str,
    operation: Operation,
    surface: Surface,
    mode: TransportMode,
    required_target: Option<RequiredTarget>,
) -> Result<RoutedUnaryResult, InferenceError> {
    let request_media = RequestMediaGuard::new(
        state.media_spool.clone(),
        operation_media_handles(&operation),
    );
    let result = execute_routed_result_for_surface_inner(
        state,
        plaintext_key,
        operation,
        surface,
        mode,
        required_target,
    )
    .await;
    request_media.cleanup().await;
    result
}

async fn execute_routed_result_for_surface_inner(
    state: &ApiState,
    plaintext_key: &str,
    operation: Operation,
    surface: Surface,
    mode: TransportMode,
    required_target: Option<RequiredTarget>,
) -> Result<RoutedUnaryResult, InferenceError> {
    let AuthenticatedProxyKey {
        runtime: snapshot,
        key,
        lookup_id,
    } = authenticate_proxy_key(state, plaintext_key)?;
    let route_slug = operation
        .route()
        .cloned()
        .ok_or_else(|| InferenceError::invalid_request("A route model is required."))?;
    let operation_kind = operation.kind();
    authorize_api_key(&key, Some(&route_slug), operation_kind, Utc::now())
        .map_err(|error| InferenceError::forbidden(error.to_string()))?;
    let request_id = RequestId::new();
    let request_started_at = Utc::now();
    let request_started = tokio::time::Instant::now();
    let lease = reserve_limits(
        state,
        &key,
        &operation,
        lookup_id.as_str(),
        snapshot
            .routes
            .get(&route_slug)
            .map(|route| route.overall_timeout.as_duration())
            .unwrap_or(Duration::from_secs(30))
            .saturating_add(Duration::from_secs(30)),
    )
    .await?;
    let attempts = match select_representable_attempts_filtered(
        &snapshot,
        &route_slug,
        &operation,
        surface,
        mode,
        request_id.as_uuid().as_bytes(),
        |_, target| {
            state.circuits.is_selectable(target.id)
                && required_target.as_ref().is_none_or(|required| {
                    target.provider_id.as_uuid() == required.provider_id
                        && target.provider_model == required.provider_model
                })
        },
    ) {
        Ok(attempts) => attempts,
        Err(error) => {
            let failure = if required_target.is_some() && error.code == "no_eligible_provider" {
                InferenceError::unavailable("media_job_target_unavailable")
            } else {
                error
            };
            emit_request_event(
                state,
                snapshot.generation.id.as_uuid(),
                key.id.as_uuid(),
                request_id.as_uuid(),
                &route_slug,
                &[],
                request_started_at,
                request_started,
                None,
                None,
                Some(failure.status.as_u16()),
                Some(failure.code.to_owned()),
                false,
                &UsageCapture::default(),
                surface,
                operation_kind.as_str(),
            );
            release_limits(state, lease.as_ref()).await;
            return Err(failure);
        }
    };
    let route = snapshot
        .routes
        .get(&route_slug)
        .expect("attempt selection returned a known route");
    let execution = execute_with_failover(
        &snapshot,
        attempts,
        RequestMetadata {
            request_id,
            operation: operation_kind,
            surface,
            mode,
        },
        operation,
        route.overall_timeout.as_duration(),
        state.media_spool.clone(),
        &state.circuits,
    )
    .await;
    let success = match execution {
        Ok(success) => success,
        Err(failure) => {
            emit_request_event(
                state,
                snapshot.generation.id.as_uuid(),
                key.id.as_uuid(),
                request_id.as_uuid(),
                &route_slug,
                &failure.attempts,
                request_started_at,
                request_started,
                None,
                None,
                Some(failure.error.status.as_u16()),
                Some(failure.error.code.to_owned()),
                false,
                &UsageCapture::default(),
                surface,
                operation_kind.as_str(),
            );
            release_limits(state, lease.as_ref()).await;
            return Err(failure.error);
        }
    };
    let ExecutionOutput::Result(result) = success.output else {
        let failure = InferenceError::bad_gateway(
            "provider_protocol_error",
            "The provider returned an event stream for a unary result operation.",
        );
        emit_request_event(
            state,
            snapshot.generation.id.as_uuid(),
            key.id.as_uuid(),
            request_id.as_uuid(),
            &route_slug,
            &success.attempts,
            request_started_at,
            request_started,
            Some(success.attempt_started),
            Some(elapsed_ms(request_started.elapsed())),
            Some(failure.status.as_u16()),
            Some(failure.code.to_owned()),
            true,
            &UsageCapture::default(),
            surface,
            operation_kind.as_str(),
        );
        release_limits(state, lease.as_ref()).await;
        return Err(failure);
    };
    let usage = usage_from_result(&result);
    let first_byte_ms = elapsed_ms(request_started.elapsed());
    release_limits(state, lease.as_ref()).await;
    let final_attempt = success
        .attempts
        .last()
        .expect("a successful execution has one provider attempt");
    crate::claim_http_inference_metadata();
    Ok(RoutedUnaryResult {
        result,
        request_id,
        api_key_id: key.id.as_uuid(),
        route_slug: route_slug.clone(),
        provider_id: final_attempt.provider_id,
        provider_model: final_attempt.upstream_model.clone(),
        completion: Some(UnaryExecutionCompletion {
            state: state.clone(),
            generation_id: snapshot.generation.id.as_uuid(),
            api_key_id: key.id.as_uuid(),
            request_id: request_id.as_uuid(),
            route_slug: route_slug.clone(),
            attempts: success.attempts,
            request_started_at,
            request_started,
            attempt_started: success.attempt_started,
            first_byte_ms,
            usage,
            surface,
            operation: operation_kind.as_str(),
        }),
    })
}

fn usage_from_result(result: &CanonicalResult) -> UsageCapture {
    let (usage, media_units) = match result {
        CanonicalResult::Embeddings(result) => (result.usage, None),
        CanonicalResult::Images(result) => (result.usage, Decimal::from_usize(result.images.len())),
        CanonicalResult::Transcription(result) => (
            None,
            result.duration_seconds.and_then(Decimal::from_f64_retain),
        ),
        CanonicalResult::VideoJob(result) => (
            None,
            result
                .seconds
                .as_deref()
                .and_then(|value| value.parse::<Decimal>().ok()),
        ),
        CanonicalResult::TokenCount(result) => (
            Some(olp_domain::Usage {
                input_tokens: result.input_tokens,
                output_tokens: 0,
                total_tokens: result.input_tokens,
                cached_input_tokens: None,
                reasoning_tokens: None,
            }),
            None,
        ),
        _ => (None, None),
    };
    if usage.is_none() && media_units.is_none() {
        return UsageCapture::default();
    }
    let (input_tokens, output_tokens, cached_input_tokens, token_complete) =
        usage.map_or((None, None, None, true), |usage| {
            let input = i64::try_from(usage.input_tokens).ok();
            let output = i64::try_from(usage.output_tokens).ok();
            let cached = usage
                .cached_input_tokens
                .and_then(|value| i64::try_from(value).ok());
            let complete = input.is_some()
                && output.is_some()
                && (usage.cached_input_tokens.is_none() || cached.is_some());
            (input, output, cached, complete)
        });
    UsageCapture {
        observed: true,
        complete: token_complete,
        input_tokens,
        output_tokens,
        cached_input_tokens,
        media_units,
    }
}

type EventStream = olp_domain::ProviderEventStream;

fn canonical_event_protocol_error(
    error: EventSequenceError,
    response_committed: bool,
) -> TransportError {
    TransportError {
        phase: olp_domain::TransportPhase::Body,
        class: AttemptFailureClass::Protocol,
        response_committed,
        message: format!("invalid canonical event stream: {error}"),
    }
}

fn validated_event_stream(events: EventStream, validator: EventSequenceValidator) -> EventStream {
    Box::pin(stream::unfold(
        (events, validator, false),
        |(mut events, mut validator, terminal)| async move {
            if terminal || validator.is_complete() {
                return None;
            }
            match events.next().await {
                Some(Ok(event)) => match validator.push(&event) {
                    Ok(()) => Some((Ok(event), (events, validator, false))),
                    Err(error) => Some((
                        Err(canonical_event_protocol_error(error, true)),
                        (events, validator, true),
                    )),
                },
                Some(Err(error)) => Some((Err(error), (events, validator, true))),
                None => validator.finish().err().map(|error| {
                    (
                        Err(canonical_event_protocol_error(error, true)),
                        (events, validator, true),
                    )
                }),
            }
        },
    ))
}

fn circuit_accounted_event_stream(
    events: EventStream,
    circuits: crate::circuit::CircuitBreaker,
    target: TargetId,
    initial_failure: bool,
) -> EventStream {
    Box::pin(stream::unfold(
        (events, circuits, initial_failure),
        move |(mut events, circuits, mut failed)| async move {
            let item = events.next().await?;
            let item = match item {
                Ok(event) => {
                    match &event.kind {
                        CanonicalEventKind::Error { error } => {
                            if let Some(class) = canonical_error_circuit_class(error.class) {
                                circuits.record_failure(target, class);
                            }
                            failed = true;
                        }
                        CanonicalEventKind::Done if !failed => circuits.record_success(target),
                        _ => {}
                    }
                    Ok(event)
                }
                Err(mut error) => {
                    // A provider stream has already committed once this wrapper
                    // owns it. Terminal transport failures still affect target
                    // health, but must never trigger request failover.
                    error.response_committed = true;
                    circuits.record_failure(target, error.class);
                    failed = true;
                    Err(error)
                }
            };
            Some((item, (events, circuits, failed)))
        },
    ))
}

const fn canonical_error_circuit_class(class: ErrorClass) -> Option<AttemptFailureClass> {
    match class {
        ErrorClass::RateLimit => Some(AttemptFailureClass::RateLimit),
        ErrorClass::Timeout => Some(AttemptFailureClass::Timeout),
        ErrorClass::Transport => Some(AttemptFailureClass::Connect),
        ErrorClass::Upstream => Some(AttemptFailureClass::UpstreamServer),
        ErrorClass::Authentication
        | ErrorClass::Authorization
        | ErrorClass::InvalidRequest
        | ErrorClass::Internal => None,
    }
}

struct CleanupMediaStream {
    inner: MediaByteStream,
    spool: std::sync::Arc<dyn MediaSpool>,
    handle: Option<MediaHandle>,
}

impl CleanupMediaStream {
    fn new(
        inner: MediaByteStream,
        spool: std::sync::Arc<dyn MediaSpool>,
        handle: MediaHandle,
    ) -> Self {
        Self {
            inner,
            spool,
            handle: Some(handle),
        }
    }

    fn schedule_cleanup(&mut self) {
        let Some(handle) = self.handle.take() else {
            return;
        };
        let spool = self.spool.clone();
        if let Ok(runtime) = tokio::runtime::Handle::try_current() {
            runtime.spawn(async move {
                let _ = spool.remove(&handle).await;
            });
        }
    }
}

impl futures::Stream for CleanupMediaStream {
    type Item = Result<Bytes, olp_domain::MediaSpoolError>;

    fn poll_next(
        mut self: std::pin::Pin<&mut Self>,
        context: &mut std::task::Context<'_>,
    ) -> std::task::Poll<Option<Self::Item>> {
        let next = self.inner.as_mut().poll_next(context);
        if matches!(next, std::task::Poll::Ready(None)) {
            self.schedule_cleanup();
        }
        next
    }
}

impl Drop for CleanupMediaStream {
    fn drop(&mut self) {
        self.schedule_cleanup();
    }
}

struct ExecutionSuccess {
    output: ExecutionOutput,
    deadline: tokio::time::Instant,
    attempts: Vec<UsageAttempt>,
    attempt_started: tokio::time::Instant,
}

enum ExecutionOutput {
    Events {
        first: CanonicalEvent,
        events: EventStream,
    },
    Result(Box<CanonicalResult>),
}

struct ExecutionFailure {
    error: InferenceError,
    attempts: Vec<UsageAttempt>,
}

async fn execute_with_failover(
    runtime: &crate::RuntimeBundle,
    attempts: Vec<AttemptPlan>,
    metadata: RequestMetadata,
    operation: Operation,
    overall_timeout: Duration,
    media_spool: std::sync::Arc<dyn MediaSpool>,
    circuits: &crate::circuit::CircuitBreaker,
) -> Result<ExecutionSuccess, ExecutionFailure> {
    let deadline = tokio::time::Instant::now() + overall_timeout;
    let mut last_error = None;
    let mut traces = Vec::with_capacity(attempts.len());
    for attempt in attempts {
        if !circuits.acquire(attempt.target_id) {
            continue;
        }
        let ordinal = u16::try_from(traces.len() + 1).unwrap_or(u16::MAX);
        let attempt_started_at = Utc::now();
        let attempt_started = tokio::time::Instant::now();
        let attempt_deadline = deadline.min(attempt_started + attempt.timeout.as_duration());
        let Some(transport) = runtime.transport(attempt.provider_id) else {
            let error = TransportError {
                phase: olp_domain::TransportPhase::Connect,
                class: AttemptFailureClass::Connect,
                response_committed: false,
                message: "provider transport is not loaded".to_owned(),
            };
            traces.push(failed_attempt(
                &attempt,
                ordinal,
                attempt_started_at,
                attempt_started,
                &error,
            ));
            circuits.record_failure(attempt.target_id, error.class);
            last_error = Some(error);
            continue;
        };
        let remaining = attempt_deadline.saturating_duration_since(tokio::time::Instant::now());
        if remaining.is_zero() {
            return Err(ExecutionFailure {
                error: InferenceError::timeout(),
                attempts: traces,
            });
        }
        let provider_request = ProviderRequest {
            metadata: metadata.clone(),
            attempt: attempt.clone(),
            operation: operation_for_provider(&operation, attempt.provider_kind),
            media: Some(media_spool.clone()),
        };
        let output =
            match tokio::time::timeout(remaining, transport.execute(provider_request)).await {
                Ok(Ok(events)) => events,
                Ok(Err(error)) if error.allows_failover() => {
                    traces.push(failed_attempt(
                        &attempt,
                        ordinal,
                        attempt_started_at,
                        attempt_started,
                        &error,
                    ));
                    circuits.record_failure(attempt.target_id, error.class);
                    last_error = Some(error);
                    continue;
                }
                Ok(Err(error)) => {
                    traces.push(failed_attempt(
                        &attempt,
                        ordinal,
                        attempt_started_at,
                        attempt_started,
                        &error,
                    ));
                    circuits.record_failure(attempt.target_id, error.class);
                    return Err(ExecutionFailure {
                        error: InferenceError::from_transport(error),
                        attempts: traces,
                    });
                }
                Err(_) => {
                    let ambiguous = operation_timeout_is_ambiguous(operation.kind());
                    let error = TransportError {
                        phase: olp_domain::TransportPhase::FirstByte,
                        class: if ambiguous {
                            AttemptFailureClass::Ambiguous
                        } else {
                            AttemptFailureClass::Timeout
                        },
                        response_committed: ambiguous,
                        message: "route deadline elapsed before provider response".to_owned(),
                    };
                    traces.push(failed_attempt(
                        &attempt,
                        ordinal,
                        attempt_started_at,
                        attempt_started,
                        &error,
                    ));
                    circuits.record_failure(attempt.target_id, error.class);
                    if error.allows_failover() {
                        last_error = Some(error);
                        continue;
                    }
                    return Err(ExecutionFailure {
                        error: InferenceError::from_transport(error),
                        attempts: traces,
                    });
                }
            };
        let mut events = match output {
            ProviderOutput::Events(events) => events,
            ProviderOutput::Result(result) => {
                circuits.record_success(attempt.target_id);
                traces.push(successful_attempt(
                    &attempt,
                    ordinal,
                    attempt_started_at,
                    attempt_started,
                ));
                return Ok(ExecutionSuccess {
                    output: ExecutionOutput::Result(result),
                    deadline: attempt_deadline,
                    attempts: traces,
                    attempt_started,
                });
            }
        };
        let remaining = attempt_deadline.saturating_duration_since(tokio::time::Instant::now());
        let first = match tokio::time::timeout(remaining, events.next()).await {
            Ok(Some(Ok(event))) => event,
            Ok(Some(Err(error))) if error.allows_failover() => {
                traces.push(failed_attempt(
                    &attempt,
                    ordinal,
                    attempt_started_at,
                    attempt_started,
                    &error,
                ));
                circuits.record_failure(attempt.target_id, error.class);
                last_error = Some(error);
                continue;
            }
            Ok(Some(Err(error))) => {
                traces.push(failed_attempt(
                    &attempt,
                    ordinal,
                    attempt_started_at,
                    attempt_started,
                    &error,
                ));
                circuits.record_failure(attempt.target_id, error.class);
                return Err(ExecutionFailure {
                    error: InferenceError::from_transport(error),
                    attempts: traces,
                });
            }
            Ok(None) => {
                let error = TransportError {
                    phase: olp_domain::TransportPhase::FirstByte,
                    class: AttemptFailureClass::Protocol,
                    response_committed: false,
                    message: "the provider returned an empty response".to_owned(),
                };
                traces.push(failed_attempt(
                    &attempt,
                    ordinal,
                    attempt_started_at,
                    attempt_started,
                    &error,
                ));
                circuits.record_failure(attempt.target_id, error.class);
                return Err(ExecutionFailure {
                    error: InferenceError::bad_gateway(
                        "provider_protocol_error",
                        "The provider returned an empty response.",
                    ),
                    attempts: traces,
                });
            }
            Err(_) => {
                let error = TransportError {
                    phase: olp_domain::TransportPhase::FirstByte,
                    class: AttemptFailureClass::Timeout,
                    response_committed: false,
                    message: "route deadline elapsed before a canonical event".to_owned(),
                };
                traces.push(failed_attempt(
                    &attempt,
                    ordinal,
                    attempt_started_at,
                    attempt_started,
                    &error,
                ));
                circuits.record_failure(attempt.target_id, error.class);
                last_error = Some(error);
                continue;
            }
        };
        let mut event_sequence = EventSequenceValidator::new();
        if let Err(sequence_error) = event_sequence.push(&first) {
            let error = canonical_event_protocol_error(sequence_error, false);
            traces.push(failed_attempt(
                &attempt,
                ordinal,
                attempt_started_at,
                attempt_started,
                &error,
            ));
            circuits.record_failure(attempt.target_id, error.class);
            return Err(ExecutionFailure {
                error: InferenceError::from_transport(error),
                attempts: traces,
            });
        }
        let initial_failure = if let CanonicalEventKind::Error { error } = &first.kind {
            if let Some(class) = canonical_error_circuit_class(error.class) {
                circuits.record_failure(attempt.target_id, class);
            }
            true
        } else {
            false
        };
        if matches!(first.kind, CanonicalEventKind::Done) && !initial_failure {
            circuits.record_success(attempt.target_id);
        }
        let events = circuit_accounted_event_stream(
            validated_event_stream(events, event_sequence),
            circuits.clone(),
            attempt.target_id,
            initial_failure,
        );
        traces.push(successful_attempt(
            &attempt,
            ordinal,
            attempt_started_at,
            attempt_started,
        ));
        return Ok(ExecutionSuccess {
            output: ExecutionOutput::Events { first, events },
            deadline: attempt_deadline,
            attempts: traces,
            attempt_started,
        });
    }
    Err(ExecutionFailure {
        error: last_error.map_or_else(
            || InferenceError::unavailable("no_eligible_provider"),
            InferenceError::from_transport,
        ),
        attempts: traces,
    })
}

const fn operation_timeout_is_ambiguous(operation: OperationKind) -> bool {
    matches!(
        operation,
        OperationKind::ImageGeneration
            | OperationKind::ImageEdit
            | OperationKind::ImageVariation
            | OperationKind::Speech
            | OperationKind::Transcription
            | OperationKind::VideoCreate
            | OperationKind::VideoDelete
    )
}

fn successful_attempt(
    attempt: &AttemptPlan,
    ordinal: u16,
    started_at: chrono::DateTime<Utc>,
    started: tokio::time::Instant,
) -> UsageAttempt {
    UsageAttempt {
        id: uuid::Uuid::now_v7(),
        ordinal,
        provider_id: attempt.provider_id.as_uuid(),
        upstream_model: attempt.provider_model.clone(),
        started_at,
        completed_at: Utc::now(),
        status_code: Some(StatusCode::OK.as_u16()),
        error_class: None,
        committed: true,
        latency_ms: elapsed_ms(started.elapsed()),
        first_byte_ms: Some(elapsed_ms(started.elapsed())),
    }
}

fn failed_attempt(
    attempt: &AttemptPlan,
    ordinal: u16,
    started_at: chrono::DateTime<Utc>,
    started: tokio::time::Instant,
    error: &TransportError,
) -> UsageAttempt {
    let mapped = InferenceError::from_transport(error.clone());
    UsageAttempt {
        id: uuid::Uuid::now_v7(),
        ordinal,
        provider_id: attempt.provider_id.as_uuid(),
        upstream_model: attempt.provider_model.clone(),
        started_at,
        completed_at: Utc::now(),
        status_code: Some(mapped.status.as_u16()),
        error_class: Some(attempt_failure_name(error.class).to_owned()),
        committed: error.response_committed,
        latency_ms: elapsed_ms(started.elapsed()),
        first_byte_ms: None,
    }
}

const fn attempt_failure_name(class: AttemptFailureClass) -> &'static str {
    match class {
        AttemptFailureClass::Connect => "connect",
        AttemptFailureClass::Timeout => "timeout",
        AttemptFailureClass::RateLimit => "rate_limit",
        AttemptFailureClass::UpstreamServer => "upstream_server",
        AttemptFailureClass::UpstreamClient => "upstream_client",
        AttemptFailureClass::Protocol => "protocol",
        AttemptFailureClass::Cancelled => "cancelled",
        AttemptFailureClass::Ambiguous => "ambiguous",
    }
}

#[allow(clippy::too_many_arguments)]
fn streaming_response(
    state: ApiState,
    generation_id: uuid::Uuid,
    api_key_id: uuid::Uuid,
    request_id: uuid::Uuid,
    route_slug: RouteSlug,
    first: CanonicalEvent,
    mut events: EventStream,
    lease: Option<LimitLease>,
    deadline: tokio::time::Instant,
    request_started_at: chrono::DateTime<Utc>,
    request_started: tokio::time::Instant,
    first_byte_ms: u64,
    attempts: Vec<UsageAttempt>,
    attempt_started: tokio::time::Instant,
) -> Response {
    let (writer, response) = sse_stream();
    tokio::spawn(async move {
        let mut encoder = OpenAiStreamEncoder::new(request_id, route_slug.as_str());
        let mut next = Some(Ok(first));
        let mut usage = UsageCapture::default();
        let mut failure = None;
        let mut terminal = None;
        'provider: while let Some(item) = next {
            match item {
                Ok(event) => {
                    let is_done = matches!(event.kind, CanonicalEventKind::Done);
                    usage.observe(&event);
                    let canonical_failure = match &event.kind {
                        CanonicalEventKind::Error { error } => {
                            Some(InferenceError::from_canonical(error))
                        }
                        _ => None,
                    };
                    let is_terminal = is_done || canonical_failure.is_some();
                    let encoded = match encoder.encode(event) {
                        Ok(encoded) => encoded,
                        Err(error) => {
                            failure = Some(error);
                            break 'provider;
                        }
                    };
                    if is_terminal {
                        let mut encoded = encoded;
                        if let Some(canonical_failure) = canonical_failure {
                            failure = Some(canonical_failure);
                            encoded.push(Bytes::from_static(b"data: [DONE]\n\n"));
                        }
                        terminal = Some(TerminalFrames::new(encoded));
                        break 'provider;
                    }
                    for bytes in encoded {
                        if let Err(error) = writer.send_or_fail(bytes, deadline).await {
                            failure = Some(error);
                            break 'provider;
                        }
                    }
                }
                Err(error) => {
                    failure = Some(InferenceError::from_transport(error));
                    break 'provider;
                }
            }
            next = tokio::select! {
                () = writer.closed() => {
                    failure = Some(InferenceError::client_cancelled());
                    break 'provider;
                }
                () = tokio::time::sleep_until(deadline) => {
                    failure = Some(InferenceError::timeout());
                    break 'provider;
                }
                next = events.next() => next,
            };
        }
        if terminal.is_none() && failure.is_none() {
            failure = Some(InferenceError::bad_gateway(
                "provider_protocol_error",
                "The provider stream ended without a terminal event.",
            ));
        }
        drop(events);
        writer.finish_stream(terminal, &mut failure, |error| {
            TerminalFrames::new(vec![
                openai_error_sse(error),
                Bytes::from_static(b"data: [DONE]\n\n"),
            ])
        });
        let status_code = failure
            .as_ref()
            .map_or(Some(StatusCode::OK.as_u16()), |error| {
                (error.code != "client_cancelled").then_some(error.status.as_u16())
            });
        let error_class = failure.as_ref().map(|error| error.code.to_owned());
        emit_request_event(
            &state,
            generation_id,
            api_key_id,
            request_id,
            &route_slug,
            &attempts,
            request_started_at,
            request_started,
            Some(attempt_started),
            Some(first_byte_ms),
            status_code,
            error_class,
            true,
            &usage,
            Surface::OpenAi,
            "generation",
        );
        release_limits(&state, lease.as_ref()).await;
    });
    response
}

#[allow(clippy::too_many_arguments)]
async fn unary_response(
    state: &ApiState,
    generation_id: uuid::Uuid,
    api_key_id: uuid::Uuid,
    request_id: uuid::Uuid,
    route_slug: &RouteSlug,
    first: CanonicalEvent,
    mut events: EventStream,
    deadline: tokio::time::Instant,
    request_started_at: chrono::DateTime<Utc>,
    request_started: tokio::time::Instant,
    first_byte_ms: u64,
    attempts: &[UsageAttempt],
    attempt_started: tokio::time::Instant,
) -> Result<Response, InferenceError> {
    let mut collected = vec![first];
    let mut usage = UsageCapture::default();
    usage.observe(&collected[0]);
    if !matches!(collected[0].kind, CanonicalEventKind::Done) {
        loop {
            let item = match tokio::time::timeout_at(deadline, events.next()).await {
                Ok(item) => item,
                Err(_) => {
                    let failure = InferenceError::timeout();
                    emit_request_event(
                        state,
                        generation_id,
                        api_key_id,
                        request_id,
                        route_slug,
                        attempts,
                        request_started_at,
                        request_started,
                        Some(attempt_started),
                        Some(first_byte_ms),
                        Some(failure.status.as_u16()),
                        Some(failure.code.to_owned()),
                        true,
                        &usage,
                        Surface::OpenAi,
                        "generation",
                    );
                    return Err(failure);
                }
            };
            let Some(item) = item else {
                break;
            };
            let event = match item {
                Ok(event) => event,
                Err(error) => {
                    let failure = InferenceError::from_transport(error);
                    emit_request_event(
                        state,
                        generation_id,
                        api_key_id,
                        request_id,
                        route_slug,
                        attempts,
                        request_started_at,
                        request_started,
                        Some(attempt_started),
                        Some(first_byte_ms),
                        Some(failure.status.as_u16()),
                        Some(failure.code.to_owned()),
                        true,
                        &usage,
                        Surface::OpenAi,
                        "generation",
                    );
                    return Err(failure);
                }
            };
            let terminal = matches!(event.kind, CanonicalEventKind::Done);
            usage.observe(&event);
            collected.push(event);
            if terminal {
                break;
            }
        }
    }
    if !matches!(
        collected.last().map(|event| &event.kind),
        Some(CanonicalEventKind::Done)
    ) {
        let failure = InferenceError::bad_gateway(
            "provider_protocol_error",
            "The provider response ended without a terminal event.",
        );
        emit_request_event(
            state,
            generation_id,
            api_key_id,
            request_id,
            route_slug,
            attempts,
            request_started_at,
            request_started,
            Some(attempt_started),
            Some(first_byte_ms),
            Some(failure.status.as_u16()),
            Some(failure.code.to_owned()),
            true,
            &usage,
            Surface::OpenAi,
            "generation",
        );
        return Err(failure);
    }
    let response = match aggregate_openai_response(request_id, route_slug.as_str(), &collected) {
        Ok(response) => response,
        Err(failure) => {
            emit_request_event(
                state,
                generation_id,
                api_key_id,
                request_id,
                route_slug,
                attempts,
                request_started_at,
                request_started,
                Some(attempt_started),
                Some(first_byte_ms),
                Some(failure.status.as_u16()),
                Some(failure.code.to_owned()),
                true,
                &usage,
                Surface::OpenAi,
                "generation",
            );
            return Err(failure);
        }
    };
    emit_request_event(
        state,
        generation_id,
        api_key_id,
        request_id,
        route_slug,
        attempts,
        request_started_at,
        request_started,
        Some(attempt_started),
        Some(first_byte_ms),
        Some(StatusCode::OK.as_u16()),
        None,
        true,
        &usage,
        Surface::OpenAi,
        "generation",
    );
    Ok((StatusCode::OK, Json(response)).into_response())
}

#[derive(Default)]
pub(crate) struct UsageCapture {
    observed: bool,
    complete: bool,
    input_tokens: Option<i64>,
    output_tokens: Option<i64>,
    cached_input_tokens: Option<i64>,
    media_units: Option<Decimal>,
}

impl UsageCapture {
    pub(crate) fn observe(&mut self, event: &CanonicalEvent) {
        let CanonicalEventKind::Usage { usage } = &event.kind else {
            return;
        };
        self.observed = true;
        self.input_tokens = i64::try_from(usage.input_tokens).ok();
        self.output_tokens = i64::try_from(usage.output_tokens).ok();
        self.cached_input_tokens = usage
            .cached_input_tokens
            .and_then(|value| i64::try_from(value).ok());
        self.complete = self.input_tokens.is_some()
            && self.output_tokens.is_some()
            && (usage.cached_input_tokens.is_none() || self.cached_input_tokens.is_some());
    }

    fn observe_openai_media_event(&mut self, event: &CanonicalEvent) {
        let CanonicalEventKind::SourceExtension { extensions } = &event.kind else {
            return;
        };
        if extensions.source != Some(Surface::OpenAi) {
            return;
        }
        let Some(usage) = extensions
            .values
            .get("/__olp/raw_sse/data")
            .and_then(|value| value.get("usage"))
        else {
            return;
        };
        let input = usage
            .get("input_tokens")
            .or_else(|| usage.get("prompt_tokens"))
            .and_then(Value::as_u64)
            .and_then(|value| i64::try_from(value).ok());
        let output = usage
            .get("output_tokens")
            .or_else(|| usage.get("completion_tokens"))
            .and_then(Value::as_u64)
            .and_then(|value| i64::try_from(value).ok());
        let cached = usage
            .get("input_tokens_details")
            .or_else(|| usage.get("prompt_tokens_details"))
            .and_then(|details| details.get("cached_tokens"))
            .and_then(Value::as_u64)
            .and_then(|value| i64::try_from(value).ok());
        if input.is_none() && output.is_none() && cached.is_none() {
            return;
        }
        self.observed = true;
        self.input_tokens = input;
        self.output_tokens = output;
        self.cached_input_tokens = cached;
        self.complete = input.is_some() && output.is_some();
    }
}

#[allow(clippy::too_many_arguments)]
fn emit_request_event(
    state: &ApiState,
    generation_id: uuid::Uuid,
    api_key_id: uuid::Uuid,
    request_id: uuid::Uuid,
    route_slug: &RouteSlug,
    attempts: &[UsageAttempt],
    request_started_at: chrono::DateTime<Utc>,
    request_started: tokio::time::Instant,
    final_attempt_started: Option<tokio::time::Instant>,
    first_byte_ms: Option<u64>,
    status_code: Option<u16>,
    error_class: Option<String>,
    committed: bool,
    usage: &UsageCapture,
    surface: Surface,
    operation: &'static str,
) {
    crate::claim_http_inference_metadata();
    if let Some(emitter) = &state.usage {
        let request_completed_at = Utc::now();
        let mut attempts = attempts.to_vec();
        if let (Some(final_attempt), Some(started)) = (attempts.last_mut(), final_attempt_started) {
            final_attempt.completed_at = request_completed_at.max(final_attempt.started_at);
            final_attempt.status_code = status_code;
            final_attempt.error_class.clone_from(&error_class);
            final_attempt.committed = committed;
            final_attempt.latency_ms = elapsed_ms(started.elapsed());
            final_attempt.first_byte_ms = first_byte_ms;
        }
        let provider_id = attempts.last().map(|attempt| attempt.provider_id);
        let upstream_model = attempts
            .last()
            .map(|attempt| attempt.upstream_model.clone());
        let result = emitter.emit(UsageEvent {
            event_id: uuid::Uuid::now_v7(),
            request_id,
            runtime_generation_id: generation_id,
            api_key_id,
            provider_id,
            route_slug: route_slug.to_string(),
            upstream_model,
            operation: operation
                .parse()
                .expect("request metadata uses a canonical operation"),
            surface,
            request_started_at,
            request_completed_at,
            observed_at: request_completed_at,
            status_code,
            error_class,
            committed,
            latency_ms: elapsed_ms(request_started.elapsed()),
            first_byte_ms,
            input_tokens: usage.input_tokens,
            output_tokens: usage.output_tokens,
            cached_input_tokens: usage.cached_input_tokens,
            media_units: usage.media_units,
            usage_complete: usage.observed && usage.complete,
            unpriced: true,
            attempts,
        });
        if result.is_err() {
            error!(%request_id, "request metadata buffer overflowed");
        }
    }
}

fn elapsed_ms(duration: Duration) -> u64 {
    u64::try_from(duration.as_millis()).unwrap_or(u64::MAX)
}

fn bearer_token(headers: &HeaderMap) -> Result<&str, InferenceError> {
    let authorization = headers
        .get(header::AUTHORIZATION)
        .and_then(|value| value.to_str().ok())
        .ok_or_else(InferenceError::unauthorized)?;
    let (scheme, token) = authorization
        .split_once(' ')
        .ok_or_else(InferenceError::unauthorized)?;
    if !scheme.eq_ignore_ascii_case("bearer")
        || token.is_empty()
        || token.contains(char::is_whitespace)
    {
        return Err(InferenceError::unauthorized());
    }
    Ok(token)
}

pub(crate) fn authenticate_model_access(
    state: &ApiState,
    plaintext_key: &str,
    operation: OperationKind,
) -> Result<(std::sync::Arc<crate::RuntimeBundle>, ApiKey), InferenceError> {
    let authenticated = authenticate_proxy_key(state, plaintext_key)?;
    authorize_api_key(&authenticated.key, None, operation, Utc::now())
        .map_err(|error| InferenceError::forbidden(error.to_string()))?;
    Ok((authenticated.runtime, authenticated.key))
}

pub(crate) async fn reserve_model_limits(
    state: &ApiState,
    key: &ApiKey,
    plaintext_key: &str,
    surface: Surface,
) -> Result<Option<LimitLease>, InferenceError> {
    let hasher = state
        .key_hasher
        .as_ref()
        .ok_or_else(|| InferenceError::unavailable("api_key_authentication_unavailable"))?;
    let lookup = hasher
        .lookup_id(plaintext_key)
        .map_err(|_| InferenceError::unauthorized())?;
    let operation = Operation::Models(olp_domain::ModelOperation::List {
        extensions: olp_domain::SourceExtensions::new(surface, BTreeMap::new()),
    });
    reserve_limits(state, key, &operation, lookup, Duration::from_secs(30)).await
}

pub(crate) async fn release_model_limits(state: &ApiState, lease: Option<&LimitLease>) {
    release_limits(state, lease).await;
}

pub(crate) struct SessionGenerationExecution {
    pub events: Vec<CanonicalEvent>,
    pub request_id: RequestId,
    pub route_slug: RouteSlug,
    pub latency_ms: u64,
}

/// Executes a management-session playground request through the exact same
/// capability selection, provider transport, deadline, cancellation, and
/// failover machinery as API-key inference. The caller owns session/RBAC
/// authorization. Content and usage are intentionally not emitted or stored.
pub(crate) async fn execute_session_generation(
    state: &ApiState,
    operation: Operation,
    surface: Surface,
) -> Result<SessionGenerationExecution, InferenceError> {
    let request_media = RequestMediaGuard::new(
        state.media_spool.clone(),
        operation_media_handles(&operation),
    );
    let result = execute_session_generation_inner(state, operation, surface).await;
    request_media.cleanup().await;
    result
}

async fn execute_session_generation_inner(
    state: &ApiState,
    operation: Operation,
    surface: Surface,
) -> Result<SessionGenerationExecution, InferenceError> {
    if operation.kind() != OperationKind::Generation {
        return Err(InferenceError::invalid_request(
            "The playground supports generation only.",
        ));
    }
    let snapshot = state.runtime.pin();
    let route_slug = operation
        .route()
        .cloned()
        .ok_or_else(|| InferenceError::invalid_request("A route model is required."))?;
    let request_id = RequestId::new();
    let attempts = select_representable_attempts_filtered(
        &snapshot,
        &route_slug,
        &operation,
        surface,
        TransportMode::Unary,
        request_id.as_uuid().as_bytes(),
        |_, target| state.circuits.is_selectable(target.id),
    )?;
    let route = snapshot
        .routes
        .get(&route_slug)
        .expect("attempt selection returned a known route");
    let started = tokio::time::Instant::now();
    let execution = execute_with_failover(
        &snapshot,
        attempts,
        RequestMetadata {
            request_id,
            operation: OperationKind::Generation,
            surface,
            mode: TransportMode::Unary,
        },
        operation,
        route.overall_timeout.as_duration(),
        state.media_spool.clone(),
        &state.circuits,
    )
    .await;
    let success = execution.map_err(|failure| failure.error)?;
    let ExecutionOutput::Events { first, mut events } = success.output else {
        return Err(incompatible_result("generation"));
    };
    let events = collect_provider_events(first, &mut events, success.deadline).await?;
    Ok(SessionGenerationExecution {
        events,
        request_id,
        route_slug,
        latency_ms: elapsed_ms(started.elapsed()),
    })
}

pub(crate) struct InferenceError {
    status: StatusCode,
    code: &'static str,
    kind: &'static str,
    message: String,
    retry_after: Option<Duration>,
}

impl fmt::Debug for InferenceError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("InferenceError")
            .field("status", &self.status)
            .field("code", &self.code)
            .field("kind", &self.kind)
            .field("message", &"[REDACTED]")
            .field("retry_after", &self.retry_after)
            .finish()
    }
}

impl InferenceError {
    pub(crate) fn unauthorized() -> Self {
        Self {
            status: StatusCode::UNAUTHORIZED,
            code: "invalid_api_key",
            kind: "authentication_error",
            message: "The API key is invalid or unavailable.".to_owned(),
            retry_after: None,
        }
    }

    pub(crate) fn forbidden(message: String) -> Self {
        Self {
            status: StatusCode::FORBIDDEN,
            code: "permission_denied",
            kind: "permission_error",
            message,
            retry_after: None,
        }
    }

    pub(crate) fn invalid_request(message: impl Into<String>) -> Self {
        Self {
            status: StatusCode::BAD_REQUEST,
            code: "invalid_request",
            kind: "invalid_request_error",
            message: message.into(),
            retry_after: None,
        }
    }

    fn payload_too_large(code: &'static str) -> Self {
        Self {
            status: StatusCode::PAYLOAD_TOO_LARGE,
            code,
            kind: "invalid_request_error",
            message: "The uploaded media exceeds the configured limit.".to_owned(),
            retry_after: None,
        }
    }

    pub(crate) fn not_found(message: String) -> Self {
        Self {
            status: StatusCode::NOT_FOUND,
            code: "route_not_found",
            kind: "invalid_request_error",
            message,
            retry_after: None,
        }
    }

    fn resource_not_found(code: &'static str) -> Self {
        Self {
            status: StatusCode::NOT_FOUND,
            code,
            kind: "invalid_request_error",
            message: "The requested resource was not found.".to_owned(),
            retry_after: None,
        }
    }

    pub(crate) fn rate_limited(dimension: LimitDimension, retry_after: Duration) -> Self {
        let name = match dimension {
            LimitDimension::Requests => "requests per minute",
            LimitDimension::Tokens => "tokens per minute",
            LimitDimension::Concurrency => "concurrency",
            LimitDimension::Unknown => "configured",
        };
        Self {
            status: StatusCode::TOO_MANY_REQUESTS,
            code: "rate_limit_exceeded",
            kind: "rate_limit_error",
            message: format!("The API key {name} limit was exceeded."),
            retry_after: Some(retry_after),
        }
    }

    pub(crate) fn unavailable(code: &'static str) -> Self {
        Self {
            status: StatusCode::SERVICE_UNAVAILABLE,
            code,
            kind: "service_unavailable_error",
            message: "The gateway is temporarily unavailable.".to_owned(),
            retry_after: None,
        }
    }

    fn multipart_parser_timeout() -> Self {
        Self {
            status: StatusCode::REQUEST_TIMEOUT,
            code: "multipart_parser_timeout",
            kind: "timeout_error",
            message: "The multipart upload exceeded its parser deadline.".to_owned(),
            retry_after: None,
        }
    }

    pub(crate) fn timeout() -> Self {
        Self {
            status: StatusCode::GATEWAY_TIMEOUT,
            code: "gateway_timeout",
            kind: "timeout_error",
            message: "The route deadline elapsed.".to_owned(),
            retry_after: None,
        }
    }

    pub(crate) fn bad_gateway(code: &'static str, message: impl Into<String>) -> Self {
        Self {
            status: StatusCode::BAD_GATEWAY,
            code,
            kind: "upstream_error",
            message: message.into(),
            retry_after: None,
        }
    }

    pub(crate) fn client_cancelled() -> Self {
        Self {
            status: StatusCode::BAD_GATEWAY,
            code: "client_cancelled",
            kind: "cancelled_error",
            message: "The client disconnected.".to_owned(),
            retry_after: None,
        }
    }

    pub(crate) const fn status(&self) -> StatusCode {
        self.status
    }

    pub(crate) const fn code(&self) -> &'static str {
        self.code
    }

    pub(crate) const fn kind(&self) -> &'static str {
        self.kind
    }

    pub(crate) fn message(&self) -> &str {
        &self.message
    }

    pub(crate) const fn retry_after(&self) -> Option<Duration> {
        self.retry_after
    }

    pub(crate) fn into_problem(self) -> Problem {
        self.into()
    }

    pub(crate) fn from_transport(error: TransportError) -> Self {
        match error.class {
            AttemptFailureClass::RateLimit => Self {
                status: StatusCode::TOO_MANY_REQUESTS,
                code: "upstream_rate_limit",
                kind: "rate_limit_error",
                message: error.message,
                retry_after: None,
            },
            AttemptFailureClass::Timeout => Self::timeout(),
            AttemptFailureClass::UpstreamClient => {
                Self::bad_gateway("upstream_rejected", error.message)
            }
            AttemptFailureClass::Connect | AttemptFailureClass::UpstreamServer => {
                Self::bad_gateway("upstream_unavailable", error.message)
            }
            AttemptFailureClass::Protocol => {
                Self::bad_gateway("provider_protocol_error", error.message)
            }
            AttemptFailureClass::Cancelled => {
                Self::bad_gateway("provider_cancelled", error.message)
            }
            AttemptFailureClass::Ambiguous => {
                Self::bad_gateway("ambiguous_upstream_result", error.message)
            }
        }
    }

    pub(crate) fn from_canonical(error: &CanonicalError) -> Self {
        let status = match error.class {
            ErrorClass::Authentication => StatusCode::BAD_GATEWAY,
            ErrorClass::Authorization => StatusCode::BAD_GATEWAY,
            ErrorClass::InvalidRequest => StatusCode::BAD_GATEWAY,
            ErrorClass::RateLimit => StatusCode::TOO_MANY_REQUESTS,
            ErrorClass::Timeout => StatusCode::GATEWAY_TIMEOUT,
            ErrorClass::Transport | ErrorClass::Upstream | ErrorClass::Internal => {
                StatusCode::BAD_GATEWAY
            }
        };
        Self {
            status,
            code: "upstream_error",
            kind: crate::openai_response::error_type(error.class),
            message: error.message.clone(),
            retry_after: None,
        }
    }
}

#[derive(Serialize)]
struct OpenAiErrorEnvelope<'a> {
    error: OpenAiErrorBody<'a>,
}

#[derive(Serialize)]
struct OpenAiErrorBody<'a> {
    message: &'a str,
    #[serde(rename = "type")]
    kind: &'a str,
    param: Option<&'a str>,
    code: &'a str,
}

impl IntoResponse for InferenceError {
    fn into_response(self) -> Response {
        let mut response = (
            self.status,
            Json(OpenAiErrorEnvelope {
                error: OpenAiErrorBody {
                    message: &self.message,
                    kind: self.kind,
                    param: None,
                    code: self.code,
                },
            }),
        )
            .into_response();
        if let Some(retry_after) = self.retry_after {
            let seconds = retry_after.as_secs().max(1).to_string();
            if let Ok(value) = HeaderValue::from_str(&seconds) {
                response.headers_mut().insert(header::RETRY_AFTER, value);
            }
        }
        response
    }
}

impl From<InferenceError> for Problem {
    fn from(error: InferenceError) -> Self {
        Problem::new(error.status, error.code, error.kind, error.message)
    }
}

#[cfg(test)]
mod tests {
    use std::{
        collections::{BTreeMap, BTreeSet},
        num::{NonZeroU16, NonZeroU32},
        sync::{
            Arc, Mutex,
            atomic::{AtomicBool, AtomicUsize, Ordering},
        },
    };

    use axum::{body::Body, http::Request};
    use chrono::Utc;
    use http_body_util::BodyExt;
    use olp_domain::{
        ApiKeyDigest, ApiKeyId, ApiKeyLimits, ApiKeyScope, ApiKeyStatus, BoxFuture, Capability,
        CredentialVersionId, DurationMs, FinishReason, MessageRole, Provider, ProviderEventStream,
        ProviderId, ProviderKind, ProviderTransport, Route, RouteId, RuntimeGeneration,
        RuntimeGenerationId, RuntimeSnapshot, Target, TargetId,
    };
    use olp_storage::KeyHasher;
    use tower::ServiceExt;

    use super::*;

    #[test]
    fn inference_error_debug_redacts_client_message() {
        let error =
            InferenceError::bad_gateway("provider_protocol_error", "sensitive upstream response");
        let debug = format!("{error:?}");

        assert!(!debug.contains("sensitive upstream response"));
        assert!(debug.contains("[REDACTED]"));
    }

    #[test]
    fn unknown_upstream_video_status_fails_closed() {
        let error = media_job_state(&olp_domain::VideoStatus::Other("mystery".to_owned()))
            .expect_err("unknown upstream status must not become a local terminal state");
        assert_eq!(error.status, StatusCode::BAD_GATEWAY);
        assert_eq!(error.code, "provider_protocol_error");
    }

    #[test]
    fn upstream_media_identity_is_bounded_before_durable_attachment() {
        assert!(valid_upstream_media_job_id("video_123"));
        assert!(!valid_upstream_media_job_id(""));
        assert!(!valid_upstream_media_job_id(" video_123"));
        assert!(!valid_upstream_media_job_id("video\n123"));
        assert!(!valid_upstream_media_job_id(&"x".repeat(1_025)));
    }

    #[tokio::test]
    async fn canonical_event_stream_wrapper_rejects_gaps_and_missing_done() {
        let first = CanonicalEvent::new(
            0,
            CanonicalEventKind::ResponseStart {
                response_id: None,
                provider_model: None,
            },
        );
        let mut validator = EventSequenceValidator::new();
        validator.push(&first).unwrap();
        let events: EventStream = Box::pin(stream::iter([Ok(CanonicalEvent::new(
            2,
            CanonicalEventKind::Done,
        ))]));
        let error = match validated_event_stream(events, validator).next().await {
            Some(Err(error)) => error,
            _ => panic!("sequence gap must become a protocol error"),
        };
        assert_eq!(error.class, AttemptFailureClass::Protocol);
        assert!(error.response_committed);
        assert!(
            error
                .message
                .contains("expected canonical event sequence 1")
        );

        let mut validator = EventSequenceValidator::new();
        validator.push(&first).unwrap();
        let events: EventStream = Box::pin(stream::empty());
        let error = match validated_event_stream(events, validator).next().await {
            Some(Err(error)) => error,
            _ => panic!("missing done must become a protocol error"),
        };
        assert_eq!(error.class, AttemptFailureClass::Protocol);
        assert!(error.message.contains("ended before done"));

        let mut validator = EventSequenceValidator::new();
        validator.push(&first).unwrap();
        let events: EventStream = Box::pin(stream::iter([Ok(CanonicalEvent::new(
            1,
            CanonicalEventKind::Done,
        ))]));
        let mut events = validated_event_stream(events, validator);
        assert!(matches!(
            events.next().await,
            Some(Ok(CanonicalEvent {
                kind: CanonicalEventKind::Done,
                ..
            }))
        ));
        assert!(events.next().await.is_none());
    }

    #[tokio::test]
    async fn committed_stream_failures_trip_circuit_only_after_terminal_accounting() {
        let circuits = crate::circuit::CircuitBreaker::default();
        let target = TargetId::new();
        let first = CanonicalEvent::new(
            0,
            CanonicalEventKind::ResponseStart {
                response_id: None,
                provider_model: None,
            },
        );

        for _ in 0..5 {
            assert!(circuits.acquire(target));
            let mut validator = EventSequenceValidator::new();
            validator.push(&first).unwrap();
            let provider: EventStream = Box::pin(stream::iter([Err(TransportError {
                phase: olp_domain::TransportPhase::Body,
                class: AttemptFailureClass::UpstreamServer,
                response_committed: false,
                message: "stream failed after its first event".to_owned(),
            })]));
            let mut events = circuit_accounted_event_stream(
                validated_event_stream(provider, validator),
                circuits.clone(),
                target,
                false,
            );
            let error = events.next().await.unwrap().unwrap_err();
            assert!(error.response_committed);
        }
        assert!(!circuits.is_selectable(target));

        let recovered_target = TargetId::new();
        circuits.record_failure(recovered_target, AttemptFailureClass::UpstreamServer);
        let mut validator = EventSequenceValidator::new();
        validator.push(&first).unwrap();
        let provider: EventStream = Box::pin(stream::iter([Ok(CanonicalEvent::new(
            1,
            CanonicalEventKind::Done,
        ))]));
        let mut events = circuit_accounted_event_stream(
            validated_event_stream(provider, validator),
            circuits.clone(),
            recovered_target,
            false,
        );
        assert!(matches!(
            events.next().await,
            Some(Ok(CanonicalEvent {
                kind: CanonicalEventKind::Done,
                ..
            }))
        ));
        for _ in 0..4 {
            circuits.record_failure(recovered_target, AttemptFailureClass::UpstreamServer);
        }
        assert!(circuits.is_selectable(recovered_target));
    }

    #[derive(Clone)]
    struct StaticTransport {
        events: Vec<CanonicalEvent>,
    }

    #[derive(Clone)]
    struct FiniteStaticTransport {
        events: Vec<CanonicalEvent>,
    }

    #[derive(Clone)]
    struct StaticResultTransport {
        result: CanonicalResult,
    }

    struct PendingTransport;

    #[derive(Default)]
    struct CountingAdmissionSpool {
        puts: AtomicUsize,
    }

    impl olp_domain::MediaSpool for CountingAdmissionSpool {
        fn put<'a>(
            &'a self,
            _upload: olp_domain::MediaUpload,
        ) -> BoxFuture<'a, Result<olp_domain::MediaArtifact, olp_domain::MediaSpoolError>> {
            self.puts.fetch_add(1, Ordering::SeqCst);
            Box::pin(async { Err(olp_domain::MediaSpoolError::Unavailable) })
        }

        fn open<'a>(
            &'a self,
            _handle: &'a MediaHandle,
        ) -> BoxFuture<'a, Result<olp_domain::OpenedMedia, olp_domain::MediaSpoolError>> {
            Box::pin(async { Err(olp_domain::MediaSpoolError::NotFound) })
        }

        fn remove<'a>(
            &'a self,
            _handle: &'a MediaHandle,
        ) -> BoxFuture<'a, Result<(), olp_domain::MediaSpoolError>> {
            Box::pin(async { Ok(()) })
        }
    }

    struct RecordingSpool {
        inner: Arc<dyn MediaSpool>,
        handles: Mutex<Vec<MediaHandle>>,
    }

    impl RecordingSpool {
        fn new(inner: Arc<dyn MediaSpool>) -> Arc<Self> {
            Arc::new(Self {
                inner,
                handles: Mutex::new(Vec::new()),
            })
        }

        fn handles(&self) -> Vec<MediaHandle> {
            self.handles.lock().unwrap().clone()
        }
    }

    impl MediaSpool for RecordingSpool {
        fn capacity_bytes(&self) -> Option<u64> {
            self.inner.capacity_bytes()
        }

        fn put<'a>(
            &'a self,
            upload: olp_domain::MediaUpload,
        ) -> BoxFuture<'a, Result<olp_domain::MediaArtifact, olp_domain::MediaSpoolError>> {
            Box::pin(async move {
                let artifact = self.inner.put(upload).await?;
                self.handles.lock().unwrap().push(artifact.handle.clone());
                Ok(artifact)
            })
        }

        fn open<'a>(
            &'a self,
            handle: &'a MediaHandle,
        ) -> BoxFuture<'a, Result<olp_domain::OpenedMedia, olp_domain::MediaSpoolError>> {
            self.inner.open(handle)
        }

        fn remove<'a>(
            &'a self,
            handle: &'a MediaHandle,
        ) -> BoxFuture<'a, Result<(), olp_domain::MediaSpoolError>> {
            self.inner.remove(handle)
        }
    }

    struct CapturingPendingTransport {
        captured: tokio::sync::mpsc::UnboundedSender<MediaHandle>,
    }

    impl ProviderTransport for PendingTransport {
        fn execute<'a>(
            &'a self,
            _request: ProviderRequest,
        ) -> BoxFuture<'a, Result<ProviderOutput, TransportError>> {
            Box::pin(std::future::pending())
        }
    }

    impl ProviderTransport for CapturingPendingTransport {
        fn execute<'a>(
            &'a self,
            request: ProviderRequest,
        ) -> BoxFuture<'a, Result<ProviderOutput, TransportError>> {
            if let Operation::TokenCount(operation) = &request.operation
                && let Some(handle) = operation.input.iter().find_map(|part| match part {
                    olp_domain::ContentPart::InputAudio { media, .. }
                    | olp_domain::ContentPart::InputFile { media, .. } => Some(media.clone()),
                    _ => None,
                })
            {
                let _ = self.captured.send(handle);
            }
            Box::pin(std::future::pending())
        }
    }

    #[tokio::test]
    async fn invalid_keys_cannot_spool_responses_media_before_authentication() {
        let (mut state, _) = test_state(false);
        let (_, invalid_key) = test_state(false);
        let spool = Arc::new(CountingAdmissionSpool::default());
        state.media_spool = spool.clone();

        for (path, body) in [
            (
                "/openai/v1/responses",
                r#"{"model":"default","input":[{"type":"message","role":"user","content":[{"type":"input_audio","input_audio":{"data":"YXVkaW8=","format":"wav"}}]}]}"#,
            ),
            (
                "/openai/v1/responses/input_tokens",
                r#"{"model":"default","input":[{"type":"message","role":"user","content":[{"type":"input_audio","input_audio":{"data":"YXVkaW8=","format":"wav"}}]}]}"#,
            ),
        ] {
            let response = post_json(&state, &invalid_key, path, body).await;
            assert_eq!(response.status(), StatusCode::UNAUTHORIZED);
        }
        assert_eq!(spool.puts.load(Ordering::SeqCst), 0);
    }

    #[tokio::test]
    async fn restricted_multipart_key_rejects_file_before_model_without_spooling() {
        let (mut state, key) = test_state(false);
        let spool = Arc::new(CountingAdmissionSpool::default());
        state.media_spool = spool.clone();
        restrict_api_key_to_route(&state, RouteSlug::parse("default").unwrap());
        let body = concat!(
            "--olp-test-boundary\r\n",
            "Content-Disposition: form-data; name=\"image\"; filename=\"fixture.png\"\r\n",
            "Content-Type: image/png\r\n\r\n",
            "file-before-model\r\n",
            "--olp-test-boundary\r\n",
            "Content-Disposition: form-data; name=\"model\"\r\n\r\n",
            "default\r\n",
            "--olp-test-boundary--\r\n"
        )
        .to_owned();
        let response = post_multipart(&state, &key, "/openai/v1/images/edits", body).await;
        assert_eq!(response.status(), StatusCode::BAD_REQUEST);
        assert_eq!(spool.puts.load(Ordering::SeqCst), 0);
    }

    #[tokio::test]
    async fn multipart_route_header_mismatch_cleans_the_staged_file() {
        let (mut state, key) = test_state(false);
        let recording = RecordingSpool::new(
            crate::media_spool::FileMediaSpool::create().unwrap() as Arc<dyn MediaSpool>
        );
        state.media_spool = recording.clone();
        restrict_api_key_to_route(&state, RouteSlug::parse("default").unwrap());
        let body = concat!(
            "--olp-test-boundary\r\n",
            "Content-Disposition: form-data; name=\"image\"; filename=\"fixture.png\"\r\n",
            "Content-Type: image/png\r\n\r\n",
            "staged-image\r\n",
            "--olp-test-boundary\r\n",
            "Content-Disposition: form-data; name=\"model\"\r\n\r\n",
            "other\r\n",
            "--olp-test-boundary\r\n",
            "Content-Disposition: form-data; name=\"prompt\"\r\n\r\n",
            "route mismatch\r\n",
            "--olp-test-boundary--\r\n"
        );
        let response = crate::public_router(state.clone())
            .oneshot(
                Request::post("/openai/v1/images/edits")
                    .header(header::AUTHORIZATION, format!("Bearer {key}"))
                    .header("x-olp-route", "default")
                    .header(
                        header::CONTENT_TYPE,
                        "multipart/form-data; boundary=olp-test-boundary",
                    )
                    .body(Body::from(body))
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(response.status(), StatusCode::BAD_REQUEST);
        let handles = recording.handles();
        assert_eq!(handles.len(), 1);
        assert!(matches!(
            recording.open(&handles[0]).await,
            Err(olp_domain::MediaSpoolError::NotFound)
        ));
    }

    #[tokio::test]
    async fn cancelling_response_input_tokens_handler_cleans_admitted_media() {
        let (state, key) = test_state(false);
        install_result(
            &state,
            OperationKind::TokenCount,
            CanonicalResult::TokenCount(olp_domain::TokenCountResult {
                input_tokens: 1,
                extensions: olp_domain::SourceExtensions::default(),
            }),
        );
        let (captured, mut handles) = tokio::sync::mpsc::unbounded_channel();
        install_transport(&state, Arc::new(CapturingPendingTransport { captured }));
        let state_for_task = state.clone();
        let task = tokio::spawn(async move {
            post_json(
                &state_for_task,
                &key,
                "/openai/v1/responses/input_tokens",
                r#"{"model":"default","input":[{"type":"message","role":"user","content":[{"type":"input_audio","input_audio":{"data":"YXVkaW8=","format":"wav"}}]}]}"#,
            )
            .await
        });
        let handle = tokio::time::timeout(Duration::from_secs(1), handles.recv())
            .await
            .expect("token-count request must reach its transport")
            .expect("transport must capture the admitted handle");
        assert!(state.media_spool.open(&handle).await.is_ok());

        task.abort();
        let _ = task.await;
        tokio::time::timeout(Duration::from_secs(1), async {
            loop {
                match state.media_spool.open(&handle).await {
                    Err(olp_domain::MediaSpoolError::NotFound) => break,
                    Ok(_) => tokio::task::yield_now().await,
                    Err(error) => panic!("unexpected media cleanup error: {error}"),
                }
            }
        })
        .await
        .expect("handler cancellation must schedule admitted-media cleanup");
    }

    #[tokio::test]
    async fn dropping_blocked_upstream_request_cleans_owned_media_handles() {
        let (state, key) = test_state(false);
        let artifact = state
            .media_spool
            .put(olp_domain::MediaUpload {
                filename: "inline.wav".into(),
                content_type: Some("audio/wav".into()),
                maximum_length: 16,
                bytes: Box::pin(stream::once(async { Ok(Bytes::from_static(b"audio")) })),
            })
            .await
            .unwrap();
        install_transport(&state, Arc::new(PendingTransport));
        let request: ChatCompletionRequest = serde_json::from_value(json!({
            "model":"default","messages":[{"role":"user","content":"hello"}]
        }))
        .unwrap();
        let mut operation = decode_chat_completion(request).unwrap();
        let Operation::Generation(generation) = &mut operation else {
            unreachable!()
        };
        generation.messages[0].content = vec![olp_domain::ContentPart::InputAudio {
            media: artifact.handle.clone(),
            format: "wav".into(),
        }];

        let state_for_task = state.clone();
        let task = tokio::spawn(async move {
            execute_event_operation_for_surface(
                &state_for_task,
                &key,
                operation,
                Surface::OpenAi,
                TransportMode::Unary,
            )
            .await
        });
        tokio::time::sleep(Duration::from_millis(20)).await;
        task.abort();
        let _ = task.await;

        tokio::time::timeout(Duration::from_secs(1), async {
            loop {
                match state.media_spool.open(&artifact.handle).await {
                    Err(olp_domain::MediaSpoolError::NotFound) => break,
                    Ok(_) => tokio::task::yield_now().await,
                    Err(error) => panic!("unexpected media cleanup error: {error}"),
                }
            }
        })
        .await
        .expect("request media guard must schedule cleanup when its future is dropped");
    }

    impl ProviderTransport for StaticResultTransport {
        fn execute<'a>(
            &'a self,
            _request: ProviderRequest,
        ) -> BoxFuture<'a, Result<ProviderOutput, TransportError>> {
            let result = self.result.clone();
            Box::pin(async move { Ok(ProviderOutput::Result(Box::new(result))) })
        }
    }

    struct DropAwareStream {
        first: Option<CanonicalEvent>,
        dropped: Arc<AtomicBool>,
    }

    impl futures::Stream for DropAwareStream {
        type Item = Result<CanonicalEvent, TransportError>;

        fn poll_next(
            mut self: std::pin::Pin<&mut Self>,
            _context: &mut std::task::Context<'_>,
        ) -> std::task::Poll<Option<Self::Item>> {
            self.first.take().map_or(std::task::Poll::Pending, |event| {
                std::task::Poll::Ready(Some(Ok(event)))
            })
        }
    }

    impl Drop for DropAwareStream {
        fn drop(&mut self) {
            self.dropped.store(true, Ordering::Release);
        }
    }

    #[derive(Clone)]
    struct DropAwareTransport {
        dropped: Arc<AtomicBool>,
    }

    impl ProviderTransport for DropAwareTransport {
        fn execute<'a>(
            &'a self,
            _request: ProviderRequest,
        ) -> BoxFuture<'a, Result<ProviderOutput, TransportError>> {
            let dropped = self.dropped.clone();
            Box::pin(async move {
                Ok(ProviderOutput::Events(Box::pin(DropAwareStream {
                    first: Some(CanonicalEvent::new(
                        0,
                        CanonicalEventKind::TextDelta {
                            output_index: 0,
                            text: "first".into(),
                        },
                    )),
                    dropped,
                })))
            })
        }
    }

    impl ProviderTransport for StaticTransport {
        fn execute<'a>(
            &'a self,
            _request: ProviderRequest,
        ) -> BoxFuture<'a, Result<ProviderOutput, TransportError>> {
            let events = self.events.clone();
            Box::pin(async move {
                let events = stream::iter(events.into_iter().map(Ok)).chain(stream::pending());
                Ok(ProviderOutput::Events(
                    Box::pin(events) as ProviderEventStream
                ))
            })
        }
    }

    impl ProviderTransport for FiniteStaticTransport {
        fn execute<'a>(
            &'a self,
            _request: ProviderRequest,
        ) -> BoxFuture<'a, Result<ProviderOutput, TransportError>> {
            let events = self.events.clone();
            Box::pin(async move {
                Ok(ProviderOutput::Events(Box::pin(stream::iter(
                    events.into_iter().map(Ok),
                ))))
            })
        }
    }

    fn test_state(streaming: bool) -> (ApiState, String) {
        let key_hasher = Arc::new(KeyHasher::new([7; 32]));
        let material = key_hasher.generate_api_key();
        let plaintext = material.expose_once().to_owned();
        let lookup = ApiKeyLookupId::parse(material.lookup_id.clone()).unwrap();
        let route_slug = RouteSlug::parse("default").unwrap();
        let provider_id = ProviderId::new();
        let mode = if streaming {
            TransportMode::Streaming
        } else {
            TransportMode::Unary
        };
        let provider = Provider {
            id: provider_id,
            name: "mock-openai".to_owned(),
            kind: ProviderKind::OpenAi,
            enabled: true,
            active_credential: Some(CredentialVersionId::new()),
            capabilities: BTreeSet::from([Capability::new(
                "upstream-model",
                OperationKind::Generation,
                Surface::OpenAi,
                mode,
            )]),
        };
        let route = Route {
            id: RouteId::new(),
            routing_id: None,
            slug: route_slug.clone(),
            operations: BTreeSet::from([OperationKind::Generation]),
            overall_timeout: DurationMs::new(5_000),
            max_attempts: NonZeroU16::new(1).unwrap(),
            targets: vec![Target {
                id: TargetId::new(),
                routing_id: None,
                provider_id,
                provider_model: "upstream-model".to_owned(),
                priority: 0,
                weight: NonZeroU32::new(1).unwrap(),
                timeout: DurationMs::new(4_000),
            }],
        };
        let snapshot = RuntimeSnapshot {
            generation: RuntimeGeneration {
                id: RuntimeGenerationId::new(),
                ordinal: 1,
                activated_at: Utc::now(),
            },
            providers: BTreeMap::from([(provider_id, provider)]),
            routes: BTreeMap::from([(route_slug, route)]),
            api_keys: BTreeMap::from([(
                lookup.clone(),
                ApiKey {
                    id: ApiKeyId::new(),
                    lookup_id: lookup,
                    digest: ApiKeyDigest::new(material.digest),
                    status: ApiKeyStatus::Active,
                    expires_at: None,
                    scopes: BTreeSet::from([ApiKeyScope::Inference]),
                    allowed_routes: BTreeSet::new(),
                    limits: ApiKeyLimits::default(),
                },
            )]),
        };
        let runtime = Arc::new(crate::RuntimeManager::empty());
        let transport: Arc<dyn ProviderTransport> = Arc::new(StaticTransport {
            events: vec![
                CanonicalEvent::new(
                    0,
                    CanonicalEventKind::ResponseStart {
                        response_id: Some("chatcmpl-upstream".to_owned()),
                        provider_model: Some("upstream-model".to_owned()),
                    },
                ),
                CanonicalEvent::new(
                    1,
                    CanonicalEventKind::MessageStart {
                        output_index: 0,
                        role: MessageRole::Assistant,
                    },
                ),
                CanonicalEvent::new(
                    2,
                    CanonicalEventKind::TextDelta {
                        output_index: 0,
                        text: "hello from OLP".to_owned(),
                    },
                ),
                CanonicalEvent::new(
                    3,
                    CanonicalEventKind::Finish {
                        output_index: 0,
                        reason: FinishReason::Stop,
                    },
                ),
                CanonicalEvent::new(4, CanonicalEventKind::Done),
            ],
        });
        runtime
            .install(snapshot, BTreeMap::from([(provider_id, transport)]))
            .unwrap();
        let mut state = ApiState::new(
            crate::ApiMode::Gateway,
            None,
            runtime,
            "https://olp.test",
            "console",
        );
        state.key_hasher = Some(key_hasher);
        (state, plaintext)
    }

    fn reinstall_api_keys(state: &ApiState, api_keys: BTreeMap<ApiKeyLookupId, ApiKey>) {
        let pinned = state.runtime.pin();
        let snapshot = RuntimeSnapshot {
            generation: RuntimeGeneration {
                id: RuntimeGenerationId::new(),
                ordinal: pinned.generation.ordinal + 1,
                activated_at: Utc::now(),
            },
            providers: pinned.providers.clone(),
            routes: pinned.routes.clone(),
            api_keys,
        };
        let transports = pinned
            .providers
            .keys()
            .map(|provider_id| (*provider_id, pinned.transport(*provider_id).unwrap()))
            .collect();
        state.runtime.install(snapshot, transports).unwrap();
    }

    fn install_hard_limits(state: &ApiState) {
        let pinned = state.runtime.pin();
        let mut api_keys = pinned.api_keys.clone();
        api_keys
            .values_mut()
            .next()
            .unwrap()
            .limits
            .requests_per_minute = NonZeroU32::new(10);
        api_keys.values_mut().next().unwrap().limits.concurrency = NonZeroU32::new(2);
        reinstall_api_keys(state, api_keys);
    }

    fn restrict_api_key_to_route(state: &ApiState, route: RouteSlug) {
        let pinned = state.runtime.pin();
        let mut api_keys = pinned.api_keys.clone();
        api_keys.values_mut().next().unwrap().allowed_routes = BTreeSet::from([route]);
        reinstall_api_keys(state, api_keys);
    }

    fn install_result(state: &ApiState, operation: OperationKind, result: CanonicalResult) {
        let pinned = state.runtime.pin();
        let provider_id = *pinned.providers.keys().next().unwrap();
        let mut providers = pinned.providers.clone();
        providers.get_mut(&provider_id).unwrap().capabilities = BTreeSet::from([Capability::new(
            "upstream-model",
            operation,
            Surface::OpenAi,
            TransportMode::Unary,
        )]);
        let mut routes = pinned.routes.clone();
        let route = routes
            .get_mut(&RouteSlug::parse("default").unwrap())
            .unwrap();
        route.operations = BTreeSet::from([operation]);
        let snapshot = RuntimeSnapshot {
            generation: RuntimeGeneration {
                id: RuntimeGenerationId::new(),
                ordinal: pinned.generation.ordinal + 1,
                activated_at: Utc::now(),
            },
            providers,
            routes,
            api_keys: pinned.api_keys.clone(),
        };
        let transport: Arc<dyn ProviderTransport> = Arc::new(StaticResultTransport { result });
        state
            .runtime
            .install(snapshot, BTreeMap::from([(provider_id, transport)]))
            .unwrap();
    }

    fn install_transport(state: &ApiState, transport: Arc<dyn ProviderTransport>) {
        let pinned = state.runtime.pin();
        let provider_id = *pinned.providers.keys().next().unwrap();
        let snapshot = RuntimeSnapshot {
            generation: RuntimeGeneration {
                id: RuntimeGenerationId::new(),
                ordinal: pinned.generation.ordinal + 1,
                activated_at: Utc::now(),
            },
            providers: pinned.providers.clone(),
            routes: pinned.routes.clone(),
            api_keys: pinned.api_keys.clone(),
        };
        state
            .runtime
            .install(snapshot, BTreeMap::from([(provider_id, transport)]))
            .unwrap();
    }

    fn install_event_stream(
        state: &ApiState,
        operation: OperationKind,
        events: Vec<CanonicalEvent>,
        finite: bool,
    ) {
        let pinned = state.runtime.pin();
        let provider_id = *pinned.providers.keys().next().unwrap();
        let mut providers = pinned.providers.clone();
        providers.get_mut(&provider_id).unwrap().capabilities = BTreeSet::from([Capability::new(
            "upstream-model",
            operation,
            Surface::OpenAi,
            TransportMode::Streaming,
        )]);
        let mut routes = pinned.routes.clone();
        routes
            .get_mut(&RouteSlug::parse("default").unwrap())
            .unwrap()
            .operations = BTreeSet::from([operation]);
        let snapshot = RuntimeSnapshot {
            generation: RuntimeGeneration {
                id: RuntimeGenerationId::new(),
                ordinal: pinned.generation.ordinal + 1,
                activated_at: Utc::now(),
            },
            providers,
            routes,
            api_keys: pinned.api_keys.clone(),
        };
        let transport: Arc<dyn ProviderTransport> = if finite {
            Arc::new(FiniteStaticTransport { events })
        } else {
            Arc::new(StaticTransport { events })
        };
        state
            .runtime
            .install(snapshot, BTreeMap::from([(provider_id, transport)]))
            .unwrap();
    }

    fn generation_stream_events(text: &str) -> Vec<CanonicalEvent> {
        vec![
            CanonicalEvent::new(
                0,
                CanonicalEventKind::ResponseStart {
                    response_id: Some("response-upstream".into()),
                    provider_model: Some("upstream-model".into()),
                },
            ),
            CanonicalEvent::new(
                1,
                CanonicalEventKind::MessageStart {
                    output_index: 0,
                    role: MessageRole::Assistant,
                },
            ),
            CanonicalEvent::new(
                2,
                CanonicalEventKind::TextDelta {
                    output_index: 0,
                    text: text.to_owned(),
                },
            ),
            CanonicalEvent::new(
                3,
                CanonicalEventKind::Finish {
                    output_index: 0,
                    reason: FinishReason::Stop,
                },
            ),
            CanonicalEvent::new(
                4,
                CanonicalEventKind::Usage {
                    usage: olp_domain::Usage {
                        input_tokens: 7,
                        output_tokens: 3,
                        total_tokens: 10,
                        cached_input_tokens: Some(2),
                        reasoning_tokens: Some(1),
                    },
                },
            ),
            CanonicalEvent::new(5, CanonicalEventKind::Done),
        ]
    }

    fn raw_media_event(sequence: u64, event: &str, data: Value) -> CanonicalEvent {
        CanonicalEvent::new(
            sequence,
            CanonicalEventKind::SourceExtension {
                extensions: olp_domain::SourceExtensions::new(
                    Surface::OpenAi,
                    BTreeMap::from([
                        ("/__olp/raw_sse/event".into(), Value::String(event.into())),
                        ("/__olp/raw_sse/data".into(), data),
                    ]),
                ),
            },
        )
    }

    #[tokio::test]
    async fn unary_openai_route_authenticates_routes_and_encodes() {
        let (mut state, key) = test_state(false);
        let (emitter, mut usage) = olp_storage::UsageEmitter::bounded(4);
        state.usage = Some(emitter);
        let response = tokio::time::timeout(
            Duration::from_millis(250),
            crate::public_router(state).oneshot(
                Request::post("/openai/v1/chat/completions")
                    .header(header::AUTHORIZATION, format!("Bearer {key}"))
                    .header(header::CONTENT_TYPE, "application/json")
                    .body(Body::from(
                        r#"{"model":"default","messages":[{"role":"user","content":"hi"}]}"#,
                    ))
                    .unwrap(),
            ),
        )
        .await
        .expect("canonical Done must stop polling a provider that holds the stream open")
        .unwrap();
        assert_eq!(response.status(), StatusCode::OK);
        let body = response.into_body().collect().await.unwrap().to_bytes();
        let value: Value = serde_json::from_slice(&body).unwrap();
        assert_eq!(value["model"], "default");
        assert_eq!(value["choices"][0]["message"]["content"], "hello from OLP");
        let event = usage.recv_next().await.unwrap();
        assert_eq!(event.status_code, Some(200));
        assert_eq!(event.attempts.len(), 1);
        assert!(event.committed);
        assert!(!event.usage_complete, "missing provider usage is explicit");
    }

    #[tokio::test]
    async fn openai_json_audio_and_responses_pdf_reach_same_protocol_transport() {
        let (state, key) = test_state(false);
        let app = crate::public_router(state);
        let response = app
            .clone()
            .oneshot(
                Request::post("/openai/v1/chat/completions")
                    .header(header::AUTHORIZATION, format!("Bearer {key}"))
                    .header(header::CONTENT_TYPE, "application/json")
                    .body(Body::from(
                        r#"{"model":"default","messages":[{"role":"user","content":[{"type":"input_audio","input_audio":{"data":"aGk=","format":"wav"}}]}]}"#,
                    ))
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(response.status(), StatusCode::OK);

        let response = app
            .oneshot(
                Request::post("/openai/v1/responses")
                    .header(header::AUTHORIZATION, format!("Bearer {key}"))
                    .header(header::CONTENT_TYPE, "application/json")
                    .body(Body::from(
                        r#"{"model":"default","input":[{"type":"message","role":"user","content":[{"type":"input_file","filename":"brief.pdf","file_data":"data:application/pdf;base64,aGk="}]}]}"#,
                    ))
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(response.status(), StatusCode::OK);
    }

    #[tokio::test]
    async fn direct_executor_reserves_hard_limits_before_route_selection() {
        let (state, key) = test_state(false);
        install_hard_limits(&state);
        let request: ChatCompletionRequest = serde_json::from_value(json!({
            "model": "route-does-not-exist",
            "messages": [{"role": "user", "content": "hello"}]
        }))
        .unwrap();
        let operation = decode_chat_completion(request).unwrap();
        let error = match execute_event_operation_for_surface_inner(
            &state,
            &key,
            operation,
            Surface::OpenAi,
            TransportMode::Unary,
        )
        .await
        {
            Ok(_) => panic!("missing limiter must fail closed before route selection"),
            Err(error) => error,
        };
        assert_eq!(error.status, StatusCode::SERVICE_UNAVAILABLE);
        assert_eq!(error.code, "distributed_limits_unavailable");
    }

    #[tokio::test]
    async fn http_pre_reservation_marker_reuses_the_full_reservation() {
        let (state, key) = test_state(false);
        install_hard_limits(&state);
        let snapshot = state.runtime.pin();
        let api_key = snapshot.api_keys.values().next().unwrap();
        let lookup = state.key_hasher.as_ref().unwrap().lookup_id(&key).unwrap();
        let operation = decode_chat_completion(
            serde_json::from_value(json!({
                "model": "default",
                "messages": [{"role": "user", "content": "hello"}]
            }))
            .unwrap(),
        )
        .unwrap();
        let lease = crate::HTTP_INFERENCE_LIMITS_RESERVED
            .scope(
                10_000,
                reserve_limits(&state, api_key, &operation, lookup, Duration::from_secs(30)),
            )
            .await
            .expect("the canonical executor must reuse the HTTP reservation");
        assert!(lease.is_none());
    }

    #[tokio::test]
    async fn http_request_above_baseline_requires_token_delta_reservation() {
        let (state, key) = test_state(false);
        let pinned = state.runtime.pin();
        let mut api_keys = pinned.api_keys.clone();
        api_keys
            .values_mut()
            .next()
            .unwrap()
            .limits
            .tokens_per_minute = std::num::NonZeroU64::new(2_200);
        reinstall_api_keys(&state, api_keys);
        let snapshot = state.runtime.pin();
        let api_key = snapshot.api_keys.values().next().unwrap();
        let lookup = state.key_hasher.as_ref().unwrap().lookup_id(&key).unwrap();
        let operation = Operation::Images(olp_domain::ImageOperation::Edit(
            olp_domain::ImageEditRequest {
                route: RouteSlug::parse("default").unwrap(),
                images: vec![MediaHandle::new("bounded-image")],
                mask: None,
                prompt: "x".repeat(8_500),
                stream: false,
                extensions: olp_domain::SourceExtensions::default(),
            },
        ));
        let error = crate::HTTP_INFERENCE_LIMITS_RESERVED
            .scope(
                2_000,
                reserve_limits(&state, api_key, &operation, lookup, Duration::from_secs(30)),
            )
            .await
            .expect_err("missing delta limiter must fail closed above the HTTP baseline");
        assert_eq!(error.status, StatusCode::SERVICE_UNAVAILABLE);
        assert_eq!(error.code, "distributed_limits_unavailable");
    }

    #[tokio::test]
    async fn invalid_proxy_key_gets_native_openai_error() {
        let (state, _) = test_state(false);
        let response = crate::public_router(state)
            .oneshot(
                Request::post("/openai/v1/chat/completions")
                    .header(header::AUTHORIZATION, "Bearer olp_v2_deadbeef0000_bad")
                    .header(header::CONTENT_TYPE, "application/json")
                    .body(Body::from(r#"{"model":"default","messages":[]}"#))
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(response.status(), StatusCode::UNAUTHORIZED);
        let body = response.into_body().collect().await.unwrap().to_bytes();
        let value: Value = serde_json::from_slice(&body).unwrap();
        assert_eq!(value["error"]["type"], "authentication_error");
    }

    #[tokio::test]
    async fn responses_surface_encodes_responses_object_not_chat_object() {
        let (state, key) = test_state(false);
        let response = crate::public_router(state)
            .oneshot(
                Request::post("/openai/v1/responses")
                    .header(header::AUTHORIZATION, format!("Bearer {key}"))
                    .header(header::CONTENT_TYPE, "application/json")
                    .body(Body::from(r#"{"model":"default","input":"hello"}"#))
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(response.status(), StatusCode::OK);
        let body = response.into_body().collect().await.unwrap().to_bytes();
        let value: Value = serde_json::from_slice(&body).unwrap();
        assert_eq!(value["object"], "response");
        assert_eq!(value["model"], "default");
        assert_eq!(value["output"][0]["content"][0]["text"], "hello from OLP");
    }

    #[tokio::test]
    async fn embeddings_surface_routes_and_encodes_typed_result() {
        let (state, key) = test_state(false);
        install_result(
            &state,
            OperationKind::Embeddings,
            CanonicalResult::Embeddings(olp_domain::EmbeddingsResult {
                model: Some("upstream-model".into()),
                data: vec![olp_domain::EmbeddingVector {
                    index: 0,
                    values: vec![0.25, -0.5],
                }],
                usage: Some(olp_domain::Usage {
                    input_tokens: 1,
                    output_tokens: 0,
                    total_tokens: 1,
                    cached_input_tokens: None,
                    reasoning_tokens: None,
                }),
                extensions: olp_domain::SourceExtensions::new(Surface::OpenAi, BTreeMap::new()),
            }),
        );
        let response = crate::public_router(state)
            .oneshot(
                Request::post("/openai/v1/embeddings")
                    .header(header::AUTHORIZATION, format!("Bearer {key}"))
                    .header(header::CONTENT_TYPE, "application/json")
                    .body(Body::from(r#"{"model":"default","input":"hello"}"#))
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(response.status(), StatusCode::OK);
        let body = response.into_body().collect().await.unwrap().to_bytes();
        let value: Value = serde_json::from_slice(&body).unwrap();
        assert_eq!(value["object"], "list");
        assert_eq!(value["model"], "default");
        assert_eq!(value["data"][0]["embedding"][0], 0.25);
    }

    async fn post_json(state: &ApiState, key: &str, path: &str, body: &'static str) -> Response {
        crate::public_router(state.clone())
            .oneshot(
                Request::post(path)
                    .header(header::AUTHORIZATION, format!("Bearer {key}"))
                    .header(header::CONTENT_TYPE, "application/json")
                    .body(Body::from(body))
                    .unwrap(),
            )
            .await
            .unwrap()
    }

    async fn post_multipart(state: &ApiState, key: &str, path: &str, body: String) -> Response {
        crate::public_router(state.clone())
            .oneshot(
                Request::post(path)
                    .header(header::AUTHORIZATION, format!("Bearer {key}"))
                    .header(
                        header::CONTENT_TYPE,
                        "multipart/form-data; boundary=olp-test-boundary",
                    )
                    .body(Body::from(body))
                    .unwrap(),
            )
            .await
            .unwrap()
    }

    async fn response_text(response: Response) -> String {
        String::from_utf8(
            response
                .into_body()
                .collect()
                .await
                .unwrap()
                .to_bytes()
                .to_vec(),
        )
        .unwrap()
    }

    fn multipart(fields: &[(&str, &str)], file_name: &str, bytes: &str) -> String {
        let mut body = String::new();
        for (name, value) in fields {
            body.push_str(&format!(
                "--olp-test-boundary\r\nContent-Disposition: form-data; name=\"{name}\"\r\n\r\n{value}\r\n"
            ));
        }
        body.push_str(&format!(
            "--olp-test-boundary\r\nContent-Disposition: form-data; name=\"{file_name}\"; filename=\"fixture.bin\"\r\nContent-Type: application/octet-stream\r\n\r\n{bytes}\r\n--olp-test-boundary--\r\n"
        ));
        body
    }

    #[tokio::test]
    async fn chat_and_responses_stream_through_the_real_router_with_native_usage() {
        let (mut state, key) = test_state(true);
        let (emitter, mut usage) = olp_storage::UsageEmitter::bounded(8);
        state.usage = Some(emitter);

        install_event_stream(
            &state,
            OperationKind::Generation,
            generation_stream_events("chat stream"),
            false,
        );
        let response = post_json(
            &state,
            &key,
            "/openai/v1/chat/completions",
            r#"{"model":"default","messages":[{"role":"user","content":"hi"}],"stream":true}"#,
        )
        .await;
        assert_eq!(response.status(), StatusCode::OK);
        assert_eq!(
            response.headers()[header::CONTENT_TYPE],
            "text/event-stream; charset=utf-8"
        );
        let body = response_text(response).await;
        assert!(body.contains("\"object\":\"chat.completion.chunk\""));
        assert!(body.contains("chat stream"));
        assert!(body.contains("\"prompt_tokens\":7"));
        assert!(body.ends_with("data: [DONE]\n\n"));
        let event = usage.recv_next().await.unwrap();
        assert_eq!(event.operation, OperationKind::Generation);
        assert_eq!(event.input_tokens, Some(7));
        assert_eq!(event.output_tokens, Some(3));
        assert!(event.usage_complete);

        install_event_stream(
            &state,
            OperationKind::Generation,
            generation_stream_events("responses stream"),
            false,
        );
        let response = post_json(
            &state,
            &key,
            "/openai/v1/responses",
            r#"{"model":"default","input":"hi","stream":true}"#,
        )
        .await;
        assert_eq!(response.status(), StatusCode::OK);
        let body = response_text(response).await;
        assert!(body.contains("event: response.created"));
        assert!(body.contains("event: response.output_text.delta"));
        assert!(body.contains("responses stream"));
        assert!(body.contains("event: response.completed"));
        assert!(!body.contains("chat.completion.chunk"));
        let event = usage.recv_next().await.unwrap();
        assert_eq!(event.operation, OperationKind::Generation);
        assert_eq!(event.input_tokens, Some(7));
        assert!(event.usage_complete);
    }

    #[tokio::test]
    async fn canonical_stream_error_is_not_persisted_as_success() {
        let (mut state, key) = test_state(true);
        let (emitter, mut usage) = olp_storage::UsageEmitter::bounded(2);
        state.usage = Some(emitter);
        install_event_stream(
            &state,
            OperationKind::Generation,
            vec![
                CanonicalEvent::new(
                    0,
                    CanonicalEventKind::Error {
                        error: CanonicalError {
                            class: ErrorClass::RateLimit,
                            message: "provider throttled the request".to_owned(),
                            provider_code: Some("rate_limit".to_owned()),
                            retryable: true,
                        },
                    },
                ),
                CanonicalEvent::new(1, CanonicalEventKind::Done),
            ],
            true,
        );

        let response = post_json(
            &state,
            &key,
            "/openai/v1/responses",
            r#"{"model":"default","input":"hi","stream":true}"#,
        )
        .await;
        assert_eq!(response.status(), StatusCode::OK);
        let body = response_text(response).await;
        assert!(
            body.contains("error") || body.contains("failed"),
            "stream body was {body:?}"
        );
        let event = usage.recv_next().await.unwrap();
        assert_eq!(event.status_code, Some(429));
        assert_ne!(event.error_class.as_deref(), None);
        assert!(event.committed);
    }

    #[tokio::test]
    async fn incompatible_unary_result_is_finalized_as_protocol_failure() {
        let (mut state, key) = test_state(false);
        let (emitter, mut usage) = olp_storage::UsageEmitter::bounded(2);
        state.usage = Some(emitter);
        install_result(
            &state,
            OperationKind::TokenCount,
            CanonicalResult::ModelList(olp_domain::ModelListResult {
                models: Vec::new(),
                extensions: olp_domain::SourceExtensions::new(Surface::OpenAi, BTreeMap::new()),
            }),
        );

        let response = post_json(
            &state,
            &key,
            "/openai/v1/responses/input_tokens",
            r#"{"model":"default","input":"hello"}"#,
        )
        .await;
        assert_eq!(response.status(), StatusCode::BAD_GATEWAY);
        let event = usage.recv_next().await.unwrap();
        assert_eq!(event.status_code, Some(502));
        assert_eq!(
            event.error_class.as_deref(),
            Some("provider_protocol_error")
        );
        assert_eq!(event.attempts.len(), 1);
        assert!(event.committed);
    }

    #[tokio::test]
    async fn real_router_generation_streams_report_truncation_in_native_envelopes() {
        let (state, key) = test_state(true);
        let truncated = generation_stream_events("partial")
            .into_iter()
            .take(3)
            .collect::<Vec<_>>();
        install_event_stream(&state, OperationKind::Generation, truncated.clone(), true);
        let response = post_json(
            &state,
            &key,
            "/openai/v1/chat/completions",
            r#"{"model":"default","messages":[{"role":"user","content":"hi"}],"stream":true}"#,
        )
        .await;
        let body = response_text(response).await;
        assert!(body.contains("provider_protocol_error"));
        assert!(body.ends_with("data: [DONE]\n\n"));

        install_event_stream(&state, OperationKind::Generation, truncated, true);
        let response = post_json(
            &state,
            &key,
            "/openai/v1/responses",
            r#"{"model":"default","input":"hi","stream":true}"#,
        )
        .await;
        let body = response_text(response).await;
        assert!(body.contains("event: error"));
        assert!(body.contains("\"type\":\"error\""));
        assert!(body.contains("provider_protocol_error"));
        assert!(!body.contains("event: response.completed"));

        install_event_stream(
            &state,
            OperationKind::ImageGeneration,
            vec![raw_media_event(
                0,
                "image_generation.partial_image",
                json!({"type":"image_generation.partial_image","partial_image_index":0,"b64_json":"YQ=="}),
            )],
            true,
        );
        let response = post_json(
            &state,
            &key,
            "/openai/v1/images/generations",
            r#"{"model":"default","prompt":"fox","stream":true}"#,
        )
        .await;
        let body = response_text(response).await;
        assert!(body.contains("event: image_generation.partial_image"));
        assert!(body.contains("provider_protocol_error"));
    }

    #[tokio::test]
    async fn image_speech_and_transcription_stream_native_sse_and_usage_through_router() {
        let (mut state, key) = test_state(true);
        let (emitter, mut usage) = olp_storage::UsageEmitter::bounded(8);
        state.usage = Some(emitter);

        install_event_stream(
            &state,
            OperationKind::ImageGeneration,
            vec![
                raw_media_event(
                    0,
                    "image_generation.partial_image",
                    json!({"type":"image_generation.partial_image","partial_image_index":0,"b64_json":"YQ=="}),
                ),
                raw_media_event(
                    1,
                    "image_generation.completed",
                    json!({"type":"image_generation.completed","usage":{"input_tokens":4,"output_tokens":2,"total_tokens":6}}),
                ),
                CanonicalEvent::new(2, CanonicalEventKind::Done),
            ],
            false,
        );
        let response = post_json(
            &state,
            &key,
            "/openai/v1/images/generations",
            r#"{"model":"default","prompt":"fox","stream":true}"#,
        )
        .await;
        let body = response_text(response).await;
        assert!(body.contains("event: image_generation.partial_image"));
        assert!(body.contains("event: image_generation.completed"));
        let event = usage.recv_next().await.unwrap();
        assert_eq!(event.operation, OperationKind::ImageGeneration);
        assert_eq!(event.input_tokens, Some(4));
        assert!(event.usage_complete);

        install_event_stream(
            &state,
            OperationKind::Speech,
            vec![
                raw_media_event(
                    0,
                    "speech.audio.delta",
                    json!({"type":"speech.audio.delta","audio":"bXAz"}),
                ),
                raw_media_event(
                    1,
                    "speech.audio.done",
                    json!({"type":"speech.audio.done","usage":{"input_tokens":2,"output_tokens":1,"total_tokens":3}}),
                ),
                CanonicalEvent::new(2, CanonicalEventKind::Done),
            ],
            false,
        );
        let response = post_json(
            &state,
            &key,
            "/openai/v1/audio/speech",
            r#"{"model":"default","input":"hello","voice":"coral","stream_format":"sse"}"#,
        )
        .await;
        let body = response_text(response).await;
        assert!(body.contains("event: speech.audio.delta"));
        assert!(body.contains("\"audio\":\"bXAz\""));
        assert!(body.contains("event: speech.audio.done"));
        let event = usage.recv_next().await.unwrap();
        assert_eq!(event.operation, OperationKind::Speech);
        assert_eq!(event.input_tokens, Some(2));
        assert!(event.usage_complete);

        install_event_stream(
            &state,
            OperationKind::Transcription,
            vec![
                raw_media_event(
                    0,
                    "transcript.text.delta",
                    json!({"type":"transcript.text.delta","delta":"hello"}),
                ),
                raw_media_event(
                    1,
                    "transcript.text.done",
                    json!({"type":"transcript.text.done","text":"hello","usage":{"input_tokens":3,"output_tokens":1,"total_tokens":4}}),
                ),
                CanonicalEvent::new(2, CanonicalEventKind::Done),
            ],
            false,
        );
        let response = post_multipart(
            &state,
            &key,
            "/openai/v1/audio/transcriptions",
            multipart(
                &[("model", "default"), ("stream", "true")],
                "file",
                "wave-bytes",
            ),
        )
        .await;
        let body = response_text(response).await;
        assert!(body.contains("event: transcript.text.delta"));
        assert!(body.contains("event: transcript.text.done"));
        let event = usage.recv_next().await.unwrap();
        assert_eq!(event.operation, OperationKind::Transcription);
        assert_eq!(event.input_tokens, Some(3));
        assert!(event.usage_complete);
    }

    #[tokio::test]
    async fn selected_openai_unary_surfaces_route_and_encode_native_results() {
        let (state, key) = test_state(false);

        install_result(
            &state,
            OperationKind::TokenCount,
            CanonicalResult::TokenCount(olp_domain::TokenCountResult {
                input_tokens: 9,
                extensions: olp_domain::SourceExtensions::new(Surface::OpenAi, BTreeMap::new()),
            }),
        );
        let response = post_json(
            &state,
            &key,
            "/openai/v1/responses/input_tokens",
            r#"{"model":"default","input":"hello"}"#,
        )
        .await;
        assert_eq!(response.status(), StatusCode::OK);
        let body: Value =
            serde_json::from_slice(&response.into_body().collect().await.unwrap().to_bytes())
                .unwrap();
        assert_eq!(body["input_tokens"], 9);

        install_result(
            &state,
            OperationKind::Moderation,
            CanonicalResult::Moderation(olp_domain::ModerationResult {
                id: Some("modr-upstream".to_owned()),
                model: Some("omni-moderation-latest".to_owned()),
                results: vec![olp_domain::ModerationItem {
                    flagged: true,
                    categories: BTreeMap::from([("violence".to_owned(), true)]),
                    category_scores: BTreeMap::from([("violence".to_owned(), 0.9)]),
                }],
                extensions: olp_domain::SourceExtensions::new(Surface::OpenAi, BTreeMap::new()),
            }),
        );
        let response = post_json(
            &state,
            &key,
            "/openai/v1/moderations",
            r#"{"model":"default","input":"hello"}"#,
        )
        .await;
        assert_eq!(response.status(), StatusCode::OK);
        let body: Value =
            serde_json::from_slice(&response.into_body().collect().await.unwrap().to_bytes())
                .unwrap();
        assert_eq!(body["model"], "default");
        assert_eq!(body["results"][0]["flagged"], true);

        let image_result = || {
            CanonicalResult::Images(olp_domain::ImagesResult {
                created_at: Some(1_800_000_000),
                images: vec![olp_domain::ImageArtifact {
                    source: olp_domain::MediaSource::Uri(
                        "https://images.example/result.png".into(),
                    ),
                    revised_prompt: Some("revised".into()),
                }],
                usage: None,
                extensions: olp_domain::SourceExtensions::new(Surface::OpenAi, BTreeMap::new()),
            })
        };
        install_result(&state, OperationKind::ImageGeneration, image_result());
        let response = post_json(
            &state,
            &key,
            "/openai/v1/images/generations",
            r#"{"model":"default","prompt":"cobalt fox"}"#,
        )
        .await;
        assert_eq!(response.status(), StatusCode::OK);
        let body: Value =
            serde_json::from_slice(&response.into_body().collect().await.unwrap().to_bytes())
                .unwrap();
        assert_eq!(body["data"][0]["url"], "https://images.example/result.png");

        install_result(&state, OperationKind::ImageEdit, image_result());
        let response = post_multipart(
            &state,
            &key,
            "/openai/v1/images/edits",
            multipart(
                &[("model", "default"), ("prompt", "edit this")],
                "image",
                "image-bytes",
            ),
        )
        .await;
        assert_eq!(response.status(), StatusCode::OK);

        install_result(&state, OperationKind::ImageVariation, image_result());
        let response = post_multipart(
            &state,
            &key,
            "/openai/v1/images/variations",
            multipart(&[("model", "default")], "image", "image-bytes"),
        )
        .await;
        assert_eq!(response.status(), StatusCode::OK);

        install_result(
            &state,
            OperationKind::Transcription,
            CanonicalResult::Transcription(olp_domain::TranscriptionResult {
                text: "transcribed".to_owned(),
                language: Some("en".to_owned()),
                duration_seconds: Some(1.0),
                segments: Vec::new(),
                extensions: olp_domain::SourceExtensions::new(Surface::OpenAi, BTreeMap::new()),
            }),
        );
        let response = post_multipart(
            &state,
            &key,
            "/openai/v1/audio/transcriptions",
            multipart(&[("model", "default")], "file", "audio-bytes"),
        )
        .await;
        assert_eq!(response.status(), StatusCode::OK);
        let body: Value =
            serde_json::from_slice(&response.into_body().collect().await.unwrap().to_bytes())
                .unwrap();
        assert_eq!(body["text"], "transcribed");
    }

    #[tokio::test]
    async fn speech_surface_streams_bounded_spooled_result() {
        let (state, key) = test_state(false);
        let artifact = state
            .media_spool
            .put(olp_domain::MediaUpload {
                filename: "speech.mp3".into(),
                content_type: Some("audio/mpeg".into()),
                maximum_length: 32,
                bytes: Box::pin(stream::once(async {
                    Ok(Bytes::from_static(b"audio-result"))
                })),
            })
            .await
            .unwrap();
        install_result(
            &state,
            OperationKind::Speech,
            CanonicalResult::Speech(olp_domain::SpeechResult {
                audio: artifact,
                extensions: olp_domain::SourceExtensions::new(Surface::OpenAi, BTreeMap::new()),
            }),
        );
        let response = post_json(
            &state,
            &key,
            "/openai/v1/audio/speech",
            r#"{"model":"default","input":"hello","voice":"coral"}"#,
        )
        .await;
        assert_eq!(response.status(), StatusCode::OK);
        assert_eq!(response.headers()[header::CONTENT_TYPE], "audio/mpeg");
        assert_eq!(
            response.into_body().collect().await.unwrap().to_bytes(),
            Bytes::from_static(b"audio-result")
        );
    }

    #[tokio::test]
    async fn malformed_multipart_is_rejected_before_routing() {
        let (state, key) = test_state(false);
        let response = crate::public_router(state)
            .oneshot(
                Request::post("/openai/v1/images/edits")
                    .header(header::AUTHORIZATION, format!("Bearer {key}"))
                    .header(header::CONTENT_TYPE, "multipart/form-data")
                    .body(Body::from("not-a-multipart-body"))
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(response.status(), StatusCode::BAD_REQUEST);
    }

    #[tokio::test]
    async fn failed_multipart_validation_removes_staged_files() {
        let spool = crate::media_spool::FileMediaSpool::create().unwrap();
        let artifact = spool
            .put(olp_domain::MediaUpload {
                filename: "upload.png".to_owned(),
                content_type: Some("image/png".to_owned()),
                maximum_length: 16,
                bytes: Box::pin(stream::once(async { Ok(Bytes::from_static(b"staged")) })),
            })
            .await
            .unwrap();
        let mut form =
            MultipartFormData::new(spool.clone(), MultipartRequestAdmission::unrestricted());
        form.cleanup_handles.push(artifact.handle.clone());
        drop(form);

        tokio::time::timeout(Duration::from_secs(1), async {
            loop {
                match spool.open(&artifact.handle).await {
                    Err(olp_domain::MediaSpoolError::NotFound) => break,
                    Ok(_) => tokio::task::yield_now().await,
                    Err(error) => panic!("unexpected spool cleanup error: {error}"),
                }
            }
        })
        .await
        .expect("multipart error cleanup must remove the staged file promptly");
    }

    #[tokio::test]
    async fn dropping_client_stream_drops_upstream_within_one_second() {
        let (state, key) = test_state(true);
        let dropped = Arc::new(AtomicBool::new(false));
        install_transport(
            &state,
            Arc::new(DropAwareTransport {
                dropped: dropped.clone(),
            }),
        );
        let response = crate::public_router(state)
            .oneshot(
                Request::post("/openai/v1/chat/completions")
                    .header(header::AUTHORIZATION, format!("Bearer {key}"))
                    .header(header::CONTENT_TYPE, "application/json")
                    .body(Body::from(
                        r#"{"model":"default","messages":[{"role":"user","content":"hi"}],"stream":true}"#,
                    ))
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(response.status(), StatusCode::OK);
        drop(response);
        tokio::time::timeout(Duration::from_secs(1), async {
            while !dropped.load(Ordering::Acquire) {
                tokio::task::yield_now().await;
            }
        })
        .await
        .expect("client cancellation must promptly drop the upstream stream");
    }
}
