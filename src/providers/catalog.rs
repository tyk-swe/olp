//! Reviewed vendor profiles. Vendor identity is independent of wire protocol.
use serde::Serialize;
use std::sync::LazyLock;
use utoipa::ToSchema;

use crate::providers::runtime_model::ProviderKind;

#[derive(Clone, Debug, Serialize, ToSchema)]
pub struct Vendor {
    pub id: &'static str,
    pub name: &'static str,
    pub connector: ProviderKind,
    pub endpoint: Option<&'static str>,
    pub discovery: bool,
    pub operations: &'static [&'static str],
    pub authentication: Vec<crate::providers::types::ProviderAuthMode>,
    pub parameters: Vec<&'static str>,
    pub documentation_url: &'static str,
}

/// Compatible vendors that expose Chat Completions only: Responses-specific
/// hints are stripped and `max_completion_tokens` is renamed on the wire.
pub(crate) fn chat_completions_only(vendor_id: Option<&str>) -> bool {
    matches!(
        vendor_id,
        Some("deepseek" | "fireworks" | "deepinfra" | "huggingface" | "perplexity" | "cohere")
    )
}

/// Request parameters Cohere's compatible endpoint cannot represent, whether
/// supplied as connection defaults or as request extensions.
pub(crate) const COHERE_UNSUPPORTED_PARAMETERS: &[&str] = &[
    "n",
    "parallel_tool_calls",
    "dimensions",
    "user",
    "store",
    "metadata",
    "logit_bias",
    "top_logprobs",
    "modalities",
    "prediction",
    "audio",
    "service_tier",
    "input_type",
    "truncate",
];

/// Reviewed facts for one vendor. Official vendors carry their own identity;
/// OpenAI-compatible presets take name, endpoint, and documentation from the
/// connector spec and fall back to `PRESET_PROFILE` when unlisted here.
struct Profile {
    id: &'static str,
    official: Option<(&'static str, ProviderKind, &'static str)>,
    discovery: bool,
    operations: &'static [&'static str],
    parameters: &'static [&'static str],
}

const GENERATION: &[&str] = &["generation"];
const GENERATION_AND_TOKEN_COUNT: &[&str] = &["generation", "token_count"];
const GENERATION_AND_EMBEDDINGS: &[&str] = &["generation", "embeddings"];
const DEFAULT_PARAMETERS: &[&str] = &["temperature", "max_output_tokens"];

const PRESET_PROFILE: Profile = Profile {
    id: "",
    official: None,
    discovery: true,
    operations: GENERATION,
    parameters: DEFAULT_PARAMETERS,
};

const PROFILES: &[Profile] = &[
    Profile {
        id: "openai",
        official: Some((
            "OpenAI",
            ProviderKind::OpenAi,
            "https://platform.openai.com/docs/api-reference",
        )),
        discovery: true,
        operations: GENERATION_AND_EMBEDDINGS,
        parameters: DEFAULT_PARAMETERS,
    },
    Profile {
        id: "anthropic",
        official: Some((
            "Anthropic",
            ProviderKind::Anthropic,
            "https://docs.anthropic.com/en/api/overview",
        )),
        discovery: true,
        operations: GENERATION_AND_TOKEN_COUNT,
        parameters: DEFAULT_PARAMETERS,
    },
    Profile {
        id: "google",
        official: Some((
            "Google Gemini",
            ProviderKind::Gemini,
            "https://ai.google.dev/gemini-api/docs",
        )),
        discovery: true,
        operations: GENERATION_AND_TOKEN_COUNT,
        parameters: DEFAULT_PARAMETERS,
    },
    Profile {
        id: "google-vertex",
        official: Some((
            "Google Vertex AI",
            ProviderKind::VertexAi,
            "https://cloud.google.com/vertex-ai/generative-ai/docs",
        )),
        discovery: false,
        operations: GENERATION_AND_TOKEN_COUNT,
        parameters: DEFAULT_PARAMETERS,
    },
    Profile {
        id: "amazon-bedrock",
        official: Some((
            "Amazon Bedrock",
            ProviderKind::Bedrock,
            "https://docs.aws.amazon.com/bedrock/",
        )),
        discovery: true,
        operations: GENERATION_AND_TOKEN_COUNT,
        parameters: DEFAULT_PARAMETERS,
    },
    Profile {
        id: "azure",
        official: Some((
            "Azure OpenAI",
            ProviderKind::AzureOpenAi,
            "https://learn.microsoft.com/azure/ai-services/openai/",
        )),
        discovery: true,
        operations: GENERATION_AND_EMBEDDINGS,
        parameters: DEFAULT_PARAMETERS,
    },
    Profile {
        id: "perplexity",
        official: None,
        discovery: false,
        operations: GENERATION,
        parameters: DEFAULT_PARAMETERS,
    },
    Profile {
        id: "voyage",
        official: None,
        discovery: false,
        operations: &["embeddings"],
        parameters: &[
            "dimensions",
            "input_type",
            "truncation",
            "output_dtype",
            "encoding_format",
        ],
    },
    Profile {
        id: "cohere",
        official: None,
        discovery: false,
        operations: GENERATION_AND_EMBEDDINGS,
        parameters: &[
            "temperature",
            "max_output_tokens",
            "top_p",
            "stop",
            "seed",
            "tools",
            "response_format",
            "encoding_format",
        ],
    },
];

fn vendor_profile(
    profile: &Profile,
    id: &'static str,
    name: &'static str,
    connector: ProviderKind,
    endpoint: Option<&'static str>,
    documentation_url: &'static str,
) -> Vendor {
    Vendor {
        id,
        name,
        connector,
        endpoint,
        discovery: profile.discovery,
        operations: profile.operations,
        authentication: crate::providers::validation::provider_kind_spec(connector)
            .auth_modes
            .iter()
            .map(|auth| auth.mode)
            .collect(),
        parameters: profile.parameters.to_vec(),
        documentation_url,
    }
}

static VENDORS: LazyLock<Vec<Vendor>> = LazyLock::new(|| {
    let official = PROFILES.iter().filter_map(|profile| {
        let (name, connector, documentation_url) = profile.official?;
        Some(vendor_profile(
            profile,
            profile.id,
            name,
            connector,
            None,
            documentation_url,
        ))
    });
    let presets = crate::providers::validation::provider_kind_spec(ProviderKind::OpenAiCompatible)
        .presets
        .iter()
        .map(|preset| {
            let profile = PROFILES
                .iter()
                .find(|profile| profile.id == preset.id)
                .unwrap_or(&PRESET_PROFILE);
            vendor_profile(
                profile,
                preset.id,
                preset.label,
                ProviderKind::OpenAiCompatible,
                Some(preset.endpoint),
                preset.documentation_url,
            )
        });
    official.chain(presets).collect()
});

pub fn vendors() -> &'static [Vendor] {
    &VENDORS
}

pub fn vendor(id: &str) -> Option<&'static Vendor> {
    VENDORS.iter().find(|vendor| vendor.id == id)
}

/// The configured vendor identity, or the official one inferred from the
/// connector and endpoint when none was configured.
pub(crate) fn effective_vendor<'a>(
    configured: Option<&'a str>,
    kind: ProviderKind,
    endpoint: Option<&str>,
) -> Option<&'a str> {
    configured.or_else(|| official_vendor(kind, endpoint))
}

/// Infer identity only for known official destinations. A custom protocol
/// endpoint does not acquire the identity of the protocol's original vendor.
pub(crate) fn official_vendor(kind: ProviderKind, endpoint: Option<&str>) -> Option<&'static str> {
    let host = endpoint
        .and_then(|url| url::Url::parse(url).ok())
        .and_then(|url| url.host_str().map(str::to_owned));
    use ProviderKind::*;
    match kind {
        OpenAi if endpoint.is_none() || host.as_deref() == Some("api.openai.com") => Some("openai"),
        Anthropic if endpoint.is_none() || host.as_deref() == Some("api.anthropic.com") => {
            Some("anthropic")
        }
        Gemini
            if endpoint.is_none()
                || host.as_deref() == Some("generativelanguage.googleapis.com") =>
        {
            Some("google")
        }
        AzureOpenAi
            if host
                .as_deref()
                .is_some_and(|h| h.ends_with(".openai.azure.com")) =>
        {
            Some("azure")
        }
        VertexAi => Some("google-vertex"),
        Bedrock => Some("amazon-bedrock"),
        OpenAiCompatible => crate::providers::validation::provider_kind_spec(kind)
            .presets
            .iter()
            .filter(|preset| {
                matches!(
                    preset.id,
                    "groq" | "mistral_ai" | "together_ai" | "xai" | "cerebras" | "openrouter"
                )
            })
            .find(|preset| endpoint.is_some_and(|e| e.trim_end_matches('/') == preset.endpoint))
            .map(|preset| preset.id),
        _ => None,
    }
}
