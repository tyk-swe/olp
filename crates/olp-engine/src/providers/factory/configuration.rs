use std::fmt;

use crate::domain::{provider::ProviderAuthMode, routing::provider::ProviderKind};
use zeroize::Zeroizing;

use crate::providers::anthropic::ConnectorConfig as AnthropicConnectorConfig;
use crate::providers::azure_openai::ConnectorConfig as AzureOpenAiConnectorConfig;
use crate::providers::bedrock::{
    ConnectorConfig as BedrockConnectorConfig, StaticCredentials as BedrockStaticCredentials,
};
use crate::providers::connector::ApiKey;
use crate::providers::gemini::ConnectorConfig as GeminiConnectorConfig;
use crate::providers::openai::ConnectorConfig as OpenAiConnectorConfig;
use crate::providers::vertex::{
    Connector as VertexConnector, ConnectorConfig as VertexConnectorConfig,
};

/// Secret material supplied by the caller after its own storage or file-I/O boundary.
#[derive(Clone, Copy)]
pub(super) enum BorrowedCredential<'a> {
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
    pub(super) fn configuration(error: impl ToString) -> Self {
        Self::Configuration(error.to_string())
    }

    pub(super) fn credential(error: impl ToString) -> Self {
        Self::Credential(error.to_string())
    }
}

/// Provider-specific, non-secret connector configuration.
#[derive(Clone, Debug)]
pub enum Config {
    OpenAi {
        endpoint: Option<String>,
    },
    OpenAiCompatible {
        endpoint: String,
    },
    Anthropic {
        endpoint: Option<String>,
        api_version: Option<String>,
    },
    Gemini {
        endpoint: Option<String>,
    },
    VertexAi {
        project: String,
        location: String,
        probe_model: String,
        auth_mode: ProviderAuthMode,
    },
    Bedrock {
        region: String,
        auth_mode: ProviderAuthMode,
    },
    AzureOpenAi {
        endpoint: String,
        deployment: String,
        api_version: String,
    },
}

impl Config {
    #[must_use]
    pub const fn kind(&self) -> ProviderKind {
        match self {
            Self::OpenAi { .. } => ProviderKind::OpenAi,
            Self::OpenAiCompatible { .. } => ProviderKind::OpenAiCompatible,
            Self::Anthropic { .. } => ProviderKind::Anthropic,
            Self::Gemini { .. } => ProviderKind::Gemini,
            Self::VertexAi { .. } => ProviderKind::VertexAi,
            Self::Bedrock { .. } => ProviderKind::Bedrock,
            Self::AzureOpenAi { .. } => ProviderKind::AzureOpenAi,
        }
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

pub(super) fn credential_kind(config: &Config) -> Result<CredentialKind, Error> {
    match config {
        Config::OpenAi { .. }
        | Config::OpenAiCompatible { .. }
        | Config::Anthropic { .. }
        | Config::Gemini { .. }
        | Config::AzureOpenAi { .. } => Ok(CredentialKind::ApiKey),
        Config::VertexAi {
            auth_mode: ProviderAuthMode::ApplicationDefault,
            ..
        }
        | Config::Bedrock {
            auth_mode: ProviderAuthMode::DefaultChain,
            ..
        } => Ok(CredentialKind::None),
        Config::VertexAi {
            auth_mode: ProviderAuthMode::ServiceAccount,
            ..
        } => Ok(CredentialKind::ServiceAccountJson),
        Config::Bedrock {
            auth_mode: ProviderAuthMode::Static,
            ..
        } => Ok(CredentialKind::AwsStatic),
        Config::VertexAi { auth_mode, .. } => Err(Error::configuration(format!(
            "Unsupported Vertex AI authentication mode {auth_mode}"
        ))),
        Config::Bedrock { auth_mode, .. } => Err(Error::configuration(format!(
            "Unsupported Bedrock authentication mode {auth_mode}"
        ))),
    }
}

pub(super) fn validate_provider_credential(
    config: &Config,
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
pub(super) fn validate_connector_configuration(
    config: &Config,
    allow_unsafe_test_targets: bool,
) -> Result<(), Error> {
    connector_configuration(config, allow_unsafe_test_targets).map(|_| ())
}

/// Validates only a supplied credential. Callers retain their own encryption,
/// decryption, and response-field mapping boundaries.
fn validate_connector_credential(
    config: &Config,
    credential: BorrowedCredential<'_>,
) -> Result<(), Error> {
    match config {
        Config::OpenAi { .. } | Config::OpenAiCompatible { .. } => ApiKey::new(
            text_credential(credential, "OpenAI provider credential is missing")?.to_owned(),
        )
        .map(|_| ())
        .map_err(Error::credential),
        Config::Anthropic { .. } => ApiKey::new(
            text_credential(credential, "Anthropic provider credential is missing")?.to_owned(),
        )
        .map(|_| ())
        .map_err(Error::credential),
        Config::Gemini { .. } => ApiKey::new(
            text_credential(credential, "Gemini provider credential is missing")?.to_owned(),
        )
        .map(|_| ())
        .map_err(Error::credential),
        Config::VertexAi {
            project,
            location,
            probe_model,
            auth_mode: ProviderAuthMode::ServiceAccount,
        } => VertexConnector::with_service_account_json(
            vertex_configuration(project, location, probe_model)?,
            text_credential(
                credential,
                "Vertex AI service-account credential is missing",
            )?,
        )
        .map(|_| ())
        .map_err(Error::credential),
        Config::VertexAi { .. } => Err(Error::credential(
            "ADC providers do not accept stored credentials",
        )),
        Config::Bedrock {
            auth_mode: ProviderAuthMode::Static,
            ..
        } => BedrockStaticCredentials::from_json(bytes_credential(
            credential,
            "Bedrock static credential is missing",
        )?)
        .map(|_| ())
        .map_err(Error::credential),
        Config::Bedrock { .. } => Err(Error::credential(
            "default-chain providers do not accept stored credentials",
        )),
        Config::AzureOpenAi { .. } => ApiKey::new(
            text_credential(credential, "Azure OpenAI credential is missing")?.to_owned(),
        )
        .map(|_| ())
        .map_err(Error::credential),
    }
}

pub(super) enum ConnectorConfiguration {
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

pub(super) enum VertexAuthMode {
    ApplicationDefault,
    ServiceAccount,
}

pub(super) enum BedrockAuthMode {
    DefaultChain,
    Static,
}

pub(super) fn connector_configuration(
    config: &Config,
    allow_unsafe_test_targets: bool,
) -> Result<ConnectorConfiguration, Error> {
    match config {
        Config::OpenAi { endpoint } => endpoint
            .as_deref()
            .map(|endpoint| open_ai_configuration(endpoint, allow_unsafe_test_targets))
            .transpose()
            .map(|configuration| ConnectorConfiguration::OpenAi(configuration.unwrap_or_default()))
            .map_err(Error::configuration),
        Config::OpenAiCompatible { endpoint } => {
            open_ai_configuration(endpoint, allow_unsafe_test_targets)
                .map(ConnectorConfiguration::OpenAi)
                .map_err(Error::configuration)
        }
        Config::Anthropic {
            endpoint,
            api_version,
        } => {
            let mut configuration = endpoint
                .as_deref()
                .map(|endpoint| anthropic_configuration(endpoint, allow_unsafe_test_targets))
                .transpose()
                .map_err(Error::configuration)?
                .unwrap_or_default();
            if let Some(version) = api_version {
                configuration = configuration
                    .with_api_version(version.clone())
                    .map_err(Error::configuration)?;
            }
            Ok(ConnectorConfiguration::Anthropic(configuration))
        }
        Config::Gemini { endpoint } => endpoint
            .as_deref()
            .map(|endpoint| gemini_configuration(endpoint, allow_unsafe_test_targets))
            .transpose()
            .map(|configuration| ConnectorConfiguration::Gemini(configuration.unwrap_or_default()))
            .map_err(Error::configuration),
        Config::VertexAi {
            project,
            location,
            probe_model,
            auth_mode,
        } => {
            let configuration = vertex_configuration(project, location, probe_model)?;
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
        Config::Bedrock { region, auth_mode } => {
            let configuration =
                BedrockConnectorConfig::new(region).map_err(Error::configuration)?;
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
        Config::AzureOpenAi {
            endpoint,
            deployment,
            api_version,
        } => Ok(ConnectorConfiguration::AzureOpenAi(Box::new(
            azure_open_ai_configuration(
                endpoint,
                deployment,
                api_version,
                allow_unsafe_test_targets,
            )
            .map_err(Error::configuration)?,
        ))),
    }
}

/// The unsafe-target constructors exist only in test builds; outside them the
/// policy flag is compiled out and the strict parser is the only path.
fn open_ai_configuration(
    endpoint: &str,
    allow_unsafe_test_targets: bool,
) -> Result<OpenAiConnectorConfig, crate::providers::openai::ConnectorBuildError> {
    #[cfg(any(test, feature = "test-util"))]
    if allow_unsafe_test_targets {
        return OpenAiConnectorConfig::with_base_url_unsafe_test_target(endpoint);
    }
    #[cfg(not(any(test, feature = "test-util")))]
    let _ = allow_unsafe_test_targets;
    OpenAiConnectorConfig::with_base_url(endpoint)
}

fn anthropic_configuration(
    endpoint: &str,
    allow_unsafe_test_targets: bool,
) -> Result<AnthropicConnectorConfig, crate::providers::anthropic::ConnectorBuildError> {
    #[cfg(any(test, feature = "test-util"))]
    if allow_unsafe_test_targets {
        return Ok(AnthropicConnectorConfig::for_local_test(
            endpoint,
            crate::providers::connector::Timeouts::default(),
        ));
    }
    #[cfg(not(any(test, feature = "test-util")))]
    let _ = allow_unsafe_test_targets;
    AnthropicConnectorConfig::with_base_url(endpoint)
}

fn gemini_configuration(
    endpoint: &str,
    allow_unsafe_test_targets: bool,
) -> Result<GeminiConnectorConfig, crate::providers::gemini::ConnectorBuildError> {
    #[cfg(any(test, feature = "test-util"))]
    if allow_unsafe_test_targets {
        return Ok(GeminiConnectorConfig::for_local_test(
            endpoint,
            crate::providers::connector::Timeouts::default(),
        ));
    }
    #[cfg(not(any(test, feature = "test-util")))]
    let _ = allow_unsafe_test_targets;
    GeminiConnectorConfig::with_base_url(endpoint)
}

fn azure_open_ai_configuration(
    endpoint: &str,
    deployment: &str,
    api_version: &str,
    allow_unsafe_test_targets: bool,
) -> Result<AzureOpenAiConnectorConfig, crate::providers::azure_openai::ConnectorBuildError> {
    #[cfg(any(test, feature = "test-util"))]
    if allow_unsafe_test_targets {
        return AzureOpenAiConnectorConfig::new_unsafe_test_target(
            endpoint,
            deployment.to_owned(),
            api_version.to_owned(),
        );
    }
    #[cfg(not(any(test, feature = "test-util")))]
    let _ = allow_unsafe_test_targets;
    AzureOpenAiConnectorConfig::new(endpoint, deployment.to_owned(), api_version.to_owned())
}

fn vertex_configuration(
    project: &str,
    location: &str,
    probe_model: &str,
) -> Result<VertexConnectorConfig, Error> {
    VertexConnectorConfig::new(project, location, probe_model).map_err(Error::configuration)
}

pub(super) fn text_credential<'a>(
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

pub(super) fn bytes_credential<'a>(
    credential: BorrowedCredential<'a>,
    missing: &'static str,
) -> Result<&'a [u8], Error> {
    match credential {
        BorrowedCredential::Text(value) => Ok(value.as_bytes()),
        BorrowedCredential::Bytes(value) => Ok(value),
        BorrowedCredential::None => Err(Error::credential(missing)),
    }
}

pub(super) fn no_credential(
    credential: BorrowedCredential<'_>,
    message: &'static str,
) -> Result<(), Error> {
    matches!(credential, BorrowedCredential::None)
        .then_some(())
        .ok_or_else(|| Error::credential(message))
}
