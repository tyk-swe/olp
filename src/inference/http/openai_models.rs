use crate::http::request_admission::HttpRequestAdmission;
use crate::ids::RouteSlug;
use crate::protocols::canonical::identity::OperationKind;
use crate::protocols::canonical::identity::Surface;
use axum::Json;
use axum::extract::Extension;
use axum::extract::Path;
use axum::extract::State;
use axum::http::StatusCode;
use axum::response::IntoResponse;
use axum::response::Response;
use serde::Serialize;

use crate::runtime::manager::Bundle;

use crate::inference::http::authorize_model_access;
use crate::inference::http::error::InferenceError;
use crate::inference::http::release_model_limits;
use crate::inference::http::reserve_model_limits;
use crate::inference::http::state::GatewayState;

use crate::inference::http::error::openai_error_response;

pub(crate) async fn list_models(
    State(state): State<GatewayState>,
    Extension(principal): Extension<HttpRequestAdmission>,
) -> Result<Json<ModelList>, OpenAiModelError> {
    let (runtime, key) =
        authorize_model_access(&state, &principal).map_err(OpenAiModelError::from_inference)?;
    let lease = reserve_model_limits(&state, &principal)
        .await
        .map_err(OpenAiModelError::from_inference)?;

    let created = runtime.generation.activated_at.timestamp().max(0);
    let data = runtime
        .routes
        .keys()
        .filter(|slug| key.allowed_routes.is_empty() || key.allowed_routes.contains(*slug))
        .filter(|slug| route_is_visible(runtime, slug))
        .map(|slug| ModelObject::new(slug.as_str(), created))
        .collect();

    let response = Json(ModelList {
        object: "list",
        data,
    });
    release_model_limits(&state, lease).await;
    Ok(response)
}

pub(crate) async fn get_model(
    State(state): State<GatewayState>,
    Extension(principal): Extension<HttpRequestAdmission>,
    Path(model_id): Path<String>,
) -> Result<Json<ModelObject>, OpenAiModelError> {
    let (runtime, key) =
        authorize_model_access(&state, &principal).map_err(OpenAiModelError::from_inference)?;
    let lease = reserve_model_limits(&state, &principal)
        .await
        .map_err(OpenAiModelError::from_inference)?;

    let result = (|| {
        let slug = RouteSlug::parse(model_id.clone())
            .map_err(|_| OpenAiModelError::model_not_found(&model_id))?;
        if !runtime.routes.contains_key(&slug)
            || (!key.allowed_routes.is_empty() && !key.allowed_routes.contains(&slug))
            || !route_is_visible(runtime, &slug)
        {
            return Err(OpenAiModelError::model_not_found(&model_id));
        }

        Ok(Json(ModelObject::new(
            slug.as_str(),
            runtime.generation.activated_at.timestamp().max(0),
        )))
    })();
    release_model_limits(&state, lease).await;
    result
}

fn route_is_visible(runtime: &Bundle, slug: &RouteSlug) -> bool {
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

#[derive(Debug, Serialize)]
pub(crate) struct ModelList {
    object: &'static str,
    data: Vec<ModelObject>,
}

#[derive(Debug, Serialize)]
pub(crate) struct ModelObject {
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
pub(crate) struct OpenAiModelError {
    status: StatusCode,
    code: &'static str,
    kind: &'static str,
    message: String,
    retry_after: Option<std::time::Duration>,
}

impl OpenAiModelError {
    fn model_not_found(id: &str) -> Self {
        Self {
            status: StatusCode::NOT_FOUND,
            code: "model_not_found",
            kind: "invalid_request_error",
            message: format!("The model `{id}` does not exist or you do not have access to it."),
            retry_after: None,
        }
    }

    fn from_inference(error: InferenceError) -> Self {
        Self {
            status: error.status(),
            code: error.code(),
            kind: error.kind(),
            message: error.message().to_owned(),
            retry_after: error.retry_after(),
        }
    }
}

impl IntoResponse for OpenAiModelError {
    fn into_response(self) -> Response {
        openai_error_response(
            self.status,
            self.code,
            self.kind,
            &self.message,
            self.retry_after,
            self.status == StatusCode::UNAUTHORIZED,
        )
    }
}

#[cfg(test)]
mod tests {
    use std::collections::BTreeMap;
    use std::collections::BTreeSet;
    use std::num::NonZeroU16;
    use std::num::NonZeroU32;
    use std::sync::Arc;

    use crate::access::policy::ApiKey;
    use crate::access::policy::ApiKeyDigest;
    use crate::access::policy::ApiKeyLimits;
    use crate::access::policy::ApiKeyScope;
    use crate::access::policy::ApiKeyStatus;
    use crate::crypto::key_material::AuthHmacKey;
    use crate::ids::ApiKeyId;
    use crate::ids::ApiKeyLookupId;
    use crate::ids::DurationMs;
    use crate::ids::ProviderId;
    use crate::ids::RouteId;
    use crate::ids::RuntimeGenerationId;
    use crate::ids::TargetId;
    use crate::inference::transport::BoxFuture;
    use crate::inference::transport::ProviderEventStream;
    use crate::inference::transport::ProviderOutput;
    use crate::inference::transport::ProviderRequest;
    use crate::inference::transport::ProviderTransport;
    use crate::inference::transport::TransportError;
    use crate::protocols::canonical::identity::TransportMode;
    use crate::providers::runtime_model::Capability;
    use crate::providers::runtime_model::Provider;
    use crate::providers::runtime_model::ProviderKind;
    use crate::routes::model::Route;
    use crate::routes::model::Target;
    use crate::runtime::snapshot::RuntimeGeneration;
    use crate::runtime::snapshot::Snapshot;
    use axum::body::Body;
    use axum::http::Request;
    use axum::http::header;
    use chrono::TimeZone;
    use chrono::Utc;
    use futures::stream;
    use http_body_util::BodyExt;
    use serde_json::Value;
    use tower::ServiceExt;

    use crate::inference::http::openai_models::*;

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
    ) -> (GatewayState, String) {
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
                    routing_id: RouteId::new(),
                    slug,
                    operations: BTreeSet::from([operation]),
                    overall_timeout: DurationMs::new(1_000),
                    max_attempts: NonZeroU16::new(1).unwrap(),
                    targets: vec![Target {
                        id: TargetId::new(),
                        routing_id: TargetId::new(),
                        provider_id,
                        upstream_model: "private-upstream-model".to_owned(),
                        priority: 0,
                        weight: NonZeroU32::new(1).unwrap(),
                        timeout: DurationMs::new(1_000),
                    }],
                },
            )
        };
        let snapshot = Snapshot {
            generation: RuntimeGeneration {
                id: RuntimeGenerationId::new(),
                ordinal: 7,
                activated_at: Utc.timestamp_opt(1_700_000_000, 0).unwrap(),
            },
            providers: BTreeMap::from([(
                provider_id,
                Provider {
                    id: provider_id,
                    revision_id: uuid::Uuid::now_v7(),
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
        let runtime = Arc::new(crate::runtime::manager::Manager::empty());
        let transport: Arc<dyn ProviderTransport> = Arc::new(UnusedTransport);
        runtime
            .install(snapshot, BTreeMap::from([(provider_id, transport)]))
            .unwrap();
        let mut state = GatewayState::new(
            crate::process::mode::ApiMode::Gateway,
            None,
            runtime,
            "https://olp.test",
            "console",
        );
        state.replace_auth_hmac_key_for_test(auth_hmac_key);
        (state, plaintext)
    }

    async fn json(response: Response) -> Value {
        let body = response.into_body().collect().await.unwrap().to_bytes();
        serde_json::from_slice(&body).unwrap()
    }

    #[tokio::test]
    async fn list_returns_sorted_public_route_slugs_only() {
        let (state, key) = test_state(BTreeSet::from([ApiKeyScope::ModelsRead]), BTreeSet::new());
        let response = crate::http::router::gateway_router_for_test(state)
            .oneshot(
                Request::get("/v1/models")
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
        let app = crate::http::router::gateway_router_for_test(state);
        let response = app
            .clone()
            .oneshot(
                Request::get("/v1/models/alpha")
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
                Request::get("/v1/models/media-stream")
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
        let response = crate::http::router::gateway_router_for_test(state)
            .oneshot(
                Request::get("/v1/models")
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
        let response = crate::http::router::gateway_router_for_test(state)
            .oneshot(
                Request::get("/v1/models")
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
        let app = crate::http::router::gateway_router_for_test(state);

        let list = app
            .clone()
            .oneshot(
                Request::get("/v1/models")
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
                    Request::get(format!("/v1/models/{model}"))
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
