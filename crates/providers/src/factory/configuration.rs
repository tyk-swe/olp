use std::fmt;

use olp_domain::{CredentialRequirement, ProviderAuthMode, ProviderKind, provider_kind_spec};
use zeroize::Zeroizing;

use crate::anthropic::{AnthropicApiKey, ConnectorConfig as AnthropicConnectorConfig};
use crate::azure_openai::{AzureOpenAiApiKey, ConnectorConfig as AzureOpenAiConnectorConfig};
use crate::bedrock::{
    ConnectorConfig as BedrockConnectorConfig, StaticCredentials as BedrockStaticCredentials,
};
use crate::gemini::{ConnectorConfig as GeminiConnectorConfig, GeminiApiKey};
use crate::openai::{ConnectorConfig as OpenAiConnectorConfig, OpenAiApiKey};
use crate::vertex::{ConnectorConfig as VertexConnectorConfig, VertexConnector};

/// Non-secret provider fields required to assemble a connector.
#[derive(Clone, Copy, Debug)]
pub(super) struct ConnectorSpec<'a> {
    pub(super) kind: ProviderKind,
    pub(super) endpoint: Option<&'a str>,
    pub(super) cloud_region: Option<&'a str>,
    pub(super) cloud_project: Option<&'a str>,
    pub(super) deployment: Option<&'a str>,
    pub(super) api_version: Option<&'a str>,
    pub(super) auth_mode: ProviderAuthMode,
    pub(super) probe_model: Option<&'a str>,
}

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

/// The secret representation expected by a provider authentication mode.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(super) enum RawCredentialKind {
    None,
    Text,
    Bytes,
}

/// Connector assembly failures are deliberately string-only so callers can map
/// them to their own HTTP or process error contracts without exposing secrets.
#[derive(Debug, thiserror::Error)]
pub enum ProviderError {
    #[error("{0}")]
    Configuration(String),
    #[error("{0}")]
    Credential(String),
}

impl ProviderError {
    pub(super) fn configuration(error: impl ToString) -> Self {
        Self::Configuration(error.to_string())
    }

    pub(super) fn credential(error: impl ToString) -> Self {
        Self::Credential(error.to_string())
    }
}

/// Provider-specific, non-secret connector configuration.
#[derive(Clone, Debug)]
pub enum ProviderConfig {
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

impl ProviderConfig {
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

    pub(super) fn spec(&self) -> ConnectorSpec<'_> {
        match self {
            Self::OpenAi { endpoint } => ConnectorSpec {
                kind: ProviderKind::OpenAi,
                endpoint: endpoint.as_deref(),
                cloud_region: None,
                cloud_project: None,
                deployment: None,
                api_version: None,
                auth_mode: ProviderAuthMode::ApiKey,
                probe_model: None,
            },
            Self::OpenAiCompatible { endpoint } => ConnectorSpec {
                kind: ProviderKind::OpenAiCompatible,
                endpoint: Some(endpoint),
                cloud_region: None,
                cloud_project: None,
                deployment: None,
                api_version: None,
                auth_mode: ProviderAuthMode::ApiKey,
                probe_model: None,
            },
            Self::Anthropic {
                endpoint,
                api_version,
            } => ConnectorSpec {
                kind: ProviderKind::Anthropic,
                endpoint: endpoint.as_deref(),
                cloud_region: None,
                cloud_project: None,
                deployment: None,
                api_version: api_version.as_deref(),
                auth_mode: ProviderAuthMode::ApiKey,
                probe_model: None,
            },
            Self::Gemini { endpoint } => ConnectorSpec {
                kind: ProviderKind::Gemini,
                endpoint: endpoint.as_deref(),
                cloud_region: None,
                cloud_project: None,
                deployment: None,
                api_version: None,
                auth_mode: ProviderAuthMode::ApiKey,
                probe_model: None,
            },
            Self::VertexAi {
                project,
                location,
                probe_model,
                auth_mode,
            } => ConnectorSpec {
                kind: ProviderKind::VertexAi,
                endpoint: None,
                cloud_region: Some(location),
                cloud_project: Some(project),
                deployment: None,
                api_version: None,
                auth_mode: *auth_mode,
                probe_model: Some(probe_model),
            },
            Self::Bedrock { region, auth_mode } => ConnectorSpec {
                kind: ProviderKind::Bedrock,
                endpoint: None,
                cloud_region: Some(region),
                cloud_project: None,
                deployment: None,
                api_version: None,
                auth_mode: *auth_mode,
                probe_model: None,
            },
            Self::AzureOpenAi {
                endpoint,
                deployment,
                api_version,
            } => ConnectorSpec {
                kind: ProviderKind::AzureOpenAi,
                endpoint: Some(endpoint),
                cloud_region: None,
                cloud_project: None,
                deployment: Some(deployment),
                api_version: Some(api_version),
                auth_mode: ProviderAuthMode::ApiKey,
                probe_model: None,
            },
        }
    }
}

/// Secret material is named by semantics and zeroized on drop.
pub enum ProviderCredential {
    None,
    ApiKey(Zeroizing<String>),
    ServiceAccountJson(Zeroizing<String>),
    AwsStatic(Zeroizing<Vec<u8>>),
}

impl fmt::Debug for ProviderCredential {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::None => formatter.write_str("ProviderCredential::None"),
            Self::ApiKey(_) => formatter.write_str("ProviderCredential::ApiKey([REDACTED])"),
            Self::ServiceAccountJson(_) => {
                formatter.write_str("ProviderCredential::ServiceAccountJson([REDACTED])")
            }
            Self::AwsStatic(_) => formatter.write_str("ProviderCredential::AwsStatic([REDACTED])"),
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

/// Returns the expected secret representation without reading or retaining it.
#[doc(hidden)]
pub(super) fn raw_credential_kind(
    spec: ConnectorSpec<'_>,
) -> Result<RawCredentialKind, ProviderError> {
    let auth = provider_kind_spec(spec.kind)
        .auth_mode(spec.auth_mode)
        .ok_or_else(|| {
            ProviderError::configuration(format!(
                "Unsupported {} authentication mode {}",
                spec.kind, spec.auth_mode
            ))
        })?;
    match auth.credential {
        CredentialRequirement::Forbidden => Ok(RawCredentialKind::None),
        CredentialRequirement::Required if spec.kind == ProviderKind::Bedrock => {
            Ok(RawCredentialKind::Bytes)
        }
        CredentialRequirement::Required => Ok(RawCredentialKind::Text),
    }
}

pub(super) fn credential_kind(config: &ProviderConfig) -> Result<CredentialKind, ProviderError> {
    let kind = match raw_credential_kind(config.spec())? {
        RawCredentialKind::None => CredentialKind::None,
        RawCredentialKind::Text => match config {
            ProviderConfig::VertexAi {
                auth_mode: ProviderAuthMode::ServiceAccount,
                ..
            } => CredentialKind::ServiceAccountJson,
            _ => CredentialKind::ApiKey,
        },
        RawCredentialKind::Bytes => CredentialKind::AwsStatic,
    };
    Ok(kind)
}

pub(super) fn validate_provider_credential(
    config: &ProviderConfig,
    credential: &ProviderCredential,
) -> Result<(), ProviderError> {
    let expected = credential_kind(config)?;
    let (supplied, borrowed) = match credential {
        ProviderCredential::None => (CredentialKind::None, BorrowedCredential::None),
        ProviderCredential::ApiKey(value) => (
            CredentialKind::ApiKey,
            BorrowedCredential::Text(value.as_str()),
        ),
        ProviderCredential::ServiceAccountJson(value) => (
            CredentialKind::ServiceAccountJson,
            BorrowedCredential::Text(value.as_str()),
        ),
        ProviderCredential::AwsStatic(value) => (
            CredentialKind::AwsStatic,
            BorrowedCredential::Bytes(value.as_slice()),
        ),
    };
    if expected != supplied {
        return Err(ProviderError::credential(
            "provider credential does not match its authentication mode",
        ));
    }
    validate_connector_credential(config.spec(), borrowed)
}

/// Validates connector configuration without acquiring default credentials or
/// issuing network I/O.
pub(super) fn validate_connector_configuration(
    spec: ConnectorSpec<'_>,
) -> Result<(), ProviderError> {
    connector_configuration(spec).map(|_| ())
}

/// Validates only a supplied credential. Callers retain their own encryption,
/// decryption, and response-field mapping boundaries.
pub(super) fn validate_connector_credential(
    spec: ConnectorSpec<'_>,
    credential: BorrowedCredential<'_>,
) -> Result<(), ProviderError> {
    match spec.kind {
        ProviderKind::OpenAi | ProviderKind::OpenAiCompatible => OpenAiApiKey::new(
            text_credential(credential, "OpenAI provider credential is missing")?.to_owned(),
        )
        .map(|_| ())
        .map_err(ProviderError::credential),
        ProviderKind::Anthropic => AnthropicApiKey::new(
            text_credential(credential, "Anthropic provider credential is missing")?.to_owned(),
        )
        .map(|_| ())
        .map_err(ProviderError::credential),
        ProviderKind::Gemini => GeminiApiKey::new(
            text_credential(credential, "Gemini provider credential is missing")?.to_owned(),
        )
        .map(|_| ())
        .map_err(ProviderError::credential),
        ProviderKind::VertexAi if spec.auth_mode == ProviderAuthMode::ServiceAccount => {
            VertexConnector::with_service_account_json(
                vertex_configuration(spec)?,
                text_credential(
                    credential,
                    "Vertex AI service-account credential is missing",
                )?,
            )
            .map(|_| ())
            .map_err(ProviderError::credential)
        }
        ProviderKind::VertexAi => Err(ProviderError::credential(
            "ADC providers do not accept stored credentials",
        )),
        ProviderKind::Bedrock if spec.auth_mode == ProviderAuthMode::Static => {
            BedrockStaticCredentials::from_json(bytes_credential(
                credential,
                "Bedrock static credential is missing",
            )?)
            .map(|_| ())
            .map_err(ProviderError::credential)
        }
        ProviderKind::Bedrock => Err(ProviderError::credential(
            "default-chain providers do not accept stored credentials",
        )),
        ProviderKind::AzureOpenAi => AzureOpenAiApiKey::new(
            text_credential(credential, "Azure OpenAI credential is missing")?.to_owned(),
        )
        .map(|_| ())
        .map_err(ProviderError::credential),
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
    spec: ConnectorSpec<'_>,
) -> Result<ConnectorConfiguration, ProviderError> {
    match spec.kind {
        ProviderKind::OpenAi | ProviderKind::OpenAiCompatible => spec
            .endpoint
            .map(OpenAiConnectorConfig::with_base_url)
            .transpose()
            .map(|configuration| ConnectorConfiguration::OpenAi(configuration.unwrap_or_default()))
            .map_err(ProviderError::configuration),
        ProviderKind::Anthropic => {
            let mut configuration = spec
                .endpoint
                .map(AnthropicConnectorConfig::with_base_url)
                .transpose()
                .map_err(ProviderError::configuration)?
                .unwrap_or_default();
            if let Some(version) = spec.api_version {
                configuration = configuration
                    .with_api_version(version.to_owned())
                    .map_err(ProviderError::configuration)?;
            }
            Ok(ConnectorConfiguration::Anthropic(configuration))
        }
        ProviderKind::Gemini => spec
            .endpoint
            .map(GeminiConnectorConfig::with_base_url)
            .transpose()
            .map(|configuration| ConnectorConfiguration::Gemini(configuration.unwrap_or_default()))
            .map_err(ProviderError::configuration),
        ProviderKind::VertexAi => {
            let configuration = vertex_configuration(spec)?;
            let auth_mode = match spec.auth_mode {
                ProviderAuthMode::ApplicationDefault => VertexAuthMode::ApplicationDefault,
                ProviderAuthMode::ServiceAccount => VertexAuthMode::ServiceAccount,
                mode => {
                    return Err(ProviderError::configuration(format!(
                        "Unsupported Vertex AI authentication mode {mode}"
                    )));
                }
            };
            Ok(ConnectorConfiguration::Vertex {
                configuration,
                auth_mode,
            })
        }
        ProviderKind::Bedrock => {
            let configuration = BedrockConnectorConfig::new(required_configuration_field(
                spec.cloud_region,
                "Bedrock AWS region is missing",
            )?)
            .map_err(ProviderError::configuration)?;
            let auth_mode = match spec.auth_mode {
                ProviderAuthMode::DefaultChain => BedrockAuthMode::DefaultChain,
                ProviderAuthMode::Static => BedrockAuthMode::Static,
                mode => {
                    return Err(ProviderError::configuration(format!(
                        "Unsupported Bedrock authentication mode {mode}"
                    )));
                }
            };
            Ok(ConnectorConfiguration::Bedrock {
                configuration,
                auth_mode,
            })
        }
        ProviderKind::AzureOpenAi => {
            let endpoint = required_configuration_field(
                spec.endpoint,
                "Azure OpenAI resource endpoint is missing",
            )?;
            let deployment = required_configuration_field(
                spec.deployment,
                "Azure OpenAI deployment is missing",
            )?;
            let api_version = required_configuration_field(
                spec.api_version,
                "Azure OpenAI API version is missing",
            )?;
            if spec.auth_mode != ProviderAuthMode::ApiKey {
                return Err(ProviderError::configuration(format!(
                    "Unsupported Azure OpenAI authentication mode {}",
                    spec.auth_mode
                )));
            }
            Ok(ConnectorConfiguration::AzureOpenAi(Box::new(
                AzureOpenAiConnectorConfig::new(
                    endpoint,
                    deployment.to_owned(),
                    api_version.to_owned(),
                )
                .map_err(ProviderError::configuration)?,
            )))
        }
    }
}

fn vertex_configuration(spec: ConnectorSpec<'_>) -> Result<VertexConnectorConfig, ProviderError> {
    VertexConnectorConfig::new(
        required_configuration_field(spec.cloud_project, "Vertex AI cloud project is missing")?,
        required_configuration_field(spec.cloud_region, "Vertex AI cloud location is missing")?,
        required_configuration_field(spec.probe_model, "Vertex AI probe model is missing")?,
    )
    .map_err(ProviderError::configuration)
}

fn required_configuration_field<'a>(
    value: Option<&'a str>,
    message: &'static str,
) -> Result<&'a str, ProviderError> {
    value.ok_or_else(|| ProviderError::configuration(message))
}

pub(super) fn text_credential<'a>(
    credential: BorrowedCredential<'a>,
    missing: &'static str,
) -> Result<&'a str, ProviderError> {
    match credential {
        BorrowedCredential::Text(value) => Ok(value),
        BorrowedCredential::Bytes(_) => Err(ProviderError::credential(
            "provider credential is not valid UTF-8",
        )),
        BorrowedCredential::None => Err(ProviderError::credential(missing)),
    }
}

pub(super) fn bytes_credential<'a>(
    credential: BorrowedCredential<'a>,
    missing: &'static str,
) -> Result<&'a [u8], ProviderError> {
    match credential {
        BorrowedCredential::Text(value) => Ok(value.as_bytes()),
        BorrowedCredential::Bytes(value) => Ok(value),
        BorrowedCredential::None => Err(ProviderError::credential(missing)),
    }
}

pub(super) fn no_credential(
    credential: BorrowedCredential<'_>,
    message: &'static str,
) -> Result<(), ProviderError> {
    matches!(credential, BorrowedCredential::None)
        .then_some(())
        .ok_or_else(|| ProviderError::credential(message))
}
