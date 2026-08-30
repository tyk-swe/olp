//! Native Vertex AI connector.
//!
//! Vertex uses the Gemini canonical codecs, but its resource names and
//! authentication boundary are distinct: requests target a regional or multi-region
//! `projects/.../locations/.../publishers/google/models/...` resource and use
//! short-lived OAuth access tokens. Provider requests retain the Gemini
//! connector's isolated, DNS-revalidated connection pool, redirect/retry/proxy
//! denial, response bounds, and phase-specific deadlines. Service-account
//! token exchange uses the same bounded-pool policy.

mod oauth;

use std::{fmt, sync::Arc};

use crate::domain::ports::{
    DiscoveredProviderModel, ProviderOutput, ProviderRequest, ProviderTransport, TransportError,
};
use crate::providers::gemini::{
    BearerTokenProvider, ConnectorConfig as GeminiConnectorConfig,
    transport::operations::Connector as GeminiConnector,
};
use url::Url;

use oauth::ServiceAccountError;

const DEFAULT_SCOPE: &str = "https://www.googleapis.com/auth/cloud-platform";

#[derive(Clone, Debug)]
pub(in crate::providers) struct ConnectorConfig {
    inner: GeminiConnectorConfig,
    project: String,
    location: String,
    probe_model: String,
}

impl ConnectorConfig {
    pub(in crate::providers) fn new(
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

    pub(in crate::providers) fn with_response_limits(
        mut self,
        limits: crate::providers::connector::ResponseLimits,
    ) -> Result<Self, ConnectorBuildError> {
        self.inner = self
            .inner
            .with_response_limits(limits.max_response_bytes, limits.max_event_bytes)?;
        Ok(self)
    }

    #[cfg(test)]
    pub(in crate::providers) fn response_limits(
        &self,
    ) -> crate::providers::connector::ResponseLimits {
        self.inner.response_limits()
    }

    #[cfg(any(test, feature = "test-util"))]
    pub(in crate::providers) fn for_local_test(
        project: &str,
        location: &str,
        probe_model: &str,
        base_url: &str,
        timeouts: crate::providers::connector::Timeouts,
    ) -> Self {
        Self {
            inner: GeminiConnectorConfig::for_local_test(base_url, timeouts),
            project: project.to_owned(),
            location: location.to_owned(),
            probe_model: probe_model.to_owned(),
        }
    }
}

pub(in crate::providers) struct Connector {
    config: ConnectorConfig,
    inner: GeminiConnector,
}

impl Connector {
    /// Uses Application Default Credentials, including attached workload
    /// identity, external-account federation, user ADC, and metadata identity.
    pub(in crate::providers) fn with_application_default(
        config: ConnectorConfig,
    ) -> Result<Self, ConnectorBuildError> {
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
    pub(in crate::providers) fn with_service_account_json(
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
    pub(in crate::providers) fn with_token_provider(
        config: ConnectorConfig,
        provider: Arc<dyn BearerTokenProvider>,
    ) -> Self {
        let inner = GeminiConnector::with_bearer_token_provider(
            config.inner.clone(),
            crate::domain::routing::provider::ProviderKind::VertexAi,
            provider,
        );
        Self { config, inner }
    }

    /// Vertex publisher-model collections do not provide the Gemini Developer
    /// API's model-list contract. Probe the configured model with countTokens
    /// and return that explicit model as the discovered target.
    pub(in crate::providers) async fn discover_models(
        &self,
    ) -> Result<Vec<DiscoveredProviderModel>, TransportError> {
        self.inner.probe_model(&self.config.probe_model).await?;
        Ok(vec![DiscoveredProviderModel {
            id: self.config.probe_model.clone(),
            display_name: self.config.probe_model.clone(),
        }])
    }
}

impl fmt::Debug for Connector {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("Connector")
            .field("project", &self.config.project)
            .field("location", &self.config.location)
            .field("probe_model", &self.config.probe_model)
            .field("credential", &"[REDACTED]")
            .finish()
    }
}

impl ProviderTransport for Connector {
    fn execute<'a>(
        &'a self,
        request: ProviderRequest,
    ) -> crate::domain::ports::BoxFuture<'a, Result<ProviderOutput, TransportError>> {
        self.inner.execute(request)
    }
}

fn regional_base_url(project: &str, location: &str) -> Result<Url, ConnectorBuildError> {
    let host = match location {
        "global" => "aiplatform.googleapis.com".to_owned(),
        "us" | "eu" => format!("aiplatform.{location}.rep.googleapis.com"),
        _ => format!("{location}-aiplatform.googleapis.com"),
    };
    Url::parse(&format!(
        "https://{host}/v1/projects/{project}/locations/{location}/publishers/google/"
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
pub(in crate::providers) enum ConnectorBuildError {
    #[error(transparent)]
    Gemini(#[from] crate::providers::gemini::ConnectorBuildError),
    #[error("Vertex AI {0} is not a valid cloud resource identifier")]
    InvalidIdentifier(&'static str),
    #[error("Vertex AI cloud context could not be represented as an API endpoint")]
    InvalidCloudContext,
    #[error("Application Default Credentials are unavailable or invalid")]
    ApplicationDefaultCredentials,
    #[error("stored Vertex AI service-account credential is invalid: {0}")]
    ServiceAccount(#[source] ServiceAccountError),
}

#[cfg(test)]
mod tests;
