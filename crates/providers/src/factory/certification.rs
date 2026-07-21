use std::time::Duration;

use futures::StreamExt as _;
use olp_domain::{
    AttemptFailureClass, AttemptPlan, CanonicalEventKind, CanonicalResult, ContentPart,
    EmbeddingInput, EmbeddingsRequest, EventSequenceValidator, GenerationParameters,
    GenerationRequest, Message, MessageRole, ModerationRequest, Operation, OperationKind,
    ProviderId, ProviderKind, ProviderOutput, ProviderRequest, ProviderTransport, RequestId,
    RequestMetadata, RouteId, RouteSlug, RuntimeGenerationId, SourceExtensions, Surface, TargetId,
    TokenCountRequest, TransportMode, TransportPhase,
};

use crate::openai::{
    CompatibleCapability, CompatibleCapabilityCertificationError, NativeOpenAiCertificationEvidence,
};

use super::assembly::{ConcreteConnector, ConcreteProvider};

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
pub const fn supports_capability_certification(
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
        .filter(move |(operation, surface, mode)| {
            supports_capability_certification(kind, *operation, *surface, *mode)
        })
}

impl ConcreteProvider {
    pub(super) async fn certify_capability(
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
            (ConcreteConnector::OpenAi(connector), ProviderKind::OpenAi) => {
                execute_native_capability_probe(
                    connector.as_ref(),
                    ProviderKind::OpenAi,
                    upstream_model,
                    capability,
                )
                .await
                .map(|()| CapabilityCertificationEvidence::LiveProbe)
            }
            (ConcreteConnector::Anthropic(connector), ProviderKind::Anthropic) => {
                execute_native_capability_probe(
                    connector.as_ref(),
                    ProviderKind::Anthropic,
                    upstream_model,
                    capability,
                )
                .await
                .map(|()| CapabilityCertificationEvidence::LiveProbe)
            }
            (ConcreteConnector::Gemini(connector), ProviderKind::Gemini) => {
                execute_native_capability_probe(
                    connector.as_ref(),
                    ProviderKind::Gemini,
                    upstream_model,
                    capability,
                )
                .await
                .map(|()| CapabilityCertificationEvidence::LiveProbe)
            }
            (ConcreteConnector::Vertex(connector), ProviderKind::VertexAi) => {
                execute_native_capability_probe(
                    connector.as_ref(),
                    ProviderKind::VertexAi,
                    upstream_model,
                    capability,
                )
                .await
                .map(|()| CapabilityCertificationEvidence::LiveProbe)
            }
            (ConcreteConnector::Bedrock(connector), ProviderKind::Bedrock) => {
                execute_native_capability_probe(
                    connector.as_ref(),
                    ProviderKind::Bedrock,
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

const NATIVE_PROBE_TIMEOUT_MS: u64 = 10_000;
const MAX_NATIVE_PROBE_EVENTS: usize = 4_096;

pub(super) async fn execute_native_capability_probe(
    transport: &dyn ProviderTransport,
    provider_kind: ProviderKind,
    upstream_model: &str,
    capability: CompatibleCapability,
) -> Result<(), CompatibleCapabilityCertificationError> {
    let operation = native_probe_operation(provider_kind, capability)?;
    let request = ProviderRequest {
        metadata: RequestMetadata {
            request_id: RequestId::new(),
            operation: capability.operation,
            surface: capability.surface,
            mode: capability.mode,
        },
        attempt: AttemptPlan {
            generation_id: RuntimeGenerationId::new(),
            route_id: RouteId::new(),
            target_id: TargetId::new(),
            provider_id: ProviderId::new(),
            provider_kind,
            upstream_model: upstream_model.to_owned(),
            timeout: olp_domain::DurationMs::new(NATIVE_PROBE_TIMEOUT_MS),
            priority: 0,
        },
        operation,
        media: None,
    };
    let output = tokio::time::timeout(
        Duration::from_millis(NATIVE_PROBE_TIMEOUT_MS),
        transport.execute(request),
    )
    .await
    .map_err(|_| CompatibleCapabilityCertificationError::Transport {
        phase: TransportPhase::FirstByte,
        class: AttemptFailureClass::Timeout,
    })?
    .map_err(|error| CompatibleCapabilityCertificationError::Transport {
        phase: error.phase,
        class: error.class,
    })?;
    validate_native_probe_output(capability.operation, output).await
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

async fn validate_native_probe_output(
    operation: OperationKind,
    output: ProviderOutput,
) -> Result<(), CompatibleCapabilityCertificationError> {
    match (operation, output) {
        (OperationKind::Generation, ProviderOutput::Events(mut events)) => {
            let mut validator = EventSequenceValidator::new();
            let deadline =
                tokio::time::Instant::now() + Duration::from_millis(NATIVE_PROBE_TIMEOUT_MS);
            let mut count = 0_usize;
            loop {
                let event = tokio::time::timeout_at(deadline, events.next())
                    .await
                    .map_err(|_| CompatibleCapabilityCertificationError::Transport {
                        phase: TransportPhase::Body,
                        class: AttemptFailureClass::Timeout,
                    })?;
                let Some(event) = event else {
                    break;
                };
                if count >= MAX_NATIVE_PROBE_EVENTS {
                    return Err(CompatibleCapabilityCertificationError::InvalidResult);
                }
                let event =
                    event.map_err(|error| CompatibleCapabilityCertificationError::Transport {
                        phase: error.phase,
                        class: error.class,
                    })?;
                if matches!(event.kind, CanonicalEventKind::Error { .. }) {
                    return Err(CompatibleCapabilityCertificationError::InvalidResult);
                }
                validator
                    .push(&event)
                    .map_err(|_| CompatibleCapabilityCertificationError::InvalidResult)?;
                count = count.saturating_add(1);
                if validator.is_complete() {
                    break;
                }
            }
            validator
                .finish()
                .map_err(|_| CompatibleCapabilityCertificationError::InvalidResult)
        }
        (OperationKind::TokenCount, ProviderOutput::Result(result))
            if matches!(&*result, CanonicalResult::TokenCount(_)) =>
        {
            Ok(())
        }
        (OperationKind::Embeddings, ProviderOutput::Result(result)) if matches!(&*result, CanonicalResult::Embeddings(value) if !value.data.is_empty() && value.data.iter().all(|item| !item.values.is_empty())) => {
            Ok(())
        }
        (OperationKind::Moderation, ProviderOutput::Result(result)) if matches!(&*result, CanonicalResult::Moderation(value) if !value.results.is_empty()) => {
            Ok(())
        }
        _ => Err(CompatibleCapabilityCertificationError::InvalidResult),
    }
}
