use super::*;

#[test]
fn upstream_errors_are_bounded_and_do_not_echo_unknown_body_fields() {
    let body = serde_json::json!({
        "error": {
            "message": "bad request for upstream-secret",
            "internal_secret": "must-not-leak"
        }
    });
    let message = safe_upstream_error_message(
        StatusCode::BAD_REQUEST,
        serde_json::to_vec(&body).unwrap().as_slice(),
        "upstream-secret",
    );
    assert!(message.contains("bad request"));
    assert!(message.contains("[REDACTED]"));
    assert!(!message.contains("upstream-secret"));
    assert!(!message.contains("must-not-leak"));
}

#[test]
fn transport_error_never_allows_failover_after_commit() {
    let error = transport_error(
        TransportPhase::Body,
        AttemptFailureClass::Timeout,
        true,
        "idle timeout",
    );
    assert!(!error.allows_failover());
}

#[tokio::test]
async fn status_errors_retain_bounded_retry_after_metadata() {
    let response = status_response_with_headers(
        "429 Too Many Requests",
        "application/json",
        &[("Retry-After", "41")],
        br#"{"error":{"message":"busy"}}"#,
    );
    let (base_url, _) = spawn_mock(MockResponse::immediate(response)).await;

    let error = execute_error(
        &test_connector(&base_url, ConnectorTimeouts::default()),
        fixture_request(false),
    )
    .await;

    assert_eq!(error.class, AttemptFailureClass::RateLimit);
    assert_eq!(error.retry_after, Some(Duration::from_secs(41)));
}

#[tokio::test]
async fn raw_media_stream_is_bounded_ordered_and_terminal() {
    let body = concat!(
        "event: image_generation.partial_image\n",
        "data: {\"type\":\"image_generation.partial_image\",\"b64_json\":\"YQ==\",\"partial_image_index\":0}\n\n",
        "event: image_generation.completed\n",
        "data: {\"type\":\"image_generation.completed\"}\n\n"
    );
    let headers =
        b"HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nConnection: close\r\n\r\n";
    let (base_url, _) = spawn_mock(MockResponse {
        chunks: vec![(
            Duration::ZERO,
            [headers.as_slice(), body.as_bytes()].concat(),
        )],
    })
    .await;
    let connector = test_connector(&base_url, ConnectorTimeouts::default());
    let mut events = execute_events(&connector, image_request(true)).await;
    let mut collected = Vec::new();
    while let Some(event) = events.next().await {
        collected.push(event.unwrap());
    }
    assert_eq!(collected.len(), 3);
    assert!(matches!(
        collected.last().map(|event| &event.kind),
        Some(CanonicalEventKind::Done)
    ));
    assert!(
        collected
            .iter()
            .enumerate()
            .all(|(index, event)| event.sequence == index as u64)
    );
}

#[tokio::test]
async fn raw_media_stream_rejects_malformed_conflicting_and_inconsistent_usage() {
    for frames in [
        vec![
            serde_json::json!({
                "type": "image_generation.partial_image",
                "usage": { "input_tokens": true }
            }),
            serde_json::json!({ "type": "image_generation.completed" }),
        ],
        vec![
            serde_json::json!({
                "type": "image_generation.partial_image",
                "usage": { "input_tokens": 2 }
            }),
            serde_json::json!({
                "type": "image_generation.completed",
                "usage": { "input_tokens": 3 }
            }),
        ],
        vec![serde_json::json!({
            "type": "image_generation.completed",
            "usage": { "input_tokens": 2, "output_tokens": 1, "total_tokens": 4 }
        })],
        vec![
            serde_json::json!({
                "type": "image_generation.partial_image",
                "usage": { "input_tokens": 2 }
            }),
            serde_json::json!({
                "type": "image_generation.completed",
                "usage": { "total_tokens": 1 }
            }),
        ],
        vec![
            serde_json::json!({
                "type": "image_generation.partial_image",
                "usage": { "total_tokens": 1 }
            }),
            serde_json::json!({
                "type": "image_generation.completed",
                "usage": { "output_tokens": 2 }
            }),
        ],
        vec![serde_json::json!({
            "type": "image_generation.completed",
            "usage": { "input_tokens": 2, "output_tokens": 2, "total_tokens": 3 }
        })],
        vec![serde_json::json!({
            "type": "image_generation.completed",
            "usage": {
                "total_tokens": 1,
                "input_tokens_details": { "cached_tokens": 2 }
            }
        })],
    ] {
        let body = frames
            .into_iter()
            .map(|frame| format!("data: {frame}\n\n"))
            .collect::<String>();
        let headers =
            b"HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nConnection: close\r\n\r\n";
        let (base_url, _) = spawn_mock(MockResponse::immediate(
            [headers.as_slice(), body.as_bytes()].concat(),
        ))
        .await;
        let connector = test_connector(&base_url, ConnectorTimeouts::default());
        let mut events = execute_events(&connector, image_request(true)).await;
        let failure = loop {
            match events.next().await {
                Some(Ok(_)) => continue,
                Some(Err(error)) => break error,
                None => panic!("invalid raw usage must fail the stream"),
            }
        };
        assert_eq!(failure.phase, TransportPhase::Body);
        assert_eq!(failure.class, AttemptFailureClass::Protocol);
    }
}

#[tokio::test]
async fn decodes_fragmented_streaming_chat_and_usage() {
    let sse = concat!(
        "data: {\"id\":\"chatcmpl-2\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"gpt-4o-mini\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"snow ☃\"},\"finish_reason\":null}]}\n\n",
        "data: {\"id\":\"chatcmpl-2\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"gpt-4o-mini\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":2,\"total_tokens\":4}}\n\n",
        "data: [DONE]\n\n"
    )
    .as_bytes()
    .to_vec();
    let snowman = find_bytes(&sse, "☃".as_bytes()).unwrap();
    let headers = b"HTTP/1.1 200 OK\r\nContent-Type: text/event-stream; charset=utf-8\r\nConnection: close\r\n\r\n";
    let (base_url, captured) = spawn_mock(MockResponse {
        chunks: vec![
            (
                Duration::ZERO,
                [headers.as_slice(), &sse[..snowman + 1]].concat(),
            ),
            (
                Duration::from_millis(5),
                sse[snowman + 1..snowman + 2].to_vec(),
            ),
            (Duration::from_millis(5), sse[snowman + 2..].to_vec()),
        ],
    })
    .await;
    let connector = test_connector(&base_url, ConnectorTimeouts::default());

    let mut events = execute_events(&connector, fixture_request(true)).await;
    let mut collected = Vec::new();
    while let Some(event) = events.next().await {
        collected.push(event.unwrap());
    }

    assert!(collected.iter().any(|event| matches!(
        &event.kind,
        CanonicalEventKind::TextDelta { text, .. } if text == "snow ☃"
    )));
    assert!(collected.iter().any(|event| matches!(
        &event.kind,
        CanonicalEventKind::Usage { usage } if usage.total_tokens == 4
    )));
    assert!(matches!(
        collected.last().map(|event| &event.kind),
        Some(CanonicalEventKind::Done)
    ));
    assert!(
        collected
            .windows(2)
            .all(|events| events[1].sequence == events[0].sequence + 1)
    );
    let captured = String::from_utf8(captured.await.unwrap()).unwrap();
    assert!(captured.contains("\"stream_options\":{\"include_usage\":true}"));
}

#[tokio::test]
async fn idle_timeout_after_commit_is_not_failover_eligible() {
    let first_event = b"data: {\"id\":\"chatcmpl-3\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"gpt-4o-mini\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"partial\"},\"finish_reason\":null}]}\n\n";
    let headers =
        b"HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nConnection: close\r\n\r\n";
    let (base_url, _) = spawn_mock(MockResponse {
        chunks: vec![
            (
                Duration::ZERO,
                [headers.as_slice(), first_event.as_slice()].concat(),
            ),
            (Duration::from_millis(150), b"data: [DONE]\n\n".to_vec()),
        ],
    })
    .await;
    let connector = test_connector(
        &base_url,
        ConnectorTimeouts {
            idle: Duration::from_millis(25),
            ..ConnectorTimeouts::default()
        },
    );

    let mut events = execute_events(&connector, fixture_request(true)).await;
    let mut failure = None;
    while let Some(event) = events.next().await {
        if let Err(error) = event {
            failure = Some(error);
            break;
        }
    }
    let failure = failure.expect("stream must time out while upstream is idle");
    assert_eq!(failure.phase, TransportPhase::Body);
    assert_eq!(failure.class, AttemptFailureClass::Timeout);
    assert!(failure.response_committed);
    assert!(!failure.allows_failover());
}

#[tokio::test]
async fn first_byte_timeout_is_classified_before_commit() {
    let (base_url, _) = spawn_mock(MockResponse {
        chunks: vec![(Duration::from_millis(150), Vec::new())],
    })
    .await;
    let connector = test_connector(
        &base_url,
        ConnectorTimeouts {
            first_byte: Duration::from_millis(25),
            ..ConnectorTimeouts::default()
        },
    );

    let failure = execute_error(&connector, fixture_request(false)).await;
    assert_eq!(failure.phase, TransportPhase::FirstByte);
    assert_eq!(failure.class, AttemptFailureClass::Timeout);
    assert!(!failure.response_committed);
    assert!(failure.allows_failover());
}

#[tokio::test]
async fn raw_media_delayed_headers_use_the_bounded_header_wait() {
    let (base_url, _) = spawn_mock(MockResponse {
        chunks: vec![(Duration::from_millis(150), Vec::new())],
    })
    .await;
    let connector = test_connector(
        &base_url,
        ConnectorTimeouts {
            first_byte: Duration::from_millis(25),
            ..ConnectorTimeouts::default()
        },
    );

    let failure = execute_error(&connector, image_request(true)).await;
    assert_eq!(failure.phase, TransportPhase::FirstByte);
    assert_eq!(failure.class, AttemptFailureClass::Timeout);
    assert!(!failure.response_committed);
}

#[tokio::test]
async fn binary_media_has_a_distinct_first_body_deadline_after_headers() {
    let headers = b"HTTP/1.1 200 OK\r\nContent-Type: audio/mpeg\r\nContent-Length: 9\r\nConnection: close\r\n\r\n";
    let (base_url, _) = spawn_mock(MockResponse {
        chunks: vec![
            (Duration::ZERO, headers.to_vec()),
            (Duration::from_millis(150), b"mp3-audio".to_vec()),
        ],
    })
    .await;
    let connector = test_connector(
        &base_url,
        ConnectorTimeouts {
            first_byte: Duration::from_millis(25),
            idle: Duration::from_secs(1),
            ..ConnectorTimeouts::default()
        },
    );
    let mut request = speech_request(false);
    request.media = Some(Arc::new(RecordingMediaSpool::default()));

    let failure = connector.execute(request).await.unwrap_err();
    assert_eq!(failure.phase, TransportPhase::FirstByte);
    assert_eq!(failure.class, AttemptFailureClass::Timeout);
    assert!(!failure.response_committed);
}

#[tokio::test]
async fn raw_sse_resets_idle_after_each_body_chunk() {
    let headers =
        b"HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nConnection: close\r\n\r\n";
    let partial = b"event: image_generation.partial_image\ndata: {\"type\":\"image_generation.partial_image\",\"b64_json\":\"YQ==\"}\n\n";
    let terminal =
        b"event: image_generation.completed\ndata: {\"type\":\"image_generation.completed\"}\n\n";
    let (base_url, _) = spawn_mock(MockResponse {
        chunks: vec![
            (
                Duration::ZERO,
                [headers.as_slice(), partial.as_slice()].concat(),
            ),
            (Duration::from_millis(150), terminal.to_vec()),
        ],
    })
    .await;
    let connector = test_connector(
        &base_url,
        ConnectorTimeouts {
            first_byte: Duration::from_secs(1),
            idle: Duration::from_millis(25),
            ..ConnectorTimeouts::default()
        },
    );

    let mut events = execute_events(&connector, image_request(true)).await;
    assert!(events.next().await.is_some_and(|event| event.is_ok()));
    let failure = match events.next().await {
        Some(Err(error)) => error,
        _ => panic!("raw media stream must enforce its resetting idle deadline"),
    };
    assert_eq!(failure.phase, TransportPhase::Body);
    assert_eq!(failure.class, AttemptFailureClass::Timeout);
    assert!(failure.response_committed);
}

#[tokio::test]
async fn multipart_first_byte_timeout_is_ambiguous_and_terminal() {
    let (base_url, _) = spawn_mock(MockResponse {
        chunks: vec![(Duration::from_millis(150), Vec::new())],
    })
    .await;
    let connector = test_connector(
        &base_url,
        ConnectorTimeouts {
            first_byte: Duration::from_millis(25),
            ..ConnectorTimeouts::default()
        },
    );

    let failure = connector
        .execute(transcription_request(false))
        .await
        .expect_err("multipart request must time out");
    assert_eq!(failure.phase, TransportPhase::Body);
    assert_eq!(failure.class, AttemptFailureClass::Ambiguous);
    assert!(failure.response_committed);
    assert!(!failure.allows_failover());
}

#[tokio::test]
async fn speech_binary_body_enforces_idle_deadline() {
    let headers = b"HTTP/1.1 200 OK\r\nContent-Type: audio/mpeg\r\nContent-Length: 9\r\nConnection: close\r\n\r\n";
    let (base_url, _) = spawn_mock(MockResponse {
        chunks: vec![
            (Duration::ZERO, [headers.as_slice(), b"mp3"].concat()),
            (Duration::from_millis(150), b"-audio".to_vec()),
        ],
    })
    .await;
    let connector = test_connector(
        &base_url,
        ConnectorTimeouts {
            idle: Duration::from_millis(25),
            ..ConnectorTimeouts::default()
        },
    );
    let mut request = speech_request(false);
    request.media = Some(Arc::new(RecordingMediaSpool::default()));

    let failure = connector
        .execute(request)
        .await
        .expect_err("stalled speech body must time out");
    assert_eq!(failure.phase, TransportPhase::Body);
    assert_eq!(failure.class, AttemptFailureClass::Timeout);
}

#[tokio::test]
async fn redirects_are_returned_as_errors_and_never_followed() {
    let response = b"HTTP/1.1 302 Found\r\nLocation: http://169.254.169.254/latest/meta-data\r\nContent-Length: 0\r\nConnection: close\r\n\r\n";
    let (base_url, _) = spawn_mock(MockResponse {
        chunks: vec![(Duration::ZERO, response.to_vec())],
    })
    .await;
    let connector = test_connector(&base_url, ConnectorTimeouts::default());

    let failure = execute_error(&connector, fixture_request(false)).await;
    assert_eq!(failure.class, AttemptFailureClass::UpstreamClient);
    assert!(!failure.response_committed);
}
