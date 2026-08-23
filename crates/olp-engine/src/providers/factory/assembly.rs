use std::sync::Arc;

use crate::domain::ports::{DiscoveredProviderModel, ProviderTransport};

use crate::providers::anthropic::transport::operations::Connector as AnthropicConnector;
use crate::providers::azure_openai::Connector as AzureOpenAiConnector;
use crate::providers::bedrock::{
    Credentials, StaticCredentials as BedrockStaticCredentials,
    transport::Connector as BedrockConnector,
};
use crate::providers::connector::ApiKey;
use crate::providers::gemini::transport::operations::Connector as GeminiConnector;
use crate::providers::openai::transport::Connector as OpenAiConnector;
use crate::providers::vertex::Connector as VertexConnector;

#[cfg(any(test, feature = "test-util"))]
use super::configuration::validate_connector_configuration_with_policy;
use super::configuration::{
    BedrockAuthMode, BorrowedCredential, Config, ConnectorConfiguration, Credential,
    CredentialKind, Error, VertexAuthMode, bytes_credential, connector_configuration_with_policy,
    credential_kind, no_credential, text_credential, validate_connector_configuration,
    validate_provider_credential,
};

/// Single assembly entrypoint for runtime transport, discovery, probes, and
/// capability certification.
#[derive(Clone, Copy, Debug, Default)]
pub struct Factory;

impl Factory {
    pub fn validate(config: &Config) -> Result<(), Error> {
        validate_connector_configuration(config)
    }

    pub fn credential_kind(config: &Config) -> Result<CredentialKind, Error> {
        credential_kind(config)
    }

    pub fn validate_credential(config: &Config, credential: &Credential) -> Result<(), Error> {
        validate_provider_credential(config, credential)
    }

    pub async fn create(config: Config, credential: Credential) -> Result<Facade, Error> {
        Self::create_with_policy(config, credential, false).await
    }

    /// Test-build-only assembly that accepts plain-HTTP and non-public
    /// provider endpoints. Release binaries never compile these variants.
    #[cfg(any(test, feature = "test-util"))]
    pub async fn create_with_unsafe_test_endpoints(
        config: Config,
        credential: Credential,
    ) -> Result<Facade, Error> {
        Self::create_with_policy(config, credential, true).await
    }

    #[cfg(any(test, feature = "test-util"))]
    pub fn validate_with_unsafe_test_endpoints(config: &Config) -> Result<(), Error> {
        validate_connector_configuration_with_policy(config, true)
    }

    #[cfg(any(test, feature = "test-util"))]
    pub async fn transport_with_unsafe_test_endpoints(
        config: Config,
        credential: Credential,
    ) -> Result<Arc<dyn ProviderTransport>, Error> {
        Self::create_with_unsafe_test_endpoints(config, credential)
            .await
            .map(Facade::into_transport)
    }

    async fn create_with_policy(
        config: Config,
        credential: Credential,
        allow_unsafe_test_targets: bool,
    ) -> Result<Facade, Error> {
        let expected = Self::credential_kind(&config)?;
        let supplied = match &credential {
            Credential::None => CredentialKind::None,
            Credential::ApiKey(_) => CredentialKind::ApiKey,
            Credential::ServiceAccountJson(_) => CredentialKind::ServiceAccountJson,
            Credential::AwsStatic(_) => CredentialKind::AwsStatic,
        };
        if expected != supplied {
            return Err(Error::credential(
                "provider credential does not match its authentication mode",
            ));
        }
        let borrowed = match &credential {
            Credential::None => BorrowedCredential::None,
            Credential::ApiKey(value) | Credential::ServiceAccountJson(value) => {
                BorrowedCredential::Text(value.as_str())
            }
            Credential::AwsStatic(value) => BorrowedCredential::Bytes(value.as_slice()),
        };
        build_connector(&config, borrowed, allow_unsafe_test_targets).await
    }

    pub async fn transport(
        config: Config,
        credential: Credential,
    ) -> Result<Arc<dyn ProviderTransport>, Error> {
        Self::create(config, credential)
            .await
            .map(Facade::into_transport)
    }
}

pub struct Facade {
    pub(super) kind: crate::domain::routing::provider::ProviderKind,
    pub(super) connector: ConcreteConnector,
}

impl Facade {
    pub fn into_transport(self) -> Arc<dyn ProviderTransport> {
        self.connector.into_transport()
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
    pub(in crate::providers) fn from_local_vertex(connector: VertexConnector) -> Self {
        Self {
            kind: crate::domain::routing::provider::ProviderKind::VertexAi,
            connector: ConcreteConnector::Vertex(Arc::new(connector)),
        }
    }

    #[cfg(any(test, feature = "test-util"))]
    pub(in crate::providers) fn from_local_bedrock(connector: BedrockConnector) -> Self {
        Self {
            kind: crate::domain::routing::provider::ProviderKind::Bedrock,
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

impl ConcreteConnector {
    pub(super) fn as_transport(&self) -> &dyn ProviderTransport {
        match self {
            Self::OpenAi(connector) => connector.as_ref(),
            Self::Anthropic(connector) => connector.as_ref(),
            Self::Gemini(connector) => connector.as_ref(),
            Self::Vertex(connector) => connector.as_ref(),
            Self::Bedrock(connector) => connector.as_ref(),
            Self::AzureOpenAi(connector) => connector.as_ref(),
        }
    }

    fn into_transport(self) -> Arc<dyn ProviderTransport> {
        match self {
            Self::OpenAi(connector) => connector,
            Self::Anthropic(connector) => connector,
            Self::Gemini(connector) => connector,
            Self::Vertex(connector) => connector,
            Self::Bedrock(connector) => connector,
            Self::AzureOpenAi(connector) => connector,
        }
    }
}

async fn build_connector(
    config: &Config,
    credential: BorrowedCredential<'_>,
    allow_unsafe_test_targets: bool,
) -> Result<Facade, Error> {
    let kind = config.kind();
    let connector = match connector_configuration_with_policy(config, allow_unsafe_test_targets)? {
        ConnectorConfiguration::OpenAi(configuration) => {
            let key = ApiKey::new(
                text_credential(credential, "OpenAI provider credential is missing")?.to_owned(),
            )
            .map_err(Error::credential)?;
            ConcreteConnector::OpenAi(Arc::new(OpenAiConnector::new(configuration, key)))
        }
        ConnectorConfiguration::Anthropic(configuration) => {
            let key = ApiKey::new(
                text_credential(credential, "Anthropic provider credential is missing")?.to_owned(),
            )
            .map_err(Error::credential)?;
            ConcreteConnector::Anthropic(Arc::new(AnthropicConnector::new(configuration, key)))
        }
        ConnectorConfiguration::Gemini(configuration) => {
            let key = ApiKey::new(
                text_credential(credential, "Gemini provider credential is missing")?.to_owned(),
            )
            .map_err(Error::credential)?;
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
            .map_err(Error::credential)?;
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
                    Credentials::DefaultChain
                }
                BedrockAuthMode::Static => Credentials::Static(
                    BedrockStaticCredentials::from_json(bytes_credential(
                        credential,
                        "Bedrock static credential is missing",
                    )?)
                    .map_err(Error::credential)?,
                ),
            };
            ConcreteConnector::Bedrock(Arc::new(
                BedrockConnector::new(configuration, credentials).await,
            ))
        }
        ConnectorConfiguration::AzureOpenAi(configuration) => {
            let key = ApiKey::new(
                text_credential(credential, "Azure OpenAI credential is missing")?.to_owned(),
            )
            .map_err(Error::credential)?;
            ConcreteConnector::AzureOpenAi(Arc::new(AzureOpenAiConnector::new(*configuration, key)))
        }
    };
    Ok(Facade { kind, connector })
}
