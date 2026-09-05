use crate::public_http::request_admission::HttpRequestAdmission;
use axum::{
    Json,
    body::Bytes,
    extract::{Extension, State, rejection::JsonRejection},
    http::StatusCode,
    response::{IntoResponse, Response},
};
use olp_engine::domain::canonical::{events::Event, identity::TransportMode};
use olp_engine::inference::execution::RoutedEvents;
use olp_engine::protocols::openai::chat::{CompletionRequest, decode};

use crate::{
    gateway::state::GatewayState,
    public_http::{
        json_media::admit_openai_chat,
        streaming_response::{
            ProtocolStreamEncoder, TerminalFrames, precommit_stream_failure,
            protocol_streaming_response,
        },
    },
};

use super::{
    error::InferenceError,
    execution::execute_event_operation,
    openai_chat_response::{OpenAiChatCompletionStreamEncoder, aggregate_chat_completion_response},
    openai_http::error_sse as openai_error_sse,
};

pub(super) async fn chat_completions(
    State(state): State<GatewayState>,
    Extension(principal): Extension<HttpRequestAdmission>,
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
    // OpenAI only appends the trailing usage-only chunk when the client asked
    // for it. The upstream request always sets it so accounting stays exact.
    let include_usage = wire_request
        .extra
        .get("stream_options")
        .and_then(|options| options.get("include_usage"))
        .and_then(serde_json::Value::as_bool)
        .unwrap_or(false);
    let operation = match decode::chat_completion(wire_request) {
        Ok(operation) => operation,
        Err(error) => {
            admitted.release().await;
            return Err(InferenceError::invalid_request(error.to_string()));
        }
    };
    admitted.disarm();
    let mode = if streaming {
        TransportMode::Streaming
    } else {
        TransportMode::Unary
    };
    let execution = execute_event_operation(&state, &principal, operation, mode).await?;
    if streaming {
        let execution = precommit_stream_failure(execution)?;
        Ok(streaming_response(execution, include_usage))
    } else {
        unary_response(execution).await
    }
}

fn streaming_response(execution: RoutedEvents, include_usage: bool) -> Response {
    let encoder = OpenAiChatCompletionStreamEncoder::new(
        execution.request_id,
        execution.route_slug.as_str(),
        include_usage,
    );
    protocol_streaming_response(execution, encoder)
}

impl ProtocolStreamEncoder for OpenAiChatCompletionStreamEncoder {
    fn push(&mut self, event: Event) -> Result<Vec<Bytes>, InferenceError> {
        self.encode(event)
    }

    fn encode_error(&self, error: &InferenceError) -> Bytes {
        openai_error_sse(error)
    }

    /// Chat streams always end in `[DONE]`; the encoder emits it for a
    /// provider `Done`, so only an error terminal needs one appended.
    fn terminal_tail(&self, failure: Option<&InferenceError>) -> Vec<Bytes> {
        failure
            .map(|_| Bytes::from_static(b"data: [DONE]\n\n"))
            .into_iter()
            .collect()
    }

    fn error_frames(&self, error: &InferenceError) -> TerminalFrames {
        TerminalFrames::new(vec![
            openai_error_sse(error),
            Bytes::from_static(b"data: [DONE]\n\n"),
        ])
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
