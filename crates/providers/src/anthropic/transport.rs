use std::{
    collections::{BTreeMap, VecDeque},
    fmt,
    future::ready,
    pin::Pin,
    task::{Context, Poll},
    time::Duration,
};

use base64::{Engine as _, engine::general_purpose::STANDARD};
use bytes::Bytes;
use futures::{Stream, StreamExt, stream};
use http::{HeaderMap, HeaderValue, StatusCode, header};
#[cfg(test)]
use olp_domain::CanonicalEventKind;
use olp_domain::{
    AttemptFailureClass, CanonicalEvent, CanonicalResult, ContentPart, DiscoveredProviderModel,
    MediaSource, MediaSpool, Operation, ProviderEventStream, ProviderKind, ProviderOutput,
    ProviderRequest, ProviderTransport, SourceExtensions, Surface, TokenCountResult,
    TransportError, TransportMode, TransportPhase, media_handle_from_inline_marker,
};
use olp_protocols::anthropic::{
    ANTHROPIC_COUNT_REQUEST_EXTENSION, AnthropicMessagesStreamDecoder, ContentBlock,
    CountTokensRequest, CountTokensResponse, ImageBlock, MediaSource as AnthropicMediaSource,
    Message, MessageContent, MessagesResponse, Role, TextBlock, decode_messages_response,
    encode_messages_request,
};
use reqwest::{Response, Url};
use tokio::time::{Instant, Sleep, timeout};
use zeroize::Zeroizing;

use crate::anthropic::{
    AnthropicApiKey, ConnectorConfig, endpoint::EndpointError, headers::sanitize_forward_headers,
};

type ReqwestByteStream =
    Pin<Box<dyn Stream<Item = Result<Bytes, reqwest::Error>> + Send + 'static>>;

/// Validates the concrete canonical request against the same encoders used by
/// the production transport. The gateway invokes this before attempt ordering
/// so a cross-origin capability remains eligible only when no source semantics
/// would be lost.
pub fn validate_operation(
    operation: &Operation,
    provider_model: &str,
) -> Result<(), TransportError> {
    match operation {
        Operation::Generation(generation) => encode_messages_request(generation, provider_model)
            .map(|_| ())
            .map_err(|error| protocol_error(error.to_string())),
        Operation::TokenCount(count) => encode_count_tokens(count, provider_model).map(|_| ()),
        operation => Err(protocol_error(format!(
            "Anthropic connector does not support {:?}",
            operation.kind()
        ))),
    }
}

#[derive(Clone, Copy)]
enum ResponseKind {
    Generation,
    TokenCount,
}

pub struct AnthropicConnector {
    config: ConnectorConfig,
    api_key: AnthropicApiKey,
}

impl AnthropicConnector {
    #[must_use]
    pub fn new(config: ConnectorConfig, api_key: AnthropicApiKey) -> Self {
        Self { config, api_key }
    }

    /// Lists the upstream model catalog through the same pinned-DNS and
    /// redirect-free transport boundary as inference.
    pub async fn discover_models(&self) -> Result<Vec<DiscoveredProviderModel>, TransportError> {
        let mut discovered = Vec::new();
        let mut after_id: Option<String> = None;
        for _ in 0..100 {
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
            let mut url = self
                .config
                .endpoint
                .models_url()
                .map_err(map_endpoint_error)?;
            {
                let mut query = url.query_pairs_mut();
                query.append_pair("limit", "100");
                if let Some(after_id) = &after_id {
                    query.append_pair("after_id", after_id);
                }
            }
            let mut headers = sanitize_forward_headers(&HeaderMap::new());
            headers.insert("x-api-key", secret_header(&self.api_key)?);
            headers.insert(
                "anthropic-version",
                HeaderValue::from_str(&self.config.api_version).map_err(|_| {
                    protocol_error("Anthropic API version cannot be represented as a header")
                })?,
            );
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
                protocol_body_error(format!(
                    "Anthropic model discovery is not valid JSON: {error}"
                ))
            })?;
            let data = value
                .get("data")
                .and_then(serde_json::Value::as_array)
                .ok_or_else(|| protocol_body_error("Anthropic model discovery omitted data"))?;
            for model in data {
                let id = model
                    .get("id")
                    .and_then(serde_json::Value::as_str)
                    .filter(|id| !id.is_empty())
                    .ok_or_else(|| {
                        protocol_body_error("Anthropic model discovery returned an invalid ID")
                    })?;
                let display_name = model
                    .get("display_name")
                    .and_then(serde_json::Value::as_str)
                    .filter(|name| !name.is_empty())
                    .unwrap_or(id);
                discovered.push(DiscoveredProviderModel {
                    id: id.to_owned(),
                    display_name: display_name.to_owned(),
                });
            }
            if !value
                .get("has_more")
                .and_then(serde_json::Value::as_bool)
                .unwrap_or(false)
            {
                return Ok(discovered);
            }
            after_id = value
                .get("last_id")
                .and_then(serde_json::Value::as_str)
                .map(str::to_owned)
                .or_else(|| discovered.last().map(|model| model.id.clone()));
            if after_id.is_none() {
                return Err(protocol_body_error(
                    "Anthropic discovery indicated another page without a cursor",
                ));
            }
        }
        Err(protocol_body_error(
            "Anthropic model discovery exceeded 100 pages",
        ))
    }

    async fn execute_request(
        &self,
        request: ProviderRequest,
    ) -> Result<ProviderOutput, TransportError> {
        validate_request_envelope(&request)?;
        let (url, body, response_kind, streaming) = self.encode_request(&request).await?;

        let attempt_deadline = Instant::now() + request.attempt.timeout.as_duration();
        let connect_timeout = bounded_duration(
            self.config.timeouts.connect,
            remaining(attempt_deadline, TransportPhase::Connect)?,
        );
        // Resolve, validate, and pin before materializing the credential header.
        let client = self
            .config
            .endpoint
            .pinned_client(connect_timeout)
            .await
            .map_err(map_endpoint_error)?;

        let first_byte_deadline = Instant::now() + self.config.timeouts.first_byte;
        let mut headers = sanitize_forward_headers(&HeaderMap::new());
        headers.insert("x-api-key", secret_header(&self.api_key)?);
        headers.insert(
            "anthropic-version",
            HeaderValue::from_str(&self.config.api_version).map_err(|_| {
                protocol_error("Anthropic API version cannot be represented as a header")
            })?,
        );
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
            HeaderValue::from_str(&request.metadata.request_id.to_string())
                .map_err(|_| protocol_error("request ID cannot be represented as a header"))?,
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
        if streaming {
            self.streaming_response(
                response,
                first_byte_deadline,
                attempt_deadline,
                request.metadata.surface == Surface::Anthropic,
            )
            .await
            .map(ProviderOutput::Events)
        } else {
            self.unary_response(
                response,
                response_kind,
                first_byte_deadline,
                attempt_deadline,
            )
            .await
        }
    }

    async fn encode_request(
        &self,
        request: &ProviderRequest,
    ) -> Result<(Url, Vec<u8>, ResponseKind, bool), TransportError> {
        match &request.operation {
            Operation::Generation(generation) => {
                let streaming = request.metadata.mode == TransportMode::Streaming;
                if generation.parameters.stream != streaming {
                    return Err(protocol_error(
                        "canonical stream flag does not match the selected transport mode",
                    ));
                }
                let mut wire = encode_messages_request(generation, &request.attempt.provider_model)
                    .map_err(|error| {
                        protocol_error(format!("cannot encode Anthropic messages request: {error}"))
                    })?;
                hydrate_anthropic_messages(&mut wire.messages, request.media.as_ref()).await?;
                let body = serde_json::to_vec(&wire).map_err(|error| {
                    protocol_error(format!("cannot serialize Anthropic request: {error}"))
                })?;
                Ok((
                    self.config
                        .endpoint
                        .messages_url()
                        .map_err(map_endpoint_error)?,
                    body,
                    ResponseKind::Generation,
                    streaming,
                ))
            }
            Operation::TokenCount(count) => {
                if request.metadata.mode != TransportMode::Unary {
                    return Err(protocol_error(
                        "Anthropic token counting supports unary mode only",
                    ));
                }
                let mut wire = encode_count_tokens(count, &request.attempt.provider_model)?;
                hydrate_anthropic_messages(&mut wire.messages, request.media.as_ref()).await?;
                let body = serde_json::to_vec(&wire).map_err(|error| {
                    protocol_error(format!("cannot serialize Anthropic count request: {error}"))
                })?;
                Ok((
                    self.config
                        .endpoint
                        .count_tokens_url()
                        .map_err(map_endpoint_error)?,
                    body,
                    ResponseKind::TokenCount,
                    false,
                ))
            }
            Operation::Models(_) => Err(protocol_error(
                "canonical model response values are not yet defined; model list/get is unavailable",
            )),
            operation => Err(protocol_error(format!(
                "Anthropic connector does not support {:?}",
                operation.kind()
            ))),
        }
    }

    async fn unary_response(
        &self,
        response: Response,
        kind: ResponseKind,
        first_byte_deadline: Instant,
        attempt_deadline: Instant,
    ) -> Result<ProviderOutput, TransportError> {
        require_content_type(&response, "application/json")?;
        let body = read_bounded_body(
            response,
            first_byte_deadline,
            attempt_deadline,
            self.config.timeouts.idle,
            self.config.max_response_bytes,
        )
        .await?;
        match kind {
            ResponseKind::Generation => {
                let response: MessagesResponse =
                    serde_json::from_slice(&body).map_err(|error| {
                        protocol_body_error(format!(
                            "Anthropic response is not valid JSON: {error}"
                        ))
                    })?;
                let events = decode_messages_response(response).map_err(|error| {
                    protocol_body_error(format!("Anthropic response is invalid: {error}"))
                })?;
                Ok(ProviderOutput::Events(Box::pin(stream::iter(
                    events.into_iter().map(Ok),
                ))))
            }
            ResponseKind::TokenCount => {
                let response: CountTokensResponse =
                    serde_json::from_slice(&body).map_err(|error| {
                        protocol_body_error(format!(
                            "Anthropic count response is not valid JSON: {error}"
                        ))
                    })?;
                Ok(ProviderOutput::Result(Box::new(
                    CanonicalResult::TokenCount(TokenCountResult {
                        input_tokens: response.input_tokens,
                        extensions: source_extensions(Surface::Anthropic, response.extra),
                    }),
                )))
            }
        }
    }

    async fn streaming_response(
        &self,
        response: Response,
        first_byte_deadline: Instant,
        attempt_deadline: Instant,
        preserve_raw_frames: bool,
    ) -> Result<ProviderEventStream, TransportError> {
        require_content_type(&response, "text/event-stream")?;
        let mut source: ReqwestByteStream = Box::pin(response.bytes_stream());
        let first_wait = remaining_until(first_byte_deadline, attempt_deadline)
            .ok_or_else(first_byte_timeout)?;
        let first = timeout(first_wait, source.next())
            .await
            .map_err(|_| first_byte_timeout())?
            .ok_or_else(|| {
                transport_error(
                    TransportPhase::FirstByte,
                    AttemptFailureClass::Protocol,
                    false,
                    "Anthropic stream ended before its first body byte",
                )
            })?
            .map_err(map_first_body_error)?;
        let source = Box::pin(stream::once(ready(Ok(first))).chain(source));
        let bytes = DeadlineByteStream::new(source, self.config.timeouts.idle, attempt_deadline);
        let decoder = AnthropicMessagesStreamDecoder::with_max_event_bytes_and_raw_passthrough(
            self.config.max_event_bytes,
            preserve_raw_frames,
        );
        Ok(Box::pin(DecodedEventStream::new(bytes, decoder)))
    }

    async fn map_error_response(
        &self,
        response: Response,
        attempt_deadline: Instant,
    ) -> TransportError {
        let status = response.status();
        let deadline = Instant::now() + self.config.timeouts.first_byte;
        let message = match read_bounded_body(
            response,
            deadline,
            attempt_deadline,
            self.config.timeouts.idle,
            self.config.max_response_bytes.min(64 * 1024),
        )
        .await
        {
            Ok(body) => safe_upstream_error_message(status, &body, self.api_key.expose()),
            Err(_) => format!("Anthropic returned HTTP {status}"),
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

const MAX_INLINE_MEDIA_BYTES: usize = 1024 * 1024;

async fn hydrate_anthropic_messages(
    messages: &mut [Message],
    spool: Option<&std::sync::Arc<dyn MediaSpool>>,
) -> Result<(), TransportError> {
    for message in messages {
        let MessageContent::Blocks(blocks) = &mut message.content else {
            continue;
        };
        for block in blocks {
            hydrate_anthropic_block(block, spool).await?;
        }
    }
    Ok(())
}

async fn hydrate_anthropic_block(
    block: &mut ContentBlock,
    spool: Option<&std::sync::Arc<dyn MediaSpool>>,
) -> Result<(), TransportError> {
    match block {
        ContentBlock::Image(image) if image.source.kind == "base64" => {
            let Some(marker) = image.source.data.as_deref() else {
                return Err(protocol_error("Anthropic base64 image omitted data"));
            };
            if media_handle_from_inline_marker(marker).is_some() {
                image.source.data = Some(read_inline_media(marker, spool).await?);
            }
        }
        ContentBlock::ToolResult(result) => {
            if let Some(olp_protocols::anthropic::ToolResultContent::Blocks(blocks)) =
                &mut result.content
            {
                for block in blocks {
                    Box::pin(hydrate_anthropic_block(block, spool)).await?;
                }
            }
        }
        _ => {}
    }
    Ok(())
}

async fn read_inline_media(
    marker: &str,
    spool: Option<&std::sync::Arc<dyn MediaSpool>>,
) -> Result<String, TransportError> {
    let handle = media_handle_from_inline_marker(marker)
        .ok_or_else(|| protocol_error("invalid bounded inline-media handle"))?;
    let spool = spool.ok_or_else(|| protocol_error("bounded inline-media spool is unavailable"))?;
    let opened = spool.open(&handle).await.map_err(|error| {
        protocol_error(format!(
            "bounded inline-media handle cannot be opened: {error}"
        ))
    })?;
    if opened
        .artifact
        .content_length
        .is_none_or(|length| length > MAX_INLINE_MEDIA_BYTES as u64)
    {
        return Err(protocol_error("bounded inline media exceeded its limit"));
    }
    let mut stream = opened.bytes;
    let mut bytes = Vec::new();
    while let Some(chunk) = stream.next().await {
        let chunk = chunk.map_err(|error| {
            protocol_error(format!("bounded inline-media read failed: {error}"))
        })?;
        if bytes.len().saturating_add(chunk.len()) > MAX_INLINE_MEDIA_BYTES {
            return Err(protocol_error("bounded inline media exceeded its limit"));
        }
        bytes.extend_from_slice(&chunk);
    }
    Ok(STANDARD.encode(bytes))
}

impl fmt::Debug for AnthropicConnector {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("AnthropicConnector")
            .field("config", &self.config)
            .field("api_key", &"[REDACTED]")
            .finish()
    }
}

impl ProviderTransport for AnthropicConnector {
    fn execute<'a>(
        &'a self,
        request: ProviderRequest,
    ) -> olp_domain::BoxFuture<'a, Result<ProviderOutput, TransportError>> {
        Box::pin(async move { self.execute_request(request).await })
    }
}

fn validate_request_envelope(request: &ProviderRequest) -> Result<(), TransportError> {
    if request.metadata.operation != request.operation.kind() {
        return Err(protocol_error(
            "request metadata operation does not match the canonical operation",
        ));
    }
    if request.attempt.provider_kind != ProviderKind::Anthropic {
        return Err(protocol_error(
            "Anthropic connector received an attempt for another provider kind",
        ));
    }
    if request.metadata.mode == TransportMode::Async {
        return Err(protocol_error(
            "Anthropic connector does not support asynchronous mode",
        ));
    }
    Ok(())
}

fn encode_count_tokens(
    request: &olp_domain::TokenCountRequest,
    provider_model: &str,
) -> Result<CountTokensRequest, TransportError> {
    request
        .extensions
        .ensure_representable_on(Surface::Anthropic)
        .map_err(|error| protocol_error(error.to_string()))?;
    let mut extensions = request.extensions.values.clone();
    if let Some(value) = extensions.remove(ANTHROPIC_COUNT_REQUEST_EXTENSION) {
        if !extensions.is_empty() {
            return Err(protocol_error(
                "Anthropic token-count extensions cannot be reconstructed without losing semantics",
            ));
        }
        let mut wire: CountTokensRequest = serde_json::from_value(value).map_err(|error| {
            protocol_error(format!(
                "preserved Anthropic countTokens request is invalid: {error}"
            ))
        })?;
        wire.model = provider_model.to_owned();
        return Ok(wire);
    }
    if !extensions.is_empty() {
        return Err(protocol_error(
            "Anthropic token-count extensions cannot be reconstructed without losing semantics",
        ));
    }
    if request.input.is_empty() {
        return Err(protocol_error("token-count input cannot be empty"));
    }
    let mut blocks = Vec::with_capacity(request.input.len());
    for part in &request.input {
        match part {
            ContentPart::Text { text } => blocks.push(ContentBlock::Text(TextBlock {
                kind: "text".into(),
                text: text.clone(),
                extra: BTreeMap::new(),
            })),
            ContentPart::Image { source, detail } => {
                if detail.is_some() {
                    return Err(protocol_error(
                        "Anthropic token counting cannot represent image detail",
                    ));
                }
                let MediaSource::Uri(url) = source else {
                    return Err(protocol_error(
                        "Anthropic token counting cannot encode media handles",
                    ));
                };
                blocks.push(ContentBlock::Image(ImageBlock {
                    kind: "image".into(),
                    source: AnthropicMediaSource {
                        kind: "url".into(),
                        media_type: None,
                        data: None,
                        url: Some(url.clone()),
                        extra: BTreeMap::new(),
                    },
                    extra: BTreeMap::new(),
                }));
            }
            ContentPart::InputAudio { .. }
            | ContentPart::InputFile { .. }
            | ContentPart::Refusal { .. } => {
                return Err(protocol_error(
                    "Anthropic token counting cannot represent this input part",
                ));
            }
        }
    }
    Ok(CountTokensRequest {
        model: provider_model.to_owned(),
        messages: vec![Message {
            role: Role::User,
            content: MessageContent::Blocks(blocks),
            extra: BTreeMap::new(),
        }],
        system: None,
        tools: Vec::new(),
        tool_choice: None,
        extra: BTreeMap::new(),
    })
}

fn secret_header(api_key: &AnthropicApiKey) -> Result<HeaderValue, TransportError> {
    let value = Zeroizing::new(api_key.expose().as_bytes().to_vec());
    HeaderValue::from_bytes(value.as_slice())
        .map_err(|_| protocol_error("Anthropic API key cannot be represented as a header"))
}

fn require_content_type(response: &Response, expected: &'static str) -> Result<(), TransportError> {
    let valid = response
        .headers()
        .get(header::CONTENT_TYPE)
        .and_then(|value| value.to_str().ok())
        .and_then(|value| value.split(';').next())
        .is_some_and(|value| value.trim().eq_ignore_ascii_case(expected));
    if valid {
        Ok(())
    } else {
        Err(transport_error(
            TransportPhase::FirstByte,
            AttemptFailureClass::Protocol,
            false,
            format!("Anthropic response must use content type {expected}"),
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
        let Some(chunk) = next else { break };
        let chunk = chunk.map_err(|error| {
            if first {
                map_first_body_error(error)
            } else {
                map_body_error(error, false)
            }
        })?;
        first = false;
        if output.len().saturating_add(chunk.len()) > maximum {
            return Err(protocol_body_error(format!(
                "Anthropic response exceeded the {maximum} byte limit"
            )));
        }
        output.extend_from_slice(&chunk);
    }
    if first {
        return Err(transport_error(
            TransportPhase::FirstByte,
            AttemptFailureClass::Protocol,
            false,
            "Anthropic response body was empty",
        ));
    }
    Ok(output)
}

struct DeadlineByteStream {
    source: ReqwestByteStream,
    idle_timeout: Duration,
    idle_sleep: Pin<Box<Sleep>>,
    attempt_deadline: Instant,
    terminal: bool,
}

impl DeadlineByteStream {
    fn new(source: ReqwestByteStream, idle_timeout: Duration, attempt_deadline: Instant) -> Self {
        Self {
            source,
            idle_timeout,
            idle_sleep: Box::pin(tokio::time::sleep_until(
                (Instant::now() + idle_timeout).min(attempt_deadline),
            )),
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
            return Poll::Ready(Some(Err(attempt_body_timeout())));
        }
        match self.source.as_mut().poll_next(context) {
            Poll::Ready(Some(Ok(chunk))) => {
                let wake = (Instant::now() + self.idle_timeout).min(self.attempt_deadline);
                self.idle_sleep.as_mut().reset(wake);
                return Poll::Ready(Some(Ok(chunk)));
            }
            Poll::Ready(Some(Err(error))) => {
                self.terminal = true;
                return Poll::Ready(Some(Err(map_body_error(error, false))));
            }
            Poll::Ready(None) => {
                self.terminal = true;
                return Poll::Ready(None);
            }
            Poll::Pending => {}
        }
        if self.idle_sleep.as_mut().poll(context).is_ready() {
            self.terminal = true;
            return Poll::Ready(Some(Err(if Instant::now() >= self.attempt_deadline {
                attempt_body_timeout()
            } else {
                body_idle_timeout()
            })));
        }
        Poll::Pending
    }
}

struct DecodedEventStream {
    bytes: DeadlineByteStream,
    decoder: AnthropicMessagesStreamDecoder,
    queued: VecDeque<CanonicalEvent>,
    committed: bool,
    terminal: bool,
}

impl DecodedEventStream {
    fn new(bytes: DeadlineByteStream, decoder: AnthropicMessagesStreamDecoder) -> Self {
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
                            self.protocol_error(format!("invalid Anthropic event stream: {error}"))
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
                                "truncated Anthropic event stream: {error}"
                            )))));
                        }
                    }
                }
                Poll::Pending => return Poll::Pending,
            }
        }
    }
}

fn safe_upstream_error_message(status: StatusCode, body: &[u8], api_key: &str) -> String {
    let message = serde_json::from_slice::<serde_json::Value>(body)
        .ok()
        .and_then(|value| value.get("error").cloned())
        .and_then(|error| {
            error
                .get("message")
                .and_then(|value| value.as_str())
                .map(str::to_owned)
        })
        .map(|message| message.replace(api_key, "[REDACTED]"))
        .map(|message| message.chars().take(512).collect::<String>());
    match message {
        Some(message) if !message.is_empty() => {
            format!("Anthropic returned HTTP {status}: {message}")
        }
        _ => format!("Anthropic returned HTTP {status}"),
    }
}

fn source_extensions(
    surface: Surface,
    values: BTreeMap<String, serde_json::Value>,
) -> SourceExtensions {
    let values = values
        .into_iter()
        .map(|(key, value)| {
            let escaped = key.replace('~', "~0").replace('/', "~1");
            (format!("/{escaped}"), value)
        })
        .collect();
    SourceExtensions::new(surface, values)
}

fn bounded_duration(configured: Duration, remaining: Duration) -> Duration {
    configured.min(remaining)
}

fn remaining(deadline: Instant, phase: TransportPhase) -> Result<Duration, TransportError> {
    deadline
        .checked_duration_since(Instant::now())
        .ok_or_else(|| {
            transport_error(
                phase,
                AttemptFailureClass::Timeout,
                false,
                "Anthropic attempt deadline elapsed",
            )
        })
}

fn remaining_until(phase_deadline: Instant, attempt_deadline: Instant) -> Option<Duration> {
    phase_deadline
        .min(attempt_deadline)
        .checked_duration_since(Instant::now())
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
    if error.is_connect() {
        transport_error(
            TransportPhase::Connect,
            if error.is_timeout() {
                AttemptFailureClass::Timeout
            } else {
                AttemptFailureClass::Connect
            },
            false,
            "Anthropic connection failed",
        )
    } else if error.is_timeout() {
        first_byte_timeout()
    } else {
        transport_error(
            TransportPhase::FirstByte,
            AttemptFailureClass::Connect,
            false,
            "Anthropic request failed before response headers",
        )
    }
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
        "Anthropic response body failed before its first byte",
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
        "Anthropic response body failed",
    )
}

fn first_byte_timeout() -> TransportError {
    transport_error(
        TransportPhase::FirstByte,
        AttemptFailureClass::Timeout,
        false,
        "Anthropic first-byte deadline elapsed",
    )
}

fn body_idle_timeout() -> TransportError {
    transport_error(
        TransportPhase::Body,
        AttemptFailureClass::Timeout,
        false,
        "Anthropic response idle deadline elapsed",
    )
}

fn attempt_body_timeout() -> TransportError {
    transport_error(
        TransportPhase::Body,
        AttemptFailureClass::Timeout,
        false,
        "Anthropic attempt deadline elapsed while reading the response",
    )
}

fn protocol_error(message: impl Into<String>) -> TransportError {
    transport_error(
        TransportPhase::Connect,
        AttemptFailureClass::Protocol,
        false,
        message,
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
    use std::{collections::BTreeMap, sync::Arc};

    use olp_domain::{
        AttemptPlan, DurationMs, GenerationParameters, GenerationRequest, Message as CoreMessage,
        MessageRole, OperationKind, ProviderId, RequestId, RequestMetadata, RouteId, RouteSlug,
        RuntimeGenerationId, SourceExtensions, TargetId, TokenCountRequest,
    };
    use tokio::{
        io::{AsyncReadExt, AsyncWriteExt},
        net::{TcpListener, TcpStream},
        sync::oneshot,
    };

    use super::*;
    use crate::anthropic::ConnectorTimeouts;

    struct MockResponse {
        chunks: Vec<(Duration, Vec<u8>)>,
    }

    struct InlineSpool;

    impl MediaSpool for InlineSpool {
        fn put<'a>(
            &'a self,
            _upload: olp_domain::MediaUpload,
        ) -> olp_domain::BoxFuture<'a, Result<olp_domain::MediaArtifact, olp_domain::MediaSpoolError>>
        {
            Box::pin(async { Err(olp_domain::MediaSpoolError::Unavailable) })
        }

        fn open<'a>(
            &'a self,
            handle: &'a olp_domain::MediaHandle,
        ) -> olp_domain::BoxFuture<'a, Result<olp_domain::OpenedMedia, olp_domain::MediaSpoolError>>
        {
            let handle = handle.clone();
            Box::pin(async move {
                Ok(olp_domain::OpenedMedia {
                    artifact: olp_domain::MediaArtifact {
                        handle,
                        content_type: Some("image/png".into()),
                        content_length: Some(2),
                    },
                    filename: "inline.png".into(),
                    bytes: Box::pin(stream::once(async { Ok(Bytes::from_static(b"hi")) })),
                })
            })
        }

        fn remove<'a>(
            &'a self,
            _handle: &'a olp_domain::MediaHandle,
        ) -> olp_domain::BoxFuture<'a, Result<(), olp_domain::MediaSpoolError>> {
            Box::pin(async { Ok(()) })
        }
    }

    #[tokio::test]
    async fn same_protocol_base64_image_handle_is_rehydrated() {
        let handle = olp_domain::MediaHandle::new("inline");
        let mut messages = vec![Message {
            role: Role::User,
            content: MessageContent::Blocks(vec![ContentBlock::Image(ImageBlock {
                kind: "image".into(),
                source: AnthropicMediaSource {
                    kind: "base64".into(),
                    media_type: Some("image/png".into()),
                    data: Some(olp_domain::inline_media_marker(&handle)),
                    url: None,
                    extra: BTreeMap::new(),
                },
                extra: BTreeMap::new(),
            })]),
            extra: BTreeMap::new(),
        }];
        let spool: Arc<dyn MediaSpool> = Arc::new(InlineSpool);
        hydrate_anthropic_messages(&mut messages, Some(&spool))
            .await
            .unwrap();
        let MessageContent::Blocks(blocks) = &messages[0].content else {
            panic!("expected blocks")
        };
        let ContentBlock::Image(image) = &blocks[0] else {
            panic!("expected image")
        };
        assert_eq!(image.source.data.as_deref(), Some("aGk="));
    }

    async fn spawn_mock(response: MockResponse) -> (String, oneshot::Receiver<Vec<u8>>) {
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let address = listener.local_addr().unwrap();
        let (sender, receiver) = oneshot::channel();
        tokio::spawn(async move {
            let (mut socket, _) = listener.accept().await.unwrap();
            let request = read_request(&mut socket).await;
            let _ = sender.send(request);
            for (delay, chunk) in response.chunks {
                tokio::time::sleep(delay).await;
                if socket.write_all(&chunk).await.is_err() {
                    return;
                }
                let _ = socket.flush().await;
            }
        });
        (format!("http://{address}/v1/"), receiver)
    }

    async fn read_request(socket: &mut TcpStream) -> Vec<u8> {
        let mut request = Vec::new();
        let mut buffer = [0_u8; 4096];
        let mut expected = None;
        loop {
            let read = socket.read(&mut buffer).await.unwrap();
            if read == 0 {
                return request;
            }
            request.extend_from_slice(&buffer[..read]);
            if expected.is_none()
                && let Some(end) = find_bytes(&request, b"\r\n\r\n")
            {
                let headers = String::from_utf8_lossy(&request[..end]);
                let length = headers
                    .lines()
                    .find_map(|line| {
                        let (name, value) = line.split_once(':')?;
                        name.eq_ignore_ascii_case("content-length")
                            .then(|| value.trim().parse::<usize>().ok())
                            .flatten()
                    })
                    .unwrap_or_default();
                expected = Some(end + 4 + length);
            }
            if expected.is_some_and(|length| request.len() >= length) {
                return request;
            }
        }
    }

    fn find_bytes(haystack: &[u8], needle: &[u8]) -> Option<usize> {
        haystack
            .windows(needle.len())
            .position(|part| part == needle)
    }

    fn attempt(
        operation: OperationKind,
        mode: TransportMode,
        operation_value: Operation,
    ) -> ProviderRequest {
        ProviderRequest {
            metadata: RequestMetadata {
                request_id: RequestId::new(),
                operation,
                surface: Surface::Anthropic,
                mode,
            },
            attempt: AttemptPlan {
                generation_id: RuntimeGenerationId::new(),
                route_id: RouteId::new(),
                target_id: TargetId::new(),
                provider_id: ProviderId::new(),
                provider_kind: ProviderKind::Anthropic,
                provider_model: "claude-sonnet-4-5".into(),
                timeout: DurationMs::new(2_000),
                priority: 0,
            },
            operation: operation_value,
            media: None,
        }
    }

    fn generation(streaming: bool) -> ProviderRequest {
        attempt(
            OperationKind::Generation,
            if streaming {
                TransportMode::Streaming
            } else {
                TransportMode::Unary
            },
            Operation::Generation(GenerationRequest {
                route: RouteSlug::parse("default").unwrap(),
                messages: vec![CoreMessage {
                    role: MessageRole::User,
                    content: vec![ContentPart::Text {
                        text: "hello".into(),
                    }],
                    name: None,
                    tool_call_id: None,
                    tool_calls: Vec::new(),
                }],
                parameters: GenerationParameters {
                    max_output_tokens: Some(32),
                    stream: streaming,
                    ..GenerationParameters::default()
                },
                tools: Vec::new(),
                tool_choice: None,
                response_format: None,
                extensions: SourceExtensions::new(Surface::Anthropic, BTreeMap::new()),
            }),
        )
    }

    fn count() -> ProviderRequest {
        attempt(
            OperationKind::TokenCount,
            TransportMode::Unary,
            Operation::TokenCount(TokenCountRequest {
                route: RouteSlug::parse("default").unwrap(),
                input: vec![ContentPart::Text {
                    text: "hello".into(),
                }],
                extensions: SourceExtensions::default(),
            }),
        )
    }

    #[test]
    fn preserved_count_tokens_body_is_forwarded_exactly_with_late_bound_model() {
        let mut request = count();
        let Operation::TokenCount(count) = &mut request.operation else {
            unreachable!()
        };
        count.extensions = SourceExtensions::new(
            Surface::Anthropic,
            BTreeMap::from([(
                ANTHROPIC_COUNT_REQUEST_EXTENSION.into(),
                serde_json::json!({
                    "model": "public-route",
                    "system": "keep system",
                    "messages": [{"role":"user","content":"hello"}],
                    "tools": [{"name":"lookup","input_schema":{"type":"object"}}],
                    "vendor": true
                }),
            )]),
        );
        let wire = encode_count_tokens(count, "claude-private").unwrap();
        let wire = serde_json::to_value(wire).unwrap();
        assert_eq!(wire["model"], "claude-private");
        assert_eq!(wire["system"], "keep system");
        assert_eq!(wire["tools"][0]["name"], "lookup");
        assert_eq!(wire["vendor"], true);
    }

    fn connector(base_url: &str) -> AnthropicConnector {
        AnthropicConnector::new(
            ConnectorConfig::for_local_test(base_url, ConnectorTimeouts::default()),
            AnthropicApiKey::new("upstream-secret").unwrap(),
        )
    }

    fn response(content_type: &str, body: &[u8]) -> Vec<u8> {
        let headers = format!(
            "HTTP/1.1 200 OK\r\nContent-Type: {content_type}\r\nContent-Length: {}\r\nConnection: close\r\n\r\n",
            body.len()
        );
        [headers.as_bytes(), body].concat()
    }

    #[tokio::test]
    async fn model_discovery_uses_anthropic_pagination_contract() {
        let body = br#"{"data":[{"id":"claude-test","display_name":"Claude Test"}],"has_more":false,"last_id":"claude-test"}"#;
        let (base_url, captured) = spawn_mock(MockResponse {
            chunks: vec![(Duration::ZERO, response("application/json", body))],
        })
        .await;
        let models = connector(&base_url).discover_models().await.unwrap();
        assert_eq!(models[0].display_name, "Claude Test");
        let request = String::from_utf8(captured.await.unwrap()).unwrap();
        assert!(request.starts_with("GET /v1/models?limit=100 "));
        assert!(request.contains("x-api-key: upstream-secret"));
    }

    async fn collect(
        connector: &AnthropicConnector,
        request: ProviderRequest,
    ) -> Vec<CanonicalEvent> {
        let ProviderOutput::Events(mut stream) = connector.execute(request).await.unwrap() else {
            panic!("Anthropic connector returned a unary result for an event operation");
        };
        let mut events = Vec::new();
        while let Some(event) = stream.next().await {
            events.push(event.unwrap());
        }
        events
    }

    #[tokio::test]
    async fn executes_unary_messages_with_late_bound_headers() {
        let body = serde_json::to_vec(&serde_json::json!({
            "id":"msg_1","type":"message","role":"assistant",
            "content":[{"type":"text","text":"hello back"}],
            "model":"claude-sonnet-4-5","stop_reason":"end_turn","stop_sequence":null,
            "usage":{"input_tokens":2,"output_tokens":2}
        }))
        .unwrap();
        let (base, captured) = spawn_mock(MockResponse {
            chunks: vec![(Duration::ZERO, response("application/json", &body))],
        })
        .await;
        let events = collect(&connector(&base), generation(false)).await;
        assert!(events.iter().any(|event| matches!(&event.kind, CanonicalEventKind::TextDelta { text, .. } if text == "hello back")));
        assert!(matches!(
            events.last().map(|event| &event.kind),
            Some(CanonicalEventKind::Done)
        ));
        let request = String::from_utf8(captured.await.unwrap())
            .unwrap()
            .to_ascii_lowercase();
        assert!(request.starts_with("post /v1/messages "));
        assert!(request.contains("x-api-key: upstream-secret"));
        assert!(request.contains("anthropic-version: 2023-06-01"));
        assert!(request.contains("\"model\":\"claude-sonnet-4-5\""));
    }

    #[tokio::test]
    async fn decodes_fragmented_stream_and_token_count() {
        let sse = concat!(
            "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_2\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude\",\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":2,\"output_tokens\":0}}}\n\n",
            "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
            "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"snow ☃\"}}\n\n",
            "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
            "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":2}}\n\n",
            "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
        ).as_bytes().to_vec();
        let split = find_bytes(&sse, "☃".as_bytes()).unwrap() + 1;
        let headers =
            b"HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nConnection: close\r\n\r\n";
        let (base, _) = spawn_mock(MockResponse {
            chunks: vec![
                (Duration::ZERO, [headers.as_slice(), &sse[..split]].concat()),
                (Duration::from_millis(2), sse[split..].to_vec()),
            ],
        })
        .await;
        let events = collect(&connector(&base), generation(true)).await;
        assert!(events.iter().any(|event| matches!(&event.kind, CanonicalEventKind::TextDelta { text, .. } if text == "snow ☃")));

        let count_body = br#"{"input_tokens":7}"#;
        let (base, captured) = spawn_mock(MockResponse {
            chunks: vec![(Duration::ZERO, response("application/json", count_body))],
        })
        .await;
        let output = connector(&base).execute(count()).await.unwrap();
        assert!(matches!(
            output,
            ProviderOutput::Result(result)
                if matches!(*result, CanonicalResult::TokenCount(TokenCountResult { input_tokens: 7, .. }))
        ));
        let request = String::from_utf8(captured.await.unwrap()).unwrap();
        assert!(request.starts_with("POST /v1/messages/count_tokens "));
    }

    #[tokio::test]
    async fn redirects_are_not_followed_and_errors_redact_credentials() {
        let redirect = b"HTTP/1.1 302 Found\r\nLocation: http://169.254.169.254/latest\r\nContent-Length: 0\r\nConnection: close\r\n\r\n";
        let (base, _) = spawn_mock(MockResponse {
            chunks: vec![(Duration::ZERO, redirect.to_vec())],
        })
        .await;
        let error = connector(&base)
            .execute(generation(false))
            .await
            .err()
            .unwrap();
        assert_eq!(error.class, AttemptFailureClass::UpstreamClient);

        let message = safe_upstream_error_message(
            StatusCode::BAD_REQUEST,
            br#"{"error":{"message":"bad upstream-secret","private":"do-not-echo"}}"#,
            "upstream-secret",
        );
        assert!(message.contains("[REDACTED]"));
        assert!(!message.contains("upstream-secret"));
        assert!(!message.contains("do-not-echo"));
    }

    #[tokio::test]
    #[ignore = "requires OLP_LIVE_ANTHROPIC_API_KEY"]
    async fn live_provider_discovers_anthropic_models() {
        let key = std::env::var("OLP_LIVE_ANTHROPIC_API_KEY")
            .expect("set OLP_LIVE_ANTHROPIC_API_KEY for the ignored live test");
        let connector = AnthropicConnector::new(
            ConnectorConfig::default(),
            AnthropicApiKey::new(key).expect("live Anthropic key must be representable"),
        );
        assert!(!connector.discover_models().await.unwrap().is_empty());
    }
}
