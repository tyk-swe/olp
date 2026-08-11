use std::collections::BTreeSet;

use serde::{Deserialize, Serialize};
use thiserror::Error;

use crate::domain::{CredentialVersionId, OperationKind, ProviderId, Surface, TransportMode};

closed_string_enum! {
    pub enum ProviderKind {
        OpenAi => "openai",
        Anthropic => "anthropic",
        Gemini => "gemini",
        VertexAi => "vertex_ai",
        Bedrock => "bedrock",
        AzureOpenAi => "azure_openai",
        OpenAiCompatible => "openai_compatible",
    }
    parse_error InvalidProviderKind => |_| InvalidProviderKind;
}

impl ProviderKind {
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
