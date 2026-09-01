use std::{collections::BTreeMap, fmt};

use crate::domain::{
    canonical::{
        identity::{Surface, TransportMode},
        requests::{ContentPart, MediaSource, Operation},
        results::{CanonicalResult, TokenCountResult},
    },
    ports::{
        DiscoveredProviderModel, ProviderOutput, ProviderRequest, ProviderTransport,
        TransportError, TransportPhase,
    },
    routing::provider::ProviderKind,
};
use crate::protocols::anthropic::{
    count::ANTHROPIC_COUNT_REQUEST_EXTENSION,
    dto::{
        ContentBlock, CountTokensRequest, CountTokensResponse, ImageBlock,
        MediaSource as AnthropicMediaSource, Message, MessageContent, MessagesResponse, Role,
        TextBlock,
    },
    translate::{encode::request as encode_request, response::decode},
};
use crate::providers::transport_common::{inject_trace_context, request_id_header};
use futures::stream;
use http::{HeaderMap, HeaderValue, header};
use reqwest::{Response, Url};
use tokio::time::Instant;

use super::errors::*;
use super::media::hydrate_anthropic_messages;
use crate::providers::anthropic::{ApiKey, ConnectorConfig};
use crate::providers::transport_common::upstream_response_error;
use crate::providers::transport_common::{protocol_body_error, protocol_error, source_extensions};
use crate::providers::transport_io::{ProviderResponseIo, bounded_duration};

const RESPONSE_IO: ProviderResponseIo = ProviderResponseIo::new("Anthropic");

/// Validates the concrete canonical request against the same encoders used by
/// the production transport. The gateway invokes this before attempt ordering
/// so a cross-origin capability remains eligible only when no source semantics
/// would be lost.
pub fn validate_operation(
    operation: &Operation,
    upstream_model: &str,
) -> Result<(), TransportError> {
    match operation {
        Operation::Generation(generation) => encode_request(generation, upstream_model)
            .map(|_| ())
            .map_err(|error| protocol_error(error.to_string())),
        Operation::TokenCount(count) => encode_count_tokens(count, upstream_model).map(|_| ()),
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

pub struct Connector {
    pub(super) config: ConnectorConfig,
    pub(super) api_key: ApiKey,
}

impl Connector {
    #[must_use]
    pub fn new(config: ConnectorConfig, api_key: ApiKey) -> Self {
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
            let response = RESPONSE_IO
                .send_before(
                    client.get(url).headers(headers),
                    first_byte_deadline,
                    attempt_deadline,
                    map_send_error,
                )
                .await?;
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
            request_id_header(request.metadata.request_id)?,
        );
        inject_trace_context(&mut headers, request.propagate_trace_context);

        let response = RESPONSE_IO
            .send_before(
                client.post(url).headers(headers).body(body),
                first_byte_deadline,
                attempt_deadline,
                map_send_error,
            )
            .await?;

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
        match &*request.operation {
            Operation::Generation(generation) => {
                let streaming = request.metadata.mode == TransportMode::Streaming;
                if generation.parameters.stream != streaming {
                    return Err(protocol_error(
                        "canonical stream flag does not match the selected transport mode",
                    ));
                }
                let mut wire = encode_request(generation, &request.attempt.upstream_model)
                    .map_err(|error| {
                        protocol_error(format!("cannot encode Anthropic messages request: {error}"))
                    })?;
                hydrate_anthropic_messages(
                    &mut wire.messages,
                    request.media.as_ref(),
                    request.max_inline_media_bytes,
                )
                .await?;
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
                let mut wire = encode_count_tokens(count, &request.attempt.upstream_model)?;
                hydrate_anthropic_messages(
                    &mut wire.messages,
                    request.media.as_ref(),
                    request.max_inline_media_bytes,
                )
                .await?;
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
                let events = decode(response).map_err(|error| {
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

    async fn map_error_response(
        &self,
        response: Response,
        attempt_deadline: Instant,
    ) -> TransportError {
        let status = response.status();
        let headers = response.headers().clone();
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
        upstream_response_error(TransportPhase::FirstByte, status, &headers, message)
    }
}

impl fmt::Debug for Connector {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("Connector")
            .field("config", &self.config)
            .field("api_key", &"[REDACTED]")
            .finish()
    }
}

impl ProviderTransport for Connector {
    fn execute<'a>(
        &'a self,
        request: ProviderRequest,
    ) -> crate::domain::ports::BoxFuture<'a, Result<ProviderOutput, TransportError>> {
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

pub(super) fn encode_count_tokens(
    request: &crate::domain::canonical::requests::TokenCountRequest,
    upstream_model: &str,
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
        wire.model = upstream_model.to_owned();
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
            ContentPart::Image {
                source,
                detail,
                mime_type,
            } => {
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
                        media_type: mime_type.clone(),
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
        model: upstream_model.to_owned(),
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
