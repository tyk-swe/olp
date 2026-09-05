use crate::public_http::request_admission::HttpRequestAdmission;
use axum::{
    Json,
    body::Bytes,
    extract::{Extension, State, rejection::JsonRejection},
    http::StatusCode,
    response::{IntoResponse, Response},
};
use olp_engine::domain::{
    auth::GatewayCapability,
    canonical::{
        events::Event,
        identity::TransportMode,
        requests::{OPENAI_ENDPOINT_EXTENSION, Operation},
        results::CanonicalResult,
    },
};
use olp_engine::inference::execution::RoutedEvents;
use olp_engine::protocols::openai::{
    client::{Encoder, encode_response_object},
    responses::{
        request::{Create, decode_response_create},
        token_count::{
            ResponseInputTokensRequest, decode_response_input_tokens,
            encode_response_input_tokens_result,
        },
    },
};
use serde_json::{Value, json};

use crate::{
    gateway::state::GatewayState,
    public_http::{
        json_media::{admit_openai_response_input_tokens, admit_openai_responses},
        streaming_response::{
            ProtocolStreamEncoder, encode_protocol_sse_frames, encode_server_sse_frame,
            precommit_stream_failure, protocol_streaming_response,
        },
    },
};

use super::{
    error::{InferenceError, valid_json},
    execution::{
        authorize_principal, execute_event_operation, execute_unary_result, incompatible_result,
        mark_unary_outcome,
    },
    openai_http::unix_seconds,
};

pub(super) async fn responses(
    State(state): State<GatewayState>,
    Extension(principal): Extension<HttpRequestAdmission>,
    payload: Result<Json<Create>, JsonRejection>,
) -> Result<Response, InferenceError> {
    let _ = authorize_principal(&state, &principal, GatewayCapability::Inference, None)?;
    let Json(mut request) = valid_json(payload)?;
    let streaming = request.stream;
    let admitted = admit_openai_responses(&state, &mut request).await?;
    let mut operation = match decode_response_create(request) {
        Ok(operation) => operation,
        Err(error) => {
            admitted.release().await;
            return Err(InferenceError::invalid_request(error.to_string()));
        }
    };
    admitted.disarm();
    let Operation::Generation(generation) = &mut operation else {
        unreachable!("the Responses codec always produces generation")
    };
    generation.extensions.values.insert(
        OPENAI_ENDPOINT_EXTENSION.into(),
        Value::String("responses".into()),
    );
    let mode = if streaming {
        TransportMode::Streaming
    } else {
        TransportMode::Unary
    };
    let execution = execute_event_operation(&state, &principal, operation, mode).await?;
    if streaming {
        let execution = precommit_stream_failure(execution)?;
        Ok(responses_streaming_response(execution))
    } else {
        responses_unary_response(execution).await
    }
}

async fn responses_unary_response(execution: RoutedEvents) -> Result<Response, InferenceError> {
    let mut completed = execution.collect().await.map_err(InferenceError::from)?;
    let response = encode_response_object(
        &completed.events,
        completed.route_slug.as_str(),
        &format!("resp_{}", completed.request_id.simple()),
        unix_seconds(),
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

fn responses_streaming_response(execution: RoutedEvents) -> Response {
    let encoder = Encoder::new(
        execution.route_slug.as_str(),
        format!("resp_{}", execution.request_id.simple()),
        unix_seconds(),
    );
    protocol_streaming_response(execution, encoder)
}

impl ProtocolStreamEncoder for Encoder {
    fn push(&mut self, event: Event) -> Result<Vec<Bytes>, InferenceError> {
        encode_protocol_sse_frames(Encoder::push(self, event))
    }

    fn encode_error(&self, error: &InferenceError) -> Bytes {
        responses_error_sse(error)
    }
}

fn responses_error_sse(error: &InferenceError) -> Bytes {
    encode_server_sse_frame(&olp_engine::protocols::sse::Frame {
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
    Extension(principal): Extension<HttpRequestAdmission>,
    payload: Result<Json<ResponseInputTokensRequest>, JsonRejection>,
) -> Result<Response, InferenceError> {
    let _ = authorize_principal(&state, &principal, GatewayCapability::Inference, None)?;
    let Json(mut request) = valid_json(payload)?;
    let admitted = admit_openai_response_input_tokens(&state, &mut request).await?;
    let operation = match decode_response_input_tokens(request) {
        Ok(operation) => operation,
        Err(error) => {
            admitted.release().await;
            return Err(InferenceError::invalid_request(error.to_string()));
        }
    };
    admitted.disarm();
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
