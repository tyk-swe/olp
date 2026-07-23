use axum::Router;

use crate::GatewayState;

mod anthropic;
mod chat;
mod endpoint_policy;
mod error;
mod execution;
mod failover;
mod gemini;
mod limits;
mod media;
mod media_jobs;
mod multipart;
mod native_models;
mod openai_chat_response;
mod openai_http;
mod openai_models;
mod protocol_error;
mod responses;
mod telemetry;
mod videos;

pub(crate) use endpoint_policy::{InferenceEndpoint, TokenEstimate};
pub(crate) use error::InferenceError;
pub(crate) use execution::{
    RoutedEventExecution, RoutedUnaryResult, authorize_model_access,
    execute_event_operation_for_surface, execute_routed_result_for_surface,
    execute_session_generation, release_model_limits, reserve_model_limits,
};
pub(crate) use limits::release_limits;
pub use media_jobs::reconcile_media_jobs_once;
pub(crate) use protocol_error::{inference_error_response, problem_response};
pub(crate) use telemetry::{UsageCapture, emit_event_execution_metadata};

pub fn router() -> Router<GatewayState> {
    endpoint_policy::router()
}

#[cfg(test)]
mod tests;
