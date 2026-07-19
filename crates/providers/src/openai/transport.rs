use std::{
    collections::VecDeque,
    fmt,
    future::ready,
    pin::Pin,
    sync::{Arc, Mutex},
    task::{Context, Poll},
    time::Duration,
};

use base64::{Engine as _, engine::general_purpose::STANDARD};
use bytes::Bytes;
use futures::{Stream, StreamExt, stream};
use http::{HeaderMap, HeaderValue, StatusCode, header};
use olp_domain::{
    AttemptFailureClass, CanonicalEvent, CanonicalEventKind, CanonicalResult,
    DiscoveredProviderModel, MEDIA_DELETE_MISSING_IS_SUCCESS_EXTENSION, MediaArtifact, MediaSpool,
    MediaSpoolError, MediaUpload, Operation, ProviderEventStream, ProviderKind, ProviderOutput,
    ProviderRequest, ProviderTransport, Surface, TransportError, TransportMode, TransportPhase,
    media_handle_from_inline_marker,
};
use olp_protocols::openai::{
    BoundedMediaPart, ChatCompletionRequest, ChatCompletionResponse, ChatContentPart,
    ChatMessageContent, EmbeddingResponse, OpenAiChatStreamDecoder, OpenAiImageResponse,
    OpenAiModerationResponse, OpenAiResponsesStreamDecoder, OpenAiTranscriptionResponse,
    OpenAiVideoDeleteResponse, OpenAiVideoListResponse, OpenAiVideoObject, ResponseInput,
    ResponseInputTokensResponse, ResponseObject, TranscriptionResponseFormat,
    decode_chat_completion_response, decode_embedding_response, decode_image_response,
    decode_moderation_response, decode_response_input_tokens_result, decode_response_object,
    decode_speech_body, decode_transcription_response, decode_video_content_body,
    decode_video_delete_response, decode_video_list_response, decode_video_object,
    encode_chat_completion, encode_embedding_request, encode_image_edit, encode_image_generation,
    encode_image_variation, encode_moderation, encode_response_create,
    encode_response_input_tokens, encode_speech, encode_transcription, encode_video_create,
    encode_video_list,
};
use olp_protocols::sse::{SseDecoder, SseFrame};
use reqwest::{Method, Response, multipart};
use serde_json::Value;
use tokio::time::{Instant, Sleep, timeout};
use zeroize::Zeroizing;

use crate::openai::{
    ConnectorConfig, OpenAiApiKey, endpoint::EndpointError, headers::sanitize_forward_headers,
};

type ReqwestByteStream =
    Pin<Box<dyn Stream<Item = Result<Bytes, reqwest::Error>> + Send + 'static>>;

struct DeadlineResponse {
    response: Response,
    first_body_deadline: Instant,
    attempt_deadline: Instant,
}

impl std::ops::Deref for DeadlineResponse {
    type Target = Response;

    fn deref(&self) -> &Self::Target {
        &self.response
    }
}

impl DeadlineResponse {
    fn new(response: Response, first_byte_timeout: Duration, attempt_deadline: Instant) -> Self {
        Self {
            response,
            first_body_deadline: Instant::now() + first_byte_timeout,
            attempt_deadline,
        }
    }
}

pub struct OpenAiConnector {
    config: ConnectorConfig,
    api_key: OpenAiApiKey,
    auth_style: AuthStyle,
}

#[derive(Clone, Copy, Debug)]
enum AuthStyle {
    Bearer,
    ApiKeyHeader,
}

impl OpenAiConnector {
    #[must_use]
    pub fn new(config: ConnectorConfig, api_key: OpenAiApiKey) -> Self {
        Self {
            config,
            api_key,
            auth_style: AuthStyle::Bearer,
        }
    }

    /// Builds an Azure-compatible transport using the raw `api-key` header.
    /// The endpoint retains the same DNS pinning, redirect, retry, and private
    /// address protections as the ordinary OpenAI connector.
    #[must_use]
    pub fn new_with_api_key_header(config: ConnectorConfig, api_key: OpenAiApiKey) -> Self {
        Self {
            config,
            api_key,
            auth_style: AuthStyle::ApiKeyHeader,
        }
    }

    fn attach_auth(&self, headers: &mut HeaderMap) -> Result<(), TransportError> {
        match self.auth_style {
            AuthStyle::Bearer => {
                headers.insert(header::AUTHORIZATION, bearer_header(&self.api_key)?);
            }
            AuthStyle::ApiKeyHeader => {
                headers.insert("api-key", raw_api_key_header(&self.api_key)?);
            }
        }
        Ok(())
    }

    /// Performs a credentialed, SSRF-hardened model-catalog request. This is
    /// intentionally separate from inference so management discovery never
    /// consumes the routing retry budget.
    pub async fn discover_models(&self) -> Result<Vec<DiscoveredProviderModel>, TransportError> {
        let attempt_deadline = Instant::now()
            + self.config.timeouts.connect
            + self.config.timeouts.first_byte
            + self.config.timeouts.idle;
        let client = self
            .config
            .endpoint
            .pinned_client(self.config.timeouts.connect)
            .await
            .map_err(map_endpoint_error)?;
        let url = self
            .config
            .endpoint
            .resource_url("models")
            .map_err(map_endpoint_error)?;
        let mut headers = sanitize_forward_headers(&HeaderMap::new());
        self.attach_auth(&mut headers)?;
        headers.insert(header::ACCEPT, HeaderValue::from_static("application/json"));
        let first_byte_deadline = Instant::now() + self.config.timeouts.first_byte;
        let response = timeout(
            self.config.timeouts.first_byte,
            client.get(url).headers(headers).send(),
        )
        .await
        .map_err(|_| first_byte_timeout())?
        .map_err(map_send_error)?;
        if !response.status().is_success() {
            return Err(self.map_error_response(response, attempt_deadline).await);
        }
        require_content_type(&response, "application/json")?;
        let body = read_bounded_body(
            response,
            first_byte_deadline,
            attempt_deadline,
            self.config.timeouts.idle,
            self.config.max_response_bytes,
        )
        .await?;
        let value: serde_json::Value = serde_json::from_slice(&body).map_err(|error| {
            protocol_body_error(format!("OpenAI model discovery is not valid JSON: {error}"))
        })?;
        let data = value
            .get("data")
            .and_then(serde_json::Value::as_array)
            .ok_or_else(|| protocol_body_error("OpenAI model discovery omitted data"))?;
        data.iter()
            .map(|model| {
                let id = model
                    .get("id")
                    .and_then(serde_json::Value::as_str)
                    .filter(|id| !id.is_empty())
                    .ok_or_else(|| {
                        protocol_body_error("OpenAI model discovery returned an invalid ID")
                    })?;
                Ok(DiscoveredProviderModel {
                    id: id.to_owned(),
                    display_name: id.to_owned(),
                })
            })
            .collect()
    }

    async fn execute_request(
        &self,
        request: ProviderRequest,
    ) -> Result<ProviderOutput, TransportError> {
        let provider_kind_matches = match self.auth_style {
            AuthStyle::Bearer => matches!(
                request.attempt.provider_kind,
                ProviderKind::OpenAi | ProviderKind::OpenAiCompatible
            ),
            // Compatibility probes use OpenAiCompatible before the Azure
            // wrapper executes real attempts as AzureOpenAi.
            AuthStyle::ApiKeyHeader => matches!(
                request.attempt.provider_kind,
                ProviderKind::AzureOpenAi | ProviderKind::OpenAiCompatible
            ),
        };
        if !provider_kind_matches {
            return Err(transport_error(
                TransportPhase::Connect,
                AttemptFailureClass::Protocol,
                false,
                "OpenAI connector received an attempt for another provider kind",
            ));
        }
        if request.metadata.operation != request.operation.kind() {
            return Err(transport_error(
                TransportPhase::Connect,
                AttemptFailureClass::Protocol,
                false,
                "request metadata operation does not match the canonical operation",
            ));
        }
        validate_transport_mode(&request)?;
        if !matches!(request.operation, Operation::Generation(_)) {
            return self.execute_result_request(request).await;
        }
        let Operation::Generation(generation) = &request.operation else {
            unreachable!("checked above")
        };
        let mut generation = generation.clone();
        let responses_endpoint = generation
            .extensions
            .values
            .remove("/__olp/openai_endpoint")
            .and_then(|value| value.as_str().map(str::to_owned))
            .is_some_and(|endpoint| endpoint == "responses");
        generation
            .extensions
            .ensure_representable_on(Surface::OpenAi)
            .map_err(|error| {
                transport_error(
                    TransportPhase::Connect,
                    AttemptFailureClass::Protocol,
                    false,
                    error.to_string(),
                )
            })?;

        let streaming = request.metadata.mode == TransportMode::Streaming;
        let body = if responses_endpoint {
            let mut wire = encode_response_create(&generation, &request.attempt.provider_model)
                .map_err(|error| protocol_encode_error("Responses", error))?;
            hydrate_responses_media(&mut wire.input, request.media.as_ref()).await?;
            serialize_wire("Responses", &wire)?
        } else {
            let mut wire = encode_chat_completion(&generation, &request.attempt.provider_model)
                .map_err(|error| protocol_encode_error("chat", error))?;
            if streaming {
                require_stream_usage(&mut wire)?;
            }
            hydrate_chat_media(&mut wire, request.media.as_ref()).await?;
            serialize_wire("chat", &wire)?
        };

        let started = Instant::now();
        let attempt_deadline = started + request.attempt.timeout.as_duration();
        let connect_timeout = bounded_duration(
            self.config.timeouts.connect,
            remaining(attempt_deadline, TransportPhase::Connect)?,
        );
        // Resolution is validated and pinned before any credential is copied
        // into an HTTP header or request object.
        let client = self
            .config
            .endpoint
            .pinned_client(connect_timeout)
            .await
            .map_err(map_endpoint_error)?;
        let url = self
            .config
            .endpoint
            .resource_url(if responses_endpoint {
                "responses"
            } else {
                "chat/completions"
            })
            .map_err(map_endpoint_error)?;

        let first_byte_deadline = Instant::now() + self.config.timeouts.first_byte;
        let mut headers = sanitize_forward_headers(&HeaderMap::new());
        self.attach_auth(&mut headers)?;
        headers.insert(
            header::CONTENT_TYPE,
            HeaderValue::from_static("application/json"),
        );
        headers.insert(
            header::ACCEPT,
            HeaderValue::from_static(if streaming {
                "text/event-stream"
            } else {
                "application/json"
            }),
        );
        headers.insert(
            "x-request-id",
            HeaderValue::from_str(&request.metadata.request_id.to_string()).map_err(|_| {
                transport_error(
                    TransportPhase::Connect,
                    AttemptFailureClass::Protocol,
                    false,
                    "request ID cannot be represented as an HTTP header",
                )
            })?,
        );

        let send_wait = remaining_until(first_byte_deadline, attempt_deadline)
            .ok_or_else(first_byte_timeout)?;
        let response = timeout(
            send_wait,
            client.post(url).headers(headers).body(body).send(),
        )
        .await
        .map_err(|_| first_byte_timeout())?
        .map_err(map_send_error)?;

        if !response.status().is_success() {
            return Err(self.map_error_response(response, attempt_deadline).await);
        }

        let events = if streaming {
            self.streaming_response(
                response,
                first_byte_deadline,
                attempt_deadline,
                responses_endpoint,
            )
            .await
        } else {
            self.unary_response(
                response,
                first_byte_deadline,
                attempt_deadline,
                responses_endpoint,
            )
            .await
        }?;
        Ok(ProviderOutput::Events(events))
    }

    async fn execute_result_request(
        &self,
        request: ProviderRequest,
    ) -> Result<ProviderOutput, TransportError> {
        if matches!(request.operation, Operation::Images(_)) {
            return self.execute_image_request(request).await;
        }
        if matches!(request.operation, Operation::Speech(_)) {
            return self.execute_speech_request(request).await;
        }
        if matches!(request.operation, Operation::Transcription(_)) {
            return self.execute_transcription_request(request).await;
        }
        if matches!(request.operation, Operation::Video(_)) {
            return self.execute_video_request(request).await;
        }
        let (path, body, result_kind) = match &request.operation {
            Operation::Embeddings(operation) => {
                let wire = encode_embedding_request(operation, &request.attempt.provider_model)
                    .map_err(|error| protocol_encode_error("embeddings", error))?;
                (
                    "embeddings",
                    serialize_wire("embeddings", &wire)?,
                    ResultKind::Embeddings,
                )
            }
            Operation::TokenCount(operation) => {
                let mut wire =
                    encode_response_input_tokens(operation, &request.attempt.provider_model)
                        .map_err(|error| protocol_encode_error("input-token count", error))?;
                hydrate_responses_media(&mut wire.input, request.media.as_ref()).await?;
                (
                    "responses/input_tokens",
                    serialize_wire("input-token count", &wire)?,
                    ResultKind::TokenCount,
                )
            }
            Operation::Moderation(operation) => {
                let wire = encode_moderation(operation, &request.attempt.provider_model)
                    .map_err(|error| protocol_encode_error("moderation", error))?;
                (
                    "moderations",
                    serialize_wire("moderation", &wire)?,
                    ResultKind::Moderation,
                )
            }
            operation => {
                return Err(transport_error(
                    TransportPhase::Connect,
                    AttemptFailureClass::Protocol,
                    false,
                    format!(
                        "OpenAI connector does not yet transport {:?}",
                        operation.kind()
                    ),
                ));
            }
        };
        let response = self.post_unary_json(&request, path, body).await?;
        let result = match result_kind {
            ResultKind::Embeddings => {
                let wire: EmbeddingResponse = parse_wire("embeddings", &response)?;
                CanonicalResult::Embeddings(
                    decode_embedding_response(wire)
                        .map_err(|error| protocol_decode_error("embeddings", error))?,
                )
            }
            ResultKind::TokenCount => {
                let wire: ResponseInputTokensResponse = parse_wire("input-token count", &response)?;
                CanonicalResult::TokenCount(decode_response_input_tokens_result(wire))
            }
            ResultKind::Moderation => {
                let wire: OpenAiModerationResponse = parse_wire("moderation", &response)?;
                CanonicalResult::Moderation(decode_moderation_response(wire))
            }
        };
        Ok(ProviderOutput::Result(Box::new(result)))
    }

    async fn execute_image_request(
        &self,
        request: ProviderRequest,
    ) -> Result<ProviderOutput, TransportError> {
        let Operation::Images(operation) = &request.operation else {
            unreachable!("checked by caller")
        };
        let (path, body) = match operation {
            olp_domain::ImageOperation::Generation(operation) => {
                let wire = encode_image_generation(operation, &request.attempt.provider_model)
                    .map_err(|error| protocol_encode_error("image generation", error))?;
                (
                    "images/generations",
                    serialize_wire("image generation", &wire)?,
                )
            }
            olp_domain::ImageOperation::Edit(_) | olp_domain::ImageOperation::Variation(_) => {
                return self.execute_image_multipart_request(request).await;
            }
        };
        let response = self.post_raw_json(&request, path, body).await?;
        if request.metadata.mode == TransportMode::Streaming {
            require_content_type(&response, "text/event-stream")?;
            return Ok(ProviderOutput::Events(self.raw_sse_response(response)?));
        }
        require_content_type(&response, "application/json")?;
        let bytes = read_deadline_body(
            response,
            self.config.timeouts.idle,
            self.config.max_response_bytes,
        )
        .await?;
        let wire: OpenAiImageResponse = parse_wire("image", &bytes)?;
        let result = self.decode_image_result(&request, wire).await?;
        Ok(ProviderOutput::Result(Box::new(CanonicalResult::Images(
            result,
        ))))
    }

    async fn decode_image_result(
        &self,
        request: &ProviderRequest,
        mut wire: OpenAiImageResponse,
    ) -> Result<olp_domain::ImagesResult, TransportError> {
        let mut handles = VecDeque::new();
        let mut staged = Vec::new();
        let decoded = async {
            for (index, image) in wire.data.iter_mut().enumerate() {
                let Some(encoded) = image.b64_json.take() else {
                    continue;
                };
                let spool = request.media.as_ref().ok_or_else(|| {
                    protocol_body_error("the OpenAI image response requires a bounded media spool")
                })?;
                let bytes = STANDARD
                    .decode(encoded)
                    .map_err(|error| protocol_decode_error("image base64", error))?;
                if bytes.len() > self.config.max_response_bytes {
                    return Err(protocol_body_error(
                        "OpenAI image payload exceeded the configured response bound",
                    ));
                }
                let artifact = spool
                    .put(MediaUpload {
                        filename: format!("image-{index}.bin"),
                        content_type: Some("application/octet-stream".into()),
                        maximum_length: u64::try_from(self.config.max_response_bytes)
                            .unwrap_or(u64::MAX),
                        bytes: Box::pin(stream::once(ready(Ok(Bytes::from(bytes))))),
                    })
                    .await
                    .map_err(map_spool_error)?;
                staged.push(artifact.handle.clone());
                handles.push_back(artifact.handle);
                image.b64_json = Some(String::new());
            }
            decode_image_response(wire, |_| {
                handles.pop_front().ok_or_else(|| {
                    olp_protocols::openai::ImageCodecError::Staging(
                        "image spool handle was unavailable".into(),
                    )
                })
            })
            .map_err(|error| protocol_decode_error("image", error))
        }
        .await;
        if decoded.is_err()
            && let Some(spool) = request.media.as_ref()
        {
            for handle in staged {
                let _ = spool.remove(&handle).await;
            }
        }
        decoded
    }

    async fn execute_speech_request(
        &self,
        request: ProviderRequest,
    ) -> Result<ProviderOutput, TransportError> {
        let Operation::Speech(operation) = &request.operation else {
            unreachable!("checked by caller")
        };
        let wire = encode_speech(operation, &request.attempt.provider_model)
            .map_err(|error| protocol_encode_error("speech", error))?;
        let response = self
            .post_raw_json(&request, "audio/speech", serialize_wire("speech", &wire)?)
            .await?;
        if request.metadata.mode == TransportMode::Streaming {
            require_content_type(&response, "text/event-stream")?;
            return Ok(ProviderOutput::Events(self.raw_sse_response(response)?));
        }
        let content_type = response
            .headers()
            .get(header::CONTENT_TYPE)
            .and_then(|value| value.to_str().ok())
            .and_then(|value| value.split(';').next())
            .map(str::trim)
            .filter(|value| value.starts_with("audio/") || *value == "application/octet-stream")
            .ok_or_else(|| {
                protocol_body_error("OpenAI speech response used an invalid content type")
            })?
            .to_owned();
        let spool = request.media.as_ref().ok_or_else(|| {
            protocol_body_error("the OpenAI speech response requires a bounded media spool")
        })?;
        let maximum = u64::try_from(self.config.max_response_bytes).unwrap_or(u64::MAX);
        let artifact = spool_response_body(
            response,
            spool,
            "speech-output.bin".into(),
            Some(content_type),
            maximum,
            self.config.timeouts.idle,
        )
        .await?;
        Ok(ProviderOutput::Result(Box::new(CanonicalResult::Speech(
            decode_speech_body(olp_protocols::openai::BinaryMediaBody { media: artifact }),
        ))))
    }

    fn raw_sse_response(
        &self,
        response: DeadlineResponse,
    ) -> Result<ProviderEventStream, TransportError> {
        let source: ReqwestByteStream = Box::pin(response.response.bytes_stream());
        let bytes = DeadlineByteStream::new(
            source,
            response.first_body_deadline,
            self.config.timeouts.idle,
            response.attempt_deadline,
        );
        Ok(Box::pin(RawSseEventStream::new(
            bytes,
            self.config.max_event_bytes,
        )))
    }

    async fn post_raw_json(
        &self,
        request: &ProviderRequest,
        path: &str,
        body: Vec<u8>,
    ) -> Result<DeadlineResponse, TransportError> {
        let started = Instant::now();
        let attempt_deadline = started + request.attempt.timeout.as_duration();
        let connect_timeout = bounded_duration(
            self.config.timeouts.connect,
            remaining(attempt_deadline, TransportPhase::Connect)?,
        );
        let client = self
            .config
            .endpoint
            .pinned_client(connect_timeout)
            .await
            .map_err(map_endpoint_error)?;
        let url = self
            .config
            .endpoint
            .resource_url(path)
            .map_err(map_endpoint_error)?;
        let mut headers = sanitize_forward_headers(&HeaderMap::new());
        self.attach_auth(&mut headers)?;
        headers.insert(
            header::CONTENT_TYPE,
            HeaderValue::from_static("application/json"),
        );
        let wait = bounded_duration(
            self.config.timeouts.first_byte,
            remaining(attempt_deadline, TransportPhase::FirstByte)?,
        );
        let response = timeout(wait, client.post(url).headers(headers).body(body).send())
            .await
            .map_err(|_| first_byte_timeout())?
            .map_err(map_send_error)?;
        if !response.status().is_success() {
            return Err(self.map_error_response(response, attempt_deadline).await);
        }
        Ok(DeadlineResponse::new(
            response,
            self.config.timeouts.first_byte,
            attempt_deadline,
        ))
    }

    async fn execute_image_multipart_request(
        &self,
        request: ProviderRequest,
    ) -> Result<ProviderOutput, TransportError> {
        let spool = request.media.as_ref().ok_or_else(|| {
            protocol_body_error("OpenAI image uploads require a bounded media spool")
        })?;
        let Operation::Images(operation) = &request.operation else {
            unreachable!("checked by caller")
        };
        let mut form = multipart::Form::new();
        let path;
        match operation {
            olp_domain::ImageOperation::Edit(operation) => {
                let mut parts = VecDeque::new();
                for handle in operation.images.iter().chain(operation.mask.iter()) {
                    parts.push_back(bounded_part(spool.as_ref(), handle, 50 * 1024 * 1024).await?);
                }
                let wire = encode_image_edit(operation, &request.attempt.provider_model, |_| {
                    parts.pop_front().ok_or_else(|| {
                        olp_protocols::openai::ImageCodecError::InvalidMediaPart(
                            "media spool metadata was unavailable".into(),
                        )
                    })
                })
                .map_err(|error| protocol_encode_error("image edit", error))?;
                form = form
                    .text("model", wire.model.clone())
                    .text("prompt", wire.prompt.clone());
                for (index, handle) in operation.images.iter().enumerate() {
                    let opened = spool.open(handle).await.map_err(map_spool_error)?;
                    let field = if operation.images.len() == 1 {
                        "image".to_owned()
                    } else {
                        format!("image[{index}]")
                    };
                    form = form.part(field, multipart_part(opened)?);
                }
                if let Some(mask) = &operation.mask {
                    form = form.part(
                        "mask",
                        multipart_part(spool.open(mask).await.map_err(map_spool_error)?)?,
                    );
                }
                form = add_image_edit_fields(form, &wire);
                path = "images/edits";
            }
            olp_domain::ImageOperation::Variation(operation) => {
                let metadata =
                    bounded_part(spool.as_ref(), &operation.image, 50 * 1024 * 1024).await?;
                let wire =
                    encode_image_variation(operation, &request.attempt.provider_model, |_| {
                        Ok(metadata.clone())
                    })
                    .map_err(|error| protocol_encode_error("image variation", error))?;
                form = form.text("model", wire.model).part(
                    "image",
                    multipart_part(
                        spool
                            .open(&operation.image)
                            .await
                            .map_err(map_spool_error)?,
                    )?,
                );
                form = add_optional_text(form, "n", wire.n.map(|value| value.to_string()));
                form = add_optional_text(form, "size", wire.size);
                form = add_optional_text(form, "response_format", wire.response_format);
                form = add_optional_text(form, "user", wire.user);
                form = add_extra_fields(form, wire.extra);
                path = "images/variations";
            }
            olp_domain::ImageOperation::Generation(_) => {
                unreachable!("generation uses JSON transport")
            }
        }
        let response = self.post_multipart_raw(&request, path, form).await?;
        if request.metadata.mode == TransportMode::Streaming {
            require_content_type(&response, "text/event-stream")?;
            return Ok(ProviderOutput::Events(self.raw_sse_response(response)?));
        }
        require_content_type(&response, "application/json")?;
        let response = read_deadline_body(
            response,
            self.config.timeouts.idle,
            self.config.max_response_bytes,
        )
        .await?;
        let wire: OpenAiImageResponse = parse_wire("image", &response)?;
        let result = self.decode_image_result(&request, wire).await?;
        Ok(ProviderOutput::Result(Box::new(CanonicalResult::Images(
            result,
        ))))
    }

    async fn execute_transcription_request(
        &self,
        request: ProviderRequest,
    ) -> Result<ProviderOutput, TransportError> {
        let Operation::Transcription(operation) = &request.operation else {
            unreachable!("checked by caller")
        };
        let spool = request.media.as_ref().ok_or_else(|| {
            protocol_body_error("OpenAI transcription requires a bounded media spool")
        })?;
        let metadata = bounded_part(spool.as_ref(), &operation.audio, 25 * 1024 * 1024).await?;
        let wire = encode_transcription(operation, &request.attempt.provider_model, |_| {
            Ok(metadata.clone())
        })
        .map_err(|error| protocol_encode_error("transcription", error))?;
        let opened = spool
            .open(&operation.audio)
            .await
            .map_err(map_spool_error)?;
        let mut form = multipart::Form::new()
            .text("model", wire.model)
            .part("file", multipart_part(opened)?);
        form = add_optional_text(form, "language", wire.language);
        form = add_optional_text(form, "prompt", wire.prompt);
        form = add_optional_text(form, "response_format", wire.response_format.clone());
        form = add_optional_text(
            form,
            "temperature",
            wire.temperature.map(|value| value.to_string()),
        );
        if !wire.include.is_empty() {
            for value in wire.include {
                form = form.text("include[]", value);
            }
        }
        if !wire.timestamp_granularities.is_empty() {
            for value in wire.timestamp_granularities {
                form = form.text("timestamp_granularities[]", value);
            }
        }
        if let Some(value) = wire.chunking_strategy {
            form = form.text("chunking_strategy", value.to_string());
        }
        form = form.text("stream", wire.stream.to_string());
        form = add_extra_fields(form, wire.extra);
        let response = self
            .post_multipart_raw(&request, "audio/transcriptions", form)
            .await?;
        if request.metadata.mode == TransportMode::Streaming {
            require_content_type(&response, "text/event-stream")?;
            return Ok(ProviderOutput::Events(self.raw_sse_response(response)?));
        }
        let response_format = TranscriptionResponseFormat::parse(wire.response_format.as_deref())
            .map_err(|error| protocol_encode_error("transcription", error))?;
        if response_format.is_text() {
            require_transcription_text_content_type(&response, response_format)?;
        } else {
            require_content_type(&response, "application/json")?;
        }
        let bytes = read_deadline_body(
            response,
            self.config.timeouts.idle,
            self.config.max_response_bytes,
        )
        .await?;
        let response = if response_format.is_text() {
            OpenAiTranscriptionResponse::Text(
                String::from_utf8(bytes)
                    .map_err(|error| protocol_decode_error("transcription text", error))?,
            )
        } else {
            parse_wire("transcription", &bytes)?
        };
        Ok(ProviderOutput::Result(Box::new(
            CanonicalResult::Transcription(decode_transcription_response(response)),
        )))
    }

    async fn execute_video_request(
        &self,
        request: ProviderRequest,
    ) -> Result<ProviderOutput, TransportError> {
        let Operation::Video(operation) = &request.operation else {
            unreachable!("checked by caller")
        };
        match operation {
            olp_domain::VideoOperation::Create(operation) => {
                let reference = if let Some(handle) = operation.input.as_ref() {
                    let spool = request.media.as_ref().ok_or_else(|| {
                        protocol_body_error("OpenAI video input requires a bounded media spool")
                    })?;
                    Some(
                        bounded_part(
                            spool.as_ref(),
                            handle,
                            olp_protocols::openai::DEFAULT_VIDEO_REFERENCE_LIMIT,
                        )
                        .await?,
                    )
                } else {
                    None
                };
                let mut reference_metadata = reference.clone();
                let wire = encode_video_create(operation, &request.attempt.provider_model, |_| {
                    reference_metadata.take().ok_or_else(|| {
                        olp_protocols::openai::VideoCodecError::Staging(
                            "video input spool metadata was unavailable".into(),
                        )
                    })
                })
                .map_err(|error| protocol_encode_error("video create", error))?;
                let mut form = multipart::Form::new()
                    .text("model", wire.model)
                    .text("prompt", wire.prompt);
                form = add_optional_text(form, "seconds", wire.seconds);
                form = add_optional_text(form, "size", wire.size);
                if let Some(handle) = operation.input.as_ref() {
                    let spool = request.media.as_ref().expect("validated above");
                    form = form.part(
                        "input_reference",
                        multipart_part(spool.open(handle).await.map_err(map_spool_error)?)?,
                    );
                }
                form = add_extra_fields(form, wire.extra);
                let response = self.post_multipart_raw(&request, "videos", form).await?;
                require_content_type(&response, "application/json")?;
                let bytes = read_deadline_body(
                    response,
                    self.config.timeouts.idle,
                    self.config.max_response_bytes,
                )
                .await?;
                let wire: OpenAiVideoObject = parse_wire("video create", &bytes)?;
                let result = decode_video_object(wire)
                    .map_err(|error| protocol_decode_error("video create", error))?;
                Ok(ProviderOutput::Result(Box::new(CanonicalResult::VideoJob(
                    result,
                ))))
            }
            olp_domain::VideoOperation::List(operation) => {
                let wire = encode_video_list(operation)
                    .map_err(|error| protocol_encode_error("video list", error))?;
                let mut path = "videos".to_owned();
                let mut query = Vec::new();
                if let Some(after) = wire.after {
                    query.push(("after", after));
                }
                if let Some(limit) = wire.limit {
                    query.push(("limit", limit.to_string()));
                }
                if let Some(order) = wire.order {
                    query.push(("order", order));
                }
                if !query.is_empty() {
                    path.push('?');
                    path.push_str(
                        &query
                            .into_iter()
                            .map(|(name, value)| format!("{name}={}", percent_encode(&value)))
                            .collect::<Vec<_>>()
                            .join("&"),
                    );
                }
                let bytes = self
                    .request_json(&request, Method::GET, &path, None)
                    .await?;
                let wire: OpenAiVideoListResponse = parse_wire("video list", &bytes)?;
                let result = decode_video_list_response(wire)
                    .map_err(|error| protocol_decode_error("video list", error))?;
                Ok(ProviderOutput::Result(Box::new(
                    CanonicalResult::VideoList(result),
                )))
            }
            olp_domain::VideoOperation::Get(operation) => {
                let path = video_job_path(&operation.job_id, None)?;
                let bytes = self
                    .request_json(&request, Method::GET, &path, None)
                    .await?;
                let wire: OpenAiVideoObject = parse_wire("video get", &bytes)?;
                let result = decode_video_object(wire)
                    .map_err(|error| protocol_decode_error("video get", error))?;
                Ok(ProviderOutput::Result(Box::new(CanonicalResult::VideoJob(
                    result,
                ))))
            }
            olp_domain::VideoOperation::Content(operation) => {
                let variant = operation
                    .extensions
                    .values
                    .get("/variant")
                    .and_then(serde_json::Value::as_str);
                let path = video_job_path(&operation.job_id, Some("content"))?;
                let path = variant.map_or(path.clone(), |variant| {
                    format!("{path}?variant={}", percent_encode(variant))
                });
                let response = self
                    .request_raw(&request, Method::GET, &path, None, "*/*")
                    .await?;
                let content_type = response
                    .headers()
                    .get(header::CONTENT_TYPE)
                    .and_then(|value| value.to_str().ok())
                    .and_then(|value| value.split(';').next())
                    .map(str::trim)
                    .filter(|value| value.starts_with("video/") || value.starts_with("image/"))
                    .ok_or_else(|| {
                        protocol_body_error("OpenAI video content used an invalid content type")
                    })?
                    .to_owned();
                let spool = request.media.as_ref().ok_or_else(|| {
                    protocol_body_error("OpenAI video content requires a bounded media spool")
                })?;
                let maximum = u64::try_from(self.config.max_response_bytes).unwrap_or(u64::MAX);
                let artifact = spool_response_body(
                    response,
                    spool,
                    format!("video-content-{}.bin", operation.job_id),
                    Some(content_type),
                    maximum,
                    self.config.timeouts.idle,
                )
                .await?;
                Ok(ProviderOutput::Result(Box::new(
                    CanonicalResult::VideoContent(decode_video_content_body(
                        olp_protocols::openai::BinaryMediaBody { media: artifact },
                    )),
                )))
            }
            olp_domain::VideoOperation::Delete(operation) => {
                let path = video_job_path(&operation.job_id, None)?;
                let reconcile_missing = operation
                    .extensions
                    .values
                    .get(MEDIA_DELETE_MISSING_IS_SUCCESS_EXTENSION)
                    .and_then(serde_json::Value::as_bool)
                    .unwrap_or(false);
                let response = self
                    .request_raw_unchecked(
                        &request,
                        Method::DELETE,
                        &path,
                        None,
                        "application/json",
                    )
                    .await?;
                if response.status() == StatusCode::NOT_FOUND && reconcile_missing {
                    return Ok(ProviderOutput::Result(Box::new(
                        CanonicalResult::VideoDelete(olp_domain::VideoDeleteResult {
                            id: operation.job_id.clone(),
                            deleted: true,
                            extensions: olp_domain::SourceExtensions::new(
                                Surface::OpenAi,
                                std::collections::BTreeMap::new(),
                            ),
                        }),
                    )));
                }
                if !response.status().is_success() {
                    return Err(self
                        .map_error_response(response.response, response.attempt_deadline)
                        .await);
                }
                require_content_type(&response, "application/json")?;
                let bytes = read_deadline_body(
                    response,
                    self.config.timeouts.idle,
                    self.config.max_response_bytes,
                )
                .await?;
                let wire: OpenAiVideoDeleteResponse = parse_wire("video delete", &bytes)?;
                Ok(ProviderOutput::Result(Box::new(
                    CanonicalResult::VideoDelete(decode_video_delete_response(wire)),
                )))
            }
        }
    }

    async fn request_json(
        &self,
        request: &ProviderRequest,
        method: Method,
        path: &str,
        body: Option<Vec<u8>>,
    ) -> Result<Vec<u8>, TransportError> {
        let response = self
            .request_raw(request, method, path, body, "application/json")
            .await?;
        require_content_type(&response, "application/json")?;
        read_deadline_body(
            response,
            self.config.timeouts.idle,
            self.config.max_response_bytes,
        )
        .await
    }

    async fn request_raw(
        &self,
        request: &ProviderRequest,
        method: Method,
        path: &str,
        body: Option<Vec<u8>>,
        accept: &'static str,
    ) -> Result<DeadlineResponse, TransportError> {
        let response = self
            .request_raw_unchecked(request, method, path, body, accept)
            .await?;
        if !response.status().is_success() {
            return Err(self
                .map_error_response(response.response, response.attempt_deadline)
                .await);
        }
        Ok(response)
    }

    async fn request_raw_unchecked(
        &self,
        request: &ProviderRequest,
        method: Method,
        path: &str,
        body: Option<Vec<u8>>,
        accept: &'static str,
    ) -> Result<DeadlineResponse, TransportError> {
        let started = Instant::now();
        let attempt_deadline = started + request.attempt.timeout.as_duration();
        let connect_timeout = bounded_duration(
            self.config.timeouts.connect,
            remaining(attempt_deadline, TransportPhase::Connect)?,
        );
        let client = self
            .config
            .endpoint
            .pinned_client(connect_timeout)
            .await
            .map_err(map_endpoint_error)?;
        let (resource, query) = path
            .split_once('?')
            .map_or((path, None), |(path, query)| (path, Some(query)));
        let mut url = self
            .config
            .endpoint
            .resource_url(resource)
            .map_err(map_endpoint_error)?;
        if let Some(query) = query {
            let combined = url.query().map_or_else(
                || query.to_owned(),
                |existing| format!("{existing}&{query}"),
            );
            url.set_query(Some(&combined));
        }
        let mut headers = sanitize_forward_headers(&HeaderMap::new());
        self.attach_auth(&mut headers)?;
        headers.insert(header::ACCEPT, HeaderValue::from_static(accept));
        let mut builder = client.request(method, url).headers(headers);
        if let Some(body) = body {
            builder = builder
                .header(header::CONTENT_TYPE, "application/json")
                .body(body);
        }
        let wait = bounded_duration(
            self.config.timeouts.first_byte,
            remaining(attempt_deadline, TransportPhase::FirstByte)?,
        );
        let response = timeout(wait, builder.send())
            .await
            .map_err(|_| first_byte_timeout())?
            .map_err(map_send_error)?;
        Ok(DeadlineResponse::new(
            response,
            self.config.timeouts.first_byte,
            attempt_deadline,
        ))
    }

    async fn post_multipart_raw(
        &self,
        request: &ProviderRequest,
        path: &str,
        form: multipart::Form,
    ) -> Result<DeadlineResponse, TransportError> {
        let started = Instant::now();
        let attempt_deadline = started + request.attempt.timeout.as_duration();
        let connect_timeout = bounded_duration(
            self.config.timeouts.connect,
            remaining(attempt_deadline, TransportPhase::Connect)?,
        );
        let client = self
            .config
            .endpoint
            .pinned_client(connect_timeout)
            .await
            .map_err(map_endpoint_error)?;
        let url = self
            .config
            .endpoint
            .resource_url(path)
            .map_err(map_endpoint_error)?;
        let mut headers = sanitize_forward_headers(&HeaderMap::new());
        self.attach_auth(&mut headers)?;
        headers.insert(header::ACCEPT, HeaderValue::from_static("application/json"));
        let wait = bounded_duration(
            self.config.timeouts.first_byte,
            remaining(attempt_deadline, TransportPhase::FirstByte)?,
        );
        let response = timeout(
            wait,
            client.post(url).headers(headers).multipart(form).send(),
        )
        .await
        .map_err(|_| ambiguous_multipart_timeout())?
        .map_err(map_ambiguous_send_error)?;
        if !response.status().is_success() {
            return Err(self.map_error_response(response, attempt_deadline).await);
        }
        Ok(DeadlineResponse::new(
            response,
            self.config.timeouts.first_byte,
            attempt_deadline,
        ))
    }

    async fn post_unary_json(
        &self,
        request: &ProviderRequest,
        path: &str,
        body: Vec<u8>,
    ) -> Result<Vec<u8>, TransportError> {
        let started = Instant::now();
        let attempt_deadline = started + request.attempt.timeout.as_duration();
        let connect_timeout = bounded_duration(
            self.config.timeouts.connect,
            remaining(attempt_deadline, TransportPhase::Connect)?,
        );
        let client = self
            .config
            .endpoint
            .pinned_client(connect_timeout)
            .await
            .map_err(map_endpoint_error)?;
        let url = self
            .config
            .endpoint
            .resource_url(path)
            .map_err(map_endpoint_error)?;
        let first_byte_deadline = Instant::now() + self.config.timeouts.first_byte;
        let mut headers = sanitize_forward_headers(&HeaderMap::new());
        self.attach_auth(&mut headers)?;
        headers.insert(
            header::CONTENT_TYPE,
            HeaderValue::from_static("application/json"),
        );
        headers.insert(header::ACCEPT, HeaderValue::from_static("application/json"));
        headers.insert(
            "x-request-id",
            HeaderValue::from_str(&request.metadata.request_id.to_string()).map_err(|_| {
                transport_error(
                    TransportPhase::Connect,
                    AttemptFailureClass::Protocol,
                    false,
                    "request ID cannot be represented as an HTTP header",
                )
            })?,
        );
        let send_wait = remaining_until(first_byte_deadline, attempt_deadline)
            .ok_or_else(first_byte_timeout)?;
        let response = timeout(
            send_wait,
            client.post(url).headers(headers).body(body).send(),
        )
        .await
        .map_err(|_| first_byte_timeout())?
        .map_err(map_send_error)?;
        if !response.status().is_success() {
            return Err(self.map_error_response(response, attempt_deadline).await);
        }
        require_content_type(&response, "application/json")?;
        read_bounded_body(
            response,
            first_byte_deadline,
            attempt_deadline,
            self.config.timeouts.idle,
            self.config.max_response_bytes,
        )
        .await
    }

    async fn unary_response(
        &self,
        response: Response,
        first_byte_deadline: Instant,
        attempt_deadline: Instant,
        responses_endpoint: bool,
    ) -> Result<ProviderEventStream, TransportError> {
        require_content_type(&response, "application/json")?;
        let body = read_bounded_body(
            response,
            first_byte_deadline,
            attempt_deadline,
            self.config.timeouts.idle,
            self.config.max_response_bytes,
        )
        .await?;
        let events = if responses_endpoint {
            let response: ResponseObject = parse_wire("Responses", &body)?;
            decode_response_object(response)
                .map_err(|error| protocol_decode_error("Responses", error))?
        } else {
            let response: ChatCompletionResponse = parse_wire("chat", &body)?;
            decode_chat_completion_response(response)
                .map_err(|error| protocol_decode_error("chat", error))?
        };
        Ok(Box::pin(stream::iter(events.into_iter().map(Ok))))
    }

    async fn streaming_response(
        &self,
        response: Response,
        first_byte_deadline: Instant,
        attempt_deadline: Instant,
        responses_endpoint: bool,
    ) -> Result<ProviderEventStream, TransportError> {
        require_content_type(&response, "text/event-stream")?;
        let source: ReqwestByteStream = Box::pin(response.bytes_stream());
        let bytes = DeadlineByteStream::new(
            source,
            first_byte_deadline,
            self.config.timeouts.idle,
            attempt_deadline,
        );
        let decoder = if responses_endpoint {
            OpenAiEventDecoder::Responses(OpenAiResponsesStreamDecoder::with_max_event_bytes(
                self.config.max_event_bytes,
            ))
        } else {
            OpenAiEventDecoder::Chat(OpenAiChatStreamDecoder::with_max_event_bytes(
                self.config.max_event_bytes,
            ))
        };
        Ok(Box::pin(DecodedEventStream::new(bytes, decoder)))
    }

    async fn map_error_response(
        &self,
        response: Response,
        attempt_deadline: Instant,
    ) -> TransportError {
        let status = response.status();
        let first_byte_deadline = Instant::now() + self.config.timeouts.first_byte;
        let message = match read_bounded_body(
            response,
            first_byte_deadline,
            attempt_deadline,
            self.config.timeouts.idle,
            self.config.max_response_bytes.min(64 * 1024),
        )
        .await
        {
            Ok(body) => safe_upstream_error_message(status, &body, self.api_key.expose()),
            Err(_) => format!("OpenAI returned HTTP {status}"),
        };
        let class = if status == StatusCode::REQUEST_TIMEOUT {
            AttemptFailureClass::Timeout
        } else if status == StatusCode::TOO_MANY_REQUESTS {
            AttemptFailureClass::RateLimit
        } else if status.is_server_error() {
            AttemptFailureClass::UpstreamServer
        } else {
            AttemptFailureClass::UpstreamClient
        };
        transport_error(TransportPhase::FirstByte, class, false, message)
    }
}

const MAX_INLINE_REQUEST_MEDIA_BYTES: usize = 1024 * 1024;

async fn hydrate_chat_media(
    request: &mut ChatCompletionRequest,
    spool: Option<&Arc<dyn MediaSpool>>,
) -> Result<(), TransportError> {
    for message in &mut request.messages {
        let Some(ChatMessageContent::Parts(parts)) = &mut message.content else {
            continue;
        };
        for part in parts {
            let ChatContentPart::InputAudio { input_audio, .. } = part else {
                continue;
            };
            if media_handle_from_inline_marker(&input_audio.data).is_some() {
                input_audio.data = read_inline_request_media(&input_audio.data, spool).await?;
            }
        }
    }
    Ok(())
}

async fn hydrate_responses_media(
    input: &mut ResponseInput,
    spool: Option<&Arc<dyn MediaSpool>>,
) -> Result<(), TransportError> {
    let ResponseInput::Items(items) = input else {
        return Ok(());
    };
    for item in items {
        let Some(content) = item.get_mut("content").and_then(Value::as_array_mut) else {
            continue;
        };
        for part in content {
            let Some(object) = part.as_object_mut() else {
                continue;
            };
            match object.get("type").and_then(Value::as_str) {
                Some("input_audio") => {
                    let Some(audio) = object.get_mut("input_audio").and_then(Value::as_object_mut)
                    else {
                        return Err(protocol_body_error(
                            "OpenAI Responses input_audio is malformed",
                        ));
                    };
                    let Some(marker) = audio.get("data").and_then(Value::as_str) else {
                        return Err(protocol_body_error(
                            "OpenAI Responses input_audio omitted data",
                        ));
                    };
                    if media_handle_from_inline_marker(marker).is_some() {
                        let encoded = read_inline_request_media(marker, spool).await?;
                        audio.insert("data".to_owned(), Value::String(encoded));
                    }
                }
                Some("input_file") => {
                    let Some(marker) = object.get("file_data").and_then(Value::as_str) else {
                        return Err(protocol_body_error(
                            "OpenAI Responses input_file omitted file_data",
                        ));
                    };
                    if media_handle_from_inline_marker(marker).is_some() {
                        let encoded = read_inline_request_media(marker, spool).await?;
                        object.insert(
                            "file_data".to_owned(),
                            Value::String(format!("data:application/pdf;base64,{encoded}")),
                        );
                    }
                }
                _ => {}
            }
        }
    }
    Ok(())
}

async fn read_inline_request_media(
    marker: &str,
    spool: Option<&Arc<dyn MediaSpool>>,
) -> Result<String, TransportError> {
    let handle = media_handle_from_inline_marker(marker)
        .ok_or_else(|| protocol_body_error("invalid bounded inline-media handle"))?;
    let spool =
        spool.ok_or_else(|| protocol_body_error("bounded inline-media spool is unavailable"))?;
    let opened = spool.open(&handle).await.map_err(map_spool_error)?;
    if opened
        .artifact
        .content_length
        .is_none_or(|length| length > MAX_INLINE_REQUEST_MEDIA_BYTES as u64)
    {
        return Err(protocol_body_error(
            "bounded inline request media exceeded its limit",
        ));
    }
    let mut stream = opened.bytes;
    let mut bytes = Vec::new();
    while let Some(chunk) = stream.next().await {
        let chunk = chunk.map_err(map_spool_error)?;
        if bytes.len().saturating_add(chunk.len()) > MAX_INLINE_REQUEST_MEDIA_BYTES {
            return Err(protocol_body_error(
                "bounded inline request media exceeded its limit",
            ));
        }
        bytes.extend_from_slice(&chunk);
    }
    Ok(STANDARD.encode(bytes))
}

impl fmt::Debug for OpenAiConnector {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("OpenAiConnector")
            .field("config", &self.config)
            .field("api_key", &"[REDACTED]")
            .finish()
    }
}

impl ProviderTransport for OpenAiConnector {
    fn execute<'a>(
        &'a self,
        request: ProviderRequest,
    ) -> olp_domain::BoxFuture<'a, Result<ProviderOutput, TransportError>> {
        Box::pin(self.execute_request(request))
    }
}

enum ResultKind {
    Embeddings,
    TokenCount,
    Moderation,
}

fn validate_transport_mode(request: &ProviderRequest) -> Result<(), TransportError> {
    let mode = request.metadata.mode;
    let streaming = mode == TransportMode::Streaming;
    let valid = match &request.operation {
        Operation::Generation(operation) => {
            matches!(mode, TransportMode::Unary | TransportMode::Streaming)
                && operation.parameters.stream == streaming
        }
        Operation::Images(olp_domain::ImageOperation::Generation(operation)) => {
            matches!(mode, TransportMode::Unary | TransportMode::Streaming)
                && operation.stream == streaming
        }
        Operation::Images(olp_domain::ImageOperation::Edit(operation)) => {
            matches!(mode, TransportMode::Unary | TransportMode::Streaming)
                && operation.stream == streaming
        }
        Operation::Images(olp_domain::ImageOperation::Variation(_)) => mode == TransportMode::Unary,
        Operation::Speech(operation) => {
            matches!(mode, TransportMode::Unary | TransportMode::Streaming)
                && operation.stream == streaming
        }
        Operation::Transcription(operation) => {
            matches!(mode, TransportMode::Unary | TransportMode::Streaming)
                && operation.stream == streaming
        }
        Operation::Video(olp_domain::VideoOperation::Create(_)) => mode == TransportMode::Async,
        Operation::Video(
            olp_domain::VideoOperation::List(_)
            | olp_domain::VideoOperation::Get(_)
            | olp_domain::VideoOperation::Content(_)
            | olp_domain::VideoOperation::Delete(_),
        )
        | Operation::Embeddings(_)
        | Operation::TokenCount(_)
        | Operation::Moderation(_)
        | Operation::Models(_) => mode == TransportMode::Unary,
    };
    if valid {
        Ok(())
    } else {
        Err(transport_error(
            TransportPhase::Connect,
            AttemptFailureClass::Protocol,
            false,
            "canonical operation does not match the selected OpenAI transport mode",
        ))
    }
}

async fn bounded_part(
    spool: &dyn olp_domain::MediaSpool,
    handle: &olp_domain::MediaHandle,
    maximum: u64,
) -> Result<BoundedMediaPart, TransportError> {
    let opened = spool.open(handle).await.map_err(map_spool_error)?;
    let length = opened.artifact.content_length.ok_or_else(|| {
        protocol_body_error("bounded media spool omitted the admitted content length")
    })?;
    BoundedMediaPart::new(
        handle.clone(),
        opened.filename,
        opened.artifact.content_type,
        length,
        maximum,
    )
    .map_err(|error| protocol_body_error(error.to_string()))
}

fn multipart_part(opened: olp_domain::OpenedMedia) -> Result<multipart::Part, TransportError> {
    let length = opened.artifact.content_length.ok_or_else(|| {
        protocol_body_error("bounded media spool omitted the admitted content length")
    })?;
    let mut part =
        multipart::Part::stream_with_length(reqwest::Body::wrap_stream(opened.bytes), length)
            .file_name(opened.filename);
    if let Some(content_type) = opened.artifact.content_type {
        part = part.mime_str(&content_type).map_err(|_| {
            protocol_body_error("bounded media spool returned an invalid content type")
        })?;
    }
    Ok(part)
}

fn add_optional_text(
    form: multipart::Form,
    name: &'static str,
    value: Option<String>,
) -> multipart::Form {
    match value {
        Some(value) => form.text(name, value),
        None => form,
    }
}

fn add_extra_fields(
    mut form: multipart::Form,
    extra: std::collections::BTreeMap<String, serde_json::Value>,
) -> multipart::Form {
    for (name, value) in extra {
        if let serde_json::Value::Array(values) = value {
            let name = if name.ends_with("[]") {
                name
            } else {
                format!("{name}[]")
            };
            for value in values {
                let value = value
                    .as_str()
                    .map_or_else(|| value.to_string(), str::to_owned);
                form = form.text(name.clone(), value);
            }
        } else {
            let value = value
                .as_str()
                .map_or_else(|| value.to_string(), str::to_owned);
            form = form.text(name, value);
        }
    }
    form
}

fn add_image_edit_fields(
    mut form: multipart::Form,
    wire: &olp_protocols::openai::OpenAiImageEditRequest,
) -> multipart::Form {
    form = add_optional_text(form, "n", wire.n.map(|value| value.to_string()));
    form = add_optional_text(form, "size", wire.size.clone());
    form = form.text("stream", wire.stream.to_string());
    form = add_optional_text(form, "quality", wire.quality.clone());
    form = add_optional_text(form, "response_format", wire.response_format.clone());
    form = add_optional_text(form, "user", wire.user.clone());
    form = add_optional_text(form, "background", wire.background.clone());
    form = add_optional_text(form, "input_fidelity", wire.input_fidelity.clone());
    form = add_optional_text(
        form,
        "output_compression",
        wire.output_compression.map(|value| value.to_string()),
    );
    form = add_optional_text(form, "output_format", wire.output_format.clone());
    form = add_optional_text(
        form,
        "partial_images",
        wire.partial_images.map(|value| value.to_string()),
    );
    add_extra_fields(form, wire.extra.clone())
}

fn video_job_path(job_id: &str, suffix: Option<&str>) -> Result<String, TransportError> {
    if job_id.is_empty()
        || job_id.len() > 256
        || !job_id
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'-' | b'_'))
    {
        return Err(protocol_body_error("OpenAI video job ID is invalid"));
    }
    Ok(suffix.map_or_else(
        || format!("videos/{job_id}"),
        |suffix| format!("videos/{job_id}/{suffix}"),
    ))
}

fn percent_encode(value: &str) -> String {
    let mut encoded = String::with_capacity(value.len());
    const HEX: &[u8; 16] = b"0123456789ABCDEF";
    for byte in value.bytes() {
        if byte.is_ascii_alphanumeric() || matches!(byte, b'-' | b'_' | b'.' | b'~') {
            encoded.push(char::from(byte));
        } else {
            encoded.push('%');
            encoded.push(char::from(HEX[usize::from(byte >> 4)]));
            encoded.push(char::from(HEX[usize::from(byte & 0x0f)]));
        }
    }
    encoded
}

fn serialize_wire<T: serde::Serialize>(
    operation: &'static str,
    wire: &T,
) -> Result<Vec<u8>, TransportError> {
    serde_json::to_vec(wire).map_err(|error| protocol_encode_error(operation, error))
}

fn parse_wire<T: serde::de::DeserializeOwned>(
    operation: &'static str,
    body: &[u8],
) -> Result<T, TransportError> {
    serde_json::from_slice(body).map_err(|error| protocol_decode_error(operation, error))
}

fn protocol_encode_error(operation: &'static str, error: impl fmt::Display) -> TransportError {
    transport_error(
        TransportPhase::Connect,
        AttemptFailureClass::Protocol,
        false,
        format!("cannot encode OpenAI {operation} request: {error}"),
    )
}

fn protocol_decode_error(operation: &'static str, error: impl fmt::Display) -> TransportError {
    transport_error(
        TransportPhase::Body,
        AttemptFailureClass::Protocol,
        false,
        format!("OpenAI {operation} response is invalid: {error}"),
    )
}

fn protocol_body_error(message: impl Into<String>) -> TransportError {
    transport_error(
        TransportPhase::Body,
        AttemptFailureClass::Protocol,
        false,
        message,
    )
}

fn map_spool_error(error: MediaSpoolError) -> TransportError {
    transport_error(
        TransportPhase::Body,
        AttemptFailureClass::Protocol,
        false,
        format!("bounded media spool failed: {error}"),
    )
}

async fn spool_response_body(
    response: DeadlineResponse,
    spool: &Arc<dyn MediaSpool>,
    filename: String,
    content_type: Option<String>,
    maximum_length: u64,
    idle_timeout: Duration,
) -> Result<MediaArtifact, TransportError> {
    let source: ReqwestByteStream = Box::pin(response.response.bytes_stream());
    let failures = Arc::new(Mutex::new(None::<TransportError>));
    let failure_sink = Arc::clone(&failures);
    let bytes = DeadlineByteStream::new(
        source,
        response.first_body_deadline,
        idle_timeout,
        response.attempt_deadline,
    )
    .map(move |item| {
        item.map_err(|error| {
            if let Ok(mut failure) = failure_sink.lock() {
                *failure = Some(error);
            }
            MediaSpoolError::Unavailable
        })
    });
    match spool
        .put(MediaUpload {
            filename,
            content_type,
            maximum_length,
            bytes: Box::pin(bytes),
        })
        .await
    {
        Ok(artifact) => Ok(artifact),
        Err(error) => {
            let transport = failures.lock().ok().and_then(|failure| failure.clone());
            Err(transport.unwrap_or_else(|| map_spool_error(error)))
        }
    }
}

fn bearer_header(api_key: &OpenAiApiKey) -> Result<HeaderValue, TransportError> {
    let mut value = Zeroizing::new(Vec::with_capacity(7 + api_key.expose().len()));
    value.extend_from_slice(b"Bearer ");
    value.extend_from_slice(api_key.expose().as_bytes());
    HeaderValue::from_bytes(value.as_slice()).map_err(|_| {
        transport_error(
            TransportPhase::Connect,
            AttemptFailureClass::Protocol,
            false,
            "OpenAI API key cannot be represented as an HTTP header",
        )
    })
}

fn raw_api_key_header(api_key: &OpenAiApiKey) -> Result<HeaderValue, TransportError> {
    HeaderValue::from_bytes(api_key.expose().as_bytes()).map_err(|_| {
        transport_error(
            TransportPhase::Connect,
            AttemptFailureClass::Protocol,
            false,
            "OpenAI API key cannot be represented as an HTTP header",
        )
    })
}

fn require_stream_usage(request: &mut ChatCompletionRequest) -> Result<(), TransportError> {
    let options = request
        .extra
        .entry("stream_options".to_owned())
        .or_insert_with(|| serde_json::json!({}));
    let Some(options) = options.as_object_mut() else {
        return Err(transport_error(
            TransportPhase::Connect,
            AttemptFailureClass::Protocol,
            false,
            "OpenAI stream_options extension must be an object",
        ));
    };
    options.insert("include_usage".to_owned(), serde_json::Value::Bool(true));
    Ok(())
}

fn require_content_type(response: &Response, expected: &'static str) -> Result<(), TransportError> {
    let valid = response
        .headers()
        .get(header::CONTENT_TYPE)
        .and_then(|value| value.to_str().ok())
        .and_then(|value| value.split(';').next())
        .is_some_and(|value| value.trim().eq_ignore_ascii_case(expected));
    if valid {
        return Ok(());
    }
    Err(transport_error(
        TransportPhase::FirstByte,
        AttemptFailureClass::Protocol,
        false,
        format!("OpenAI response must use content type {expected}"),
    ))
}

fn require_transcription_text_content_type(
    response: &Response,
    format: TranscriptionResponseFormat,
) -> Result<(), TransportError> {
    let actual = response
        .headers()
        .get(header::CONTENT_TYPE)
        .and_then(|value| value.to_str().ok())
        .and_then(|value| value.split(';').next())
        .map(str::trim);
    let valid = match format {
        TranscriptionResponseFormat::Text => actual == Some("text/plain"),
        TranscriptionResponseFormat::Srt => {
            matches!(actual, Some("application/x-subrip" | "text/plain"))
        }
        TranscriptionResponseFormat::Vtt => matches!(actual, Some("text/vtt" | "text/plain")),
        _ => false,
    };
    if valid {
        Ok(())
    } else {
        Err(transport_error(
            TransportPhase::FirstByte,
            AttemptFailureClass::Protocol,
            false,
            "OpenAI transcription response used an invalid text content type",
        ))
    }
}

async fn read_bounded_body(
    response: Response,
    first_byte_deadline: Instant,
    attempt_deadline: Instant,
    idle_timeout: Duration,
    maximum: usize,
) -> Result<Vec<u8>, TransportError> {
    let mut source = response.bytes_stream();
    let mut output = Vec::new();
    let mut first = true;
    loop {
        let wait = if first {
            remaining_until(first_byte_deadline, attempt_deadline).ok_or_else(first_byte_timeout)?
        } else {
            bounded_duration(
                idle_timeout,
                remaining(attempt_deadline, TransportPhase::Body)?,
            )
        };
        let next = timeout(wait, source.next()).await.map_err(|_| {
            if first {
                first_byte_timeout()
            } else {
                body_idle_timeout()
            }
        })?;
        let Some(chunk) = next else {
            break;
        };
        let chunk = chunk.map_err(|error| {
            if first {
                map_first_body_error(error)
            } else {
                map_body_error(error, false)
            }
        })?;
        first = false;
        if output.len().saturating_add(chunk.len()) > maximum {
            return Err(transport_error(
                TransportPhase::Body,
                AttemptFailureClass::Protocol,
                false,
                format!("OpenAI response exceeded the {maximum} byte limit"),
            ));
        }
        output.extend_from_slice(&chunk);
    }
    if first {
        return Err(transport_error(
            TransportPhase::FirstByte,
            AttemptFailureClass::Protocol,
            false,
            "OpenAI response body was empty",
        ));
    }
    Ok(output)
}

async fn read_deadline_body(
    response: DeadlineResponse,
    idle_timeout: Duration,
    maximum: usize,
) -> Result<Vec<u8>, TransportError> {
    read_bounded_body(
        response.response,
        response.first_body_deadline,
        response.attempt_deadline,
        idle_timeout,
        maximum,
    )
    .await
}

struct DeadlineByteStream {
    source: ReqwestByteStream,
    first: bool,
    idle_timeout: Duration,
    idle_sleep: Pin<Box<Sleep>>,
    attempt_deadline: Instant,
    terminal: bool,
}

impl DeadlineByteStream {
    fn new(
        source: ReqwestByteStream,
        first_body_deadline: Instant,
        idle_timeout: Duration,
        attempt_deadline: Instant,
    ) -> Self {
        let wake_at = bounded_instant(first_body_deadline, attempt_deadline);
        Self {
            source,
            first: true,
            idle_timeout,
            idle_sleep: Box::pin(tokio::time::sleep_until(wake_at)),
            attempt_deadline,
            terminal: false,
        }
    }
}

impl Stream for DeadlineByteStream {
    type Item = Result<Bytes, TransportError>;

    fn poll_next(mut self: Pin<&mut Self>, context: &mut Context<'_>) -> Poll<Option<Self::Item>> {
        if self.terminal {
            return Poll::Ready(None);
        }
        if Instant::now() >= self.attempt_deadline {
            self.terminal = true;
            let error = if self.first {
                first_byte_timeout()
            } else {
                attempt_body_timeout()
            };
            return Poll::Ready(Some(Err(error)));
        }

        match self.source.as_mut().poll_next(context) {
            Poll::Ready(Some(Ok(chunk))) => {
                self.first = false;
                let wake_at =
                    bounded_instant(Instant::now() + self.idle_timeout, self.attempt_deadline);
                self.idle_sleep.as_mut().reset(wake_at);
                return Poll::Ready(Some(Ok(chunk)));
            }
            Poll::Ready(Some(Err(error))) => {
                self.terminal = true;
                let error = if self.first {
                    map_first_body_error(error)
                } else {
                    map_body_error(error, false)
                };
                return Poll::Ready(Some(Err(error)));
            }
            Poll::Ready(None) => {
                self.terminal = true;
                if self.first {
                    return Poll::Ready(Some(Err(transport_error(
                        TransportPhase::FirstByte,
                        AttemptFailureClass::Protocol,
                        false,
                        "OpenAI response body was empty",
                    ))));
                }
                return Poll::Ready(None);
            }
            Poll::Pending => {}
        }

        if self.idle_sleep.as_mut().poll(context).is_ready() {
            self.terminal = true;
            let error = if Instant::now() >= self.attempt_deadline {
                if self.first {
                    first_byte_timeout()
                } else {
                    attempt_body_timeout()
                }
            } else if self.first {
                first_byte_timeout()
            } else {
                body_idle_timeout()
            };
            return Poll::Ready(Some(Err(error)));
        }
        Poll::Pending
    }
}

struct DecodedEventStream {
    bytes: DeadlineByteStream,
    decoder: OpenAiEventDecoder,
    queued: VecDeque<CanonicalEvent>,
    committed: bool,
    terminal: bool,
}

impl DecodedEventStream {
    fn new(bytes: DeadlineByteStream, decoder: OpenAiEventDecoder) -> Self {
        Self {
            bytes,
            decoder,
            queued: VecDeque::new(),
            committed: false,
            terminal: false,
        }
    }

    fn protocol_error(&self, message: impl Into<String>) -> TransportError {
        transport_error(
            TransportPhase::Body,
            AttemptFailureClass::Protocol,
            self.committed,
            message,
        )
    }
}

enum OpenAiEventDecoder {
    Chat(OpenAiChatStreamDecoder),
    Responses(OpenAiResponsesStreamDecoder),
}

impl OpenAiEventDecoder {
    fn push(&mut self, bytes: &[u8]) -> Result<Vec<CanonicalEvent>, String> {
        match self {
            Self::Chat(decoder) => decoder.push(bytes).map_err(|error| error.to_string()),
            Self::Responses(decoder) => decoder.push(bytes).map_err(|error| error.to_string()),
        }
    }

    fn finish(&mut self) -> Result<Vec<CanonicalEvent>, String> {
        match self {
            Self::Chat(decoder) => decoder.finish().map_err(|error| error.to_string()),
            Self::Responses(decoder) => decoder.finish().map_err(|error| error.to_string()),
        }
    }
}

impl Stream for DecodedEventStream {
    type Item = Result<CanonicalEvent, TransportError>;

    fn poll_next(mut self: Pin<&mut Self>, context: &mut Context<'_>) -> Poll<Option<Self::Item>> {
        loop {
            if let Some(event) = self.queued.pop_front() {
                self.committed = true;
                return Poll::Ready(Some(Ok(event)));
            }
            if self.terminal {
                return Poll::Ready(None);
            }

            match Pin::new(&mut self.bytes).poll_next(context) {
                Poll::Ready(Some(Ok(chunk))) => match self.decoder.push(&chunk) {
                    Ok(events) => self.queued.extend(events),
                    Err(error) => {
                        self.terminal = true;
                        return Poll::Ready(Some(Err(
                            self.protocol_error(format!("invalid OpenAI event stream: {error}"))
                        )));
                    }
                },
                Poll::Ready(Some(Err(mut error))) => {
                    self.terminal = true;
                    error.response_committed = self.committed;
                    return Poll::Ready(Some(Err(error)));
                }
                Poll::Ready(None) => {
                    self.terminal = true;
                    match self.decoder.finish() {
                        Ok(events) => self.queued.extend(events),
                        Err(error) => {
                            return Poll::Ready(Some(Err(self.protocol_error(format!(
                                "truncated OpenAI event stream: {error}"
                            )))));
                        }
                    }
                }
                Poll::Pending => return Poll::Pending,
            }
        }
    }
}

struct RawSseEventStream {
    bytes: DeadlineByteStream,
    decoder: SseDecoder,
    queued: VecDeque<CanonicalEvent>,
    sequence: u64,
    committed: bool,
    terminal: bool,
}

impl RawSseEventStream {
    fn new(bytes: DeadlineByteStream, maximum_event_bytes: usize) -> Self {
        Self {
            bytes,
            decoder: SseDecoder::new(maximum_event_bytes),
            queued: VecDeque::new(),
            sequence: 0,
            committed: false,
            terminal: false,
        }
    }

    fn queue_frames(&mut self, frames: Vec<SseFrame>) -> Result<(), TransportError> {
        for frame in frames {
            if self.terminal {
                return Err(self.protocol_error("OpenAI sent media events after completion"));
            }
            if frame.data.trim() == "[DONE]" {
                self.push(CanonicalEventKind::Done);
                self.terminal = true;
                continue;
            }
            let value: serde_json::Value = serde_json::from_str(&frame.data).map_err(|error| {
                self.protocol_error(format!("OpenAI media event is invalid JSON: {error}"))
            })?;
            let kind = value
                .get("type")
                .and_then(serde_json::Value::as_str)
                .or(frame.event.as_deref())
                .unwrap_or("message")
                .to_owned();
            let extensions = olp_domain::SourceExtensions::new(
                Surface::OpenAi,
                std::collections::BTreeMap::from([
                    ("/__olp/raw_sse/data".into(), value),
                    (
                        "/__olp/raw_sse/event".into(),
                        serde_json::Value::String(kind.clone()),
                    ),
                ]),
            );
            self.push(CanonicalEventKind::SourceExtension { extensions });
            if is_raw_media_terminal(&kind) {
                self.push(CanonicalEventKind::Done);
                self.terminal = true;
            }
        }
        Ok(())
    }

    fn push(&mut self, kind: olp_domain::CanonicalEventKind) {
        self.queued
            .push_back(CanonicalEvent::new(self.sequence, kind));
        self.sequence = self.sequence.saturating_add(1);
    }

    fn protocol_error(&self, message: impl Into<String>) -> TransportError {
        transport_error(
            TransportPhase::Body,
            AttemptFailureClass::Protocol,
            self.committed,
            message,
        )
    }
}

impl Stream for RawSseEventStream {
    type Item = Result<CanonicalEvent, TransportError>;

    fn poll_next(mut self: Pin<&mut Self>, context: &mut Context<'_>) -> Poll<Option<Self::Item>> {
        loop {
            if let Some(event) = self.queued.pop_front() {
                self.committed = true;
                return Poll::Ready(Some(Ok(event)));
            }
            if self.terminal {
                return Poll::Ready(None);
            }
            match Pin::new(&mut self.bytes).poll_next(context) {
                Poll::Ready(Some(Ok(chunk))) => {
                    let frames = match self.decoder.push(&chunk) {
                        Ok(frames) => frames,
                        Err(error) => {
                            self.terminal = true;
                            return Poll::Ready(Some(Err(self.protocol_error(format!(
                                "invalid OpenAI media event stream: {error}"
                            )))));
                        }
                    };
                    if let Err(error) = self.queue_frames(frames) {
                        self.terminal = true;
                        return Poll::Ready(Some(Err(error)));
                    }
                }
                Poll::Ready(Some(Err(mut error))) => {
                    self.terminal = true;
                    error.response_committed = self.committed;
                    return Poll::Ready(Some(Err(error)));
                }
                Poll::Ready(None) => {
                    let frames = match self.decoder.finish() {
                        Ok(frames) => frames,
                        Err(error) => {
                            self.terminal = true;
                            return Poll::Ready(Some(Err(self.protocol_error(format!(
                                "truncated OpenAI media event stream: {error}"
                            )))));
                        }
                    };
                    if let Err(error) = self.queue_frames(frames) {
                        self.terminal = true;
                        return Poll::Ready(Some(Err(error)));
                    }
                    if !self.terminal {
                        self.terminal = true;
                        return Poll::Ready(Some(Err(self.protocol_error(
                            "OpenAI media event stream ended without completion",
                        ))));
                    }
                }
                Poll::Pending => return Poll::Pending,
            }
        }
    }
}

fn is_raw_media_terminal(kind: &str) -> bool {
    matches!(
        kind,
        "image_generation.completed"
            | "image_edit.completed"
            | "speech.audio.done"
            | "transcript.text.done"
            | "transcription.done"
            | "transcription.completed"
    ) || kind.ends_with(".failed")
}

fn safe_upstream_error_message(status: StatusCode, body: &[u8], api_key: &str) -> String {
    let message = serde_json::from_slice::<serde_json::Value>(body)
        .ok()
        .and_then(|value| value.get("error").cloned())
        .and_then(|error| {
            error
                .get("message")
                .and_then(|message| message.as_str())
                .map(str::to_owned)
        })
        .map(|message| message.replace(api_key, "[REDACTED]"))
        .map(|message| message.chars().take(512).collect::<String>());
    match message {
        Some(message) if !message.is_empty() => format!("OpenAI returned HTTP {status}: {message}"),
        _ => format!("OpenAI returned HTTP {status}"),
    }
}

fn bounded_duration(configured: Duration, remaining: Duration) -> Duration {
    configured.min(remaining)
}

fn bounded_instant(configured: Instant, deadline: Instant) -> Instant {
    configured.min(deadline)
}

fn remaining(deadline: Instant, phase: TransportPhase) -> Result<Duration, TransportError> {
    deadline
        .checked_duration_since(Instant::now())
        .ok_or_else(|| {
            transport_error(
                phase,
                AttemptFailureClass::Timeout,
                false,
                "OpenAI attempt deadline elapsed",
            )
        })
}

fn remaining_until(phase_deadline: Instant, attempt_deadline: Instant) -> Option<Duration> {
    bounded_instant(phase_deadline, attempt_deadline).checked_duration_since(Instant::now())
}

fn map_endpoint_error(error: EndpointError) -> TransportError {
    let class = if matches!(error, EndpointError::DnsTimeout) {
        AttemptFailureClass::Timeout
    } else {
        AttemptFailureClass::Connect
    };
    transport_error(TransportPhase::Connect, class, false, error.to_string())
}

fn map_send_error(error: reqwest::Error) -> TransportError {
    let (phase, class, message) = if error.is_connect() {
        (
            TransportPhase::Connect,
            if error.is_timeout() {
                AttemptFailureClass::Timeout
            } else {
                AttemptFailureClass::Connect
            },
            "OpenAI connection failed",
        )
    } else if error.is_timeout() {
        (
            TransportPhase::FirstByte,
            AttemptFailureClass::Timeout,
            "OpenAI first-byte deadline elapsed",
        )
    } else {
        (
            TransportPhase::FirstByte,
            AttemptFailureClass::Connect,
            "OpenAI request failed before response headers",
        )
    };
    transport_error(phase, class, false, message)
}

fn map_ambiguous_send_error(error: reqwest::Error) -> TransportError {
    if error.is_connect() {
        return map_send_error(error);
    }
    transport_error(
        TransportPhase::Body,
        AttemptFailureClass::Ambiguous,
        true,
        "OpenAI multipart request may have been committed before transport failure",
    )
}

fn ambiguous_multipart_timeout() -> TransportError {
    transport_error(
        TransportPhase::Body,
        AttemptFailureClass::Ambiguous,
        true,
        "OpenAI multipart request may have been committed before its first-byte deadline",
    )
}

fn map_first_body_error(error: reqwest::Error) -> TransportError {
    transport_error(
        TransportPhase::FirstByte,
        if error.is_timeout() {
            AttemptFailureClass::Timeout
        } else {
            AttemptFailureClass::Connect
        },
        false,
        "OpenAI response body failed before its first byte",
    )
}

fn map_body_error(error: reqwest::Error, committed: bool) -> TransportError {
    transport_error(
        TransportPhase::Body,
        if error.is_timeout() {
            AttemptFailureClass::Timeout
        } else {
            AttemptFailureClass::Connect
        },
        committed,
        "OpenAI response body failed",
    )
}

fn first_byte_timeout() -> TransportError {
    transport_error(
        TransportPhase::FirstByte,
        AttemptFailureClass::Timeout,
        false,
        "OpenAI first-byte deadline elapsed",
    )
}

fn body_idle_timeout() -> TransportError {
    transport_error(
        TransportPhase::Body,
        AttemptFailureClass::Timeout,
        false,
        "OpenAI response idle deadline elapsed",
    )
}

fn attempt_body_timeout() -> TransportError {
    transport_error(
        TransportPhase::Body,
        AttemptFailureClass::Timeout,
        false,
        "OpenAI attempt deadline elapsed while reading the response",
    )
}

fn transport_error(
    phase: TransportPhase,
    class: AttemptFailureClass,
    response_committed: bool,
    message: impl Into<String>,
) -> TransportError {
    TransportError {
        phase,
        class,
        response_committed,
        message: message.into(),
    }
}

#[cfg(test)]
mod tests {
    use std::{
        collections::BTreeMap,
        sync::{
            Arc, Mutex,
            atomic::{AtomicUsize, Ordering},
        },
    };

    use olp_domain::{
        AttemptPlan, CanonicalEventKind, ContentPart, DurationMs, EmbeddingInput,
        EmbeddingsRequest, GenerationParameters, GenerationRequest, ImageEditRequest,
        ImageGenerationRequest, ImageOperation, ImageVariationRequest, MediaArtifact, MediaHandle,
        MediaSource, MediaSpool, Message, MessageRole, ModerationRequest, OpenedMedia,
        OperationKind, ProviderId, ProviderKind, RequestId, RequestMetadata, RouteId, RouteSlug,
        RuntimeGenerationId, SourceExtensions, SpeechRequest, TargetId, TranscriptionRequest,
        VideoCreateRequest, VideoJobRequest, VideoOperation, VideoStatus,
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

    async fn execute_error(
        connector: &OpenAiConnector,
        request: ProviderRequest,
    ) -> TransportError {
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
        let failure =
            execute_error(&connector, video_job_request(OperationKind::VideoDelete)).await;
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
        let terminal = b"event: image_generation.completed\ndata: {\"type\":\"image_generation.completed\"}\n\n";
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
}
