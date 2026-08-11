use super::*;

#[tokio::test]
async fn speech_unary_spools_bounded_audio_and_streaming_preserves_sse() {
    let spool = Arc::new(RecordingMediaSpool::default());
    let (base_url, captured) = spawn_mock(MockResponse {
        chunks: vec![(Duration::ZERO, http_response("audio/mpeg", b"mp3-audio"))],
    })
    .await;
    let connector = test_connector(&base_url, ConnectorTimeouts::default());
    let mut request = speech_request(false);
    request.media = Some(spool.clone());
    let ProviderOutput::Result(result) = connector.execute(request).await.unwrap() else {
        panic!("unary speech returned a stream")
    };
    let CanonicalResult::Speech(result) = *result else {
        panic!("speech returned the wrong result kind")
    };
    assert_eq!(result.audio.handle.as_str(), "recorded-0");
    assert_eq!(result.audio.content_type.as_deref(), Some("audio/mpeg"));
    assert_eq!(result.audio.content_length, Some(9));
    {
        let uploads = spool.uploads.lock().unwrap();
        assert_eq!(uploads.len(), 1);
        assert_eq!(uploads[0].filename, "speech-output.bin");
        assert_eq!(uploads[0].content_type.as_deref(), Some("audio/mpeg"));
        assert_eq!(uploads[0].maximum_length, DEFAULT_MAX_RESPONSE_BYTES as u64);
        assert_eq!(uploads[0].bytes, b"mp3-audio");
    }
    let captured = String::from_utf8(captured.await.unwrap()).unwrap();
    assert!(captured.starts_with("POST /v1/audio/speech HTTP/1.1"));
    assert_bearer_auth(&captured);
    assert!(
        captured
            .to_ascii_lowercase()
            .contains("content-type: application/json")
    );
    assert!(captured.contains("\"model\":\"gpt-4o-mini-tts\""));
    assert!(captured.contains("\"response_format\":\"mp3\""));

    let sse = concat!(
        "event: speech.audio.delta\n",
        "data: {\"type\":\"speech.audio.delta\",\"audio\":\"bXAz\"}\n\n",
        "event: speech.audio.done\n",
        "data: {\"type\":\"speech.audio.done\"}\n\n"
    );
    let (base_url, captured) = spawn_mock(MockResponse {
        chunks: vec![(
            Duration::ZERO,
            http_response("text/event-stream", sse.as_bytes()),
        )],
    })
    .await;
    let connector = test_connector(&base_url, ConnectorTimeouts::default());
    let mut events = execute_events(&connector, speech_request(true)).await;
    let mut collected = Vec::new();
    while let Some(event) = events.next().await {
        collected.push(event.unwrap());
    }
    assert!(collected.iter().any(|event| matches!(
        &event.kind,
        CanonicalEventKind::SourceExtension { extensions }
            if extensions.values["/__olp/raw_sse/event"] == "speech.audio.delta"
    )));
    assert!(matches!(
        collected.last().map(|event| &event.kind),
        Some(CanonicalEventKind::Done)
    ));
    let captured = String::from_utf8(captured.await.unwrap()).unwrap();
    assert!(captured.starts_with("POST /v1/audio/speech HTTP/1.1"));
    assert_bearer_auth(&captured);
    assert!(captured.contains("\"stream_format\":\"sse\""));
}

#[tokio::test]
async fn transcription_unary_and_streaming_forward_bounded_multipart_audio() {
    let response = serde_json::to_vec(&serde_json::json!({
        "text": "hello Ada",
        "language": "en",
        "duration": 1.5,
        "segments": [{"id": 0, "start": 0.0, "end": 1.5, "text": "hello Ada"}]
    }))
    .unwrap();
    let (base_url, captured) = spawn_mock(MockResponse {
        chunks: vec![(Duration::ZERO, http_response("application/json", &response))],
    })
    .await;
    let connector = test_connector(&base_url, ConnectorTimeouts::default());
    let ProviderOutput::Result(result) = connector
        .execute(transcription_request(false))
        .await
        .unwrap()
    else {
        panic!("unary transcription returned a stream")
    };
    let CanonicalResult::Transcription(result) = *result else {
        panic!("transcription returned the wrong result kind")
    };
    assert_eq!(result.text, "hello Ada");
    assert_eq!(result.language.as_deref(), Some("en"));
    assert_eq!(result.segments.len(), 1);
    let captured = String::from_utf8(captured.await.unwrap()).unwrap();
    assert!(captured.starts_with("POST /v1/audio/transcriptions HTTP/1.1"));
    assert_bearer_auth(&captured);
    assert!(
        captured
            .to_ascii_lowercase()
            .contains("content-type: multipart/form-data; boundary=")
    );
    assert!(captured.contains("name=\"file\"; filename=\"sample.wav\""));
    assert!(
        captured
            .to_ascii_lowercase()
            .contains("content-type: audio/wav")
    );
    assert!(captured.contains("wave-data"));
    assert!(captured.contains("name=\"language\""));
    assert!(captured.contains("name=\"prompt\""));
    assert!(captured.contains("name=\"stream\""));
    assert!(captured.contains("\r\n\r\nfalse\r\n"));

    let sse = concat!(
        "event: transcript.text.delta\n",
        "data: {\"type\":\"transcript.text.delta\",\"delta\":\"hello\"}\n\n",
        "event: transcript.text.done\n",
        "data: {\"type\":\"transcript.text.done\",\"text\":\"hello\"}\n\n"
    );
    let (base_url, captured) = spawn_mock(MockResponse {
        chunks: vec![(
            Duration::ZERO,
            http_response("text/event-stream", sse.as_bytes()),
        )],
    })
    .await;
    let connector = test_connector(&base_url, ConnectorTimeouts::default());
    let mut events = execute_events(&connector, transcription_request(true)).await;
    let mut collected = Vec::new();
    while let Some(event) = events.next().await {
        collected.push(event.unwrap());
    }
    assert!(matches!(
        collected.last().map(|event| &event.kind),
        Some(CanonicalEventKind::Done)
    ));
    assert!(
        collected
            .windows(2)
            .all(|events| { events[1].sequence == events[0].sequence.saturating_add(1) })
    );
    let captured = String::from_utf8(captured.await.unwrap()).unwrap();
    assert!(captured.starts_with("POST /v1/audio/transcriptions HTTP/1.1"));
    assert_bearer_auth(&captured);
    assert!(captured.contains("name=\"stream\""));
    assert!(captured.contains("\r\n\r\ntrue\r\n"));
}

#[tokio::test]
async fn transcription_text_formats_and_known_speakers_use_current_multipart_contract() {
    let (base_url, _) = spawn_mock(MockResponse {
        chunks: vec![(
            Duration::ZERO,
            http_response(
                "application/x-subrip",
                b"1\n00:00:00,000 --> 00:00:01,000\nhello\n",
            ),
        )],
    })
    .await;
    let connector = test_connector(&base_url, ConnectorTimeouts::default());
    let mut request = transcription_request(false);
    let Operation::Transcription(operation) = &mut request.operation else {
        unreachable!()
    };
    operation.extensions = SourceExtensions::new(
        Surface::OpenAi,
        BTreeMap::from([("/response_format".into(), serde_json::json!("srt"))]),
    );
    let ProviderOutput::Result(result) = connector.execute(request).await.unwrap() else {
        panic!("SRT transcription returned a stream")
    };
    let CanonicalResult::Transcription(result) = *result else {
        panic!("SRT transcription returned the wrong result")
    };
    assert!(result.text.contains("00:00:00,000"));

    let response = serde_json::to_vec(&serde_json::json!({
        "text": "hello",
        "segments": [{"id": 0, "start": 0.0, "end": 1.0, "text": "hello", "speaker": "agent"}]
    }))
    .unwrap();
    let (base_url, captured) = spawn_mock(MockResponse {
        chunks: vec![(Duration::ZERO, http_response("application/json", &response))],
    })
    .await;
    let connector = test_connector(&base_url, ConnectorTimeouts::default());
    let mut request = transcription_request(false);
    request.attempt.upstream_model = "gpt-4o-transcribe-diarize".into();
    let Operation::Transcription(operation) = &mut request.operation else {
        unreachable!()
    };
    operation.extensions = SourceExtensions::new(
        Surface::OpenAi,
        BTreeMap::from([
            (
                "/response_format".into(),
                serde_json::json!("diarized_json"),
            ),
            (
                "/known_speaker_names".into(),
                serde_json::json!(["agent", "customer"]),
            ),
            (
                "/known_speaker_references".into(),
                serde_json::json!(["data:audio/wav;base64,AAAA", "data:audio/wav;base64,BBBB"]),
            ),
        ]),
    );
    connector.execute(request).await.unwrap();
    let captured = String::from_utf8(captured.await.unwrap()).unwrap();
    assert_eq!(
        captured.matches("name=\"known_speaker_names[]\"").count(),
        2
    );
    assert_eq!(
        captured
            .matches("name=\"known_speaker_references[]\"")
            .count(),
        2
    );
    assert!(captured.contains("data:audio/wav;base64,AAAA"));
    assert!(captured.contains("data:audio/wav;base64,BBBB"));
}
