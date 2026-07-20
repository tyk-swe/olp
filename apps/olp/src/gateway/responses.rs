use axum::{
    Json,
    body::Bytes,
    extract::{State, rejection::JsonRejection},
    http::{HeaderMap, StatusCode},
    response::{IntoResponse, Response},
};
use olp_domain::{CanonicalEvent, CanonicalResult, Operation, OperationKind, TransportMode};
use olp_protocols::openai::{
    OpenAiResponsesStreamEncoder, ResponseCreateRequest, ResponseInputTokensRequest,
    decode_response_create, decode_response_input_tokens, encode_response_input_tokens_result,
    encode_response_object,
};
use serde_json::{Value, json};

use crate::{
    ApiState,
    event_completion::collect_provider_events,
    json_media::{admit_openai_response_input_tokens, admit_openai_responses, cleanup_admitted},
    openai_response::unix_seconds,
    streaming_response::{
        ProtocolStreamEncoder, encode_server_sse_frame, encode_sse_frame,
        protocol_streaming_response,
    },
};

use super::{
    error::{InferenceError, valid_json},
    execution::{
        RoutedEventExecution, authenticate_key, execute_event_operation, execute_unary_result,
        incompatible_result,
    },
    limits::release_limits,
    telemetry::{UsageCapture, emit_event_execution},
};

pub(super) async fn responses(
    State(state): State<ApiState>,
    headers: HeaderMap,
    payload: Result<Json<ResponseCreateRequest>, JsonRejection>,
) -> Result<Response, InferenceError> {
    let _ = authenticate_key(&state, &headers, OperationKind::Generation, None)?;
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
    let execution = execute_event_operation(&state, &headers, operation, mode).await?;
    if streaming {
        Ok(responses_streaming_response(state, execution))
    } else {
        responses_unary_response(&state, execution).await
    }
}

async fn responses_unary_response(
    state: &ApiState,
    mut execution: RoutedEventExecution,
) -> Result<Response, InferenceError> {
    let events = collect_provider_events(
        execution.first.clone(),
        &mut execution.events,
        execution.deadline,
    )
    .await;
    let (events, failure) = match events {
        Ok(events) => (events, None),
        Err(failure) => (Vec::new(), Some(failure)),
    };
    if let Some(failure) = failure {
        emit_event_execution(state, &execution, &UsageCapture::default(), Some(&failure));
        release_limits(state, execution.lease.as_ref()).await;
        return Err(failure);
    }
    let mut usage = UsageCapture::default();
    for event in &events {
        usage.observe(event);
    }
    let response = encode_response_object(
        &events,
        execution.route_slug.as_str(),
        &format!("resp_{}", execution.request_id.simple()),
    )
    .map_err(|error| InferenceError::bad_gateway("provider_protocol_error", error.to_string()));
    match response {
        Ok(response) => {
            emit_event_execution(state, &execution, &usage, None);
            release_limits(state, execution.lease.as_ref()).await;
            Ok((StatusCode::OK, Json(response)).into_response())
        }
        Err(failure) => {
            emit_event_execution(state, &execution, &usage, Some(&failure));
            release_limits(state, execution.lease.as_ref()).await;
            Err(failure)
        }
    }
}

fn responses_streaming_response(state: ApiState, execution: RoutedEventExecution) -> Response {
    let encoder = OpenAiResponsesHttpStreamEncoder(OpenAiResponsesStreamEncoder::new(
        execution.route_slug.as_str(),
        format!("resp_{}", execution.request_id.simple()),
        unix_seconds(),
    ));
    protocol_streaming_response(state, execution, encoder)
}

struct OpenAiResponsesHttpStreamEncoder(OpenAiResponsesStreamEncoder);

impl ProtocolStreamEncoder for OpenAiResponsesHttpStreamEncoder {
    fn push(&mut self, event: CanonicalEvent) -> Result<Vec<Bytes>, String> {
        self.0
            .push(event)
            .map_err(|error| error.to_string())
            .and_then(|frames| {
                frames
                    .iter()
                    .map(encode_sse_frame)
                    .collect::<Result<Vec<_>, _>>()
                    .map_err(|error| error.to_string())
            })
    }

    fn encode_error(&self, error: &InferenceError) -> Bytes {
        responses_error_sse(error)
    }
}

fn responses_error_sse(error: &InferenceError) -> Bytes {
    encode_server_sse_frame(&olp_protocols::sse::SseFrame {
        event: Some("error".to_owned()),
        data: json!({
            "type": "error",
            "code": error.code,
            "message": error.message,
            "param": null
        })
        .to_string(),
        id: None,
        retry_ms: None,
    })
}

pub(super) async fn response_input_tokens(
    State(state): State<ApiState>,
    headers: HeaderMap,
    payload: Result<Json<ResponseInputTokensRequest>, JsonRejection>,
) -> Result<Response, InferenceError> {
    let _ = authenticate_key(&state, &headers, OperationKind::TokenCount, None)?;
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
    let mut executed = execute_unary_result(&state, &headers, operation).await?;
    let CanonicalResult::TokenCount(result) = executed.result.as_ref() else {
        return Err(incompatible_result("token count"));
    };
    let response = encode_response_input_tokens_result(result).map_err(|error| {
        InferenceError::bad_gateway("provider_protocol_error", error.to_string())
    })?;
    executed.mark_success();
    Ok((StatusCode::OK, Json(response)).into_response())
}
