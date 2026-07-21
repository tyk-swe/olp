use std::sync::Arc;

use axum::{
    Json, Router,
    extract::{Path, State},
    http::{HeaderMap, HeaderValue, StatusCode, header},
    response::{IntoResponse, Response},
    routing::get,
};
use chrono::Utc;
use olp_domain::{
    ApiKey, ApiKeyAuthorizationError, ApiKeyLookupId, OperationKind, RouteSlug, Surface,
    authorize_api_key,
};
use serde::Serialize;

use crate::{
    ApiState, RuntimeBundle,
    gateway::{InferenceError, release_model_limits, reserve_model_limits},
};

pub(crate) fn router() -> Router<ApiState> {
    Router::new()
        .route("/openai/v1/models", get(list_models))
        .route("/openai/v1/models/{id}", get(get_model))
}

async fn list_models(
    State(state): State<ApiState>,
    headers: HeaderMap,
) -> Result<Json<ModelList>, OpenAiModelError> {
    // Pin before authentication: key state and the returned route set must
    // always come from exactly one immutable runtime generation.
    let runtime = crate::pin_inference_runtime(&state);
    let key = authenticate(&state, &runtime, &headers)?;
    authorize_api_key(key, None, OperationKind::ModelList, Utc::now())
        .map_err(map_authorization_error)?;
    let plaintext = bearer_token(&headers)?;
    let lease = reserve_model_limits(&state, key, plaintext, Surface::OpenAi)
        .await
        .map_err(OpenAiModelError::from_inference)?;

    let created = runtime.generation.activated_at.timestamp().max(0);
    let data = runtime
        .routes
        .keys()
        .filter(|slug| key.allowed_routes.is_empty() || key.allowed_routes.contains(*slug))
        .filter(|slug| route_is_visible(&runtime, slug))
        .map(|slug| ModelObject::new(slug.as_str(), created))
        .collect();

    let response = Json(ModelList {
        object: "list",
        data,
    });
    release_model_limits(&state, lease.as_ref()).await;
    Ok(response)
}

async fn get_model(
    State(state): State<ApiState>,
    headers: HeaderMap,
    Path(model_id): Path<String>,
) -> Result<Json<ModelObject>, OpenAiModelError> {
    let runtime = crate::pin_inference_runtime(&state);
    let key = authenticate(&state, &runtime, &headers)?;
    authorize_api_key(key, None, OperationKind::ModelGet, Utc::now())
        .map_err(map_authorization_error)?;
    let plaintext = bearer_token(&headers)?;
    let lease = reserve_model_limits(&state, key, plaintext, Surface::OpenAi)
        .await
        .map_err(OpenAiModelError::from_inference)?;

    let result = (|| {
        let slug = RouteSlug::parse(model_id.clone())
            .map_err(|_| OpenAiModelError::model_not_found(&model_id))?;
        if !runtime.routes.contains_key(&slug)
            || (!key.allowed_routes.is_empty() && !key.allowed_routes.contains(&slug))
            || !route_is_visible(&runtime, &slug)
        {
            return Err(OpenAiModelError::model_not_found(&model_id));
        }

        Ok(Json(ModelObject::new(
            slug.as_str(),
            runtime.generation.activated_at.timestamp().max(0),
        )))
    })();
    release_model_limits(&state, lease.as_ref()).await;
    result
}

fn route_is_visible(runtime: &RuntimeBundle, slug: &RouteSlug) -> bool {
    let Some(route) = runtime.routes.get(slug) else {
        return false;
    };
    route.targets.iter().any(|target| {
        runtime
            .providers
            .get(&target.provider_id)
            .is_some_and(|provider| {
                provider.enabled
                    && provider.capabilities.iter().any(|capability| {
                        capability.model == target.upstream_model
                            && capability.surface == Surface::OpenAi
                            && route.operations.contains(&capability.operation)
                            && !matches!(
                                capability.operation,
                                OperationKind::ModelList | OperationKind::ModelGet
                            )
                    })
            })
    })
}

fn authenticate<'a>(
    state: &ApiState,
    runtime: &'a Arc<RuntimeBundle>,
    headers: &HeaderMap,
) -> Result<&'a ApiKey, OpenAiModelError> {
    let plaintext = bearer_token(headers)?;
    let auth_hmac_key = state
        .auth_hmac_key
        .as_ref()
        .ok_or_else(OpenAiModelError::authentication_unavailable)?;
    let lookup = auth_hmac_key
        .lookup_id(plaintext)
        .map_err(|_| OpenAiModelError::unauthorized())?;
    let lookup_id = ApiKeyLookupId::parse(lookup).map_err(|_| OpenAiModelError::unauthorized())?;
    let key = runtime
        .api_keys
        .get(&lookup_id)
        .ok_or_else(OpenAiModelError::unauthorized)?;
    auth_hmac_key
        .parse_and_verify(plaintext, key.digest.as_bytes())
        .map_err(|_| OpenAiModelError::unauthorized())?;
    Ok(key)
}

fn bearer_token(headers: &HeaderMap) -> Result<&str, OpenAiModelError> {
    let value = headers
        .get(header::AUTHORIZATION)
        .and_then(|value| value.to_str().ok())
        .ok_or_else(OpenAiModelError::unauthorized)?;
    let (scheme, token) = value
        .split_once(' ')
        .ok_or_else(OpenAiModelError::unauthorized)?;
    if !scheme.eq_ignore_ascii_case("bearer")
        || token.is_empty()
        || token.contains(char::is_whitespace)
    {
        return Err(OpenAiModelError::unauthorized());
    }
    Ok(token)
}

fn map_authorization_error(error: ApiKeyAuthorizationError) -> OpenAiModelError {
    match error {
        ApiKeyAuthorizationError::Revoked | ApiKeyAuthorizationError::Expired => {
            OpenAiModelError::unauthorized()
        }
        ApiKeyAuthorizationError::MissingScope { .. }
        | ApiKeyAuthorizationError::RouteNotAllowed { .. } => OpenAiModelError::forbidden(),
    }
}

#[derive(Debug, Serialize)]
struct ModelList {
    object: &'static str,
    data: Vec<ModelObject>,
}

#[derive(Debug, Serialize)]
struct ModelObject {
    id: String,
    object: &'static str,
    created: i64,
    owned_by: &'static str,
}

impl ModelObject {
    fn new(id: &str, created: i64) -> Self {
        Self {
            id: id.to_owned(),
            object: "model",
            created,
            owned_by: "openllmproxy",
        }
    }
}

#[derive(Debug)]
struct OpenAiModelError {
    status: StatusCode,
    code: &'static str,
    kind: &'static str,
    message: String,
    retry_after: Option<std::time::Duration>,
}

impl OpenAiModelError {
    fn unauthorized() -> Self {
        Self {
            status: StatusCode::UNAUTHORIZED,
            code: "invalid_api_key",
            kind: "authentication_error",
            message: "The API key is invalid or unavailable.".to_owned(),
            retry_after: None,
        }
    }

    fn forbidden() -> Self {
        Self {
            status: StatusCode::FORBIDDEN,
            code: "permission_denied",
            kind: "permission_error",
            message: "The API key does not have the models_read scope.".to_owned(),
            retry_after: None,
        }
    }

    fn model_not_found(id: &str) -> Self {
        Self {
            status: StatusCode::NOT_FOUND,
            code: "model_not_found",
            kind: "invalid_request_error",
            message: format!("The model `{id}` does not exist or you do not have access to it."),
            retry_after: None,
        }
    }

    fn authentication_unavailable() -> Self {
        Self {
            status: StatusCode::SERVICE_UNAVAILABLE,
            code: "api_key_authentication_unavailable",
            kind: "service_unavailable_error",
            message: "The gateway is temporarily unavailable.".to_owned(),
            retry_after: None,
        }
    }

    fn from_inference(error: InferenceError) -> Self {
        let status = error.status();
        let (code, kind) = if status == StatusCode::TOO_MANY_REQUESTS {
            ("rate_limit_exceeded", "rate_limit_error")
        } else {
            ("service_unavailable", "service_unavailable_error")
        };
        Self {
            status,
            code,
            kind,
            message: error.message().to_owned(),
            retry_after: error.retry_after(),
        }
    }
}

#[derive(Serialize)]
struct OpenAiErrorEnvelope<'a> {
    error: OpenAiErrorBody<'a>,
}

#[derive(Serialize)]
struct OpenAiErrorBody<'a> {
    message: &'a str,
    #[serde(rename = "type")]
    kind: &'a str,
    param: Option<&'a str>,
    code: &'a str,
}

impl IntoResponse for OpenAiModelError {
    fn into_response(self) -> Response {
        let is_unauthorized = self.status == StatusCode::UNAUTHORIZED;
        let mut response = (
            self.status,
            Json(OpenAiErrorEnvelope {
                error: OpenAiErrorBody {
                    message: &self.message,
                    kind: self.kind,
                    param: None,
                    code: self.code,
                },
            }),
        )
            .into_response();
        if is_unauthorized {
            response
                .headers_mut()
                .insert(header::WWW_AUTHENTICATE, HeaderValue::from_static("Bearer"));
        }
        if let Some(retry_after) = self.retry_after {
            let value = HeaderValue::from_str(&retry_after.as_secs().max(1).to_string())
                .expect("retry-after seconds are a valid header value");
            response.headers_mut().insert(header::RETRY_AFTER, value);
        }
        response
    }
}

#[cfg(test)]
mod tests {
    use std::{
        collections::{BTreeMap, BTreeSet},
        num::{NonZeroU16, NonZeroU32},
        sync::Arc,
    };

    use axum::{body::Body, http::Request};
    use chrono::{TimeZone, Utc};
    use futures::stream;
    use http_body_util::BodyExt;
    use olp_domain::{
        ApiKeyDigest, ApiKeyId, ApiKeyLimits, ApiKeyScope, ApiKeyStatus, BoxFuture, Capability,
        DurationMs, Provider, ProviderEventStream, ProviderId, ProviderKind, ProviderOutput,
        ProviderRequest, ProviderTransport, Route, RouteId, RuntimeGeneration, RuntimeGenerationId,
        RuntimeSnapshot, Target, TargetId, TransportError, TransportMode,
    };
    use olp_storage::AuthHmacKey;
    use serde_json::Value;
    use tower::ServiceExt;

    use super::*;

    struct UnusedTransport;

    impl ProviderTransport for UnusedTransport {
        fn execute<'a>(
            &'a self,
            _request: ProviderRequest,
        ) -> BoxFuture<'a, Result<ProviderOutput, TransportError>> {
            Box::pin(async {
                Ok(ProviderOutput::Events(
                    Box::pin(stream::empty::<Result<_, TransportError>>()) as ProviderEventStream,
                ))
            })
        }
    }

    fn test_state(
        scopes: BTreeSet<ApiKeyScope>,
        allowed_routes: BTreeSet<RouteSlug>,
    ) -> (ApiState, String) {
        let auth_hmac_key = Arc::new(AuthHmacKey::new([11; 32]));
        let material = auth_hmac_key.generate_api_key();
        let plaintext = material.expose_once().to_owned();
        let lookup = ApiKeyLookupId::parse(material.lookup_id.clone()).unwrap();
        let provider_id = ProviderId::new();
        let make_route = |slug: &str, operation: OperationKind| {
            let slug = RouteSlug::parse(slug).unwrap();
            (
                slug.clone(),
                Route {
                    id: RouteId::new(),
                    routing_id: None,
                    slug,
                    operations: BTreeSet::from([operation]),
                    overall_timeout: DurationMs::new(1_000),
                    max_attempts: NonZeroU16::new(1).unwrap(),
                    targets: vec![Target {
                        id: TargetId::new(),
                        routing_id: None,
                        provider_id,
                        upstream_model: "private-upstream-model".to_owned(),
                        priority: 0,
                        weight: NonZeroU32::new(1).unwrap(),
                        timeout: DurationMs::new(1_000),
                    }],
                },
            )
        };
        let snapshot = RuntimeSnapshot {
            generation: RuntimeGeneration {
                id: RuntimeGenerationId::new(),
                ordinal: 7,
                activated_at: Utc.timestamp_opt(1_700_000_000, 0).unwrap(),
            },
            providers: BTreeMap::from([(
                provider_id,
                Provider {
                    id: provider_id,
                    name: "private-provider".to_owned(),
                    kind: ProviderKind::OpenAi,
                    enabled: true,
                    active_credential: None,
                    capabilities: BTreeSet::from([
                        Capability::new(
                            "private-upstream-model",
                            OperationKind::Generation,
                            Surface::OpenAi,
                            TransportMode::Unary,
                        ),
                        Capability::new(
                            "private-upstream-model",
                            OperationKind::ImageGeneration,
                            Surface::OpenAi,
                            TransportMode::Streaming,
                        ),
                    ]),
                },
            )]),
            routes: BTreeMap::from([
                make_route("zeta", OperationKind::Generation),
                make_route("alpha", OperationKind::Generation),
                make_route("media-stream", OperationKind::ImageGeneration),
            ]),
            api_keys: BTreeMap::from([(
                lookup.clone(),
                ApiKey {
                    id: ApiKeyId::new(),
                    lookup_id: lookup,
                    digest: ApiKeyDigest::new(material.digest),
                    status: ApiKeyStatus::Active,
                    expires_at: None,
                    scopes,
                    allowed_routes,
                    limits: ApiKeyLimits::default(),
                },
            )]),
        };
        let runtime = Arc::new(crate::RuntimeManager::empty());
        let transport: Arc<dyn ProviderTransport> = Arc::new(UnusedTransport);
        runtime
            .install(snapshot, BTreeMap::from([(provider_id, transport)]))
            .unwrap();
        let mut state = ApiState::new(
            crate::ApiMode::Gateway,
            None,
            runtime,
            "https://olp.test",
            "console",
        );
        state.auth_hmac_key = Some(auth_hmac_key);
        (state, plaintext)
    }

    async fn json(response: Response) -> Value {
        let body = response.into_body().collect().await.unwrap().to_bytes();
        serde_json::from_slice(&body).unwrap()
    }

    #[tokio::test]
    async fn list_returns_sorted_public_route_slugs_only() {
        let (state, key) = test_state(BTreeSet::from([ApiKeyScope::ModelsRead]), BTreeSet::new());
        let response = crate::public_router(state)
            .oneshot(
                Request::get("/openai/v1/models")
                    .header(header::AUTHORIZATION, format!("Bearer {key}"))
                    .body(Body::empty())
                    .unwrap(),
            )
            .await
            .unwrap();

        assert_eq!(response.status(), StatusCode::OK);
        let body = json(response).await;
        assert_eq!(body["object"], "list");
        assert_eq!(body["data"][0]["id"], "alpha");
        assert_eq!(body["data"][1]["id"], "media-stream");
        assert_eq!(body["data"][2]["id"], "zeta");
        assert_eq!(body["data"][0]["created"], 1_700_000_000_i64);
        assert_eq!(body["data"][0]["owned_by"], "openllmproxy");
        assert!(!body.to_string().contains("private-upstream-model"));
        assert!(!body.to_string().contains("private-provider"));
    }

    #[tokio::test]
    async fn get_returns_route_slug_as_openai_model() {
        let (state, key) = test_state(BTreeSet::from([ApiKeyScope::ModelsRead]), BTreeSet::new());
        let app = crate::public_router(state);
        let response = app
            .clone()
            .oneshot(
                Request::get("/openai/v1/models/alpha")
                    .header(header::AUTHORIZATION, format!("Bearer {key}"))
                    .body(Body::empty())
                    .unwrap(),
            )
            .await
            .unwrap();

        assert_eq!(response.status(), StatusCode::OK);
        let body = json(response).await;
        assert_eq!(body["id"], "alpha");
        assert_eq!(body["object"], "model");

        let response = app
            .oneshot(
                Request::get("/openai/v1/models/media-stream")
                    .header(header::AUTHORIZATION, format!("Bearer {key}"))
                    .body(Body::empty())
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(response.status(), StatusCode::OK);
        let body = json(response).await;
        assert_eq!(body["id"], "media-stream");
    }

    #[tokio::test]
    async fn models_read_scope_is_required_with_native_error_envelope() {
        let (state, key) = test_state(BTreeSet::from([ApiKeyScope::Inference]), BTreeSet::new());
        let response = crate::public_router(state)
            .oneshot(
                Request::get("/openai/v1/models")
                    .header(header::AUTHORIZATION, format!("Bearer {key}"))
                    .body(Body::empty())
                    .unwrap(),
            )
            .await
            .unwrap();

        assert_eq!(response.status(), StatusCode::FORBIDDEN);
        let body = json(response).await;
        assert_eq!(body["error"]["type"], "permission_error");
        assert_eq!(body["error"]["code"], "permission_denied");
    }

    #[tokio::test]
    async fn invalid_key_is_a_native_openai_authentication_error() {
        let (state, _) = test_state(BTreeSet::from([ApiKeyScope::ModelsRead]), BTreeSet::new());
        let response = crate::public_router(state)
            .oneshot(
                Request::get("/openai/v1/models")
                    .header(header::AUTHORIZATION, "Bearer not-a-valid-key")
                    .body(Body::empty())
                    .unwrap(),
            )
            .await
            .unwrap();

        assert_eq!(response.status(), StatusCode::UNAUTHORIZED);
        assert_eq!(
            response.headers().get(header::WWW_AUTHENTICATE).unwrap(),
            "Bearer"
        );
        let body = json(response).await;
        assert_eq!(body["error"]["type"], "authentication_error");
        assert_eq!(body["error"]["code"], "invalid_api_key");
    }

    #[tokio::test]
    async fn get_conceals_missing_and_disallowed_routes() {
        let allowed = BTreeSet::from([RouteSlug::parse("alpha").unwrap()]);
        let (state, key) = test_state(BTreeSet::from([ApiKeyScope::ModelsRead]), allowed);
        let app = crate::public_router(state);

        let list = app
            .clone()
            .oneshot(
                Request::get("/openai/v1/models")
                    .header(header::AUTHORIZATION, format!("Bearer {key}"))
                    .body(Body::empty())
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(list.status(), StatusCode::OK);
        let body = json(list).await;
        assert_eq!(body["data"].as_array().unwrap().len(), 1);
        assert_eq!(body["data"][0]["id"], "alpha");

        for model in ["zeta", "media-stream", "does-not-exist", "INVALID"] {
            let response = app
                .clone()
                .oneshot(
                    Request::get(format!("/openai/v1/models/{model}"))
                        .header(header::AUTHORIZATION, format!("Bearer {key}"))
                        .body(Body::empty())
                        .unwrap(),
                )
                .await
                .unwrap();
            assert_eq!(response.status(), StatusCode::NOT_FOUND);
            let body = json(response).await;
            assert_eq!(body["error"]["code"], "model_not_found");
        }
    }
}
