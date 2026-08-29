//! Shared public delivery boundary: listener hardening, routing, admission,
//! request parsing, and protocol-independent response primitives.

pub(crate) mod cors;
pub(crate) mod image_response;
pub(crate) mod json_media;
pub(crate) mod listener;
pub(crate) mod problem;
pub(crate) mod proxy;
pub(crate) mod public_auth_routes;
pub(crate) mod public_origin;
pub(crate) mod relative_url;
pub(crate) mod request_admission;
pub(crate) mod request_cookies;
#[cfg(feature = "test-util")]
pub mod router;
#[cfg(not(feature = "test-util"))]
pub(crate) mod router;
pub(crate) mod streaming_response;
