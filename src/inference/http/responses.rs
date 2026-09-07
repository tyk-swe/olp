use crate::access::policy::GatewayCapability;
use crate::http::request_admission::HttpRequestAdmission;
use crate::inference::execution::RoutedEvents;
use crate::protocols::canonical::events::Event;
use crate::protocols::canonical::identity::TransportMode;
use crate::protocols::canonical::requests::OPENAI_ENDPOINT_EXTENSION;
use crate::protocols::canonical::requests::Operation;
use crate::protocols::canonical::results::CanonicalResult;
use crate::protocols::openai::client::Encoder;
use crate::protocols::openai::client::encode_response_object;
use crate::protocols::openai::responses::request::Create;
use crate::protocols::openai::responses::request::decode_response_create;
use crate::protocols::openai::responses::token_count::ResponseInputTokensRequest;
use crate::protocols::openai::responses::token_count::decode_response_input_tokens;
use crate::protocols::openai::responses::token_count::encode_response_input_tokens_result;
use axum::Json;
use axum::body::Bytes;
use axum::extract::Extension;
use axum::extract::State;
use axum::extract::rejection::JsonRejection;
use axum::http::StatusCode;
use axum::response::IntoResponse;
use axum::response::Response;
use serde_json::Value;
use serde_json::json;

use crate::http::json_media::admit_openai_response_input_tokens;
use crate::http::json_media::admit_openai_responses;
use crate::http::streaming_response::ProtocolStreamEncoder;
use crate::http::streaming_response::encode_protocol_sse_frames;
use crate::http::streaming_response::encode_server_sse_frame;
use crate::http::streaming_response::precommit_stream_failure;
use crate::http::streaming_response::protocol_streaming_response;
use crate::inference::http::state::GatewayState;

use crate::inference::http::error::InferenceError;
use crate::inference::http::error::valid_json;
use crate::inference::http::execution::authorize_principal;
use crate::inference::http::execution::execute_event_operation;
use crate::inference::http::execution::execute_unary_result;
use crate::inference::http::execution::incompatible_result;
use crate::inference::http::execution::mark_unary_outcome;
use crate::inference::http::openai_http::unix_seconds;

pub(crate) async fn responses(
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
    encode_server_sse_frame(&crate::protocols::sse::Frame {
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

pub(crate) async fn response_input_tokens(
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
