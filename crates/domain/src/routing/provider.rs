use std::{collections::BTreeSet, fmt, str::FromStr};

use serde::{Deserialize, Serialize};
use thiserror::Error;
use utoipa::ToSchema;

use crate::{CredentialVersionId, OperationKind, ProviderId, Surface, TransportMode};

#[derive(
    Clone, Copy, Debug, Deserialize, Eq, Hash, Ord, PartialEq, PartialOrd, Serialize, ToSchema,
)]
#[serde(rename_all = "snake_case")]
pub enum ProviderKind {
    #[serde(rename = "openai")]
    OpenAi,
    Anthropic,
    Gemini,
    VertexAi,
    Bedrock,
    #[serde(rename = "azure_openai")]
    AzureOpenAi,
    #[serde(rename = "openai_compatible")]
    OpenAiCompatible,
}

impl ProviderKind {
    pub const ALL: [Self; 7] = [
        Self::OpenAi,
        Self::Anthropic,
        Self::Gemini,
        Self::VertexAi,
        Self::Bedrock,
        Self::AzureOpenAi,
        Self::OpenAiCompatible,
    ];

    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::OpenAi => "openai",
            Self::Anthropic => "anthropic",
            Self::Gemini => "gemini",
            Self::VertexAi => "vertex_ai",
            Self::Bedrock => "bedrock",
            Self::AzureOpenAi => "azure_openai",
            Self::OpenAiCompatible => "openai_compatible",
        }
    }

    /// Returns whether this provider family can serve a reviewed capability
    /// tuple. Connector-specific request validation remains at the adapter
    /// boundary; this is the canonical configuration eligibility policy.
    #[must_use]
    pub const fn supports_capability(
        self,
        operation: OperationKind,
        surface: Surface,
        mode: TransportMode,
    ) -> bool {
        let shared_canonical_operation = matches!(
            surface,
            Surface::OpenAi | Surface::Anthropic | Surface::Gemini
        ) && matches!(
            (operation, mode),
            (
                OperationKind::Generation,
                TransportMode::Unary | TransportMode::Streaming
            ) | (OperationKind::TokenCount, TransportMode::Unary)
        );

        match self {
            Self::Anthropic | Self::Gemini | Self::VertexAi | Self::Bedrock => {
                shared_canonical_operation
            }
            Self::OpenAi | Self::OpenAiCompatible | Self::AzureOpenAi => {
                shared_canonical_operation
                    || (matches!(surface, Surface::OpenAi)
                        && matches!(
                            (operation, mode),
                            (
                                OperationKind::Embeddings
                                    | OperationKind::ImageVariation
                                    | OperationKind::Moderation,
                                TransportMode::Unary
                            ) | (
                                OperationKind::ImageGeneration
                                    | OperationKind::ImageEdit
                                    | OperationKind::Speech
                                    | OperationKind::Transcription,
                                TransportMode::Unary | TransportMode::Streaming
                            ) | (OperationKind::VideoCreate, TransportMode::Async)
                                | (
                                    OperationKind::VideoList
                                        | OperationKind::VideoGet
                                        | OperationKind::VideoContent
                                        | OperationKind::VideoDelete,
                                    TransportMode::Unary
                                )
                        ))
            }
        }
    }

    /// Iterates the reviewed capability tuples supported by this provider
    /// family. This is intentionally derived from [`Self::supports_capability`]
    /// so API consumers cannot drift from configuration validation.
    pub fn supported_capabilities(
        self,
    ) -> impl Iterator<Item = (OperationKind, Surface, TransportMode)> {
        OperationKind::ALL.into_iter().flat_map(move |operation| {
            Surface::ALL.into_iter().flat_map(move |surface| {
                TransportMode::ALL
                    .into_iter()
                    .filter(move |mode| self.supports_capability(operation, surface, *mode))
                    .map(move |mode| (operation, surface, mode))
            })
        })
    }
}

impl FromStr for ProviderKind {
    type Err = InvalidProviderKind;

    fn from_str(value: &str) -> Result<Self, Self::Err> {
        match value {
            "openai" => Ok(Self::OpenAi),
            "anthropic" => Ok(Self::Anthropic),
            "gemini" => Ok(Self::Gemini),
            "vertex_ai" => Ok(Self::VertexAi),
            "bedrock" => Ok(Self::Bedrock),
            "azure_openai" => Ok(Self::AzureOpenAi),
            "openai_compatible" => Ok(Self::OpenAiCompatible),
            _ => Err(InvalidProviderKind),
        }
    }
}

impl fmt::Display for ProviderKind {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(self.as_str())
    }
}

#[derive(Clone, Copy, Debug, Error, Eq, PartialEq)]
#[error("invalid canonical provider kind")]
pub struct InvalidProviderKind;

#[derive(Clone, Debug, Deserialize, Eq, Hash, Ord, PartialEq, PartialOrd, Serialize)]
pub struct Capability {
    pub model: String,
    pub operation: OperationKind,
    pub surface: Surface,
    pub mode: TransportMode,
}

impl Capability {
    #[must_use]
    pub fn new(
        model: impl Into<String>,
        operation: OperationKind,
        surface: Surface,
        mode: TransportMode,
    ) -> Self {
        Self {
            model: model.into(),
            operation,
            surface,
            mode,
        }
    }
}

#[derive(Clone, Debug, Deserialize, Serialize)]
pub struct Provider {
    pub id: ProviderId,
    pub name: String,
    pub kind: ProviderKind,
    pub enabled: bool,
    pub active_credential: Option<CredentialVersionId>,
    #[serde(default)]
    pub capabilities: BTreeSet<Capability>,
}

impl Provider {
    #[must_use]
    pub fn supports(
        &self,
        model: &str,
        operation: OperationKind,
        surface: Surface,
        mode: TransportMode,
    ) -> bool {
        self.enabled
            && self
                .capabilities
                .contains(&Capability::new(model, operation, surface, mode))
    }
}
