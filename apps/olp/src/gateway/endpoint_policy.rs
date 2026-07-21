use axum::http::Method;
use olp_domain::{OperationKind, RouteSlug, Surface};

use crate::{MAX_JSON_BODY_BYTES, MAX_MEDIA_BODY_BYTES};

pub(crate) const IMAGE_VARIATION_BODY_BYTES: usize = 55 * 1024 * 1024;
pub(crate) const TRANSCRIPTION_BODY_BYTES: usize = 30 * 1024 * 1024;
pub(crate) const VIDEO_CREATE_BODY_BYTES: usize = 25 * 1024 * 1024;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum TokenEstimate {
    Default,
    Generation,
    Embeddings,
    Transcription,
    Media,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) struct MetadataPolicy {
    pub(crate) operation: &'static str,
    pub(crate) fallback_route: &'static str,
    pub(crate) always_emit: bool,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum BodyAdmission {
    Standard,
    Media,
    Multipart {
        operation: OperationKind,
        reservation_bytes: u64,
    },
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum InferenceEndpoint {
    OpenAiChatCompletions,
    OpenAiResponses,
    OpenAiResponseInputTokens,
    OpenAiEmbeddings,
    OpenAiModerations,
    OpenAiImageGenerations,
    OpenAiImageEdits,
    OpenAiImageVariations,
    OpenAiSpeech,
    OpenAiTranscriptions,
    OpenAiVideoCreate,
    OpenAiVideoList,
    OpenAiVideoGet,
    OpenAiVideoDelete,
    OpenAiVideoContent,
    OpenAiModelList,
    OpenAiModelGet,
    AnthropicMessages,
    AnthropicCountTokens,
    AnthropicModelList,
    AnthropicModelGet,
    GeminiModelList,
    GeminiModelGet,
    GeminiGenerateContent,
    GeminiStreamGenerateContent,
    GeminiCountTokens,
    GeminiUnknownAction,
    Unknown {
        surface: Surface,
        media_body: bool,
        metadata: Option<MetadataPolicy>,
        token_estimate: TokenEstimate,
        route_from_path: bool,
    },
}

impl InferenceEndpoint {
    pub(crate) fn classify(method: &Method, path: &str) -> Option<Self> {
        let endpoint = match (method, path) {
            (&Method::POST, "/openai/v1/chat/completions") => Self::OpenAiChatCompletions,
            (&Method::POST, "/openai/v1/responses") => Self::OpenAiResponses,
            (&Method::POST, "/openai/v1/responses/input_tokens") => Self::OpenAiResponseInputTokens,
            (&Method::POST, "/openai/v1/embeddings") => Self::OpenAiEmbeddings,
            (&Method::POST, "/openai/v1/moderations") => Self::OpenAiModerations,
            (&Method::POST, "/openai/v1/images/generations") => Self::OpenAiImageGenerations,
            (&Method::POST, "/openai/v1/images/edits") => Self::OpenAiImageEdits,
            (&Method::POST, "/openai/v1/images/variations") => Self::OpenAiImageVariations,
            (&Method::POST, "/openai/v1/audio/speech") => Self::OpenAiSpeech,
            (&Method::POST, "/openai/v1/audio/transcriptions") => Self::OpenAiTranscriptions,
            (&Method::POST, "/openai/v1/videos") => Self::OpenAiVideoCreate,
            (&Method::GET, "/openai/v1/videos") => Self::OpenAiVideoList,
            (&Method::GET, "/openai/v1/models") => Self::OpenAiModelList,
            (&Method::GET, "/anthropic/v1/models") => Self::AnthropicModelList,
            (&Method::POST, "/anthropic/v1/messages") => Self::AnthropicMessages,
            (&Method::POST, "/anthropic/v1/messages/count_tokens") => Self::AnthropicCountTokens,
            _ if method == Method::GET
                && single_segment(path, "/openai/v1/videos/", Some("/content")) =>
            {
                Self::OpenAiVideoContent
            }
            _ if method == Method::GET && single_segment(path, "/openai/v1/videos/", None) => {
                Self::OpenAiVideoGet
            }
            _ if method == Method::DELETE && single_segment(path, "/openai/v1/videos/", None) => {
                Self::OpenAiVideoDelete
            }
            _ if method == Method::GET && single_segment(path, "/openai/v1/models/", None) => {
                Self::OpenAiModelGet
            }
            _ if method == Method::GET && single_segment(path, "/anthropic/v1/models/", None) => {
                Self::AnthropicModelGet
            }
            _ if let Some(resource) = gemini_model_resource(path) => match *method {
                Method::GET => Self::GeminiModelGet,
                Method::POST if resource.ends_with(":generateContent") => {
                    Self::GeminiGenerateContent
                }
                Method::POST if resource.ends_with(":streamGenerateContent") => {
                    Self::GeminiStreamGenerateContent
                }
                Method::POST if resource.ends_with(":countTokens") => Self::GeminiCountTokens,
                Method::POST => Self::GeminiUnknownAction,
                _ => return Self::unknown(method, path),
            },
            _ if method == Method::GET
                && matches!(path, "/gemini/v1/models" | "/gemini/v1beta/models") =>
            {
                Self::GeminiModelList
            }
            _ => return Self::unknown(method, path),
        };
        Some(endpoint)
    }

    fn unknown(method: &Method, path: &str) -> Option<Self> {
        let surface = surface_from_path(path)?;
        let metadata = legacy_metadata(method, path);
        Some(Self::Unknown {
            surface,
            media_body: path.starts_with("/openai/v1/images/")
                || path.starts_with("/openai/v1/audio/")
                || path == "/openai/v1/videos",
            metadata,
            token_estimate: token_estimate_from_path(path),
            route_from_path: metadata.is_some() && path.contains("/models/"),
        })
    }

    pub(crate) const fn surface(self) -> Surface {
        match self {
            Self::AnthropicMessages
            | Self::AnthropicCountTokens
            | Self::AnthropicModelList
            | Self::AnthropicModelGet => Surface::Anthropic,
            Self::GeminiModelList
            | Self::GeminiModelGet
            | Self::GeminiGenerateContent
            | Self::GeminiStreamGenerateContent
            | Self::GeminiCountTokens
            | Self::GeminiUnknownAction => Surface::Gemini,
            Self::Unknown { surface, .. } => surface,
            _ => Surface::OpenAi,
        }
    }

    pub(crate) const fn metadata(self) -> Option<MetadataPolicy> {
        let (operation, fallback_route, always_emit) = match self {
            Self::OpenAiModelList | Self::AnthropicModelList | Self::GeminiModelList => {
                ("model_list", "models", true)
            }
            Self::OpenAiModelGet | Self::AnthropicModelGet | Self::GeminiModelGet => {
                ("model_get", "models", true)
            }
            Self::OpenAiVideoList => ("video_list", "videos", true),
            Self::OpenAiVideoCreate => ("video_create", "invalid-request", false),
            Self::OpenAiVideoGet => ("video_get", "invalid-request", false),
            Self::OpenAiVideoDelete => ("video_delete", "invalid-request", false),
            Self::OpenAiVideoContent => ("video_content", "invalid-request", false),
            Self::OpenAiResponseInputTokens
            | Self::AnthropicCountTokens
            | Self::GeminiCountTokens => ("token_count", "invalid-request", false),
            Self::OpenAiEmbeddings => ("embeddings", "invalid-request", false),
            Self::OpenAiImageGenerations => ("image_generation", "invalid-request", false),
            Self::OpenAiImageEdits => ("image_edit", "invalid-request", false),
            Self::OpenAiImageVariations => ("image_variation", "invalid-request", false),
            Self::OpenAiSpeech => ("speech", "invalid-request", false),
            Self::OpenAiTranscriptions => ("transcription", "invalid-request", false),
            Self::OpenAiModerations => ("moderation", "invalid-request", false),
            Self::OpenAiChatCompletions
            | Self::OpenAiResponses
            | Self::AnthropicMessages
            | Self::GeminiGenerateContent
            | Self::GeminiStreamGenerateContent => ("generation", "invalid-request", false),
            Self::GeminiUnknownAction => return None,
            Self::Unknown { metadata, .. } => return metadata,
        };
        Some(MetadataPolicy {
            operation,
            fallback_route,
            always_emit,
        })
    }

    const fn body_admission(self) -> BodyAdmission {
        match self {
            Self::OpenAiImageEdits => BodyAdmission::Multipart {
                operation: OperationKind::ImageEdit,
                reservation_bytes: MAX_MEDIA_BODY_BYTES as u64,
            },
            Self::OpenAiImageVariations => BodyAdmission::Multipart {
                operation: OperationKind::ImageVariation,
                reservation_bytes: IMAGE_VARIATION_BODY_BYTES as u64,
            },
            Self::OpenAiTranscriptions => BodyAdmission::Multipart {
                operation: OperationKind::Transcription,
                reservation_bytes: TRANSCRIPTION_BODY_BYTES as u64,
            },
            Self::OpenAiVideoCreate => BodyAdmission::Multipart {
                operation: OperationKind::VideoCreate,
                reservation_bytes: VIDEO_CREATE_BODY_BYTES as u64,
            },
            Self::OpenAiImageGenerations | Self::OpenAiSpeech | Self::OpenAiVideoList => {
                BodyAdmission::Media
            }
            Self::Unknown {
                media_body: true, ..
            } => BodyAdmission::Media,
            _ => BodyAdmission::Standard,
        }
    }

    pub(crate) fn body_limit(self, content_type: &str) -> usize {
        if matches!(
            self.body_admission(),
            BodyAdmission::Media | BodyAdmission::Multipart { .. }
        ) && is_media_content_type(content_type)
        {
            MAX_MEDIA_BODY_BYTES
        } else {
            MAX_JSON_BODY_BYTES
        }
    }

    pub(crate) const fn multipart(self) -> Option<(OperationKind, u64)> {
        match self.body_admission() {
            BodyAdmission::Multipart {
                operation,
                reservation_bytes,
            } => Some((operation, reservation_bytes)),
            _ => None,
        }
    }

    pub(crate) const fn token_estimate(self) -> TokenEstimate {
        match self {
            Self::OpenAiChatCompletions
            | Self::OpenAiResponses
            | Self::AnthropicMessages
            | Self::GeminiGenerateContent
            | Self::GeminiStreamGenerateContent => TokenEstimate::Generation,
            Self::OpenAiEmbeddings => TokenEstimate::Embeddings,
            Self::OpenAiTranscriptions => TokenEstimate::Transcription,
            Self::OpenAiImageEdits
            | Self::OpenAiImageVariations
            | Self::OpenAiVideoCreate
            | Self::OpenAiVideoList => TokenEstimate::Media,
            Self::Unknown { token_estimate, .. } => token_estimate,
            _ => TokenEstimate::Default,
        }
    }

    pub(crate) fn route_from_json(self, path: &str, body: &[u8]) -> Option<String> {
        if let Ok(value) = serde_json::from_slice::<serde_json::Value>(body)
            && let Some(model) = value.get("model").and_then(serde_json::Value::as_str)
            && RouteSlug::parse(model).is_ok()
        {
            return Some(model.to_owned());
        }
        let route_from_path = matches!(
            self,
            Self::OpenAiModelGet
                | Self::AnthropicModelGet
                | Self::GeminiModelGet
                | Self::GeminiGenerateContent
                | Self::GeminiStreamGenerateContent
                | Self::GeminiCountTokens
                | Self::GeminiUnknownAction
                | Self::Unknown {
                    route_from_path: true,
                    ..
                }
        );
        if !route_from_path {
            return None;
        }
        let resource = path.split("/models/").nth(1)?;
        let model = resource.split(':').next()?;
        RouteSlug::parse(model).is_ok().then(|| model.to_owned())
    }
}

fn surface_from_path(path: &str) -> Option<Surface> {
    if path.starts_with("/openai/") {
        Some(Surface::OpenAi)
    } else if path.starts_with("/anthropic/") {
        Some(Surface::Anthropic)
    } else if path.starts_with("/gemini/") {
        Some(Surface::Gemini)
    } else {
        None
    }
}

fn single_segment(path: &str, prefix: &str, suffix: Option<&str>) -> bool {
    let Some(resource) = path.strip_prefix(prefix) else {
        return false;
    };
    let resource = match suffix {
        Some(suffix) => match resource.strip_suffix(suffix) {
            Some(resource) => resource,
            None => return false,
        },
        None => resource,
    };
    !resource.is_empty() && !resource.contains('/')
}

fn gemini_model_resource(path: &str) -> Option<&str> {
    path.strip_prefix("/gemini/v1/models/")
        .or_else(|| path.strip_prefix("/gemini/v1beta/models/"))
        .filter(|resource| !resource.is_empty())
}

fn is_media_content_type(content_type: &str) -> bool {
    content_type
        .split(';')
        .next()
        .is_some_and(|value| value.trim().eq_ignore_ascii_case("multipart/form-data"))
        || content_type.eq_ignore_ascii_case("application/octet-stream")
}

fn token_estimate_from_path(path: &str) -> TokenEstimate {
    if path.ends_with("/chat/completions")
        || path.ends_with("/responses")
        || path.ends_with("/messages")
        || path.ends_with(":generateContent")
        || path.ends_with(":streamGenerateContent")
    {
        TokenEstimate::Generation
    } else if path.ends_with("/embeddings") {
        TokenEstimate::Embeddings
    } else if path.ends_with("/audio/transcriptions") {
        TokenEstimate::Transcription
    } else if path.ends_with("/images/edits")
        || path.ends_with("/images/variations")
        || path.ends_with("/videos")
    {
        TokenEstimate::Media
    } else {
        TokenEstimate::Default
    }
}

fn legacy_metadata(method: &Method, path: &str) -> Option<MetadataPolicy> {
    let (operation, fallback_route, always_emit) = if method == Method::GET
        && matches!(
            path,
            "/openai/v1/models"
                | "/anthropic/v1/models"
                | "/gemini/v1/models"
                | "/gemini/v1beta/models"
        ) {
        ("model_list", "models", true)
    } else if method == Method::GET
        && (path.starts_with("/openai/v1/models/")
            || path.starts_with("/anthropic/v1/models/")
            || path.starts_with("/gemini/v1/models/")
            || path.starts_with("/gemini/v1beta/models/"))
    {
        ("model_get", "models", true)
    } else if path == "/openai/v1/videos" && method == Method::GET {
        ("video_list", "videos", true)
    } else if path == "/openai/v1/videos" && method == Method::POST {
        ("video_create", "invalid-request", false)
    } else if path.starts_with("/openai/v1/videos/") && path.ends_with("/content") {
        ("video_content", "invalid-request", false)
    } else if path.starts_with("/openai/v1/videos/") && method == Method::DELETE {
        ("video_delete", "invalid-request", false)
    } else if path.starts_with("/openai/v1/videos/") && method == Method::GET {
        ("video_get", "invalid-request", false)
    } else if path == "/openai/v1/responses/input_tokens"
        || path == "/anthropic/v1/messages/count_tokens"
        || path.ends_with(":countTokens")
    {
        ("token_count", "invalid-request", false)
    } else if path == "/openai/v1/embeddings" {
        ("embeddings", "invalid-request", false)
    } else if path == "/openai/v1/images/generations" {
        ("image_generation", "invalid-request", false)
    } else if path == "/openai/v1/images/edits" {
        ("image_edit", "invalid-request", false)
    } else if path == "/openai/v1/images/variations" {
        ("image_variation", "invalid-request", false)
    } else if path == "/openai/v1/audio/speech" {
        ("speech", "invalid-request", false)
    } else if path == "/openai/v1/audio/transcriptions" {
        ("transcription", "invalid-request", false)
    } else if path == "/openai/v1/moderations" {
        ("moderation", "invalid-request", false)
    } else if path == "/openai/v1/chat/completions"
        || path == "/openai/v1/responses"
        || path == "/anthropic/v1/messages"
        || path.ends_with(":generateContent")
        || path.ends_with(":streamGenerateContent")
    {
        ("generation", "invalid-request", false)
    } else {
        return None;
    };
    Some(MetadataPolicy {
        operation,
        fallback_route,
        always_emit,
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    fn assert_registered_facts(endpoint: InferenceEndpoint, path: &str) {
        let expected_surface = match endpoint {
            InferenceEndpoint::AnthropicMessages
            | InferenceEndpoint::AnthropicCountTokens
            | InferenceEndpoint::AnthropicModelList
            | InferenceEndpoint::AnthropicModelGet => Surface::Anthropic,
            InferenceEndpoint::GeminiModelList
            | InferenceEndpoint::GeminiModelGet
            | InferenceEndpoint::GeminiGenerateContent
            | InferenceEndpoint::GeminiStreamGenerateContent
            | InferenceEndpoint::GeminiCountTokens
            | InferenceEndpoint::GeminiUnknownAction => Surface::Gemini,
            InferenceEndpoint::Unknown { .. } => panic!("registered matrix contains an unknown"),
            _ => Surface::OpenAi,
        };
        assert_eq!(endpoint.surface(), expected_surface, "surface for {path}");

        let expected_metadata = match endpoint {
            InferenceEndpoint::OpenAiModelList
            | InferenceEndpoint::AnthropicModelList
            | InferenceEndpoint::GeminiModelList => Some(("model_list", "models", true)),
            InferenceEndpoint::OpenAiModelGet
            | InferenceEndpoint::AnthropicModelGet
            | InferenceEndpoint::GeminiModelGet => Some(("model_get", "models", true)),
            InferenceEndpoint::OpenAiVideoList => Some(("video_list", "videos", true)),
            InferenceEndpoint::OpenAiVideoCreate => {
                Some(("video_create", "invalid-request", false))
            }
            InferenceEndpoint::OpenAiVideoGet => Some(("video_get", "invalid-request", false)),
            InferenceEndpoint::OpenAiVideoDelete => {
                Some(("video_delete", "invalid-request", false))
            }
            InferenceEndpoint::OpenAiVideoContent => {
                Some(("video_content", "invalid-request", false))
            }
            InferenceEndpoint::OpenAiResponseInputTokens
            | InferenceEndpoint::AnthropicCountTokens
            | InferenceEndpoint::GeminiCountTokens => {
                Some(("token_count", "invalid-request", false))
            }
            InferenceEndpoint::OpenAiEmbeddings => Some(("embeddings", "invalid-request", false)),
            InferenceEndpoint::OpenAiImageGenerations => {
                Some(("image_generation", "invalid-request", false))
            }
            InferenceEndpoint::OpenAiImageEdits => Some(("image_edit", "invalid-request", false)),
            InferenceEndpoint::OpenAiImageVariations => {
                Some(("image_variation", "invalid-request", false))
            }
            InferenceEndpoint::OpenAiSpeech => Some(("speech", "invalid-request", false)),
            InferenceEndpoint::OpenAiTranscriptions => {
                Some(("transcription", "invalid-request", false))
            }
            InferenceEndpoint::OpenAiModerations => Some(("moderation", "invalid-request", false)),
            InferenceEndpoint::OpenAiChatCompletions
            | InferenceEndpoint::OpenAiResponses
            | InferenceEndpoint::AnthropicMessages
            | InferenceEndpoint::GeminiGenerateContent
            | InferenceEndpoint::GeminiStreamGenerateContent => {
                Some(("generation", "invalid-request", false))
            }
            InferenceEndpoint::GeminiUnknownAction => None,
            InferenceEndpoint::Unknown { .. } => unreachable!(),
        };
        assert_eq!(
            endpoint.metadata().map(|policy| (
                policy.operation,
                policy.fallback_route,
                policy.always_emit
            )),
            expected_metadata,
            "metadata for {path}"
        );

        let expected_multipart = match endpoint {
            InferenceEndpoint::OpenAiImageEdits => {
                Some((OperationKind::ImageEdit, MAX_MEDIA_BODY_BYTES as u64))
            }
            InferenceEndpoint::OpenAiImageVariations => Some((
                OperationKind::ImageVariation,
                IMAGE_VARIATION_BODY_BYTES as u64,
            )),
            InferenceEndpoint::OpenAiTranscriptions => Some((
                OperationKind::Transcription,
                TRANSCRIPTION_BODY_BYTES as u64,
            )),
            InferenceEndpoint::OpenAiVideoCreate => {
                Some((OperationKind::VideoCreate, VIDEO_CREATE_BODY_BYTES as u64))
            }
            _ => None,
        };
        assert_eq!(
            endpoint.multipart(),
            expected_multipart,
            "multipart for {path}"
        );
        let uses_media_ceiling = matches!(
            endpoint,
            InferenceEndpoint::OpenAiImageGenerations
                | InferenceEndpoint::OpenAiImageEdits
                | InferenceEndpoint::OpenAiImageVariations
                | InferenceEndpoint::OpenAiSpeech
                | InferenceEndpoint::OpenAiTranscriptions
                | InferenceEndpoint::OpenAiVideoCreate
                | InferenceEndpoint::OpenAiVideoList
        );
        assert_eq!(
            endpoint.body_limit("multipart/form-data; boundary=matrix"),
            if uses_media_ceiling {
                MAX_MEDIA_BODY_BYTES
            } else {
                MAX_JSON_BODY_BYTES
            },
            "body class for {path}"
        );

        let expected_tokens = match endpoint {
            InferenceEndpoint::OpenAiChatCompletions
            | InferenceEndpoint::OpenAiResponses
            | InferenceEndpoint::AnthropicMessages
            | InferenceEndpoint::GeminiGenerateContent
            | InferenceEndpoint::GeminiStreamGenerateContent => TokenEstimate::Generation,
            InferenceEndpoint::OpenAiEmbeddings => TokenEstimate::Embeddings,
            InferenceEndpoint::OpenAiTranscriptions => TokenEstimate::Transcription,
            InferenceEndpoint::OpenAiImageEdits
            | InferenceEndpoint::OpenAiImageVariations
            | InferenceEndpoint::OpenAiVideoCreate
            | InferenceEndpoint::OpenAiVideoList => TokenEstimate::Media,
            InferenceEndpoint::Unknown { .. } => unreachable!(),
            _ => TokenEstimate::Default,
        };
        assert_eq!(
            endpoint.token_estimate(),
            expected_tokens,
            "tokens for {path}"
        );

        let expected_path_route = match endpoint {
            InferenceEndpoint::OpenAiModelGet
            | InferenceEndpoint::AnthropicModelGet
            | InferenceEndpoint::GeminiModelGet
            | InferenceEndpoint::GeminiGenerateContent
            | InferenceEndpoint::GeminiStreamGenerateContent
            | InferenceEndpoint::GeminiCountTokens
            | InferenceEndpoint::GeminiUnknownAction => path
                .split("/models/")
                .nth(1)
                .and_then(|resource| resource.split(':').next())
                .filter(|model| RouteSlug::parse(*model).is_ok())
                .map(str::to_owned),
            _ => None,
        };
        assert_eq!(
            endpoint.route_from_json(path, b"{}"),
            expected_path_route,
            "path route for {path}"
        );
    }

    #[test]
    fn registered_endpoint_policy_matrix_is_complete() {
        let cases = [
            (
                Method::POST,
                "/openai/v1/chat/completions",
                InferenceEndpoint::OpenAiChatCompletions,
            ),
            (
                Method::POST,
                "/openai/v1/responses",
                InferenceEndpoint::OpenAiResponses,
            ),
            (
                Method::POST,
                "/openai/v1/responses/input_tokens",
                InferenceEndpoint::OpenAiResponseInputTokens,
            ),
            (
                Method::POST,
                "/openai/v1/embeddings",
                InferenceEndpoint::OpenAiEmbeddings,
            ),
            (
                Method::POST,
                "/openai/v1/moderations",
                InferenceEndpoint::OpenAiModerations,
            ),
            (
                Method::POST,
                "/openai/v1/images/generations",
                InferenceEndpoint::OpenAiImageGenerations,
            ),
            (
                Method::POST,
                "/openai/v1/images/edits",
                InferenceEndpoint::OpenAiImageEdits,
            ),
            (
                Method::POST,
                "/openai/v1/images/variations",
                InferenceEndpoint::OpenAiImageVariations,
            ),
            (
                Method::POST,
                "/openai/v1/audio/speech",
                InferenceEndpoint::OpenAiSpeech,
            ),
            (
                Method::POST,
                "/openai/v1/audio/transcriptions",
                InferenceEndpoint::OpenAiTranscriptions,
            ),
            (
                Method::POST,
                "/openai/v1/videos",
                InferenceEndpoint::OpenAiVideoCreate,
            ),
            (
                Method::GET,
                "/openai/v1/videos",
                InferenceEndpoint::OpenAiVideoList,
            ),
            (
                Method::GET,
                "/openai/v1/videos/video-1",
                InferenceEndpoint::OpenAiVideoGet,
            ),
            (
                Method::DELETE,
                "/openai/v1/videos/video-1",
                InferenceEndpoint::OpenAiVideoDelete,
            ),
            (
                Method::GET,
                "/openai/v1/videos/video-1/content",
                InferenceEndpoint::OpenAiVideoContent,
            ),
            (
                Method::GET,
                "/openai/v1/models",
                InferenceEndpoint::OpenAiModelList,
            ),
            (
                Method::GET,
                "/openai/v1/models/route-1",
                InferenceEndpoint::OpenAiModelGet,
            ),
            (
                Method::POST,
                "/anthropic/v1/messages",
                InferenceEndpoint::AnthropicMessages,
            ),
            (
                Method::POST,
                "/anthropic/v1/messages/count_tokens",
                InferenceEndpoint::AnthropicCountTokens,
            ),
            (
                Method::GET,
                "/anthropic/v1/models",
                InferenceEndpoint::AnthropicModelList,
            ),
            (
                Method::GET,
                "/anthropic/v1/models/route-1",
                InferenceEndpoint::AnthropicModelGet,
            ),
            (
                Method::GET,
                "/gemini/v1/models",
                InferenceEndpoint::GeminiModelList,
            ),
            (
                Method::GET,
                "/gemini/v1/models/route-1",
                InferenceEndpoint::GeminiModelGet,
            ),
            (
                Method::POST,
                "/gemini/v1/models/route-1:generateContent",
                InferenceEndpoint::GeminiGenerateContent,
            ),
            (
                Method::POST,
                "/gemini/v1/models/route-1:streamGenerateContent",
                InferenceEndpoint::GeminiStreamGenerateContent,
            ),
            (
                Method::POST,
                "/gemini/v1/models/route-1:countTokens",
                InferenceEndpoint::GeminiCountTokens,
            ),
            (
                Method::POST,
                "/gemini/v1/models/route-1:unknown",
                InferenceEndpoint::GeminiUnknownAction,
            ),
            (
                Method::GET,
                "/gemini/v1beta/models",
                InferenceEndpoint::GeminiModelList,
            ),
            (
                Method::GET,
                "/gemini/v1beta/models/team/route-1",
                InferenceEndpoint::GeminiModelGet,
            ),
            (
                Method::POST,
                "/gemini/v1beta/models/team/route-1:generateContent",
                InferenceEndpoint::GeminiGenerateContent,
            ),
            (
                Method::POST,
                "/gemini/v1beta/models/team/route-1:streamGenerateContent",
                InferenceEndpoint::GeminiStreamGenerateContent,
            ),
            (
                Method::POST,
                "/gemini/v1beta/models/team/route-1:countTokens",
                InferenceEndpoint::GeminiCountTokens,
            ),
            (
                Method::POST,
                "/gemini/v1beta/models/team/route-1:unknown",
                InferenceEndpoint::GeminiUnknownAction,
            ),
        ];
        for (method, path, expected) in cases {
            assert_eq!(
                InferenceEndpoint::classify(&method, path),
                Some(expected),
                "{method} {path}"
            );
            assert_registered_facts(expected, path);
        }
    }

    #[test]
    fn representative_unknown_paths_keep_protocol_boundary_policy() {
        for (method, path, surface) in [
            (Method::GET, "/openai/v1/not-enabled", Surface::OpenAi),
            (Method::GET, "/anthropic/v2/messages", Surface::Anthropic),
            (
                Method::DELETE,
                "/gemini/v1/models/route:generateContent",
                Surface::Gemini,
            ),
            (Method::GET, "/openai/v1/videos/id/extra", Surface::OpenAi),
        ] {
            let endpoint = InferenceEndpoint::classify(&method, path).unwrap();
            assert!(
                matches!(endpoint, InferenceEndpoint::Unknown { .. }),
                "{method} {path}"
            );
            assert_eq!(endpoint.surface(), surface);
        }
        assert_eq!(
            InferenceEndpoint::classify(&Method::GET, "/api/v1/models"),
            None
        );
    }

    #[test]
    fn resource_names_do_not_change_endpoint_token_categories() {
        for path in [
            "/openai/v1/models/responses",
            "/openai/v1/videos/embeddings",
            "/anthropic/v1/models/messages",
            "/gemini/v1/models/route:generateContent",
        ] {
            let endpoint = InferenceEndpoint::classify(&Method::GET, path).unwrap();
            assert_eq!(endpoint.token_estimate(), TokenEstimate::Default, "{path}");
        }
    }
}
