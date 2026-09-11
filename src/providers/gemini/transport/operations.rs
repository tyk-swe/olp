use std::collections::BTreeMap;
use std::fmt;

use crate::inference::transport::AttemptFailureClass;
use crate::inference::transport::DiscoveredProviderModel;
use crate::inference::transport::ProviderOutput;
use crate::inference::transport::ProviderRequest;
use crate::inference::transport::ProviderTransport;
use crate::inference::transport::TransportError;
use crate::inference::transport::TransportPhase;
use crate::protocols::canonical::identity::Surface;
use crate::protocols::canonical::identity::TransportMode;
use crate::protocols::canonical::requests::ContentPart;
use crate::protocols::canonical::requests::MediaSource;
use crate::protocols::canonical::requests::Operation;
use crate::protocols::canonical::results::CanonicalResult;
use crate::protocols::canonical::results::TokenCountResult;
use crate::protocols::gemini::count::GEMINI_COUNT_REQUEST_EXTENSION;
use crate::protocols::gemini::dto::Content;
use crate::protocols::gemini::dto::CountTokensRequest;
use crate::protocols::gemini::dto::CountTokensResponse;
use crate::protocols::gemini::dto::FileData;
use crate::protocols::gemini::dto::FileDataPart;
use crate::protocols::gemini::dto::GenerateContentResponse;
use crate::protocols::gemini::dto::Part;
use crate::protocols::gemini::dto::TextPart;
use crate::protocols::gemini::translate::encode::request as encode_request;
use crate::protocols::gemini::translate::response::decode;
use crate::protocols::gemini::translate::validation::validate_count_tokens_request;
use crate::providers::runtime_model::ProviderKind;
use crate::providers::transport_common::insert_json_request_headers;
use ::http::HeaderMap;
use ::http::HeaderValue;
use ::http::StatusCode;
use ::http::header;
use futures::stream;
use reqwest::Response;
use reqwest::Url;
use tokio::time::Instant;
use tokio::time::timeout;

use crate::providers::gemini::ApiKey;
use crate::providers::gemini::BearerTokenProvider;
use crate::providers::gemini::ConnectorConfig;
use crate::providers::gemini::ConnectorCredential;
use crate::providers::gemini::transport::errors::*;
use crate::providers::gemini::transport::media::hydrate_gemini_contents;
use crate::providers::transport_common::protocol_body_error;
use crate::providers::transport_common::protocol_error;
use crate::providers::transport_common::source_extensions;
use crate::providers::transport_common::transport_error;
use crate::providers::transport_common::upstream_response_error;
use crate::providers::transport_io::bounded_duration;

/// Validates the concrete canonical request with the production Gemini
/// encoders before routing. This is especially important for cross-origin
/// token-count requests, whose source-scoped exact bodies cannot be translated
/// to another vendor protocol.
pub fn validate_operation(
    operation: &Operation,
    upstream_model: &str,
) -> Result<(), TransportError> {
    match operation {
        Operation::Generation(generation) => encode_request(generation)
            .map(|_| ())
            .map_err(|error| protocol_error(error.to_string())),
        Operation::TokenCount(count) => encode_count_tokens(count, upstream_model).map(|_| ()),
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

pub struct Connector {
    options: crate::providers::options::ConnectionOptions,
    custom_headers: Option<HeaderMap>,
    pub(crate) config: ConnectorConfig,
    pub(crate) credential: ConnectorCredential,
    pub(crate) provider_kind: ProviderKind,
}

impl Connector {
    pub(crate) fn with_options(
        mut self,
        options: crate::providers::options::ConnectionOptions,
    ) -> Self {
        self.options = options;
        self
    }

    pub(crate) fn with_headers(mut self, headers: Option<HeaderMap>) -> Self {
        self.custom_headers = headers;
        self
    }

    #[must_use]
    pub fn new(config: ConnectorConfig, api_key: ApiKey) -> Self {
        Self {
            custom_headers: None,
            options: Default::default(),
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
            custom_headers: None,
            options: Default::default(),
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
            let mut headers = HeaderMap::new();
            self.insert_authentication_header(&mut headers).await?;
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
    pub async fn probe_model(&self, upstream_model: &str) -> Result<(), TransportError> {
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
            .count_tokens_url(upstream_model)
            .map_err(map_endpoint_error)?;
        let body = br#"{"contents":[{"role":"user","parts":[{"text":"health"}]}]}"#;
        let mut headers = HeaderMap::new();
        self.insert_authentication_header(&mut headers).await?;
        headers.insert(
            header::CONTENT_TYPE,
            HeaderValue::from_static("application/json"),
        );
        headers.insert(header::ACCEPT, HeaderValue::from_static("application/json"));
        let first_byte_deadline = Instant::now() + self.config.timeouts.first_byte;
        let response = RESPONSE_IO
            .send_before(
                client.post(url).headers(headers).body(body.as_slice()),
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
        mut request: ProviderRequest,
    ) -> Result<ProviderOutput, TransportError> {
        validate_request_envelope(&request, self.provider_kind)?;
        if let Some(deployment) = self
            .options
            .models
            .get(&request.attempt.upstream_model)
            .and_then(|m| m.deployment.as_ref())
        {
            request.attempt.upstream_model = deployment.clone();
        }
        let (url, body, response_kind, streaming) = self.encode_request(&request).await?;
        let body = crate::providers::http_options::request_body(
            &self.options,
            if request.metadata.operation
                == crate::protocols::canonical::identity::OperationKind::Generation
            {
                "generation"
            } else {
                "token_count"
            },
            body,
        )?;
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

        let mut headers = HeaderMap::new();
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
        insert_json_request_headers(&mut headers, &request, streaming)?;

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
        match &*request.operation {
            Operation::Generation(generation) => {
                let streaming = request.metadata.mode == TransportMode::Streaming;
                if generation.parameters.stream != streaming {
                    return Err(protocol_error(
                        "canonical stream flag does not match the selected transport mode",
                    ));
                }
                let mut wire = encode_request(generation).map_err(|error| {
                    protocol_error(format!("cannot encode Gemini generation request: {error}"))
                })?;
                hydrate_gemini_contents(
                    &mut wire.contents,
                    request.media.as_ref(),
                    request.max_inline_media_bytes,
                )
                .await?;
                let body = serde_json::to_vec(&wire).map_err(|error| {
                    protocol_error(format!("cannot serialize Gemini request: {error}"))
                })?;
                Ok((
                    self.config
                        .endpoint
                        .generate_url(&request.attempt.upstream_model, streaming)
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
                let mut wire = encode_count_tokens(count, &request.attempt.upstream_model)?;
                hydrate_gemini_contents(
                    &mut wire.contents,
                    request.media.as_ref(),
                    request.max_inline_media_bytes,
                )
                .await?;
                if let Some(generation) = &mut wire.generate_content_request {
                    hydrate_gemini_contents(
                        &mut generation.contents,
                        request.media.as_ref(),
                        request.max_inline_media_bytes,
                    )
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
                        .count_tokens_url(&request.attempt.upstream_model)
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
                let events = decode(response).map_err(|error| {
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

    async fn map_error_response(
        &self,
        response: Response,
        attempt_deadline: Instant,
    ) -> TransportError {
        let status = response.status();
        let mut mapped_status = status;
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
            Ok(body) => {
                if matches!(status, StatusCode::BAD_REQUEST | StatusCode::FORBIDDEN)
                    && serde_json::from_slice::<serde_json::Value>(&body).is_ok_and(|value| {
                        value
                            .pointer("/error/details")
                            .and_then(serde_json::Value::as_array)
                            .is_some_and(|details| {
                                details.iter().any(|detail| {
                                    detail["@type"] == "type.googleapis.com/google.rpc.ErrorInfo"
                                        && detail["reason"] == "API_KEY_INVALID"
                                })
                            })
                    })
                {
                    mapped_status = StatusCode::UNAUTHORIZED;
                }
                self.safe_upstream_error_message(status, &body)
            }
            Err(_) => format!("Gemini returned HTTP {status}"),
        };
        upstream_response_error(TransportPhase::FirstByte, mapped_status, &headers, message)
    }

    async fn insert_authentication_header(
        &self,
        headers: &mut HeaderMap,
    ) -> Result<(), TransportError> {
        if let Some(custom) = &self.custom_headers {
            headers.extend(custom.clone());
            return Ok(());
        }
        match &self.credential {
            ConnectorCredential::ApiKey(api_key) => {
                headers.insert("x-goog-api-key", secret_header(api_key)?);
            }
            ConnectorCredential::Bearer(provider) => {
                let token = provider.token().await.map_err(|error| match error {
                    crate::providers::gemini::BearerTokenError::Authentication => {
                        // Token rejection precedes inference dispatch. Normalize
                        // it to the credential failure signal used by slot failover.
                        upstream_response_error(
                            TransportPhase::Connect,
                            StatusCode::UNAUTHORIZED,
                            &HeaderMap::new(),
                            "Google OAuth credential was rejected",
                        )
                    }
                    crate::providers::gemini::BearerTokenError::Unavailable => transport_error(
                        TransportPhase::Connect,
                        AttemptFailureClass::Connect,
                        false,
                        "Google OAuth bearer token acquisition failed",
                    ),
                })?;
                headers.insert(header::AUTHORIZATION, bearer_header(&token)?);
            }
        }
        Ok(())
    }

    fn safe_upstream_error_message(&self, status: StatusCode, body: &[u8]) -> String {
        if self.custom_headers.is_some() {
            return format!("Google provider returned HTTP {status}");
        }
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

impl fmt::Debug for Connector {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("Connector")
            .field("config", &self.config)
            .field("credential", &"[REDACTED]")
            .field("provider_kind", &self.provider_kind)
            .finish()
    }
}

impl ProviderTransport for Connector {
    fn execute<'a>(
        &'a self,
        request: ProviderRequest,
    ) -> crate::inference::transport::BoxFuture<'a, Result<ProviderOutput, TransportError>> {
        Box::pin(async move {
            crate::providers::http_options::redact_output_errors(
                self.execute_request(request).await,
                self.custom_headers.is_some(),
            )
        })
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

fn restore_preserved_count_request(
    value: serde_json::Value,
    upstream_model: &str,
) -> Result<CountTokensRequest, TransportError> {
    let mut wire: CountTokensRequest = serde_json::from_value(value).map_err(|error| {
        protocol_error(format!(
            "preserved Gemini countTokens request is invalid: {error}"
        ))
    })?;
    if let Some(generation) = &mut wire.generate_content_request {
        let model = upstream_model
            .strip_prefix("models/")
            .unwrap_or(upstream_model);
        generation.model = Some(format!("models/{model}"));
    }
    validate_count_tokens_request(&wire).map_err(|error| {
        protocol_error(format!(
            "preserved Gemini countTokens request is invalid: {error}"
        ))
    })?;
    Ok(wire)
}

fn encode_count_file_part(
    source: &MediaSource,
    mime_type: Option<&str>,
    part_index: usize,
    remaining_extensions: &mut BTreeMap<String, serde_json::Value>,
) -> Result<Part, TransportError> {
    let MediaSource::Uri(file_uri) = source else {
        return Err(protocol_error(
            "Gemini token counting cannot encode media handles",
        ));
    };
    let mime_path = format!("/contents/0/parts/{part_index}/fileData/mimeType");
    let mime_type = mime_type
        .map(str::to_owned)
        .or_else(|| {
            remaining_extensions
                .remove(&mime_path)
                .and_then(|value| value.as_str().map(str::to_owned))
        })
        .ok_or_else(|| {
            protocol_error(format!(
                "Gemini image token counting requires a MIME type extension at {mime_path}"
            ))
        })?;
    Ok(Part::FileData(FileDataPart {
        file_data: FileData {
            mime_type,
            file_uri: file_uri.clone(),
            extra: BTreeMap::new(),
        },
        extra: BTreeMap::new(),
    }))
}

pub(crate) fn encode_count_tokens(
    request: &crate::protocols::canonical::requests::TokenCountRequest,
    upstream_model: &str,
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
        return restore_preserved_count_request(value, upstream_model);
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
            ContentPart::Image {
                source,
                detail,
                mime_type,
            } => {
                if detail.is_some() {
                    return Err(protocol_error(
                        "Gemini token counting cannot represent image detail",
                    ));
                }
                parts.push(encode_count_file_part(
                    source,
                    mime_type.as_deref(),
                    parts.len(),
                    &mut remaining_extensions,
                )?);
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
