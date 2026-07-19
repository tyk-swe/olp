use axum::{
    Json, Router,
    extract::{Path, Query, State, rejection::JsonRejection},
    http::{HeaderMap, HeaderValue, StatusCode, header},
    response::{IntoResponse, Response},
    routing::{get, post},
};
use base64::{Engine as _, engine::general_purpose::URL_SAFE_NO_PAD};
use olp_domain::{
    ApiKey, CanonicalResult, OperationKind, RouteSlug, Surface, TransportMode, select_attempts,
};
use olp_protocols::{
    anthropic::{
        AnthropicMessagesClientStreamEncoder, CountTokensRequest as AnthropicCountTokensRequest,
        MessagesRequest, decode_count_tokens_request as decode_anthropic_count_tokens,
        decode_messages_request, encode_count_tokens_result as encode_anthropic_count_tokens,
        encode_messages_response,
    },
    gemini::{
        CountTokensRequest as GeminiCountTokensRequest, GeminiGenerateContentClientStreamEncoder,
        GenerateContentRequest, decode_count_tokens_request as decode_gemini_count_tokens,
        decode_generate_content_request, encode_count_tokens_result as encode_gemini_count_tokens,
        encode_generate_content_response,
    },
};
use serde::{Deserialize, Serialize};
use serde_json::json;

use crate::{
    ApiState, RuntimeBundle,
    event_completion::{CompletedEventExecution, collect_event_execution},
    gateway::{
        InferenceError, RoutedUnaryResult, authenticate_model_access,
        execute_event_operation_for_surface, execute_routed_result_for_surface,
        release_model_limits, reserve_model_limits,
    },
    json_media::{
        admit_anthropic_count, admit_anthropic_messages, admit_gemini_count, admit_gemini_generate,
        cleanup_admitted,
    },
    streaming_response::{
        ProtocolStreamEncoder, encode_server_sse_frame, encode_sse_frame,
        protocol_streaming_response,
    },
};

pub(crate) fn router() -> Router<ApiState> {
    Router::new()
        .route("/anthropic/v1/messages", post(anthropic_messages))
        .route(
            "/anthropic/v1/messages/count_tokens",
            post(anthropic_count_tokens),
        )
        .route("/anthropic/v1/models", get(anthropic_models))
        .route("/anthropic/v1/models/{id}", get(anthropic_model))
        .route("/gemini/v1/models", get(gemini_models))
        .route(
            "/gemini/v1/models/{*resource}",
            get(gemini_model).post(gemini_action),
        )
        .route("/gemini/v1beta/models", get(gemini_models))
        .route(
            "/gemini/v1beta/models/{*resource}",
            get(gemini_model).post(gemini_action),
        )
}

async fn anthropic_messages(
    State(state): State<ApiState>,
    headers: HeaderMap,
    payload: Result<Json<MessagesRequest>, JsonRejection>,
) -> Result<Response, VendorError> {
    let token = anthropic_key(&headers).map_err(VendorError::anthropic)?;
    let Json(mut request) = valid_json(payload, Surface::Anthropic)?;
    let streaming = request.stream;
    let admitted = admit_anthropic_messages(&state, &mut request)
        .await
        .map_err(VendorError::anthropic)?;
    let operation = match decode_messages_request(request) {
        Ok(operation) => operation,
        Err(error) => {
            cleanup_admitted(&state, admitted).await;
            return Err(VendorError::invalid(
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
    let execution =
        execute_event_operation_for_surface(&state, token, operation, Surface::Anthropic, mode)
            .await
            .map_err(VendorError::anthropic)?;
    if streaming {
        let encoder = AnthropicHttpStreamEncoder(AnthropicMessagesClientStreamEncoder::new(
            execution.route_slug.as_str(),
            format!("msg_{}", execution.request_id.simple()),
        ));
        return Ok(protocol_streaming_response(state, execution, encoder));
    }
    let completed = collect_event_execution(&state, execution)
        .await
        .map_err(VendorError::anthropic)?;
    anthropic_unary(completed)
}

fn anthropic_unary(mut completed: CompletedEventExecution) -> Result<Response, VendorError> {
    let response = encode_messages_response(
        &completed.events,
        completed.route_slug.as_str(),
        &format!("msg_{}", completed.request_id.simple()),
    )
    .map_err(|error| {
        VendorError::upstream(
            Surface::Anthropic,
            format!("The provider response cannot be represented as Messages: {error}"),
        )
    })?;
    completed.mark_success();
    Ok((StatusCode::OK, Json(response)).into_response())
}

async fn anthropic_count_tokens(
    State(state): State<ApiState>,
    headers: HeaderMap,
    payload: Result<Json<AnthropicCountTokensRequest>, JsonRejection>,
) -> Result<Response, VendorError> {
    let token = anthropic_key(&headers).map_err(VendorError::anthropic)?;
    let Json(mut request) = valid_json(payload, Surface::Anthropic)?;
    let admitted = admit_anthropic_count(&state, &mut request)
        .await
        .map_err(VendorError::anthropic)?;
    let operation = match decode_anthropic_count_tokens(request) {
        Ok(operation) => operation,
        Err(error) => {
            cleanup_admitted(&state, admitted).await;
            return Err(VendorError::invalid(
                Surface::Anthropic,
                format!("Invalid count_tokens request: {error}"),
            ));
        }
    };
    let mut executed = execute_routed_result_for_surface(
        &state,
        token,
        operation,
        Surface::Anthropic,
        TransportMode::Unary,
        None,
    )
    .await
    .map_err(VendorError::anthropic)?;
    let CanonicalResult::TokenCount(result) = executed.result.as_ref() else {
        return Err(VendorError::upstream(
            Surface::Anthropic,
            "The provider returned an incompatible token-count result.",
        ));
    };
    let response = encode_anthropic_count_tokens(result).map_err(|error| {
        VendorError::upstream(
            Surface::Anthropic,
            format!("The token-count result is not representable: {error}"),
        )
    })?;
    executed.mark_success();
    Ok((StatusCode::OK, Json(response)).into_response())
}

async fn gemini_action(
    State(state): State<ApiState>,
    Path(resource): Path<String>,
    Query(query): Query<GeminiActionQuery>,
    headers: HeaderMap,
    payload: Result<Json<serde_json::Value>, JsonRejection>,
) -> Result<Response, VendorError> {
    let Json(value) = valid_json(payload, Surface::Gemini)?;
    let token = gemini_key(&headers).map_err(VendorError::gemini)?;
    if let Some(model) = resource.strip_suffix(":generateContent") {
        let mut request: GenerateContentRequest =
            serde_json::from_value(value).map_err(|error| {
                VendorError::invalid(Surface::Gemini, format!("Invalid JSON request: {error}"))
            })?;
        let admitted = admit_gemini_generate(&state, &mut request)
            .await
            .map_err(VendorError::gemini)?;
        let operation = match decode_generate_content_request(model, request, false) {
            Ok(operation) => operation,
            Err(error) => {
                cleanup_admitted(&state, admitted).await;
                return Err(VendorError::invalid(
                    Surface::Gemini,
                    format!("Invalid generateContent request: {error}"),
                ));
            }
        };
        let execution = execute_event_operation_for_surface(
            &state,
            token,
            operation,
            Surface::Gemini,
            TransportMode::Unary,
        )
        .await
        .map_err(VendorError::gemini)?;
        let completed = collect_event_execution(&state, execution)
            .await
            .map_err(VendorError::gemini)?;
        return gemini_unary(completed);
    }
    if let Some(model) = resource.strip_suffix(":streamGenerateContent") {
        if query.alt.as_deref().is_some_and(|alt| alt != "sse") {
            return Err(VendorError::invalid(
                Surface::Gemini,
                "streamGenerateContent supports only alt=sse.",
            ));
        }
        let mut request: GenerateContentRequest =
            serde_json::from_value(value).map_err(|error| {
                VendorError::invalid(Surface::Gemini, format!("Invalid JSON request: {error}"))
            })?;
        let admitted = admit_gemini_generate(&state, &mut request)
            .await
            .map_err(VendorError::gemini)?;
        let operation = match decode_generate_content_request(model, request, true) {
            Ok(operation) => operation,
            Err(error) => {
                cleanup_admitted(&state, admitted).await;
                return Err(VendorError::invalid(
                    Surface::Gemini,
                    format!("Invalid streamGenerateContent request: {error}"),
                ));
            }
        };
        let execution = execute_event_operation_for_surface(
            &state,
            token,
            operation,
            Surface::Gemini,
            TransportMode::Streaming,
        )
        .await
        .map_err(VendorError::gemini)?;
        let encoder = GeminiHttpStreamEncoder(GeminiGenerateContentClientStreamEncoder::new(
            execution.route_slug.as_str(),
            execution.request_id.to_string(),
        ));
        return Ok(protocol_streaming_response(state, execution, encoder));
    }
    if let Some(model) = resource.strip_suffix(":countTokens") {
        let mut request: GeminiCountTokensRequest =
            serde_json::from_value(value).map_err(|error| {
                VendorError::invalid(Surface::Gemini, format!("Invalid JSON request: {error}"))
            })?;
        let admitted = admit_gemini_count(&state, &mut request)
            .await
            .map_err(VendorError::gemini)?;
        let operation = match decode_gemini_count_tokens(model, request) {
            Ok(operation) => operation,
            Err(error) => {
                cleanup_admitted(&state, admitted).await;
                return Err(VendorError::invalid(
                    Surface::Gemini,
                    format!("Invalid countTokens request: {error}"),
                ));
            }
        };
        let executed = execute_routed_result_for_surface(
            &state,
            token,
            operation,
            Surface::Gemini,
            TransportMode::Unary,
            None,
        )
        .await
        .map_err(VendorError::gemini)?;
        return gemini_count_result(executed);
    }
    Err(VendorError::not_found(
        Surface::Gemini,
        "The requested Gemini method does not exist.",
    ))
}

fn gemini_unary(mut completed: CompletedEventExecution) -> Result<Response, VendorError> {
    let response = encode_generate_content_response(
        &completed.events,
        completed.route_slug.as_str(),
        &completed.request_id.to_string(),
    )
    .map_err(|error| {
        VendorError::upstream(
            Surface::Gemini,
            format!("The provider response cannot be represented as generateContent: {error}"),
        )
    })?;
    completed.mark_success();
    Ok((StatusCode::OK, Json(response)).into_response())
}

fn gemini_count_result(mut executed: RoutedUnaryResult) -> Result<Response, VendorError> {
    let CanonicalResult::TokenCount(result) = executed.result.as_ref() else {
        return Err(VendorError::upstream(
            Surface::Gemini,
            "The provider returned an incompatible token-count result.",
        ));
    };
    let response = encode_gemini_count_tokens(result).map_err(|error| {
        VendorError::upstream(
            Surface::Gemini,
            format!("The token-count result is not representable: {error}"),
        )
    })?;
    executed.mark_success();
    Ok((StatusCode::OK, Json(response)).into_response())
}

#[derive(Default, Deserialize)]
#[serde(rename_all = "camelCase")]
struct GeminiActionQuery {
    alt: Option<String>,
}

#[derive(Default, Deserialize)]
struct AnthropicModelsQuery {
    before_id: Option<String>,
    after_id: Option<String>,
    limit: Option<usize>,
}

#[derive(Serialize)]
struct AnthropicModelList {
    data: Vec<AnthropicModel>,
    has_more: bool,
    first_id: Option<String>,
    last_id: Option<String>,
}

#[derive(Clone, Serialize)]
struct AnthropicModel {
    id: String,
    created_at: String,
    display_name: String,
    #[serde(rename = "type")]
    kind: &'static str,
}

async fn anthropic_models(
    State(state): State<ApiState>,
    headers: HeaderMap,
    Query(query): Query<AnthropicModelsQuery>,
) -> Result<Response, VendorError> {
    let token = anthropic_key(&headers).map_err(VendorError::anthropic)?;
    let (runtime, key) = authenticate_model_access(&state, token, OperationKind::ModelList)
        .map_err(VendorError::anthropic)?;
    let lease = reserve_model_limits(&state, &key, token, Surface::Anthropic)
        .await
        .map_err(VendorError::anthropic)?;
    let result = anthropic_models_response(&runtime, &key, query);
    release_model_limits(&state, lease.as_ref()).await;
    result
}

fn anthropic_models_response(
    runtime: &RuntimeBundle,
    key: &ApiKey,
    query: AnthropicModelsQuery,
) -> Result<Response, VendorError> {
    let limit = query.limit.unwrap_or(20);
    if !(1..=1_000).contains(&limit) || (query.before_id.is_some() && query.after_id.is_some()) {
        return Err(VendorError::invalid(
            Surface::Anthropic,
            "Model pagination parameters are invalid.",
        ));
    }
    let all = visible_routes(runtime, key, Surface::Anthropic);
    let start = match query.after_id.as_deref() {
        Some(cursor) => all
            .iter()
            .position(|slug| slug.as_str() == cursor)
            .map(|index| index.saturating_add(1))
            .ok_or_else(|| {
                VendorError::invalid(
                    Surface::Anthropic,
                    "The after_id cursor is stale or unknown.",
                )
            })?,
        None => 0,
    };
    let end = match query.before_id.as_deref() {
        Some(cursor) => all
            .iter()
            .position(|slug| slug.as_str() == cursor)
            .ok_or_else(|| {
                VendorError::invalid(
                    Surface::Anthropic,
                    "The before_id cursor is stale or unknown.",
                )
            })?,
        None => all.len(),
    };
    let end = end.max(start).min(all.len());
    let selected = &all[start.min(end)..end];
    let has_more = selected.len() > limit;
    let models = selected
        .iter()
        .take(limit)
        .map(|slug| anthropic_model_object(runtime, slug))
        .collect::<Vec<_>>();
    let response = AnthropicModelList {
        first_id: models.first().map(|model| model.id.clone()),
        last_id: models.last().map(|model| model.id.clone()),
        data: models,
        has_more,
    };
    Ok((StatusCode::OK, Json(response)).into_response())
}

async fn anthropic_model(
    State(state): State<ApiState>,
    headers: HeaderMap,
    Path(id): Path<String>,
) -> Result<Response, VendorError> {
    let token = anthropic_key(&headers).map_err(VendorError::anthropic)?;
    let (runtime, key) = authenticate_model_access(&state, token, OperationKind::ModelGet)
        .map_err(VendorError::anthropic)?;
    let lease = reserve_model_limits(&state, &key, token, Surface::Anthropic)
        .await
        .map_err(VendorError::anthropic)?;
    let result = visible_route(&runtime, &key, &id, Surface::Anthropic).map(|slug| {
        (
            StatusCode::OK,
            Json(anthropic_model_object(&runtime, &slug)),
        )
            .into_response()
    });
    release_model_limits(&state, lease.as_ref()).await;
    result
}

fn anthropic_model_object(runtime: &RuntimeBundle, slug: &RouteSlug) -> AnthropicModel {
    AnthropicModel {
        id: slug.to_string(),
        created_at: runtime.generation.activated_at.to_rfc3339(),
        display_name: slug.to_string(),
        kind: "model",
    }
}

#[derive(Default, Deserialize)]
#[serde(rename_all = "camelCase")]
struct GeminiModelsQuery {
    page_size: Option<usize>,
    page_token: Option<String>,
}

#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
struct GeminiModelList {
    models: Vec<GeminiModel>,
    #[serde(skip_serializing_if = "Option::is_none")]
    next_page_token: Option<String>,
}

#[derive(Clone, Serialize)]
#[serde(rename_all = "camelCase")]
struct GeminiModel {
    name: String,
    base_model_id: String,
    version: String,
    display_name: String,
    description: String,
    supported_generation_methods: Vec<&'static str>,
}

async fn gemini_models(
    State(state): State<ApiState>,
    headers: HeaderMap,
    Query(query): Query<GeminiModelsQuery>,
) -> Result<Response, VendorError> {
    let token = gemini_key(&headers).map_err(VendorError::gemini)?;
    let (runtime, key) = authenticate_model_access(&state, token, OperationKind::ModelList)
        .map_err(VendorError::gemini)?;
    let lease = reserve_model_limits(&state, &key, token, Surface::Gemini)
        .await
        .map_err(VendorError::gemini)?;
    let result = gemini_models_response(&runtime, &key, query);
    release_model_limits(&state, lease.as_ref()).await;
    result
}

fn gemini_models_response(
    runtime: &RuntimeBundle,
    key: &ApiKey,
    query: GeminiModelsQuery,
) -> Result<Response, VendorError> {
    let limit = query.page_size.unwrap_or(50);
    if !(1..=1_000).contains(&limit) {
        return Err(VendorError::invalid(
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
    let start = match after.as_deref() {
        Some(cursor) => all
            .iter()
            .position(|slug| slug.as_str() == cursor)
            .map(|index| index.saturating_add(1))
            .ok_or_else(|| {
                VendorError::invalid(Surface::Gemini, "The pageToken is stale or unknown.")
            })?,
        None => 0,
    };
    let remaining = &all[start.min(all.len())..];
    let has_more = remaining.len() > limit;
    let models = remaining
        .iter()
        .take(limit)
        .map(|slug| gemini_model_object(runtime, slug))
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
        Json(GeminiModelList {
            models,
            next_page_token,
        }),
    )
        .into_response())
}

async fn gemini_model(
    State(state): State<ApiState>,
    headers: HeaderMap,
    Path(resource): Path<String>,
) -> Result<Response, VendorError> {
    let token = gemini_key(&headers).map_err(VendorError::gemini)?;
    let (runtime, key) = authenticate_model_access(&state, token, OperationKind::ModelGet)
        .map_err(VendorError::gemini)?;
    let lease = reserve_model_limits(&state, &key, token, Surface::Gemini)
        .await
        .map_err(VendorError::gemini)?;
    let result = if resource.contains(':') {
        Err(VendorError::not_found(
            Surface::Gemini,
            "The requested Gemini model does not exist.",
        ))
    } else {
        visible_route(&runtime, &key, &resource, Surface::Gemini).map(|slug| {
            (StatusCode::OK, Json(gemini_model_object(&runtime, &slug))).into_response()
        })
    };
    release_model_limits(&state, lease.as_ref()).await;
    result
}

fn gemini_model_object(runtime: &RuntimeBundle, slug: &RouteSlug) -> GeminiModel {
    GeminiModel {
        name: format!("models/{slug}"),
        base_model_id: slug.to_string(),
        version: runtime.generation.ordinal.to_string(),
        display_name: slug.to_string(),
        description: "OpenLLMProxy route".to_owned(),
        supported_generation_methods: gemini_methods(runtime, slug),
    }
}

fn visible_routes(runtime: &RuntimeBundle, key: &ApiKey, surface: Surface) -> Vec<RouteSlug> {
    runtime
        .routes
        .keys()
        .filter(|slug| key.allowed_routes.is_empty() || key.allowed_routes.contains(*slug))
        .filter(|slug| route_is_visible(runtime, slug, surface))
        .cloned()
        .collect()
}

fn visible_route(
    runtime: &RuntimeBundle,
    key: &ApiKey,
    id: &str,
    surface: Surface,
) -> Result<RouteSlug, VendorError> {
    let slug = RouteSlug::parse(id.to_owned()).map_err(|_| {
        VendorError::not_found(
            surface,
            "The requested model does not exist or is unavailable.",
        )
    })?;
    if (!key.allowed_routes.is_empty() && !key.allowed_routes.contains(&slug))
        || !runtime.routes.contains_key(&slug)
        || !route_is_visible(runtime, &slug, surface)
    {
        return Err(VendorError::not_found(
            surface,
            "The requested model does not exist or is unavailable.",
        ));
    }
    Ok(slug)
}

fn route_is_visible(runtime: &RuntimeBundle, slug: &RouteSlug, surface: Surface) -> bool {
    !gemini_or_anthropic_operations(runtime, slug, surface).is_empty()
}

fn gemini_methods(runtime: &RuntimeBundle, slug: &RouteSlug) -> Vec<&'static str> {
    gemini_or_anthropic_operations(runtime, slug, Surface::Gemini)
        .into_iter()
        .filter_map(|operation| match operation {
            OperationKind::Generation => Some("generateContent"),
            OperationKind::TokenCount => Some("countTokens"),
            _ => None,
        })
        .collect()
}

fn gemini_or_anthropic_operations(
    runtime: &RuntimeBundle,
    slug: &RouteSlug,
    surface: Surface,
) -> Vec<OperationKind> {
    [OperationKind::Generation, OperationKind::TokenCount]
        .into_iter()
        .filter(|operation| {
            let modes: &[TransportMode] = if *operation == OperationKind::Generation {
                &[TransportMode::Unary, TransportMode::Streaming]
            } else {
                &[TransportMode::Unary]
            };
            modes.iter().any(|mode| {
                select_attempts(runtime, slug, *operation, surface, *mode, &[0; 16]).is_ok()
            })
        })
        .collect()
}

fn encode_page_token(slug: &str) -> String {
    URL_SAFE_NO_PAD.encode(format!("olp-v1:{slug}"))
}

fn decode_page_token(token: &str) -> Result<String, VendorError> {
    let bytes = URL_SAFE_NO_PAD
        .decode(token)
        .map_err(|_| VendorError::invalid(Surface::Gemini, "The pageToken is invalid."))?;
    let decoded = String::from_utf8(bytes)
        .map_err(|_| VendorError::invalid(Surface::Gemini, "The pageToken is invalid."))?;
    decoded
        .strip_prefix("olp-v1:")
        .filter(|slug| !slug.is_empty())
        .map(str::to_owned)
        .ok_or_else(|| VendorError::invalid(Surface::Gemini, "The pageToken is invalid."))
}

fn anthropic_key(headers: &HeaderMap) -> Result<&str, InferenceError> {
    protocol_key(headers, "x-api-key")
}

fn gemini_key(headers: &HeaderMap) -> Result<&str, InferenceError> {
    protocol_key(headers, "x-goog-api-key")
}

fn protocol_key<'a>(headers: &'a HeaderMap, name: &'static str) -> Result<&'a str, InferenceError> {
    let token = headers
        .get(name)
        .and_then(|value| value.to_str().ok())
        .ok_or_else(InferenceError::unauthorized)?;
    if token.is_empty() || token.contains(char::is_whitespace) {
        return Err(InferenceError::unauthorized());
    }
    Ok(token)
}

fn valid_json<T>(
    payload: Result<Json<T>, JsonRejection>,
    surface: Surface,
) -> Result<Json<T>, VendorError> {
    payload.map_err(|error| VendorError::invalid(surface, format!("Invalid JSON request: {error}")))
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

#[derive(Debug)]
struct VendorError {
    surface: Surface,
    error: InferenceError,
}

impl VendorError {
    fn anthropic(error: InferenceError) -> Self {
        Self {
            surface: Surface::Anthropic,
            error,
        }
    }

    fn gemini(error: InferenceError) -> Self {
        Self {
            surface: Surface::Gemini,
            error,
        }
    }

    fn invalid(surface: Surface, message: impl Into<String>) -> Self {
        Self {
            surface,
            error: InferenceError::invalid_request(message),
        }
    }

    fn not_found(surface: Surface, message: impl Into<String>) -> Self {
        Self {
            surface,
            error: InferenceError::not_found(message.into()),
        }
    }

    fn upstream(surface: Surface, message: impl Into<String>) -> Self {
        Self {
            surface,
            error: InferenceError::bad_gateway("provider_protocol_error", message),
        }
    }
}

impl IntoResponse for VendorError {
    fn into_response(self) -> Response {
        let status = self.error.status();
        let retry_after = self.error.retry_after();
        let mut response = match self.surface {
            Surface::Anthropic => (
                status,
                Json(json!({
                    "type": "error",
                    "error": {
                        "type": anthropic_error_kind(&self.error),
                        "message": self.error.message()
                    }
                })),
            )
                .into_response(),
            Surface::Gemini => (status, Json(gemini_error_body(&self.error))).into_response(),
            Surface::OpenAi => self.error.into_response(),
        };
        if let Some(retry_after) = retry_after
            && let Ok(value) = HeaderValue::from_str(&retry_after.as_secs().max(1).to_string())
        {
            response.headers_mut().insert(header::RETRY_AFTER, value);
        }
        response
    }
}

pub(crate) fn problem_response(surface: Surface, problem: crate::Problem) -> Response {
    let status = StatusCode::from_u16(problem.status).unwrap_or(StatusCode::INTERNAL_SERVER_ERROR);
    let code = if status == StatusCode::UNAUTHORIZED {
        "invalid_api_key".to_owned()
    } else {
        problem
            .problem_type
            .rsplit('/')
            .next()
            .unwrap_or("request_failed")
            .to_owned()
    };
    let mut response = match surface {
        Surface::OpenAi => (
            status,
            Json(json!({
                "error": {
                    "message": problem.detail,
                    "type": match status {
                        StatusCode::UNAUTHORIZED => "authentication_error",
                        StatusCode::FORBIDDEN => "permission_error",
                        StatusCode::TOO_MANY_REQUESTS => "rate_limit_error",
                        status if status.is_client_error() => "invalid_request_error",
                        _ => "server_error"
                    },
                    "param": null,
                    "code": code
                }
            })),
        )
            .into_response(),
        Surface::Anthropic => (
            status,
            Json(json!({
                "type": "error",
                "error": {
                    "type": match status {
                        StatusCode::UNAUTHORIZED => "authentication_error",
                        StatusCode::FORBIDDEN => "permission_error",
                        StatusCode::NOT_FOUND => "not_found_error",
                        StatusCode::TOO_MANY_REQUESTS => "rate_limit_error",
                        status if status.is_client_error() => "invalid_request_error",
                        _ => "api_error"
                    },
                    "message": problem.detail
                }
            })),
        )
            .into_response(),
        Surface::Gemini => (
            status,
            Json(json!({
                "error": {
                    "code": status.as_u16(),
                    "message": problem.detail,
                    "status": gemini_error_status(status)
                }
            })),
        )
            .into_response(),
    };
    if matches!(surface, Surface::OpenAi) && status == StatusCode::UNAUTHORIZED {
        response
            .headers_mut()
            .insert(header::WWW_AUTHENTICATE, HeaderValue::from_static("Bearer"));
    }
    response
}

pub(crate) fn inference_error_response(surface: Surface, error: InferenceError) -> Response {
    VendorError { surface, error }.into_response()
}

fn anthropic_error_kind(error: &InferenceError) -> &'static str {
    match error.status() {
        StatusCode::UNAUTHORIZED => "authentication_error",
        StatusCode::FORBIDDEN => "permission_error",
        StatusCode::NOT_FOUND => "not_found_error",
        StatusCode::TOO_MANY_REQUESTS => "rate_limit_error",
        status if status.is_client_error() => "invalid_request_error",
        _ => "api_error",
    }
}

fn gemini_error_body(error: &InferenceError) -> serde_json::Value {
    json!({
        "error": {
            "code": error.status().as_u16(),
            "message": error.message(),
            "status": gemini_error_status(error.status())
        }
    })
}

fn gemini_error_status(status: StatusCode) -> &'static str {
    match status {
        StatusCode::BAD_REQUEST => "INVALID_ARGUMENT",
        StatusCode::UNAUTHORIZED => "UNAUTHENTICATED",
        StatusCode::FORBIDDEN => "PERMISSION_DENIED",
        StatusCode::NOT_FOUND => "NOT_FOUND",
        StatusCode::TOO_MANY_REQUESTS => "RESOURCE_EXHAUSTED",
        StatusCode::GATEWAY_TIMEOUT => "DEADLINE_EXCEEDED",
        StatusCode::SERVICE_UNAVAILABLE => "UNAVAILABLE",
        _ => "INTERNAL",
    }
}
