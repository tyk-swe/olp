use std::fmt;

use http::{HeaderMap, HeaderValue, header};
use olp_domain::{
    DiscoveredProviderModel, ProviderOutput, ProviderRequest, ProviderTransport, TransportError,
};
use tokio::time::{Instant, timeout};

use crate::openai::{ConnectorConfig, OpenAiApiKey, headers::sanitize_forward_headers};

mod errors;
mod media;
mod operations;
mod streams;

use errors::{
    bearer_header, first_byte_timeout, map_endpoint_error, map_send_error, protocol_body_error,
    raw_api_key_header,
};
use streams::read_bounded_body;

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
        operations::require_content_type(&response, "application/json")?;
        let body = read_bounded_body(
            response,
            first_byte_deadline,
            attempt_deadline,
            self.config.timeouts.idle,
            self.config.response_limits.response_bytes,
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

#[cfg(test)]
use self::{errors::*, media::*, operations::*};

#[cfg(test)]
mod tests;
