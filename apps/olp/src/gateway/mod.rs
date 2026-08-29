use axum::Router;

use crate::bootstrap::mode_dependencies::GatewayState;

mod anthropic;
mod chat;
pub(crate) mod endpoint_policy;
pub(crate) mod error;
mod execution;
mod gemini;
mod media;
#[cfg(feature = "test-util")]
pub mod media_jobs;
#[cfg(not(feature = "test-util"))]
pub(crate) mod media_jobs;
mod multipart;
mod native_models;
mod openai_chat_response;
mod openai_http;
mod openai_models;
pub(crate) mod protocol_error;
mod responses;
mod videos;

use execution::{authorize_model_access, release_model_limits, reserve_model_limits};
pub fn router(limits: crate::bootstrap::state::BodyLimits) -> Router<GatewayState> {
    endpoint_policy::router::router(limits)
}

#[cfg(test)]
mod tests;
