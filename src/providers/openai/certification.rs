use std::collections::BTreeMap;
use std::sync::Arc;
use std::time::Duration;

use crate::ids::DurationMs;
use crate::ids::ProviderId;
use crate::ids::RequestId;
use crate::ids::RouteId;
use crate::ids::RouteSlug;
use crate::ids::RuntimeGenerationId;
use crate::ids::TargetId;
use crate::inference::transport::AttemptFailureClass;
use crate::inference::transport::ProviderOutput;
use crate::inference::transport::ProviderRequest;
use crate::inference::transport::ProviderTransport;
use crate::inference::transport::TransportPhase;
use crate::protocols::canonical::events::EventSequenceValidator;
use crate::protocols::canonical::events::Kind;
use crate::protocols::canonical::identity::OperationKind;
use crate::protocols::canonical::identity::RequestMetadata;
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
use crate::protocols::canonical::requests::OPENAI_ENDPOINT_EXTENSION;
use crate::protocols::canonical::requests::Operation;
use crate::protocols::canonical::requests::SourceExtensions;
use crate::protocols::canonical::requests::TokenCountRequest;
use crate::protocols::canonical::results::CanonicalResult;
use crate::providers::runtime_model::ProviderKind;
use crate::routes::selection::AttemptPlan;
use futures::StreamExt as _;
use serde_json::Value;

use crate::providers::openai::transport::Connector;

const PROBE_TIMEOUT_MS: u64 = 10_000;
const MAX_PROBE_EVENTS: usize = 4_096;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct CompatibleCapability {
    pub operation: OperationKind,
    pub surface: Surface,
    pub mode: TransportMode,
}

/// Server-owned evidence accepted only for the official native OpenAI
/// connector. Generic compatible endpoints must continue to use exact live
/// probes through [`Connector::certify_compatible_capability`].
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum NativeOpenAiCertificationEvidence {
    LiveProbe,
    ModelDiscoveryAndConnectorContract,
}

#[derive(Clone, Debug, Eq, PartialEq, thiserror::Error)]
pub enum CompatibleCapabilityCertificationError {
    #[error("the capability tuple does not have a safe compatible-endpoint certification probe")]
    Unsupported,
    #[error("the capability probe transport failed during {phase:?} ({class:?})")]
    Transport {
        phase: TransportPhase,
        class: AttemptFailureClass,
    },
    #[error("the capability probe returned an invalid canonical result")]
    InvalidResult,
    #[error("credentialed model discovery did not return the exact provider model")]
    ModelNotDiscovered,
}

impl Connector {
    /// Executes a bounded, content-minimal request through the same transport
    /// and response codecs used by inference. A tuple is certifiable only when
    /// the compatible endpoint proves the exact operation and transport mode;
    /// cross-protocol surfaces and operations that require user media or create
    /// costly asynchronous jobs intentionally fail closed.
    pub async fn certify_compatible_capability(
        &self,
        upstream_model: &str,
        capability: CompatibleCapability,
    ) -> Result<(), CompatibleCapabilityCertificationError> {
        self.execute_probe_operations(upstream_model, capability, probe_operations(capability)?)
            .await
    }

    /// Certifies an official native OpenAI tuple. Safe content-minimal live
    /// probes remain authoritative wherever they exist. Selected media,
    /// and asynchronous-video operations cannot be probed without user
    /// media, cost, or side effects; those tuples instead require a
    /// credentialed, bounded `/models` response containing the exact target
    /// model and an entry in the closed native connector contract matrix.
    ///
    /// Callers must never use this fallback for generic OpenAI-compatible
    /// endpoints. Their capability breadth remains live-probe-only through
    /// [`Self::certify_compatible_capability`].
    pub async fn certify_native_openai_capability(
        &self,
        upstream_model: &str,
        capability: CompatibleCapability,
    ) -> Result<NativeOpenAiCertificationEvidence, CompatibleCapabilityCertificationError> {
        if probe_operations(capability).is_ok() {
            self.certify_compatible_capability(upstream_model, capability)
                .await?;
            return Ok(NativeOpenAiCertificationEvidence::LiveProbe);
        }
        if !native_openai_discovery_contract(capability) {
            return Err(CompatibleCapabilityCertificationError::Unsupported);
        }
        let discovered = self.discover_models().await.map_err(|error| {
            CompatibleCapabilityCertificationError::Transport {
                phase: error.phase,
                class: error.class,
            }
        })?;
        if !discovered.iter().any(|model| model.id == upstream_model) {
            return Err(CompatibleCapabilityCertificationError::ModelNotDiscovered);
        }
        Ok(NativeOpenAiCertificationEvidence::ModelDiscoveryAndConnectorContract)
    }

    /// Proves only the Chat Completions transport for a canonical generation
    /// tuple. Azure uses this bounded probe to test a deployment path before
    /// its operation breadth is known. Full OpenAI-surface generation
    /// certification must still use [`Self::certify_compatible_capability`],
    /// which proves both Chat Completions and Responses.
    pub async fn certify_chat_completions_capability(
        &self,
        upstream_model: &str,
        mode: TransportMode,
    ) -> Result<(), CompatibleCapabilityCertificationError> {
        let capability = CompatibleCapability {
            operation: OperationKind::Generation,
            surface: Surface::OpenAi,
            mode,
        };
        let operation = generation_probe_operation(mode, false)?;
        self.execute_probe_operations(upstream_model, capability, vec![operation])
            .await
    }

    async fn execute_probe_operations(
        &self,
        upstream_model: &str,
        capability: CompatibleCapability,
        operations: Vec<Operation>,
    ) -> Result<(), CompatibleCapabilityCertificationError> {
        for operation in operations {
            execute_capability_probe(
                self,
                ProviderKind::OpenAiCompatible,
                upstream_model,
                capability,
                operation,
            )
            .await?;
        }
        Ok(())
    }
}

/// Closed fallback matrix for official OpenAI operations where a certification
/// probe would require user media, billable generation, or mutation of an
/// asynchronous job. Adding a new tuple requires an explicit code change.
const fn native_openai_discovery_contract(capability: CompatibleCapability) -> bool {
    if !matches!(capability.surface, Surface::OpenAi) {
        return false;
    }
    matches!(
        (capability.operation, capability.mode),
        (
            OperationKind::ImageGeneration
                | OperationKind::ImageEdit
                | OperationKind::Speech
                | OperationKind::Transcription,
            TransportMode::Unary | TransportMode::Streaming
        ) | (
            OperationKind::ImageVariation
                | OperationKind::VideoList
                | OperationKind::VideoGet
                | OperationKind::VideoContent
                | OperationKind::VideoDelete,
            TransportMode::Unary
        ) | (OperationKind::VideoCreate, TransportMode::Async)
    )
}

fn generation_probe_operation(
    mode: TransportMode,
    responses: bool,
) -> Result<Operation, CompatibleCapabilityCertificationError> {
    if !matches!(mode, TransportMode::Unary | TransportMode::Streaming) {
        return Err(CompatibleCapabilityCertificationError::Unsupported);
    }
    let route = RouteSlug::parse("capability-probe")
        .map_err(|_| CompatibleCapabilityCertificationError::InvalidResult)?;
    let extensions = if responses {
        SourceExtensions::new(
            Surface::OpenAi,
            BTreeMap::from([(
                OPENAI_ENDPOINT_EXTENSION.to_owned(),
                Value::String("responses".to_owned()),
            )]),
        )
    } else {
        SourceExtensions::default()
    };
    Ok(Operation::Generation(GenerationRequest {
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
            stream: mode == TransportMode::Streaming,
            ..GenerationParameters::default()
        },
        tools: Vec::new(),
        tool_choice: None,
        response_format: None,
        extensions,
    }))
}

fn probe_operations(
    capability: CompatibleCapability,
) -> Result<Vec<Operation>, CompatibleCapabilityCertificationError> {
    if capability.surface != Surface::OpenAi {
        return Err(CompatibleCapabilityCertificationError::Unsupported);
    }
    let route = RouteSlug::parse("capability-probe")
        .map_err(|_| CompatibleCapabilityCertificationError::InvalidResult)?;
    match (capability.operation, capability.mode) {
        (OperationKind::Generation, TransportMode::Unary | TransportMode::Streaming) => {
            // One capability gates both OpenAI generation entry points. Prove
            // both, otherwise a chat-only endpoint could be selected for a
            // Responses request (or vice versa).
            Ok(vec![
                generation_probe_operation(capability.mode, false)?,
                generation_probe_operation(capability.mode, true)?,
            ])
        }
        (OperationKind::Embeddings, TransportMode::Unary) => {
            Ok(vec![Operation::Embeddings(EmbeddingsRequest {
                route,
                input: vec![EmbeddingInput::Text("OLP capability probe".to_owned())],
                dimensions: None,
                extensions: SourceExtensions::default(),
            })])
        }
        (OperationKind::TokenCount, TransportMode::Unary) => {
            Ok(vec![Operation::TokenCount(TokenCountRequest {
                route,
                input: vec![ContentPart::Text {
                    text: "OLP capability probe".to_owned(),
                }],
                extensions: SourceExtensions::default(),
            })])
        }
        (OperationKind::Moderation, TransportMode::Unary) => {
            Ok(vec![Operation::Moderation(ModerationRequest {
                route,
                input: vec![ContentPart::Text {
                    text: "OLP capability probe".to_owned(),
                }],
                extensions: SourceExtensions::default(),
            })])
        }
        _ => Err(CompatibleCapabilityCertificationError::Unsupported),
    }
}

pub(crate) async fn execute_capability_probe(
    transport: &dyn ProviderTransport,
    provider_kind: ProviderKind,
    upstream_model: &str,
    capability: CompatibleCapability,
    operation: Operation,
) -> Result<(), CompatibleCapabilityCertificationError> {
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
            routing_id: TargetId::new(),
            provider_id: ProviderId::new(),
            provider_revision_id: uuid::Uuid::now_v7(),
            provider_kind,
            upstream_model: upstream_model.to_owned(),
            timeout: DurationMs::new(PROBE_TIMEOUT_MS),
            priority: 0,
        },
        operation: Arc::new(operation),
        media: None,
        max_inline_media_bytes: 1024 * 1024,
        propagate_trace_context: false,
    };
    let output = tokio::time::timeout(
        Duration::from_millis(PROBE_TIMEOUT_MS),
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

    match (capability.operation, output) {
        (OperationKind::Generation, ProviderOutput::Events(mut stream)) => {
            let mut validator = EventSequenceValidator::new();
            let deadline = tokio::time::Instant::now() + Duration::from_millis(PROBE_TIMEOUT_MS);
            let mut count = 0_usize;
            loop {
                let event = tokio::time::timeout_at(deadline, stream.next())
                    .await
                    .map_err(|_| CompatibleCapabilityCertificationError::Transport {
                        phase: TransportPhase::Body,
                        class: AttemptFailureClass::Timeout,
                    })?;
                let Some(event) = event else {
                    break;
                };
                if count >= MAX_PROBE_EVENTS {
                    return Err(CompatibleCapabilityCertificationError::InvalidResult);
                }
                let event =
                    event.map_err(|error| CompatibleCapabilityCertificationError::Transport {
                        phase: error.phase,
                        class: error.class,
                    })?;
                if matches!(event.kind, Kind::Error { .. }) {
                    return Err(CompatibleCapabilityCertificationError::InvalidResult);
                }
                validator
                    .push(&event)
                    .map_err(|_| CompatibleCapabilityCertificationError::InvalidResult)?;
                count = count.saturating_add(1);
                if provider_kind != ProviderKind::OpenAiCompatible && validator.is_complete() {
                    break;
                }
            }
            validator
                .finish()
                .map_err(|_| CompatibleCapabilityCertificationError::InvalidResult)
        }
        (OperationKind::Embeddings, ProviderOutput::Result(result)) if matches!(&*result, CanonicalResult::Embeddings(value) if !value.data.is_empty() && value.data.iter().all(|item| !item.values.is_empty())) => {
            Ok(())
        }
        (OperationKind::TokenCount, ProviderOutput::Result(result))
            if matches!(&*result, CanonicalResult::TokenCount(_)) =>
        {
            Ok(())
        }
        (OperationKind::Moderation, ProviderOutput::Result(result)) if matches!(&*result, CanonicalResult::Moderation(value) if !value.results.is_empty()) => {
            Ok(())
        }
        _ => Err(CompatibleCapabilityCertificationError::InvalidResult),
    }
}

#[cfg(test)]
mod tests;
