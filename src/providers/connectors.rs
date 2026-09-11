use std::sync::Arc;

use crate::inference::transport::DiscoveredProviderModel;
use crate::inference::transport::ProviderTransport;

use crate::providers::anthropic::ApiKey as AnthropicApiKey;
use crate::providers::anthropic::transport::operations::Connector as AnthropicConnector;
use crate::providers::azure_openai::ApiKey as AzureOpenAiApiKey;
use crate::providers::azure_openai::Connector as AzureOpenAiConnector;
use crate::providers::bedrock::Credentials;
use crate::providers::bedrock::StaticCredentials as BedrockStaticCredentials;
use crate::providers::bedrock::transport::Connector as BedrockConnector;
use crate::providers::gemini::ApiKey as GeminiApiKey;
use crate::providers::gemini::transport::operations::Connector as GeminiConnector;
use crate::providers::openai::ApiKey as OpenAiApiKey;
use crate::providers::openai::transport::Connector as OpenAiConnector;
use crate::providers::vertex::Connector as VertexConnector;

use crate::net::egress::EgressPolicy;
use crate::providers::connector::ResponseLimits;

use crate::providers::configuration::ProviderConfiguration;
use crate::providers::connectors::configuration::BedrockAuthMode;
use crate::providers::connectors::configuration::BorrowedCredential;
use crate::providers::connectors::configuration::ConnectorConfiguration;
use crate::providers::connectors::configuration::Credential;
use crate::providers::connectors::configuration::CredentialKind;
use crate::providers::connectors::configuration::Error;
use crate::providers::connectors::configuration::VertexAuthMode;
use crate::providers::connectors::configuration::bytes_credential;
use crate::providers::connectors::configuration::connector_configuration_with_policy;
use crate::providers::connectors::configuration::credential_kind;
use crate::providers::connectors::configuration::no_credential;
use crate::providers::connectors::configuration::text_credential;

pub async fn create(
    config: ProviderConfiguration,
    credential: Credential,
    policy: &EgressPolicy,
    limits: ResponseLimits,
) -> Result<ProviderConnector, Error> {
    let expected = credential_kind(&config)?;
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
    build_connector(&config, borrowed, policy, limits).await
}

pub async fn transport(
    config: ProviderConfiguration,
    credential: Credential,
    policy: &EgressPolicy,
    limits: ResponseLimits,
) -> Result<Arc<dyn ProviderTransport>, Error> {
    create(config, credential, policy, limits)
        .await
        .map(ProviderConnector::into_transport)
}

impl ProviderConnector {
    pub async fn discover_models(&self) -> Result<Vec<DiscoveredProviderModel>, String> {
        let models = match self {
            ProviderConnector::OpenAi(connector)
            | ProviderConnector::OpenAiCompatible(connector) => connector.discover_models().await,
            ProviderConnector::Anthropic(connector) => connector.discover_models().await,
            ProviderConnector::Gemini(connector) => connector.discover_models().await,
            ProviderConnector::Vertex(connector) => connector.discover_models().await,
            ProviderConnector::Bedrock(connector) => connector.discover_models().await,
            ProviderConnector::AzureOpenAi(connector) => connector.discover_models().await,
        };
        models.map_err(|error| error.to_string())
    }

    #[cfg(any(test, feature = "test-util"))]
    pub(crate) fn from_local_vertex(connector: VertexConnector) -> Self {
        Self::Vertex(Arc::new(connector))
    }

    #[cfg(any(test, feature = "test-util"))]
    pub(crate) fn from_local_bedrock(connector: BedrockConnector) -> Self {
        Self::Bedrock(Arc::new(connector))
    }
}

pub enum ProviderConnector {
    OpenAi(Arc<OpenAiConnector>),
    OpenAiCompatible(Arc<OpenAiConnector>),
    Anthropic(Arc<AnthropicConnector>),
    Gemini(Arc<GeminiConnector>),
    Vertex(Arc<VertexConnector>),
    Bedrock(Arc<BedrockConnector>),
    AzureOpenAi(Arc<AzureOpenAiConnector>),
}

impl ProviderConnector {
    pub(crate) fn as_transport(&self) -> &dyn ProviderTransport {
        match self {
            Self::OpenAi(connector) | Self::OpenAiCompatible(connector) => connector.as_ref(),
            Self::Anthropic(connector) => connector.as_ref(),
            Self::Gemini(connector) => connector.as_ref(),
            Self::Vertex(connector) => connector.as_ref(),
            Self::Bedrock(connector) => connector.as_ref(),
            Self::AzureOpenAi(connector) => connector.as_ref(),
        }
    }

    pub fn into_transport(self) -> Arc<dyn ProviderTransport> {
        match self {
            Self::OpenAi(connector) | Self::OpenAiCompatible(connector) => connector,
            Self::Anthropic(connector) => connector,
            Self::Gemini(connector) => connector,
            Self::Vertex(connector) => connector,
            Self::Bedrock(connector) => connector,
            Self::AzureOpenAi(connector) => connector,
        }
    }
}

async fn build_connector(
    config: &ProviderConfiguration,
    credential: BorrowedCredential<'_>,
    policy: &EgressPolicy,
    limits: ResponseLimits,
) -> Result<ProviderConnector, Error> {
    let kind = config.kind;
    let connector = match connector_configuration_with_policy(config, policy, limits)? {
        ConnectorConfiguration::OpenAi(configuration) => {
            let key = OpenAiApiKey::new(
                crate::providers::http_options::key_text(config, credential)?.to_owned(),
            )
            .map_err(Error::credential)?;
            let connector = Arc::new(OpenAiConnector::new(configuration, key).with_options(
                config.options.clone(),
                crate::providers::http_options::headers(config, credential)?,
            ));
            if kind == crate::providers::runtime_model::ProviderKind::OpenAiCompatible {
                ProviderConnector::OpenAiCompatible(connector)
            } else {
                ProviderConnector::OpenAi(connector)
            }
        }
        ConnectorConfiguration::Anthropic(configuration) => {
            let key = AnthropicApiKey::new(
                crate::providers::http_options::key_text(config, credential)?.to_owned(),
            )
            .map_err(Error::credential)?;
            ProviderConnector::Anthropic(Arc::new(
                AnthropicConnector::new(configuration, key)
                    .with_options(config.options.clone())
                    .with_headers(crate::providers::http_options::headers(config, credential)?),
            ))
        }
        ConnectorConfiguration::Gemini(configuration) => {
            let key = GeminiApiKey::new(
                crate::providers::http_options::key_text(config, credential)?.to_owned(),
            )
            .map_err(Error::credential)?;
            ProviderConnector::Gemini(Arc::new(
                GeminiConnector::new(configuration, key)
                    .with_options(config.options.clone())
                    .with_headers(crate::providers::http_options::headers(config, credential)?),
            ))
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
            ProviderConnector::Vertex(Arc::new(connector))
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
            ProviderConnector::Bedrock(Arc::new(
                BedrockConnector::new(configuration, credentials).await,
            ))
        }
        ConnectorConfiguration::AzureOpenAi(configuration) => {
            let key = AzureOpenAiApiKey::new(
                text_credential(credential, "Azure OpenAI credential is missing")?.to_owned(),
            )
            .map_err(Error::credential)?;
            ProviderConnector::AzureOpenAi(Arc::new(
                AzureOpenAiConnector::configured(
                    *configuration,
                    key,
                    config.options.clone(),
                    policy,
                    limits,
                )
                .map_err(Error::configuration)?,
            ))
        }
    };
    Ok(connector)
}

pub mod certification;

pub mod configuration;

pub mod input;

#[cfg(any(test, feature = "test-util"))]
pub mod overrides;

#[cfg(test)]
pub mod tests;
