use crate::ids::RouteSlug;
use crate::inference::transport::ProviderTransport;
use crate::protocols::canonical::identity::OperationKind;
use crate::protocols::canonical::identity::Surface;
use crate::protocols::canonical::identity::TransportMode;
use crate::protocols::canonical::requests::ContentPart;
use crate::protocols::canonical::requests::EmbeddingInput;
use crate::protocols::canonical::requests::EmbeddingsRequest;
use crate::protocols::canonical::requests::GenerationParameters;
use crate::protocols::canonical::requests::GenerationRequest;
use crate::protocols::canonical::requests::Message;
use crate::protocols::canonical::requests::MessageRole;
use crate::protocols::canonical::requests::ModerationRequest;
use crate::protocols::canonical::requests::Operation;
use crate::protocols::canonical::requests::SourceExtensions;
use crate::protocols::canonical::requests::TokenCountRequest;
use crate::providers::runtime_model::ProviderKind;

use crate::providers::openai::certification::CompatibleCapability;
use crate::providers::openai::certification::CompatibleCapabilityCertificationError;
use crate::providers::openai::certification::NativeOpenAiCertificationEvidence;
use crate::providers::openai::certification::execute_capability_probe;

use crate::providers::connectors::ProviderConnector;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum CapabilityCertificationEvidence {
    LiveProbe,
    NativeOpenAiModelDiscoveryAndConnectorContract,
}

impl From<NativeOpenAiCertificationEvidence> for CapabilityCertificationEvidence {
    fn from(value: NativeOpenAiCertificationEvidence) -> Self {
        match value {
            NativeOpenAiCertificationEvidence::LiveProbe => Self::LiveProbe,
            NativeOpenAiCertificationEvidence::ModelDiscoveryAndConnectorContract => {
                Self::NativeOpenAiModelDiscoveryAndConnectorContract
            }
        }
    }
}

/// Returns whether the installed connector has a safe certification path for
/// a reviewed capability. This is narrower than configuration eligibility: the
/// management UI must not offer tuples that can never satisfy activation's
/// certification requirement.
pub const fn supports(
    kind: ProviderKind,
    operation: OperationKind,
    surface: Surface,
    mode: TransportMode,
) -> bool {
    if !kind.supports_capability(operation, surface, mode) {
        return false;
    }

    match kind {
        ProviderKind::OpenAiCompatible => matches!(
            (operation, surface, mode),
            (
                OperationKind::Generation,
                Surface::OpenAi,
                TransportMode::Unary | TransportMode::Streaming
            ) | (
                OperationKind::Embeddings | OperationKind::TokenCount | OperationKind::Moderation,
                Surface::OpenAi,
                TransportMode::Unary
            )
        ),
        ProviderKind::AzureOpenAi => matches!(
            (operation, mode),
            (
                OperationKind::Generation,
                TransportMode::Unary | TransportMode::Streaming
            ) | (
                OperationKind::Embeddings | OperationKind::TokenCount | OperationKind::Moderation,
                TransportMode::Unary
            )
        ),
        _ => true,
    }
}

pub fn certifiable_capabilities(
    kind: ProviderKind,
) -> impl Iterator<Item = (OperationKind, Surface, TransportMode)> {
    kind.supported_capabilities()
        .filter(move |(operation, surface, mode)| supports(kind, *operation, *surface, *mode))
}

impl ProviderConnector {
    pub async fn certify_capability(
        &self,
        upstream_model: &str,
        capability: CompatibleCapability,
    ) -> Result<CapabilityCertificationEvidence, CompatibleCapabilityCertificationError> {
        match self {
            Self::OpenAiCompatible(connector) => connector
                .certify_compatible_capability(upstream_model, capability)
                .await
                .map(|()| CapabilityCertificationEvidence::LiveProbe),
            Self::AzureOpenAi(connector) => connector
                .certify_deployment_capability(upstream_model, capability)
                .await
                .map(|()| CapabilityCertificationEvidence::LiveProbe),
            Self::OpenAi(connector) if capability.surface == Surface::OpenAi => connector
                .certify_native_openai_capability(upstream_model, capability)
                .await
                .map(Into::into),
            Self::OpenAi(_)
            | Self::Anthropic(_)
            | Self::Gemini(_)
            | Self::Vertex(_)
            | Self::Bedrock(_) => {
                let kind = match self {
                    Self::OpenAi(_) => ProviderKind::OpenAi,
                    Self::Anthropic(_) => ProviderKind::Anthropic,
                    Self::Gemini(_) => ProviderKind::Gemini,
                    Self::Vertex(_) => ProviderKind::VertexAi,
                    Self::Bedrock(_) => ProviderKind::Bedrock,
                    _ => unreachable!(),
                };
                execute_native_capability_probe(
                    self.as_transport(),
                    kind,
                    upstream_model,
                    capability,
                )
                .await
                .map(|()| CapabilityCertificationEvidence::LiveProbe)
            }
        }
    }
}

pub(crate) async fn execute_native_capability_probe(
    transport: &dyn ProviderTransport,
    provider_kind: ProviderKind,
    upstream_model: &str,
    capability: CompatibleCapability,
) -> Result<(), CompatibleCapabilityCertificationError> {
    let operation = native_probe_operation(provider_kind, capability)?;
    execute_capability_probe(
        transport,
        provider_kind,
        upstream_model,
        capability,
        operation,
    )
    .await
}

pub(crate) fn native_probe_operation(
    provider_kind: ProviderKind,
    capability: CompatibleCapability,
) -> Result<Operation, CompatibleCapabilityCertificationError> {
    let route = RouteSlug::parse("capability-probe")
        .map_err(|_| CompatibleCapabilityCertificationError::InvalidResult)?;
    let extensions = || SourceExtensions::new(capability.surface, Default::default());
    match (provider_kind, capability.operation, capability.mode) {
        (
            ProviderKind::OpenAi
            | ProviderKind::Anthropic
            | ProviderKind::Gemini
            | ProviderKind::VertexAi
            | ProviderKind::Bedrock,
            OperationKind::Generation,
            TransportMode::Unary | TransportMode::Streaming,
        ) => Ok(Operation::Generation(GenerationRequest {
            route,
            messages: vec![Message {
                role: MessageRole::User,
                content: vec![ContentPart::Text {
                    text: "OLP capability probe".to_owned(),
                }],
                name: None,
                tool_call_id: None,
                tool_calls: Vec::new(),
            }],
            parameters: GenerationParameters {
                max_output_tokens: Some(1),
                temperature: Some(0.0),
                stream: capability.mode == TransportMode::Streaming,
                ..GenerationParameters::default()
            },
            tools: Vec::new(),
            tool_choice: None,
            response_format: None,
            extensions: extensions(),
        })),
        (
            ProviderKind::OpenAi
            | ProviderKind::Anthropic
            | ProviderKind::Gemini
            | ProviderKind::VertexAi
            | ProviderKind::Bedrock,
            OperationKind::TokenCount,
            TransportMode::Unary,
        ) => Ok(Operation::TokenCount(TokenCountRequest {
            route,
            input: vec![ContentPart::Text {
                text: "OLP capability probe".to_owned(),
            }],
            extensions: extensions(),
        })),
        (ProviderKind::OpenAi, OperationKind::Embeddings, TransportMode::Unary)
            if capability.surface == Surface::OpenAi =>
        {
            Ok(Operation::Embeddings(EmbeddingsRequest {
                route,
                input: vec![EmbeddingInput::Text("OLP capability probe".to_owned())],
                dimensions: None,
                extensions: extensions(),
            }))
        }
        (ProviderKind::OpenAi, OperationKind::Moderation, TransportMode::Unary)
            if capability.surface == Surface::OpenAi =>
        {
            Ok(Operation::Moderation(ModerationRequest {
                route,
                input: vec![ContentPart::Text {
                    text: "OLP capability probe".to_owned(),
                }],
                extensions: extensions(),
            }))
        }
        _ => Err(CompatibleCapabilityCertificationError::Unsupported),
    }
}
