use crate::providers::configuration::ProviderConfiguration;
use std::fmt;

use crate::providers::runtime_model::ProviderKind;
use crate::providers::types::ProviderAuthMode;
use zeroize::Zeroizing;

use crate::net::egress::EgressPolicy;
use crate::providers::connector::ResponseLimits;

use crate::providers::anthropic::ApiKey as AnthropicApiKey;
use crate::providers::anthropic::ConnectorConfig as AnthropicConnectorConfig;
use crate::providers::azure_openai::ApiKey as AzureOpenAiApiKey;
use crate::providers::azure_openai::ConnectorConfig as AzureOpenAiConnectorConfig;
use crate::providers::bedrock::ConnectorConfig as BedrockConnectorConfig;
use crate::providers::bedrock::StaticCredentials as BedrockStaticCredentials;
use crate::providers::gemini::ApiKey as GeminiApiKey;
use crate::providers::gemini::ConnectorConfig as GeminiConnectorConfig;
use crate::providers::openai::ApiKey as OpenAiApiKey;
use crate::providers::openai::ConnectorConfig as OpenAiConnectorConfig;
use crate::providers::vertex::Connector as VertexConnector;
use crate::providers::vertex::ConnectorConfig as VertexConnectorConfig;

/// Secret material supplied by the caller after its own storage or file-I/O boundary.
#[derive(Clone, Copy)]
pub(crate) enum BorrowedCredential<'a> {
    None,
    Text(&'a str),
    Bytes(&'a [u8]),
}

impl fmt::Debug for BorrowedCredential<'_> {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::None => formatter.write_str("BorrowedCredential::None"),
            Self::Text(_) => formatter.write_str("BorrowedCredential::Text([REDACTED])"),
            Self::Bytes(_) => formatter.write_str("BorrowedCredential::Bytes([REDACTED])"),
        }
    }
}

/// Connector assembly failures are deliberately string-only so callers can map
/// them to their own HTTP or process error contracts without exposing secrets.
#[derive(Debug, thiserror::Error)]
pub enum Error {
    #[error("{0}")]
    Configuration(String),
    #[error("{0}")]
    Credential(String),
}

impl Error {
    pub(crate) fn configuration(error: impl ToString) -> Self {
        Self::Configuration(error.to_string())
    }

    pub(crate) fn credential(error: impl ToString) -> Self {
        Self::Credential(error.to_string())
    }
}

/// Secret material is named by semantics and zeroized on drop.
pub enum Credential {
    None,
    ApiKey(Zeroizing<String>),
    ServiceAccountJson(Zeroizing<String>),
    AwsStatic(Zeroizing<Vec<u8>>),
}

impl fmt::Debug for Credential {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::None => formatter.write_str("Credential::None"),
            Self::ApiKey(_) => formatter.write_str("Credential::ApiKey([REDACTED])"),
            Self::ServiceAccountJson(_) => {
                formatter.write_str("Credential::ServiceAccountJson([REDACTED])")
            }
            Self::AwsStatic(_) => formatter.write_str("Credential::AwsStatic([REDACTED])"),
        }
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum CredentialKind {
    None,
    ApiKey,
    ServiceAccountJson,
    AwsStatic,
}

pub fn credential_kind(config: &ProviderConfiguration) -> Result<CredentialKind, Error> {
    match (config.kind, config.auth_mode) {
        (ProviderKind::VertexAi, ProviderAuthMode::ApplicationDefault)
        | (ProviderKind::Bedrock, ProviderAuthMode::DefaultChain) => Ok(CredentialKind::None),
        (ProviderKind::VertexAi, ProviderAuthMode::ServiceAccount) => {
            Ok(CredentialKind::ServiceAccountJson)
        }
        (ProviderKind::Bedrock, ProviderAuthMode::Static) => Ok(CredentialKind::AwsStatic),
        (ProviderKind::VertexAi | ProviderKind::Bedrock, _) => Err(Error::configuration(
            "Provider authentication mode is invalid",
        )),
        (_, ProviderAuthMode::ApiKey) => Ok(CredentialKind::ApiKey),
        _ => Err(Error::configuration(
            "Provider authentication must be api_key",
        )),
    }
}

pub(crate) fn validate_provider_credential(
    config: &ProviderConfiguration,
    credential: &Credential,
) -> Result<(), Error> {
    let expected = credential_kind(config)?;
    let (supplied, borrowed) = match credential {
        Credential::None => (CredentialKind::None, BorrowedCredential::None),
        Credential::ApiKey(value) => (
            CredentialKind::ApiKey,
            BorrowedCredential::Text(value.as_str()),
        ),
        Credential::ServiceAccountJson(value) => (
            CredentialKind::ServiceAccountJson,
            BorrowedCredential::Text(value.as_str()),
        ),
        Credential::AwsStatic(value) => (
            CredentialKind::AwsStatic,
            BorrowedCredential::Bytes(value.as_slice()),
        ),
    };
    if expected != supplied {
        return Err(Error::credential(
            "provider credential does not match its authentication mode",
        ));
    }
    validate_connector_credential(config, borrowed)
}

/// Validates connector configuration without acquiring default credentials or
/// issuing network I/O.
pub(crate) fn validate_connector_configuration(
    config: &ProviderConfiguration,
    policy: &EgressPolicy,
) -> Result<(), Error> {
    connector_configuration_with_policy(config, policy, ResponseLimits::default()).map(|_| ())
}

/// Validates only a supplied credential. Callers retain their own encryption,
/// decryption, and response-field mapping boundaries.
pub(crate) fn validate_connector_credential(
    config: &ProviderConfiguration,
    credential: BorrowedCredential<'_>,
) -> Result<(), Error> {
    match config.kind {
        ProviderKind::OpenAi | ProviderKind::OpenAiCompatible => OpenAiApiKey::new(
            text_credential(credential, "OpenAI provider credential is missing")?.to_owned(),
        )
        .map(|_| ())
        .map_err(Error::credential),
        ProviderKind::Anthropic => AnthropicApiKey::new(
            text_credential(credential, "Anthropic provider credential is missing")?.to_owned(),
        )
        .map(|_| ())
        .map_err(Error::credential),
        ProviderKind::Gemini => GeminiApiKey::new(
            text_credential(credential, "Gemini provider credential is missing")?.to_owned(),
        )
        .map(|_| ())
        .map_err(Error::credential),
        ProviderKind::VertexAi if config.auth_mode == ProviderAuthMode::ServiceAccount => {
            let project = config
                .cloud_project
                .as_ref()
                .ok_or_else(|| Error::configuration("Provider cloud_project is required"))?;
            let location = config
                .cloud_region
                .as_ref()
                .ok_or_else(|| Error::configuration("Provider cloud_region is required"))?;
            let probe_model = config
                .probe_model
                .as_ref()
                .ok_or_else(|| Error::configuration("Provider probe_model is required"))?;
            VertexConnector::with_service_account_json(
                vertex_configuration(project, location, probe_model)?,
                text_credential(
                    credential,
                    "Vertex AI service-account credential is missing",
                )?,
            )
            .map(|_| ())
            .map_err(Error::credential)
        }
        ProviderKind::VertexAi => Err(Error::credential(
            "ADC providers do not accept stored credentials",
        )),
        ProviderKind::Bedrock if config.auth_mode == ProviderAuthMode::Static => {
            BedrockStaticCredentials::from_json(bytes_credential(
                credential,
                "Bedrock static credential is missing",
            )?)
            .map(|_| ())
            .map_err(Error::credential)
        }
        ProviderKind::Bedrock => Err(Error::credential(
            "default-chain providers do not accept stored credentials",
        )),
        ProviderKind::AzureOpenAi => AzureOpenAiApiKey::new(
            text_credential(credential, "Azure OpenAI credential is missing")?.to_owned(),
        )
        .map(|_| ())
        .map_err(Error::credential),
    }
}

pub(crate) enum ConnectorConfiguration {
    OpenAi(OpenAiConnectorConfig),
    Anthropic(AnthropicConnectorConfig),
    Gemini(GeminiConnectorConfig),
    Vertex {
        configuration: VertexConnectorConfig,
        auth_mode: VertexAuthMode,
    },
    Bedrock {
        configuration: BedrockConnectorConfig,
        auth_mode: BedrockAuthMode,
    },
    AzureOpenAi(Box<AzureOpenAiConnectorConfig>),
}

impl ConnectorConfiguration {
    #[cfg(test)]
    pub(crate) fn response_limits(&self) -> Option<ResponseLimits> {
        match self {
            Self::OpenAi(configuration) => Some(configuration.response_limits()),
            Self::Anthropic(configuration) => Some(configuration.response_limits()),
            Self::Gemini(configuration) => Some(configuration.response_limits()),
            Self::Vertex { configuration, .. } => Some(configuration.response_limits()),
            Self::AzureOpenAi(configuration) => Some(configuration.response_limits()),
            Self::Bedrock { .. } => None,
        }
    }
}

pub(crate) enum VertexAuthMode {
    ApplicationDefault,
    ServiceAccount,
}

pub(crate) enum BedrockAuthMode {
    DefaultChain,
    Static,
}

pub(crate) fn connector_configuration_with_policy(
    config: &ProviderConfiguration,
    policy: &EgressPolicy,
    limits: ResponseLimits,
) -> Result<ConnectorConfiguration, Error> {
    let (max_response_bytes, max_event_bytes) = (limits.max_response_bytes, limits.max_event_bytes);
    match config.kind {
        ProviderKind::OpenAi => {
            let endpoint = &config.endpoint;
            endpoint
                .as_deref()
                .map(|endpoint| OpenAiConnectorConfig::with_base_url_and_policy(endpoint, policy))
                .transpose()
                .and_then(|configuration| {
                    configuration
                        .unwrap_or_default()
                        .with_response_limits(max_response_bytes, max_event_bytes)
                })
                .map(ConnectorConfiguration::OpenAi)
                .map_err(Error::configuration)
        }
        ProviderKind::OpenAiCompatible => {
            let endpoint = config
                .endpoint
                .as_ref()
                .ok_or_else(|| Error::configuration("Provider endpoint is required"))?;
            OpenAiConnectorConfig::with_base_url_and_policy(endpoint, policy)
                .and_then(|configuration| {
                    configuration.with_response_limits(max_response_bytes, max_event_bytes)
                })
                .map(ConnectorConfiguration::OpenAi)
                .map_err(Error::configuration)
        }
        ProviderKind::Anthropic => anthropic_configuration(config, policy, limits),
        ProviderKind::Gemini => {
            let endpoint = &config.endpoint;
            endpoint
                .as_deref()
                .map(|endpoint| GeminiConnectorConfig::with_base_url_and_policy(endpoint, policy))
                .transpose()
                .and_then(|configuration| {
                    configuration
                        .unwrap_or_default()
                        .with_response_limits(max_response_bytes, max_event_bytes)
                })
                .map(ConnectorConfiguration::Gemini)
                .map_err(Error::configuration)
        }
        ProviderKind::VertexAi => {
            let project = config
                .cloud_project
                .as_ref()
                .ok_or_else(|| Error::configuration("Provider cloud_project is required"))?;
            let location = config
                .cloud_region
                .as_ref()
                .ok_or_else(|| Error::configuration("Provider cloud_region is required"))?;
            let probe_model = config
                .probe_model
                .as_ref()
                .ok_or_else(|| Error::configuration("Provider probe_model is required"))?;
            let auth_mode = &config.auth_mode;
            vertex_connector_configuration(project, location, probe_model, *auth_mode, limits)
        }
        ProviderKind::Bedrock => {
            let region = config
                .cloud_region
                .as_ref()
                .ok_or_else(|| Error::configuration("Provider cloud_region is required"))?;
            let auth_mode = &config.auth_mode;
            bedrock_connector_configuration(region, *auth_mode)
        }
        ProviderKind::AzureOpenAi => {
            let endpoint = config
                .endpoint
                .as_ref()
                .ok_or_else(|| Error::configuration("Provider endpoint is required"))?;
            let api_version = config
                .api_version
                .as_ref()
                .ok_or_else(|| Error::configuration("Provider api_version is required"))?;
            let deployment = config
                .deployment
                .as_ref()
                .ok_or_else(|| Error::configuration("Provider deployment is required"))?;
            Ok(ConnectorConfiguration::AzureOpenAi(Box::new(
                AzureOpenAiConnectorConfig::new_with_policy(
                    endpoint,
                    deployment.clone(),
                    api_version.clone(),
                    policy,
                )
                .and_then(|configuration| configuration.with_response_limits(limits))
                .map_err(Error::configuration)?,
            )))
        }
    }
}

fn anthropic_configuration(
    config: &ProviderConfiguration,
    policy: &EgressPolicy,
    limits: ResponseLimits,
) -> Result<ConnectorConfiguration, Error> {
    let mut configuration = config
        .endpoint
        .as_deref()
        .map(|endpoint| AnthropicConnectorConfig::with_base_url_and_policy(endpoint, policy))
        .transpose()
        .map_err(Error::configuration)?
        .unwrap_or_default();
    if let Some(version) = &config.api_version {
        configuration = configuration
            .with_api_version(version.clone())
            .map_err(Error::configuration)?;
    }
    configuration
        .with_response_limits(limits.max_response_bytes, limits.max_event_bytes)
        .map(ConnectorConfiguration::Anthropic)
        .map_err(Error::configuration)
}

fn vertex_configuration(
    project: &str,
    location: &str,
    probe_model: &str,
) -> Result<VertexConnectorConfig, Error> {
    VertexConnectorConfig::new(project, location, probe_model).map_err(Error::configuration)
}

fn vertex_connector_configuration(
    project: &str,
    location: &str,
    probe_model: &str,
    auth_mode: ProviderAuthMode,
    limits: ResponseLimits,
) -> Result<ConnectorConfiguration, Error> {
    let configuration = vertex_configuration(project, location, probe_model)?
        .with_response_limits(limits)
        .map_err(Error::configuration)?;
    let auth_mode = match auth_mode {
        ProviderAuthMode::ApplicationDefault => VertexAuthMode::ApplicationDefault,
        ProviderAuthMode::ServiceAccount => VertexAuthMode::ServiceAccount,
        mode => {
            return Err(Error::configuration(format!(
                "Unsupported Vertex AI authentication mode {mode}"
            )));
        }
    };
    Ok(ConnectorConfiguration::Vertex {
        configuration,
        auth_mode,
    })
}

/// Bedrock speaks the AWS SDK, which carries no response byte cap, so the
/// response limits do not apply here.
fn bedrock_connector_configuration(
    region: &str,
    auth_mode: ProviderAuthMode,
) -> Result<ConnectorConfiguration, Error> {
    let configuration = BedrockConnectorConfig::new(region).map_err(Error::configuration)?;
    let auth_mode = match auth_mode {
        ProviderAuthMode::DefaultChain => BedrockAuthMode::DefaultChain,
        ProviderAuthMode::Static => BedrockAuthMode::Static,
        mode => {
            return Err(Error::configuration(format!(
                "Unsupported Bedrock authentication mode {mode}"
            )));
        }
    };
    Ok(ConnectorConfiguration::Bedrock {
        configuration,
        auth_mode,
    })
}

pub(crate) fn text_credential<'a>(
    credential: BorrowedCredential<'a>,
    missing: &'static str,
) -> Result<&'a str, Error> {
    match credential {
        BorrowedCredential::Text(value) => Ok(value),
        BorrowedCredential::Bytes(_) => {
            Err(Error::credential("provider credential is not valid UTF-8"))
        }
        BorrowedCredential::None => Err(Error::credential(missing)),
    }
}

pub(crate) fn bytes_credential<'a>(
    credential: BorrowedCredential<'a>,
    missing: &'static str,
) -> Result<&'a [u8], Error> {
    match credential {
        BorrowedCredential::Text(value) => Ok(value.as_bytes()),
        BorrowedCredential::Bytes(value) => Ok(value),
        BorrowedCredential::None => Err(Error::credential(missing)),
    }
}

pub(crate) fn no_credential(
    credential: BorrowedCredential<'_>,
    message: &'static str,
) -> Result<(), Error> {
    matches!(credential, BorrowedCredential::None)
        .then_some(())
        .ok_or_else(|| Error::credential(message))
}
