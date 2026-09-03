use std::sync::Arc;

use olp_engine::domain::{
    canonical::{
        events::{Event, Kind, Usage, validate_event_sequence},
        identity::TransportMode,
        requests::{Operation, ResponseFormat},
    },
    ports::{AttemptFailureClass, TransportPhase},
    routing::provider::ProviderKind,
};
use olp_engine::providers::test_support::{
    API_KEY, BEDROCK_ACCESS_KEY, BEDROCK_SECRET_KEY, VERTEX_TOKEN, local_provider,
};
use serde_json::{Value, json};
use tokio::net::TcpListener;

use super::{fixtures::*, support::*};

#[tokio::test]
async fn all_connectors_execute_unary_generation_with_reviewed_endpoint_and_auth() {
    use super::matrix::{Disposition, row_for};

    for kind in ProviderKind::ALL {
        let (transport, server) = transport_at(kind, unary_response(kind)).await;
        let events = collect_events(
            transport
                .execute(generation_request(
                    kind,
                    native_surface(kind),
                    TransportMode::Unary,
                ))
                .await
                .unwrap(),
        )
        .await
        .unwrap();
        validate_event_sequence(&events).unwrap();
        assert!(
            events.iter().any(|event| matches!(
                &event.kind,
                Kind::TextDelta { text, .. } if text == "hello back"
            )),
            "{kind:?}"
        );
        assert_usage(kind, &events);
        assert_response_id(kind, &events);

        let captured = server.await.unwrap();
        assert_endpoint_and_auth(kind, TransportMode::Unary, &captured);
        assert_eq!(
            captured.header("x-request-id"),
            match row_for(kind).request_ids {
                Disposition::SharedContract => Some(REQUEST_ID),
                Disposition::Inapplicable(_) => None,
            },
            "{kind:?} request ID placement"
        );
    }
}

pub(super) fn assert_usage(kind: ProviderKind, events: &[Event]) {
    use super::matrix::{Disposition, row_for};

    let usage = events.iter().find_map(|event| match event.kind {
        Kind::Usage { usage } => Some(usage),
        _ => None,
    });
    let cached_input_tokens = match row_for(kind).cached_usage {
        Disposition::SharedContract => Some(3),
        Disposition::Inapplicable(_) => None,
    };
    assert_eq!(
        usage,
        Some(Usage {
            input_tokens: 5,
            output_tokens: 2,
            total_tokens: 7,
            cached_input_tokens,
            reasoning_tokens: None,
        }),
        "{kind:?} usage"
    );
}

pub(super) fn assert_response_id(kind: ProviderKind, events: &[Event]) {
    use super::matrix::{Disposition, row_for};

    let id = events.iter().find_map(|event| match &event.kind {
        Kind::ResponseStart { response_id, .. } => response_id.as_deref(),
        _ => None,
    });
    assert_eq!(
        id,
        match row_for(kind).request_ids {
            Disposition::SharedContract => Some("provider-response-id"),
            Disposition::Inapplicable(_) => None,
        },
        "{kind:?} provider response ID"
    );
}

pub(super) fn assert_endpoint_and_auth(
    kind: ProviderKind,
    mode: TransportMode,
    request: &CapturedRequest,
) {
    let streaming = mode == TransportMode::Streaming;
    let (path_fragment, auth_name) = match kind {
        ProviderKind::OpenAi | ProviderKind::OpenAiCompatible => {
            ("/v1/chat/completions", "authorization")
        }
        ProviderKind::Anthropic => ("/v1/messages", "x-api-key"),
        ProviderKind::Gemini if streaming => (
            "/v1beta/models/conformance-model:streamGenerateContent?alt=sse",
            "x-goog-api-key",
        ),
        ProviderKind::Gemini => (
            "/v1beta/models/conformance-model:generateContent",
            "x-goog-api-key",
        ),
        ProviderKind::VertexAi if streaming => (
            "/v1/projects/conformance-project/locations/us-central1/publishers/google/models/conformance-model:streamGenerateContent?alt=sse",
            "authorization",
        ),
        ProviderKind::VertexAi => (
            "/v1/projects/conformance-project/locations/us-central1/publishers/google/models/conformance-model:generateContent",
            "authorization",
        ),
        ProviderKind::Bedrock => (
            if streaming {
                "/model/conformance-model/converse-stream"
            } else {
                "/model/conformance-model/converse"
            },
            "authorization",
        ),
        ProviderKind::AzureOpenAi => (
            "/openai/deployments/conformance-deployment/chat/completions?api-version=2024-10-21",
            "api-key",
        ),
    };
    assert_eq!(request.path(), path_fragment, "{kind:?} endpoint");
    let auth = request
        .header(auth_name)
        .unwrap_or_else(|| panic!("{kind:?} missing {auth_name} header: {}", request.head));
    match kind {
        ProviderKind::OpenAi | ProviderKind::OpenAiCompatible => {
            assert_eq!(auth, format!("Bearer {API_KEY}"), "{kind:?}");
        }
        ProviderKind::VertexAi => {
            assert_eq!(auth, format!("Bearer {VERTEX_TOKEN}"), "{kind:?}");
        }
        ProviderKind::Bedrock => {
            assert!(
                auth.contains(BEDROCK_ACCESS_KEY),
                "{kind:?} authentication placement: {}",
                request.head
            );
        }
        ProviderKind::Anthropic | ProviderKind::Gemini | ProviderKind::AzureOpenAi => {
            assert_eq!(auth, API_KEY, "{kind:?}");
        }
    }
    assert!(!request.head.contains(BEDROCK_SECRET_KEY));
    if kind == ProviderKind::Gemini {
        assert!(
            !request.path().contains("key="),
            "Gemini key must stay out of URL"
        );
    }
}

#[tokio::test]
async fn all_connectors_stream_valid_terminal_sequences_and_usage() {
    for kind in ProviderKind::ALL {
        let (transport, server) = transport_at(kind, streaming_response(kind)).await;
        let events = collect_events(
            transport
                .execute(generation_request(
                    kind,
                    native_surface(kind),
                    TransportMode::Streaming,
                ))
                .await
                .unwrap(),
        )
        .await
        .unwrap();
        validate_event_sequence(&events).unwrap();
        assert!(
            matches!(events.last().map(|event| &event.kind), Some(Kind::Done)),
            "{kind:?}"
        );
        assert!(
            events.iter().any(|event| matches!(
                &event.kind,
                Kind::TextDelta { text, .. } if text == "hello back"
            )),
            "{kind:?}"
        );
        assert_usage(kind, &events);
        assert_response_id(kind, &events);
        let captured = server.await.unwrap();
        assert_endpoint_and_auth(kind, TransportMode::Streaming, &captured);
        match kind {
            ProviderKind::OpenAi | ProviderKind::OpenAiCompatible | ProviderKind::AzureOpenAi => {
                assert!(captured.body_text().contains("\"include_usage\":true"));
            }
            _ => {}
        }
    }
}

#[tokio::test]
async fn all_connectors_round_trip_advertised_tools() {
    for kind in ProviderKind::ALL {
        let (transport, server) = transport_at(kind, tool_response(kind)).await;
        let events = collect_events(transport.execute(tool_request(kind)).await.unwrap())
            .await
            .unwrap();
        validate_event_sequence(&events).unwrap();
        assert!(
            events.iter().any(|event| matches!(
                &event.kind,
                Kind::ToolCallDelta {
                    name: Some(name),
                    arguments_delta,
                    ..
                } if name == "weather" && arguments_delta.contains("Paris")
            )),
            "{kind:?}: {events:?}"
        );
        if !matches!(kind, ProviderKind::Gemini | ProviderKind::VertexAi) {
            assert!(
                events.iter().any(|event| matches!(
                    &event.kind,
                    Kind::ToolCallDelta { id: Some(id), .. } if id == "call-1"
                )),
                "{kind:?}: provider tool ID"
            );
        }
        let captured = server.await.unwrap();
        assert!(captured.body_text().contains("weather"), "{kind:?}");
        assert!(captured.body_text().contains("city"), "{kind:?}");
    }
}

#[tokio::test]
async fn structured_output_is_exercised_or_rejected_exactly_as_reviewed() {
    use super::matrix::{Disposition, ROWS};

    const SCHEMA_ONLY_PROPERTY: &str = "schema_only_conformance_sentinel";
    for row in ROWS {
        let kind = row.kind;
        let mut request = generation_request(kind, native_surface(kind), TransportMode::Unary);
        let Operation::Generation(generation) = Arc::make_mut(&mut request.operation) else {
            unreachable!()
        };
        generation.response_format = Some(ResponseFormat::JsonSchema {
            name: "conformance_schema".to_owned(),
            description: Some("A bounded answer".to_owned()),
            schema: json!({
                "type": "object",
                "properties": {SCHEMA_ONLY_PROPERTY: {"type": "string"}},
                "required": [SCHEMA_ONLY_PROPERTY],
                "additionalProperties": false
            }),
            strict: Some(true),
        });

        match row.structured_output {
            Disposition::SharedContract => {
                let (transport, server) = transport_at(kind, unary_response(kind)).await;
                let events = collect_events(transport.execute(request).await.unwrap())
                    .await
                    .unwrap();
                validate_event_sequence(&events).unwrap();
                let captured = server.await.unwrap();
                let body: Value = serde_json::from_slice(&captured.body).unwrap();
                if matches!(kind, ProviderKind::Gemini | ProviderKind::VertexAi) {
                    assert_eq!(
                        body["generationConfig"]["responseMimeType"], "application/json",
                        "{kind:?}"
                    );
                    assert_eq!(
                        body["generationConfig"]["responseSchema"]["properties"]
                            [SCHEMA_ONLY_PROPERTY]["type"],
                        "string",
                        "{kind:?}"
                    );
                } else {
                    assert_eq!(body["response_format"]["type"], "json_schema", "{kind:?}");
                    assert_eq!(
                        body["response_format"]["json_schema"]["strict"], true,
                        "{kind:?}"
                    );
                    assert_eq!(
                        body["response_format"]["json_schema"]["schema"]["properties"]
                            [SCHEMA_ONLY_PROPERTY]["type"],
                        "string",
                        "{kind:?}"
                    );
                }
            }
            Disposition::Inapplicable(_) => {
                let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
                let origin = format!("http://{}", listener.local_addr().unwrap());
                let provider = local_provider(kind, &origin).await.unwrap();
                let error = provider
                    .into_transport()
                    .execute(request)
                    .await
                    .expect_err("reviewed inapplicable format must fail closed");
                assert_eq!(error.class, AttemptFailureClass::Protocol, "{kind:?}");
                assert_eq!(error.phase, TransportPhase::Connect, "{kind:?}");
                drop(listener);
            }
        }
    }
}
