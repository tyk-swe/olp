use super::*;

#[test]
fn unknown_upstream_video_status_fails_closed() {
    let error = media_job_state(&olp_domain::VideoStatus::Other("mystery".to_owned()))
        .expect_err("unknown upstream status must not become a local terminal state");
    assert_eq!(error.status, StatusCode::BAD_GATEWAY);
    assert_eq!(error.code, "provider_protocol_error");
}

#[test]
fn upstream_media_identity_is_bounded_before_durable_attachment() {
    assert!(valid_upstream_media_job_id("video_123"));
    assert!(!valid_upstream_media_job_id(""));
    assert!(!valid_upstream_media_job_id(" video_123"));
    assert!(!valid_upstream_media_job_id("video\n123"));
    assert!(!valid_upstream_media_job_id(&"x".repeat(1_025)));
}

#[derive(Default)]
struct CountingAdmissionSpool {
    puts: AtomicUsize,
}

impl olp_domain::MediaSpool for CountingAdmissionSpool {
    fn put<'a>(
        &'a self,
        _upload: olp_domain::MediaUpload,
    ) -> BoxFuture<'a, Result<olp_domain::MediaArtifact, olp_domain::MediaSpoolError>> {
        self.puts.fetch_add(1, Ordering::SeqCst);
        Box::pin(async { Err(olp_domain::MediaSpoolError::Unavailable) })
    }

    fn open<'a>(
        &'a self,
        _handle: &'a MediaHandle,
    ) -> BoxFuture<'a, Result<olp_domain::OpenedMedia, olp_domain::MediaSpoolError>> {
        Box::pin(async { Err(olp_domain::MediaSpoolError::NotFound) })
    }

    fn remove<'a>(
        &'a self,
        _handle: &'a MediaHandle,
    ) -> BoxFuture<'a, Result<(), olp_domain::MediaSpoolError>> {
        Box::pin(async { Ok(()) })
    }
}

struct RecordingSpool {
    inner: Arc<dyn MediaSpool>,
    handles: Mutex<Vec<MediaHandle>>,
}

impl RecordingSpool {
    fn new(inner: Arc<dyn MediaSpool>) -> Arc<Self> {
        Arc::new(Self {
            inner,
            handles: Mutex::new(Vec::new()),
        })
    }

    fn handles(&self) -> Vec<MediaHandle> {
        self.handles.lock().unwrap().clone()
    }
}

impl MediaSpool for RecordingSpool {
    fn capacity_bytes(&self) -> Option<u64> {
        self.inner.capacity_bytes()
    }

    fn put<'a>(
        &'a self,
        upload: olp_domain::MediaUpload,
    ) -> BoxFuture<'a, Result<olp_domain::MediaArtifact, olp_domain::MediaSpoolError>> {
        Box::pin(async move {
            let artifact = self.inner.put(upload).await?;
            self.handles.lock().unwrap().push(artifact.handle.clone());
            Ok(artifact)
        })
    }

    fn open<'a>(
        &'a self,
        handle: &'a MediaHandle,
    ) -> BoxFuture<'a, Result<olp_domain::OpenedMedia, olp_domain::MediaSpoolError>> {
        self.inner.open(handle)
    }

    fn remove<'a>(
        &'a self,
        handle: &'a MediaHandle,
    ) -> BoxFuture<'a, Result<(), olp_domain::MediaSpoolError>> {
        self.inner.remove(handle)
    }
}

#[tokio::test]
async fn invalid_keys_cannot_spool_responses_media_before_authentication() {
    let (mut state, _) = test_state(false);
    let (_, invalid_key) = test_state(false);
    let spool = Arc::new(CountingAdmissionSpool::default());
    state.media_spool = spool.clone();

    for (path, body) in [
        (
            "/openai/v1/responses",
            r#"{"model":"default","input":[{"type":"message","role":"user","content":[{"type":"input_audio","input_audio":{"data":"YXVkaW8=","format":"wav"}}]}]}"#,
        ),
        (
            "/openai/v1/responses/input_tokens",
            r#"{"model":"default","input":[{"type":"message","role":"user","content":[{"type":"input_audio","input_audio":{"data":"YXVkaW8=","format":"wav"}}]}]}"#,
        ),
    ] {
        let response = post_json(&state, &invalid_key, path, body).await;
        assert_eq!(response.status(), StatusCode::UNAUTHORIZED);
    }
    assert_eq!(spool.puts.load(Ordering::SeqCst), 0);
}

#[tokio::test]
async fn responses_scope_authorization_precedes_json_errors_and_media_staging() {
    let (mut state, key) = test_state(false);
    let spool = Arc::new(CountingAdmissionSpool::default());
    state.media_spool = spool.clone();
    replace_api_key_scopes(&state, BTreeSet::from([ApiKeyScope::ModelsRead]));

    for path in ["/openai/v1/responses", "/openai/v1/responses/input_tokens"] {
        let response = post_json(&state, &key, path, "{").await;
        assert_eq!(
            response.status(),
            StatusCode::FORBIDDEN,
            "operation authorization must precede malformed JSON for {path}"
        );

        let response = post_json(
            &state,
            &key,
            path,
            r#"{"model":"default","input":[{"type":"message","role":"user","content":[{"type":"input_audio","input_audio":{"data":"YXVkaW8=","format":"wav"}}]}]}"#,
        )
        .await;
        assert_eq!(
            response.status(),
            StatusCode::FORBIDDEN,
            "operation authorization must precede inline-media admission for {path}"
        );
        assert_eq!(spool.puts.load(Ordering::SeqCst), 0, "{path}");
    }
}

#[tokio::test]
async fn restricted_multipart_key_rejects_file_before_model_without_spooling() {
    let (mut state, key) = test_state(false);
    let spool = Arc::new(CountingAdmissionSpool::default());
    state.media_spool = spool.clone();
    restrict_api_key_to_route(&state, RouteSlug::parse("default").unwrap());
    let body = concat!(
        "--olp-test-boundary\r\n",
        "Content-Disposition: form-data; name=\"image\"; filename=\"fixture.png\"\r\n",
        "Content-Type: image/png\r\n\r\n",
        "file-before-model\r\n",
        "--olp-test-boundary\r\n",
        "Content-Disposition: form-data; name=\"model\"\r\n\r\n",
        "default\r\n",
        "--olp-test-boundary--\r\n"
    )
    .to_owned();
    let response = post_multipart(&state, &key, "/openai/v1/images/edits", body).await;
    assert_eq!(response.status(), StatusCode::BAD_REQUEST);
    assert_eq!(spool.puts.load(Ordering::SeqCst), 0);
}

#[tokio::test]
async fn multipart_route_header_mismatch_cleans_the_staged_file() {
    let (mut state, key) = test_state(false);
    let recording = RecordingSpool::new(
        crate::media_spool::FileMediaSpool::create().unwrap() as Arc<dyn MediaSpool>
    );
    state.media_spool = recording.clone();
    restrict_api_key_to_route(&state, RouteSlug::parse("default").unwrap());
    let body = concat!(
        "--olp-test-boundary\r\n",
        "Content-Disposition: form-data; name=\"image\"; filename=\"fixture.png\"\r\n",
        "Content-Type: image/png\r\n\r\n",
        "staged-image\r\n",
        "--olp-test-boundary\r\n",
        "Content-Disposition: form-data; name=\"model\"\r\n\r\n",
        "other\r\n",
        "--olp-test-boundary\r\n",
        "Content-Disposition: form-data; name=\"prompt\"\r\n\r\n",
        "route mismatch\r\n",
        "--olp-test-boundary--\r\n"
    );
    let response = crate::router::gateway_router_for_test(state.clone())
        .oneshot(
            Request::post("/openai/v1/images/edits")
                .header(header::AUTHORIZATION, format!("Bearer {key}"))
                .header("x-olp-route", "default")
                .header(
                    header::CONTENT_TYPE,
                    "multipart/form-data; boundary=olp-test-boundary",
                )
                .body(Body::from(body))
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(response.status(), StatusCode::BAD_REQUEST);
    let handles = recording.handles();
    assert_eq!(handles.len(), 1);
    assert!(matches!(
        recording.open(&handles[0]).await,
        Err(olp_domain::MediaSpoolError::NotFound)
    ));
}

fn restrict_api_key_to_route(state: &GatewayState, route: RouteSlug) {
    let pinned = state.runtime.pin();
    let mut api_keys = pinned.api_keys.clone();
    api_keys.values_mut().next().unwrap().allowed_routes = BTreeSet::from([route]);
    reinstall_api_keys(state, api_keys);
}

fn replace_api_key_scopes(state: &GatewayState, scopes: BTreeSet<ApiKeyScope>) {
    let pinned = state.runtime.pin();
    let mut api_keys = pinned.api_keys.clone();
    api_keys.values_mut().next().unwrap().scopes = scopes;
    reinstall_api_keys(state, api_keys);
}

#[tokio::test]
async fn malformed_multipart_is_rejected_before_routing() {
    let (state, key) = test_state(false);
    let response = crate::router::gateway_router_for_test(state)
        .oneshot(
            Request::post("/openai/v1/images/edits")
                .header(header::AUTHORIZATION, format!("Bearer {key}"))
                .header(header::CONTENT_TYPE, "multipart/form-data")
                .body(Body::from("not-a-multipart-body"))
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(response.status(), StatusCode::BAD_REQUEST);
}

#[tokio::test]
async fn failed_multipart_validation_removes_staged_files() {
    let spool = crate::media_spool::FileMediaSpool::create().unwrap();
    let artifact = spool
        .put(olp_domain::MediaUpload {
            filename: "upload.png".to_owned(),
            content_type: Some("image/png".to_owned()),
            maximum_length: 16,
            bytes: Box::pin(stream::once(async { Ok(Bytes::from_static(b"staged")) })),
        })
        .await
        .unwrap();
    let mut form = MultipartFormData::new(spool.clone(), MultipartRequestAdmission::unrestricted());
    form.cleanup_handles.push(artifact.handle.clone());
    drop(form);

    tokio::time::timeout(Duration::from_secs(1), async {
        loop {
            match spool.open(&artifact.handle).await {
                Err(olp_domain::MediaSpoolError::NotFound) => break,
                Ok(_) => tokio::task::yield_now().await,
                Err(error) => panic!("unexpected spool cleanup error: {error}"),
            }
        }
    })
    .await
    .expect("multipart error cleanup must remove the staged file promptly");
}

#[test]
fn video_duration_is_billed_once_at_creation_not_on_every_poll() {
    let job = olp_domain::VideoJobResult {
        id: "video_1".to_owned(),
        model: Some("sora".to_owned()),
        status: olp_domain::VideoStatus::Completed,
        progress_percent: None,
        created_at: None,
        completed_at: None,
        expires_at: None,
        prompt: None,
        seconds: Some("8".to_owned()),
        size: None,
        error: None,
        extensions: Default::default(),
    };
    let result = CanonicalResult::VideoJob(job);

    // The creating operation bills the duration once.
    let created = super::super::telemetry::usage_from_result(&result, OperationKind::VideoCreate);
    assert_eq!(created.media_units(), Some(rust_decimal::Decimal::from(8)));

    // Status polls return the identical result shape — the reconciliation
    // supervisor re-reads it every ~5s, and clients poll it directly. Counting
    // media units there would re-bill the video once per poll.
    for operation in [
        OperationKind::VideoGet,
        OperationKind::VideoList,
        OperationKind::VideoDelete,
        OperationKind::VideoContent,
    ] {
        let polled = super::super::telemetry::usage_from_result(&result, operation);
        assert_eq!(
            polled.media_units(),
            None,
            "{operation:?} must not re-bill the video duration"
        );
    }
}
