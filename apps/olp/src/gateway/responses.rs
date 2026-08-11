use axum::{
    Json,
    body::Bytes,
    extract::{Extension, State, rejection::JsonRejection},
    http::StatusCode,
    response::{IntoResponse, Response},
};
use olp_engine::domain::{
    CanonicalEvent, CanonicalResult, GatewayCapability, Operation, TransportMode,
};
use olp_engine::protocols::openai::{
    OpenAiResponsesStreamEncoder, ResponseCreateRequest, ResponseInputTokensRequest,
    decode_response_create, decode_response_input_tokens, encode_response_input_tokens_result,
    encode_response_object,
};
use serde_json::{Value, json};

use crate::{
    GatewayState, InferencePrincipal,
    public_http::json_media::{
        admit_openai_response_input_tokens, admit_openai_responses, cleanup_admitted,
    },
    public_http::streaming_response::{
        ProtocolStreamEncoder, encode_protocol_sse_frames, encode_server_sse_frame,
        protocol_streaming_response,
    },
};

use super::{
    error::{InferenceError, valid_json},
    execution::{
        RoutedEventExecution, authorize_principal, execute_event_operation, execute_unary_result,
        incompatible_result, mark_unary_outcome,
    },
    openai_http::unix_seconds,
};

pub(super) async fn responses(
    State(state): State<GatewayState>,
    Extension(principal): Extension<InferencePrincipal>,
    payload: Result<Json<ResponseCreateRequest>, JsonRejection>,
) -> Result<Response, InferenceError> {
    let _ = authorize_principal(&state, &principal, GatewayCapability::Inference, None)?;
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
    let execution = execute_event_operation(&state, &principal, operation, mode).await?;
    if streaming {
        Ok(responses_streaming_response(execution))
    } else {
        responses_unary_response(execution).await
    }
}

async fn responses_unary_response(
    execution: RoutedEventExecution,
) -> Result<Response, InferenceError> {
    let mut completed = execution.collect().await.map_err(InferenceError::from)?;
    let response = encode_response_object(
        &completed.events,
        completed.route_slug.as_str(),
        &format!("resp_{}", completed.request_id.simple()),
    )
    .map_err(|error| InferenceError::bad_gateway("provider_protocol_error", error.to_string()));
    match response {
        Ok(response) => {
            completed.mark_success();
            Ok((StatusCode::OK, Json(response)).into_response())
        }
        Err(failure) => Err(failure),
    }
}

fn responses_streaming_response(execution: RoutedEventExecution) -> Response {
    let encoder = OpenAiResponsesHttpStreamEncoder(OpenAiResponsesStreamEncoder::new(
        execution.route_slug.as_str(),
        format!("resp_{}", execution.request_id.simple()),
        unix_seconds(),
    ));
    protocol_streaming_response(execution, encoder)
}

struct OpenAiResponsesHttpStreamEncoder(OpenAiResponsesStreamEncoder);

impl ProtocolStreamEncoder for OpenAiResponsesHttpStreamEncoder {
    fn push(&mut self, event: CanonicalEvent) -> Result<Vec<Bytes>, String> {
        encode_protocol_sse_frames(self.0.push(event))
    }

    fn encode_error(&self, error: &InferenceError) -> Bytes {
        responses_error_sse(error)
    }
}

fn responses_error_sse(error: &InferenceError) -> Bytes {
    encode_server_sse_frame(&olp_engine::protocols::sse::SseFrame {
        event: Some("error".to_owned()),
        data: json!({
            "type": "error",
            "code": error.code(),
            "message": error.message(),
            "param": null
        })
        .to_string(),
        id: None,
        retry_ms: None,
    })
}

pub(super) async fn response_input_tokens(
    State(state): State<GatewayState>,
    Extension(principal): Extension<InferencePrincipal>,
    payload: Result<Json<ResponseInputTokensRequest>, JsonRejection>,
) -> Result<Response, InferenceError> {
    let _ = authorize_principal(&state, &principal, GatewayCapability::Inference, None)?;
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
    let mut executed = execute_unary_result(&state, &principal, operation).await?;
    let CanonicalResult::TokenCount(result) = executed.result.as_ref() else {
        executed.mark_provider_protocol_failure();
        return Err(incompatible_result("token count"));
    };
    let response = encode_response_input_tokens_result(result)
        .map_err(|error| InferenceError::bad_gateway("provider_protocol_error", error.to_string()));
    mark_unary_outcome(&mut executed, &response);
    let response = response?;
    Ok((StatusCode::OK, Json(response)).into_response())
}
