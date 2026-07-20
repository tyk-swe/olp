use std::{collections::BTreeMap, fmt, future::ready};

#[cfg(test)]
use std::time::Duration;

use base64::{Engine as _, engine::general_purpose::STANDARD};
#[cfg(test)]
use bytes::Bytes;
use futures::{StreamExt, stream};
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
use tokio::time::{Instant, timeout};
use zeroize::Zeroizing;

use crate::anthropic::{AnthropicApiKey, ConnectorConfig, endpoint::EndpointError};
use crate::transport_io::{
    CanonicalEventDecoder, DecodedEventStream, ProviderResponseIo, ReqwestByteStream,
    bounded_duration,
};

const RESPONSE_IO: ProviderResponseIo = ProviderResponseIo::new("Anthropic");

impl CanonicalEventDecoder for AnthropicMessagesStreamDecoder {
    type Error = olp_protocols::anthropic::StreamError;

    fn push(&mut self, bytes: &[u8]) -> Result<Vec<CanonicalEvent>, Self::Error> {
        Self::push(self, bytes)
    }

    fn finish(&mut self) -> Result<Vec<CanonicalEvent>, Self::Error> {
        Self::finish(self)
    }
}

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
            let mut headers = HeaderMap::new();
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
            .map_err(|_| RESPONSE_IO.first_byte_timeout())?
            .map_err(map_send_error)?;
            if !response.status().is_success() {
                return Err(self.map_error_response(response, attempt_deadline).await);
            }
            RESPONSE_IO.require_content_type(&response, "application/json")?;
            let body = RESPONSE_IO
                .read_bounded_body(
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
            RESPONSE_IO.remaining(attempt_deadline, TransportPhase::Connect)?,
        );
        // Resolve, validate, and pin before materializing the credential header.
        let client = self
            .config
            .endpoint
            .pinned_client(connect_timeout)
            .await
            .map_err(map_endpoint_error)?;

        let first_byte_deadline = Instant::now() + self.config.timeouts.first_byte;
        let mut headers = HeaderMap::new();
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

        let send_wait = RESPONSE_IO
            .remaining_until(first_byte_deadline, attempt_deadline)
            .ok_or_else(|| RESPONSE_IO.first_byte_timeout())?;
        let response = timeout(
            send_wait,
            client.post(url).headers(headers).body(body).send(),
        )
        .await
        .map_err(|_| RESPONSE_IO.first_byte_timeout())?
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
                "model operations are installation-local and are not routed to providers",
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
        RESPONSE_IO.require_content_type(&response, "application/json")?;
        let body = RESPONSE_IO
            .read_bounded_body(
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
        RESPONSE_IO.require_content_type(&response, "text/event-stream")?;
        let mut source: ReqwestByteStream = Box::pin(response.bytes_stream());
        let first_wait = RESPONSE_IO
            .remaining_until(first_byte_deadline, attempt_deadline)
            .ok_or_else(|| RESPONSE_IO.first_byte_timeout())?;
        let first = timeout(first_wait, source.next())
            .await
            .map_err(|_| RESPONSE_IO.first_byte_timeout())?
            .ok_or_else(|| {
                transport_error(
                    TransportPhase::FirstByte,
                    AttemptFailureClass::Protocol,
                    false,
                    "Anthropic stream ended before its first body byte",
                )
            })?
            .map_err(|error| RESPONSE_IO.map_first_body_error(error))?;
        let source = Box::pin(stream::once(ready(Ok(first))).chain(source));
        let bytes = RESPONSE_IO.after_first_byte_stream(
            source,
            self.config.timeouts.idle,
            attempt_deadline,
        );
        let decoder = AnthropicMessagesStreamDecoder::with_max_event_bytes_and_raw_passthrough(
            self.config.max_event_bytes,
            preserve_raw_frames,
        );
        Ok(Box::pin(DecodedEventStream::new(
            RESPONSE_IO,
            bytes,
            decoder,
        )))
    }

    async fn map_error_response(
        &self,
        response: Response,
        attempt_deadline: Instant,
    ) -> TransportError {
        let status = response.status();
        let deadline = Instant::now() + self.config.timeouts.first_byte;
        let message = match RESPONSE_IO
            .read_bounded_body(
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
        RESPONSE_IO.first_byte_timeout()
    } else {
        transport_error(
            TransportPhase::FirstByte,
            AttemptFailureClass::Connect,
            false,
            "Anthropic request failed before response headers",
        )
    }
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
mod tests;
