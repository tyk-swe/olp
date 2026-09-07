use std::time::Duration;

use crate::providers::mock_server::MockResponse;
use crate::providers::mock_server::response;
use crate::providers::mock_server::spawn_mock;
use crate::providers::mock_server::spawn_sequence;
use crate::providers::openai::ApiKey;
use crate::providers::openai::ConnectorConfig;
use crate::providers::openai::Timeouts;
use crate::providers::openai::certification::*;

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

async fn spawn_json_response(body: Vec<u8>) -> (String, tokio::sync::oneshot::Receiver<Vec<u8>>) {
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
