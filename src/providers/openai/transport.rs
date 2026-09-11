use std::fmt;

use crate::inference::transport::DiscoveredProviderModel;
use crate::inference::transport::ProviderOutput;
use crate::inference::transport::ProviderRequest;
use crate::inference::transport::ProviderTransport;
use crate::inference::transport::TransportError;
use ::http::HeaderMap;
use ::http::HeaderValue;
use ::http::header;
use tokio::time::Instant;

use crate::providers::openai::ApiKey;
use crate::providers::openai::ConnectorConfig;

use crate::providers::openai::transport::errors::bearer_header;
use crate::providers::openai::transport::errors::map_endpoint_error;
use crate::providers::openai::transport::errors::map_send_error;
use crate::providers::openai::transport::errors::raw_api_key_header;
use crate::providers::openai::transport::streams::read_bounded_body;
use crate::providers::transport_common::protocol_body_error;

pub struct Connector {
    pub(crate) config: ConnectorConfig,
    api_key: ApiKey,
    auth_style: AuthStyle,
    pub(crate) custom_headers: Option<HeaderMap>,
    options: crate::providers::options::ConnectionOptions,
}

#[derive(Clone, Copy, Debug)]
enum AuthStyle {
    Bearer,
    ApiKeyHeader,
}

impl Connector {
    pub(super) fn dispatched_model<'a>(&'a self, model: &'a str) -> &'a str {
        self.options
            .models
            .get(model)
            .and_then(|metadata| metadata.deployment.as_deref())
            .unwrap_or(model)
    }

    pub(crate) fn with_options(
        mut self,
        options: crate::providers::options::ConnectionOptions,
        headers: Option<HeaderMap>,
    ) -> Self {
        self.options = options;
        self.custom_headers = headers;
        self
    }

    #[must_use]
    pub fn new(config: ConnectorConfig, api_key: ApiKey) -> Self {
        Self {
            config,
            api_key,
            custom_headers: None,
            options: Default::default(),
            auth_style: AuthStyle::Bearer,
        }
    }

    /// Builds an Azure-compatible transport using the raw `api-key` header.
    /// The endpoint retains the same DNS pinning, redirect, retry, and private
    /// address protections as the ordinary OpenAI connector.
    #[must_use]
    pub fn new_with_api_key_header(config: ConnectorConfig, api_key: ApiKey) -> Self {
        Self {
            config,
            api_key,
            custom_headers: None,
            options: Default::default(),
            auth_style: AuthStyle::ApiKeyHeader,
        }
    }

    /// Auth plus outbound trace context: every OpenAI-shaped request needs
    /// both, and assembling them in one place keeps a new endpoint from
    /// silently dropping trace continuity.
    fn base_headers(
        &self,
        request: &crate::inference::transport::ProviderRequest,
    ) -> Result<HeaderMap, TransportError> {
        let mut headers = HeaderMap::new();
        self.attach_auth(&mut headers)?;
        crate::providers::transport_common::inject_trace_context(
            &mut headers,
            request.propagate_trace_context,
        );
        Ok(headers)
    }

    fn attach_auth(&self, headers: &mut HeaderMap) -> Result<(), TransportError> {
        if let Some(custom) = &self.custom_headers {
            headers.extend(custom.clone());
            return Ok(());
        }
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
        let mut headers = HeaderMap::new();
        self.attach_auth(&mut headers)?;
        headers.insert(header::ACCEPT, HeaderValue::from_static("application/json"));
        let first_byte_deadline = Instant::now() + self.config.timeouts.first_byte;
        let response = errors::RESPONSE_IO
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
        streams::require_content_type(&response, "application/json")?;
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
    ) -> crate::inference::transport::BoxFuture<'a, Result<ProviderOutput, TransportError>> {
        Box::pin(async move {
            crate::providers::http_options::redact_output_errors(
                self.execute_request(request).await,
                self.custom_headers.is_some(),
            )
        })
    }
}

#[cfg(test)]
use crate::providers::openai::transport::errors::*;

pub mod errors;

pub mod media;

pub mod operations;

pub mod streams;

#[cfg(test)]
pub mod tests;
