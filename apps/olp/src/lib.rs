//! Axum delivery adapter for management, inference, the static operator
//! console, and the separately bound private observability surface.
//!
//! Top-level modules follow runtime ownership: process bootstrap, public HTTP
//! policy, inference delivery, management delivery, observability, and the
//! embedded console. `apps/olp/AGENTS.md` carries the detailed map.

// HTTP surfaces: inference gateway, management API, operations reads, OIDC,
// playground, embedded console, private observability listener, composition.
mod bootstrap;
mod console;
mod gateway;
mod management;
mod observability;
mod public_http;

pub use bootstrap::cli::run_cli;
#[cfg(test)]
pub(crate) use bootstrap::media_spool::create_bounded_media_spool_for_test;
pub(crate) use bootstrap::media_spool::create_media_spool;
pub(crate) use bootstrap::mode_dependencies::ModeDependencies;
pub(crate) use bootstrap::mode_dependencies::{GatewayState, ManagementState, ObservabilityState};
pub(crate) use bootstrap::state::{
    ApiMode, MAX_HTTP_HEADER_BYTES, MAX_HTTP_HEADER_COUNT, ProcessComposition, TransportRegistry,
};
pub(crate) use gateway::reconcile_media_jobs_once;
pub use management::management_openapi;
pub(crate) use observability::spawn_observability_cache;
#[cfg(test)]
pub(crate) use observability::{observability_router, refresh_observability_cache};
pub(crate) use public_http::problem::{FieldErrors, Problem};
pub(crate) use public_http::proxy::{TrustedProxyCidr, TrustedProxyCidrParseError};
pub(crate) use public_http::public_origin::PublicOrigin;
pub(crate) use public_http::relative_url::RelativeReturnTo;
#[cfg(test)]
pub(crate) use public_http::router::public_router;

pub(crate) use bootstrap::mode_dependencies::RequestBoundaryState;
pub(crate) use bootstrap::state::{MAX_JSON_BODY_BYTES, MAX_MEDIA_BODY_BYTES};
pub(crate) use observability::HealthResponse;
#[cfg(test)]
pub(crate) use public_http::proxy::public_auth_source;
pub(crate) use public_http::proxy::{public_auth_source_digest, public_auth_source_target_digests};
#[cfg(test)]
pub(crate) use public_http::request_admission::HTTP_INFERENCE_LIMITS_RESERVED;
#[cfg(test)]
pub(crate) use public_http::request_admission::claim_http_inference_metadata;
pub(crate) use public_http::request_admission::{
    FirstOwnerSetupAuthorized, InferencePrincipal, MultipartRequestAdmission,
    MultipartRouteAdmission, http_inference_metadata_claim, http_inference_reservation,
    http_inference_reserved_tokens, spawn_http_inference_task,
};

/// Integration-only assembly hooks. Production callers enter through
/// [`run_cli`]; the broad bootstrap composition object is deliberately not
/// part of the normal application API.
#[cfg(feature = "test-util")]
#[doc(hidden)]
pub mod test_support {
    pub use crate::bootstrap::media_spool::create_bounded_media_spool_for_test;
    pub use crate::bootstrap::mode_dependencies::{
        GatewayState, ManagementState, ModeDependencies, ModeDependencyError, ObservabilityState,
    };
    pub use crate::bootstrap::state::{
        ApiMode, MAX_HTTP_HEADER_BYTES, MAX_HTTP_HEADER_COUNT, ProcessComposition,
        TransportRegistry,
    };
    pub use crate::gateway::reconcile_media_jobs_once;
    pub use crate::observability::{observability_router, refresh_observability_cache};
    pub use crate::public_http::proxy::{TrustedProxyCidr, TrustedProxyCidrParseError};
    pub use crate::public_http::public_origin::{PublicOrigin, PublicOriginError};
    pub use crate::public_http::router::{IntoPublicRouter, public_router};
}

#[cfg(test)]
mod tests;
