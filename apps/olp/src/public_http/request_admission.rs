//! HTTP request admission, inference reservations, and body safety limits.

use std::{
    net::SocketAddr,
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
use olp_engine::{
    domain::canonical::identity::{OperationKind, Surface},
    inference::request_metadata::{Emitter, Event},
};

use crate::{
    bootstrap::mode_dependencies::RequestBoundaryState,
    gateway::{
        self,
        endpoint_policy::classification::{InferenceEndpoint, TokenEstimate},
    },
    management,
    public_http::problem::Problem,
    public_http::proxy::public_auth_source,
    public_http::public_auth_routes::PublicAuthRoute,
    public_http::router::REQUEST_BODY_TIMEOUT,
};

pub(crate) mod limits;
pub(crate) mod multipart;
pub(crate) mod public;
pub(crate) mod validation;

use limits::{
    ReleaseReservationBody, authenticate_inference_headers, estimate_http_json_request_tokens,
    estimate_http_non_json_request_tokens, reserve_http_inference_limits,
};
use multipart::{MultipartRequestAdmission, preauthorize_multipart, validate_multipart_boundary};
use validation::{
    BodyAdmission, ContentEncoding, JsonBodyReadError, content_encoding_unsupported,
    decompress_gzip_bounded, payload_too_large, read_json_body, request_body_timeout,
    validate_body_framing_and_encoding, validate_json_depth, validate_target_and_headers,
};

use olp_engine::inference::{
    execution::RequestAdmission, limits::Reservation, principal::Principal,
};

#[derive(Clone, Copy)]
pub(crate) struct FirstOwnerSetupAuthorized;

/// Everything an admitted inference request carries into its handler: the
/// verified principal, the metadata-completion claim, and the limits
/// reservation the HTTP boundary took on its behalf. It travels as a request
/// extension, so handlers receive it explicitly rather than through a
/// task-local, and a detached task keeps it alive simply by cloning it. It
/// dereferences to the principal, which is what most handler code reads.
#[derive(Clone)]
pub(crate) struct HttpRequestAdmission {
    principal: Principal,
    metadata_claimed: Arc<AtomicBool>,
    reserved_tokens: Option<i64>,
    reservation_hold: Option<Reservation>,
}

impl HttpRequestAdmission {
    #[cfg(test)]
    pub(crate) fn for_test(
        principal: Principal,
        reservation_hold: Option<Reservation>,
        reserved_tokens: Option<i64>,
    ) -> Self {
        Self {
            principal,
            metadata_claimed: Arc::new(AtomicBool::new(false)),
            reserved_tokens,
            reservation_hold,
        }
    }

    pub(crate) fn principal(&self) -> &Principal {
        &self.principal
    }

    /// The RPM/TPM tokens this exact HTTP request reserved, so canonical
    /// executors do not charge a second reservation for it.
    pub(crate) fn reserved_tokens(&self) -> Option<i64> {
        self.reserved_tokens
    }

    /// The engine-side view: the canonical pipeline takes over metadata
    /// completion and the reservation from here.
    pub(crate) fn engine_admission(&self) -> RequestAdmission {
        RequestAdmission::new(
            self.reservation_hold.clone(),
            self.reserved_tokens,
            Some(Arc::clone(&self.metadata_claimed)),
        )
    }

    #[cfg(test)]
    pub(crate) fn claim_metadata(&self) {
        self.metadata_claimed.store(true, Ordering::Release);
    }

    #[cfg(test)]
    pub(crate) fn metadata_claimed(&self) -> bool {
        self.metadata_claimed.load(Ordering::Acquire)
    }

    #[cfg(test)]
    pub(crate) fn holds_reservation(&self) -> bool {
        self.reservation_hold.is_some()
    }
}

impl std::ops::Deref for HttpRequestAdmission {
    type Target = Principal;

    fn deref(&self) -> &Principal {
        &self.principal
    }
}

pub(crate) async fn enforce_request_limits(
    State(state): State<RequestBoundaryState>,
    request: Request<axum::body::Body>,
    next: middleware::Next,
) -> Response {
    let endpoint = if is_cors_preflight(&request) {
        None
    } else {
        InferenceEndpoint::classify(request.method(), request.uri().path())
    };
    let surface = endpoint.map(InferenceEndpoint::surface);
    match enforce_request_limits_inner(&state, request, next, endpoint).await {
        Ok(response) => response,
        Err(RequestLimitRejection::Problem(problem)) => match surface {
            Some(surface) => gateway::protocol_error::problem_response(surface, problem),
            None => problem.into_response(),
        },
        Err(RequestLimitRejection::Inference(error)) => match surface {
            Some(surface) => gateway::protocol_error::inference_error_response(surface, error),
            None => Problem::from(error).into_response(),
        },
    }
}

/// A browser preflight carries no credentials; the gateway CORS layer answers
/// it (or the protocol fallback returns 404), so it must not be classified as
/// an authenticated inference request.
fn is_cors_preflight(request: &Request<axum::body::Body>) -> bool {
    request.method() == axum::http::Method::OPTIONS
        && request.headers().contains_key(axum::http::header::ORIGIN)
        && request
            .headers()
            .contains_key(axum::http::header::ACCESS_CONTROL_REQUEST_METHOD)
}

enum RequestLimitRejection {
    Problem(Problem),
    Inference(gateway::error::InferenceError),
}

impl From<Problem> for RequestLimitRejection {
    fn from(problem: Problem) -> Self {
        Self::Problem(problem)
    }
}

impl From<gateway::error::InferenceError> for RequestLimitRejection {
    fn from(error: gateway::error::InferenceError) -> Self {
        Self::Inference(error)
    }
}

#[derive(Clone)]
pub(crate) struct LocalRequestMetadata {
    pub(crate) request_metadata: Option<Emitter>,
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
        let event = Event {
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
    endpoint: Option<InferenceEndpoint>,
) -> Result<Response, RequestLimitRejection> {
    let request_started_at = chrono::Utc::now();
    let metadata_policy = endpoint.and_then(InferenceEndpoint::metadata);
    validate_target_and_headers(&request)?;
    enforce_public_auth_source(state, &request)?;
    let mut request = request;
    preauthorize_setup_if_needed(state, &mut request).await?;
    let content_encoding = validate_body_framing_and_encoding(&request)?;
    let limits = state.body_limits;
    let body_admission = BodyAdmission::classify(&request, endpoint, limits);
    if content_encoding == ContentEncoding::Gzip && !body_admission.is_json {
        return Err(content_encoding_unsupported().into());
    }
    body_admission.enforce_declared_size(&request)?;
    let BodyAdmission {
        multipart_content_type,
        is_json,
        ..
    } = body_admission;

    let endpoint_capability = endpoint.and_then(InferenceEndpoint::capability);
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
    let multipart_policy = endpoint.and_then(|endpoint| endpoint.multipart(limits));
    if multipart_policy.is_some() && multipart_content_type.is_none() {
        if let Some(metadata) = local_metadata {
            metadata.emit(axum::http::StatusCode::BAD_REQUEST);
        }
        return Err(gateway::error::InferenceError::invalid_request(
            "Content-Type must be multipart/form-data.",
        )
        .into());
    }

    if is_json {
        let (mut parts, body) = request.into_parts();
        let bytes = match read_json_body(body, limits.json_body_bytes, REQUEST_BODY_TIMEOUT).await {
            Ok(bytes) => bytes,
            Err(JsonBodyReadError::Rejected) => {
                if let Some(metadata) = local_metadata.clone() {
                    metadata.emit(axum::http::StatusCode::PAYLOAD_TOO_LARGE);
                }
                return Err(payload_too_large(limits.json_body_bytes).into());
            }
            Err(JsonBodyReadError::Timeout) => {
                if let Some(metadata) = local_metadata.clone() {
                    metadata.emit(axum::http::StatusCode::REQUEST_TIMEOUT);
                }
                return Err(request_body_timeout().into());
            }
        };
        let bytes = match content_encoding {
            ContentEncoding::Identity => bytes,
            ContentEncoding::Gzip => {
                parts.headers.remove(axum::http::header::CONTENT_ENCODING);
                match decompress_gzip_bounded(&bytes, limits.json_body_bytes) {
                    Ok(bytes) => bytes,
                    Err(problem) => {
                        if let Some(metadata) = local_metadata.clone() {
                            metadata.emit(
                                axum::http::StatusCode::from_u16(problem.status)
                                    .unwrap_or(axum::http::StatusCode::BAD_REQUEST),
                            );
                        }
                        return Err(problem.into());
                    }
                }
            }
        };
        let requested_route =
            endpoint.and_then(|endpoint| endpoint.route_from_json(parts.uri.path(), &bytes));
        let local_metadata = local_metadata.map(|mut metadata| {
            if let Some(route) = requested_route.clone() {
                metadata.route_slug = route;
            }
            metadata
        });
        let requested_route = requested_route
            .as_deref()
            .and_then(|slug| olp_engine::domain::ids::RouteSlug::parse(slug).ok());
        let requested_tokens = estimate_http_json_request_tokens(
            endpoint
                .map(InferenceEndpoint::token_estimate)
                .unwrap_or(TokenEstimate::Default),
            &bytes,
        );
        // Protocol-shaped misses remain authenticated, but capability-free
        // requests must reach the router fallback without requiring a limiter.
        let reservation = if let (Some(principal), Some(_)) = (&principal, endpoint_capability) {
            match reserve_http_inference_limits(
                state,
                principal,
                requested_route.as_ref(),
                requested_tokens,
            )
            .await
            {
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
            .map(InferenceEndpoint::token_estimate)
            .unwrap_or(TokenEstimate::Default),
    );
    let reservation = if let (Some(principal), Some(_)) = (&principal, endpoint_capability) {
        match reserve_http_inference_limits(state, principal, None, requested_tokens).await {
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
            return Err(gateway::error::InferenceError::unauthorized().into());
        };
        let Some(lease) = state
            .multipart_admission
            .try_admit(principal.key().id.as_uuid(), reservation_bytes)
        else {
            finalization
                .finish_rejection(axum::http::StatusCode::SERVICE_UNAVAILABLE)
                .await;
            return Err(gateway::error::InferenceError::unavailable(
                "multipart_admission_exhausted",
            )
            .into());
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
        .map_err(management::error_mapping::map_persistence)?
    {
        return Err(Problem::conflict(
            "setup_already_completed",
            "This installation already has an owner.",
        )
        .into());
    }
    let supplied_token = headers
        .get(management::sessions::SETUP_TOKEN_HEADER)
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
    management::sessions::enforce_origin(&state.public_origin, headers)?;
    Ok(FirstOwnerSetupAuthorized)
}

/// Owns all post-reservation cleanup. Explicit rejection paths await release;
/// successful dispatch transfers release ownership to the response body. If
/// either future is cancelled, `Reservation` retains its Drop
/// fallback and starts the same idempotent release operation.
pub(crate) struct RequestFinalization {
    reservation: Option<Reservation>,
    local_metadata: Option<LocalRequestMetadata>,
    principal: Option<Principal>,
    reserved_tokens: Option<i64>,
}

impl RequestFinalization {
    pub(crate) fn new(
        reservation: Option<Reservation>,
        local_metadata: Option<LocalRequestMetadata>,
        principal: Option<Principal>,
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

    fn principal(&self) -> Option<&Principal> {
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

    async fn dispatch(mut self, mut request: Request<Body>, next: middleware::Next) -> Response {
        // Only an authenticated inference request carries an admission; the
        // fallback metadata below is suppressed once the canonical pipeline
        // claims completion through it. Unlimited keys keep the same pinned
        // generation and therefore remain unlimited throughout this request
        // even if a newer release activates concurrently.
        let admission = self.principal.take().map(|principal| HttpRequestAdmission {
            principal,
            metadata_claimed: Arc::new(AtomicBool::new(false)),
            reserved_tokens: self.reserved_tokens,
            reservation_hold: self.reservation.clone(),
        });
        let metadata_claimed = admission
            .as_ref()
            .map(|admission| Arc::clone(&admission.metadata_claimed));
        if let Some(admission) = admission {
            request.extensions_mut().insert(admission);
        }
        let response = next.run(request).await;
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
