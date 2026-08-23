use super::*;

#[derive(Default)]
struct TrackingMediaSpool {
    puts: AtomicUsize,
    removes: AtomicUsize,
}

impl MediaSpool for TrackingMediaSpool {
    fn put<'a>(
        &'a self,
        upload: MediaUpload,
    ) -> crate::domain::ports::BoxFuture<'a, Result<MediaArtifact, MediaSpoolError>> {
        let index = self.puts.fetch_add(1, Ordering::AcqRel);
        Box::pin(async move {
            Ok(MediaArtifact {
                handle: MediaHandle::new(format!("staged-{index}")),
                content_type: upload.content_type,
                content_length: Some(1),
            })
        })
    }

    fn open<'a>(
        &'a self,
        _handle: &'a MediaHandle,
    ) -> crate::domain::ports::BoxFuture<'a, Result<OpenedMedia, MediaSpoolError>> {
        Box::pin(async { Err(MediaSpoolError::NotFound) })
    }

    fn remove<'a>(
        &'a self,
        _handle: &'a MediaHandle,
    ) -> crate::domain::ports::BoxFuture<'a, Result<(), MediaSpoolError>> {
        self.removes.fetch_add(1, Ordering::AcqRel);
        Box::pin(async { Ok(()) })
    }
}

#[tokio::test]
async fn image_generation_and_video_creation_use_current_paths() {
    let image_body = serde_json::to_vec(&serde_json::json!({
        "created": 1,
        "data": [{"url": "https://cdn.example.test/image.png"}]
    }))
    .unwrap();
    let (base_url, captured_image) = spawn_mock(MockResponse {
        chunks: vec![(
            Duration::ZERO,
            http_response("application/json", &image_body),
        )],
    })
    .await;
    let connector = test_connector(&base_url, Timeouts::default());
    let output = connector.execute(image_request(false)).await.unwrap();
    assert!(matches!(
        output,
        ProviderOutput::Result(result) if matches!(*result, CanonicalResult::Images(_))
    ));
    assert!(
        String::from_utf8(captured_image.await.unwrap())
            .unwrap()
            .starts_with("POST /v1/images/generations ")
    );

    let video_body = serde_json::to_vec(&serde_json::json!({
        "id": "video_123",
        "object": "video",
        "model": "sora-2",
        "status": "queued",
        "created_at": 1
    }))
    .unwrap();
    let (base_url, captured_video) = spawn_mock(MockResponse {
        chunks: vec![(
            Duration::ZERO,
            http_response("application/json", &video_body),
        )],
    })
    .await;
    let connector = test_connector(&base_url, Timeouts::default());
    let output = connector.execute(video_create_request()).await.unwrap();
    assert!(matches!(
        output,
        ProviderOutput::Result(result) if matches!(*result, CanonicalResult::VideoJob(_))
    ));
    let captured_video = String::from_utf8(captured_video.await.unwrap()).unwrap();
    assert!(captured_video.starts_with("POST /v1/videos "));
    assert!(
        captured_video
            .to_ascii_lowercase()
            .contains("content-type: multipart/form-data; boundary=")
    );
    assert!(captured_video.contains("name=\"model\""));
    assert!(captured_video.contains("sora-2"));
    assert!(captured_video.contains("name=\"prompt\""));
    assert!(captured_video.contains("a calm ocean"));
    assert!(captured_video.contains("name=\"input_reference\""));
    assert!(captured_video.contains("filename=\"reference.png\""));
    assert!(captured_video.contains("data"));
}

#[tokio::test]
async fn image_edit_and_variation_forward_bounded_multipart_parts() {
    let response = serde_json::to_vec(&serde_json::json!({
        "created": 1,
        "data": [{"url": "https://cdn.example.test/edited.png"}]
    }))
    .unwrap();
    let (base_url, captured) = spawn_mock(MockResponse {
        chunks: vec![(Duration::ZERO, http_response("application/json", &response))],
    })
    .await;
    let connector = test_connector(&base_url, Timeouts::default());
    let ProviderOutput::Result(result) = connector.execute(image_edit_request()).await.unwrap()
    else {
        panic!("image edit returned a stream")
    };
    let CanonicalResult::Images(result) = *result else {
        panic!("image edit returned the wrong result kind")
    };
    assert!(matches!(
        &result.images[0].source,
        MediaSource::Uri(uri) if uri == "https://cdn.example.test/edited.png"
    ));
    let captured = String::from_utf8(captured.await.unwrap()).unwrap();
    assert!(captured.starts_with("POST /v1/images/edits HTTP/1.1"));
    assert_bearer_auth(&captured);
    assert!(
        captured
            .to_ascii_lowercase()
            .contains("content-type: multipart/form-data; boundary=")
    );
    assert!(captured.contains("name=\"model\""));
    assert!(captured.contains("gpt-image-1"));
    assert!(captured.contains("name=\"prompt\""));
    assert!(captured.contains("replace the sky"));
    assert!(captured.contains("name=\"image\"; filename=\"source.png\""));
    assert!(captured.contains("name=\"mask\"; filename=\"source.png\""));
    assert_eq!(captured.matches("png-data").count(), 2);

    let response = serde_json::to_vec(&serde_json::json!({
        "created": 2,
        "data": [{"url": "https://cdn.example.test/variation.png"}]
    }))
    .unwrap();
    let (base_url, captured) = spawn_mock(MockResponse {
        chunks: vec![(Duration::ZERO, http_response("application/json", &response))],
    })
    .await;
    let connector = test_connector(&base_url, Timeouts::default());
    let ProviderOutput::Result(result) =
        connector.execute(image_variation_request()).await.unwrap()
    else {
        panic!("image variation returned a stream")
    };
    assert!(matches!(*result, CanonicalResult::Images(_)));
    let captured = String::from_utf8(captured.await.unwrap()).unwrap();
    assert!(captured.starts_with("POST /v1/images/variations HTTP/1.1"));
    assert_bearer_auth(&captured);
    assert!(
        captured
            .to_ascii_lowercase()
            .contains("content-type: multipart/form-data; boundary=")
    );
    assert!(captured.contains("name=\"image\"; filename=\"variation.png\""));
    assert!(captured.contains("variation-data"));
    assert!(captured.contains("name=\"n\""));
    assert!(captured.contains("\r\n\r\n2\r\n"));
    assert!(captured.contains("name=\"size\""));
    assert!(captured.contains("512x512"));
}

#[tokio::test]
async fn moderation_posts_json_and_returns_dynamic_typed_categories() {
    let response = serde_json::to_vec(&serde_json::json!({
        "id": "modr_123",
        "model": "omni-moderation-latest",
        "results": [{
            "flagged": true,
            "categories": {"violence": true, "violence/graphic": false},
            "category_scores": {"violence": 0.97, "violence/graphic": 0.12}
        }]
    }))
    .unwrap();
    let (base_url, captured) = spawn_mock(MockResponse {
        chunks: vec![(Duration::ZERO, http_response("application/json", &response))],
    })
    .await;
    let connector = test_connector(&base_url, Timeouts::default());
    let ProviderOutput::Result(result) = connector.execute(moderation_request()).await.unwrap()
    else {
        panic!("moderation returned a stream")
    };
    let CanonicalResult::Moderation(result) = *result else {
        panic!("moderation returned the wrong result kind")
    };
    assert_eq!(result.id.as_deref(), Some("modr_123"));
    assert!(result.results[0].flagged);
    assert!(result.results[0].categories["violence"]);
    assert_eq!(result.results[0].category_scores["violence"], 0.97);
    let captured = String::from_utf8(captured.await.unwrap()).unwrap();
    assert!(captured.starts_with("POST /v1/moderations HTTP/1.1"));
    assert_bearer_auth(&captured);
    assert!(
        captured
            .to_ascii_lowercase()
            .contains("content-type: application/json")
    );
    assert!(captured.contains("\"model\":\"omni-moderation-latest\""));
    assert!(captured.contains("https://images.example.test/a.png"));
}

#[tokio::test]
async fn video_get_content_and_delete_use_current_lifecycle_paths() {
    let response = serde_json::to_vec(&serde_json::json!({
        "id": "video_123",
        "object": "video",
        "model": "sora-2",
        "status": "completed",
        "progress": 100.0,
        "created_at": 1,
        "completed_at": 2
    }))
    .unwrap();
    let (base_url, captured) = spawn_mock(MockResponse {
        chunks: vec![(Duration::ZERO, http_response("application/json", &response))],
    })
    .await;
    let connector = test_connector(&base_url, Timeouts::default());
    let ProviderOutput::Result(result) = connector
        .execute(video_job_request(OperationKind::VideoGet))
        .await
        .unwrap()
    else {
        panic!("video get returned a stream")
    };
    let CanonicalResult::VideoJob(result) = *result else {
        panic!("video get returned the wrong result kind")
    };
    assert_eq!(result.id, "video_123");
    assert_eq!(result.status, VideoStatus::Completed);
    assert_eq!(result.progress_percent, Some(100.0));
    let captured = String::from_utf8(captured.await.unwrap()).unwrap();
    assert!(captured.starts_with("GET /v1/videos/video_123 HTTP/1.1"));
    assert_bearer_auth(&captured);
    assert!(
        captured
            .to_ascii_lowercase()
            .contains("accept: application/json")
    );

    let spool = Arc::new(RecordingMediaSpool::default());
    let (base_url, captured) = spawn_mock(MockResponse {
        chunks: vec![(Duration::ZERO, http_response("video/mp4", b"video-bytes"))],
    })
    .await;
    let connector = test_connector(&base_url, Timeouts::default());
    let mut request = video_job_request(OperationKind::VideoContent);
    request.media = Some(spool.clone());
    let ProviderOutput::Result(result) = connector.execute(request).await.unwrap() else {
        panic!("video content returned a stream")
    };
    let CanonicalResult::VideoContent(result) = *result else {
        panic!("video content returned the wrong result kind")
    };
    assert_eq!(result.media.handle.as_str(), "recorded-0");
    assert_eq!(result.media.content_type.as_deref(), Some("video/mp4"));
    assert_eq!(result.media.content_length, Some(11));
    {
        let uploads = spool.uploads.lock().unwrap();
        assert_eq!(uploads.len(), 1);
        assert_eq!(uploads[0].filename, "video-content-video_123.bin");
        assert_eq!(uploads[0].content_type.as_deref(), Some("video/mp4"));
        assert_eq!(uploads[0].maximum_length, DEFAULT_MAX_RESPONSE_BYTES as u64);
        assert_eq!(uploads[0].bytes, b"video-bytes");
    }
    let captured = String::from_utf8(captured.await.unwrap()).unwrap();
    assert!(captured.starts_with("GET /v1/videos/video_123/content HTTP/1.1"));
    assert_bearer_auth(&captured);
    assert!(captured.to_ascii_lowercase().contains("accept: */*"));

    let response = serde_json::to_vec(&serde_json::json!({
        "id": "video_123",
        "object": "video.deleted",
        "deleted": true
    }))
    .unwrap();
    let (base_url, captured) = spawn_mock(MockResponse {
        chunks: vec![(Duration::ZERO, http_response("application/json", &response))],
    })
    .await;
    let connector = test_connector(&base_url, Timeouts::default());
    let ProviderOutput::Result(result) = connector
        .execute(video_job_request(OperationKind::VideoDelete))
        .await
        .unwrap()
    else {
        panic!("video delete returned a stream")
    };
    let CanonicalResult::VideoDelete(result) = *result else {
        panic!("video delete returned the wrong result kind")
    };
    assert_eq!(result.id, "video_123");
    assert!(result.deleted);
    let captured = String::from_utf8(captured.await.unwrap()).unwrap();
    assert!(captured.starts_with("DELETE /v1/videos/video_123 HTTP/1.1"));
    assert_bearer_auth(&captured);
    assert!(
        captured
            .to_ascii_lowercase()
            .contains("accept: application/json")
    );
}

#[tokio::test]
async fn video_delete_missing_is_success_only_for_durable_reconciliation() {
    let missing = b"{\"error\":{\"message\":\"not found\"}}";
    let response = format!(
        "HTTP/1.1 404 Not Found\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{}",
        missing.len(),
        String::from_utf8_lossy(missing)
    )
    .into_bytes();
    let (base_url, _) = spawn_mock(MockResponse {
        chunks: vec![(Duration::ZERO, response.clone())],
    })
    .await;
    let connector = test_connector(&base_url, Timeouts::default());
    let failure = execute_error(&connector, video_job_request(OperationKind::VideoDelete)).await;
    assert_eq!(failure.class, AttemptFailureClass::UpstreamClient);

    let (base_url, _) = spawn_mock(MockResponse {
        chunks: vec![(Duration::ZERO, response)],
    })
    .await;
    let connector = test_connector(&base_url, Timeouts::default());
    let mut request = video_job_request(OperationKind::VideoDelete);
    let Operation::Video(VideoOperation::Delete(operation)) = &mut request.operation else {
        unreachable!()
    };
    operation.extensions.source = Some(Surface::OpenAi);
    operation.extensions.values.insert(
        MEDIA_DELETE_MISSING_IS_SUCCESS_EXTENSION.to_owned(),
        serde_json::Value::Bool(true),
    );
    let ProviderOutput::Result(result) = connector.execute(request).await.unwrap() else {
        panic!("video reconciliation returned a stream")
    };
    let CanonicalResult::VideoDelete(result) = *result else {
        panic!("video reconciliation returned the wrong result kind")
    };
    assert!(result.deleted);
    assert_eq!(result.id, "video_123");
}

#[tokio::test]
async fn media_bounds_fail_closed_before_dispatch_and_during_response_staging() {
    let connector = test_connector("http://127.0.0.1:1/v1/", Timeouts::default());
    let mut request = image_edit_request();
    request.media = Some(Arc::new(
        FixtureMediaSpool::new("source.png", "image/png", b"tiny")
            .with_declared_length(50 * 1024 * 1024 + 1),
    ));
    let failure = execute_error(&connector, request).await;
    assert_eq!(failure.phase, TransportPhase::Body);
    assert_eq!(failure.class, AttemptFailureClass::Protocol);
    assert!(!failure.response_committed);

    let spool = Arc::new(RecordingMediaSpool::default());
    let (base_url, captured) = spawn_mock(MockResponse {
        chunks: vec![(Duration::ZERO, http_response("audio/mpeg", b"four"))],
    })
    .await;
    let mut config = ConnectorConfig::for_local_test(&base_url, Timeouts::default());
    config.max_response_bytes = 3;
    let connector = Connector::new(config, ApiKey::new("upstream-secret").unwrap());
    let mut request = speech_request(false);
    request.media = Some(spool.clone());
    let failure = execute_error(&connector, request).await;
    assert_eq!(failure.phase, TransportPhase::Body);
    assert_eq!(failure.class, AttemptFailureClass::Protocol);
    assert!(!failure.response_committed);
    assert_eq!(spool.puts.load(Ordering::Acquire), 1);
    assert!(spool.uploads.lock().unwrap().is_empty());
    assert_eq!(spool.removes.load(Ordering::Acquire), 0);
    let captured = String::from_utf8(captured.await.unwrap()).unwrap();
    assert!(captured.starts_with("POST /v1/audio/speech HTTP/1.1"));
}

#[tokio::test]
async fn image_decode_failure_removes_already_staged_response_media() {
    let connector = test_connector("http://127.0.0.1:1/v1/", Timeouts::default());
    let spool = Arc::new(TrackingMediaSpool::default());
    let mut request = image_request(false);
    request.media = Some(spool.clone());
    let wire: OpenAiImageResponse = serde_json::from_value(serde_json::json!({
        "created": 1,
        "data": [
            {"b64_json": "b2s="},
            {"b64_json": "%%%"}
        ]
    }))
    .unwrap();

    assert!(connector.decode_image_result(&request, wire).await.is_err());
    assert_eq!(spool.puts.load(Ordering::Acquire), 1);
    assert_eq!(spool.removes.load(Ordering::Acquire), 1);
}
