//! HTTP request admission, inference reservations, and body safety limits.

use std::{
    future::Future,
    net::SocketAddr,
    pin::Pin,
    sync::{
        Arc,
        atomic::{AtomicBool, Ordering},
    },
};

use axum::{
    body::Body,
    extract::State,
    http::{HeaderMap, Request},
    middleware,
    response::{IntoResponse, Response},
};
use olp_domain::{OperationKind, Surface};
use olp_storage::{RequestMetadataEmitter, RequestMetadataEvent};

use crate::{
    GatewayState, MAX_HTTP_HEADER_BYTES, MAX_HTTP_HEADER_COUNT, MAX_JSON_BODY_BYTES, Problem,
    RuntimeBundle, gateway, management_api, proxy::public_auth_source,
    router::REQUEST_BODY_TIMEOUT,
};

mod limits;
mod multipart;
mod validation;

use limits::{
    authenticate_inference_headers, estimate_http_non_json_request_tokens,
    reserve_http_inference_limits,
};
use multipart::preauthorize_multipart;
use validation::{is_json_content_type, payload_too_large, request_body_timeout};

pub(crate) use limits::InferencePrincipal;
pub(super) use limits::{
    InferenceReservation, ReleaseReservationBody, estimate_http_json_request_tokens,
};
pub(super) use multipart::{MultipartAdmissionState, validate_multipart_boundary};
pub(crate) use multipart::{MultipartRequestAdmission, MultipartRouteAdmission};
pub(super) use validation::{JsonBodyReadError, read_json_body, validate_json_depth};

const MAX_HEADER_VALUE_BYTES: usize = 8 * 1024;
const MAX_URI_BYTES: usize = 8 * 1024;

#[derive(Clone, Copy)]
pub(crate) struct FirstOwnerSetupAuthorized;

tokio::task_local! {
    /// The immutable generation selected by the inference HTTP boundary. Every
    /// downstream authentication, route, capability, and transport decision
    /// must use this same bundle for the lifetime of the request.
    pub(super) static HTTP_INFERENCE_RUNTIME: Arc<RuntimeBundle>;

    /// The sole verified API-key identity for an admitted inference request.
    /// It contains no plaintext credential and is propagated into detached
    /// request work together with the pinned runtime generation.
    pub(super) static HTTP_INFERENCE_PRINCIPAL: InferencePrincipal;

    /// Set by the canonical pipeline once it owns metadata completion for an
    /// authenticated request. The HTTP boundary emits a content-free fallback
    /// only when decoding or authorization fails before that handoff.
    pub(super) static HTTP_INFERENCE_METADATA_CLAIMED: Arc<AtomicBool>;

    /// Set while an authenticated inference request is executing beneath the
    /// HTTP boundary. Canonical executors use this marker to avoid charging a
    /// second RPM/TPM reservation for the same request.
    pub(super) static HTTP_INFERENCE_LIMITS_RESERVED: i64;

    /// Keeps the HTTP concurrency reservation alive while request work is
    /// transferred to a detached inference task.
    pub(super) static HTTP_INFERENCE_RESERVATION_HOLD: InferenceReservation;
}

pub(crate) fn pin_inference_runtime(state: &GatewayState) -> Arc<RuntimeBundle> {
    HTTP_INFERENCE_PRINCIPAL
        .try_with(|principal| Arc::clone(principal.runtime()))
        .or_else(|_| HTTP_INFERENCE_RUNTIME.try_with(Arc::clone))
        .unwrap_or_else(|_| state.runtime.pin())
}

#[cfg(test)]
pub(crate) fn http_inference_principal() -> Option<InferencePrincipal> {
    HTTP_INFERENCE_PRINCIPAL.try_with(Clone::clone).ok()
}

pub(crate) fn http_inference_reserved_tokens() -> Option<i64> {
    HTTP_INFERENCE_LIMITS_RESERVED
        .try_with(|tokens| *tokens)
        .ok()
}

pub(crate) fn claim_http_inference_metadata() {
    let _ = HTTP_INFERENCE_METADATA_CLAIMED.try_with(|claimed| {
        claimed.store(true, Ordering::Release);
    });
}

#[derive(Clone)]
struct HttpInferenceTaskContext {
    runtime: Arc<RuntimeBundle>,
    principal: Option<InferencePrincipal>,
    metadata_claimed: Option<Arc<AtomicBool>>,
    reserved_tokens: Option<i64>,
    reservation_hold: Option<InferenceReservation>,
}

impl HttpInferenceTaskContext {
    fn capture(state: &GatewayState) -> Self {
        Self {
            runtime: pin_inference_runtime(state),
            principal: HTTP_INFERENCE_PRINCIPAL.try_with(Clone::clone).ok(),
            metadata_claimed: HTTP_INFERENCE_METADATA_CLAIMED.try_with(Arc::clone).ok(),
            reserved_tokens: http_inference_reserved_tokens(),
            reservation_hold: HTTP_INFERENCE_RESERVATION_HOLD
                .try_with(|reservation| reservation.clone())
                .ok(),
        }
    }

    async fn scope<F, T>(self, future: F) -> T
    where
        F: Future<Output = T> + Send + 'static,
        T: Send + 'static,
    {
        let mut future: Pin<Box<dyn Future<Output = T> + Send>> = Box::pin(future);
        if let Some(reservation) = self.reservation_hold {
            future = Box::pin(HTTP_INFERENCE_RESERVATION_HOLD.scope(reservation, future));
        }
        if let Some(reserved_tokens) = self.reserved_tokens {
            future = Box::pin(HTTP_INFERENCE_LIMITS_RESERVED.scope(reserved_tokens, future));
        }
        if let Some(metadata_claimed) = self.metadata_claimed {
            future = Box::pin(HTTP_INFERENCE_METADATA_CLAIMED.scope(metadata_claimed, future));
        }
        if let Some(principal) = self.principal {
            future = Box::pin(HTTP_INFERENCE_PRINCIPAL.scope(principal, future));
        }
        HTTP_INFERENCE_RUNTIME.scope(self.runtime, future).await
    }
}

pub(crate) fn spawn_http_inference_task<F, T>(
    state: &GatewayState,
    future: F,
) -> tokio::task::JoinHandle<T>
where
    F: Future<Output = T> + Send + 'static,
    T: Send + 'static,
{
    let context = HttpInferenceTaskContext::capture(state);
    tokio::spawn(context.scope(future))
}

pub(super) async fn enforce_request_limits(
    State(state): State<GatewayState>,
    request: Request<axum::body::Body>,
    next: middleware::Next,
) -> Response {
    let endpoint = gateway::InferenceEndpoint::classify(request.method(), request.uri().path());
    let surface = endpoint.map(gateway::InferenceEndpoint::surface);
    match enforce_request_limits_inner(&state, request, next, endpoint).await {
        Ok(response) => response,
        Err(RequestLimitRejection::Problem(problem)) => match surface {
            Some(surface) => gateway::problem_response(surface, problem),
            None => problem.into_response(),
        },
        Err(RequestLimitRejection::Inference(error)) => match surface {
            Some(surface) => gateway::inference_error_response(surface, error),
            None => Problem::from(error).into_response(),
        },
    }
}

enum RequestLimitRejection {
    Problem(Problem),
    Inference(gateway::InferenceError),
}

impl From<Problem> for RequestLimitRejection {
    fn from(problem: Problem) -> Self {
        Self::Problem(problem)
    }
}

impl From<gateway::InferenceError> for RequestLimitRejection {
    fn from(error: gateway::InferenceError) -> Self {
        Self::Inference(error)
    }
}

#[derive(Clone)]
pub(super) struct LocalRequestMetadata {
    pub(super) request_metadata: Option<RequestMetadataEmitter>,
    pub(super) request_started_at: chrono::DateTime<chrono::Utc>,
    pub(super) runtime_generation_id: uuid::Uuid,
    pub(super) api_key_id: uuid::Uuid,
    pub(super) route_slug: String,
    pub(super) operation: OperationKind,
    pub(super) surface: Surface,
    pub(super) always_emit: bool,
}

impl LocalRequestMetadata {
    pub(super) fn emit(self, status: axum::http::StatusCode) {
        let Some(request_metadata) = self.request_metadata else {
            return;
        };
        let completed_at = chrono::Utc::now();
        let latency_ms = completed_at
            .signed_duration_since(self.request_started_at)
            .num_milliseconds()
            .max(0)
            .try_into()
            .unwrap_or(u64::MAX);
        let operation = self.operation;
        let event = RequestMetadataEvent {
            event_id: uuid::Uuid::now_v7(),
            request_id: uuid::Uuid::now_v7(),
            runtime_generation_id: self.runtime_generation_id,
            api_key_id: self.api_key_id,
            provider_id: None,
            route_slug: self.route_slug,
            upstream_model: None,
            operation,
            surface: self.surface,
            request_started_at: self.request_started_at,
            request_completed_at: completed_at,
            observed_at: completed_at,
            status_code: Some(status.as_u16()),
            error_class: status
                .is_client_error()
                .then(|| "client_error".to_owned())
                .or_else(|| status.is_server_error().then(|| "server_error".to_owned())),
            committed: false,
            latency_ms,
            first_byte_ms: None,
            input_tokens: None,
            output_tokens: None,
            cached_input_tokens: None,
            media_units: None,
            usage_complete: false,
            unpriced: true,
            attempts: Vec::new(),
        };
        if let Err(error) = request_metadata.emit(event) {
            tracing::warn!(%error, operation = %operation, "local request metadata was not queued");
        }
    }
}

async fn enforce_request_limits_inner(
    state: &GatewayState,
    request: Request<axum::body::Body>,
    next: middleware::Next,
    endpoint: Option<gateway::InferenceEndpoint>,
) -> Result<Response, RequestLimitRejection> {
    let request_started_at = chrono::Utc::now();
    let metadata_policy = endpoint.and_then(gateway::InferenceEndpoint::metadata);
    if request.uri().to_string().len() > MAX_URI_BYTES {
        return Err(Problem::new(
            axum::http::StatusCode::URI_TOO_LONG,
            "uri_too_long",
            "Request URI too long",
            "The request URI exceeds the gateway limit.",
        )
        .into());
    }
    let header_bytes = request
        .headers()
        .iter()
        .fold(0_usize, |size, (name, value)| {
            size.saturating_add(name.as_str().len())
                .saturating_add(value.as_bytes().len())
                .saturating_add(4)
        });
    if request.headers().len() > MAX_HTTP_HEADER_COUNT
        || header_bytes > MAX_HTTP_HEADER_BYTES
        || request
            .headers()
            .values()
            .any(|value| value.as_bytes().len() > MAX_HEADER_VALUE_BYTES)
    {
        return Err(Problem::new(
            axum::http::StatusCode::REQUEST_HEADER_FIELDS_TOO_LARGE,
            "headers_too_large",
            "Request headers too large",
            "Request headers exceed the gateway limit.",
        )
        .into());
    }
    // Public authentication endpoints must reject a malformed forwarding
    // chain before their extractors consume a JSON body.  This keeps the
    // trusted-proxy boundary uniform even for syntactically invalid login or
    // invitation payloads, which otherwise return before source admission.
    if public_auth_source_required(&request) {
        public_auth_source(
            state,
            request.headers(),
            request
                .extensions()
                .get::<axum::extract::ConnectInfo<SocketAddr>>()
                .map(|connect_info| connect_info.0),
        )?;
    }
    let mut request = request;
    if is_first_owner_setup(&request) {
        let authorization = preauthorize_first_owner_setup(state, request.headers()).await?;
        request.extensions_mut().insert(authorization);
    }
    let count = request
        .headers()
        .get_all(axum::http::header::CONTENT_LENGTH)
        .iter()
        .count();
    let transfer_encoding = request
        .headers()
        .get_all(axum::http::header::TRANSFER_ENCODING)
        .iter()
        .collect::<Vec<_>>();
    if count > 1
        || !transfer_encoding.is_empty()
            && request
                .headers()
                .contains_key(axum::http::header::CONTENT_LENGTH)
        || transfer_encoding.len() > 1
        || transfer_encoding.first().is_some_and(|value| {
            !value
                .to_str()
                .is_ok_and(|value| value.trim().eq_ignore_ascii_case("chunked"))
        })
    {
        return Err(Problem::bad_request(
            "ambiguous_body_length",
            "The request has ambiguous framing headers.",
        )
        .into());
    }
    if request
        .headers()
        .get(axum::http::header::CONTENT_ENCODING)
        .is_some_and(|value| {
            !value
                .to_str()
                .is_ok_and(|value| value.trim().eq_ignore_ascii_case("identity"))
        })
    {
        return Err(Problem::new(
            axum::http::StatusCode::UNSUPPORTED_MEDIA_TYPE,
            "content_encoding_unsupported",
            "Content encoding unsupported",
            "Compressed request bodies are not accepted.",
        )
        .into());
    }

    let (maximum, multipart_content_type, is_json) = {
        let content_type = request
            .headers()
            .get(axum::http::header::CONTENT_TYPE)
            .and_then(|value| value.to_str().ok())
            .unwrap_or_default();
        (
            endpoint
                .map(|endpoint| endpoint.body_limit(content_type))
                .unwrap_or(MAX_JSON_BODY_BYTES),
            content_type
                .split(';')
                .next()
                .is_some_and(|value| value.trim().eq_ignore_ascii_case("multipart/form-data"))
                .then(|| content_type.to_owned()),
            is_json_content_type(content_type),
        )
    };
    if request
        .headers()
        .get(axum::http::header::CONTENT_LENGTH)
        .and_then(|value| value.to_str().ok())
        .and_then(|value| value.parse::<u64>().ok())
        .is_some_and(|value| value > maximum as u64)
    {
        return Err(payload_too_large(maximum).into());
    }

    let principal = endpoint
        .map(|endpoint| {
            authenticate_inference_headers(state, request.headers(), endpoint.surface())
        })
        .transpose()?;
    if let Some(principal) = principal.clone() {
        request.extensions_mut().insert(principal);
    }
    let local_metadata = principal.as_ref().and_then(|principal| {
        metadata_policy.map(|metadata| LocalRequestMetadata {
            request_metadata: state.request_metadata.clone(),
            request_started_at,
            runtime_generation_id: principal.runtime().generation.id.as_uuid(),
            api_key_id: principal.key().id.as_uuid(),
            route_slug: metadata.fallback_route.to_owned(),
            operation: metadata.operation,
            surface: principal.surface(),
            always_emit: metadata.always_emit,
        })
    });
    let multipart_policy = endpoint.and_then(gateway::InferenceEndpoint::multipart);
    if multipart_policy.is_some() && multipart_content_type.is_none() {
        if let Some(metadata) = local_metadata {
            metadata.emit(axum::http::StatusCode::BAD_REQUEST);
        }
        return Err(gateway::InferenceError::invalid_request(
            "Content-Type must be multipart/form-data.",
        )
        .into());
    }

    if is_json {
        let (parts, body) = request.into_parts();
        let bytes = match read_json_body(body, MAX_JSON_BODY_BYTES, REQUEST_BODY_TIMEOUT).await {
            Ok(bytes) => bytes,
            Err(JsonBodyReadError::Rejected) => {
                if let Some(metadata) = local_metadata.clone() {
                    metadata.emit(axum::http::StatusCode::PAYLOAD_TOO_LARGE);
                }
                return Err(payload_too_large(MAX_JSON_BODY_BYTES).into());
            }
            Err(JsonBodyReadError::Timeout) => {
                if let Some(metadata) = local_metadata.clone() {
                    metadata.emit(axum::http::StatusCode::REQUEST_TIMEOUT);
                }
                return Err(request_body_timeout().into());
            }
        };
        let local_metadata = local_metadata.map(|mut metadata| {
            if let Some(route) =
                endpoint.and_then(|endpoint| endpoint.route_from_json(parts.uri.path(), &bytes))
            {
                metadata.route_slug = route;
            }
            metadata
        });
        let requested_tokens = estimate_http_json_request_tokens(
            endpoint
                .map(gateway::InferenceEndpoint::token_estimate)
                .unwrap_or(gateway::TokenEstimate::Default),
            &bytes,
        );
        let reservation = if let Some(principal) = &principal {
            match reserve_http_inference_limits(state, principal, requested_tokens).await {
                Ok(reservation) => reservation,
                Err(error) => {
                    if let Some(metadata) = local_metadata.clone() {
                        metadata.emit(error.status());
                    }
                    return Err(error.into());
                }
            }
        } else {
            None
        };
        if let Err(problem) = validate_json_depth(&bytes) {
            release_reservation(reservation).await;
            if let Some(metadata) = local_metadata {
                metadata.emit(axum::http::StatusCode::BAD_REQUEST);
            }
            return Err(problem.into());
        }
        let request = Request::from_parts(parts, Body::from(bytes));
        let reserved_tokens = reservation.as_ref().map(|_| requested_tokens);
        return Ok(run_request_with_reservation(
            request,
            next,
            reservation,
            local_metadata,
            principal,
            reserved_tokens,
        )
        .await);
    }

    let requested_tokens = estimate_http_non_json_request_tokens(
        endpoint
            .map(gateway::InferenceEndpoint::token_estimate)
            .unwrap_or(gateway::TokenEstimate::Default),
    );
    let reservation = if let Some(principal) = &principal {
        match reserve_http_inference_limits(state, principal, requested_tokens).await {
            Ok(reservation) => reservation,
            Err(error) => {
                if let Some(metadata) = local_metadata.clone() {
                    metadata.emit(error.status());
                }
                return Err(error.into());
            }
        }
    } else {
        None
    };
    let multipart_preauthorization = if let Some(content_type) = multipart_content_type {
        if let Err(problem) = validate_multipart_boundary(&content_type) {
            release_reservation(reservation).await;
            if let Some(metadata) = local_metadata.clone() {
                metadata.emit(axum::http::StatusCode::BAD_REQUEST);
            }
            return Err(problem.into());
        }
        match (multipart_policy, principal.as_ref()) {
            (Some((operation, reservation_bytes)), Some(principal)) => {
                match preauthorize_multipart(
                    request.headers(),
                    principal.key(),
                    operation,
                    reservation_bytes,
                ) {
                    Ok(admission) => Some(admission),
                    Err(error) => {
                        release_reservation(reservation).await;
                        if let Some(metadata) = local_metadata.clone() {
                            metadata.emit(error.status());
                        }
                        return Err(error.into());
                    }
                }
            }
            // Only gateway endpoints use multipart today. Keep unrelated
            // control-plane multipart content out of this admission path.
            _ => None,
        }
    } else {
        None
    };
    let multipart_admission = if let Some((route, reservation_bytes)) = multipart_preauthorization {
        let Some(principal) = principal.as_ref() else {
            release_reservation(reservation).await;
            return Err(gateway::InferenceError::unauthorized().into());
        };
        let Some(lease) = state
            .multipart_admission
            .try_admit(principal.key().id.as_uuid(), reservation_bytes)
        else {
            release_reservation(reservation).await;
            if let Some(metadata) = local_metadata.clone() {
                metadata.emit(axum::http::StatusCode::SERVICE_UNAVAILABLE);
            }
            return Err(
                gateway::InferenceError::unavailable("multipart_admission_exhausted").into(),
            );
        };
        Some(MultipartRequestAdmission {
            route,
            lease: Some(lease),
        })
    } else {
        None
    };
    if let Some(admission) = multipart_admission {
        request.extensions_mut().insert(admission);
    }
    let reserved_tokens = reservation.as_ref().map(|_| requested_tokens);
    Ok(run_request_with_reservation(
        request,
        next,
        reservation,
        local_metadata,
        principal,
        reserved_tokens,
    )
    .await)
}

fn public_auth_source_required(request: &Request<Body>) -> bool {
    matches!(
        (request.method(), request.uri().path()),
        (&axum::http::Method::POST, "/api/v1/setup")
            | (&axum::http::Method::POST, "/api/v1/sessions")
            | (&axum::http::Method::POST, "/api/v1/invitations/accept")
            | (&axum::http::Method::GET, "/api/v1/oidc/login")
            | (&axum::http::Method::POST, "/api/v1/oidc/login")
    )
}

fn is_first_owner_setup(request: &Request<Body>) -> bool {
    request.method() == axum::http::Method::POST && request.uri().path() == "/api/v1/setup"
}

async fn preauthorize_first_owner_setup(
    state: &GatewayState,
    headers: &HeaderMap,
) -> Result<FirstOwnerSetupAuthorized, RequestLimitRejection> {
    let store = state.store();
    if !store
        .setup_required()
        .await
        .map_err(management_api::map_persistence)?
    {
        return Err(Problem::conflict(
            "setup_already_completed",
            "This installation already has an owner.",
        )
        .into());
    }
    let supplied_token = headers
        .get(management_api::SETUP_TOKEN_HEADER)
        .and_then(|value| value.to_str().ok());
    match state.verify_bootstrap_token(supplied_token).await {
        Some(true) => {}
        Some(false) => {
            return Err(Problem::unauthorized(
                "A valid setup token is required to create the first owner.",
            )
            .into());
        }
        None => {
            return Err(Problem::service_unavailable("bootstrap_token_not_configured").into());
        }
    }
    management_api::enforce_origin(state, headers)?;
    Ok(FirstOwnerSetupAuthorized)
}

async fn run_request_with_reservation(
    request: Request<Body>,
    next: middleware::Next,
    reservation: Option<InferenceReservation>,
    local_metadata: Option<LocalRequestMetadata>,
    principal: Option<InferencePrincipal>,
    reserved_tokens: Option<i64>,
) -> Response {
    let metadata_claimed = principal.as_ref().map(|_| Arc::new(AtomicBool::new(false)));
    let run = async move {
        // Only suppress the canonical fallback when this exact HTTP request
        // actually acquired a hard-limit reservation. Unlimited keys retain
        // the same pinned generation and therefore remain unlimited throughout
        // this request even if a newer release activates concurrently.
        if let Some(reserved_tokens) = reserved_tokens {
            HTTP_INFERENCE_LIMITS_RESERVED
                .scope(reserved_tokens, next.run(request))
                .await
        } else {
            next.run(request).await
        }
    };
    let run: Pin<Box<dyn Future<Output = Response> + Send>> =
        if let Some(reservation_hold) = reservation.clone() {
            Box::pin(HTTP_INFERENCE_RESERVATION_HOLD.scope(reservation_hold, run))
        } else {
            Box::pin(run)
        };
    let response = match (principal, metadata_claimed.as_ref()) {
        (Some(principal), Some(claimed)) => {
            let runtime = Arc::clone(principal.runtime());
            HTTP_INFERENCE_METADATA_CLAIMED
                .scope(
                    Arc::clone(claimed),
                    HTTP_INFERENCE_PRINCIPAL
                        .scope(principal, HTTP_INFERENCE_RUNTIME.scope(runtime, run)),
                )
                .await
        }
        _ => run.await,
    };
    if let Some(metadata) = local_metadata {
        let claimed = metadata_claimed
            .as_ref()
            .is_some_and(|claimed| claimed.load(Ordering::Acquire));
        if metadata.always_emit || !claimed {
            metadata.emit(response.status());
        }
    }
    if let Some(reservation) = reservation {
        let (parts, body) = response.into_parts();
        Response::from_parts(
            parts,
            Body::new(ReleaseReservationBody {
                inner: body,
                reservation,
            }),
        )
    } else {
        response
    }
}

async fn release_reservation(reservation: Option<InferenceReservation>) {
    if let Some(reservation) = reservation {
        reservation.release().await;
    }
}
