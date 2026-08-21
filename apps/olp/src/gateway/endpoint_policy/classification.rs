use std::borrow::Cow;

use axum::http::Method;
use olp_engine::domain::{
    auth::{GatewayCapability, gateway_capability_for_operation},
    canonical::identity::{OperationKind, Surface},
    ids::RouteSlug,
};

use crate::bootstrap::state::{MAX_JSON_BODY_BYTES, MAX_MEDIA_BODY_BYTES};

use super::registry::{
    BodyAdmission, ENDPOINTS, EndpointSpec, INVALID_ROUTE, Policy, RouteExtraction,
};

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
        if let Some(spec) = ENDPOINTS.iter().find(|spec| {
            spec.method.matches(method)
                && (spec.matcher.matches(spec.route_path, path)
                    || spec
                        .aliases
                        .iter()
                        .any(|alias| alias.matcher.matches(alias.route_path, path)))
        }) {
            let action = matches!(spec.policy, Policy::GeminiAction).then(|| gemini_action(path));
            return Some(Self::Registered { spec, action });
        }
        let surface = surface_from_path(path)?;
        Some(Self::Unknown {
            surface,
            media_body: path.starts_with("/openai/v1/images/")
                || path.starts_with("/openai/v1/audio/")
                || path == "/openai/v1/videos"
                || path.starts_with("/v1/images/")
                || path.starts_with("/v1/audio/")
                || path == "/v1/videos",
            token_estimate: token_estimate_from_path(path),
        })
    }

    pub(crate) const fn surface(self) -> Surface {
        match self {
            Self::Registered { spec, .. } => spec.surface,
            Self::Unknown { surface, .. } => surface,
        }
    }

    /// Resolves the capability from the registered endpoint's operation.
    /// Unsupported dynamic actions and unknown endpoints are deliberately
    /// capability-free and therefore cannot pass a later authorization check.
    pub(crate) const fn capability(self) -> Option<GatewayCapability> {
        match self.metadata() {
            Some(metadata) => Some(gateway_capability_for_operation(metadata.operation)),
            None => None,
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

    pub(crate) const fn multipart(self) -> Option<(GatewayCapability, u64)> {
        let Some(capability) = self.capability() else {
            return None;
        };
        match self.body_admission() {
            BodyAdmission::Multipart { reservation_bytes } => Some((capability, reservation_bytes)),
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
        let resource = path_model_resource(path).and_then(percent_decode_path_resource)?;
        let model = resource.split(':').next()?;
        RouteSlug::parse(model).is_ok().then(|| model.to_owned())
    }
}

fn surface_from_path(path: &str) -> Option<Surface> {
    if path.starts_with("/openai/") || path.starts_with("/v1/") {
        Some(Surface::OpenAi)
    } else if path.starts_with("/anthropic/") {
        Some(Surface::Anthropic)
    } else if path.starts_with("/gemini/") {
        Some(Surface::Gemini)
    } else {
        None
    }
}

fn path_model_resource(path: &str) -> Option<&str> {
    path.split_once("/models/")
        .map(|(_, resource)| resource)
        .filter(|resource| !resource.is_empty())
}

fn percent_decode_path_resource(resource: &str) -> Option<Cow<'_, str>> {
    let bytes = resource.as_bytes();
    let Some(first_percent) = bytes.iter().position(|byte| *byte == b'%') else {
        return Some(Cow::Borrowed(resource));
    };

    let mut decoded = Vec::with_capacity(bytes.len());
    decoded.extend_from_slice(&bytes[..first_percent]);
    let mut index = first_percent;
    while index < bytes.len() {
        if bytes[index] == b'%' {
            let high = hex_value(*bytes.get(index + 1)?)?;
            let low = hex_value(*bytes.get(index + 2)?)?;
            decoded.push((high << 4) | low);
            index += 3;
        } else {
            decoded.push(bytes[index]);
            index += 1;
        }
    }

    String::from_utf8(decoded).ok().map(Cow::Owned)
}

fn hex_value(byte: u8) -> Option<u8> {
    match byte {
        b'0'..=b'9' => Some(byte - b'0'),
        b'a'..=b'f' => Some(byte - b'a' + 10),
        b'A'..=b'F' => Some(byte - b'A' + 10),
        _ => None,
    }
}

fn gemini_action(path: &str) -> GeminiAction {
    let Some(resource) = path_model_resource(path).and_then(percent_decode_path_resource) else {
        return GeminiAction::Unsupported;
    };
    if resource.ends_with(":generateContent") {
        GeminiAction::Generate
    } else if resource.ends_with(":streamGenerateContent") {
        GeminiAction::StreamGenerate
    } else if resource.ends_with(":countTokens") {
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
