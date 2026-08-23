use std::{
    collections::BTreeMap,
    convert::Infallible,
    net::SocketAddr,
    num::NonZeroU32,
    path::PathBuf,
    sync::{
        Arc,
        atomic::{AtomicBool, AtomicUsize, Ordering},
    },
    time::{Duration, Instant},
};

use olp_db::security::key_material::AuthHmacKey;
use olp_engine::inference::request_metadata::Emitter;

use axum::{
    Router,
    body::Body,
    extract::{Extension, State},
    http::{HeaderMap, HeaderName, HeaderValue, Request, Response, Uri},
    middleware,
    routing::get,
};
use base64::Engine as _;
use http_body_util::BodyExt as _;
use olp_engine::domain::{
    auth::{ApiKeyDigest, ApiKeyLimits, ApiKeyScope, ApiKeyStatus},
    canonical::identity::{OperationKind, Surface},
    ids::ApiKeyLookupId,
    routing::fixtures,
};
use olp_engine::inference::{limits::Reservation, principal::Principal, runtime::Manager};
use tower::{ServiceBuilder, ServiceExt, service_fn};
use tower_http::{
    sensitive_headers::{SetSensitiveRequestHeadersLayer, SetSensitiveResponseHeadersLayer},
    trace::TraceLayer,
};
use uuid::Uuid;

use super::*;
use super::{
    bootstrap::mode_dependencies::GatewayState,
    bootstrap::state::{ApiMode, MAX_JSON_BODY_BYTES, ProcessComposition},
    gateway::endpoint_policy::classification::{InferenceEndpoint, TokenEstimate},
    observability::{
        cache::{OBSERVABILITY_SNAPSHOT_STALE_AFTER, refresh_observability_cache},
        metrics::prometheus_label,
        router as observability_router,
    },
    public_http::problem::Problem,
    public_http::proxy::public_auth_source,
    public_http::public_auth_routes::PublicAuthRoute,
    public_http::request_admission::{
        HTTP_INFERENCE_LIMITS_RESERVED, HTTP_INFERENCE_METADATA_CLAIMED, HTTP_INFERENCE_PRINCIPAL,
        HTTP_INFERENCE_RESERVATION_HOLD, LocalRequestMetadata, RequestFinalization,
        claim_http_inference_metadata, enforce_request_limits, http_inference_principal,
        http_inference_reserved_tokens,
        limits::{ReleaseReservationBody, estimate_http_json_request_tokens},
        multipart::{MultipartAdmissionState, validate_multipart_boundary},
        spawn_http_inference_task,
        validation::{JsonBodyReadError, read_json_body, validate_json_depth},
    },
    public_http::router::{
        gateway_router_for_test, http_request_span, management_router_for_test, request_trace_path,
        sensitive_request_headers, sensitive_response_headers,
    },
};

mod admission;
mod authentication;
mod observability;
mod reservations;

fn inference_state(limited: bool) -> (ProcessComposition, String) {
    let auth_hmac_key = Arc::new(AuthHmacKey::new([19; 32]));
    let material = auth_hmac_key.generate_api_key();
    let plaintext = material.expose_once().to_owned();
    let lookup_id = ApiKeyLookupId::parse(material.lookup_id.clone()).unwrap();
    let runtime = Arc::new(Manager::empty());
    runtime
        .install(
            {
                let mut api_key = fixtures::api_key(
                    lookup_id,
                    ApiKeyDigest::new(material.digest),
                    [ApiKeyScope::Inference, ApiKeyScope::ModelsRead],
                );
                api_key.limits = ApiKeyLimits {
                    requests_per_minute: limited.then(|| NonZeroU32::new(10).unwrap()),
                    tokens_per_minute: None,
                    concurrency: limited.then(|| NonZeroU32::new(2).unwrap()),
                };
                fixtures::snapshot(1).with_api_key(api_key)
            },
            BTreeMap::new(),
        )
        .unwrap();
    let mut state = ProcessComposition::new(
        ApiMode::Gateway,
        None,
        runtime,
        "https://olp.example.test",
        PathBuf::from("missing-console"),
    );
    state.auth_hmac_key = Some(auth_hmac_key);
    (state, plaintext)
}
