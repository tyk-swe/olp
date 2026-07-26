mod audio;
mod chat;
mod client;
mod embeddings;
mod extensions;
mod images;
mod media;
mod models;
mod moderation;
mod response;
mod responses;
mod video;

pub use audio::*;
pub use chat::*;
pub use client::*;
pub use embeddings::*;
pub use images::*;
pub use media::*;
pub use models::*;
pub use moderation::*;
pub use response::*;
pub use responses::*;
pub use video::*;

/// One taxonomy for "this OpenAI error is an upstream rate limit" across the
/// unary Responses decoder, the Responses stream decoder, and the Chat
/// Completions decoder, so retryability cannot diverge per surface.
pub(crate) fn error_signals_rate_limit(code: Option<&str>, kind: Option<&str>) -> bool {
    code.is_some_and(|code| code.contains("rate_limit"))
        || kind.is_some_and(|kind| kind.contains("rate_limit"))
}
