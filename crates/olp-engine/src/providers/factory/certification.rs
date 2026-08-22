use crate::domain::{
    canonical::{
        identity::{OperationKind, Surface, TransportMode},
        requests::{
            ContentPart, EmbeddingInput, EmbeddingsRequest, GenerationParameters,
            GenerationRequest, Message, MessageRole, ModerationRequest, Operation,
            SourceExtensions, TokenCountRequest,
        },
    },
    ids::RouteSlug,
    ports::ProviderTransport,
    routing::provider::ProviderKind,
};

use crate::providers::openai::certification::{
    CompatibleCapability, CompatibleCapabilityCertificationError,
    NativeOpenAiCertificationEvidence, execute_capability_probe,
};

use super::assembly::{ConcreteConnector, Facade};

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

impl Facade {
    pub async fn certify_capability(
        &self,
        upstream_model: &str,
        capability: CompatibleCapability,
    ) -> Result<CapabilityCertificationEvidence, CompatibleCapabilityCertificationError> {
        match (&self.connector, self.kind) {
            (ConcreteConnector::OpenAi(connector), ProviderKind::OpenAiCompatible) => connector
                .certify_compatible_capability(upstream_model, capability)
                .await
                .map(|()| CapabilityCertificationEvidence::LiveProbe),
            (ConcreteConnector::AzureOpenAi(connector), ProviderKind::AzureOpenAi) => connector
                .certify_deployment_capability(upstream_model, capability)
                .await
                .map(|()| CapabilityCertificationEvidence::LiveProbe),
            (ConcreteConnector::OpenAi(connector), ProviderKind::OpenAi)
                if capability.surface == Surface::OpenAi =>
            {
                connector
                    .certify_native_openai_capability(upstream_model, capability)
                    .await
                    .map(Into::into)
            }
            (_, kind)
                if matches!(
                    kind,
                    ProviderKind::OpenAi
                        | ProviderKind::Anthropic
                        | ProviderKind::Gemini
                        | ProviderKind::VertexAi
                        | ProviderKind::Bedrock
                ) =>
            {
                execute_native_capability_probe(
                    self.connector.as_transport(),
                    kind,
                    upstream_model,
                    capability,
                )
                .await
                .map(|()| CapabilityCertificationEvidence::LiveProbe)
            }
            _ => Err(CompatibleCapabilityCertificationError::Unsupported),
        }
    }
}

pub(super) async fn execute_native_capability_probe(
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

pub(super) fn native_probe_operation(
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
