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
use olp_storage::{
    request_metadata::RequestMetadataEmitter, request_metadata::RequestMetadataEvent,
};

use crate::{
    MAX_JSON_BODY_BYTES, Problem, RequestBoundaryState, gateway, management,
    public_http::proxy::public_auth_source, public_http::public_auth_routes::PublicAuthRoute,
    public_http::router::REQUEST_BODY_TIMEOUT,
};

mod limits;
mod multipart;
mod public;
mod validation;

use limits::{
    authenticate_inference_headers, estimate_http_non_json_request_tokens,
    reserve_http_inference_limits,
};
use multipart::preauthorize_multipart;
use validation::{
    BodyAdmission, payload_too_large, request_body_timeout, validate_body_framing_and_encoding,
    validate_target_and_headers,
};

pub(crate) use limits::{ReleaseReservationBody, estimate_http_json_request_tokens};
pub(crate) use multipart::{MultipartAdmissionState, validate_multipart_boundary};
pub(crate) use multipart::{MultipartRequestAdmission, MultipartRouteAdmission};
pub(crate) use olp_inference::{InferencePrincipal, InferenceReservation};
pub(crate) use public::{
    DEFAULT_MAX_IN_FLIGHT_INFERENCE_REQUESTS, DEFAULT_MAX_IN_FLIGHT_MANAGEMENT_REQUESTS,
    MAX_ADMISSION_CAPACITY, PublicAdmission, PublicAdmissionMiddleware, admit_public_request,
};
pub(crate) use validation::{JsonBodyReadError, read_json_body, validate_json_depth};

#[derive(Clone, Copy)]
pub(crate) struct FirstOwnerSetupAuthorized;

tokio::task_local! {
    /// The sole verified API-key identity for an admitted inference request.
    pub(crate) static HTTP_INFERENCE_PRINCIPAL: InferencePrincipal;

    /// Set by the canonical pipeline once it owns metadata completion for an
    /// authenticated request. The HTTP boundary emits a content-free fallback
    /// only when decoding or authorization fails before that handoff.
    pub(crate) static HTTP_INFERENCE_METADATA_CLAIMED: Arc<AtomicBool>;

    /// Set while an authenticated inference request is executing beneath the
    /// HTTP boundary. Canonical executors use this marker to avoid charging a
    /// second RPM/TPM reservation for the same request.
    pub(crate) static HTTP_INFERENCE_LIMITS_RESERVED: i64;

    /// Keeps the HTTP concurrency reservation alive while request work is
    /// transferred to a detached inference task.
    pub(crate) static HTTP_INFERENCE_RESERVATION_HOLD: InferenceReservation;
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

pub(crate) fn http_inference_reservation() -> Option<InferenceReservation> {
    HTTP_INFERENCE_RESERVATION_HOLD
        .try_with(InferenceReservation::clone)
        .ok()
}

#[cfg(test)]
pub(crate) fn claim_http_inference_metadata() {
    let _ = HTTP_INFERENCE_METADATA_CLAIMED.try_with(|claimed| {
        claimed.store(true, Ordering::Release);
    });
}

pub(crate) fn http_inference_metadata_claim() -> Option<Arc<AtomicBool>> {
    HTTP_INFERENCE_METADATA_CLAIMED.try_with(Arc::clone).ok()
}

#[derive(Clone)]
struct HttpInferenceTaskContext {
    principal: Option<InferencePrincipal>,
    metadata_claimed: Option<Arc<AtomicBool>>,
    reserved_tokens: Option<i64>,
    reservation_hold: Option<InferenceReservation>,
}

impl HttpInferenceTaskContext {
    fn capture() -> Self {
        Self {
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
        future.await
    }
}

pub(crate) fn spawn_http_inference_task<F, T>(future: F) -> tokio::task::JoinHandle<T>
where
    F: Future<Output = T> + Send + 'static,
    T: Send + 'static,
{
    let context = HttpInferenceTaskContext::capture();
    tokio::spawn(context.scope(future))
}

pub(crate) async fn enforce_request_limits(
    State(state): State<RequestBoundaryState>,
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
pub(crate) struct LocalRequestMetadata {
    pub(crate) request_metadata: Option<RequestMetadataEmitter>,
    pub(crate) request_started_at: chrono::DateTime<chrono::Utc>,
    pub(crate) runtime_generation_id: uuid::Uuid,
    pub(crate) api_key_id: uuid::Uuid,
    pub(crate) route_slug: String,
    pub(crate) operation: OperationKind,
    pub(crate) surface: Surface,
    pub(crate) always_emit: bool,
}

impl LocalRequestMetadata {
    pub(crate) fn emit(self, status: axum::http::StatusCode) {
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
    state: &RequestBoundaryState,
    request: Request<axum::body::Body>,
    next: middleware::Next,
    endpoint: Option<gateway::InferenceEndpoint>,
) -> Result<Response, RequestLimitRejection> {
    let request_started_at = chrono::Utc::now();
    let metadata_policy = endpoint.and_then(gateway::InferenceEndpoint::metadata);
    validate_target_and_headers(&request)?;
    enforce_public_auth_source(state, &request)?;
    let mut request = request;
    preauthorize_setup_if_needed(state, &mut request).await?;
    validate_body_framing_and_encoding(&request)?;
    let body_admission = BodyAdmission::classify(&request, endpoint);
    body_admission.enforce_declared_size(&request)?;
    let BodyAdmission {
        multipart_content_type,
        is_json,
        ..
    } = body_admission;

    let endpoint_capability = endpoint.and_then(gateway::InferenceEndpoint::capability);
    let principal = endpoint
        .map(|endpoint| {
            authenticate_inference_headers(
                state,
                request.headers(),
                endpoint.surface(),
                endpoint_capability,
            )
        })
        .transpose()?;
    if let Some(principal) = principal.clone() {
        request.extensions_mut().insert(principal);
    }
    let local_metadata = principal.as_ref().and_then(|principal| {
        metadata_policy.map(|metadata| LocalRequestMetadata {
            request_metadata: state.inference.request_metadata().cloned(),
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
        // Protocol-shaped misses remain authenticated, but capability-free
        // requests must reach the router fallback without requiring a limiter.
        let reservation = if let (Some(principal), Some(_)) = (&principal, endpoint_capability) {
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
        let finalization =
            RequestFinalization::new(reservation, local_metadata, principal, requested_tokens);
        if let Err(problem) = validate_json_depth(&bytes) {
            finalization
                .finish_rejection(axum::http::StatusCode::BAD_REQUEST)
                .await;
            return Err(problem.into());
        }
        let request = Request::from_parts(parts, Body::from(bytes));
        return Ok(finalization.dispatch(request, next).await);
    }

    let requested_tokens = estimate_http_non_json_request_tokens(
        endpoint
            .map(gateway::InferenceEndpoint::token_estimate)
            .unwrap_or(gateway::TokenEstimate::Default),
    );
    let reservation = if let (Some(principal), Some(_)) = (&principal, endpoint_capability) {
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
    let finalization =
        RequestFinalization::new(reservation, local_metadata, principal, requested_tokens);
    let multipart_preauthorization = if let Some(content_type) = multipart_content_type {
        if let Err(problem) = validate_multipart_boundary(&content_type) {
            finalization
                .finish_rejection(axum::http::StatusCode::BAD_REQUEST)
                .await;
            return Err(problem.into());
        }
        match (multipart_policy, finalization.principal()) {
            (Some((capability, reservation_bytes)), Some(principal)) => {
                match preauthorize_multipart(
                    request.headers(),
                    principal.key(),
                    capability,
                    reservation_bytes,
                ) {
                    Ok(admission) => Some(admission),
                    Err(error) => {
                        finalization.finish_rejection(error.status()).await;
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
        let Some(principal) = finalization.principal() else {
            finalization
                .finish_rejection(axum::http::StatusCode::UNAUTHORIZED)
                .await;
            return Err(gateway::InferenceError::unauthorized().into());
        };
        let Some(lease) = state
            .multipart_admission
            .try_admit(principal.key().id.as_uuid(), reservation_bytes)
        else {
            finalization
                .finish_rejection(axum::http::StatusCode::SERVICE_UNAVAILABLE)
                .await;
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
    Ok(finalization.dispatch(request, next).await)
}

fn enforce_public_auth_source(
    state: &RequestBoundaryState,
    request: &Request<Body>,
) -> Result<(), RequestLimitRejection> {
    // This stage deliberately precedes setup authorization and every body
    // extractor so malformed trusted forwarding retains error precedence.
    if public_auth_source_required(request) {
        public_auth_source(
            state,
            request.headers(),
            request
                .extensions()
                .get::<axum::extract::ConnectInfo<SocketAddr>>()
                .map(|connect_info| connect_info.0),
        )?;
    }
    Ok(())
}

async fn preauthorize_setup_if_needed(
    state: &RequestBoundaryState,
    request: &mut Request<Body>,
) -> Result<(), RequestLimitRejection> {
    if is_first_owner_setup(request) {
        let authorization = preauthorize_first_owner_setup(state, request.headers()).await?;
        request.extensions_mut().insert(authorization);
    }
    Ok(())
}

fn public_auth_source_required(request: &Request<Body>) -> bool {
    PublicAuthRoute::classify(request.method(), request.uri().path()).is_some()
}

fn is_first_owner_setup(request: &Request<Body>) -> bool {
    PublicAuthRoute::classify(request.method(), request.uri().path())
        == Some(PublicAuthRoute::FirstOwnerSetup)
}

async fn preauthorize_first_owner_setup(
    state: &RequestBoundaryState,
    headers: &HeaderMap,
) -> Result<FirstOwnerSetupAuthorized, RequestLimitRejection> {
    let store = state.store();
    if !store
        .setup_required()
        .await
        .map_err(management::map_persistence)?
    {
        return Err(Problem::conflict(
            "setup_already_completed",
            "This installation already has an owner.",
        )
        .into());
    }
    let supplied_token = headers
        .get(management::SETUP_TOKEN_HEADER)
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
    management::enforce_origin(&state.public_origin, headers)?;
    Ok(FirstOwnerSetupAuthorized)
}

/// Owns all post-reservation cleanup. Explicit rejection paths await release;
/// successful dispatch transfers release ownership to the response body. If
/// either future is cancelled, `InferenceReservation` retains its Drop
/// fallback and starts the same idempotent release operation.
pub(crate) struct RequestFinalization {
    reservation: Option<InferenceReservation>,
    local_metadata: Option<LocalRequestMetadata>,
    principal: Option<InferencePrincipal>,
    reserved_tokens: Option<i64>,
}

impl RequestFinalization {
    pub(crate) fn new(
        reservation: Option<InferenceReservation>,
        local_metadata: Option<LocalRequestMetadata>,
        principal: Option<InferencePrincipal>,
        requested_tokens: i64,
    ) -> Self {
        let reserved_tokens = reservation.as_ref().map(|_| requested_tokens);
        Self {
            reservation,
            local_metadata,
            principal,
            reserved_tokens,
        }
    }

    fn principal(&self) -> Option<&InferencePrincipal> {
        self.principal.as_ref()
    }

    pub(crate) async fn finish_rejection(mut self, status: axum::http::StatusCode) {
        if let Some(metadata) = self.local_metadata.take() {
            metadata.emit(status);
        }
        if let Some(reservation) = self.reservation.take() {
            reservation.release().await;
        }
    }

    async fn dispatch(mut self, request: Request<Body>, next: middleware::Next) -> Response {
        let metadata_claimed = self
            .principal
            .as_ref()
            .map(|_| Arc::new(AtomicBool::new(false)));
        let reserved_tokens = self.reserved_tokens;
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
            if let Some(reservation_hold) = self.reservation.clone() {
                Box::pin(HTTP_INFERENCE_RESERVATION_HOLD.scope(reservation_hold, run))
            } else {
                Box::pin(run)
            };
        let response = match (self.principal.take(), metadata_claimed.as_ref()) {
            (Some(principal), Some(claimed)) => {
                HTTP_INFERENCE_METADATA_CLAIMED
                    .scope(
                        Arc::clone(claimed),
                        HTTP_INFERENCE_PRINCIPAL.scope(principal, run),
                    )
                    .await
            }
            _ => run.await,
        };
        if let Some(metadata) = self.local_metadata.take() {
            let claimed = metadata_claimed
                .as_ref()
                .is_some_and(|claimed| claimed.load(Ordering::Acquire));
            if metadata.always_emit || !claimed {
                metadata.emit(response.status());
            }
        }
        if let Some(reservation) = self.reservation.take() {
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
}
