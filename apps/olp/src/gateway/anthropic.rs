use axum::{
    Json,
    extract::{Extension, Path, Query, State, rejection::JsonRejection},
    http::StatusCode,
    response::{IntoResponse, Response},
};
use olp_engine::domain::{
    auth::ApiKey,
    canonical::{
        identity::{Surface, TransportMode},
        results::CanonicalResult,
    },
    ids::RouteSlug,
};
use olp_engine::protocols::anthropic::{
    client::encode_messages_response,
    client_stream::Encoder,
    count::{decode_count_tokens_request, encode_count_tokens_result},
    dto::{CountTokensRequest, MessagesRequest},
    translate::decode::request as decode_request,
};
use serde::{Deserialize, Serialize};

use olp_engine::inference::{execution::CompletedEvents, principal::Principal, runtime::Bundle};

use crate::{
    bootstrap::mode_dependencies::GatewayState,
    public_http::json_media::{admit_anthropic_messages, cleanup_admitted},
    public_http::streaming_response::{
        ProtocolStreamEncoder, encode_protocol_sse_frames, encode_server_sse_frame,
        protocol_streaming_response,
    },
};

use super::{
    authorize_model_access,
    error::InferenceError,
    execution::{execute_event_operation, execute_routed_result},
    native_models::{after_cursor_start, before_cursor_end, visible_route, visible_routes},
    protocol_error::{ProtocolError, anthropic_error_body, valid_json},
    release_model_limits, reserve_model_limits,
};

pub(super) async fn messages(
    State(state): State<GatewayState>,
    Extension(principal): Extension<Principal>,
    payload: Result<Json<MessagesRequest>, JsonRejection>,
) -> Result<Response, ProtocolError> {
    let Json(mut request) = valid_json(payload, Surface::Anthropic)?;
    let streaming = request.stream;
    let admitted = admit_anthropic_messages(&state, &mut request.messages)
        .await
        .map_err(ProtocolError::anthropic)?;
    let operation = match decode_request(request) {
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
    let execution = execute_event_operation(&state, &principal, operation, mode)
        .await
        .map_err(ProtocolError::anthropic)?;
    if streaming {
        let encoder = AnthropicHttpStreamEncoder(Encoder::new(
            execution.route_slug.as_str(),
            format!("msg_{}", execution.request_id.simple()),
        ));
        return Ok(protocol_streaming_response(execution, encoder));
    }
    let completed = execution
        .collect()
        .await
        .map_err(InferenceError::from)
        .map_err(ProtocolError::anthropic)?;
    unary_response(completed)
}

fn unary_response(mut completed: CompletedEvents) -> Result<Response, ProtocolError> {
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

pub(super) async fn count_tokens(
    State(state): State<GatewayState>,
    Extension(principal): Extension<Principal>,
    payload: Result<Json<CountTokensRequest>, JsonRejection>,
) -> Result<Response, ProtocolError> {
    let Json(mut request) = valid_json(payload, Surface::Anthropic)?;
    let admitted = admit_anthropic_messages(&state, &mut request.messages)
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
    let mut executed =
        execute_routed_result(&state, &principal, operation, TransportMode::Unary, None)
            .await
            .map_err(ProtocolError::anthropic)?;
    let CanonicalResult::TokenCount(result) = executed.result.as_ref() else {
        executed.mark_provider_protocol_failure();
        return Err(ProtocolError::upstream(
            Surface::Anthropic,
            "The provider returned an incompatible token-count result.",
        ));
    };
    let response = match encode_count_tokens_result(result) {
        Ok(response) => response,
        Err(error) => {
            executed.mark_provider_protocol_failure();
            return Err(ProtocolError::upstream(
                Surface::Anthropic,
                format!("The token-count result is not representable: {error}"),
            ));
        }
    };
    executed.mark_success();
    Ok((StatusCode::OK, Json(response)).into_response())
}

#[derive(Default, Deserialize)]
pub(super) struct ModelsQuery {
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

pub(super) async fn models(
    State(state): State<GatewayState>,
    Extension(principal): Extension<Principal>,
    Query(query): Query<ModelsQuery>,
) -> Result<Response, ProtocolError> {
    let (runtime, key) =
        authorize_model_access(&state, &principal).map_err(ProtocolError::anthropic)?;
    let lease = reserve_model_limits(&state, &principal)
        .await
        .map_err(ProtocolError::anthropic)?;
    let result = models_response(runtime, key, query);
    release_model_limits(&state, lease).await;
    result
}

fn models_response(
    runtime: &Bundle,
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
    let (selected, has_more) = model_page(&all, &query, limit)?;
    let models = selected
        .iter()
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

fn model_page<'a>(
    routes: &'a [RouteSlug],
    query: &ModelsQuery,
    limit: usize,
) -> Result<(&'a [RouteSlug], bool), ProtocolError> {
    if query.before_id.is_some() {
        let end = before_cursor_end(
            routes,
            query.before_id.as_deref(),
            Surface::Anthropic,
            "The before_id cursor is stale or unknown.",
        )?;
        let start = end.saturating_sub(limit);
        return Ok((&routes[start..end], start != 0));
    }

    let start = after_cursor_start(
        routes,
        query.after_id.as_deref(),
        Surface::Anthropic,
        "The after_id cursor is stale or unknown.",
    )?;
    let end = start.saturating_add(limit).min(routes.len());
    Ok((&routes[start..end], end != routes.len()))
}

pub(super) async fn model(
    State(state): State<GatewayState>,
    Extension(principal): Extension<Principal>,
    Path(id): Path<String>,
) -> Result<Response, ProtocolError> {
    let (runtime, key) =
        authorize_model_access(&state, &principal).map_err(ProtocolError::anthropic)?;
    let lease = reserve_model_limits(&state, &principal)
        .await
        .map_err(ProtocolError::anthropic)?;
    let result = visible_route(runtime, key, &id, Surface::Anthropic)
        .map(|slug| (StatusCode::OK, Json(model_object(runtime, &slug))).into_response());
    release_model_limits(&state, lease).await;
    result
}

fn model_object(runtime: &Bundle, slug: &RouteSlug) -> Model {
    Model {
        id: slug.to_string(),
        created_at: runtime.generation.activated_at.to_rfc3339(),
        display_name: slug.to_string(),
        kind: "model",
    }
}

struct AnthropicHttpStreamEncoder(Encoder);

impl ProtocolStreamEncoder for AnthropicHttpStreamEncoder {
    fn push(
        &mut self,
        event: olp_engine::domain::canonical::events::Event,
    ) -> Result<Vec<bytes::Bytes>, InferenceError> {
        encode_protocol_sse_frames(self.0.push(event))
    }

    fn encode_error(&self, error: &InferenceError) -> Vec<bytes::Bytes> {
        vec![encode_server_sse_frame(
            &olp_engine::protocols::sse::Frame {
                event: Some("error".to_owned()),
                data: anthropic_error_body(error.status(), error.message()).to_string(),
                id: None,
                retry_ms: None,
            },
        )]
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn routes() -> Vec<RouteSlug> {
        ["a", "b", "c", "d", "e"]
            .into_iter()
            .map(|value| RouteSlug::parse(value).unwrap())
            .collect()
    }

    #[test]
    fn before_id_returns_the_adjacent_preceding_page() {
        let routes = routes();
        let query = ModelsQuery {
            before_id: Some("e".to_owned()),
            after_id: None,
            limit: Some(2),
        };

        let (page, has_more) = model_page(&routes, &query, 2).unwrap();
        assert_eq!(
            page.iter().map(RouteSlug::as_str).collect::<Vec<_>>(),
            vec!["c", "d"]
        );
        assert!(has_more);
    }

    #[test]
    fn before_id_reports_no_more_items_at_the_start() {
        let routes = routes();
        let query = ModelsQuery {
            before_id: Some("c".to_owned()),
            after_id: None,
            limit: Some(2),
        };

        let (page, has_more) = model_page(&routes, &query, 2).unwrap();
        assert_eq!(
            page.iter().map(RouteSlug::as_str).collect::<Vec<_>>(),
            vec!["a", "b"]
        );
        assert!(!has_more);
    }
}
