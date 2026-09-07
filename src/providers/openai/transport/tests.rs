use std::collections::BTreeMap;
use std::future::ready;
use std::sync::Arc;
use std::sync::Mutex;
use std::sync::atomic::AtomicUsize;
use std::sync::atomic::Ordering;
use std::time::Duration;

use crate::ids::DurationMs;
use crate::ids::ProviderId;
use crate::ids::RequestId;
use crate::ids::RouteId;
use crate::ids::RouteSlug;
use crate::ids::RuntimeGenerationId;
use crate::ids::TargetId;
use crate::inference::transport::AttemptFailureClass;
use crate::inference::transport::MediaSpool;
use crate::inference::transport::MediaSpoolError;
use crate::inference::transport::MediaUpload;
use crate::inference::transport::OpenedMedia;
use crate::inference::transport::ProviderEventStream;
use crate::inference::transport::ProviderOutput;
use crate::inference::transport::ProviderRequest;
use crate::inference::transport::TransportError;
use crate::inference::transport::TransportPhase;
use crate::protocols::canonical::events::Kind;
use crate::protocols::canonical::identity::OperationKind;
use crate::protocols::canonical::identity::RequestMetadata;
use crate::protocols::canonical::identity::Surface;
use crate::protocols::canonical::identity::TransportMode;
use crate::protocols::canonical::requests::ContentPart;
use crate::protocols::canonical::requests::EmbeddingInput;
use crate::protocols::canonical::requests::EmbeddingsRequest;
use crate::protocols::canonical::requests::GenerationParameters;
use crate::protocols::canonical::requests::GenerationRequest;
use crate::protocols::canonical::requests::ImageEditRequest;
use crate::protocols::canonical::requests::ImageGenerationRequest;
use crate::protocols::canonical::requests::ImageOperation;
use crate::protocols::canonical::requests::ImageVariationRequest;
use crate::protocols::canonical::requests::MEDIA_DELETE_MISSING_IS_SUCCESS_EXTENSION;
use crate::protocols::canonical::requests::MediaHandle;
use crate::protocols::canonical::requests::MediaSource;
use crate::protocols::canonical::requests::Message;
use crate::protocols::canonical::requests::MessageRole;
use crate::protocols::canonical::requests::ModerationRequest;
use crate::protocols::canonical::requests::OPENAI_ENDPOINT_EXTENSION;
use crate::protocols::canonical::requests::Operation;
use crate::protocols::canonical::requests::SourceExtensions;
use crate::protocols::canonical::requests::SpeechRequest;
use crate::protocols::canonical::requests::TranscriptionRequest;
use crate::protocols::canonical::requests::VideoCreateRequest;
use crate::protocols::canonical::requests::VideoJobRequest;
use crate::protocols::canonical::requests::VideoOperation;
use crate::protocols::canonical::results::CanonicalResult;
use crate::protocols::canonical::results::MediaArtifact;
use crate::protocols::canonical::results::VideoStatus;
use crate::protocols::openai::chat::CompletionRequest;
use crate::protocols::openai::chat::ContentPart as OpenAiContentPart;
use crate::protocols::openai::chat::MessageContent;
use crate::protocols::openai::images::OpenAiImageResponse;
use crate::protocols::openai::responses::request::ResponseInput;
use crate::providers::connector::Timeouts;
use crate::providers::mock_server::MockResponse;
use crate::providers::mock_server::find_bytes;
use crate::providers::mock_server::response as http_response;
use crate::providers::mock_server::spawn_mock as spawn_http_mock;
use crate::providers::openai::DEFAULT_MAX_EVENT_BYTES;
use crate::providers::openai::DEFAULT_MAX_RESPONSE_BYTES;
use crate::providers::openai::transport::Connector;
use crate::providers::openai::transport::*;
use crate::providers::runtime_model::ProviderKind;
use crate::providers::transport_common::transport_error;
use crate::routes::selection::AttemptPlan;
use ::http::StatusCode;
use bytes::Bytes;
use futures::StreamExt;
use futures::stream;

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

impl MediaSpool for StaticMediaSpool {
    fn put<'a>(
        &'a self,
        _upload: MediaUpload,
    ) -> crate::inference::transport::BoxFuture<'a, Result<MediaArtifact, MediaSpoolError>> {
        Box::pin(async { Err(MediaSpoolError::Unavailable) })
    }

    fn open<'a>(
        &'a self,
        handle: &'a MediaHandle,
    ) -> crate::inference::transport::BoxFuture<'a, Result<OpenedMedia, MediaSpoolError>> {
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
    ) -> crate::inference::transport::BoxFuture<'a, Result<(), MediaSpoolError>> {
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
    ) -> crate::inference::transport::BoxFuture<'a, Result<MediaArtifact, MediaSpoolError>> {
        Box::pin(async { Err(MediaSpoolError::Unavailable) })
    }

    fn open<'a>(
        &'a self,
        handle: &'a MediaHandle,
    ) -> crate::inference::transport::BoxFuture<'a, Result<OpenedMedia, MediaSpoolError>> {
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
    ) -> crate::inference::transport::BoxFuture<'a, Result<(), MediaSpoolError>> {
        Box::pin(async { Ok(()) })
    }
}

impl MediaSpool for RecordingMediaSpool {
    fn put<'a>(
        &'a self,
        mut upload: MediaUpload,
    ) -> crate::inference::transport::BoxFuture<'a, Result<MediaArtifact, MediaSpoolError>> {
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
    ) -> crate::inference::transport::BoxFuture<'a, Result<OpenedMedia, MediaSpoolError>> {
        Box::pin(async { Err(MediaSpoolError::NotFound) })
    }

    fn remove<'a>(
        &'a self,
        _handle: &'a MediaHandle,
    ) -> crate::inference::transport::BoxFuture<'a, Result<(), MediaSpoolError>> {
        self.removes.fetch_add(1, Ordering::AcqRel);
        Box::pin(async { Ok(()) })
    }
}

async fn spawn_mock(response: MockResponse) -> (String, tokio::sync::oneshot::Receiver<Vec<u8>>) {
    spawn_http_mock("/v1/", response).await
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
            routing_id: TargetId::new(),
            provider_id: ProviderId::new(),
            provider_revision_id: uuid::Uuid::now_v7(),
            provider_kind: ProviderKind::OpenAi,
            upstream_model: "gpt-4o-mini".into(),
            timeout: DurationMs::new(2_000),
            priority: 0,
        },
        operation: Arc::new(Operation::Generation(GenerationRequest {
            route: RouteSlug::parse("default").unwrap(),
            messages: vec![Message {
                role: MessageRole::User,
                content: vec![crate::protocols::canonical::requests::ContentPart::Text {
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
        })),
        media: None,
        max_inline_media_bytes: 1024 * 1024,
        propagate_trace_context: false,
    }
}

fn embeddings_request() -> ProviderRequest {
    let mut request = fixture_request(false);
    request.metadata.operation = OperationKind::Embeddings;
    request.attempt.upstream_model = "text-embedding-3-small".into();
    request.operation = Arc::new(Operation::Embeddings(EmbeddingsRequest {
        route: RouteSlug::parse("embeddings").unwrap(),
        input: vec![EmbeddingInput::Text("hello".into())],
        dimensions: Some(2),
        extensions: SourceExtensions::new(Surface::OpenAi, BTreeMap::new()),
    }));
    request
}

fn responses_request(streaming: bool) -> ProviderRequest {
    let mut request = fixture_request(streaming);
    let Operation::Generation(generation) = Arc::make_mut(&mut request.operation) else {
        unreachable!()
    };
    generation.extensions.values.insert(
        OPENAI_ENDPOINT_EXTENSION.into(),
        serde_json::Value::String("responses".into()),
    );
    request
}

fn responses_input_tokens_request() -> ProviderRequest {
    let wire: crate::protocols::openai::responses::token_count::ResponseInputTokensRequest =
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
    let operation =
        crate::protocols::openai::responses::token_count::decode_response_input_tokens(wire)
            .unwrap();
    let mut request = fixture_request(false);
    request.metadata.operation = OperationKind::TokenCount;
    request.attempt.upstream_model = "gpt-count-upstream".into();
    request.operation = Arc::new(operation);
    request
}

fn image_request(streaming: bool) -> ProviderRequest {
    let mut request = fixture_request(streaming);
    request.metadata.operation = OperationKind::ImageGeneration;
    request.attempt.upstream_model = "gpt-image-1".into();
    request.operation = Arc::new(Operation::Images(ImageOperation::Generation(
        ImageGenerationRequest {
            route: RouteSlug::parse("images").unwrap(),
            prompt: "a blue square".into(),
            count: Some(1),
            size: Some("1024x1024".into()),
            stream: streaming,
            extensions: SourceExtensions::new(Surface::OpenAi, BTreeMap::new()),
        },
    )));
    request
}

fn video_create_request() -> ProviderRequest {
    let mut request = fixture_request(false);
    request.metadata.operation = OperationKind::VideoCreate;
    request.metadata.mode = TransportMode::Async;
    request.attempt.upstream_model = "sora-2".into();
    request.operation = Arc::new(Operation::Video(VideoOperation::Create(
        VideoCreateRequest {
            route: RouteSlug::parse("video").unwrap(),
            prompt: "a calm ocean".into(),
            input: Some(MediaHandle::new("video-reference")),
            extensions: SourceExtensions::new(Surface::OpenAi, BTreeMap::new()),
        },
    )));
    request.media = Some(Arc::new(StaticMediaSpool));
    request
}

fn image_edit_request() -> ProviderRequest {
    let mut request = fixture_request(false);
    request.metadata.operation = OperationKind::ImageEdit;
    request.attempt.upstream_model = "gpt-image-1".into();
    request.operation = Arc::new(Operation::Images(ImageOperation::Edit(ImageEditRequest {
        route: RouteSlug::parse("image-edit").unwrap(),
        images: vec![MediaHandle::new("edit-source")],
        mask: Some(MediaHandle::new("edit-mask")),
        prompt: "replace the sky".into(),
        stream: false,
        extensions: SourceExtensions::new(Surface::OpenAi, BTreeMap::new()),
    })));
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
    request.attempt.upstream_model = "dall-e-2".into();
    request.operation = Arc::new(Operation::Images(ImageOperation::Variation(
        ImageVariationRequest {
            route: RouteSlug::parse("image-variation").unwrap(),
            image: MediaHandle::new("variation-source"),
            count: Some(2),
            size: Some("512x512".into()),
            extensions: SourceExtensions::new(Surface::OpenAi, BTreeMap::new()),
        },
    )));
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
    request.attempt.upstream_model = "gpt-4o-mini-tts".into();
    request.operation = Arc::new(Operation::Speech(SpeechRequest {
        route: RouteSlug::parse("speech").unwrap(),
        input: "hello from the gateway".into(),
        voice: "coral".into(),
        format: Some("mp3".into()),
        stream: streaming,
        extensions: SourceExtensions::new(Surface::OpenAi, BTreeMap::new()),
    }));
    request
}

fn transcription_request(streaming: bool) -> ProviderRequest {
    let mut request = fixture_request(streaming);
    request.metadata.operation = OperationKind::Transcription;
    request.attempt.upstream_model = "gpt-4o-transcribe".into();
    request.operation = Arc::new(Operation::Transcription(TranscriptionRequest {
        route: RouteSlug::parse("transcription").unwrap(),
        audio: MediaHandle::new("audio-source"),
        language: Some("en".into()),
        prompt: Some("Names: Ada, Grace".into()),
        stream: streaming,
        extensions: SourceExtensions::new(Surface::OpenAi, BTreeMap::new()),
    }));
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
    request.attempt.upstream_model = "omni-moderation-latest".into();
    request.operation = Arc::new(Operation::Moderation(ModerationRequest {
        route: RouteSlug::parse("moderation").unwrap(),
        input: vec![
            ContentPart::Text {
                text: "check this".into(),
            },
            ContentPart::Image {
                source: MediaSource::Uri("https://images.example.test/a.png".into()),
                detail: None,
                mime_type: None,
            },
        ],
        extensions: SourceExtensions::new(Surface::OpenAi, BTreeMap::new()),
    }));
    request
}

fn video_job_request(operation: OperationKind) -> ProviderRequest {
    let mut request = fixture_request(false);
    request.metadata.operation = operation;
    request.attempt.upstream_model = "sora-2".into();
    let job = VideoJobRequest {
        route: None,
        job_id: "video_123".into(),
        extensions: SourceExtensions::new(Surface::OpenAi, BTreeMap::new()),
    };
    request.operation = Arc::new(Operation::Video(match operation {
        OperationKind::VideoGet => VideoOperation::Get(job),
        OperationKind::VideoContent => VideoOperation::Content(job),
        OperationKind::VideoDelete => VideoOperation::Delete(job),
        _ => panic!("unsupported video job fixture operation"),
    }));
    request
}

fn test_connector(base_url: &str, timeouts: Timeouts) -> Connector {
    Connector::new(
        ConnectorConfig::for_local_test(base_url, timeouts),
        ApiKey::new("upstream-secret").unwrap(),
    )
}

async fn execute_error(connector: &Connector, request: ProviderRequest) -> TransportError {
    match connector.execute(request).await {
        Ok(_) => panic!("connector unexpectedly returned a response stream"),
        Err(error) => error,
    }
}

async fn execute_events(connector: &Connector, request: ProviderRequest) -> ProviderEventStream {
    match connector.execute(request).await.unwrap() {
        ProviderOutput::Events(events) => events,
        ProviderOutput::Result(_) => panic!("connector unexpectedly returned a unary result"),
    }
}

fn assert_bearer_auth(request: &str) {
    assert!(
        request
            .to_ascii_lowercase()
            .contains("authorization: bearer upstream-secret")
    );
}

pub mod audio;

pub mod chat_and_responses;

pub mod media_ops;

pub mod streaming_and_timeouts;
