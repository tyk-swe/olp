//! Native Vertex AI connector.
//!
//! Vertex uses the Gemini canonical codecs, but its resource names and
//! authentication boundary are distinct: requests target a regional
//! `projects/.../locations/.../publishers/google/models/...` resource and use
//! short-lived OAuth access tokens. Provider requests retain the Gemini
//! connector's isolated, DNS-revalidated connection pool, redirect/retry/proxy
//! denial, response bounds, and phase-specific deadlines. Service-account
//! token exchange uses the same bounded-pool policy.

mod oauth;

use std::{fmt, sync::Arc};

use crate::gemini::{
    BearerTokenProvider, ConnectorConfig as GeminiConnectorConfig,
    ConnectorTimeouts as GeminiConnectorTimeouts, GeminiConnector,
};
use olp_domain::{
    DiscoveredProviderModel, ProviderOutput, ProviderRequest, ProviderTransport, TransportError,
};
use url::Url;

pub use oauth::ServiceAccountError;

const DEFAULT_SCOPE: &str = "https://www.googleapis.com/auth/cloud-platform";

#[derive(Clone, Debug)]
pub struct ConnectorConfig {
    inner: GeminiConnectorConfig,
    project: String,
    location: String,
    probe_model: String,
}

impl ConnectorConfig {
    pub fn new(
        project: impl Into<String>,
        location: impl Into<String>,
        probe_model: impl Into<String>,
    ) -> Result<Self, ConnectorBuildError> {
        let project = project.into();
        let location = location.into();
        let probe_model = normalize_model(probe_model.into())?;
        validate_path_identifier("project", &project)?;
        validate_path_identifier("location", &location)?;
        let base_url = regional_base_url(&project, &location)?;
        Ok(Self {
            inner: GeminiConnectorConfig::with_base_url(base_url.as_str())?,
            project,
            location,
            probe_model,
        })
    }

    pub fn with_timeouts(
        mut self,
        timeouts: GeminiConnectorTimeouts,
    ) -> Result<Self, ConnectorBuildError> {
        self.inner = self.inner.with_timeouts(timeouts)?;
        Ok(self)
    }

    pub fn with_response_limits(
        mut self,
        max_response_bytes: usize,
        max_event_bytes: usize,
    ) -> Result<Self, ConnectorBuildError> {
        self.inner = self
            .inner
            .with_response_limits(max_response_bytes, max_event_bytes)?;
        Ok(self)
    }

    #[cfg(test)]
    fn for_local_test(
        project: &str,
        location: &str,
        probe_model: &str,
        base_url: &str,
        timeouts: GeminiConnectorTimeouts,
    ) -> Self {
        Self {
            inner: GeminiConnectorConfig::for_local_test(base_url, timeouts),
            project: project.to_owned(),
            location: location.to_owned(),
            probe_model: probe_model.to_owned(),
        }
    }
}

pub struct VertexConnector {
    config: ConnectorConfig,
    inner: GeminiConnector,
}

impl VertexConnector {
    /// Uses Application Default Credentials, including attached workload
    /// identity, external-account federation, user ADC, and metadata identity.
    pub fn with_application_default(config: ConnectorConfig) -> Result<Self, ConnectorBuildError> {
        let credentials = google_cloud_auth::credentials::Builder::default()
            .with_scopes([DEFAULT_SCOPE])
            .build_access_token_credentials()
            .map_err(|_| ConnectorBuildError::ApplicationDefaultCredentials)?;
        let provider: Arc<dyn BearerTokenProvider> =
            Arc::new(oauth::ApplicationDefaultTokenProvider::new(credentials));
        Ok(Self::with_token_provider(config, provider))
    }

    /// Uses a versioned service-account JSON value decrypted by the runtime.
    /// The long-lived key stays inside this generation's connector object;
    /// only cached short-lived access tokens are used for requests.
    pub fn with_service_account_json(
        config: ConnectorConfig,
        credential_json: &str,
    ) -> Result<Self, ConnectorBuildError> {
        let provider: Arc<dyn BearerTokenProvider> = Arc::new(
            oauth::ServiceAccountTokenProvider::from_json(credential_json)
                .map_err(ConnectorBuildError::ServiceAccount)?,
        );
        Ok(Self::with_token_provider(config, provider))
    }

    #[must_use]
    pub fn with_token_provider(
        config: ConnectorConfig,
        provider: Arc<dyn BearerTokenProvider>,
    ) -> Self {
        let inner = GeminiConnector::with_bearer_token_provider(
            config.inner.clone(),
            olp_domain::ProviderKind::VertexAi,
            provider,
        );
        Self { config, inner }
    }

    /// Vertex publisher-model collections do not provide the Gemini Developer
    /// API's model-list contract. Probe the configured model with countTokens
    /// and return that explicit model as the discovered target.
    pub async fn discover_models(&self) -> Result<Vec<DiscoveredProviderModel>, TransportError> {
        self.inner.probe_model(&self.config.probe_model).await?;
        Ok(vec![DiscoveredProviderModel {
            id: self.config.probe_model.clone(),
            display_name: self.config.probe_model.clone(),
        }])
    }
}

impl fmt::Debug for VertexConnector {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("VertexConnector")
            .field("project", &self.config.project)
            .field("location", &self.config.location)
            .field("probe_model", &self.config.probe_model)
            .field("credential", &"[REDACTED]")
            .finish()
    }
}

impl ProviderTransport for VertexConnector {
    fn execute<'a>(
        &'a self,
        request: ProviderRequest,
    ) -> olp_domain::BoxFuture<'a, Result<ProviderOutput, TransportError>> {
        self.inner.execute(request)
    }
}

fn regional_base_url(project: &str, location: &str) -> Result<Url, ConnectorBuildError> {
    Url::parse(&format!(
        "https://{location}-aiplatform.googleapis.com/v1/projects/{project}/locations/{location}/publishers/google/"
    ))
    .map_err(|_| ConnectorBuildError::InvalidCloudContext)
}

fn validate_path_identifier(name: &'static str, value: &str) -> Result<(), ConnectorBuildError> {
    let allowed = |byte: u8| match name {
        "project" | "location" => {
            byte.is_ascii_lowercase() || byte.is_ascii_digit() || byte == b'-'
        }
        "model" => byte.is_ascii_alphanumeric() || matches!(byte, b'-' | b'.' | b'_'),
        _ => false,
    };
    if value.is_empty()
        || value.len() > 128
        || value.starts_with(['-', '.'])
        || value.ends_with(['-', '.'])
        || !value.bytes().all(allowed)
    {
        return Err(ConnectorBuildError::InvalidIdentifier(name));
    }
    Ok(())
}

fn normalize_model(model: String) -> Result<String, ConnectorBuildError> {
    let model = model.strip_prefix("models/").unwrap_or(&model).to_owned();
    validate_path_identifier("model", &model)?;
    Ok(model)
}

#[derive(Debug, thiserror::Error)]
pub enum ConnectorBuildError {
    #[error(transparent)]
    Gemini(#[from] crate::gemini::ConnectorBuildError),
    #[error("Vertex AI {0} is not a valid cloud resource identifier")]
    InvalidIdentifier(&'static str),
    #[error("Vertex AI cloud context could not be represented as a regional endpoint")]
    InvalidCloudContext,
    #[error("Application Default Credentials are unavailable or invalid")]
    ApplicationDefaultCredentials,
    #[error("stored Vertex AI service-account credential is invalid: {0}")]
    ServiceAccount(#[source] ServiceAccountError),
}

#[cfg(test)]
mod tests;
