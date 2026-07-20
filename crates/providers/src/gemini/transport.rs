use std::{collections::BTreeMap, fmt, future::ready};

#[cfg(test)]
use std::time::Duration;

use base64::{Engine as _, engine::general_purpose::STANDARD};
#[cfg(test)]
use bytes::Bytes;
use futures::{StreamExt, stream};
use http::{HeaderMap, HeaderValue, StatusCode, header};
use olp_domain::{
    AttemptFailureClass, CanonicalEvent, CanonicalResult, ContentPart, DiscoveredProviderModel,
    MediaSource, MediaSpool, Operation, ProviderEventStream, ProviderKind, ProviderOutput,
    ProviderRequest, ProviderTransport, SourceExtensions, Surface, TokenCountResult,
    TransportError, TransportMode, TransportPhase, media_handle_from_inline_marker,
};
use olp_protocols::gemini::{
    Content, CountTokensRequest, CountTokensResponse, FileData, FileDataPart,
    GEMINI_COUNT_REQUEST_EXTENSION, GeminiGenerateContentStreamDecoder, GenerateContentResponse,
    Part, TextPart, decode_generate_content_response, encode_generate_content_request,
    validate_count_tokens_request,
};
use reqwest::{Response, Url};
use tokio::time::{Instant, timeout};
use zeroize::Zeroizing;

use crate::gemini::{
    BearerTokenProvider, ConnectorConfig, ConnectorCredential, GeminiApiKey,
    endpoint::EndpointError, headers::sanitize_forward_headers,
};
use crate::transport_io::{
    CanonicalEventDecoder, DecodedEventStream, ProviderResponseIo, ReqwestByteStream,
    bounded_duration,
};

const RESPONSE_IO: ProviderResponseIo = ProviderResponseIo::new("Gemini");

impl CanonicalEventDecoder for GeminiGenerateContentStreamDecoder {
    type Error = olp_protocols::gemini::StreamError;

    fn push(&mut self, bytes: &[u8]) -> Result<Vec<CanonicalEvent>, Self::Error> {
        Self::push(self, bytes)
    }

    fn finish(&mut self) -> Result<Vec<CanonicalEvent>, Self::Error> {
        Self::finish(self)
    }
}

/// Validates the concrete canonical request with the production Gemini
/// encoders before routing. This is especially important for cross-origin
/// token-count requests, whose source-scoped exact bodies cannot be translated
/// to another vendor protocol.
pub fn validate_operation(
    operation: &Operation,
    provider_model: &str,
) -> Result<(), TransportError> {
    match operation {
        Operation::Generation(generation) => encode_generate_content_request(generation)
            .map(|_| ())
            .map_err(|error| protocol_error(error.to_string())),
        Operation::TokenCount(count) => encode_count_tokens(count, provider_model).map(|_| ()),
        operation => Err(protocol_error(format!(
            "Gemini connector does not support {:?}",
            operation.kind()
        ))),
    }
}

#[derive(Clone, Copy)]
enum ResponseKind {
    Generation,
    TokenCount,
}

pub struct GeminiConnector {
    config: ConnectorConfig,
    credential: ConnectorCredential,
    provider_kind: ProviderKind,
}

impl GeminiConnector {
    #[must_use]
    pub fn new(config: ConnectorConfig, api_key: GeminiApiKey) -> Self {
        Self {
            config,
            credential: ConnectorCredential::ApiKey(api_key),
            provider_kind: ProviderKind::Gemini,
        }
    }

    /// Builds a Google OAuth transport (used by Vertex AI) while retaining the
    /// Gemini canonical codecs and bounded response machinery.
    #[must_use]
    pub fn with_bearer_token_provider(
        config: ConnectorConfig,
        provider_kind: ProviderKind,
        provider: std::sync::Arc<dyn BearerTokenProvider>,
    ) -> Self {
        Self {
            config,
            credential: ConnectorCredential::Bearer(provider),
            provider_kind,
        }
    }

    /// Lists all Gemini models with explicit pagination through a newly
    /// DNS-pinned, redirect-free client for every upstream page.
    pub async fn discover_models(&self) -> Result<Vec<DiscoveredProviderModel>, TransportError> {
        let mut discovered = Vec::new();
        let mut page_token: Option<String> = None;
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
                query.append_pair("pageSize", "1000");
                if let Some(page_token) = &page_token {
                    query.append_pair("pageToken", page_token);
                }
            }
            let mut headers = sanitize_forward_headers(&HeaderMap::new());
            self.insert_authentication_header(&mut headers).await?;
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
                protocol_body_error(format!("Gemini model discovery is not valid JSON: {error}"))
            })?;
            let models = value
                .get("models")
                .and_then(serde_json::Value::as_array)
                .ok_or_else(|| protocol_body_error("Gemini model discovery omitted models"))?;
            for model in models {
                let id = model
                    .get("name")
                    .and_then(serde_json::Value::as_str)
                    .filter(|id| !id.is_empty())
                    .ok_or_else(|| {
                        protocol_body_error("Gemini model discovery returned an invalid name")
                    })?;
                let display_name = model
                    .get("displayName")
                    .and_then(serde_json::Value::as_str)
                    .filter(|name| !name.is_empty())
                    .unwrap_or(id);
                discovered.push(DiscoveredProviderModel {
                    id: id.to_owned(),
                    display_name: display_name.to_owned(),
                });
            }
            page_token = value
                .get("nextPageToken")
                .and_then(serde_json::Value::as_str)
                .filter(|token| !token.is_empty())
                .map(str::to_owned);
            if page_token.is_none() {
                return Ok(discovered);
            }
        }
        Err(protocol_body_error(
            "Gemini model discovery exceeded 100 pages",
        ))
    }

    /// Performs a minimal credentialed token-count call for providers (such as
    /// Vertex publisher models) that do not expose a list endpoint in the same
    /// resource collection.
    pub async fn probe_model(&self, provider_model: &str) -> Result<(), TransportError> {
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
            .count_tokens_url(provider_model)
            .map_err(map_endpoint_error)?;
        let body = br#"{"contents":[{"role":"user","parts":[{"text":"health"}]}]}"#;
        let mut headers = sanitize_forward_headers(&HeaderMap::new());
        self.insert_authentication_header(&mut headers).await?;
        headers.insert(
            header::CONTENT_TYPE,
            HeaderValue::from_static("application/json"),
        );
        headers.insert(header::ACCEPT, HeaderValue::from_static("application/json"));
        let first_byte_deadline = Instant::now() + self.config.timeouts.first_byte;
        let response = timeout(
            self.config.timeouts.first_byte,
            client
                .post(url)
                .headers(headers)
                .body(body.as_slice())
                .send(),
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
        let response: CountTokensResponse = serde_json::from_slice(&body).map_err(|error| {
            protocol_body_error(format!(
                "Google token-count probe is not valid JSON: {error}"
            ))
        })?;
        if response.total_tokens == 0 {
            return Err(protocol_body_error(
                "Google token-count probe returned an invalid zero token count",
            ));
        }
        Ok(())
    }

    async fn execute_request(
        &self,
        request: ProviderRequest,
    ) -> Result<ProviderOutput, TransportError> {
        validate_request_envelope(&request, self.provider_kind)?;
        let (url, body, response_kind, streaming) = self.encode_request(&request).await?;
        let attempt_deadline = Instant::now() + request.attempt.timeout.as_duration();
        let connect_timeout = bounded_duration(
            self.config.timeouts.connect,
            RESPONSE_IO.remaining(attempt_deadline, TransportPhase::Connect)?,
        );
        // No credential is copied into request state before DNS validation and
        // per-attempt address pinning have succeeded.
        let client = self
            .config
            .endpoint
            .pinned_client(connect_timeout)
            .await
            .map_err(map_endpoint_error)?;

        let mut headers = sanitize_forward_headers(&HeaderMap::new());
        let auth_wait = RESPONSE_IO.remaining(attempt_deadline, TransportPhase::Connect)?;
        timeout(auth_wait, self.insert_authentication_header(&mut headers))
            .await
            .map_err(|_| {
                transport_error(
                    TransportPhase::Connect,
                    AttemptFailureClass::Timeout,
                    false,
                    "Google credential acquisition exceeded the attempt deadline",
                )
            })??;
        let first_byte_deadline = Instant::now() + self.config.timeouts.first_byte;
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
                request.metadata.surface == Surface::Gemini,
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
                let mut wire = encode_generate_content_request(generation).map_err(|error| {
                    protocol_error(format!("cannot encode Gemini generation request: {error}"))
                })?;
                hydrate_gemini_contents(&mut wire.contents, request.media.as_ref()).await?;
                let body = serde_json::to_vec(&wire).map_err(|error| {
                    protocol_error(format!("cannot serialize Gemini request: {error}"))
                })?;
                Ok((
                    self.config
                        .endpoint
                        .generate_url(&request.attempt.provider_model, streaming)
                        .map_err(map_endpoint_error)?,
                    body,
                    ResponseKind::Generation,
                    streaming,
                ))
            }
            Operation::TokenCount(count) => {
                if request.metadata.mode != TransportMode::Unary {
                    return Err(protocol_error(
                        "Gemini token counting supports unary mode only",
                    ));
                }
                let mut wire = encode_count_tokens(count, &request.attempt.provider_model)?;
                hydrate_gemini_contents(&mut wire.contents, request.media.as_ref()).await?;
                if let Some(generation) = &mut wire.generate_content_request {
                    hydrate_gemini_contents(&mut generation.contents, request.media.as_ref())
                        .await?;
                }
                validate_count_tokens_request(&wire).map_err(|error| {
                    protocol_error(format!("invalid Gemini count request: {error}"))
                })?;
                let body = serde_json::to_vec(&wire).map_err(|error| {
                    protocol_error(format!("cannot serialize Gemini count request: {error}"))
                })?;
                Ok((
                    self.config
                        .endpoint
                        .count_tokens_url(&request.attempt.provider_model)
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
                "Gemini connector does not support {:?}",
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
                let response: GenerateContentResponse =
                    serde_json::from_slice(&body).map_err(|error| {
                        protocol_body_error(format!("Gemini response is not valid JSON: {error}"))
                    })?;
                let events = decode_generate_content_response(response).map_err(|error| {
                    protocol_body_error(format!("Gemini response is invalid: {error}"))
                })?;
                Ok(ProviderOutput::Events(Box::pin(stream::iter(
                    events.into_iter().map(Ok),
                ))))
            }
            ResponseKind::TokenCount => {
                let response: CountTokensResponse =
                    serde_json::from_slice(&body).map_err(|error| {
                        protocol_body_error(format!(
                            "Gemini count response is not valid JSON: {error}"
                        ))
                    })?;
                let mut extensions = response.extra;
                if let Some(cached) = response.cached_content_token_count {
                    extensions.insert("cachedContentTokenCount".into(), cached.into());
                }
                Ok(ProviderOutput::Result(Box::new(
                    CanonicalResult::TokenCount(TokenCountResult {
                        input_tokens: response.total_tokens,
                        extensions: source_extensions(Surface::Gemini, extensions),
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
                    "Gemini stream ended before its first body byte",
                )
            })?
            .map_err(|error| RESPONSE_IO.map_first_body_error(error))?;
        let source = Box::pin(stream::once(ready(Ok(first))).chain(source));
        let bytes = RESPONSE_IO.after_first_byte_stream(
            source,
            self.config.timeouts.idle,
            attempt_deadline,
        );
        let decoder = GeminiGenerateContentStreamDecoder::with_max_event_bytes_and_raw_passthrough(
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
            Ok(body) => self.safe_upstream_error_message(status, &body),
            Err(_) => format!("Gemini returned HTTP {status}"),
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

    async fn insert_authentication_header(
        &self,
        headers: &mut HeaderMap,
    ) -> Result<(), TransportError> {
        match &self.credential {
            ConnectorCredential::ApiKey(api_key) => {
                headers.insert("x-goog-api-key", secret_header(api_key)?);
            }
            ConnectorCredential::Bearer(provider) => {
                let token = provider.token().await.map_err(|_| {
                    transport_error(
                        TransportPhase::Connect,
                        AttemptFailureClass::Connect,
                        false,
                        "Google OAuth bearer token acquisition failed",
                    )
                })?;
                headers.insert(header::AUTHORIZATION, bearer_header(&token)?);
            }
        }
        Ok(())
    }

    fn safe_upstream_error_message(&self, status: StatusCode, body: &[u8]) -> String {
        match &self.credential {
            ConnectorCredential::ApiKey(api_key) => {
                safe_upstream_error_message(status, body, api_key.expose())
            }
            // OAuth tokens are deliberately not retained by the connector, so
            // do not surface an upstream body that could reflect one.
            ConnectorCredential::Bearer(_) => format!("Google provider returned HTTP {status}"),
        }
    }
}

const MAX_INLINE_MEDIA_BYTES: usize = 1024 * 1024;

async fn hydrate_gemini_contents(
    contents: &mut [Content],
    spool: Option<&std::sync::Arc<dyn MediaSpool>>,
) -> Result<(), TransportError> {
    for content in contents {
        for part in &mut content.parts {
            let Part::InlineData(part) = part else {
                continue;
            };
            if media_handle_from_inline_marker(&part.inline_data.data).is_some() {
                part.inline_data.data = read_inline_media(&part.inline_data.data, spool).await?;
            }
        }
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

impl fmt::Debug for GeminiConnector {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("GeminiConnector")
            .field("config", &self.config)
            .field("credential", &"[REDACTED]")
            .field("provider_kind", &self.provider_kind)
            .finish()
    }
}

impl ProviderTransport for GeminiConnector {
    fn execute<'a>(
        &'a self,
        request: ProviderRequest,
    ) -> olp_domain::BoxFuture<'a, Result<ProviderOutput, TransportError>> {
        Box::pin(async move { self.execute_request(request).await })
    }
}

fn validate_request_envelope(
    request: &ProviderRequest,
    provider_kind: ProviderKind,
) -> Result<(), TransportError> {
    if request.metadata.operation != request.operation.kind() {
        return Err(protocol_error(
            "request metadata operation does not match the canonical operation",
        ));
    }
    if request.attempt.provider_kind != provider_kind {
        return Err(protocol_error(
            "Gemini connector received an attempt for another provider kind",
        ));
    }
    if request.metadata.mode == TransportMode::Async {
        return Err(protocol_error(
            "Gemini connector does not support asynchronous mode",
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
        .ensure_representable_on(Surface::Gemini)
        .map_err(|error| protocol_error(error.to_string()))?;
    let mut extensions = request.extensions.values.clone();
    if let Some(value) = extensions.remove(GEMINI_COUNT_REQUEST_EXTENSION) {
        if !extensions.is_empty() {
            return Err(protocol_error(
                "Gemini token-count extensions cannot be reconstructed without losing semantics",
            ));
        }
        let mut wire: CountTokensRequest = serde_json::from_value(value).map_err(|error| {
            protocol_error(format!(
                "preserved Gemini countTokens request is invalid: {error}"
            ))
        })?;
        if let Some(generation) = &mut wire.generate_content_request {
            generation.model = Some(format!("models/{provider_model}"));
        }
        validate_count_tokens_request(&wire).map_err(|error| {
            protocol_error(format!(
                "preserved Gemini countTokens request is invalid: {error}"
            ))
        })?;
        return Ok(wire);
    }
    if request.input.is_empty() {
        return Err(protocol_error("token-count input cannot be empty"));
    }
    let mut parts = Vec::with_capacity(request.input.len());
    let mut remaining_extensions = extensions;
    for part in &request.input {
        match part {
            ContentPart::Text { text } => parts.push(Part::Text(TextPart {
                text: text.clone(),
                thought: None,
                thought_signature: None,
                extra: BTreeMap::new(),
            })),
            ContentPart::Image { source, detail } => {
                if detail.is_some() {
                    return Err(protocol_error(
                        "Gemini token counting cannot represent image detail",
                    ));
                }
                let MediaSource::Uri(file_uri) = source else {
                    return Err(protocol_error(
                        "Gemini token counting cannot encode media handles",
                    ));
                };
                let mime_path = format!("/contents/0/parts/{}/fileData/mimeType", parts.len());
                let mime_type = remaining_extensions
                    .remove(&mime_path)
                    .and_then(|value| value.as_str().map(str::to_owned))
                    .ok_or_else(|| {
                        protocol_error(format!(
                            "Gemini image token counting requires a MIME type extension at {mime_path}"
                        ))
                    })?;
                parts.push(Part::FileData(FileDataPart {
                    file_data: FileData {
                        mime_type,
                        file_uri: file_uri.clone(),
                        extra: BTreeMap::new(),
                    },
                    extra: BTreeMap::new(),
                }));
            }
            ContentPart::InputAudio { .. }
            | ContentPart::InputFile { .. }
            | ContentPart::Refusal { .. } => {
                return Err(protocol_error(
                    "Gemini token counting cannot represent this input part",
                ));
            }
        }
    }
    if !remaining_extensions.is_empty() {
        return Err(protocol_error(
            "Gemini token-count extensions cannot be reconstructed without losing semantics",
        ));
    }
    Ok(CountTokensRequest {
        contents: vec![Content {
            role: Some("user".into()),
            parts,
            extra: BTreeMap::new(),
        }],
        generate_content_request: None,
        extra: BTreeMap::new(),
    })
}

fn secret_header(api_key: &GeminiApiKey) -> Result<HeaderValue, TransportError> {
    let value = Zeroizing::new(api_key.expose().as_bytes().to_vec());
    HeaderValue::from_bytes(value.as_slice())
        .map_err(|_| protocol_error("Gemini API key cannot be represented as a header"))
}

fn bearer_header(token: &crate::gemini::SecretBearerToken) -> Result<HeaderValue, TransportError> {
    let mut value = Zeroizing::new(Vec::with_capacity(7 + token.expose().len()));
    value.extend_from_slice(b"Bearer ");
    value.extend_from_slice(token.expose().as_bytes());
    HeaderValue::from_bytes(value.as_slice())
        .map_err(|_| protocol_error("Google OAuth token cannot be represented as a header"))
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
        Some(message) if !message.is_empty() => format!("Gemini returned HTTP {status}: {message}"),
        _ => format!("Gemini returned HTTP {status}"),
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
            "Gemini connection failed",
        )
    } else if error.is_timeout() {
        RESPONSE_IO.first_byte_timeout()
    } else {
        transport_error(
            TransportPhase::FirstByte,
            AttemptFailureClass::Connect,
            false,
            "Gemini request failed before response headers",
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
