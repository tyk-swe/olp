//! Axum delivery adapter for management, inference, the static operator
//! console, and the separately bound private observability surface.
//!
//! The modules below are flat but layered; `apps/olp/AGENTS.md` carries the
//! full map and disambiguates same-named modules across crates.

// Transport: listener, proxy trust, admission, cookies.
mod listener;
mod proxy;
mod request_admission;
mod request_cookies;

// HTTP surfaces: inference gateway, management API, operations reads, OIDC,
// playground, embedded console, private observability listener, composition.
mod gateway;
mod management_api;
mod observability;
mod oidc;
mod operations;
mod playground;
mod router;
mod static_console;

// Infrastructure: breakers, connector credentials, media spool, runtime
// publication, per-mode dependency wiring, provider adaptation, CLI.
mod circuit;
mod cli;
mod connectors;
mod media_job_journal;
mod media_spool;
mod mode_dependencies;
mod provider_adapter;
mod runtime;
mod state;

// Shared utilities.
mod event_completion;
mod image_response;
mod json_media;
mod problem;
mod public_origin;
mod relative_url;
mod semantic_validation;
mod streaming_response;

pub use cli::run_cli;
pub use gateway::reconcile_media_jobs_once;
pub use management_api::management_openapi;
#[cfg(any(test, feature = "test-util"))]
pub use media_spool::create_bounded_media_spool_for_test;
pub use media_spool::create_media_spool;
pub use mode_dependencies::{GatewayState, ManagementState, ObservabilityState};
pub use mode_dependencies::{ModeDependencies, ModeDependencyError};
pub use observability::{
    observability_router, refresh_observability_cache, spawn_observability_cache,
};
pub use olp_providers::{
    CredentialKind, OpenAiConnector, ProviderConfig, ProviderCredential, ProviderError,
    ProviderFactory,
};
pub use problem::{FieldErrors, Problem};
pub use proxy::{TrustedProxyCidr, TrustedProxyCidrParseError, public_auth_source};
pub use public_origin::{PublicOrigin, PublicOriginError};
pub use relative_url::{RelativeReturnTo, RelativeReturnToError};
pub use router::{IntoPublicRouter, public_router};
pub use runtime::{RuntimeBundle, RuntimeInstallError, RuntimeManager};
pub use state::{
    ApiMode, ApiState, MAX_HTTP_HEADER_BYTES, MAX_HTTP_HEADER_COUNT, ReloadableLimiter,
    TransportRegistry,
};

pub(crate) use observability::HealthResponse;
pub(crate) use proxy::{public_auth_source_digest, public_auth_source_target_digests};
#[cfg(test)]
pub(crate) use request_admission::HTTP_INFERENCE_LIMITS_RESERVED;
pub(crate) use request_admission::{
    FirstOwnerSetupAuthorized, InferencePrincipal, MultipartRequestAdmission,
    MultipartRouteAdmission, claim_http_inference_metadata, http_inference_reservation,
    http_inference_reserved_tokens, spawn_http_inference_task,
};
pub(crate) use state::{MAX_JSON_BODY_BYTES, MAX_MEDIA_BODY_BYTES};

#[cfg(test)]
mod tests;
