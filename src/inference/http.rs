use axum::Router;

use crate::inference::http::state::GatewayState;

use crate::inference::http::execution::authorize_model_access;
use crate::inference::http::execution::release_model_limits;
use crate::inference::http::execution::reserve_model_limits;
pub fn router(limits: crate::http::body_limits::BodyLimits) -> Router<GatewayState> {
    endpoint_policy::router::router(limits)
}

pub mod anthropic;

pub mod chat;

pub mod endpoint_policy;

pub mod error;

pub mod execution;

pub mod gemini;

pub mod media;

pub mod media_jobs;

pub mod multipart;

pub mod native_models;

pub mod openai_chat_response;

pub mod openai_http;

pub mod openai_models;

pub mod protocol_error;

pub mod responses;

pub mod state;

#[cfg(test)]
pub mod tests;

pub mod videos;
