use crate::protocols::canonical::identity::OperationKind;
use crate::protocols::canonical::identity::Surface;
use axum::http::Method;
use axum::routing::MethodFilter;

use crate::inference::http::endpoint_policy::classification::TokenEstimate;

/// Fraction of the configured media body cap one multipart endpoint may
/// reserve, so per-endpoint sub-caps scale with the operator's setting.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) struct MediaShare {
    numerator: u64,
    denominator: u64,
}

impl MediaShare {
    pub(crate) const FULL: Self = Self::new(1, 1);
    const IMAGE_VARIATION: Self = Self::new(55, 64);
    const TRANSCRIPTION: Self = Self::new(30, 64);
    const VIDEO_CREATE: Self = Self::new(25, 64);

    const fn new(numerator: u64, denominator: u64) -> Self {
        Self {
            numerator,
            denominator,
        }
    }

    pub(crate) const fn reservation_bytes(self, media_body_bytes: usize) -> u64 {
        media_body_bytes as u64 / self.denominator * self.numerator
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum BodyAdmission {
    Standard,
    Media,
    Multipart { share: MediaShare },
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum EndpointMethod {
    Get,
    Post,
    Delete,
}

impl EndpointMethod {
    pub(crate) fn matches(self, method: &Method) -> bool {
        matches!(
            (self, method),
            (Self::Get, &Method::GET)
                | (Self::Post, &Method::POST)
                | (Self::Delete, &Method::DELETE)
        )
    }

    pub(crate) const fn filter(self) -> MethodFilter {
        match self {
            Self::Get => MethodFilter::GET,
            Self::Post => MethodFilter::POST,
            Self::Delete => MethodFilter::DELETE,
        }
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum PathMatcher {
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
pub(crate) struct EndpointAlias {
    pub(crate) route_path: &'static str,
    pub(crate) matcher: PathMatcher,
}

impl PathMatcher {
    pub(crate) fn matches(self, route_path: &str, request_path: &str) -> bool {
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
pub(crate) enum Policy {
    Fixed {
        operation: OperationKind,
        fallback_route: &'static str,
        always_emit: bool,
        token_estimate: TokenEstimate,
    },
    GeminiAction,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum Handler {
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
    pub(crate) method: EndpointMethod,
    pub(crate) route_path: &'static str,
    pub(crate) matcher: PathMatcher,
    pub(crate) aliases: &'static [EndpointAlias],
    pub(crate) surface: Surface,
    pub(crate) policy: Policy,
    pub(crate) body_admission: BodyAdmission,
    pub(crate) handler: Handler,
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
pub(crate) const INVALID_ROUTE: &str = "invalid-request";

pub(crate) static ENDPOINTS: &[EndpointSpec] = &[
    fixed_endpoint!(
        method: EndpointMethod::Post,
        route_path: "/v1/chat/completions",
        matcher: EXACT,
        aliases: &[],
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
        route_path: "/v1/responses",
        matcher: EXACT,
        aliases: &[],
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
        route_path: "/v1/responses/input_tokens",
        matcher: EXACT,
        aliases: &[],
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
        route_path: "/v1/embeddings",
        matcher: EXACT,
        aliases: &[],
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
        route_path: "/v1/moderations",
        matcher: EXACT,
        aliases: &[],
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
        route_path: "/v1/images/generations",
        matcher: EXACT,
        aliases: &[],
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
        route_path: "/v1/images/edits",
        matcher: EXACT,
        aliases: &[],
        surface: Surface::OpenAi,
        operation: OperationKind::ImageEdit,
        fallback_route: INVALID_ROUTE,
        always_emit: false,
        token_estimate: TokenEstimate::Media,
        body_admission: BodyAdmission::Multipart {
            share: MediaShare::FULL,
        },
        handler: Handler::OpenAiImageEdits,
    ),
    fixed_endpoint!(
        method: EndpointMethod::Post,
        route_path: "/v1/images/variations",
        matcher: EXACT,
        aliases: &[],
        surface: Surface::OpenAi,
        operation: OperationKind::ImageVariation,
        fallback_route: INVALID_ROUTE,
        always_emit: false,
        token_estimate: TokenEstimate::Media,
        body_admission: BodyAdmission::Multipart {
            share: MediaShare::IMAGE_VARIATION,
        },
        handler: Handler::OpenAiImageVariations,
    ),
    fixed_endpoint!(
        method: EndpointMethod::Post,
        route_path: "/v1/audio/speech",
        matcher: EXACT,
        aliases: &[],
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
        route_path: "/v1/audio/transcriptions",
        matcher: EXACT,
        aliases: &[],
        surface: Surface::OpenAi,
        operation: OperationKind::Transcription,
        fallback_route: INVALID_ROUTE,
        always_emit: false,
        token_estimate: TokenEstimate::Transcription,
        body_admission: BodyAdmission::Multipart {
            share: MediaShare::TRANSCRIPTION,
        },
        handler: Handler::OpenAiTranscriptions,
    ),
    fixed_endpoint!(
        method: EndpointMethod::Post,
        route_path: "/v1/videos",
        matcher: EXACT,
        aliases: &[],
        surface: Surface::OpenAi,
        operation: OperationKind::VideoCreate,
        fallback_route: INVALID_ROUTE,
        always_emit: false,
        token_estimate: TokenEstimate::Media,
        body_admission: BodyAdmission::Multipart {
            share: MediaShare::VIDEO_CREATE,
        },
        handler: Handler::OpenAiVideoCreate,
    ),
    fixed_endpoint!(
        method: EndpointMethod::Get,
        route_path: "/v1/videos",
        matcher: EXACT,
        aliases: &[],
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
        route_path: "/v1/videos/{video_id}",
        matcher: PathMatcher::SingleSegment {
        prefix: "/v1/videos/",
        suffix: None,
        },
        aliases: &[],
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
        route_path: "/v1/videos/{video_id}",
        matcher: PathMatcher::SingleSegment {
        prefix: "/v1/videos/",
        suffix: None,
        },
        aliases: &[],
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
        route_path: "/v1/videos/{video_id}/content",
        matcher: PathMatcher::SingleSegment {
        prefix: "/v1/videos/",
        suffix: Some("/content"),
        },
        aliases: &[],
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
        route_path: "/v1/models",
        matcher: EXACT,
        aliases: &[],
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
        route_path: "/v1/models/{id}",
        matcher: PathMatcher::SingleSegment {
        prefix: "/v1/models/",
        suffix: None,
        },
        aliases: &[],
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
