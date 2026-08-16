pub mod audio;
pub mod chat;
pub mod client;
pub mod embeddings;
use crate::protocols::extensions;
pub mod images;
pub mod media;
pub mod moderation;
pub mod response;
pub mod responses;
pub mod video;

/// One taxonomy for "this OpenAI error is an upstream rate limit" across the
/// unary Responses decoder, the Responses stream decoder, and the Chat
/// Completions decoder, so retryability cannot diverge per surface.
pub(in crate::protocols) fn error_signals_rate_limit(
    code: Option<&str>,
    kind: Option<&str>,
) -> bool {
    code.is_some_and(|code| code.contains("rate_limit"))
        || kind.is_some_and(|kind| kind.contains("rate_limit"))
}
