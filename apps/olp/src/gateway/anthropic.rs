use axum::{
    Json, Router,
    extract::{Extension, Path, Query, State, rejection::JsonRejection},
    http::StatusCode,
    response::{IntoResponse, Response},
    routing::{get, post},
};
use olp_domain::{ApiKey, CanonicalResult, OperationKind, RouteSlug, Surface, TransportMode};
use olp_protocols::anthropic::{
    AnthropicMessagesClientStreamEncoder, CountTokensRequest, MessagesRequest,
    decode_count_tokens_request, decode_messages_request, encode_count_tokens_result,
    encode_messages_response,
};
use serde::{Deserialize, Serialize};
use serde_json::json;

use crate::{
    ApiState, InferencePrincipal, RuntimeBundle,
    event_completion::{CompletedEventExecution, collect_event_execution},
    json_media::{admit_anthropic_count, admit_anthropic_messages, cleanup_admitted},
    streaming_response::{
        ProtocolStreamEncoder, encode_server_sse_frame, encode_sse_frame,
        protocol_streaming_response,
    },
};

use super::{
    InferenceError, authorize_model_access, execute_event_operation_for_surface,
    execute_routed_result_for_surface,
    native_models::{after_cursor_start, before_cursor_end, visible_route, visible_routes},
    protocol_error::{ProtocolError, anthropic_error_kind, valid_json},
    release_model_limits, reserve_model_limits,
};

pub(super) fn router() -> Router<ApiState> {
    Router::new()
        .route("/anthropic/v1/messages", post(messages))
        .route("/anthropic/v1/messages/count_tokens", post(count_tokens))
        .route("/anthropic/v1/models", get(models))
        .route("/anthropic/v1/models/{id}", get(model))
}

async fn messages(
    State(state): State<ApiState>,
    Extension(principal): Extension<InferencePrincipal>,
    payload: Result<Json<MessagesRequest>, JsonRejection>,
) -> Result<Response, ProtocolError> {
    let Json(mut request) = valid_json(payload, Surface::Anthropic)?;
    let streaming = request.stream;
    let admitted = admit_anthropic_messages(&state, &mut request)
        .await
        .map_err(ProtocolError::anthropic)?;
    let operation = match decode_messages_request(request) {
        Ok(operation) => operation,
        Err(error) => {
            cleanup_admitted(&state, admitted).await;
            return Err(ProtocolError::invalid(
                Surface::Anthropic,
                format!("Invalid Messages request: {error}"),
            ));
        }
    };
    let mode = if streaming {
        TransportMode::Streaming
    } else {
        TransportMode::Unary
    };
    let execution = execute_event_operation_for_surface(&state, &principal, operation, mode)
        .await
        .map_err(ProtocolError::anthropic)?;
    if streaming {
        let encoder = AnthropicHttpStreamEncoder(AnthropicMessagesClientStreamEncoder::new(
            execution.route_slug.as_str(),
            format!("msg_{}", execution.request_id.simple()),
        ));
        return Ok(protocol_streaming_response(state, execution, encoder));
    }
    let completed = collect_event_execution(&state, execution)
        .await
        .map_err(ProtocolError::anthropic)?;
    unary_response(completed)
}

fn unary_response(mut completed: CompletedEventExecution) -> Result<Response, ProtocolError> {
    let response = encode_messages_response(
        &completed.events,
        completed.route_slug.as_str(),
        &format!("msg_{}", completed.request_id.simple()),
    )
    .map_err(|error| {
        ProtocolError::upstream(
            Surface::Anthropic,
            format!("The provider response cannot be represented as Messages: {error}"),
        )
    })?;
    completed.mark_success();
    Ok((StatusCode::OK, Json(response)).into_response())
}

async fn count_tokens(
    State(state): State<ApiState>,
    Extension(principal): Extension<InferencePrincipal>,
    payload: Result<Json<CountTokensRequest>, JsonRejection>,
) -> Result<Response, ProtocolError> {
    let Json(mut request) = valid_json(payload, Surface::Anthropic)?;
    let admitted = admit_anthropic_count(&state, &mut request)
        .await
        .map_err(ProtocolError::anthropic)?;
    let operation = match decode_count_tokens_request(request) {
        Ok(operation) => operation,
        Err(error) => {
            cleanup_admitted(&state, admitted).await;
            return Err(ProtocolError::invalid(
                Surface::Anthropic,
                format!("Invalid count_tokens request: {error}"),
            ));
        }
    };
    let mut executed = execute_routed_result_for_surface(
        &state,
        &principal,
        operation,
        TransportMode::Unary,
        None,
    )
    .await
    .map_err(ProtocolError::anthropic)?;
    let CanonicalResult::TokenCount(result) = executed.result.as_ref() else {
        return Err(ProtocolError::upstream(
            Surface::Anthropic,
            "The provider returned an incompatible token-count result.",
        ));
    };
    let response = encode_count_tokens_result(result).map_err(|error| {
        ProtocolError::upstream(
            Surface::Anthropic,
            format!("The token-count result is not representable: {error}"),
        )
    })?;
    executed.mark_success();
    Ok((StatusCode::OK, Json(response)).into_response())
}

#[derive(Default, Deserialize)]
struct ModelsQuery {
    before_id: Option<String>,
    after_id: Option<String>,
    limit: Option<usize>,
}

#[derive(Serialize)]
struct ModelList {
    data: Vec<Model>,
    has_more: bool,
    first_id: Option<String>,
    last_id: Option<String>,
}

#[derive(Clone, Serialize)]
struct Model {
    id: String,
    created_at: String,
    display_name: String,
    #[serde(rename = "type")]
    kind: &'static str,
}

async fn models(
    State(state): State<ApiState>,
    Extension(principal): Extension<InferencePrincipal>,
    Query(query): Query<ModelsQuery>,
) -> Result<Response, ProtocolError> {
    let (runtime, key) = authorize_model_access(&principal, OperationKind::ModelList)
        .map_err(ProtocolError::anthropic)?;
    let lease = reserve_model_limits(&state, &principal)
        .await
        .map_err(ProtocolError::anthropic)?;
    let result = models_response(runtime, key, query);
    release_model_limits(&state, lease.as_ref()).await;
    result
}

fn models_response(
    runtime: &RuntimeBundle,
    key: &ApiKey,
    query: ModelsQuery,
) -> Result<Response, ProtocolError> {
    let limit = query.limit.unwrap_or(20);
    if !(1..=1_000).contains(&limit) || (query.before_id.is_some() && query.after_id.is_some()) {
        return Err(ProtocolError::invalid(
            Surface::Anthropic,
            "Model pagination parameters are invalid.",
        ));
    }
    let all = visible_routes(runtime, key, Surface::Anthropic);
    let start = after_cursor_start(
        &all,
        query.after_id.as_deref(),
        Surface::Anthropic,
        "The after_id cursor is stale or unknown.",
    )?;
    let end = before_cursor_end(
        &all,
        query.before_id.as_deref(),
        Surface::Anthropic,
        "The before_id cursor is stale or unknown.",
    )?;
    let end = end.max(start).min(all.len());
    let selected = &all[start.min(end)..end];
    let has_more = selected.len() > limit;
    let models = selected
        .iter()
        .take(limit)
        .map(|slug| model_object(runtime, slug))
        .collect::<Vec<_>>();
    let response = ModelList {
        first_id: models.first().map(|model| model.id.clone()),
        last_id: models.last().map(|model| model.id.clone()),
        data: models,
        has_more,
    };
    Ok((StatusCode::OK, Json(response)).into_response())
}

async fn model(
    State(state): State<ApiState>,
    Extension(principal): Extension<InferencePrincipal>,
    Path(id): Path<String>,
) -> Result<Response, ProtocolError> {
    let (runtime, key) = authorize_model_access(&principal, OperationKind::ModelGet)
        .map_err(ProtocolError::anthropic)?;
    let lease = reserve_model_limits(&state, &principal)
        .await
        .map_err(ProtocolError::anthropic)?;
    let result = visible_route(runtime, key, &id, Surface::Anthropic)
        .map(|slug| (StatusCode::OK, Json(model_object(runtime, &slug))).into_response());
    release_model_limits(&state, lease.as_ref()).await;
    result
}

fn model_object(runtime: &RuntimeBundle, slug: &RouteSlug) -> Model {
    Model {
        id: slug.to_string(),
        created_at: runtime.generation.activated_at.to_rfc3339(),
        display_name: slug.to_string(),
        kind: "model",
    }
}

struct AnthropicHttpStreamEncoder(AnthropicMessagesClientStreamEncoder);

impl ProtocolStreamEncoder for AnthropicHttpStreamEncoder {
    fn push(&mut self, event: olp_domain::CanonicalEvent) -> Result<Vec<bytes::Bytes>, String> {
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

    fn encode_error(&self, error: &InferenceError) -> bytes::Bytes {
        encode_server_sse_frame(&olp_protocols::sse::SseFrame {
            event: Some("error".to_owned()),
            data: json!({
                "type": "error",
                "error": {"type": anthropic_error_kind(error), "message": error.message()}
            })
            .to_string(),
            id: None,
            retry_ms: None,
        })
    }
}
