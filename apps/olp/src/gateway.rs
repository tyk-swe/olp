use axum::{
    Router,
    extract::DefaultBodyLimit,
    routing::{get, post},
};

use crate::{
    ApiState, IMAGE_VARIATION_BODY_BYTES, MAX_MEDIA_BODY_BYTES, TRANSCRIPTION_BODY_BYTES,
    VIDEO_CREATE_BODY_BYTES,
};

mod anthropic;
mod chat;
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

pub(crate) use error::InferenceError;
pub(crate) use execution::{
    RoutedEventExecution, RoutedUnaryResult, authenticate_model_access,
    execute_event_operation_for_surface, execute_routed_result_for_surface,
    execute_session_generation, release_model_limits, reserve_model_limits,
};
pub(crate) use limits::release_limits;
pub use media_jobs::reconcile_media_jobs_once;
pub(crate) use protocol_error::{inference_error_response, problem_response};
pub(crate) use telemetry::{UsageCapture, emit_event_execution_metadata};

pub fn router() -> Router<ApiState> {
    Router::new()
        .route("/openai/v1/chat/completions", post(chat::chat_completions))
        .route("/openai/v1/responses", post(responses::responses))
        .route(
            "/openai/v1/responses/input_tokens",
            post(responses::response_input_tokens),
        )
        .route("/openai/v1/embeddings", post(media::embeddings))
        .route("/openai/v1/moderations", post(media::moderations))
        .route(
            "/openai/v1/images/generations",
            post(media::image_generations),
        )
        .route(
            "/openai/v1/images/edits",
            post(media::image_edits).layer(DefaultBodyLimit::max(MAX_MEDIA_BODY_BYTES)),
        )
        .route(
            "/openai/v1/images/variations",
            post(media::image_variations).layer(DefaultBodyLimit::max(IMAGE_VARIATION_BODY_BYTES)),
        )
        .route("/openai/v1/audio/speech", post(media::speech))
        .route(
            "/openai/v1/audio/transcriptions",
            post(media::transcriptions).layer(DefaultBodyLimit::max(TRANSCRIPTION_BODY_BYTES)),
        )
        .route(
            "/openai/v1/videos",
            post(videos::video_create)
                .get(videos::video_list)
                .layer(DefaultBodyLimit::max(VIDEO_CREATE_BODY_BYTES)),
        )
        .route(
            "/openai/v1/videos/{video_id}",
            get(videos::video_get).delete(videos::video_delete),
        )
        .route(
            "/openai/v1/videos/{video_id}/content",
            get(videos::video_content),
        )
        .merge(openai_models::router())
        .merge(anthropic::router())
        .merge(gemini::router())
}

#[cfg(test)]
mod tests;
