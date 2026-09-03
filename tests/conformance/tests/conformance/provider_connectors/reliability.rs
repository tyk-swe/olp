use std::{collections::BTreeMap, sync::Arc, time::Duration};

use olp_engine::domain::{
    canonical::{
        events::validate_event_sequence,
        identity::{OperationKind, Surface, TransportMode},
        requests::{
            ContentPart, MediaHandle, MediaSource, Operation, SourceExtensions,
            TranscriptionRequest,
        },
        results::CanonicalResult,
    },
    ids::{DurationMs, RouteSlug},
    ports::{AttemptFailureClass, ProviderOutput, TransportPhase},
    routing::provider::ProviderKind,
};
use olp_engine::providers::{
    openai::certification::CompatibleCapability,
    test_support::{API_KEY, BEDROCK_ACCESS_KEY, BEDROCK_SECRET_KEY, VERTEX_TOKEN, local_provider},
};
use serde_json::json;

use super::{fixtures::*, generation::assert_endpoint_and_auth, support::*};

#[tokio::test]
async fn all_connectors_redact_secrets_classify_retryability_and_keep_retry_after() {
    let cases = [
        (
            "429 Too Many Requests",
            AttemptFailureClass::RateLimit,
            true,
        ),
        (
            "503 Service Unavailable",
            AttemptFailureClass::UpstreamServer,
            true,
        ),
        (
            "400 Bad Request",
            AttemptFailureClass::UpstreamClient,
            false,
        ),
    ];
    for kind in ProviderKind::ALL {
        let reflected_secret = match kind {
            ProviderKind::VertexAi => VERTEX_TOKEN,
            ProviderKind::Bedrock => BEDROCK_SECRET_KEY,
            _ => API_KEY,
        };
        for (status, class, retryable) in cases {
            let response = classified_error_response(kind, status, reflected_secret);
            let (transport, server) = transport_at(kind, response).await;
            let error = transport
                .execute(generation_request(
                    kind,
                    native_surface(kind),
                    TransportMode::Unary,
                ))
                .await
                .expect_err("error status must fail");
            assert_eq!(error.class, class, "{kind:?} {status}");
            assert_eq!(error.allows_failover(), retryable, "{kind:?} {status}");
            assert_eq!(
                error.upstream.retry_after,
                Some(Duration::from_secs(7)),
                "{kind:?} {status} dropped the upstream Retry-After"
            );
            for secret in [
                API_KEY,
                VERTEX_TOKEN,
                BEDROCK_ACCESS_KEY,
                BEDROCK_SECRET_KEY,
            ] {
                assert!(
                    !error.message.contains(secret),
                    "{kind:?} leaked secret in error"
                );
                assert!(
                    !format!("{error:?}").contains(secret),
                    "{kind:?} leaked secret in Debug"
                );
            }
            assert!(format!("{error:?}").contains("[REDACTED]"));
            server.await.unwrap();
        }
    }
}

#[tokio::test]
async fn all_connectors_propagate_attempt_deadlines() {
    for kind in ProviderKind::ALL {
        let (origin, accepted, connection) = spawn_stalling_server().await;
        let provider = local_provider(kind, &origin).await.unwrap();
        let mut request = generation_request(kind, native_surface(kind), TransportMode::Unary);
        request.attempt.timeout = DurationMs::new(250);
        let transport = provider.into_transport();
        let execute = tokio::spawn(async move { transport.execute(request).await });
        tokio::time::timeout(Duration::from_secs(1), accepted)
            .await
            .unwrap_or_else(|_| panic!("{kind:?}: connector request was not sent"))
            .expect("connector request was not sent");
        let error = tokio::time::timeout(Duration::from_secs(1), execute)
            .await
            .unwrap_or_else(|_| panic!("{kind:?}: connector ignored the attempt deadline"))
            .unwrap()
            .expect_err("stalled provider must hit the attempt deadline");
        assert_eq!(
            error.class,
            AttemptFailureClass::Timeout,
            "{kind:?}: {error:?}"
        );
        let expected_phase = if kind == ProviderKind::Bedrock {
            TransportPhase::Body
        } else {
            TransportPhase::FirstByte
        };
        assert_eq!(error.phase, expected_phase, "{kind:?}: {error:?}");
        connection.abort();
    }
}

#[tokio::test]
async fn dropping_execute_cancels_every_in_flight_connector_request() {
    for kind in ProviderKind::ALL {
        let (origin, accepted, connection) = spawn_stalling_server().await;
        let provider = local_provider(kind, &origin).await.unwrap();
        let transport = provider.into_transport();
        let request = generation_request(kind, native_surface(kind), TransportMode::Unary);
        let execute = tokio::spawn(async move { transport.execute(request).await });
        accepted.await.expect("connector request was not sent");
        execute.abort();
        let _ = execute.await;
        let closed = tokio::time::timeout(Duration::from_secs(1), connection)
            .await
            .expect("cancelled request must close its socket")
            .unwrap()
            .unwrap();
        assert_eq!(closed, 0, "{kind:?}: no bytes after request body");
    }
}

#[tokio::test]
async fn dropping_stream_after_handoff_closes_every_connector_socket() {
    for kind in ProviderKind::ALL {
        let (origin, connection) =
            spawn_streaming_handoff_server(stream_handoff_response(kind)).await;
        let provider = local_provider(kind, &origin).await.unwrap();
        let output = provider
            .into_transport()
            .execute(generation_request(
                kind,
                native_surface(kind),
                TransportMode::Streaming,
            ))
            .await
            .unwrap();
        let ProviderOutput::Events(events) = output else {
            panic!("{kind:?} did not return a stream");
        };
        drop(events);

        let (captured, bytes_after_request) =
            tokio::time::timeout(Duration::from_secs(1), connection)
                .await
                .expect("dropping a handed-off stream must close its socket")
                .unwrap();
        assert_endpoint_and_auth(kind, TransportMode::Streaming, &captured);
        assert_eq!(
            bytes_after_request, 0,
            "{kind:?}: no bytes after request body"
        );
    }
}

#[tokio::test]
async fn all_streaming_connectors_reject_truncated_event_sequences() {
    for kind in ProviderKind::ALL {
        let (transport, server) = transport_at(kind, truncated_stream_response(kind)).await;
        let output = transport
            .execute(generation_request(
                kind,
                native_surface(kind),
                TransportMode::Streaming,
            ))
            .await
            .unwrap();
        let error = collect_events(output)
            .await
            .expect_err("stream without a vendor terminal event must fail");
        assert_eq!(
            error.class,
            AttemptFailureClass::Protocol,
            "{kind:?}: {error:?}"
        );
        assert!(
            error.response_committed,
            "{kind:?}: partial output commits response"
        );
        server.await.unwrap();
    }
}

#[tokio::test]
async fn all_streaming_connectors_reject_misordered_event_sequences() {
    for kind in ProviderKind::ALL {
        let (transport, server) = transport_at(kind, invalid_ordered_stream_response(kind)).await;
        let output = transport
            .execute(generation_request(
                kind,
                native_surface(kind),
                TransportMode::Streaming,
            ))
            .await
            .unwrap();
        let error = collect_events(output)
            .await
            .expect_err("misordered vendor stream must fail");
        assert_eq!(
            error.class,
            AttemptFailureClass::Protocol,
            "{kind:?}: {error:?}"
        );
        assert_eq!(error.phase, TransportPhase::Body, "{kind:?}: {error:?}");
        server.await.unwrap();
    }
}

#[tokio::test]
async fn all_unary_connectors_fail_closed_on_empty_malformed_and_truncated_bodies() {
    for kind in ProviderKind::ALL {
        for case in ["empty", "malformed", "truncated"] {
            let (transport, server) = transport_at(kind, invalid_body_response(case)).await;
            let error = transport
                .execute(generation_request(
                    kind,
                    native_surface(kind),
                    TransportMode::Unary,
                ))
                .await
                .expect_err("invalid body must fail");
            if case == "truncated" {
                assert!(
                    matches!(
                        error.class,
                        AttemptFailureClass::Protocol | AttemptFailureClass::Connect
                    ),
                    "{kind:?} {case}: {error:?}"
                );
            } else {
                assert_eq!(
                    error.class,
                    AttemptFailureClass::Protocol,
                    "{kind:?} {case}: {error:?}"
                );
            }
            assert!(!error.response_committed, "{kind:?} {case}");
            server.await.unwrap();
        }
    }
}

#[tokio::test]
async fn all_generation_connectors_hydrate_bounded_inline_media() {
    use super::matrix::{Disposition, ROWS};

    for row in ROWS {
        let kind = row.kind;
        if let Disposition::Inapplicable(_) = row.media {
            continue;
        }
        let openai_like = matches!(
            kind,
            ProviderKind::OpenAi | ProviderKind::OpenAiCompatible | ProviderKind::AzureOpenAi
        );
        let response = if openai_like {
            http_response("200 OK", "application/json", r#"{"text":"hello back"}"#)
        } else {
            unary_response(kind)
        };
        let (transport, server) = transport_at(kind, response).await;
        let mut request = generation_request(kind, native_surface(kind), TransportMode::Unary);
        if openai_like {
            request.metadata.operation = OperationKind::Transcription;
            request.operation = Arc::new(Operation::Transcription(TranscriptionRequest {
                route: RouteSlug::parse("transcription").unwrap(),
                audio: MediaHandle::new("conformance-audio"),
                language: Some("en".to_owned()),
                prompt: None,
                stream: false,
                extensions: SourceExtensions::new(Surface::OpenAi, BTreeMap::new()),
            }));
            request.media = Some(Arc::new(InlineMediaSpool {
                filename: "sample.wav",
                content_type: "audio/wav",
                data: b"wave-data",
            }));
            let output = transport.execute(request).await.unwrap();
            assert!(matches!(
                output,
                ProviderOutput::Result(result)
                    if matches!(*result, CanonicalResult::Transcription(_))
            ));
        } else {
            let Operation::Generation(generation) = Arc::make_mut(&mut request.operation) else {
                unreachable!()
            };
            // The MIME extension below still has to work for a same-surface
            // request, so the canonical field is deliberately left unset here.
            generation.messages[0].content.push(ContentPart::Image {
                source: MediaSource::Handle(MediaHandle::new("conformance-image")),
                detail: None,
                mime_type: None,
            });
            let mime_path = match kind {
                ProviderKind::Anthropic => "/messages/0/content/1/source/media_type",
                ProviderKind::Gemini | ProviderKind::VertexAi => {
                    "/contents/0/parts/1/inlineData/mimeType"
                }
                _ => unreachable!(),
            };
            generation
                .extensions
                .values
                .insert(mime_path.to_owned(), json!("image/png"));
            request.media = Some(Arc::new(InlineMediaSpool {
                filename: "pixel.png",
                content_type: "image/png",
                data: b"hi",
            }));
            let events = collect_events(transport.execute(request).await.unwrap())
                .await
                .unwrap();
            validate_event_sequence(&events).unwrap();
        }
        let captured = server.await.unwrap();
        if openai_like {
            assert!(captured.body_text().contains("wave-data"), "{kind:?}");
            assert!(captured.head.contains("multipart/form-data"), "{kind:?}");
        } else {
            assert!(captured.body_text().contains("aGk="), "{kind:?}");
            assert!(captured.body_text().contains("image/png"), "{kind:?}");
        }
    }
}

#[tokio::test]
async fn bounded_http_connectors_reject_oversized_unary_responses() {
    use super::matrix::{Disposition, ROWS};

    const OVERSIZED: usize = 16 * 1024 * 1024 + 1;
    for row in ROWS {
        let kind = row.kind;
        if let Disposition::Inapplicable(_) = row.oversized_responses {
            continue;
        }
        let response = http_response("200 OK", "application/json", vec![b'x'; OVERSIZED]);
        let (transport, server) = transport_at(kind, response).await;
        let error = transport
            .execute(generation_request(
                kind,
                native_surface(kind),
                TransportMode::Unary,
            ))
            .await
            .expect_err("oversized response must fail before decoding");
        assert_eq!(error.phase, TransportPhase::Body, "{kind:?}: {error:?}");
        assert_eq!(
            error.class,
            AttemptFailureClass::Protocol,
            "{kind:?}: {error:?}"
        );
        assert!(!error.response_committed, "{kind:?}");
        server.await.unwrap();
    }
}

#[tokio::test]
async fn bounded_http_connectors_reject_oversized_streaming_events() {
    use super::matrix::{Disposition, ROWS};

    for row in ROWS {
        let kind = row.kind;
        if let Disposition::Inapplicable(_) = row.oversized_responses {
            continue;
        }
        let (transport, server) = transport_at(kind, oversized_stream_response()).await;
        let output = transport
            .execute(generation_request(
                kind,
                native_surface(kind),
                TransportMode::Streaming,
            ))
            .await
            .unwrap();
        let error = collect_events(output)
            .await
            .expect_err("oversized streaming event must fail before decoding");
        assert_eq!(error.phase, TransportPhase::Body, "{kind:?}: {error:?}");
        assert_eq!(
            error.class,
            AttemptFailureClass::Protocol,
            "{kind:?}: {error:?}"
        );
        assert!(
            error.message.contains("SSE event exceeds"),
            "{kind:?}: {error:?}"
        );
        assert!(!error.response_committed, "{kind:?}");
        server.await.unwrap();
    }
}

#[tokio::test]
async fn every_reviewed_capability_tuple_executes_its_certification_contract() {
    for kind in ProviderKind::ALL {
        for (operation, surface, mode) in super::matrix::expected_certifiable_capabilities(kind) {
            let capability = CompatibleCapability {
                operation,
                surface,
                mode,
            };
            let (origin, shutdown, server) = spawn_certification_emulator(kind).await;
            let provider = local_provider(kind, &origin)
                .await
                .expect("assemble local certification connector");
            let upstream_model = if kind == ProviderKind::AzureOpenAi {
                "conformance-deployment"
            } else {
                MODEL
            };
            provider
                .certify_capability(upstream_model, capability)
                .await
                .unwrap_or_else(|error| {
                    panic!(
                        "{kind:?} {operation:?} {surface:?} {mode:?} certification failed: {error}"
                    )
                });
            shutdown.send(()).unwrap();
            let captured = server.await.unwrap();
            assert!(
                !captured.is_empty(),
                "{kind:?} {operation:?} {surface:?} {mode:?} made no certification probe"
            );
        }
    }
}
