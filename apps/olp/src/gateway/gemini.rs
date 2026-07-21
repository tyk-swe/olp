use axum::{
    Json, Router,
    extract::{Extension, Path, Query, State, rejection::JsonRejection},
    http::StatusCode,
    response::{IntoResponse, Response},
    routing::get,
};
use base64::{Engine as _, engine::general_purpose::URL_SAFE_NO_PAD};
use olp_domain::{ApiKey, CanonicalResult, OperationKind, RouteSlug, Surface, TransportMode};
use olp_protocols::gemini::{
    CountTokensRequest, GeminiGenerateContentClientStreamEncoder, GenerateContentRequest,
    decode_count_tokens_request, decode_generate_content_request, encode_count_tokens_result,
    encode_generate_content_response,
};
use serde::{Deserialize, Serialize};

use crate::{
    ApiState, InferencePrincipal, RuntimeBundle,
    event_completion::{CompletedEventExecution, collect_event_execution},
    json_media::{admit_gemini_count, admit_gemini_generate, cleanup_admitted},
    streaming_response::{
        ProtocolStreamEncoder, encode_server_sse_frame, encode_sse_frame,
        protocol_streaming_response,
    },
};

use super::{
    InferenceError, RoutedUnaryResult, authorize_model_access, execute_event_operation_for_surface,
    execute_routed_result_for_surface,
    native_models::{after_cursor_start, supported_operations, visible_route, visible_routes},
    protocol_error::{ProtocolError, gemini_error_body, valid_json},
    release_model_limits, reserve_model_limits,
};

pub(super) fn router() -> Router<ApiState> {
    Router::new()
        .route("/gemini/v1/models", get(models))
        .route("/gemini/v1/models/{*resource}", get(model).post(action))
        .route("/gemini/v1beta/models", get(models))
        .route("/gemini/v1beta/models/{*resource}", get(model).post(action))
}

async fn action(
    State(state): State<ApiState>,
    Extension(principal): Extension<InferencePrincipal>,
    Path(resource): Path<String>,
    Query(query): Query<ActionQuery>,
    payload: Result<Json<serde_json::Value>, JsonRejection>,
) -> Result<Response, ProtocolError> {
    let Json(value) = valid_json(payload, Surface::Gemini)?;
    if let Some(model) = resource.strip_suffix(":generateContent") {
        let mut request: GenerateContentRequest =
            serde_json::from_value(value).map_err(|error| {
                ProtocolError::invalid(Surface::Gemini, format!("Invalid JSON request: {error}"))
            })?;
        let admitted = admit_gemini_generate(&state, &mut request)
            .await
            .map_err(ProtocolError::gemini)?;
        let operation = match decode_generate_content_request(model, request, false) {
            Ok(operation) => operation,
            Err(error) => {
                cleanup_admitted(&state, admitted).await;
                return Err(ProtocolError::invalid(
                    Surface::Gemini,
                    format!("Invalid generateContent request: {error}"),
                ));
            }
        };
        let execution = execute_event_operation_for_surface(
            &state,
            &principal,
            operation,
            TransportMode::Unary,
        )
        .await
        .map_err(ProtocolError::gemini)?;
        let completed = collect_event_execution(&state, execution)
            .await
            .map_err(ProtocolError::gemini)?;
        return unary_response(completed);
    }
    if let Some(model) = resource.strip_suffix(":streamGenerateContent") {
        if query.alt.as_deref().is_some_and(|alt| alt != "sse") {
            return Err(ProtocolError::invalid(
                Surface::Gemini,
                "streamGenerateContent supports only alt=sse.",
            ));
        }
        let mut request: GenerateContentRequest =
            serde_json::from_value(value).map_err(|error| {
                ProtocolError::invalid(Surface::Gemini, format!("Invalid JSON request: {error}"))
            })?;
        let admitted = admit_gemini_generate(&state, &mut request)
            .await
            .map_err(ProtocolError::gemini)?;
        let operation = match decode_generate_content_request(model, request, true) {
            Ok(operation) => operation,
            Err(error) => {
                cleanup_admitted(&state, admitted).await;
                return Err(ProtocolError::invalid(
                    Surface::Gemini,
                    format!("Invalid streamGenerateContent request: {error}"),
                ));
            }
        };
        let execution = execute_event_operation_for_surface(
            &state,
            &principal,
            operation,
            TransportMode::Streaming,
        )
        .await
        .map_err(ProtocolError::gemini)?;
        let encoder = GeminiHttpStreamEncoder(GeminiGenerateContentClientStreamEncoder::new(
            execution.route_slug.as_str(),
            execution.request_id.to_string(),
        ));
        return Ok(protocol_streaming_response(state, execution, encoder));
    }
    if let Some(model) = resource.strip_suffix(":countTokens") {
        let mut request: CountTokensRequest = serde_json::from_value(value).map_err(|error| {
            ProtocolError::invalid(Surface::Gemini, format!("Invalid JSON request: {error}"))
        })?;
        let admitted = admit_gemini_count(&state, &mut request)
            .await
            .map_err(ProtocolError::gemini)?;
        let operation = match decode_count_tokens_request(model, request) {
            Ok(operation) => operation,
            Err(error) => {
                cleanup_admitted(&state, admitted).await;
                return Err(ProtocolError::invalid(
                    Surface::Gemini,
                    format!("Invalid countTokens request: {error}"),
                ));
            }
        };
        let executed = execute_routed_result_for_surface(
            &state,
            &principal,
            operation,
            TransportMode::Unary,
            None,
        )
        .await
        .map_err(ProtocolError::gemini)?;
        return count_result(executed);
    }
    Err(ProtocolError::not_found(
        Surface::Gemini,
        "The requested Gemini method does not exist.",
    ))
}

fn unary_response(mut completed: CompletedEventExecution) -> Result<Response, ProtocolError> {
    let response = encode_generate_content_response(
        &completed.events,
        completed.route_slug.as_str(),
        &completed.request_id.to_string(),
    )
    .map_err(|error| {
        ProtocolError::upstream(
            Surface::Gemini,
            format!("The provider response cannot be represented as generateContent: {error}"),
        )
    })?;
    completed.mark_success();
    Ok((StatusCode::OK, Json(response)).into_response())
}

fn count_result(mut executed: RoutedUnaryResult) -> Result<Response, ProtocolError> {
    let CanonicalResult::TokenCount(result) = executed.result.as_ref() else {
        return Err(ProtocolError::upstream(
            Surface::Gemini,
            "The provider returned an incompatible token-count result.",
        ));
    };
    let response = encode_count_tokens_result(result).map_err(|error| {
        ProtocolError::upstream(
            Surface::Gemini,
            format!("The token-count result is not representable: {error}"),
        )
    })?;
    executed.mark_success();
    Ok((StatusCode::OK, Json(response)).into_response())
}

#[derive(Default, Deserialize)]
#[serde(rename_all = "camelCase")]
struct ActionQuery {
    alt: Option<String>,
}

#[derive(Default, Deserialize)]
#[serde(rename_all = "camelCase")]
struct ModelsQuery {
    page_size: Option<usize>,
    page_token: Option<String>,
}

#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
struct ModelList {
    models: Vec<Model>,
    #[serde(skip_serializing_if = "Option::is_none")]
    next_page_token: Option<String>,
}

#[derive(Clone, Serialize)]
#[serde(rename_all = "camelCase")]
struct Model {
    name: String,
    base_model_id: String,
    version: String,
    display_name: String,
    description: String,
    supported_generation_methods: Vec<&'static str>,
}

async fn models(
    State(state): State<ApiState>,
    Extension(principal): Extension<InferencePrincipal>,
    Query(query): Query<ModelsQuery>,
) -> Result<Response, ProtocolError> {
    let (runtime, key) = authorize_model_access(&principal, OperationKind::ModelList)
        .map_err(ProtocolError::gemini)?;
    let lease = reserve_model_limits(&state, &principal)
        .await
        .map_err(ProtocolError::gemini)?;
    let result = models_response(runtime, key, query);
    release_model_limits(&state, lease.as_ref()).await;
    result
}

fn models_response(
    runtime: &RuntimeBundle,
    key: &ApiKey,
    query: ModelsQuery,
) -> Result<Response, ProtocolError> {
    let limit = query.page_size.unwrap_or(50);
    if !(1..=1_000).contains(&limit) {
        return Err(ProtocolError::invalid(
            Surface::Gemini,
            "pageSize must be between 1 and 1000.",
        ));
    }
    let all = visible_routes(runtime, key, Surface::Gemini);
    let after = query
        .page_token
        .as_deref()
        .map(decode_page_token)
        .transpose()?;
    let start = after_cursor_start(
        &all,
        after.as_deref(),
        Surface::Gemini,
        "The pageToken is stale or unknown.",
    )?;
    let remaining = &all[start.min(all.len())..];
    let has_more = remaining.len() > limit;
    let models = remaining
        .iter()
        .take(limit)
        .map(|slug| model_object(runtime, slug))
        .collect::<Vec<_>>();
    let next_page_token = has_more
        .then(|| {
            models
                .last()
                .map(|model| encode_page_token(&model.base_model_id))
        })
        .flatten();
    Ok((
        StatusCode::OK,
        Json(ModelList {
            models,
            next_page_token,
        }),
    )
        .into_response())
}

async fn model(
    State(state): State<ApiState>,
    Extension(principal): Extension<InferencePrincipal>,
    Path(resource): Path<String>,
) -> Result<Response, ProtocolError> {
    let (runtime, key) = authorize_model_access(&principal, OperationKind::ModelGet)
        .map_err(ProtocolError::gemini)?;
    let lease = reserve_model_limits(&state, &principal)
        .await
        .map_err(ProtocolError::gemini)?;
    let result = if resource.contains(':') {
        Err(ProtocolError::not_found(
            Surface::Gemini,
            "The requested Gemini model does not exist.",
        ))
    } else {
        visible_route(runtime, key, &resource, Surface::Gemini)
            .map(|slug| (StatusCode::OK, Json(model_object(runtime, &slug))).into_response())
    };
    release_model_limits(&state, lease.as_ref()).await;
    result
}

fn model_object(runtime: &RuntimeBundle, slug: &RouteSlug) -> Model {
    Model {
        name: format!("models/{slug}"),
        base_model_id: slug.to_string(),
        version: runtime.generation.ordinal.to_string(),
        display_name: slug.to_string(),
        description: "OpenLLMProxy route".to_owned(),
        supported_generation_methods: supported_operations(runtime, slug, Surface::Gemini)
            .into_iter()
            .filter_map(|operation| match operation {
                OperationKind::Generation => Some("generateContent"),
                OperationKind::TokenCount => Some("countTokens"),
                _ => None,
            })
            .collect(),
    }
}

fn encode_page_token(slug: &str) -> String {
    URL_SAFE_NO_PAD.encode(format!("olp-v1:{slug}"))
}

fn decode_page_token(token: &str) -> Result<String, ProtocolError> {
    let bytes = URL_SAFE_NO_PAD
        .decode(token)
        .map_err(|_| ProtocolError::invalid(Surface::Gemini, "The pageToken is invalid."))?;
    let decoded = String::from_utf8(bytes)
        .map_err(|_| ProtocolError::invalid(Surface::Gemini, "The pageToken is invalid."))?;
    decoded
        .strip_prefix("olp-v1:")
        .filter(|slug| !slug.is_empty())
        .map(str::to_owned)
        .ok_or_else(|| ProtocolError::invalid(Surface::Gemini, "The pageToken is invalid."))
}

struct GeminiHttpStreamEncoder(GeminiGenerateContentClientStreamEncoder);

impl ProtocolStreamEncoder for GeminiHttpStreamEncoder {
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
            event: None,
            data: gemini_error_body(error).to_string(),
            id: None,
            retry_ms: None,
        })
    }
}
