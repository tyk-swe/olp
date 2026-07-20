//! Native Azure OpenAI connector.
//!
//! Azure's classic data-plane contract scopes inference paths beneath a named
//! deployment and requires an `api-key` header plus `api-version` query. The
//! canonical OpenAI transport supplies codecs, bounded media/streaming, DNS
//! pinning, no-proxy/no-redirect/no-retry policy, and phase deadlines; this
//! crate owns Azure-specific configuration and provider-kind validation.
//!
//! The canonical OpenAI `generation` capability currently gates both Chat
//! Completions and Responses. Consequently an OpenAI-surface Azure tuple is
//! certified only if the deployment/API-version pair proves both endpoints.
//! A chat-only deployment may still be certified for translated Anthropic or
//! Gemini generation, whose upstream transport is Chat Completions. This
//! conservative constraint avoids advertising Responses support that Azure
//! did not prove; removing it requires splitting the canonical capability.

use std::fmt;

use crate::openai::{
    CompatibleCapability, CompatibleCapabilityCertificationError,
    ConnectorConfig as OpenAiConnectorConfig, OpenAiApiKey, OpenAiConnector,
};
use olp_domain::{
    AttemptFailureClass, DiscoveredProviderModel, OperationKind, ProviderOutput, ProviderRequest,
    ProviderTransport, Surface, TransportError, TransportMode, TransportPhase,
};
use url::Url;
use zeroize::Zeroizing;

#[derive(Clone, Debug)]
pub struct ConnectorConfig {
    inner: OpenAiConnectorConfig,
    resource_endpoint: Url,
    deployment: String,
    api_version: String,
}

impl ConnectorConfig {
    pub fn new(
        resource_endpoint: &str,
        deployment: impl Into<String>,
        api_version: impl Into<String>,
    ) -> Result<Self, ConnectorBuildError> {
        let resource_endpoint = validate_resource_endpoint(resource_endpoint, false)?;
        let deployment = deployment.into();
        validate_deployment(&deployment)?;
        let api_version = api_version.into();
        validate_api_version(&api_version)?;
        let base_url = deployment_base_url(&resource_endpoint, &deployment)?;
        let inner = OpenAiConnectorConfig::with_base_url(base_url.as_str())?
            .with_api_version(&api_version)?;
        Ok(Self {
            inner,
            resource_endpoint,
            deployment,
            api_version,
        })
    }

    #[cfg(test)]
    fn for_local_test(
        resource_endpoint: &str,
        deployment: &str,
        api_version: &str,
        timeouts: crate::openai::ConnectorTimeouts,
    ) -> Self {
        let endpoint = validate_resource_endpoint(resource_endpoint, true).unwrap();
        let base_url = deployment_base_url(&endpoint, deployment).unwrap();
        Self {
            inner: OpenAiConnectorConfig::for_local_test(base_url.as_str(), timeouts)
                .with_api_version(api_version)
                .unwrap(),
            resource_endpoint: endpoint,
            deployment: deployment.to_owned(),
            api_version: api_version.to_owned(),
        }
    }
}

pub struct AzureOpenAiApiKey(Zeroizing<String>);

impl AzureOpenAiApiKey {
    pub fn new(value: impl Into<String>) -> Result<Self, ConnectorBuildError> {
        let value = value.into();
        if value.trim().is_empty() {
            return Err(ConnectorBuildError::EmptyApiKey);
        }
        if !value.bytes().all(|byte| byte.is_ascii_graphic()) {
            return Err(ConnectorBuildError::InvalidApiKey);
        }
        Ok(Self(Zeroizing::new(value)))
    }
}

impl fmt::Debug for AzureOpenAiApiKey {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str("AzureOpenAiApiKey([REDACTED])")
    }
}

pub struct AzureOpenAiConnector {
    resource_endpoint: Url,
    deployment: String,
    api_version: String,
    inner: OpenAiConnector,
}

impl AzureOpenAiConnector {
    #[must_use]
    pub fn new(config: ConnectorConfig, api_key: AzureOpenAiApiKey) -> Self {
        let inference_key = OpenAiApiKey::new(api_key.0.as_str().to_owned())
            .expect("Azure key validation is at least as strict as OpenAI key validation");
        let inner = OpenAiConnector::new_for_azure_openai(config.inner, inference_key);
        Self {
            resource_endpoint: config.resource_endpoint,
            deployment: config.deployment,
            api_version: config.api_version,
            inner,
        }
    }

    /// Proves the exact deployment path, API version, and credential with
    /// bounded content-minimal probes. Azure has no deployment-neutral data
    /// plane request that proves a deployment can serve inference, so chat is
    /// attempted first and embeddings second. A 404, invalid API version, or
    /// deployment that supports neither operation fails closed.
    pub async fn discover_models(&self) -> Result<Vec<DiscoveredProviderModel>, TransportError> {
        let chat = self
            .inner
            .certify_azure_openai_chat_completions_capability(
                &self.deployment,
                TransportMode::Unary,
            )
            .await;
        if let Err(chat_error) = chat {
            let embedding_capability = CompatibleCapability {
                operation: OperationKind::Embeddings,
                surface: Surface::OpenAi,
                mode: TransportMode::Unary,
            };
            if let Err(embedding_error) = self
                .inner
                .certify_azure_openai_capability(&self.deployment, embedding_capability)
                .await
            {
                return Err(deployment_probe_error(chat_error, embedding_error));
            }
        }
        Ok(vec![DiscoveredProviderModel {
            id: self.deployment.clone(),
            display_name: self.deployment.clone(),
        }])
    }

    /// Certifies only operation tuples proven through this deployment's exact
    /// inference path. OpenAI-surface generation proves both Chat Completions
    /// and Responses because one canonical capability gates both endpoints.
    /// Cross-origin generation uses Chat Completions, the translation path
    /// selected when no OpenAI endpoint hint exists. Media/job tuples are not
    /// certified until a safe content-minimal probe exists.
    pub(crate) async fn certify_deployment_capability(
        &self,
        provider_model: &str,
        capability: CompatibleCapability,
    ) -> Result<(), CompatibleCapabilityCertificationError> {
        if provider_model != self.deployment {
            return Err(CompatibleCapabilityCertificationError::InvalidResult);
        }
        if capability.operation == OperationKind::Generation
            && capability.surface != Surface::OpenAi
        {
            return self
                .inner
                .certify_azure_openai_chat_completions_capability(provider_model, capability.mode)
                .await;
        }
        self.inner
            .certify_azure_openai_capability(
                provider_model,
                CompatibleCapability {
                    surface: Surface::OpenAi,
                    ..capability
                },
            )
            .await
    }
}

fn deployment_probe_error(
    chat: CompatibleCapabilityCertificationError,
    embeddings: CompatibleCapabilityCertificationError,
) -> TransportError {
    let (phase, class) = match &embeddings {
        CompatibleCapabilityCertificationError::Transport { phase, class } => (*phase, *class),
        CompatibleCapabilityCertificationError::Unsupported
        | CompatibleCapabilityCertificationError::InvalidResult
        | CompatibleCapabilityCertificationError::ModelNotDiscovered => {
            (TransportPhase::Body, AttemptFailureClass::Protocol)
        }
    };
    TransportError {
        phase,
        class,
        response_committed: false,
        message: format!(
            "configured Azure deployment rejected bounded chat and embedding probes ({chat}; {embeddings})"
        ),
    }
}

impl fmt::Debug for AzureOpenAiConnector {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("AzureOpenAiConnector")
            .field("host", &self.resource_endpoint.host_str())
            .field("deployment", &self.deployment)
            .field("api_version", &self.api_version)
            .field("api_key", &"[REDACTED]")
            .finish()
    }
}

impl ProviderTransport for AzureOpenAiConnector {
    fn execute<'a>(
        &'a self,
        request: ProviderRequest,
    ) -> olp_domain::BoxFuture<'a, Result<ProviderOutput, TransportError>> {
        if request.attempt.provider_kind != olp_domain::ProviderKind::AzureOpenAi {
            return Box::pin(async {
                Err(TransportError {
                    phase: TransportPhase::Connect,
                    class: AttemptFailureClass::Protocol,
                    response_committed: false,
                    message: "Azure OpenAI connector received a different provider kind".into(),
                })
            });
        }
        self.inner.execute(request)
    }
}

fn validate_resource_endpoint(
    value: &str,
    allow_local_test: bool,
) -> Result<Url, ConnectorBuildError> {
    let mut endpoint = Url::parse(value).map_err(|_| ConnectorBuildError::InvalidEndpoint)?;
    if (!allow_local_test && endpoint.scheme() != "https")
        || (allow_local_test && !matches!(endpoint.scheme(), "http" | "https"))
        || endpoint.host().is_none()
        || !endpoint.username().is_empty()
        || endpoint.password().is_some()
        || endpoint.port() == Some(0)
        || endpoint.query().is_some()
        || endpoint.fragment().is_some()
        || !matches!(endpoint.path(), "" | "/")
    {
        return Err(ConnectorBuildError::InvalidEndpoint);
    }
    endpoint.set_path("/");
    Ok(endpoint)
}

fn validate_deployment(value: &str) -> Result<(), ConnectorBuildError> {
    if value.is_empty()
        || value.len() > 128
        || value.starts_with('.')
        || value.ends_with('.')
        || !value
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'-' | b'_' | b'.'))
    {
        return Err(ConnectorBuildError::InvalidDeployment);
    }
    Ok(())
}

fn validate_api_version(value: &str) -> Result<(), ConnectorBuildError> {
    let date = value.strip_suffix("-preview").unwrap_or(value);
    let bytes = date.as_bytes();
    let valid_shape = bytes.len() == 10
        && bytes[4] == b'-'
        && bytes[7] == b'-'
        && bytes
            .iter()
            .enumerate()
            .all(|(index, byte)| matches!(index, 4 | 7) || byte.is_ascii_digit());
    if !valid_shape {
        return Err(ConnectorBuildError::InvalidApiVersion);
    }
    let year = date[0..4]
        .parse::<u16>()
        .map_err(|_| ConnectorBuildError::InvalidApiVersion)?;
    let month = date[5..7]
        .parse::<u8>()
        .map_err(|_| ConnectorBuildError::InvalidApiVersion)?;
    let day = date[8..10]
        .parse::<u8>()
        .map_err(|_| ConnectorBuildError::InvalidApiVersion)?;
    if year < 2020 || !(1..=12).contains(&month) || !(1..=31).contains(&day) {
        return Err(ConnectorBuildError::InvalidApiVersion);
    }
    Ok(())
}

fn deployment_base_url(
    resource_endpoint: &Url,
    deployment: &str,
) -> Result<Url, ConnectorBuildError> {
    validate_deployment(deployment)?;
    let mut url = resource_endpoint.clone();
    {
        let mut path = url
            .path_segments_mut()
            .map_err(|()| ConnectorBuildError::InvalidEndpoint)?;
        path.pop_if_empty()
            .push("openai")
            .push("deployments")
            .push(deployment)
            .push("");
    }
    Ok(url)
}

#[derive(Debug, thiserror::Error)]
pub enum ConnectorBuildError {
    #[error(
        "Azure OpenAI resource endpoint must be an HTTPS origin without credentials, query, fragment, or path"
    )]
    InvalidEndpoint,
    #[error("Azure OpenAI deployment name is invalid")]
    InvalidDeployment,
    #[error("Azure OpenAI API key cannot be empty")]
    EmptyApiKey,
    #[error("Azure OpenAI API key must contain visible ASCII characters only")]
    InvalidApiKey,
    #[error("Azure OpenAI API version must be YYYY-MM-DD or YYYY-MM-DD-preview")]
    InvalidApiVersion,
    #[error(transparent)]
    OpenAi(#[from] crate::openai::ConnectorBuildError),
}

#[cfg(test)]
mod tests;
