use axum::{
    Json,
    body::Bytes,
    extract::{Extension, State, rejection::JsonRejection},
    http::StatusCode,
    response::{IntoResponse, Response},
};
use olp_engine::domain::canonical::{
    events::{Event, Kind},
    identity::TransportMode,
};
use olp_engine::inference::{execution::RoutedEvents, principal::Principal};
use olp_engine::protocols::openai::chat::{CompletionRequest, decode};

use crate::{
    bootstrap::mode_dependencies::GatewayState,
    public_http::json_media::{admit_openai_chat, cleanup_admitted},
    public_http::streaming_response::{ProtocolStreamEncoder, protocol_streaming_response},
};

use super::{
    error::InferenceError,
    execution::execute_event_operation,
    openai_chat_response::{OpenAiChatCompletionStreamEncoder, aggregate_chat_completion_response},
    openai_http::error_sse as openai_error_sse,
};

pub(super) async fn chat_completions(
    State(state): State<GatewayState>,
    Extension(principal): Extension<Principal>,
    payload: Result<Json<CompletionRequest>, JsonRejection>,
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
    let operation = match decode::chat_completion(wire_request) {
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

fn streaming_response(execution: RoutedEvents) -> Response {
    let encoder =
        OpenAiChatCompletionStreamEncoder::new(execution.request_id, execution.route_slug.as_str());
    protocol_streaming_response(execution, encoder)
}

impl ProtocolStreamEncoder for OpenAiChatCompletionStreamEncoder {
    fn push(&mut self, event: Event) -> Result<Vec<Bytes>, InferenceError> {
        let is_error = matches!(event.kind, Kind::Error { .. });
        let mut encoded = self.encode(event)?;
        if is_error {
            encoded.push(Bytes::from_static(b"data: [DONE]\n\n"));
        }
        Ok(encoded)
    }

    fn encode_error(&self, error: &InferenceError) -> Vec<Bytes> {
        vec![
            openai_error_sse(error),
            Bytes::from_static(b"data: [DONE]\n\n"),
        ]
    }
}

async fn unary_response(execution: RoutedEvents) -> Result<Response, InferenceError> {
    let mut completed = execution.collect().await.map_err(InferenceError::from)?;
    let response = aggregate_chat_completion_response(
        completed.request_id,
        completed.route_slug.as_str(),
        &completed.events,
    )?;
    completed.mark_success();
    Ok((StatusCode::OK, Json(response)).into_response())
}
