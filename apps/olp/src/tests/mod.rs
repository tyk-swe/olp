use std::{
    collections::{BTreeMap, BTreeSet},
    convert::Infallible,
    net::SocketAddr,
    num::NonZeroU32,
    path::PathBuf,
    sync::{
        Arc,
        atomic::{AtomicBool, Ordering},
    },
    time::{Duration, Instant},
};

use olp_storage::{AuthHmacKey, RequestMetadataEmitter};

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
use olp_domain::{
    ApiKey, ApiKeyDigest, ApiKeyId, ApiKeyLimits, ApiKeyLookupId, ApiKeyScope, ApiKeyStatus,
    OperationKind, RuntimeGeneration, RuntimeGenerationId, RuntimeSnapshot, Surface,
};
use tower::{ServiceBuilder, ServiceExt, service_fn};
use tower_http::{
    sensitive_headers::{SetSensitiveRequestHeadersLayer, SetSensitiveResponseHeadersLayer},
    trace::TraceLayer,
};
use uuid::Uuid;

use super::*;
use super::{
    gateway::{InferenceEndpoint, TokenEstimate},
    observability::{OBSERVABILITY_SNAPSHOT_STALE_AFTER, prometheus_label},
    request_admission::{
        HTTP_INFERENCE_LIMITS_RESERVED, HTTP_INFERENCE_METADATA_CLAIMED, HTTP_INFERENCE_PRINCIPAL,
        HTTP_INFERENCE_RESERVATION_HOLD, InferenceReservation, JsonBodyReadError,
        LocalRequestMetadata, MultipartAdmissionState, ReleaseReservationBody,
        enforce_request_limits, estimate_http_json_request_tokens, http_inference_principal,
        read_json_body, validate_json_depth, validate_multipart_boundary,
        validate_singleton_headers,
    },
    router::{
        http_request_span, request_trace_path, sensitive_request_headers,
        sensitive_response_headers,
    },
};

mod admission;
mod authentication;
mod observability;
mod reservations;

fn inference_state(limited: bool) -> (ApiState, String) {
    let auth_hmac_key = Arc::new(AuthHmacKey::new([19; 32]));
    let material = auth_hmac_key.generate_api_key();
    let plaintext = material.expose_once().to_owned();
    let lookup_id = ApiKeyLookupId::parse(material.lookup_id.clone()).unwrap();
    let runtime = Arc::new(RuntimeManager::empty());
    runtime
        .install(
            RuntimeSnapshot {
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
                        },
                    },
                )]),
            },
            BTreeMap::new(),
        )
        .unwrap();
    let mut state = ApiState::new(
        ApiMode::Gateway,
        None,
        runtime,
        "https://olp.example.test",
        PathBuf::from("missing-console"),
    );
    state.auth_hmac_key = Some(auth_hmac_key);
    (state, plaintext)
}
