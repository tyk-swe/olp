use std::{
    collections::BTreeMap,
    future::ready,
    sync::{
        Arc, Mutex,
        atomic::{AtomicUsize, Ordering},
    },
    time::Duration,
};

use bytes::Bytes;
use futures::{StreamExt, stream};
use http::StatusCode;
use olp_domain::{
    AttemptFailureClass, AttemptPlan, CanonicalEventKind, CanonicalResult, ContentPart, DurationMs,
    EmbeddingInput, EmbeddingsRequest, GenerationParameters, GenerationRequest, ImageEditRequest,
    ImageGenerationRequest, ImageOperation, ImageVariationRequest,
    MEDIA_DELETE_MISSING_IS_SUCCESS_EXTENSION, MediaArtifact, MediaHandle, MediaSource, MediaSpool,
    MediaSpoolError, MediaUpload, Message, MessageRole, ModerationRequest, OpenedMedia, Operation,
    OperationKind, ProviderEventStream, ProviderId, ProviderKind, ProviderOutput, ProviderRequest,
    RequestId, RequestMetadata, RouteId, RouteSlug, RuntimeGenerationId, SourceExtensions,
    SpeechRequest, Surface, TargetId, TranscriptionRequest, TransportError, TransportMode,
    TransportPhase, VideoCreateRequest, VideoJobRequest, VideoOperation, VideoStatus,
};
use olp_protocols::openai::{
    ChatCompletionRequest, ChatContentPart, ChatMessageContent, OpenAiImageResponse, ResponseInput,
};
use tokio::{
    io::{AsyncReadExt, AsyncWriteExt},
    net::{TcpListener, TcpStream},
    sync::oneshot,
};

use super::*;
use crate::openai::{ConnectorTimeouts, DEFAULT_MAX_EVENT_BYTES, DEFAULT_MAX_RESPONSE_BYTES};

struct MockResponse {
    chunks: Vec<(Duration, Vec<u8>)>,
}

struct StaticMediaSpool;

struct FixtureMediaSpool {
    filename: String,
    content_type: String,
    bytes: Bytes,
    declared_length: u64,
}

#[derive(Default)]
struct RecordingMediaSpool {
    puts: AtomicUsize,
    removes: AtomicUsize,
    uploads: Mutex<Vec<RecordedUpload>>,
}

struct RecordedUpload {
    filename: String,
    content_type: Option<String>,
    maximum_length: u64,
    bytes: Vec<u8>,
}

#[derive(Default)]
struct TrackingMediaSpool {
    puts: AtomicUsize,
    removes: AtomicUsize,
}

impl MediaSpool for TrackingMediaSpool {
    fn put<'a>(
        &'a self,
        upload: MediaUpload,
    ) -> olp_domain::BoxFuture<'a, Result<MediaArtifact, MediaSpoolError>> {
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
    ) -> olp_domain::BoxFuture<'a, Result<OpenedMedia, MediaSpoolError>> {
        Box::pin(async { Err(MediaSpoolError::NotFound) })
    }

    fn remove<'a>(
        &'a self,
        _handle: &'a MediaHandle,
    ) -> olp_domain::BoxFuture<'a, Result<(), MediaSpoolError>> {
        self.removes.fetch_add(1, Ordering::AcqRel);
        Box::pin(async { Ok(()) })
    }
}

impl MediaSpool for StaticMediaSpool {
    fn put<'a>(
        &'a self,
        _upload: MediaUpload,
    ) -> olp_domain::BoxFuture<'a, Result<MediaArtifact, MediaSpoolError>> {
        Box::pin(async { Err(MediaSpoolError::Unavailable) })
    }

    fn open<'a>(
        &'a self,
        handle: &'a MediaHandle,
    ) -> olp_domain::BoxFuture<'a, Result<OpenedMedia, MediaSpoolError>> {
        let handle = handle.clone();
        Box::pin(async move {
            Ok(OpenedMedia {
                artifact: MediaArtifact {
                    handle,
                    content_type: Some("image/png".to_owned()),
                    content_length: Some(4),
                },
                filename: "reference.png".to_owned(),
                bytes: Box::pin(stream::once(async { Ok(Bytes::from_static(b"data")) })),
            })
        })
    }

    fn remove<'a>(
        &'a self,
        _handle: &'a MediaHandle,
    ) -> olp_domain::BoxFuture<'a, Result<(), MediaSpoolError>> {
        Box::pin(async { Ok(()) })
    }
}

impl FixtureMediaSpool {
    fn new(filename: &str, content_type: &str, bytes: &'static [u8]) -> Self {
        Self {
            filename: filename.into(),
            content_type: content_type.into(),
            bytes: Bytes::from_static(bytes),
            declared_length: u64::try_from(bytes.len()).unwrap(),
        }
    }

    fn with_declared_length(mut self, declared_length: u64) -> Self {
        self.declared_length = declared_length;
        self
    }
}

impl MediaSpool for FixtureMediaSpool {
    fn put<'a>(
        &'a self,
        _upload: MediaUpload,
    ) -> olp_domain::BoxFuture<'a, Result<MediaArtifact, MediaSpoolError>> {
        Box::pin(async { Err(MediaSpoolError::Unavailable) })
    }

    fn open<'a>(
        &'a self,
        handle: &'a MediaHandle,
    ) -> olp_domain::BoxFuture<'a, Result<OpenedMedia, MediaSpoolError>> {
        let artifact = MediaArtifact {
            handle: handle.clone(),
            content_type: Some(self.content_type.clone()),
            content_length: Some(self.declared_length),
        };
        let filename = self.filename.clone();
        let bytes = self.bytes.clone();
        Box::pin(async move {
            Ok(OpenedMedia {
                artifact,
                filename,
                bytes: Box::pin(stream::once(ready(Ok(bytes)))),
            })
        })
    }

    fn remove<'a>(
        &'a self,
        _handle: &'a MediaHandle,
    ) -> olp_domain::BoxFuture<'a, Result<(), MediaSpoolError>> {
        Box::pin(async { Ok(()) })
    }
}

impl MediaSpool for RecordingMediaSpool {
    fn put<'a>(
        &'a self,
        mut upload: MediaUpload,
    ) -> olp_domain::BoxFuture<'a, Result<MediaArtifact, MediaSpoolError>> {
        Box::pin(async move {
            let index = self.puts.fetch_add(1, Ordering::AcqRel);
            let mut bytes = Vec::new();
            while let Some(chunk) = upload.bytes.next().await {
                bytes.extend_from_slice(&chunk?);
                if u64::try_from(bytes.len()).unwrap_or(u64::MAX) > upload.maximum_length {
                    return Err(MediaSpoolError::TooLarge {
                        maximum: upload.maximum_length,
                    });
                }
            }
            let content_length = u64::try_from(bytes.len()).unwrap();
            let artifact = MediaArtifact {
                handle: MediaHandle::new(format!("recorded-{index}")),
                content_type: upload.content_type.clone(),
                content_length: Some(content_length),
            };
            self.uploads.lock().unwrap().push(RecordedUpload {
                filename: upload.filename,
                content_type: upload.content_type,
                maximum_length: upload.maximum_length,
                bytes,
            });
            Ok(artifact)
        })
    }

    fn open<'a>(
        &'a self,
        _handle: &'a MediaHandle,
    ) -> olp_domain::BoxFuture<'a, Result<OpenedMedia, MediaSpoolError>> {
        Box::pin(async { Err(MediaSpoolError::NotFound) })
    }

    fn remove<'a>(
        &'a self,
        _handle: &'a MediaHandle,
    ) -> olp_domain::BoxFuture<'a, Result<(), MediaSpoolError>> {
        self.removes.fetch_add(1, Ordering::AcqRel);
        Box::pin(async { Ok(()) })
    }
}

async fn spawn_mock(response: MockResponse) -> (String, oneshot::Receiver<Vec<u8>>) {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let address = listener.local_addr().unwrap();
    let (request_sender, request_receiver) = oneshot::channel();
    tokio::spawn(async move {
        let (mut socket, _) = listener.accept().await.unwrap();
        let request = read_request(&mut socket).await;
        let _ = request_sender.send(request);
        for (delay, chunk) in response.chunks {
            tokio::time::sleep(delay).await;
            if socket.write_all(&chunk).await.is_err() {
                return;
            }
            let _ = socket.flush().await;
        }
    });
    (format!("http://{address}/v1/"), request_receiver)
}

async fn read_request(socket: &mut TcpStream) -> Vec<u8> {
    let mut request = Vec::new();
    let mut buffer = [0_u8; 4096];
    let mut expected_length = None;
    loop {
        let read = socket.read(&mut buffer).await.unwrap();
        if read == 0 {
            return request;
        }
        request.extend_from_slice(&buffer[..read]);
        if expected_length.is_none()
            && let Some(headers_end) = find_bytes(&request, b"\r\n\r\n")
        {
            let headers = String::from_utf8_lossy(&request[..headers_end]);
            let content_length = headers
                .lines()
                .find_map(|line| {
                    let (name, value) = line.split_once(':')?;
                    name.eq_ignore_ascii_case("content-length")
                        .then(|| value.trim().parse::<usize>().ok())
                        .flatten()
                })
                .unwrap_or_default();
            expected_length = Some(headers_end + 4 + content_length);
        }
        if expected_length.is_some_and(|length| request.len() >= length) {
            return request;
        }
    }
}

fn find_bytes(haystack: &[u8], needle: &[u8]) -> Option<usize> {
    haystack
        .windows(needle.len())
        .position(|window| window == needle)
}

fn fixture_request(streaming: bool) -> ProviderRequest {
    ProviderRequest {
        metadata: RequestMetadata {
            request_id: RequestId::new(),
            operation: OperationKind::Generation,
            surface: Surface::OpenAi,
            mode: if streaming {
                TransportMode::Streaming
            } else {
                TransportMode::Unary
            },
        },
        attempt: AttemptPlan {
            generation_id: RuntimeGenerationId::new(),
            route_id: RouteId::new(),
            target_id: TargetId::new(),
            provider_id: ProviderId::new(),
            provider_kind: ProviderKind::OpenAi,
            provider_model: "gpt-4o-mini".into(),
            timeout: DurationMs::new(2_000),
            priority: 0,
        },
        operation: Operation::Generation(GenerationRequest {
            route: RouteSlug::parse("default").unwrap(),
            messages: vec![Message {
                role: MessageRole::User,
                content: vec![olp_domain::ContentPart::Text {
                    text: "hello".into(),
                }],
                name: None,
                tool_call_id: None,
                tool_calls: Vec::new(),
            }],
            parameters: GenerationParameters {
                stream: streaming,
                ..GenerationParameters::default()
            },
            tools: Vec::new(),
            tool_choice: None,
            response_format: None,
            extensions: SourceExtensions::new(Surface::OpenAi, BTreeMap::new()),
        }),
        media: None,
    }
}

fn embeddings_request() -> ProviderRequest {
    let mut request = fixture_request(false);
    request.metadata.operation = OperationKind::Embeddings;
    request.attempt.provider_model = "text-embedding-3-small".into();
    request.operation = Operation::Embeddings(EmbeddingsRequest {
        route: RouteSlug::parse("embeddings").unwrap(),
        input: vec![EmbeddingInput::Text("hello".into())],
        dimensions: Some(2),
        extensions: SourceExtensions::new(Surface::OpenAi, BTreeMap::new()),
    });
    request
}

fn responses_request(streaming: bool) -> ProviderRequest {
    let mut request = fixture_request(streaming);
    let Operation::Generation(generation) = &mut request.operation else {
        unreachable!()
    };
    generation.extensions.values.insert(
        "/__olp/openai_endpoint".into(),
        serde_json::Value::String("responses".into()),
    );
    request
}

fn responses_input_tokens_request() -> ProviderRequest {
    let wire: olp_protocols::openai::ResponseInputTokensRequest =
        serde_json::from_value(serde_json::json!({
            "model": "count-route",
            "input": [
                {"type":"message","role":"developer","content":[{"type":"input_text","text":"Be concise"}]},
                {"type":"message","role":"user","content":[{"type":"input_text","text":"Use the tool"}],"vendor_turn":true},
                {"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{\"id\":1}"},
                {"type":"function_call_output","call_id":"call_1","output":"found"}
            ],
            "tools": [{"type":"function","name":"lookup","parameters":{"type":"object"}}]
        }))
        .unwrap();
    let operation = olp_protocols::openai::decode_response_input_tokens(wire).unwrap();
    let mut request = fixture_request(false);
    request.metadata.operation = OperationKind::TokenCount;
    request.attempt.provider_model = "gpt-count-upstream".into();
    request.operation = operation;
    request
}

fn image_request(streaming: bool) -> ProviderRequest {
    let mut request = fixture_request(streaming);
    request.metadata.operation = OperationKind::ImageGeneration;
    request.attempt.provider_model = "gpt-image-1".into();
    request.operation = Operation::Images(ImageOperation::Generation(ImageGenerationRequest {
        route: RouteSlug::parse("images").unwrap(),
        prompt: "a blue square".into(),
        count: Some(1),
        size: Some("1024x1024".into()),
        stream: streaming,
        extensions: SourceExtensions::new(Surface::OpenAi, BTreeMap::new()),
    }));
    request
}

fn video_create_request() -> ProviderRequest {
    let mut request = fixture_request(false);
    request.metadata.operation = OperationKind::VideoCreate;
    request.metadata.mode = TransportMode::Async;
    request.attempt.provider_model = "sora-2".into();
    request.operation = Operation::Video(VideoOperation::Create(VideoCreateRequest {
        route: RouteSlug::parse("video").unwrap(),
        prompt: "a calm ocean".into(),
        input: Some(MediaHandle::new("video-reference")),
        extensions: SourceExtensions::new(Surface::OpenAi, BTreeMap::new()),
    }));
    request.media = Some(Arc::new(StaticMediaSpool));
    request
}

fn image_edit_request() -> ProviderRequest {
    let mut request = fixture_request(false);
    request.metadata.operation = OperationKind::ImageEdit;
    request.attempt.provider_model = "gpt-image-1".into();
    request.operation = Operation::Images(ImageOperation::Edit(ImageEditRequest {
        route: RouteSlug::parse("image-edit").unwrap(),
        images: vec![MediaHandle::new("edit-source")],
        mask: Some(MediaHandle::new("edit-mask")),
        prompt: "replace the sky".into(),
        stream: false,
        extensions: SourceExtensions::new(Surface::OpenAi, BTreeMap::new()),
    }));
    request.media = Some(Arc::new(FixtureMediaSpool::new(
        "source.png",
        "image/png",
        b"png-data",
    )));
    request
}

fn image_variation_request() -> ProviderRequest {
    let mut request = fixture_request(false);
    request.metadata.operation = OperationKind::ImageVariation;
    request.attempt.provider_model = "dall-e-2".into();
    request.operation = Operation::Images(ImageOperation::Variation(ImageVariationRequest {
        route: RouteSlug::parse("image-variation").unwrap(),
        image: MediaHandle::new("variation-source"),
        count: Some(2),
        size: Some("512x512".into()),
        extensions: SourceExtensions::new(Surface::OpenAi, BTreeMap::new()),
    }));
    request.media = Some(Arc::new(FixtureMediaSpool::new(
        "variation.png",
        "image/png",
        b"variation-data",
    )));
    request
}

fn speech_request(streaming: bool) -> ProviderRequest {
    let mut request = fixture_request(streaming);
    request.metadata.operation = OperationKind::Speech;
    request.attempt.provider_model = "gpt-4o-mini-tts".into();
    request.operation = Operation::Speech(SpeechRequest {
        route: RouteSlug::parse("speech").unwrap(),
        input: "hello from the gateway".into(),
        voice: "coral".into(),
        format: Some("mp3".into()),
        stream: streaming,
        extensions: SourceExtensions::new(Surface::OpenAi, BTreeMap::new()),
    });
    request
}

fn transcription_request(streaming: bool) -> ProviderRequest {
    let mut request = fixture_request(streaming);
    request.metadata.operation = OperationKind::Transcription;
    request.attempt.provider_model = "gpt-4o-transcribe".into();
    request.operation = Operation::Transcription(TranscriptionRequest {
        route: RouteSlug::parse("transcription").unwrap(),
        audio: MediaHandle::new("audio-source"),
        language: Some("en".into()),
        prompt: Some("Names: Ada, Grace".into()),
        stream: streaming,
        extensions: SourceExtensions::new(Surface::OpenAi, BTreeMap::new()),
    });
    request.media = Some(Arc::new(FixtureMediaSpool::new(
        "sample.wav",
        "audio/wav",
        b"wave-data",
    )));
    request
}

fn moderation_request() -> ProviderRequest {
    let mut request = fixture_request(false);
    request.metadata.operation = OperationKind::Moderation;
    request.attempt.provider_model = "omni-moderation-latest".into();
    request.operation = Operation::Moderation(ModerationRequest {
        route: RouteSlug::parse("moderation").unwrap(),
        input: vec![
            ContentPart::Text {
                text: "check this".into(),
            },
            ContentPart::Image {
                source: MediaSource::Uri("https://images.example.test/a.png".into()),
                detail: None,
            },
        ],
        extensions: SourceExtensions::new(Surface::OpenAi, BTreeMap::new()),
    });
    request
}

fn video_job_request(operation: OperationKind) -> ProviderRequest {
    let mut request = fixture_request(false);
    request.metadata.operation = operation;
    request.attempt.provider_model = "sora-2".into();
    let job = VideoJobRequest {
        route: None,
        job_id: "video_123".into(),
        extensions: SourceExtensions::new(Surface::OpenAi, BTreeMap::new()),
    };
    request.operation = Operation::Video(match operation {
        OperationKind::VideoGet => VideoOperation::Get(job),
        OperationKind::VideoContent => VideoOperation::Content(job),
        OperationKind::VideoDelete => VideoOperation::Delete(job),
        _ => panic!("unsupported video job fixture operation"),
    });
    request
}

fn test_connector(base_url: &str, timeouts: ConnectorTimeouts) -> OpenAiConnector {
    OpenAiConnector::new(
        ConnectorConfig::for_local_test(base_url, timeouts),
        OpenAiApiKey::new("upstream-secret").unwrap(),
    )
}

#[tokio::test]
async fn rejects_attempts_for_another_provider_kind_before_transport() {
    let connector = OpenAiConnector::new(
        ConnectorConfig::default(),
        OpenAiApiKey::new("upstream-secret").unwrap(),
    );
    let mut request = fixture_request(false);
    request.attempt.provider_kind = ProviderKind::Anthropic;

    let error = connector.execute(request).await.unwrap_err();
    assert_eq!(error.phase, TransportPhase::Connect);
    assert_eq!(error.class, AttemptFailureClass::Protocol);
}

#[test]
fn rejects_invalid_or_mismatched_modes_before_transport() {
    let mut image = image_request(false);
    image.metadata.mode = TransportMode::Streaming;
    assert!(validate_transport_mode(&image).is_err());

    let mut variation = image_variation_request();
    variation.metadata.mode = TransportMode::Streaming;
    assert!(validate_transport_mode(&variation).is_err());

    let mut video = video_create_request();
    assert!(validate_transport_mode(&video).is_ok());
    video.metadata.mode = TransportMode::Unary;
    assert!(validate_transport_mode(&video).is_err());
}

#[tokio::test]
async fn same_protocol_inline_audio_and_file_handles_are_rehydrated() {
    let handle = MediaHandle::new("inline-media");
    let marker = olp_domain::inline_media_marker(&handle);
    let spool: Arc<dyn MediaSpool> = Arc::new(FixtureMediaSpool::new(
        "inline.bin",
        "application/octet-stream",
        b"hi",
    ));
    let mut chat: ChatCompletionRequest = serde_json::from_value(serde_json::json!({
        "model":"upstream","messages":[{"role":"user","content":[{
            "type":"input_audio","input_audio":{"data":marker,"format":"wav"}
        }]}]
    }))
    .unwrap();
    hydrate_chat_media(&mut chat, Some(&spool)).await.unwrap();
    let ChatMessageContent::Parts(parts) = chat.messages[0].content.as_ref().unwrap() else {
        panic!("expected content parts")
    };
    let ChatContentPart::InputAudio { input_audio, .. } = &parts[0] else {
        panic!("expected input audio")
    };
    assert_eq!(input_audio.data, "aGk=");

    let mut input: ResponseInput = serde_json::from_value(serde_json::json!([{
        "type":"message","role":"user","content":[{
            "type":"input_file","filename":"brief.pdf","file_data":olp_domain::inline_media_marker(&handle)
        }]
    }]))
    .unwrap();
    hydrate_responses_media(&mut input, Some(&spool))
        .await
        .unwrap();
    let ResponseInput::Items(items) = input else {
        panic!("expected response items")
    };
    assert_eq!(
        items[0]["content"][0]["file_data"],
        "data:application/pdf;base64,aGk="
    );
}

async fn execute_error(connector: &OpenAiConnector, request: ProviderRequest) -> TransportError {
    match connector.execute(request).await {
        Ok(_) => panic!("connector unexpectedly returned a response stream"),
        Err(error) => error,
    }
}

async fn execute_events(
    connector: &OpenAiConnector,
    request: ProviderRequest,
) -> ProviderEventStream {
    match connector.execute(request).await.unwrap() {
        ProviderOutput::Events(events) => events,
        ProviderOutput::Result(_) => panic!("connector unexpectedly returned a unary result"),
    }
}

fn http_response(content_type: &str, body: &[u8]) -> Vec<u8> {
    let headers = format!(
        "HTTP/1.1 200 OK\r\nContent-Type: {content_type}\r\nContent-Length: {}\r\nConnection: close\r\n\r\n",
        body.len()
    );
    [headers.as_bytes(), body].concat()
}

fn assert_bearer_auth(request: &str) {
    assert!(
        request
            .to_ascii_lowercase()
            .contains("authorization: bearer upstream-secret")
    );
}

#[tokio::test]
async fn model_discovery_is_credentialed_and_bounded() {
    let body = br#"{"data":[{"id":"gpt-test","object":"model"}]}"#;
    let (base_url, captured) = spawn_mock(MockResponse {
        chunks: vec![(Duration::ZERO, http_response("application/json", body))],
    })
    .await;
    let connector = test_connector(&base_url, ConnectorTimeouts::default());
    let models = connector.discover_models().await.unwrap();
    assert_eq!(models[0].id, "gpt-test");
    let request = String::from_utf8(captured.await.unwrap()).unwrap();
    assert!(request.starts_with("GET /v1/models "));
    assert!(request.contains("authorization: Bearer upstream-secret"));
}

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
async fn executes_unary_chat_with_provider_model_and_late_bound_credential() {
    let body = serde_json::to_vec(&serde_json::json!({
        "id": "chatcmpl-1",
        "object": "chat.completion",
        "created": 1,
        "model": "gpt-4o-mini",
        "choices": [{
            "index": 0,
            "message": {"role": "assistant", "content": "hello back"},
            "finish_reason": "stop"
        }],
        "usage": {"prompt_tokens": 2, "completion_tokens": 2, "total_tokens": 4}
    }))
    .unwrap();
    let (base_url, captured) = spawn_mock(MockResponse {
        chunks: vec![(Duration::ZERO, http_response("application/json", &body))],
    })
    .await;
    let connector = test_connector(&base_url, ConnectorTimeouts::default());

    let mut events = execute_events(&connector, fixture_request(false)).await;
    let mut collected = Vec::new();
    while let Some(event) = events.next().await {
        collected.push(event.unwrap());
    }

    assert!(collected.iter().any(|event| matches!(
        &event.kind,
        CanonicalEventKind::TextDelta { text, .. } if text == "hello back"
    )));
    assert!(matches!(
        collected.last().map(|event| &event.kind),
        Some(CanonicalEventKind::Done)
    ));
    let captured = String::from_utf8(captured.await.unwrap()).unwrap();
    assert!(
        captured
            .to_ascii_lowercase()
            .contains("authorization: bearer upstream-secret")
    );
    assert!(captured.contains("\"model\":\"gpt-4o-mini\""));
    assert!(!captured.contains("\"model\":\"default\""));
}

#[tokio::test]
async fn responses_uses_distinct_upstream_endpoint_and_codec() {
    let body = serde_json::to_vec(&serde_json::json!({
        "id": "resp_1",
        "object": "response",
        "created_at": 1,
        "status": "completed",
        "model": "gpt-4o-mini",
        "output": [{
            "id": "msg_1",
            "type": "message",
            "role": "assistant",
            "status": "completed",
            "content": [{"type": "output_text", "text": "responses reply", "annotations": []}]
        }],
        "usage": {"input_tokens": 2, "output_tokens": 2, "total_tokens": 4}
    }))
    .unwrap();
    let (base_url, captured) = spawn_mock(MockResponse {
        chunks: vec![(Duration::ZERO, http_response("application/json", &body))],
    })
    .await;
    let connector = test_connector(&base_url, ConnectorTimeouts::default());
    let mut events = execute_events(&connector, responses_request(false)).await;
    let mut text = String::new();
    while let Some(event) = events.next().await {
        if let CanonicalEventKind::TextDelta { text: delta, .. } = event.unwrap().kind {
            text.push_str(&delta);
        }
    }
    assert_eq!(text, "responses reply");
    let request = String::from_utf8(captured.await.unwrap()).unwrap();
    assert!(request.starts_with("POST /v1/responses "));
    assert!(!request.starts_with("POST /v1/chat/completions "));
}

#[tokio::test]
async fn responses_input_tokens_forwards_full_stateless_multi_item_body() {
    let body = serde_json::to_vec(&serde_json::json!({
        "object": "response.input_tokens",
        "input_tokens": 19
    }))
    .unwrap();
    let (base_url, captured) = spawn_mock(MockResponse {
        chunks: vec![(Duration::ZERO, http_response("application/json", &body))],
    })
    .await;
    let connector = test_connector(&base_url, ConnectorTimeouts::default());
    let ProviderOutput::Result(result) = connector
        .execute(responses_input_tokens_request())
        .await
        .unwrap()
    else {
        panic!("input-token count returned a stream")
    };
    let CanonicalResult::TokenCount(result) = *result else {
        panic!("input-token count returned the wrong result kind")
    };
    assert_eq!(result.input_tokens, 19);
    let captured = String::from_utf8(captured.await.unwrap()).unwrap();
    assert!(captured.starts_with("POST /v1/responses/input_tokens "));
    assert!(captured.contains("\"model\":\"gpt-count-upstream\""));
    assert!(captured.contains("\"role\":\"developer\""));
    assert!(captured.contains("\"type\":\"function_call_output\""));
    assert!(captured.contains("\"vendor_turn\":true"));
    assert!(captured.contains("\"name\":\"lookup\""));
}

#[tokio::test]
async fn responses_input_tokens_rehydrates_bounded_media_before_transport() {
    let body = serde_json::to_vec(&serde_json::json!({
        "object": "response.input_tokens",
        "input_tokens": 7
    }))
    .unwrap();
    let (base_url, captured) = spawn_mock(MockResponse {
        chunks: vec![(Duration::ZERO, http_response("application/json", &body))],
    })
    .await;
    let connector = test_connector(&base_url, ConnectorTimeouts::default());
    let handle = MediaHandle::new("input-tokens-inline-media");
    let marker = olp_domain::inline_media_marker(&handle);
    let wire: olp_protocols::openai::ResponseInputTokensRequest =
        serde_json::from_value(serde_json::json!({
            "model":"count-route",
            "input":[{"type":"message","role":"user","content":[
                {"type":"input_audio","input_audio":{"data":marker,"format":"wav"}},
                {"type":"input_file","filename":"brief.pdf",
                 "file_data":olp_domain::inline_media_marker(&handle)}
            ]}]
        }))
        .unwrap();
    let operation = olp_protocols::openai::decode_response_input_tokens(wire).unwrap();
    let mut request = fixture_request(false);
    request.metadata.operation = OperationKind::TokenCount;
    request.attempt.provider_model = "gpt-count-upstream".into();
    request.operation = operation;
    request.media = Some(Arc::new(FixtureMediaSpool::new(
        "inline.bin",
        "application/octet-stream",
        b"hi",
    )));

    let ProviderOutput::Result(result) = connector.execute(request).await.unwrap() else {
        panic!("input-token count returned a stream")
    };
    let CanonicalResult::TokenCount(result) = *result else {
        panic!("input-token count returned the wrong result kind")
    };
    assert_eq!(result.input_tokens, 7);
    let captured = String::from_utf8(captured.await.unwrap()).unwrap();
    assert!(captured.starts_with("POST /v1/responses/input_tokens "));
    assert!(captured.contains("\"data\":\"aGk=\""));
    assert!(captured.contains("\"file_data\":\"data:application/pdf;base64,aGk=\""));
    assert!(!captured.contains("urn:olp:inline-media:"));
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
    let connector = test_connector(&base_url, ConnectorTimeouts::default());
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
    let connector = test_connector(&base_url, ConnectorTimeouts::default());
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
    let connector = test_connector(&base_url, ConnectorTimeouts::default());
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
    let connector = test_connector(&base_url, ConnectorTimeouts::default());
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
    request.attempt.provider_model = "gpt-4o-transcribe-diarize".into();
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
    let connector = test_connector(&base_url, ConnectorTimeouts::default());
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
    let connector = test_connector(&base_url, ConnectorTimeouts::default());
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
    let connector = test_connector(&base_url, ConnectorTimeouts::default());
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
    let connector = test_connector(&base_url, ConnectorTimeouts::default());
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
    let connector = test_connector(&base_url, ConnectorTimeouts::default());
    let failure = execute_error(&connector, video_job_request(OperationKind::VideoDelete)).await;
    assert_eq!(failure.class, AttemptFailureClass::UpstreamClient);

    let (base_url, _) = spawn_mock(MockResponse {
        chunks: vec![(Duration::ZERO, response)],
    })
    .await;
    let connector = test_connector(&base_url, ConnectorTimeouts::default());
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
    let connector = test_connector("http://127.0.0.1:1/v1/", ConnectorTimeouts::default());
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
    let config = ConnectorConfig::for_local_test(&base_url, ConnectorTimeouts::default())
        .with_response_limits(3, DEFAULT_MAX_EVENT_BYTES)
        .unwrap();
    let connector = OpenAiConnector::new(config, OpenAiApiKey::new("upstream-secret").unwrap());
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
async fn executes_embeddings_as_a_typed_unary_result() {
    let body = serde_json::to_vec(&serde_json::json!({
        "object": "list",
        "model": "text-embedding-3-small",
        "data": [{"object": "embedding", "index": 0, "embedding": [0.25, -0.5]}],
        "usage": {"prompt_tokens": 1, "total_tokens": 1}
    }))
    .unwrap();
    let (base_url, captured) = spawn_mock(MockResponse {
        chunks: vec![(Duration::ZERO, http_response("application/json", &body))],
    })
    .await;
    let connector = test_connector(&base_url, ConnectorTimeouts::default());

    let output = connector.execute(embeddings_request()).await.unwrap();
    let ProviderOutput::Result(result) = output else {
        panic!("connector returned the wrong output kind")
    };
    let CanonicalResult::Embeddings(result) = *result else {
        panic!("connector returned the wrong result kind")
    };
    assert_eq!(result.data[0].values, vec![0.25, -0.5]);
    let captured = String::from_utf8(captured.await.unwrap()).unwrap();
    assert!(captured.starts_with("POST /v1/embeddings "));
    assert!(captured.contains("\"model\":\"text-embedding-3-small\""));
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
async fn image_decode_failure_removes_already_staged_response_media() {
    let connector = test_connector("http://127.0.0.1:1/v1/", ConnectorTimeouts::default());
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

#[tokio::test]
#[ignore = "requires OLP_LIVE_OPENAI_API_KEY"]
async fn live_provider_discovers_openai_models() {
    let key = std::env::var("OLP_LIVE_OPENAI_API_KEY")
        .expect("set OLP_LIVE_OPENAI_API_KEY for the ignored live test");
    let connector = OpenAiConnector::new(
        ConnectorConfig::default(),
        OpenAiApiKey::new(key).expect("live OpenAI key must be representable"),
    );
    assert!(!connector.discover_models().await.unwrap().is_empty());
}
