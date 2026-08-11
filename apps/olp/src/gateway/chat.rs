use axum::{
    Json,
    body::Bytes,
    extract::{Extension, State, rejection::JsonRejection},
    http::StatusCode,
    response::{IntoResponse, Response},
};
use futures::{StreamExt, stream};
use olp_engine::domain::{CanonicalEventKind, TransportMode};
use olp_engine::inference::{RequestOutcome, RoutedEventExecution};
use olp_engine::protocols::openai::{ChatCompletionRequest, decode_chat_completion};

use crate::{
    GatewayState, InferencePrincipal,
    public_http::json_media::{admit_openai_chat, cleanup_admitted},
    public_http::streaming_response::{TerminalFrames, sse_stream},
};

use super::{
    error::InferenceError,
    execution::execute_event_operation,
    openai_chat_response::{OpenAiChatCompletionStreamEncoder, aggregate_chat_completion_response},
    openai_http::error_sse as openai_error_sse,
};

pub(super) async fn chat_completions(
    State(state): State<GatewayState>,
    Extension(principal): Extension<InferencePrincipal>,
    payload: Result<Json<ChatCompletionRequest>, JsonRejection>,
) -> Result<Response, InferenceError> {
    let Json(mut wire_request) = match payload {
        Ok(payload) => payload,
        Err(error) => {
            return Err(InferenceError::invalid_request(format!(
                "The JSON request is invalid: {error}"
            )));
        }
    };
    let admitted = admit_openai_chat(&state, &mut wire_request).await?;
    let streaming = wire_request.stream;
    let operation = match decode_chat_completion(wire_request) {
        Ok(operation) => operation,
        Err(error) => {
            cleanup_admitted(&state, admitted).await;
            return Err(InferenceError::invalid_request(error.to_string()));
        }
    };
    let mode = if streaming {
        TransportMode::Streaming
    } else {
        TransportMode::Unary
    };
    let execution = execute_event_operation(&state, &principal, operation, mode).await?;
    if streaming {
        Ok(streaming_response(execution))
    } else {
        unary_response(execution).await
    }
}

fn streaming_response(mut execution: RoutedEventExecution) -> Response {
    let (writer, response) = sse_stream();
    tokio::spawn(async move {
        let mut accounting = execution.take_accounting();
        let mut events = std::mem::replace(&mut execution.events, Box::pin(stream::empty()));
        let mut encoder = OpenAiChatCompletionStreamEncoder::new(
            execution.request_id,
            execution.route_slug.as_str(),
        );
        let mut next = Some(Ok(execution.first.clone()));
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
                        if let Err(error) = writer.send_or_fail(bytes, execution.deadline).await {
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
                () = tokio::time::sleep_until(execution.deadline) => {
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
        let outcome = failure
            .as_ref()
            .map_or_else(RequestOutcome::success, InferenceError::accounting_outcome);
        accounting.finish(outcome).await;
    });
    response
}

async fn unary_response(execution: RoutedEventExecution) -> Result<Response, InferenceError> {
    let mut completed = execution.collect().await.map_err(InferenceError::from)?;
    let response = aggregate_chat_completion_response(
        completed.request_id,
        completed.route_slug.as_str(),
        &completed.events,
    )?;
    completed.mark_success();
    Ok((StatusCode::OK, Json(response)).into_response())
}
