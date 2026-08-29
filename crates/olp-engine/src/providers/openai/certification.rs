use std::sync::Arc;
use std::{collections::BTreeMap, time::Duration};

use crate::domain::{
    canonical::{
        events::{EventSequenceValidator, Kind},
        identity::{OperationKind, RequestMetadata, Surface, TransportMode},
        requests::{
            ContentPart, EmbeddingInput, EmbeddingsRequest, GenerationParameters,
            GenerationRequest, Message, MessageRole, ModerationRequest, OPENAI_ENDPOINT_EXTENSION,
            Operation, SourceExtensions, TokenCountRequest,
        },
        results::CanonicalResult,
    },
    ids::{DurationMs, ProviderId, RequestId, RouteId, RouteSlug, RuntimeGenerationId, TargetId},
    ports::{
        AttemptFailureClass, ProviderOutput, ProviderRequest, ProviderTransport, TransportPhase,
    },
    routing::{provider::ProviderKind, selection::AttemptPlan},
};
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

pub(in crate::providers) async fn execute_capability_probe(
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
            provider_kind,
            upstream_model: upstream_model.to_owned(),
            timeout: DurationMs::new(PROBE_TIMEOUT_MS),
            priority: 0,
        },
        operation: Arc::new(operation),
        media: None,
        max_inline_media_bytes: 1024 * 1024,
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
mod tests {
    use std::time::Duration;

    use super::*;
    use crate::providers::mock_server::{MockResponse, response, spawn_mock, spawn_sequence};
    use crate::providers::openai::{ApiKey, ConnectorConfig, Timeouts};

    #[tokio::test]
    async fn genuine_unary_generation_probe_uses_inference_codec() {
        let body = serde_json::to_vec(&serde_json::json!({
            "id": "chatcmpl-certification",
            "object": "chat.completion",
            "created": 1,
            "model": "compatible-model",
            "choices": [{
                "index": 0,
                "message": {"role": "assistant", "content": "OK"},
                "finish_reason": "stop"
            }],
            "usage": {"prompt_tokens": 3, "completion_tokens": 1, "total_tokens": 4}
        }))
        .unwrap();
        let responses_body = serde_json::to_vec(&serde_json::json!({
            "id": "resp_certification",
            "object": "response",
            "created_at": 1,
            "status": "completed",
            "model": "compatible-model",
            "output": [{
                "id": "msg_certification",
                "type": "message",
                "role": "assistant",
                "status": "completed",
                "content": [{"type": "output_text", "text": "OK", "annotations": []}]
            }],
            "usage": {"input_tokens": 3, "output_tokens": 1, "total_tokens": 4}
        }))
        .unwrap();
        let (base_url, requests) = spawn_response_sequence(vec![
            ("application/json", body),
            ("application/json", responses_body),
        ])
        .await;
        let connector = connector(&base_url);
        connector
            .certify_compatible_capability(
                "compatible-model",
                CompatibleCapability {
                    operation: OperationKind::Generation,
                    surface: Surface::OpenAi,
                    mode: TransportMode::Unary,
                },
            )
            .await
            .unwrap();
        let requests: Vec<_> = requests.await.unwrap().into_iter().map(utf8).collect();
        assert!(requests[0].starts_with("POST /v1/chat/completions "));
        assert!(requests[1].starts_with("POST /v1/responses "));
        assert!(
            requests
                .iter()
                .all(|request| request.contains("\"model\":\"compatible-model\""))
        );
        assert!(requests[0].contains("\"max_completion_tokens\":1"));
        assert!(requests[1].contains("\"max_output_tokens\":1"));
        assert!(
            requests
                .iter()
                .all(|request| !request.contains("upstream-secret\""))
        );
    }

    #[tokio::test]
    async fn malformed_success_response_is_not_certified() {
        let (base_url, _) = spawn_json_response(br#"{"not":"a chat response"}"#.to_vec()).await;
        let error = connector(&base_url)
            .certify_compatible_capability(
                "compatible-model",
                CompatibleCapability {
                    operation: OperationKind::Generation,
                    surface: Surface::OpenAi,
                    mode: TransportMode::Unary,
                },
            )
            .await
            .unwrap_err();
        assert!(matches!(
            error,
            CompatibleCapabilityCertificationError::Transport {
                class: AttemptFailureClass::Protocol,
                ..
            }
        ));
    }

    #[tokio::test]
    async fn genuine_streaming_generation_probe_requires_valid_terminal_sse() {
        let body = concat!(
            "data: {\"id\":\"chatcmpl-cert\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"compatible-model\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"OK\"},\"finish_reason\":null}]}\n\n",
            "data: {\"id\":\"chatcmpl-cert\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"compatible-model\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":1,\"total_tokens\":4}}\n\n",
            "data: [DONE]\n\n"
        );
        let responses_body = concat!(
            "event: response.created\n",
            "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_cert\",\"model\":\"compatible-model\"}}\n\n",
            "event: response.output_text.delta\n",
            "data: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"delta\":\"OK\"}\n\n",
            "event: response.completed\n",
            "data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":3,\"output_tokens\":1,\"total_tokens\":4}}}\n\n"
        );
        let (base_url, requests) = spawn_response_sequence(vec![
            ("text/event-stream", body.as_bytes().to_vec()),
            ("text/event-stream", responses_body.as_bytes().to_vec()),
        ])
        .await;
        connector(&base_url)
            .certify_compatible_capability(
                "compatible-model",
                CompatibleCapability {
                    operation: OperationKind::Generation,
                    surface: Surface::OpenAi,
                    mode: TransportMode::Streaming,
                },
            )
            .await
            .unwrap();
        let requests: Vec<_> = requests.await.unwrap().into_iter().map(utf8).collect();
        assert!(requests[0].starts_with("POST /v1/chat/completions "));
        assert!(requests[1].starts_with("POST /v1/responses "));
        assert!(
            requests
                .iter()
                .all(|request| request.contains("\"stream\":true"))
        );
        assert!(requests[0].contains("\"include_usage\":true"));
    }

    #[tokio::test]
    async fn typed_unary_operations_are_certified_only_after_codec_validation() {
        for (operation, path, body) in [
            (
                OperationKind::Embeddings,
                "/v1/embeddings",
                serde_json::to_vec(&serde_json::json!({
                    "object": "list",
                    "model": "compatible-model",
                    "data": [{"object": "embedding", "index": 0, "embedding": [0.25]}],
                    "usage": {"prompt_tokens": 1, "total_tokens": 1}
                }))
                .unwrap(),
            ),
            (
                OperationKind::TokenCount,
                "/v1/responses/input_tokens",
                serde_json::to_vec(&serde_json::json!({
                    "object": "response.input_tokens",
                    "input_tokens": 4
                }))
                .unwrap(),
            ),
            (
                OperationKind::Moderation,
                "/v1/moderations",
                serde_json::to_vec(&serde_json::json!({
                    "id": "modr_cert",
                    "model": "compatible-model",
                    "results": [{
                        "flagged": false,
                        "categories": {"violence": false},
                        "category_scores": {"violence": 0.0}
                    }]
                }))
                .unwrap(),
            ),
        ] {
            let (base_url, request) = spawn_json_response(body).await;
            connector(&base_url)
                .certify_compatible_capability(
                    "compatible-model",
                    CompatibleCapability {
                        operation,
                        surface: Surface::OpenAi,
                        mode: TransportMode::Unary,
                    },
                )
                .await
                .unwrap();
            assert!(utf8(request.await.unwrap()).starts_with(&format!("POST {path} ")));
        }
    }

    #[tokio::test]
    async fn cross_protocol_and_media_tuples_fail_without_network_calls() {
        let connector = connector("http://127.0.0.1:9/v1/");
        for capability in [
            CompatibleCapability {
                operation: OperationKind::Generation,
                surface: Surface::Anthropic,
                mode: TransportMode::Unary,
            },
            CompatibleCapability {
                operation: OperationKind::ImageGeneration,
                surface: Surface::OpenAi,
                mode: TransportMode::Unary,
            },
        ] {
            assert_eq!(
                connector
                    .certify_compatible_capability("compatible-model", capability)
                    .await
                    .unwrap_err(),
                CompatibleCapabilityCertificationError::Unsupported
            );
        }
    }

    #[tokio::test]
    async fn native_media_contracts_require_exact_credentialed_discovery() {
        for capability in [
            CompatibleCapability {
                operation: OperationKind::ImageGeneration,
                surface: Surface::OpenAi,
                mode: TransportMode::Streaming,
            },
            CompatibleCapability {
                operation: OperationKind::VideoContent,
                surface: Surface::OpenAi,
                mode: TransportMode::Unary,
            },
        ] {
            let body = serde_json::to_vec(&serde_json::json!({
                "object": "list",
                "data": [
                    {"id": "other-model", "object": "model"},
                    {"id": "exact-native-model", "object": "model"}
                ]
            }))
            .unwrap();
            let (base_url, request) = spawn_json_response(body).await;
            let evidence = connector(&base_url)
                .certify_native_openai_capability("exact-native-model", capability)
                .await
                .unwrap();
            assert_eq!(
                evidence,
                NativeOpenAiCertificationEvidence::ModelDiscoveryAndConnectorContract
            );
            let request = utf8(request.await.unwrap());
            assert!(request.starts_with("GET /v1/models "));
            assert!(
                request
                    .to_ascii_lowercase()
                    .contains("authorization: bearer upstream-secret")
            );
        }

        let body = serde_json::to_vec(&serde_json::json!({
            "object": "list",
            "data": [{"id": "different-model", "object": "model"}]
        }))
        .unwrap();
        let (base_url, request) = spawn_json_response(body).await;
        assert_eq!(
            connector(&base_url)
                .certify_native_openai_capability(
                    "exact-native-model",
                    CompatibleCapability {
                        operation: OperationKind::Speech,
                        surface: Surface::OpenAi,
                        mode: TransportMode::Unary,
                    },
                )
                .await
                .unwrap_err(),
            CompatibleCapabilityCertificationError::ModelNotDiscovered
        );
        assert!(utf8(request.await.unwrap()).starts_with("GET /v1/models "));
    }

    #[tokio::test]
    async fn generic_cross_surface_and_unknown_native_tuples_fail_closed() {
        let connector = connector("http://127.0.0.1:9/v1/");
        let media = CompatibleCapability {
            operation: OperationKind::ImageEdit,
            surface: Surface::OpenAi,
            mode: TransportMode::Unary,
        };
        assert_eq!(
            connector
                .certify_compatible_capability("generic-model", media)
                .await
                .unwrap_err(),
            CompatibleCapabilityCertificationError::Unsupported
        );
        for capability in [
            CompatibleCapability {
                operation: OperationKind::ImageEdit,
                surface: Surface::Anthropic,
                mode: TransportMode::Unary,
            },
            CompatibleCapability {
                operation: OperationKind::ImageVariation,
                surface: Surface::OpenAi,
                mode: TransportMode::Streaming,
            },
            CompatibleCapability {
                operation: OperationKind::VideoCreate,
                surface: Surface::OpenAi,
                mode: TransportMode::Unary,
            },
        ] {
            assert_eq!(
                connector
                    .certify_native_openai_capability("native-model", capability)
                    .await
                    .unwrap_err(),
                CompatibleCapabilityCertificationError::Unsupported
            );
        }
    }

    #[test]
    fn native_discovery_contract_matrix_is_closed_and_mode_exact() {
        let supported = [
            (OperationKind::ImageGeneration, TransportMode::Unary),
            (OperationKind::ImageGeneration, TransportMode::Streaming),
            (OperationKind::ImageEdit, TransportMode::Unary),
            (OperationKind::ImageEdit, TransportMode::Streaming),
            (OperationKind::ImageVariation, TransportMode::Unary),
            (OperationKind::Speech, TransportMode::Unary),
            (OperationKind::Speech, TransportMode::Streaming),
            (OperationKind::Transcription, TransportMode::Unary),
            (OperationKind::Transcription, TransportMode::Streaming),
            (OperationKind::VideoCreate, TransportMode::Async),
            (OperationKind::VideoList, TransportMode::Unary),
            (OperationKind::VideoGet, TransportMode::Unary),
            (OperationKind::VideoContent, TransportMode::Unary),
            (OperationKind::VideoDelete, TransportMode::Unary),
        ];
        for (operation, mode) in supported {
            assert!(native_openai_discovery_contract(CompatibleCapability {
                operation,
                surface: Surface::OpenAi,
                mode,
            }));
        }
        assert!(!native_openai_discovery_contract(CompatibleCapability {
            operation: OperationKind::Moderation,
            surface: Surface::OpenAi,
            mode: TransportMode::Streaming,
        }));
        assert!(!native_openai_discovery_contract(CompatibleCapability {
            operation: OperationKind::ModelGet,
            surface: Surface::OpenAi,
            mode: TransportMode::Unary,
        }));
    }

    fn connector(base_url: &str) -> Connector {
        Connector::new(
            ConnectorConfig::for_local_test(
                base_url,
                Timeouts {
                    connect: Duration::from_secs(1),
                    first_byte: Duration::from_secs(1),
                    idle: Duration::from_secs(1),
                },
            ),
            ApiKey::new("upstream-secret").unwrap(),
        )
    }

    async fn spawn_json_response(
        body: Vec<u8>,
    ) -> (String, tokio::sync::oneshot::Receiver<Vec<u8>>) {
        spawn_response("application/json", body).await
    }

    async fn spawn_response(
        content_type: &'static str,
        body: Vec<u8>,
    ) -> (String, tokio::sync::oneshot::Receiver<Vec<u8>>) {
        spawn_mock(
            "/v1/",
            MockResponse::immediate(response(content_type, body)),
        )
        .await
    }

    async fn spawn_response_sequence(
        responses: Vec<(&'static str, Vec<u8>)>,
    ) -> (String, tokio::sync::oneshot::Receiver<Vec<Vec<u8>>>) {
        spawn_sequence(
            "/v1/",
            responses
                .into_iter()
                .map(|(content_type, body)| MockResponse::immediate(response(content_type, body)))
                .collect(),
        )
        .await
    }

    fn utf8(request: Vec<u8>) -> String {
        String::from_utf8(request).unwrap()
    }
}
