use crate::access::policy::ApiKey;
use crate::http::request_admission::HttpRequestAdmission;
use crate::ids::RouteSlug;
use crate::protocols::canonical::identity::OperationKind;
use crate::protocols::canonical::identity::Surface;
use crate::protocols::canonical::identity::TransportMode;
use crate::protocols::canonical::requests::Operation;
use crate::protocols::canonical::results::CanonicalResult;
use crate::protocols::gemini::client::encode_generate_content_response;
use crate::protocols::gemini::client_stream::Encoder;
use crate::protocols::gemini::count::decode_count_tokens_request;
use crate::protocols::gemini::count::encode_count_tokens_result;
use crate::protocols::gemini::dto::CountTokensRequest;
use crate::protocols::gemini::dto::GenerateContentRequest;
use crate::protocols::gemini::translate::decode::request as decode_request;
use axum::Json;
use axum::extract::Extension;
use axum::extract::Path;
use axum::extract::Query;
use axum::extract::State;
use axum::extract::rejection::JsonRejection;
use axum::http::StatusCode;
use axum::response::IntoResponse;
use axum::response::Response;
use base64::Engine as _;
use base64::engine::general_purpose::URL_SAFE_NO_PAD;
use serde::Deserialize;
use serde::Serialize;

use crate::inference::execution::CompletedEvents;
use crate::inference::execution::RoutedUnaryResult;
use crate::runtime::manager::Bundle;

use crate::http::json_media::admit_gemini_count;
use crate::http::json_media::admit_gemini_generate;
use crate::http::streaming_response::ProtocolStreamEncoder;
use crate::http::streaming_response::encode_protocol_sse_frames;
use crate::http::streaming_response::encode_server_sse_frame;
use crate::http::streaming_response::precommit_stream_failure;
use crate::http::streaming_response::protocol_streaming_response;
use crate::inference::http::state::GatewayState;

use crate::inference::http::authorize_model_access;
use crate::inference::http::error::InferenceError;
use crate::inference::http::execution::execute_event_operation;
use crate::inference::http::execution::execute_routed_result;
use crate::inference::http::native_models::after_cursor_start;
use crate::inference::http::native_models::supported_operations;
use crate::inference::http::native_models::visible_route;
use crate::inference::http::native_models::visible_routes;
use crate::inference::http::protocol_error::ProtocolError;
use crate::inference::http::protocol_error::gemini_error_body;
use crate::inference::http::protocol_error::valid_json;
use crate::inference::http::release_model_limits;
use crate::inference::http::reserve_model_limits;

pub(crate) async fn action(
    State(state): State<GatewayState>,
    Extension(principal): Extension<HttpRequestAdmission>,
    Path(resource): Path<String>,
    Query(query): Query<ActionQuery>,
    payload: Result<Json<serde_json::Value>, JsonRejection>,
) -> Result<Response, ProtocolError> {
    let Json(value) = valid_json(payload, Surface::Gemini)?;
    if let Some(model) = resource.strip_suffix(":generateContent") {
        let operation = decode_generate_operation(&state, value, model, false).await?;
        let execution =
            execute_event_operation(&state, &principal, operation, TransportMode::Unary)
                .await
                .map_err(ProtocolError::gemini)?;
        let completed = execution
            .collect()
            .await
            .map_err(InferenceError::from)
            .map_err(ProtocolError::gemini)?;
        return unary_response(completed);
    }
    if let Some(model) = resource.strip_suffix(":streamGenerateContent") {
        // Real Gemini frames the default (`alt` absent) as a streamed JSON
        // array, not SSE. Serving `text/event-stream` there leaves an official
        // SDK unable to parse the body, so the parameter is required.
        if query.alt.as_deref() != Some("sse") {
            return Err(ProtocolError::invalid(
                Surface::Gemini,
                "streamGenerateContent supports only alt=sse.",
            ));
        }
        let operation = decode_generate_operation(&state, value, model, true).await?;
        let execution =
            execute_event_operation(&state, &principal, operation, TransportMode::Streaming)
                .await
                .map_err(ProtocolError::gemini)?;
        let execution = precommit_stream_failure(execution).map_err(ProtocolError::gemini)?;
        let encoder = Encoder::new(
            execution.route_slug.as_str(),
            execution.request_id.to_string(),
        );
        return Ok(protocol_streaming_response(execution, encoder));
    }
    if let Some(model) = resource.strip_suffix(":countTokens") {
        let operation = decode_count_operation(&state, value, model).await?;
        let executed =
            execute_routed_result(&state, &principal, operation, TransportMode::Unary, None)
                .await
                .map_err(ProtocolError::gemini)?;
        return count_result(executed);
    }
    Err(ProtocolError::not_found(
        Surface::Gemini,
        "The requested Gemini method does not exist.",
    ))
}

async fn decode_generate_operation(
    state: &GatewayState,
    value: serde_json::Value,
    model: &str,
    stream: bool,
) -> Result<Operation, ProtocolError> {
    let mut request: GenerateContentRequest = serde_json::from_value(value).map_err(|error| {
        ProtocolError::invalid(Surface::Gemini, format!("Invalid JSON request: {error}"))
    })?;
    let admitted = admit_gemini_generate(state, &mut request)
        .await
        .map_err(ProtocolError::gemini)?;
    match decode_request(model, request, stream) {
        Ok(operation) => {
            admitted.disarm();
            Ok(operation)
        }
        Err(error) => {
            admitted.release().await;
            let method = if stream {
                "streamGenerateContent"
            } else {
                "generateContent"
            };
            Err(ProtocolError::invalid(
                Surface::Gemini,
                format!("Invalid {method} request: {error}"),
            ))
        }
    }
}

async fn decode_count_operation(
    state: &GatewayState,
    value: serde_json::Value,
    model: &str,
) -> Result<Operation, ProtocolError> {
    let mut request: CountTokensRequest = serde_json::from_value(value).map_err(|error| {
        ProtocolError::invalid(Surface::Gemini, format!("Invalid JSON request: {error}"))
    })?;
    let admitted = admit_gemini_count(state, &mut request)
        .await
        .map_err(ProtocolError::gemini)?;
    match decode_count_tokens_request(model, request) {
        Ok(operation) => {
            admitted.disarm();
            Ok(operation)
        }
        Err(error) => {
            admitted.release().await;
            Err(ProtocolError::invalid(
                Surface::Gemini,
                format!("Invalid countTokens request: {error}"),
            ))
        }
    }
}

fn unary_response(mut completed: CompletedEvents) -> Result<Response, ProtocolError> {
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
        executed.mark_provider_protocol_failure();
        return Err(ProtocolError::upstream(
            Surface::Gemini,
            "The provider returned an incompatible token-count result.",
        ));
    };
    let response = match encode_count_tokens_result(result) {
        Ok(response) => response,
        Err(error) => {
            executed.mark_provider_protocol_failure();
            return Err(ProtocolError::upstream(
                Surface::Gemini,
                format!("The token-count result is not representable: {error}"),
            ));
        }
    };
    executed.mark_success();
    Ok((StatusCode::OK, Json(response)).into_response())
}

#[derive(Default, Deserialize)]
#[serde(rename_all = "camelCase")]
pub(crate) struct ActionQuery {
    alt: Option<String>,
}

#[derive(Default, Deserialize)]
#[serde(rename_all = "camelCase")]
pub(crate) struct ModelsQuery {
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

pub(crate) async fn models(
    State(state): State<GatewayState>,
    Extension(principal): Extension<HttpRequestAdmission>,
    Query(query): Query<ModelsQuery>,
) -> Result<Response, ProtocolError> {
    let (runtime, key) =
        authorize_model_access(&state, &principal).map_err(ProtocolError::gemini)?;
    let lease = reserve_model_limits(&state, &principal)
        .await
        .map_err(ProtocolError::gemini)?;
    let result = models_response(
        runtime,
        key,
        query,
        &principal.principal().routing_preferences,
    );
    release_model_limits(&state, lease).await;
    result
}

fn models_response(
    runtime: &Bundle,
    key: &ApiKey,
    query: ModelsQuery,
    preferences: &crate::routes::policy::RoutingPreferences,
) -> Result<Response, ProtocolError> {
    let limit = query.page_size.unwrap_or(50);
    if !(1..=1_000).contains(&limit) {
        return Err(ProtocolError::invalid(
            Surface::Gemini,
            "pageSize must be between 1 and 1000.",
        ));
    }
    let all = visible_routes(runtime, key, Surface::Gemini, preferences);
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
        .map(|slug| model_object(runtime, slug, key, preferences))
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

pub(crate) async fn model(
    State(state): State<GatewayState>,
    Extension(principal): Extension<HttpRequestAdmission>,
    Path(resource): Path<String>,
) -> Result<Response, ProtocolError> {
    let (runtime, key) =
        authorize_model_access(&state, &principal).map_err(ProtocolError::gemini)?;
    let lease = reserve_model_limits(&state, &principal)
        .await
        .map_err(ProtocolError::gemini)?;
    let result = if resource.contains(':') {
        Err(ProtocolError::not_found(
            Surface::Gemini,
            "The requested Gemini model does not exist.",
        ))
    } else {
        visible_route(
            runtime,
            key,
            &resource,
            Surface::Gemini,
            &principal.principal().routing_preferences,
        )
        .map(|slug| {
            (
                StatusCode::OK,
                Json(model_object(
                    runtime,
                    &slug,
                    key,
                    &principal.principal().routing_preferences,
                )),
            )
                .into_response()
        })
    };
    release_model_limits(&state, lease).await;
    result
}

fn model_object(
    runtime: &Bundle,
    slug: &RouteSlug,
    key: &ApiKey,
    preferences: &crate::routes::policy::RoutingPreferences,
) -> Model {
    Model {
        name: format!("models/{slug}"),
        base_model_id: slug.to_string(),
        version: runtime.generation.ordinal.to_string(),
        display_name: slug.to_string(),
        description: "OpenLLMProxy route".to_owned(),
        supported_generation_methods: supported_operations(
            runtime,
            slug,
            Surface::Gemini,
            key,
            preferences,
        )
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

impl ProtocolStreamEncoder for Encoder {
    fn push(
        &mut self,
        event: crate::protocols::canonical::events::Event,
    ) -> Result<Vec<bytes::Bytes>, InferenceError> {
        encode_protocol_sse_frames(Encoder::push(self, event))
    }

    fn encode_error(&self, error: &InferenceError) -> bytes::Bytes {
        encode_server_sse_frame(&crate::protocols::sse::Frame {
            event: None,
            data: gemini_error_body(error.status(), error.message()).to_string(),
            id: None,
            retry_ms: None,
        })
    }
}

#[cfg(test)]
mod tests {
    use crate::inference::http::gemini::*;

    #[test]
    fn page_tokens_are_versioned_url_safe_and_round_trip_unicode_routes() {
        for slug in ["chat", "route-with-dashes", "ümlaut"] {
            let token = encode_page_token(slug);
            assert!(!token.contains('='));
            assert_eq!(decode_page_token(&token).unwrap(), slug);
        }
    }

    #[test]
    fn page_tokens_reject_invalid_encoding_version_utf8_and_empty_cursor() {
        let invalid_utf8 = URL_SAFE_NO_PAD.encode([0xff, 0xfe]);
        for token in [
            "%%%".to_owned(),
            URL_SAFE_NO_PAD.encode("another-v1:chat"),
            URL_SAFE_NO_PAD.encode("olp-v1:"),
            invalid_utf8,
        ] {
            assert!(decode_page_token(&token).is_err(), "accepted {token:?}");
        }
    }
}
