use axum::{
    Json,
    extract::{Extension, State, rejection::JsonRejection},
    http::StatusCode,
    response::{IntoResponse, Response},
};
use bytes::{Bytes, BytesMut};
use olp_domain::{CanonicalEvent, CanonicalEventKind, OperationKind, TransportMode};
use olp_protocols::openai::{ChatCompletionRequest, decode_chat_completion};

use crate::{
    GatewayState,
    event_completion::collect_event_execution,
    json_media::{admit_openai_chat, cleanup_admitted},
    streaming_response::{ProtocolStreamEncoder, protocol_streaming_response},
};

use super::{
    error::{InferenceError, valid_json},
    execute_event_operation,
    execution::{RoutedEventExecution, authorize_principal},
    openai_chat_response::{OpenAiChatCompletionStreamEncoder, aggregate_chat_completion_response},
    openai_http::error_sse as openai_error_sse,
};

const DONE: &[u8] = b"data: [DONE]\n\n";

pub(super) async fn chat_completions(
    State(state): State<GatewayState>,
    Extension(principal): Extension<crate::InferencePrincipal>,
    payload: Result<Json<ChatCompletionRequest>, JsonRejection>,
) -> Result<Response, InferenceError> {
    let _ = authorize_principal(&principal, OperationKind::Generation, None)?;
    let Json(mut request) = valid_json(payload)?;
    let streaming = request.stream;
    let admitted = admit_openai_chat(&state, &mut request).await?;
    let operation = match decode_chat_completion(request) {
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
        unary_response(&state, execution).await
    }
}

fn streaming_response(execution: RoutedEventExecution) -> Response {
    let encoder = OpenAiChatHttpStreamEncoder(OpenAiChatCompletionStreamEncoder::new(
        execution.request_id,
        execution.route_slug.as_str(),
    ));
    protocol_streaming_response(execution, encoder)
}

struct OpenAiChatHttpStreamEncoder(OpenAiChatCompletionStreamEncoder);

impl ProtocolStreamEncoder for OpenAiChatHttpStreamEncoder {
    fn push(&mut self, event: CanonicalEvent) -> Result<Vec<Bytes>, String> {
        let is_error = matches!(event.kind, CanonicalEventKind::Error { .. });
        let mut frames = self
            .0
            .encode(event)
            .map_err(|error| error.message().to_owned())?;
        if is_error {
            frames.push(Bytes::from_static(DONE));
        }
        Ok(frames)
    }

    fn encode_error(&self, error: &InferenceError) -> Bytes {
        let encoded = openai_error_sse(error);
        let mut output = BytesMut::with_capacity(encoded.len() + DONE.len());
        output.extend_from_slice(&encoded);
        output.extend_from_slice(DONE);
        output.freeze()
    }
}

async fn unary_response(
    state: &GatewayState,
    execution: RoutedEventExecution,
) -> Result<Response, InferenceError> {
    let mut completed = collect_event_execution(state, execution).await?;
    let response = aggregate_chat_completion_response(
        completed.request_id,
        completed.route_slug.as_str(),
        &completed.events,
    );
    match response {
        Ok(response) => {
            completed.mark_success();
            Ok((StatusCode::OK, Json(response)).into_response())
        }
        Err(failure) => Err(failure),
    }
}
