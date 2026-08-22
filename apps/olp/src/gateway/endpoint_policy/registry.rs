use axum::{http::Method, routing::MethodFilter};
use olp_engine::domain::canonical::identity::{OperationKind, Surface};

use crate::bootstrap::state::MAX_MEDIA_BODY_BYTES;

use super::classification::TokenEstimate;

const IMAGE_VARIATION_BODY_BYTES: usize = 55 * 1024 * 1024;
const TRANSCRIPTION_BODY_BYTES: usize = 30 * 1024 * 1024;
const VIDEO_CREATE_BODY_BYTES: usize = 25 * 1024 * 1024;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(super) enum BodyAdmission {
    Standard,
    Media,
    Multipart { reservation_bytes: u64 },
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(super) enum EndpointMethod {
    Get,
    Post,
    Delete,
}

impl EndpointMethod {
    pub(super) fn matches(self, method: &Method) -> bool {
        matches!(
            (self, method),
            (Self::Get, &Method::GET)
                | (Self::Post, &Method::POST)
                | (Self::Delete, &Method::DELETE)
        )
    }

    pub(super) const fn filter(self) -> MethodFilter {
        match self {
            Self::Get => MethodFilter::GET,
            Self::Post => MethodFilter::POST,
            Self::Delete => MethodFilter::DELETE,
        }
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(super) enum PathMatcher {
    Exact,
    SingleSegment {
        prefix: &'static str,
        suffix: Option<&'static str>,
    },
    Remainder {
        prefix: &'static str,
    },
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(super) struct EndpointAlias {
    pub(super) route_path: &'static str,
    pub(super) matcher: PathMatcher,
}

impl PathMatcher {
    pub(super) fn matches(self, route_path: &str, request_path: &str) -> bool {
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
pub(super) enum Policy {
    Fixed {
        operation: OperationKind,
        fallback_route: &'static str,
        always_emit: bool,
        token_estimate: TokenEstimate,
    },
    GeminiAction,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(super) enum Handler {
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
    pub(super) method: EndpointMethod,
    pub(super) route_path: &'static str,
    pub(super) matcher: PathMatcher,
    pub(super) aliases: &'static [EndpointAlias],
    pub(super) surface: Surface,
    pub(super) policy: Policy,
    pub(super) body_admission: BodyAdmission,
    pub(super) handler: Handler,
}

macro_rules! fixed_endpoint {
    (
        method: $method:expr,
        route_path: $route_path:expr,
        matcher: $matcher:expr,
        aliases: $aliases:expr,
        surface: $surface:expr,
        operation: $operation:expr,
        fallback_route: $fallback_route:expr,
        always_emit: $always_emit:expr,
        token_estimate: $token_estimate:expr,
        body_admission: $body_admission:expr,
        handler: $handler:expr $(,)?
    ) => {
        EndpointSpec {
            method: $method,
            route_path: $route_path,
            matcher: $matcher,
            aliases: $aliases,
            surface: $surface,
            policy: Policy::Fixed {
                operation: $operation,
                fallback_route: $fallback_route,
                always_emit: $always_emit,
                token_estimate: $token_estimate,
            },
            body_admission: $body_admission,
            handler: $handler,
        }
    };
}

struct GeminiEndpoint {
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
        method,
        route_path,
        matcher,
        handler,
    } = endpoint;
    EndpointSpec {
        method,
        route_path,
        matcher,
        aliases: &[],
        surface: Surface::Gemini,
        policy,
        body_admission: BodyAdmission::Standard,
        handler,
    }
}

const EXACT: PathMatcher = PathMatcher::Exact;
pub(super) const INVALID_ROUTE: &str = "invalid-request";

pub(super) static ENDPOINTS: &[EndpointSpec] = &[
    fixed_endpoint!(
        method: EndpointMethod::Post,
        route_path: "/openai/v1/chat/completions",
        matcher: EXACT,
        aliases: &[EndpointAlias {
            route_path: "/v1/chat/completions",
            matcher: EXACT,
        }],
        surface: Surface::OpenAi,
        operation: OperationKind::Generation,
        fallback_route: INVALID_ROUTE,
        always_emit: false,
        token_estimate: TokenEstimate::Generation,
        body_admission: BodyAdmission::Standard,
        handler: Handler::OpenAiChatCompletions,
    ),
    fixed_endpoint!(
        method: EndpointMethod::Post,
        route_path: "/openai/v1/responses",
        matcher: EXACT,
        aliases: &[EndpointAlias {
            route_path: "/v1/responses",
            matcher: EXACT,
        }],
        surface: Surface::OpenAi,
        operation: OperationKind::Generation,
        fallback_route: INVALID_ROUTE,
        always_emit: false,
        token_estimate: TokenEstimate::Generation,
        body_admission: BodyAdmission::Standard,
        handler: Handler::OpenAiResponses,
    ),
    fixed_endpoint!(
        method: EndpointMethod::Post,
        route_path: "/openai/v1/responses/input_tokens",
        matcher: EXACT,
        aliases: &[EndpointAlias {
            route_path: "/v1/responses/input_tokens",
            matcher: EXACT,
        }],
        surface: Surface::OpenAi,
        operation: OperationKind::TokenCount,
        fallback_route: INVALID_ROUTE,
        always_emit: false,
        token_estimate: TokenEstimate::Default,
        body_admission: BodyAdmission::Standard,
        handler: Handler::OpenAiResponseInputTokens,
    ),
    fixed_endpoint!(
        method: EndpointMethod::Post,
        route_path: "/openai/v1/embeddings",
        matcher: EXACT,
        aliases: &[EndpointAlias {
            route_path: "/v1/embeddings",
            matcher: EXACT,
        }],
        surface: Surface::OpenAi,
        operation: OperationKind::Embeddings,
        fallback_route: INVALID_ROUTE,
        always_emit: false,
        token_estimate: TokenEstimate::Embeddings,
        body_admission: BodyAdmission::Standard,
        handler: Handler::OpenAiEmbeddings,
    ),
    fixed_endpoint!(
        method: EndpointMethod::Post,
        route_path: "/openai/v1/moderations",
        matcher: EXACT,
        aliases: &[EndpointAlias {
            route_path: "/v1/moderations",
            matcher: EXACT,
        }],
        surface: Surface::OpenAi,
        operation: OperationKind::Moderation,
        fallback_route: INVALID_ROUTE,
        always_emit: false,
        token_estimate: TokenEstimate::Default,
        body_admission: BodyAdmission::Standard,
        handler: Handler::OpenAiModerations,
    ),
    fixed_endpoint!(
        method: EndpointMethod::Post,
        route_path: "/openai/v1/images/generations",
        matcher: EXACT,
        aliases: &[EndpointAlias {
            route_path: "/v1/images/generations",
            matcher: EXACT,
        }],
        surface: Surface::OpenAi,
        operation: OperationKind::ImageGeneration,
        fallback_route: INVALID_ROUTE,
        always_emit: false,
        token_estimate: TokenEstimate::Default,
        body_admission: BodyAdmission::Media,
        handler: Handler::OpenAiImageGenerations,
    ),
    fixed_endpoint!(
        method: EndpointMethod::Post,
        route_path: "/openai/v1/images/edits",
        matcher: EXACT,
        aliases: &[EndpointAlias {
            route_path: "/v1/images/edits",
            matcher: EXACT,
        }],
        surface: Surface::OpenAi,
        operation: OperationKind::ImageEdit,
        fallback_route: INVALID_ROUTE,
        always_emit: false,
        token_estimate: TokenEstimate::Media,
        body_admission: BodyAdmission::Multipart {
        reservation_bytes: MAX_MEDIA_BODY_BYTES as u64,
        },
        handler: Handler::OpenAiImageEdits,
    ),
    fixed_endpoint!(
        method: EndpointMethod::Post,
        route_path: "/openai/v1/images/variations",
        matcher: EXACT,
        aliases: &[EndpointAlias {
            route_path: "/v1/images/variations",
            matcher: EXACT,
        }],
        surface: Surface::OpenAi,
        operation: OperationKind::ImageVariation,
        fallback_route: INVALID_ROUTE,
        always_emit: false,
        token_estimate: TokenEstimate::Media,
        body_admission: BodyAdmission::Multipart {
        reservation_bytes: IMAGE_VARIATION_BODY_BYTES as u64,
        },
        handler: Handler::OpenAiImageVariations,
    ),
    fixed_endpoint!(
        method: EndpointMethod::Post,
        route_path: "/openai/v1/audio/speech",
        matcher: EXACT,
        aliases: &[EndpointAlias {
            route_path: "/v1/audio/speech",
            matcher: EXACT,
        }],
        surface: Surface::OpenAi,
        operation: OperationKind::Speech,
        fallback_route: INVALID_ROUTE,
        always_emit: false,
        token_estimate: TokenEstimate::Default,
        body_admission: BodyAdmission::Media,
        handler: Handler::OpenAiSpeech,
    ),
    fixed_endpoint!(
        method: EndpointMethod::Post,
        route_path: "/openai/v1/audio/transcriptions",
        matcher: EXACT,
        aliases: &[EndpointAlias {
            route_path: "/v1/audio/transcriptions",
            matcher: EXACT,
        }],
        surface: Surface::OpenAi,
        operation: OperationKind::Transcription,
        fallback_route: INVALID_ROUTE,
        always_emit: false,
        token_estimate: TokenEstimate::Transcription,
        body_admission: BodyAdmission::Multipart {
        reservation_bytes: TRANSCRIPTION_BODY_BYTES as u64,
        },
        handler: Handler::OpenAiTranscriptions,
    ),
    fixed_endpoint!(
        method: EndpointMethod::Post,
        route_path: "/openai/v1/videos",
        matcher: EXACT,
        aliases: &[EndpointAlias {
            route_path: "/v1/videos",
            matcher: EXACT,
        }],
        surface: Surface::OpenAi,
        operation: OperationKind::VideoCreate,
        fallback_route: INVALID_ROUTE,
        always_emit: false,
        token_estimate: TokenEstimate::Media,
        body_admission: BodyAdmission::Multipart {
        reservation_bytes: VIDEO_CREATE_BODY_BYTES as u64,
        },
        handler: Handler::OpenAiVideoCreate,
    ),
    fixed_endpoint!(
        method: EndpointMethod::Get,
        route_path: "/openai/v1/videos",
        matcher: EXACT,
        aliases: &[EndpointAlias {
            route_path: "/v1/videos",
            matcher: EXACT,
        }],
        surface: Surface::OpenAi,
        operation: OperationKind::VideoList,
        fallback_route: "videos",
        always_emit: true,
        token_estimate: TokenEstimate::Media,
        body_admission: BodyAdmission::Media,
        handler: Handler::OpenAiVideoList,
    ),
    fixed_endpoint!(
        method: EndpointMethod::Get,
        route_path: "/openai/v1/videos/{video_id}",
        matcher: PathMatcher::SingleSegment {
        prefix: "/openai/v1/videos/",
        suffix: None,
        },
        aliases: &[EndpointAlias {
            route_path: "/v1/videos/{video_id}",
            matcher: PathMatcher::SingleSegment {
                prefix: "/v1/videos/",
                suffix: None,
            },
        }],
        surface: Surface::OpenAi,
        operation: OperationKind::VideoGet,
        fallback_route: INVALID_ROUTE,
        always_emit: false,
        token_estimate: TokenEstimate::Default,
        body_admission: BodyAdmission::Standard,
        handler: Handler::OpenAiVideoGet,
    ),
    fixed_endpoint!(
        method: EndpointMethod::Delete,
        route_path: "/openai/v1/videos/{video_id}",
        matcher: PathMatcher::SingleSegment {
        prefix: "/openai/v1/videos/",
        suffix: None,
        },
        aliases: &[EndpointAlias {
            route_path: "/v1/videos/{video_id}",
            matcher: PathMatcher::SingleSegment {
                prefix: "/v1/videos/",
                suffix: None,
            },
        }],
        surface: Surface::OpenAi,
        operation: OperationKind::VideoDelete,
        fallback_route: INVALID_ROUTE,
        always_emit: false,
        token_estimate: TokenEstimate::Default,
        body_admission: BodyAdmission::Standard,
        handler: Handler::OpenAiVideoDelete,
    ),
    fixed_endpoint!(
        method: EndpointMethod::Get,
        route_path: "/openai/v1/videos/{video_id}/content",
        matcher: PathMatcher::SingleSegment {
        prefix: "/openai/v1/videos/",
        suffix: Some("/content"),
        },
        aliases: &[EndpointAlias {
            route_path: "/v1/videos/{video_id}/content",
            matcher: PathMatcher::SingleSegment {
                prefix: "/v1/videos/",
                suffix: Some("/content"),
            },
        }],
        surface: Surface::OpenAi,
        operation: OperationKind::VideoContent,
        fallback_route: INVALID_ROUTE,
        always_emit: false,
        token_estimate: TokenEstimate::Default,
        body_admission: BodyAdmission::Standard,
        handler: Handler::OpenAiVideoContent,
    ),
    fixed_endpoint!(
        method: EndpointMethod::Get,
        route_path: "/openai/v1/models",
        matcher: EXACT,
        aliases: &[EndpointAlias {
            route_path: "/v1/models",
            matcher: EXACT,
        }],
        surface: Surface::OpenAi,
        operation: OperationKind::ModelList,
        fallback_route: "models",
        always_emit: true,
        token_estimate: TokenEstimate::Default,
        body_admission: BodyAdmission::Standard,
        handler: Handler::OpenAiModelList,
    ),
    fixed_endpoint!(
        method: EndpointMethod::Get,
        route_path: "/openai/v1/models/{id}",
        matcher: PathMatcher::SingleSegment {
        prefix: "/openai/v1/models/",
        suffix: None,
        },
        aliases: &[EndpointAlias {
            route_path: "/v1/models/{model_id}",
            matcher: PathMatcher::SingleSegment {
                prefix: "/v1/models/",
                suffix: None,
            },
        }],
        surface: Surface::OpenAi,
        operation: OperationKind::ModelGet,
        fallback_route: "models",
        always_emit: true,
        token_estimate: TokenEstimate::Default,
        body_admission: BodyAdmission::Standard,
        handler: Handler::OpenAiModelGet,
    ),
    fixed_endpoint!(
        method: EndpointMethod::Post,
        route_path: "/anthropic/v1/messages",
        matcher: EXACT,
        aliases: &[],
        surface: Surface::Anthropic,
        operation: OperationKind::Generation,
        fallback_route: INVALID_ROUTE,
        always_emit: false,
        token_estimate: TokenEstimate::Generation,
        body_admission: BodyAdmission::Standard,
        handler: Handler::AnthropicMessages,
    ),
    fixed_endpoint!(
        method: EndpointMethod::Post,
        route_path: "/anthropic/v1/messages/count_tokens",
        matcher: EXACT,
        aliases: &[],
        surface: Surface::Anthropic,
        operation: OperationKind::TokenCount,
        fallback_route: INVALID_ROUTE,
        always_emit: false,
        token_estimate: TokenEstimate::Default,
        body_admission: BodyAdmission::Standard,
        handler: Handler::AnthropicCountTokens,
    ),
    fixed_endpoint!(
        method: EndpointMethod::Get,
        route_path: "/anthropic/v1/models",
        matcher: EXACT,
        aliases: &[],
        surface: Surface::Anthropic,
        operation: OperationKind::ModelList,
        fallback_route: "models",
        always_emit: true,
        token_estimate: TokenEstimate::Default,
        body_admission: BodyAdmission::Standard,
        handler: Handler::AnthropicModelList,
    ),
    fixed_endpoint!(
        method: EndpointMethod::Get,
        route_path: "/anthropic/v1/models/{id}",
        matcher: PathMatcher::SingleSegment {
        prefix: "/anthropic/v1/models/",
        suffix: None,
        },
        aliases: &[],
        surface: Surface::Anthropic,
        operation: OperationKind::ModelGet,
        fallback_route: "models",
        always_emit: true,
        token_estimate: TokenEstimate::Default,
        body_admission: BodyAdmission::Standard,
        handler: Handler::AnthropicModelGet,
    ),
    gemini_fixed(
        GeminiEndpoint {
            method: EndpointMethod::Get,
            route_path: "/gemini/v1/models",
            matcher: EXACT,
            handler: Handler::GeminiModelList,
        },
        OperationKind::ModelList,
    ),
    gemini_fixed(
        GeminiEndpoint {
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
        method: EndpointMethod::Post,
        route_path: "/gemini/v1/models/{*resource}",
        matcher: PathMatcher::Remainder {
            prefix: "/gemini/v1/models/",
        },
        handler: Handler::GeminiModelAction,
    }),
    gemini_fixed(
        GeminiEndpoint {
            method: EndpointMethod::Get,
            route_path: "/gemini/v1beta/models",
            matcher: EXACT,
            handler: Handler::GeminiModelList,
        },
        OperationKind::ModelList,
    ),
    gemini_fixed(
        GeminiEndpoint {
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
        method: EndpointMethod::Post,
        route_path: "/gemini/v1beta/models/{*resource}",
        matcher: PathMatcher::Remainder {
            prefix: "/gemini/v1beta/models/",
        },
        handler: Handler::GeminiModelAction,
    }),
];

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
