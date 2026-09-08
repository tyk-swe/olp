use std::collections::BTreeMap;
use std::collections::BTreeSet;
use std::convert::Infallible;
use std::net::SocketAddr;
use std::num::NonZeroU32;
use std::path::PathBuf;
use std::sync::Arc;
use std::sync::atomic::AtomicBool;
use std::sync::atomic::AtomicUsize;
use std::sync::atomic::Ordering;
use std::time::Duration;
use std::time::Instant;

use crate::crypto::key_material::AuthHmacKey;
use crate::usage::emitter::Emitter;

use crate::access::policy::ApiKey;
use crate::access::policy::ApiKeyDigest;
use crate::access::policy::ApiKeyLimits;
use crate::access::policy::ApiKeyScope;
use crate::access::policy::ApiKeyStatus;
use crate::ids::ApiKeyId;
use crate::ids::ApiKeyLookupId;
use crate::ids::RuntimeGenerationId;
use crate::inference::principal::Principal;
use crate::limits::admission::Reservation;
use crate::protocols::canonical::identity::OperationKind;
use crate::protocols::canonical::identity::Surface;
use crate::runtime::manager::Manager;
use crate::runtime::snapshot::RuntimeGeneration;
use crate::runtime::snapshot::Snapshot;
use axum::Router;
use axum::body::Body;
use axum::extract::Extension;
use axum::extract::State;
use axum::http::HeaderMap;
use axum::http::HeaderName;
use axum::http::HeaderValue;
use axum::http::Request;
use axum::http::Response;
use axum::middleware;
use axum::routing::get;
use base64::Engine as _;
use http_body_util::BodyExt as _;
use tower::ServiceBuilder;
use tower::ServiceExt;
use tower::service_fn;
use tower_http::sensitive_headers::SetSensitiveRequestHeadersLayer;
use tower_http::sensitive_headers::SetSensitiveResponseHeadersLayer;
use tower_http::trace::TraceLayer;
use uuid::Uuid;

use crate::http::body_limits::BodyLimits;
use crate::http::problem::Problem;
use crate::http::proxy::audit_request_provenance;
use crate::http::proxy::public_auth_source;
use crate::http::public_auth_routes::PublicAuthRoute;
use crate::http::request_admission::HttpRequestAdmission;
use crate::http::request_admission::LocalRequestMetadata;
use crate::http::request_admission::RequestFinalization;
use crate::http::request_admission::enforce_request_limits;
use crate::http::request_admission::limits::ReleaseReservationBody;
use crate::http::request_admission::limits::estimate_http_json_request_tokens;
use crate::http::request_admission::multipart::MultipartAdmissionState;
use crate::http::request_admission::multipart::validate_multipart_boundary;
use crate::http::request_admission::validation::JsonBodyReadError;
use crate::http::request_admission::validation::read_json_body;
use crate::http::request_admission::validation::validate_json_depth;
use crate::http::router::gateway_router_for_test;
use crate::http::router::http_request_span;
use crate::http::router::management_router_for_test;
use crate::http::router::request_trace_path;
use crate::http::router::sensitive_request_headers;
use crate::http::router::sensitive_response_headers;
use crate::http::router::validated_public_router;
use crate::inference::http::endpoint_policy::classification::InferenceEndpoint;
use crate::inference::http::endpoint_policy::classification::TokenEstimate;
use crate::inference::http::state::GatewayState;
use crate::observability::cache::OBSERVABILITY_SNAPSHOT_STALE_AFTER;
use crate::observability::cache::refresh_observability_cache;
use crate::observability::metrics::prometheus_label;
use crate::observability::router as observability_router;
use crate::process::mode::ApiMode;
use crate::process::state::ProcessComposition;

fn inference_state(limited: bool) -> (ProcessComposition, String) {
    let auth_hmac_key = Arc::new(AuthHmacKey::new([19; 32]));
    let material = auth_hmac_key.generate_api_key();
    let plaintext = material.expose_once().to_owned();
    let lookup_id = ApiKeyLookupId::parse(material.lookup_id.clone()).unwrap();
    let runtime = Arc::new(Manager::empty());
    runtime
        .install(
            Snapshot {
                generation: RuntimeGeneration {
                    id: RuntimeGenerationId::new(),
                    ordinal: 1,
                    activated_at: chrono::Utc::now(),
                },
                providers: BTreeMap::new(),
                routes: BTreeMap::new(),
                api_keys: BTreeMap::from([(
                    lookup_id.clone(),
                    ApiKey {
                        id: ApiKeyId::new(),
                        lookup_id,
                        digest: ApiKeyDigest::new(material.digest),
                        status: ApiKeyStatus::Active,
                        expires_at: None,
                        scopes: BTreeSet::from([ApiKeyScope::Inference, ApiKeyScope::ModelsRead]),
                        allowed_routes: BTreeSet::new(),
                        limits: ApiKeyLimits {
                            requests_per_minute: limited.then(|| NonZeroU32::new(10).unwrap()),
                            tokens_per_minute: None,
                            concurrency: limited.then(|| NonZeroU32::new(2).unwrap()),
                            daily_cost_limit: None,
                            monthly_cost_limit: None,
                        },
                    },
                )]),
            },
            BTreeMap::new(),
        )
        .unwrap();
    let mut state = ProcessComposition::new(
        ApiMode::Gateway,
        crate::process::mode_dependencies::test_store(),
        runtime,
        "https://olp.example.test",
        PathBuf::from("missing-console"),
    );
    state.auth_hmac_key = auth_hmac_key;
    (state, plaintext)
}

pub mod admission;

pub mod authentication;

pub mod cors;

pub mod observability;

pub mod reservations;
