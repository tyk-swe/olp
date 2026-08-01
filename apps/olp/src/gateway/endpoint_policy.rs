//! Canonical inference endpoint registry.
//!
//! Each routed `(method, path)` pair is declared exactly once in [`ENDPOINTS`].
//! The declaration owns its handler, surface, operation/metadata policy, body
//! admission, token estimation, and route extraction behavior.  Axum routing
//! and request-boundary classification both consume this registry.

use axum::{
    Router,
    extract::DefaultBodyLimit,
    http::Method,
    routing::{MethodFilter, MethodRouter, on},
};
use olp_domain::{OperationKind, RouteSlug, Surface};

use crate::{GatewayState, MAX_JSON_BODY_BYTES, MAX_MEDIA_BODY_BYTES};

use super::{anthropic, chat, gemini, media, openai_models, responses, videos};

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
    pub(crate) operation: OperationKind,
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

#[derive(Clone, Copy, Debug, Eq, Hash, Ord, PartialEq, PartialOrd)]
pub(crate) enum EndpointId {
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
    GeminiV1ModelList,
    GeminiV1ModelGet,
    GeminiV1ModelAction,
    GeminiV1BetaModelList,
    GeminiV1BetaModelGet,
    GeminiV1BetaModelAction,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum EndpointMethod {
    Get,
    Post,
    Delete,
}

impl EndpointMethod {
    fn matches(self, method: &Method) -> bool {
        matches!(
            (self, method),
            (Self::Get, &Method::GET)
                | (Self::Post, &Method::POST)
                | (Self::Delete, &Method::DELETE)
        )
    }

    const fn filter(self) -> MethodFilter {
        match self {
            Self::Get => MethodFilter::GET,
            Self::Post => MethodFilter::POST,
            Self::Delete => MethodFilter::DELETE,
        }
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum PathMatcher {
    Exact,
    SingleSegment {
        prefix: &'static str,
        suffix: Option<&'static str>,
    },
    Remainder {
        prefix: &'static str,
    },
}

impl PathMatcher {
    fn matches(self, route_path: &str, request_path: &str) -> bool {
        match self {
            Self::Exact => request_path == route_path,
            Self::SingleSegment { prefix, suffix } => single_segment(request_path, prefix, suffix),
            Self::Remainder { prefix } => request_path
                .strip_prefix(prefix)
                .is_some_and(|resource| !resource.is_empty()),
        }
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum RouteExtraction {
    JsonModel,
    JsonModelOrPath,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum Policy {
    Fixed {
        operation: OperationKind,
        fallback_route: &'static str,
        always_emit: bool,
        token_estimate: TokenEstimate,
    },
    GeminiAction,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum Handler {
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
    GeminiModelAction,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) struct EndpointSpec {
    pub(crate) id: EndpointId,
    method: EndpointMethod,
    pub(crate) route_path: &'static str,
    matcher: PathMatcher,
    surface: Surface,
    policy: Policy,
    body_admission: BodyAdmission,
    route_extraction: RouteExtraction,
    handler: Handler,
    axum_body_limit: Option<usize>,
}

macro_rules! fixed_endpoint {
    (
        id: $id:expr,
        method: $method:expr,
        route_path: $route_path:expr,
        matcher: $matcher:expr,
        surface: $surface:expr,
        operation: $operation:expr,
        fallback_route: $fallback_route:expr,
        always_emit: $always_emit:expr,
        token_estimate: $token_estimate:expr,
        body_admission: $body_admission:expr,
        route_extraction: $route_extraction:expr,
        handler: $handler:expr,
        axum_body_limit: $axum_body_limit:expr $(,)?
    ) => {
        EndpointSpec {
            id: $id,
            method: $method,
            route_path: $route_path,
            matcher: $matcher,
            surface: $surface,
            policy: Policy::Fixed {
                operation: $operation,
                fallback_route: $fallback_route,
                always_emit: $always_emit,
                token_estimate: $token_estimate,
            },
            body_admission: $body_admission,
            route_extraction: $route_extraction,
            handler: $handler,
            axum_body_limit: $axum_body_limit,
        }
    };
}

struct GeminiEndpoint {
    id: EndpointId,
    method: EndpointMethod,
    route_path: &'static str,
    matcher: PathMatcher,
    handler: Handler,
}

const fn gemini_fixed(endpoint: GeminiEndpoint, operation: OperationKind) -> EndpointSpec {
    gemini_endpoint(
        endpoint,
        Policy::Fixed {
            operation,
            fallback_route: "models",
            always_emit: true,
            token_estimate: TokenEstimate::Default,
        },
    )
}

const fn gemini_action_endpoint(endpoint: GeminiEndpoint) -> EndpointSpec {
    gemini_endpoint(endpoint, Policy::GeminiAction)
}

const fn gemini_endpoint(endpoint: GeminiEndpoint, policy: Policy) -> EndpointSpec {
    let GeminiEndpoint {
        id,
        method,
        route_path,
        matcher,
        handler,
    } = endpoint;
    EndpointSpec {
        id,
        method,
        route_path,
        matcher,
        surface: Surface::Gemini,
        policy,
        body_admission: BodyAdmission::Standard,
        route_extraction: if matches!(handler, Handler::GeminiModelList) {
            RouteExtraction::JsonModel
        } else {
            RouteExtraction::JsonModelOrPath
        },
        handler,
        axum_body_limit: None,
    }
}

const EXACT: PathMatcher = PathMatcher::Exact;
const INVALID_ROUTE: &str = "invalid-request";

pub(crate) static ENDPOINTS: &[EndpointSpec] = &[
    fixed_endpoint!(
        id: EndpointId::OpenAiChatCompletions,
        method: EndpointMethod::Post,
        route_path: "/openai/v1/chat/completions",
        matcher: EXACT,
        surface: Surface::OpenAi,
        operation: OperationKind::Generation,
        fallback_route: INVALID_ROUTE,
        always_emit: false,
        token_estimate: TokenEstimate::Generation,
        body_admission: BodyAdmission::Standard,
        route_extraction: RouteExtraction::JsonModel,
        handler: Handler::OpenAiChatCompletions,
        axum_body_limit: None,
    ),
    fixed_endpoint!(
        id: EndpointId::OpenAiResponses,
        method: EndpointMethod::Post,
        route_path: "/openai/v1/responses",
        matcher: EXACT,
        surface: Surface::OpenAi,
        operation: OperationKind::Generation,
        fallback_route: INVALID_ROUTE,
        always_emit: false,
        token_estimate: TokenEstimate::Generation,
        body_admission: BodyAdmission::Standard,
        route_extraction: RouteExtraction::JsonModel,
        handler: Handler::OpenAiResponses,
        axum_body_limit: None,
    ),
    fixed_endpoint!(
        id: EndpointId::OpenAiResponseInputTokens,
        method: EndpointMethod::Post,
        route_path: "/openai/v1/responses/input_tokens",
        matcher: EXACT,
        surface: Surface::OpenAi,
        operation: OperationKind::TokenCount,
        fallback_route: INVALID_ROUTE,
        always_emit: false,
        token_estimate: TokenEstimate::Default,
        body_admission: BodyAdmission::Standard,
        route_extraction: RouteExtraction::JsonModel,
        handler: Handler::OpenAiResponseInputTokens,
        axum_body_limit: None,
    ),
    fixed_endpoint!(
        id: EndpointId::OpenAiEmbeddings,
        method: EndpointMethod::Post,
        route_path: "/openai/v1/embeddings",
        matcher: EXACT,
        surface: Surface::OpenAi,
        operation: OperationKind::Embeddings,
        fallback_route: INVALID_ROUTE,
        always_emit: false,
        token_estimate: TokenEstimate::Embeddings,
        body_admission: BodyAdmission::Standard,
        route_extraction: RouteExtraction::JsonModel,
        handler: Handler::OpenAiEmbeddings,
        axum_body_limit: None,
    ),
    fixed_endpoint!(
        id: EndpointId::OpenAiModerations,
        method: EndpointMethod::Post,
        route_path: "/openai/v1/moderations",
        matcher: EXACT,
        surface: Surface::OpenAi,
        operation: OperationKind::Moderation,
        fallback_route: INVALID_ROUTE,
        always_emit: false,
        token_estimate: TokenEstimate::Default,
        body_admission: BodyAdmission::Standard,
        route_extraction: RouteExtraction::JsonModel,
        handler: Handler::OpenAiModerations,
        axum_body_limit: None,
    ),
    fixed_endpoint!(
        id: EndpointId::OpenAiImageGenerations,
        method: EndpointMethod::Post,
        route_path: "/openai/v1/images/generations",
        matcher: EXACT,
        surface: Surface::OpenAi,
        operation: OperationKind::ImageGeneration,
        fallback_route: INVALID_ROUTE,
        always_emit: false,
        token_estimate: TokenEstimate::Default,
        body_admission: BodyAdmission::Media,
        route_extraction: RouteExtraction::JsonModel,
        handler: Handler::OpenAiImageGenerations,
        axum_body_limit: None,
    ),
    fixed_endpoint!(
        id: EndpointId::OpenAiImageEdits,
        method: EndpointMethod::Post,
        route_path: "/openai/v1/images/edits",
        matcher: EXACT,
        surface: Surface::OpenAi,
        operation: OperationKind::ImageEdit,
        fallback_route: INVALID_ROUTE,
        always_emit: false,
        token_estimate: TokenEstimate::Media,
        body_admission: BodyAdmission::Multipart {
        operation: OperationKind::ImageEdit,
        reservation_bytes: MAX_MEDIA_BODY_BYTES as u64,
        },
        route_extraction: RouteExtraction::JsonModel,
        handler: Handler::OpenAiImageEdits,
        axum_body_limit: Some(MAX_MEDIA_BODY_BYTES),
    ),
    fixed_endpoint!(
        id: EndpointId::OpenAiImageVariations,
        method: EndpointMethod::Post,
        route_path: "/openai/v1/images/variations",
        matcher: EXACT,
        surface: Surface::OpenAi,
        operation: OperationKind::ImageVariation,
        fallback_route: INVALID_ROUTE,
        always_emit: false,
        token_estimate: TokenEstimate::Media,
        body_admission: BodyAdmission::Multipart {
        operation: OperationKind::ImageVariation,
        reservation_bytes: IMAGE_VARIATION_BODY_BYTES as u64,
        },
        route_extraction: RouteExtraction::JsonModel,
        handler: Handler::OpenAiImageVariations,
        axum_body_limit: Some(IMAGE_VARIATION_BODY_BYTES),
    ),
    fixed_endpoint!(
        id: EndpointId::OpenAiSpeech,
        method: EndpointMethod::Post,
        route_path: "/openai/v1/audio/speech",
        matcher: EXACT,
        surface: Surface::OpenAi,
        operation: OperationKind::Speech,
        fallback_route: INVALID_ROUTE,
        always_emit: false,
        token_estimate: TokenEstimate::Default,
        body_admission: BodyAdmission::Media,
        route_extraction: RouteExtraction::JsonModel,
        handler: Handler::OpenAiSpeech,
        axum_body_limit: None,
    ),
    fixed_endpoint!(
        id: EndpointId::OpenAiTranscriptions,
        method: EndpointMethod::Post,
        route_path: "/openai/v1/audio/transcriptions",
        matcher: EXACT,
        surface: Surface::OpenAi,
        operation: OperationKind::Transcription,
        fallback_route: INVALID_ROUTE,
        always_emit: false,
        token_estimate: TokenEstimate::Transcription,
        body_admission: BodyAdmission::Multipart {
        operation: OperationKind::Transcription,
        reservation_bytes: TRANSCRIPTION_BODY_BYTES as u64,
        },
        route_extraction: RouteExtraction::JsonModel,
        handler: Handler::OpenAiTranscriptions,
        axum_body_limit: Some(TRANSCRIPTION_BODY_BYTES),
    ),
    fixed_endpoint!(
        id: EndpointId::OpenAiVideoCreate,
        method: EndpointMethod::Post,
        route_path: "/openai/v1/videos",
        matcher: EXACT,
        surface: Surface::OpenAi,
        operation: OperationKind::VideoCreate,
        fallback_route: INVALID_ROUTE,
        always_emit: false,
        token_estimate: TokenEstimate::Media,
        body_admission: BodyAdmission::Multipart {
        operation: OperationKind::VideoCreate,
        reservation_bytes: VIDEO_CREATE_BODY_BYTES as u64,
        },
        route_extraction: RouteExtraction::JsonModel,
        handler: Handler::OpenAiVideoCreate,
        axum_body_limit: Some(VIDEO_CREATE_BODY_BYTES),
    ),
    fixed_endpoint!(
        id: EndpointId::OpenAiVideoList,
        method: EndpointMethod::Get,
        route_path: "/openai/v1/videos",
        matcher: EXACT,
        surface: Surface::OpenAi,
        operation: OperationKind::VideoList,
        fallback_route: "videos",
        always_emit: true,
        token_estimate: TokenEstimate::Media,
        body_admission: BodyAdmission::Media,
        route_extraction: RouteExtraction::JsonModel,
        handler: Handler::OpenAiVideoList,
        axum_body_limit: Some(VIDEO_CREATE_BODY_BYTES),
    ),
    fixed_endpoint!(
        id: EndpointId::OpenAiVideoGet,
        method: EndpointMethod::Get,
        route_path: "/openai/v1/videos/{video_id}",
        matcher: PathMatcher::SingleSegment {
        prefix: "/openai/v1/videos/",
        suffix: None,
        },
        surface: Surface::OpenAi,
        operation: OperationKind::VideoGet,
        fallback_route: INVALID_ROUTE,
        always_emit: false,
        token_estimate: TokenEstimate::Default,
        body_admission: BodyAdmission::Standard,
        route_extraction: RouteExtraction::JsonModel,
        handler: Handler::OpenAiVideoGet,
        axum_body_limit: None,
    ),
    fixed_endpoint!(
        id: EndpointId::OpenAiVideoDelete,
        method: EndpointMethod::Delete,
        route_path: "/openai/v1/videos/{video_id}",
        matcher: PathMatcher::SingleSegment {
        prefix: "/openai/v1/videos/",
        suffix: None,
        },
        surface: Surface::OpenAi,
        operation: OperationKind::VideoDelete,
        fallback_route: INVALID_ROUTE,
        always_emit: false,
        token_estimate: TokenEstimate::Default,
        body_admission: BodyAdmission::Standard,
        route_extraction: RouteExtraction::JsonModel,
        handler: Handler::OpenAiVideoDelete,
        axum_body_limit: None,
    ),
    fixed_endpoint!(
        id: EndpointId::OpenAiVideoContent,
        method: EndpointMethod::Get,
        route_path: "/openai/v1/videos/{video_id}/content",
        matcher: PathMatcher::SingleSegment {
        prefix: "/openai/v1/videos/",
        suffix: Some("/content"),
        },
        surface: Surface::OpenAi,
        operation: OperationKind::VideoContent,
        fallback_route: INVALID_ROUTE,
        always_emit: false,
        token_estimate: TokenEstimate::Default,
        body_admission: BodyAdmission::Standard,
        route_extraction: RouteExtraction::JsonModel,
        handler: Handler::OpenAiVideoContent,
        axum_body_limit: None,
    ),
    fixed_endpoint!(
        id: EndpointId::OpenAiModelList,
        method: EndpointMethod::Get,
        route_path: "/openai/v1/models",
        matcher: EXACT,
        surface: Surface::OpenAi,
        operation: OperationKind::ModelList,
        fallback_route: "models",
        always_emit: true,
        token_estimate: TokenEstimate::Default,
        body_admission: BodyAdmission::Standard,
        route_extraction: RouteExtraction::JsonModel,
        handler: Handler::OpenAiModelList,
        axum_body_limit: None,
    ),
    fixed_endpoint!(
        id: EndpointId::OpenAiModelGet,
        method: EndpointMethod::Get,
        route_path: "/openai/v1/models/{id}",
        matcher: PathMatcher::SingleSegment {
        prefix: "/openai/v1/models/",
        suffix: None,
        },
        surface: Surface::OpenAi,
        operation: OperationKind::ModelGet,
        fallback_route: "models",
        always_emit: true,
        token_estimate: TokenEstimate::Default,
        body_admission: BodyAdmission::Standard,
        route_extraction: RouteExtraction::JsonModelOrPath,
        handler: Handler::OpenAiModelGet,
        axum_body_limit: None,
    ),
    fixed_endpoint!(
        id: EndpointId::AnthropicMessages,
        method: EndpointMethod::Post,
        route_path: "/anthropic/v1/messages",
        matcher: EXACT,
        surface: Surface::Anthropic,
        operation: OperationKind::Generation,
        fallback_route: INVALID_ROUTE,
        always_emit: false,
        token_estimate: TokenEstimate::Generation,
        body_admission: BodyAdmission::Standard,
        route_extraction: RouteExtraction::JsonModel,
        handler: Handler::AnthropicMessages,
        axum_body_limit: None,
    ),
    fixed_endpoint!(
        id: EndpointId::AnthropicCountTokens,
        method: EndpointMethod::Post,
        route_path: "/anthropic/v1/messages/count_tokens",
        matcher: EXACT,
        surface: Surface::Anthropic,
        operation: OperationKind::TokenCount,
        fallback_route: INVALID_ROUTE,
        always_emit: false,
        token_estimate: TokenEstimate::Default,
        body_admission: BodyAdmission::Standard,
        route_extraction: RouteExtraction::JsonModel,
        handler: Handler::AnthropicCountTokens,
        axum_body_limit: None,
    ),
    fixed_endpoint!(
        id: EndpointId::AnthropicModelList,
        method: EndpointMethod::Get,
        route_path: "/anthropic/v1/models",
        matcher: EXACT,
        surface: Surface::Anthropic,
        operation: OperationKind::ModelList,
        fallback_route: "models",
        always_emit: true,
        token_estimate: TokenEstimate::Default,
        body_admission: BodyAdmission::Standard,
        route_extraction: RouteExtraction::JsonModel,
        handler: Handler::AnthropicModelList,
        axum_body_limit: None,
    ),
    fixed_endpoint!(
        id: EndpointId::AnthropicModelGet,
        method: EndpointMethod::Get,
        route_path: "/anthropic/v1/models/{id}",
        matcher: PathMatcher::SingleSegment {
        prefix: "/anthropic/v1/models/",
        suffix: None,
        },
        surface: Surface::Anthropic,
        operation: OperationKind::ModelGet,
        fallback_route: "models",
        always_emit: true,
        token_estimate: TokenEstimate::Default,
        body_admission: BodyAdmission::Standard,
        route_extraction: RouteExtraction::JsonModelOrPath,
        handler: Handler::AnthropicModelGet,
        axum_body_limit: None,
    ),
    gemini_fixed(
        GeminiEndpoint {
            id: EndpointId::GeminiV1ModelList,
            method: EndpointMethod::Get,
            route_path: "/gemini/v1/models",
            matcher: EXACT,
            handler: Handler::GeminiModelList,
        },
        OperationKind::ModelList,
    ),
    gemini_fixed(
        GeminiEndpoint {
            id: EndpointId::GeminiV1ModelGet,
            method: EndpointMethod::Get,
            route_path: "/gemini/v1/models/{*resource}",
            matcher: PathMatcher::Remainder {
                prefix: "/gemini/v1/models/",
            },
            handler: Handler::GeminiModelGet,
        },
        OperationKind::ModelGet,
    ),
    gemini_action_endpoint(GeminiEndpoint {
        id: EndpointId::GeminiV1ModelAction,
        method: EndpointMethod::Post,
        route_path: "/gemini/v1/models/{*resource}",
        matcher: PathMatcher::Remainder {
            prefix: "/gemini/v1/models/",
        },
        handler: Handler::GeminiModelAction,
    }),
    gemini_fixed(
        GeminiEndpoint {
            id: EndpointId::GeminiV1BetaModelList,
            method: EndpointMethod::Get,
            route_path: "/gemini/v1beta/models",
            matcher: EXACT,
            handler: Handler::GeminiModelList,
        },
        OperationKind::ModelList,
    ),
    gemini_fixed(
        GeminiEndpoint {
            id: EndpointId::GeminiV1BetaModelGet,
            method: EndpointMethod::Get,
            route_path: "/gemini/v1beta/models/{*resource}",
            matcher: PathMatcher::Remainder {
                prefix: "/gemini/v1beta/models/",
            },
            handler: Handler::GeminiModelGet,
        },
        OperationKind::ModelGet,
    ),
    gemini_action_endpoint(GeminiEndpoint {
        id: EndpointId::GeminiV1BetaModelAction,
        method: EndpointMethod::Post,
        route_path: "/gemini/v1beta/models/{*resource}",
        matcher: PathMatcher::Remainder {
            prefix: "/gemini/v1beta/models/",
        },
        handler: Handler::GeminiModelAction,
    }),
];

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum GeminiAction {
    Generate,
    StreamGenerate,
    CountTokens,
    Unsupported,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum InferenceEndpoint {
    Registered {
        spec: &'static EndpointSpec,
        action: Option<GeminiAction>,
    },
    Unknown {
        surface: Surface,
        media_body: bool,
        token_estimate: TokenEstimate,
    },
}

impl InferenceEndpoint {
    pub(crate) fn classify(method: &Method, path: &str) -> Option<Self> {
        if let Some(spec) = ENDPOINTS
            .iter()
            .find(|spec| spec.method.matches(method) && spec.matcher.matches(spec.route_path, path))
        {
            let action = matches!(spec.policy, Policy::GeminiAction).then(|| gemini_action(path));
            return Some(Self::Registered { spec, action });
        }
        let surface = surface_from_path(path)?;
        Some(Self::Unknown {
            surface,
            media_body: path.starts_with("/openai/v1/images/")
                || path.starts_with("/openai/v1/audio/")
                || path == "/openai/v1/videos",
            token_estimate: token_estimate_from_path(path),
        })
    }

    pub(crate) const fn surface(self) -> Surface {
        match self {
            Self::Registered { spec, .. } => spec.surface,
            Self::Unknown { surface, .. } => surface,
        }
    }

    pub(crate) const fn metadata(self) -> Option<MetadataPolicy> {
        let Self::Registered { spec, action } = self else {
            return None;
        };
        match spec.policy {
            Policy::Fixed {
                operation,
                fallback_route,
                always_emit,
                ..
            } => Some(MetadataPolicy {
                operation,
                fallback_route,
                always_emit,
            }),
            Policy::GeminiAction => match action {
                Some(GeminiAction::Generate | GeminiAction::StreamGenerate) => {
                    Some(MetadataPolicy {
                        operation: OperationKind::Generation,
                        fallback_route: INVALID_ROUTE,
                        always_emit: false,
                    })
                }
                Some(GeminiAction::CountTokens) => Some(MetadataPolicy {
                    operation: OperationKind::TokenCount,
                    fallback_route: INVALID_ROUTE,
                    always_emit: false,
                }),
                Some(GeminiAction::Unsupported) | None => None,
            },
        }
    }

    const fn body_admission(self) -> BodyAdmission {
        match self {
            Self::Registered { spec, .. } => spec.body_admission,
            Self::Unknown {
                media_body: true, ..
            } => BodyAdmission::Media,
            Self::Unknown { .. } => BodyAdmission::Standard,
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
            BodyAdmission::Standard | BodyAdmission::Media => None,
        }
    }

    pub(crate) const fn token_estimate(self) -> TokenEstimate {
        match self {
            Self::Registered { spec, action } => match spec.policy {
                Policy::Fixed { token_estimate, .. } => token_estimate,
                Policy::GeminiAction => match action {
                    Some(GeminiAction::Generate | GeminiAction::StreamGenerate) => {
                        TokenEstimate::Generation
                    }
                    Some(GeminiAction::CountTokens | GeminiAction::Unsupported) | None => {
                        TokenEstimate::Default
                    }
                },
            },
            Self::Unknown { token_estimate, .. } => token_estimate,
        }
    }

    pub(crate) fn route_from_json(self, path: &str, body: &[u8]) -> Option<String> {
        if let Ok(value) = serde_json::from_slice::<serde_json::Value>(body)
            && let Some(model) = value.get("model").and_then(serde_json::Value::as_str)
            && RouteSlug::parse(model).is_ok()
        {
            return Some(model.to_owned());
        }
        let Self::Registered { spec, .. } = self else {
            return None;
        };
        if spec.route_extraction != RouteExtraction::JsonModelOrPath {
            return None;
        }
        let resource = path.split("/models/").nth(1)?;
        let model = resource.split(':').next()?;
        RouteSlug::parse(model).is_ok().then(|| model.to_owned())
    }
}

pub(super) fn router() -> Router<GatewayState> {
    ENDPOINTS.iter().fold(Router::new(), register)
}

fn register(router: Router<GatewayState>, spec: &'static EndpointSpec) -> Router<GatewayState> {
    let filter = spec.method.filter();
    let method_router: MethodRouter<GatewayState> = match spec.handler {
        Handler::OpenAiChatCompletions => on(filter, chat::chat_completions),
        Handler::OpenAiResponses => on(filter, responses::responses),
        Handler::OpenAiResponseInputTokens => on(filter, responses::response_input_tokens),
        Handler::OpenAiEmbeddings => on(filter, media::embeddings),
        Handler::OpenAiModerations => on(filter, media::moderations),
        Handler::OpenAiImageGenerations => on(filter, media::image_generations),
        Handler::OpenAiImageEdits => on(filter, media::image_edits),
        Handler::OpenAiImageVariations => on(filter, media::image_variations),
        Handler::OpenAiSpeech => on(filter, media::speech),
        Handler::OpenAiTranscriptions => on(filter, media::transcriptions),
        Handler::OpenAiVideoCreate => on(filter, videos::video_create),
        Handler::OpenAiVideoList => on(filter, videos::video_list),
        Handler::OpenAiVideoGet => on(filter, videos::video_get),
        Handler::OpenAiVideoDelete => on(filter, videos::video_delete),
        Handler::OpenAiVideoContent => on(filter, videos::video_content),
        Handler::OpenAiModelList => on(filter, openai_models::list_models),
        Handler::OpenAiModelGet => on(filter, openai_models::get_model),
        Handler::AnthropicMessages => on(filter, anthropic::messages),
        Handler::AnthropicCountTokens => on(filter, anthropic::count_tokens),
        Handler::AnthropicModelList => on(filter, anthropic::models),
        Handler::AnthropicModelGet => on(filter, anthropic::model),
        Handler::GeminiModelList => on(filter, gemini::models),
        Handler::GeminiModelGet => on(filter, gemini::model),
        Handler::GeminiModelAction => on(filter, gemini::action),
    };
    let method_router = spec.axum_body_limit.map_or(method_router.clone(), |limit| {
        method_router.layer(DefaultBodyLimit::max(limit))
    });
    router.route(spec.route_path, method_router)
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

fn gemini_action(path: &str) -> GeminiAction {
    if path.ends_with(":generateContent") {
        GeminiAction::Generate
    } else if path.ends_with(":streamGenerateContent") {
        GeminiAction::StreamGenerate
    } else if path.ends_with(":countTokens") {
        GeminiAction::CountTokens
    } else {
        GeminiAction::Unsupported
    }
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

#[cfg(test)]
mod tests {
    use std::collections::BTreeSet;

    use super::*;

    fn representative_path(spec: &EndpointSpec) -> String {
        match spec.matcher {
            PathMatcher::Exact => spec.route_path.to_owned(),
            PathMatcher::SingleSegment { prefix, suffix } => {
                format!("{prefix}route-1{}", suffix.unwrap_or_default())
            }
            PathMatcher::Remainder { prefix } => {
                if matches!(spec.policy, Policy::GeminiAction) {
                    format!("{prefix}route-1:generateContent")
                } else {
                    format!("{prefix}route-1")
                }
            }
        }
    }

    fn method(spec: &EndpointSpec) -> Method {
        match spec.method {
            EndpointMethod::Get => Method::GET,
            EndpointMethod::Post => Method::POST,
            EndpointMethod::Delete => Method::DELETE,
        }
    }

    #[test]
    fn registry_identities_and_routes_are_unique() {
        let mut identities = BTreeSet::new();
        let mut routes = BTreeSet::new();
        for spec in ENDPOINTS {
            assert!(
                identities.insert(spec.id),
                "duplicate identity: {:?}",
                spec.id
            );
            assert!(
                routes.insert((spec.method as u8, spec.route_path)),
                "duplicate route: {:?} {}",
                spec.method,
                spec.route_path
            );
        }
    }

    #[test]
    fn every_registry_entry_drives_classification_and_policy() {
        for spec in ENDPOINTS {
            let path = representative_path(spec);
            let endpoint = InferenceEndpoint::classify(&method(spec), &path)
                .expect("a registered surface is classified");
            let InferenceEndpoint::Registered {
                spec: classified, ..
            } = endpoint
            else {
                panic!("registered endpoint classified as unknown: {:?}", spec.id);
            };
            assert_eq!(classified.id, spec.id);
            assert_eq!(endpoint.surface(), spec.surface);
            assert!(endpoint.metadata().is_some());
        }
    }

    #[test]
    fn unsupported_gemini_actions_are_explicit_and_metadata_free() {
        let endpoint =
            InferenceEndpoint::classify(&Method::POST, "/gemini/v1/models/route-1:unsupported")
                .unwrap();
        assert_eq!(endpoint.surface(), Surface::Gemini);
        assert_eq!(endpoint.metadata(), None);
        assert_eq!(
            endpoint.route_from_json("/gemini/v1/models/route-1:unsupported", b"{}"),
            Some("route-1".to_owned())
        );
    }
}
