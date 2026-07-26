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
use olp_storage::RequestAttemptMetadata;

use crate::{
    GatewayState, InferencePrincipal,
    event_completion::{MAX_COLLECTED_CANONICAL_EVENT_BYTES, collected_event_bytes},
    json_media::{admit_openai_chat, cleanup_admitted},
    semantic_validation::select_representable_attempts_filtered,
    streaming_response::{TerminalFrames, sse_stream},
};

use super::{
    error::InferenceError,
    failover::{
        EventStream, ExecutionOutput, ExecutionSuccess, FailoverContext, execute_with_failover,
    },
    limits::{RequestMediaGuard, operation_media_handles, release_limits, reserve_limits},
    openai_chat_response::{OpenAiChatCompletionStreamEncoder, aggregate_chat_completion_response},
    openai_http::error_sse as openai_error_sse,
    telemetry::{RequestAccountingGuard, UsageCapture, elapsed_ms, emit_request_metadata_event},
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
    let mut lease = reserve_limits(
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
            release_limits(&state, lease.as_ref(), None).await;
            return Err(failure);
        }
    };
    let route = snapshot
        .routes
        .get(&route_slug)
        .expect("attempt selection returned a known route");
    let mut accounting = RequestAccountingGuard::new(
        &state,
        snapshot.generation.id.as_uuid(),
        key.id.as_uuid(),
        request_id.as_uuid(),
        route_slug.clone(),
        request_started_at,
        request_started,
        Surface::OpenAi,
        OperationKind::Generation,
        lease.take(),
    );

    let metadata = RequestMetadata {
        request_id,
        operation: OperationKind::Generation,
        surface: Surface::OpenAi,
        mode,
    };
    let execution = {
        let mut record_attempt_started =
            |completed: &[RequestAttemptMetadata],
             attempt: &olp_domain::AttemptPlan,
             ordinal: u16,
             started_at: chrono::DateTime<chrono::Utc>,
             started: tokio::time::Instant| {
                accounting.record_attempt_started(
                    completed,
                    ordinal,
                    attempt.provider_id.as_uuid(),
                    &attempt.upstream_model,
                    started_at,
                    started,
                );
            };
        execute_with_failover(
            FailoverContext {
                runtime: &snapshot,
                overall_timeout: route.overall_timeout.as_duration(),
                media_spool: state.media_spool.clone(),
                circuits: &state.circuits,
                on_attempt_started: Some(&mut record_attempt_started),
            },
            attempts,
            metadata,
            operation,
        )
        .await
    };
    let first_byte_ms = elapsed_ms(request_started.elapsed());
    match &execution {
        Ok(success) => accounting.record_attempts(
            success.attempts.clone(),
            Some(success.attempt_started),
            Some(first_byte_ms),
            true,
        ),
        Err(failure) => {
            accounting.record_attempts(failure.attempts.clone(), None, None, false);
        }
    }
    request_media.cleanup().await;
    let ExecutionSuccess {
        output, deadline, ..
    } = match execution {
        Ok(execution) => execution,
        Err(failure) => {
            accounting.finish(Some(&failure.error)).await;
            return Err(failure.error);
        }
    };
    let ExecutionOutput::Events { first, events } = output else {
        let failure = InferenceError::bad_gateway(
            "provider_protocol_error",
            "The provider returned an incompatible unary result.",
        );
        accounting.finish(Some(&failure)).await;
        return Err(failure);
    };

    if streaming {
        crate::claim_http_inference_metadata();
        Ok(streaming_response(
            request_id.as_uuid(),
            route_slug,
            first,
            events,
            deadline,
            accounting,
        ))
    } else {
        unary_response(
            request_id.as_uuid(),
            &route_slug,
            first,
            events,
            deadline,
            accounting,
        )
        .await
    }
}

fn streaming_response(
    request_id: uuid::Uuid,
    route_slug: RouteSlug,
    first: CanonicalEvent,
    mut events: EventStream,
    deadline: tokio::time::Instant,
    mut accounting: RequestAccountingGuard,
) -> Response {
    let (writer, response) = sse_stream();
    tokio::spawn(async move {
        let mut encoder = OpenAiChatCompletionStreamEncoder::new(request_id, route_slug.as_str());
        let mut next = Some(Ok(first));
        let mut failure = None;
        let mut terminal = None;
        'provider: while let Some(item) = next {
            match item {
                Ok(event) => {
                    let is_done = matches!(event.kind, CanonicalEventKind::Done);
                    accounting.usage_mut().observe(&event);
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
        accounting.finish(failure.as_ref()).await;
    });
    response
}

async fn unary_response(
    request_id: uuid::Uuid,
    route_slug: &RouteSlug,
    first: CanonicalEvent,
    mut events: EventStream,
    deadline: tokio::time::Instant,
    mut accounting: RequestAccountingGuard,
) -> Result<Response, InferenceError> {
    let mut collected_bytes =
        match collected_event_bytes(0, &first, MAX_COLLECTED_CANONICAL_EVENT_BYTES) {
            Ok(collected_bytes) => collected_bytes,
            Err(failure) => {
                accounting.finish(Some(&failure)).await;
                return Err(failure);
            }
        };
    let mut collected = vec![first];
    accounting.usage_mut().observe(&collected[0]);
    if !matches!(collected[0].kind, CanonicalEventKind::Done) {
        loop {
            let item = match tokio::time::timeout_at(deadline, events.next()).await {
                Ok(item) => item,
                Err(_) => {
                    let failure = InferenceError::timeout();
                    accounting.finish(Some(&failure)).await;
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
                    accounting.finish(Some(&failure)).await;
                    return Err(failure);
                }
            };
            let terminal = matches!(event.kind, CanonicalEventKind::Done);
            accounting.usage_mut().observe(&event);
            collected_bytes = match collected_event_bytes(
                collected_bytes,
                &event,
                MAX_COLLECTED_CANONICAL_EVENT_BYTES,
            ) {
                Ok(collected_bytes) => collected_bytes,
                Err(failure) => {
                    accounting.finish(Some(&failure)).await;
                    return Err(failure);
                }
            };
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
        accounting.finish(Some(&failure)).await;
        return Err(failure);
    }
    let response =
        match aggregate_chat_completion_response(request_id, route_slug.as_str(), &collected) {
            Ok(response) => response,
            Err(failure) => {
                accounting.finish(Some(&failure)).await;
                return Err(failure);
            }
        };
    accounting.finish(None).await;
    Ok((StatusCode::OK, Json(response)).into_response())
}
