use std::{sync::Arc, time::Duration};

use axum::{
    Json,
    body::Bytes,
    extract::{Extension, State, rejection::JsonRejection},
    http::StatusCode,
    response::{IntoResponse, Response},
};
use chrono::Utc;
use futures::StreamExt;
use olp_domain::{
    CanonicalEvent, CanonicalEventKind, OperationKind, RequestId, RequestMetadata, RouteSlug,
    Surface, TransportMode, authorize_api_key,
};
use olp_protocols::openai::{ChatCompletionRequest, decode_chat_completion};
use olp_storage::{LimitLease, RequestAttemptMetadata};

use crate::{
    GatewayState, InferencePrincipal,
    json_media::{admit_openai_chat, cleanup_admitted},
    semantic_validation::select_representable_attempts_filtered,
    streaming_response::{TerminalFrames, sse_stream},
};

use super::{
    error::InferenceError,
    failover::{EventStream, ExecutionOutput, ExecutionSuccess, execute_with_failover},
    limits::{RequestMediaGuard, operation_media_handles, release_limits, reserve_limits},
    openai_chat_response::{OpenAiChatCompletionStreamEncoder, aggregate_chat_completion_response},
    openai_http::error_sse as openai_error_sse,
    telemetry::{UsageCapture, elapsed_ms, emit_request_metadata_event},
};

pub(super) async fn chat_completions(
    State(state): State<GatewayState>,
    Extension(principal): Extension<InferencePrincipal>,
    payload: Result<Json<ChatCompletionRequest>, JsonRejection>,
) -> Result<Response, InferenceError> {
    let snapshot = Arc::clone(principal.runtime());
    let key = principal.key();
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
            emit_request_metadata_event(
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
                OperationKind::Generation,
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
            emit_request_metadata_event(
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
                OperationKind::Generation,
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
    if let Err(error) = authorize_api_key(key, Some(&route_slug), operation.kind(), Utc::now()) {
        let failure = InferenceError::forbidden(error.to_string());
        emit_request_metadata_event(
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
            OperationKind::Generation,
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
        key,
        &operation,
        principal.lookup_id().as_str(),
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
            emit_request_metadata_event(
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
                OperationKind::Generation,
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
            emit_request_metadata_event(
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
                OperationKind::Generation,
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

#[allow(clippy::too_many_arguments)]
fn streaming_response(
    state: GatewayState,
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
    attempts: Vec<RequestAttemptMetadata>,
    attempt_started: tokio::time::Instant,
) -> Response {
    let (writer, response) = sse_stream();
    tokio::spawn(async move {
        let mut encoder = OpenAiChatCompletionStreamEncoder::new(request_id, route_slug.as_str());
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
        emit_request_metadata_event(
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
            OperationKind::Generation,
        );
        release_limits(&state, lease.as_ref()).await;
    });
    response
}

#[allow(clippy::too_many_arguments)]
async fn unary_response(
    state: &GatewayState,
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
    attempts: &[RequestAttemptMetadata],
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
                    emit_request_metadata_event(
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
                        OperationKind::Generation,
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
                    emit_request_metadata_event(
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
                        OperationKind::Generation,
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
        emit_request_metadata_event(
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
            OperationKind::Generation,
        );
        return Err(failure);
    }
    let response =
        match aggregate_chat_completion_response(request_id, route_slug.as_str(), &collected) {
            Ok(response) => response,
            Err(failure) => {
                emit_request_metadata_event(
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
                    OperationKind::Generation,
                );
                return Err(failure);
            }
        };
    emit_request_metadata_event(
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
        OperationKind::Generation,
    );
    Ok((StatusCode::OK, Json(response)).into_response())
}
