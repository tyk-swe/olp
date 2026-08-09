use std::sync::Arc;

use olp_domain::{DiscoveredProviderModel, ProviderTransport};

use crate::anthropic::{AnthropicApiKey, AnthropicConnector};
use crate::azure_openai::{AzureOpenAiApiKey, AzureOpenAiConnector};
use crate::bedrock::{
    BedrockConnector, BedrockCredentials, StaticCredentials as BedrockStaticCredentials,
};
use crate::gemini::{GeminiApiKey, GeminiConnector};
use crate::openai::{OpenAiApiKey, OpenAiConnector};
use crate::vertex::VertexConnector;

#[cfg(any(test, feature = "test-util"))]
use super::configuration::validate_connector_configuration_with_policy;
use super::configuration::{
    BedrockAuthMode, BorrowedCredential, ConnectorConfiguration, CredentialKind, ProviderConfig,
    ProviderCredential, ProviderError, VertexAuthMode, bytes_credential,
    connector_configuration_with_policy, credential_kind, no_credential, text_credential,
    validate_connector_configuration, validate_provider_credential,
};

/// Single assembly entrypoint for runtime transport, discovery, probes, and
/// capability certification.
#[derive(Clone, Copy, Debug, Default)]
pub struct ProviderFactory;

impl ProviderFactory {
    pub fn validate(config: &ProviderConfig) -> Result<(), ProviderError> {
        validate_connector_configuration(config.spec())
    }

    pub fn credential_kind(config: &ProviderConfig) -> Result<CredentialKind, ProviderError> {
        credential_kind(config)
    }

    pub fn validate_credential(
        config: &ProviderConfig,
        credential: &ProviderCredential,
    ) -> Result<(), ProviderError> {
        validate_provider_credential(config, credential)
    }

    pub async fn create(
        config: ProviderConfig,
        credential: ProviderCredential,
    ) -> Result<ProviderFacade, ProviderError> {
        Self::create_with_policy(config, credential, false).await
    }

    /// Test-build-only assembly that accepts plain-HTTP and non-public
    /// provider endpoints. Release binaries never compile these variants.
    #[cfg(any(test, feature = "test-util"))]
    pub async fn create_with_unsafe_test_endpoints(
        config: ProviderConfig,
        credential: ProviderCredential,
    ) -> Result<ProviderFacade, ProviderError> {
        Self::create_with_policy(config, credential, true).await
    }

    #[cfg(any(test, feature = "test-util"))]
    pub fn validate_with_unsafe_test_endpoints(
        config: &ProviderConfig,
    ) -> Result<(), ProviderError> {
        validate_connector_configuration_with_policy(config.spec(), true)
    }

    #[cfg(any(test, feature = "test-util"))]
    pub async fn transport_with_unsafe_test_endpoints(
        config: ProviderConfig,
        credential: ProviderCredential,
    ) -> Result<Arc<dyn ProviderTransport>, ProviderError> {
        Self::create_with_unsafe_test_endpoints(config, credential)
            .await
            .map(ProviderFacade::into_transport)
    }

    async fn create_with_policy(
        config: ProviderConfig,
        credential: ProviderCredential,
        allow_unsafe_test_targets: bool,
    ) -> Result<ProviderFacade, ProviderError> {
        let expected = Self::credential_kind(&config)?;
        let supplied = match &credential {
            ProviderCredential::None => CredentialKind::None,
            ProviderCredential::ApiKey(_) => CredentialKind::ApiKey,
            ProviderCredential::ServiceAccountJson(_) => CredentialKind::ServiceAccountJson,
            ProviderCredential::AwsStatic(_) => CredentialKind::AwsStatic,
        };
        if expected != supplied {
            return Err(ProviderError::credential(
                "provider credential does not match its authentication mode",
            ));
        }
        let borrowed = match &credential {
            ProviderCredential::None => BorrowedCredential::None,
            ProviderCredential::ApiKey(value) | ProviderCredential::ServiceAccountJson(value) => {
                BorrowedCredential::Text(value.as_str())
            }
            ProviderCredential::AwsStatic(value) => BorrowedCredential::Bytes(value.as_slice()),
        };
        build_connector(config.spec(), borrowed, allow_unsafe_test_targets).await
    }

    pub async fn transport(
        config: ProviderConfig,
        credential: ProviderCredential,
    ) -> Result<Arc<dyn ProviderTransport>, ProviderError> {
        Self::create(config, credential)
            .await
            .map(ProviderFacade::into_transport)
    }
}

pub struct ProviderFacade {
    pub(super) kind: olp_domain::ProviderKind,
    pub(super) connector: ConcreteConnector,
}

impl ProviderFacade {
    pub fn into_transport(self) -> Arc<dyn ProviderTransport> {
        match self.connector {
            ConcreteConnector::OpenAi(connector) => connector,
            ConcreteConnector::Anthropic(connector) => connector,
            ConcreteConnector::Gemini(connector) => connector,
            ConcreteConnector::Vertex(connector) => connector,
            ConcreteConnector::Bedrock(connector) => connector,
            ConcreteConnector::AzureOpenAi(connector) => connector,
        }
    }

    pub async fn discover_models(&self) -> Result<Vec<DiscoveredProviderModel>, String> {
        let models = match &self.connector {
            ConcreteConnector::OpenAi(connector) => connector.discover_models().await,
            ConcreteConnector::Anthropic(connector) => connector.discover_models().await,
            ConcreteConnector::Gemini(connector) => connector.discover_models().await,
            ConcreteConnector::Vertex(connector) => connector.discover_models().await,
            ConcreteConnector::Bedrock(connector) => connector.discover_models().await,
            ConcreteConnector::AzureOpenAi(connector) => connector.discover_models().await,
        };
        models.map_err(|error| error.to_string())
    }

    #[cfg(any(test, feature = "test-util"))]
    pub(crate) fn from_local_vertex(connector: VertexConnector) -> Self {
        Self {
            kind: olp_domain::ProviderKind::VertexAi,
            connector: ConcreteConnector::Vertex(Arc::new(connector)),
        }
    }

    #[cfg(any(test, feature = "test-util"))]
    pub(crate) fn from_local_bedrock(connector: BedrockConnector) -> Self {
        Self {
            kind: olp_domain::ProviderKind::Bedrock,
            connector: ConcreteConnector::Bedrock(Arc::new(connector)),
        }
    }
}

pub(super) enum ConcreteConnector {
    OpenAi(Arc<OpenAiConnector>),
    Anthropic(Arc<AnthropicConnector>),
    Gemini(Arc<GeminiConnector>),
    Vertex(Arc<VertexConnector>),
    Bedrock(Arc<BedrockConnector>),
    AzureOpenAi(Arc<AzureOpenAiConnector>),
}

async fn build_connector(
    spec: super::configuration::ConnectorSpec<'_>,
    credential: BorrowedCredential<'_>,
    allow_unsafe_test_targets: bool,
) -> Result<ProviderFacade, ProviderError> {
    let kind = spec.kind;
    let connector = match connector_configuration_with_policy(spec, allow_unsafe_test_targets)? {
        ConnectorConfiguration::OpenAi(configuration) => {
            let key = OpenAiApiKey::new(
                text_credential(credential, "OpenAI provider credential is missing")?.to_owned(),
            )
            .map_err(ProviderError::credential)?;
            ConcreteConnector::OpenAi(Arc::new(OpenAiConnector::new(configuration, key)))
        }
        ConnectorConfiguration::Anthropic(configuration) => {
            let key = AnthropicApiKey::new(
                text_credential(credential, "Anthropic provider credential is missing")?.to_owned(),
            )
            .map_err(ProviderError::credential)?;
            ConcreteConnector::Anthropic(Arc::new(AnthropicConnector::new(configuration, key)))
        }
        ConnectorConfiguration::Gemini(configuration) => {
            let key = GeminiApiKey::new(
                text_credential(credential, "Gemini provider credential is missing")?.to_owned(),
            )
            .map_err(ProviderError::credential)?;
            ConcreteConnector::Gemini(Arc::new(GeminiConnector::new(configuration, key)))
        }
        ConnectorConfiguration::Vertex {
            configuration,
            auth_mode,
        } => {
            let connector = match auth_mode {
                VertexAuthMode::ApplicationDefault => {
                    no_credential(credential, "Vertex ADC providers do not accept credentials")?;
                    VertexConnector::with_application_default(configuration)
                }
                VertexAuthMode::ServiceAccount => VertexConnector::with_service_account_json(
                    configuration,
                    text_credential(
                        credential,
                        "Vertex AI service-account credential is missing",
                    )?,
                ),
            }
            .map_err(ProviderError::credential)?;
            ConcreteConnector::Vertex(Arc::new(connector))
        }
        ConnectorConfiguration::Bedrock {
            configuration,
            auth_mode,
        } => {
            let credentials = match auth_mode {
                BedrockAuthMode::DefaultChain => {
                    no_credential(
                        credential,
                        "Bedrock default-chain provider must not store static credentials",
                    )?;
                    BedrockCredentials::DefaultChain
                }
                BedrockAuthMode::Static => BedrockCredentials::Static(
                    BedrockStaticCredentials::from_json(bytes_credential(
                        credential,
                        "Bedrock static credential is missing",
                    )?)
                    .map_err(ProviderError::credential)?,
                ),
            };
            ConcreteConnector::Bedrock(Arc::new(
                BedrockConnector::new(configuration, credentials).await,
            ))
        }
        ConnectorConfiguration::AzureOpenAi(configuration) => {
            let key = AzureOpenAiApiKey::new(
                text_credential(credential, "Azure OpenAI credential is missing")?.to_owned(),
            )
            .map_err(ProviderError::credential)?;
            ConcreteConnector::AzureOpenAi(Arc::new(AzureOpenAiConnector::new(*configuration, key)))
        }
    };
    Ok(ProviderFacade { kind, connector })
}
