use crate::http::request_admission::HttpRequestAdmission;
use crate::inference::execution::RoutedEvents;
use crate::protocols::canonical::events::Event;
use crate::protocols::canonical::identity::TransportMode;
use crate::protocols::openai::chat::CompletionRequest;
use crate::protocols::openai::chat::decode;
use axum::Json;
use axum::body::Bytes;
use axum::extract::Extension;
use axum::extract::State;
use axum::extract::rejection::JsonRejection;
use axum::http::StatusCode;
use axum::response::IntoResponse;
use axum::response::Response;

use crate::http::json_media::admit_openai_chat;
use crate::http::streaming_response::ProtocolStreamEncoder;
use crate::http::streaming_response::TerminalFrames;
use crate::http::streaming_response::precommit_stream_failure;
use crate::http::streaming_response::protocol_streaming_response;
use crate::inference::http::state::GatewayState;

use crate::inference::http::error::InferenceError;
use crate::inference::http::error::valid_json;
use crate::inference::http::execution::execute_event_operation;
use crate::inference::http::openai_chat_response::OpenAiChatCompletionStreamEncoder;
use crate::inference::http::openai_chat_response::aggregate_chat_completion_response;
use crate::inference::http::openai_http::error_sse as openai_error_sse;

pub(crate) async fn chat_completions(
    State(state): State<GatewayState>,
    Extension(principal): Extension<HttpRequestAdmission>,
    payload: Result<Json<CompletionRequest>, JsonRejection>,
) -> Result<Response, InferenceError> {
    let Json(mut wire_request) = valid_json(payload)?;
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
