use super::*;

#[derive(Default)]
struct CountingAdmissionSpool {
    puts: AtomicUsize,
}

impl olp_engine::domain::ports::MediaSpool for CountingAdmissionSpool {
    fn put<'a>(
        &'a self,
        _upload: olp_engine::domain::ports::MediaUpload,
    ) -> BoxFuture<
        'a,
        Result<
            olp_engine::domain::canonical::results::MediaArtifact,
            olp_engine::domain::ports::MediaSpoolError,
        >,
    > {
        self.puts.fetch_add(1, Ordering::SeqCst);
        Box::pin(async { Err(olp_engine::domain::ports::MediaSpoolError::Unavailable) })
    }

    fn open<'a>(
        &'a self,
        _handle: &'a MediaHandle,
    ) -> BoxFuture<
        'a,
        Result<olp_engine::domain::ports::OpenedMedia, olp_engine::domain::ports::MediaSpoolError>,
    > {
        Box::pin(async { Err(olp_engine::domain::ports::MediaSpoolError::NotFound) })
    }

    fn remove<'a>(
        &'a self,
        _handle: &'a MediaHandle,
    ) -> BoxFuture<'a, Result<(), olp_engine::domain::ports::MediaSpoolError>> {
        Box::pin(async { Ok(()) })
    }
}

struct RecordingSpool {
    inner: Arc<dyn MediaSpool>,
    handles: Mutex<Vec<MediaHandle>>,
    removed: Mutex<Vec<MediaHandle>>,
}

impl RecordingSpool {
    fn new(inner: Arc<dyn MediaSpool>) -> Arc<Self> {
        Arc::new(Self {
            inner,
            handles: Mutex::new(Vec::new()),
            removed: Mutex::new(Vec::new()),
        })
    }

    fn handles(&self) -> Vec<MediaHandle> {
        self.handles.lock().unwrap().clone()
    }

    fn removed(&self, handle: &MediaHandle) -> bool {
        self.removed.lock().unwrap().contains(handle)
    }
}

impl MediaSpool for RecordingSpool {
    fn capacity_bytes(&self) -> Option<u64> {
        self.inner.capacity_bytes()
    }

    fn put<'a>(
        &'a self,
        upload: olp_engine::domain::ports::MediaUpload,
    ) -> BoxFuture<
        'a,
        Result<
            olp_engine::domain::canonical::results::MediaArtifact,
            olp_engine::domain::ports::MediaSpoolError,
        >,
    > {
        Box::pin(async move {
            let artifact = self.inner.put(upload).await?;
            self.handles.lock().unwrap().push(artifact.handle.clone());
            Ok(artifact)
        })
    }

    fn open<'a>(
        &'a self,
        handle: &'a MediaHandle,
    ) -> BoxFuture<
        'a,
        Result<olp_engine::domain::ports::OpenedMedia, olp_engine::domain::ports::MediaSpoolError>,
    > {
        self.inner.open(handle)
    }

    fn remove<'a>(
        &'a self,
        handle: &'a MediaHandle,
    ) -> BoxFuture<'a, Result<(), olp_engine::domain::ports::MediaSpoolError>> {
        self.removed.lock().unwrap().push(handle.clone());
        self.inner.remove(handle)
    }
}

fn recording_spool() -> Arc<RecordingSpool> {
    RecordingSpool::new(crate::media_spool::FileMediaSpool::create().unwrap() as Arc<dyn MediaSpool>)
}

async fn assert_cleanup(spool: &RecordingSpool, handle: &MediaHandle) {
    tokio::time::timeout(Duration::from_secs(1), async {
        while !spool.removed(handle) {
            tokio::task::yield_now().await;
        }
    })
    .await
    .expect("media cleanup must be scheduled promptly");
}

fn raw_media_extension(source: Surface, data: bool, event: Option<&str>, extra: bool) -> Event {
    let values = [
        data.then(|| ("/__olp/raw_sse/data".to_owned(), json!({"ok":true}))),
        event.map(|event| ("/__olp/raw_sse/event".to_owned(), json!(event))),
        extra.then(|| ("unsupported".to_owned(), json!(true))),
    ]
    .into_iter()
    .flatten()
    .collect();
    Event::new(
        0,
        Kind::SourceExtension {
            extensions: SourceExtensions::new(source, values),
        },
    )
}

#[test]
fn raw_media_events_are_strictly_validated_and_encoded() {
    use crate::gateway::media::raw_media_event_bytes;

    let event = |kind| Event::new(0, kind);
    assert_eq!(
        raw_media_event_bytes(raw_media_extension(
            Surface::OpenAi,
            true,
            Some("audio.delta"),
            false,
        ))
        .unwrap(),
        Some(Bytes::from_static(
            b"event: audio.delta\ndata: {\"ok\":true}\n\n"
        ))
    );
    assert_eq!(raw_media_event_bytes(event(Kind::Done)).unwrap(), None);

    for event in [
        raw_media_extension(Surface::Anthropic, true, None, false),
        raw_media_extension(Surface::OpenAi, false, None, false),
        raw_media_extension(Surface::OpenAi, true, None, true),
        raw_media_extension(Surface::OpenAi, true, Some("invalid\nevent"), false),
        event(Kind::TextDelta {
            output_index: 0,
            text: "unexpected".to_owned(),
        }),
    ] {
        let error = raw_media_event_bytes(event).unwrap_err();
        assert_eq!(error.status(), StatusCode::BAD_GATEWAY);
        assert_eq!(error.code(), "provider_protocol_error");
    }

    let error = raw_media_event_bytes(event(Kind::Error {
        error: Error {
            class: ErrorClass::RateLimit,
            message: "slow down".to_owned(),
            provider_code: None,
            retryable: true,
        },
    }))
    .unwrap_err();
    assert_eq!(error.status(), StatusCode::TOO_MANY_REQUESTS);
    assert_eq!(error.code(), "upstream_error");
    assert_eq!(error.message(), "slow down");
}

fn opened_media(
    content_type: Option<&str>,
    content_length: Option<u64>,
) -> (olp_engine::domain::ports::OpenedMedia, MediaHandle) {
    let handle = MediaHandle::new("00000000000000000000000000000000");
    (
        olp_engine::domain::ports::OpenedMedia {
            artifact: olp_engine::domain::canonical::results::MediaArtifact {
                handle: handle.clone(),
                content_type: content_type.map(str::to_owned),
                content_length,
            },
            filename: "speech.mp3".to_owned(),
            bytes: Box::pin(stream::once(async { Ok(Bytes::from_static(b"audio")) })),
        },
        handle,
    )
}

#[tokio::test]
async fn opened_media_response_preserves_metadata_and_cleans_up() {
    use crate::gateway::media::response_from_opened_media;

    for (content_type, content_length) in [(Some("audio/mpeg"), Some(5)), (None, None)] {
        let spool = recording_spool();
        let (opened, handle) = opened_media(content_type, content_length);
        let response = response_from_opened_media(opened, spool.clone(), "type", "length").unwrap();
        assert_eq!(
            response
                .headers()
                .get(header::CONTENT_TYPE)
                .map(|value| value.to_str().unwrap()),
            content_type
        );
        assert_eq!(
            response
                .headers()
                .get(header::CONTENT_LENGTH)
                .map(|value| value.to_str().unwrap()),
            content_length.map(|_| "5")
        );
        assert_eq!(
            response.into_body().collect().await.unwrap().to_bytes(),
            "audio"
        );
        assert_cleanup(&spool, &handle).await;
    }
}

#[tokio::test]
async fn media_open_and_header_failures_still_schedule_cleanup() {
    use crate::gateway::media::{open_response_media, response_from_opened_media};

    let (mut state, _) = test_state(false);
    let spool = recording_spool();
    state.replace_media_spool_for_test(spool.clone());
    let invalid = MediaHandle::new("invalid");
    let error = open_response_media(&state, &invalid).await.unwrap_err();
    assert_eq!(
        (error.status(), error.code()),
        (StatusCode::BAD_REQUEST, "invalid_request")
    );
    assert_cleanup(&spool, &invalid).await;

    let (opened, handle) = opened_media(Some("invalid\ncontent-type"), Some(5));
    let error =
        response_from_opened_media(opened, spool.clone(), "invalid type", "length").unwrap_err();
    assert_eq!(
        (error.status(), error.code(), error.message()),
        (
            StatusCode::BAD_GATEWAY,
            "provider_protocol_error",
            "invalid type"
        )
    );
    assert_cleanup(&spool, &handle).await;
}

#[tokio::test]
async fn invalid_keys_cannot_spool_responses_media_before_authentication() {
    let (mut state, _) = test_state(false);
    let (_, invalid_key) = test_state(false);
    let spool = Arc::new(CountingAdmissionSpool::default());
    state.replace_media_spool_for_test(spool.clone());

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
    state.replace_media_spool_for_test(spool.clone());
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

/// OpenAI's own documented invocation is `curl -F file=@… -F model=…`, so a
/// route-restricted key must be able to send the file first. Authorization is
/// deferred to the `model` field wherever it appears; files stream to the
/// bounded spool the key's reservation already covers.
#[tokio::test]
async fn a_route_restricted_key_may_send_the_file_before_the_model() {
    let (mut state, key) = test_state(false);
    let spool = Arc::new(CountingAdmissionSpool::default());
    state.replace_media_spool_for_test(spool.clone());
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
    // The spool is reached, which is the point: the request is no longer
    // rejected on field order alone.
    assert_eq!(spool.puts.load(Ordering::SeqCst), 1);
    assert_ne!(
        response.status(),
        StatusCode::BAD_REQUEST,
        "documented OpenAI field ordering must not be a client error"
    );
}

#[tokio::test]
async fn a_route_restricted_multipart_request_without_a_model_is_rejected() {
    let (mut state, key) = test_state(false);
    let recording = recording_spool();
    state.replace_media_spool_for_test(recording.clone());
    restrict_api_key_to_route(&state, RouteSlug::parse("default").unwrap());
    for model_field in ["", "unauthorized-route"] {
        let mut body = concat!(
            "--olp-test-boundary\r\n",
            "Content-Disposition: form-data; name=\"image\"; filename=\"fixture.png\"\r\n",
            "Content-Type: image/png\r\n\r\n",
            "staged-image\r\n",
        )
        .to_owned();
        if !model_field.is_empty() {
            body.push_str(&format!(
                "--olp-test-boundary\r\nContent-Disposition: form-data;                  name=\"model\"\r\n\r\n{model_field}\r\n"
            ));
        }
        body.push_str("--olp-test-boundary--\r\n");
        let response = post_multipart(&state, &key, "/openai/v1/images/edits", body).await;
        assert!(
            response.status().is_client_error(),
            "{model_field:?} must not be admitted"
        );
    }
    // Every file staged for a rejected request is cleaned up.
    for handle in recording.handles() {
        assert!(matches!(
            recording.open(&handle).await,
            Err(olp_engine::domain::ports::MediaSpoolError::NotFound)
        ));
    }
}

#[tokio::test]
async fn multipart_route_header_mismatch_cleans_the_staged_file() {
    let (mut state, key) = test_state(false);
    let recording = recording_spool();
    state.replace_media_spool_for_test(recording.clone());
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
    let response = crate::public_http::router::gateway_router_for_test(state.clone())
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
        Err(olp_engine::domain::ports::MediaSpoolError::NotFound)
    ));
}

fn restrict_api_key_to_route(state: &GatewayState, route: RouteSlug) {
    let pinned = state.runtime().pin();
    let mut api_keys = pinned.api_keys.clone();
    api_keys.values_mut().next().unwrap().allowed_routes = BTreeSet::from([route]);
    reinstall_api_keys(state, api_keys);
}

fn replace_api_key_scopes(state: &GatewayState, scopes: BTreeSet<ApiKeyScope>) {
    let pinned = state.runtime().pin();
    let mut api_keys = pinned.api_keys.clone();
    api_keys.values_mut().next().unwrap().scopes = scopes;
    reinstall_api_keys(state, api_keys);
}

#[tokio::test]
async fn malformed_multipart_is_rejected_before_routing() {
    let (state, key) = test_state(false);
    let response = crate::public_http::router::gateway_router_for_test(state)
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
        .put(olp_engine::domain::ports::MediaUpload {
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
                Err(olp_engine::domain::ports::MediaSpoolError::NotFound) => break,
                Ok(_) => tokio::task::yield_now().await,
                Err(error) => panic!("unexpected spool cleanup error: {error}"),
            }
        }
    })
    .await
    .expect("multipart error cleanup must remove the staged file promptly");
}
