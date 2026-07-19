use std::{
    collections::BTreeMap,
    fmt,
    sync::{Arc, RwLock},
    time::Duration,
};

use crate::anthropic::{
    AnthropicApiKey, AnthropicConnector, ConnectorConfig as AnthropicConnectorConfig,
};
use crate::azure_openai::{
    AzureOpenAiApiKey, AzureOpenAiConnector, ConnectorConfig as AzureOpenAiConnectorConfig,
};
use crate::bedrock::{
    BedrockConnector, BedrockCredentials, ConnectorConfig as BedrockConnectorConfig,
    StaticCredentials as BedrockStaticCredentials,
};
use crate::gemini::{ConnectorConfig as GeminiConnectorConfig, GeminiApiKey, GeminiConnector};
pub use crate::openai::{CompatibleCapability, CompatibleCapabilityCertificationError};
use crate::openai::{
    ConnectorConfig as OpenAiConnectorConfig, NativeOpenAiCertificationEvidence, OpenAiApiKey,
};
use crate::vertex::{ConnectorConfig as VertexConnectorConfig, VertexConnector};
use futures::StreamExt as _;
use olp_domain::{
    AttemptFailureClass, AttemptPlan, CanonicalEventKind, CanonicalResult, ContentPart,
    DiscoveredProviderModel, DurationMs, EmbeddingInput, EmbeddingsRequest, EventSequenceValidator,
    GenerationParameters, GenerationRequest, Message, MessageRole, ModerationRequest, Operation,
    OperationKind, ProviderAuthMode, ProviderId, ProviderKind, ProviderOutput, ProviderRequest,
    ProviderTransport, RequestId, RequestMetadata, RouteId, RouteSlug, RuntimeGenerationId,
    SourceExtensions, Surface, TargetId, TokenCountRequest, TransportMode, TransportPhase,
};
use uuid::Uuid;
use zeroize::Zeroizing;

pub use crate::openai::OpenAiConnector;

/// Non-secret provider fields required to assemble a connector.
#[derive(Clone, Copy, Debug)]
struct ConnectorSpec<'a> {
    kind: ProviderKind,
    endpoint: Option<&'a str>,
    cloud_region: Option<&'a str>,
    cloud_project: Option<&'a str>,
    deployment: Option<&'a str>,
    api_version: Option<&'a str>,
    auth_mode: ProviderAuthMode,
    probe_model: Option<&'a str>,
}

/// Secret material supplied by the caller after its own storage or file-I/O boundary.
#[derive(Clone, Copy)]
enum BorrowedCredential<'a> {
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
enum RawCredentialKind {
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
    fn configuration(error: impl ToString) -> Self {
        Self::Configuration(error.to_string())
    }

    fn credential(error: impl ToString) -> Self {
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

    fn spec(&self) -> ConnectorSpec<'_> {
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

/// Single assembly entrypoint for runtime transport, discovery, probes, and
/// capability certification.
#[derive(Clone, Copy, Debug, Default)]
pub struct ProviderFactory;

impl ProviderFactory {
    pub fn validate(config: &ProviderConfig) -> Result<(), ProviderError> {
        validate_connector_configuration(config.spec())
    }

    pub fn credential_kind(config: &ProviderConfig) -> Result<CredentialKind, ProviderError> {
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

    pub fn validate_credential(
        config: &ProviderConfig,
        credential: &ProviderCredential,
    ) -> Result<(), ProviderError> {
        let expected = Self::credential_kind(config)?;
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

    pub async fn create(
        config: ProviderConfig,
        credential: ProviderCredential,
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
        build_connector(config.spec(), borrowed)
            .await
            .map(|inner| ProviderFacade { inner })
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
    inner: ConcreteProvider,
}

/// Whether a successful discovery response is safe to use as an inventory
/// snapshot. Compatible endpoints and configured deployment probes deliberately
/// remain partial: their omission cannot establish that a model disappeared.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum ModelDiscoveryCompleteness {
    Complete,
    Partial,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ProviderModelDiscovery {
    pub models: Vec<DiscoveredProviderModel>,
    pub completeness: ModelDiscoveryCompleteness,
}

pub const fn model_discovery_completeness(kind: ProviderKind) -> ModelDiscoveryCompleteness {
    match kind {
        ProviderKind::OpenAi | ProviderKind::Anthropic | ProviderKind::Gemini => {
            ModelDiscoveryCompleteness::Complete
        }
        // Bedrock only lists foundation models, while valid runtime targets can
        // also be custom models or inference profiles.
        ProviderKind::OpenAiCompatible
        | ProviderKind::VertexAi
        | ProviderKind::Bedrock
        | ProviderKind::AzureOpenAi => ModelDiscoveryCompleteness::Partial,
    }
}

impl ProviderFacade {
    pub fn into_transport(self) -> Arc<dyn ProviderTransport> {
        self.inner.into_transport()
    }

    pub async fn discover_models(&self) -> Result<ProviderModelDiscovery, String> {
        self.inner.discover_models().await
    }

    pub async fn certify_capability(
        &self,
        provider_model: &str,
        capability: CompatibleCapability,
    ) -> Result<CapabilityCertificationEvidence, CompatibleCapabilityCertificationError> {
        self.inner
            .certify_capability(provider_model, capability)
            .await
    }
}

#[derive(Clone, Default)]
pub struct OpenAiConnectorOverrideRegistry {
    inner: Arc<RwLock<BTreeMap<Uuid, Arc<OpenAiConnector>>>>,
}

impl OpenAiConnectorOverrideRegistry {
    pub fn register(&self, provider_id: Uuid, connector: OpenAiConnector) {
        self.inner
            .write()
            .expect("catalog connector registry lock poisoned")
            .insert(provider_id, Arc::new(connector));
    }

    pub fn get(&self, provider_id: Uuid, kind: ProviderKind) -> Option<ProviderFacade> {
        if !matches!(kind, ProviderKind::OpenAi | ProviderKind::OpenAiCompatible) {
            return None;
        }
        self.inner
            .read()
            .expect("catalog connector registry lock poisoned")
            .get(&provider_id)
            .cloned()
            .map(|connector| ProviderFacade {
                inner: ConcreteProvider {
                    kind,
                    connector: ConcreteConnector::OpenAi(connector),
                },
            })
    }
}

/// Returns the expected secret representation without reading or retaining it.
#[doc(hidden)]
fn raw_credential_kind(spec: ConnectorSpec<'_>) -> Result<RawCredentialKind, ProviderError> {
    match spec.kind {
        ProviderKind::OpenAi
        | ProviderKind::OpenAiCompatible
        | ProviderKind::Anthropic
        | ProviderKind::Gemini
        | ProviderKind::AzureOpenAi
            if spec.auth_mode == ProviderAuthMode::ApiKey =>
        {
            Ok(RawCredentialKind::Text)
        }
        ProviderKind::OpenAi
        | ProviderKind::OpenAiCompatible
        | ProviderKind::Anthropic
        | ProviderKind::Gemini
        | ProviderKind::AzureOpenAi => Err(ProviderError::configuration(format!(
            "Unsupported {} authentication mode {}",
            spec.kind, spec.auth_mode
        ))),
        ProviderKind::VertexAi => match spec.auth_mode {
            ProviderAuthMode::ApplicationDefault => Ok(RawCredentialKind::None),
            ProviderAuthMode::ServiceAccount => Ok(RawCredentialKind::Text),
            mode => Err(ProviderError::configuration(format!(
                "Unsupported Vertex AI authentication mode {mode}"
            ))),
        },
        ProviderKind::Bedrock => match spec.auth_mode {
            ProviderAuthMode::DefaultChain => Ok(RawCredentialKind::None),
            ProviderAuthMode::Static => Ok(RawCredentialKind::Bytes),
            mode => Err(ProviderError::configuration(format!(
                "Unsupported Bedrock authentication mode {mode}"
            ))),
        },
    }
}

/// Returns whether the installed connector has a safe certification path for
/// a reviewed capability. This is narrower than catalog eligibility: the
/// management UI must not offer tuples that can never satisfy activation's
/// certification requirement.
pub const fn supports_capability_certification(
    kind: ProviderKind,
    operation: OperationKind,
    surface: Surface,
    mode: TransportMode,
) -> bool {
    if !kind.supports_capability(operation, surface, mode) {
        return false;
    }

    match kind {
        ProviderKind::OpenAiCompatible => matches!(
            (operation, surface, mode),
            (
                OperationKind::Generation,
                Surface::OpenAi,
                TransportMode::Unary | TransportMode::Streaming
            ) | (
                OperationKind::Embeddings | OperationKind::TokenCount | OperationKind::Moderation,
                Surface::OpenAi,
                TransportMode::Unary
            )
        ),
        ProviderKind::AzureOpenAi => matches!(
            (operation, mode),
            (
                OperationKind::Generation,
                TransportMode::Unary | TransportMode::Streaming
            ) | (
                OperationKind::Embeddings | OperationKind::TokenCount | OperationKind::Moderation,
                TransportMode::Unary
            )
        ),
        _ => true,
    }
}

pub fn certifiable_capabilities(
    kind: ProviderKind,
) -> impl Iterator<Item = (OperationKind, Surface, TransportMode)> {
    kind.supported_capabilities()
        .filter(move |(operation, surface, mode)| {
            supports_capability_certification(kind, *operation, *surface, *mode)
        })
}

/// Validates connector configuration without acquiring default credentials or
/// issuing network I/O.
fn validate_connector_configuration(spec: ConnectorSpec<'_>) -> Result<(), ProviderError> {
    connector_configuration(spec).map(|_| ())
}

/// Validates only a supplied credential. Callers retain their own encryption,
/// decryption, and response-field mapping boundaries.
fn validate_connector_credential(
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

async fn build_connector(
    spec: ConnectorSpec<'_>,
    credential: BorrowedCredential<'_>,
) -> Result<ConcreteProvider, ProviderError> {
    let kind = spec.kind;
    let connector = match connector_configuration(spec)? {
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
    Ok(ConcreteProvider { kind, connector })
}

enum ConnectorConfiguration {
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

enum VertexAuthMode {
    ApplicationDefault,
    ServiceAccount,
}

enum BedrockAuthMode {
    DefaultChain,
    Static,
}

fn connector_configuration(
    spec: ConnectorSpec<'_>,
) -> Result<ConnectorConfiguration, ProviderError> {
    match spec.kind {
        ProviderKind::OpenAi | ProviderKind::OpenAiCompatible => {
            require_api_key_auth(spec)?;
            spec.endpoint
                .map(OpenAiConnectorConfig::with_base_url)
                .transpose()
                .map(|configuration| {
                    ConnectorConfiguration::OpenAi(configuration.unwrap_or_default())
                })
                .map_err(ProviderError::configuration)
        }
        ProviderKind::Anthropic => {
            require_api_key_auth(spec)?;
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
        ProviderKind::Gemini => {
            require_api_key_auth(spec)?;
            spec.endpoint
                .map(GeminiConnectorConfig::with_base_url)
                .transpose()
                .map(|configuration| {
                    ConnectorConfiguration::Gemini(configuration.unwrap_or_default())
                })
                .map_err(ProviderError::configuration)
        }
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

fn require_api_key_auth(spec: ConnectorSpec<'_>) -> Result<(), ProviderError> {
    if spec.auth_mode == ProviderAuthMode::ApiKey {
        Ok(())
    } else {
        Err(ProviderError::configuration(format!(
            "Unsupported {} authentication mode {}",
            spec.kind, spec.auth_mode
        )))
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

fn text_credential<'a>(
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

fn bytes_credential<'a>(
    credential: BorrowedCredential<'a>,
    missing: &'static str,
) -> Result<&'a [u8], ProviderError> {
    match credential {
        BorrowedCredential::Text(value) => Ok(value.as_bytes()),
        BorrowedCredential::Bytes(value) => Ok(value),
        BorrowedCredential::None => Err(ProviderError::credential(missing)),
    }
}

fn no_credential(
    credential: BorrowedCredential<'_>,
    message: &'static str,
) -> Result<(), ProviderError> {
    matches!(credential, BorrowedCredential::None)
        .then_some(())
        .ok_or_else(|| ProviderError::credential(message))
}

enum ConcreteConnector {
    OpenAi(Arc<OpenAiConnector>),
    Anthropic(Arc<AnthropicConnector>),
    Gemini(Arc<GeminiConnector>),
    Vertex(Arc<VertexConnector>),
    Bedrock(Arc<BedrockConnector>),
    AzureOpenAi(Arc<AzureOpenAiConnector>),
}

struct ConcreteProvider {
    kind: ProviderKind,
    connector: ConcreteConnector,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum CapabilityCertificationEvidence {
    LiveProbe,
    NativeOpenAiModelDiscoveryAndConnectorContract,
}

impl From<NativeOpenAiCertificationEvidence> for CapabilityCertificationEvidence {
    fn from(value: NativeOpenAiCertificationEvidence) -> Self {
        match value {
            NativeOpenAiCertificationEvidence::LiveProbe => Self::LiveProbe,
            NativeOpenAiCertificationEvidence::ModelDiscoveryAndConnectorContract => {
                Self::NativeOpenAiModelDiscoveryAndConnectorContract
            }
        }
    }
}

impl ConcreteProvider {
    fn into_transport(self) -> Arc<dyn ProviderTransport> {
        match self.connector {
            ConcreteConnector::OpenAi(connector) => connector,
            ConcreteConnector::Anthropic(connector) => connector,
            ConcreteConnector::Gemini(connector) => connector,
            ConcreteConnector::Vertex(connector) => connector,
            ConcreteConnector::Bedrock(connector) => connector,
            ConcreteConnector::AzureOpenAi(connector) => connector,
        }
    }

    pub async fn discover_models(&self) -> Result<ProviderModelDiscovery, String> {
        let models = match &self.connector {
            ConcreteConnector::OpenAi(connector) => connector.discover_models().await,
            ConcreteConnector::Anthropic(connector) => connector.discover_models().await,
            ConcreteConnector::Gemini(connector) => connector.discover_models().await,
            ConcreteConnector::Vertex(connector) => connector.discover_models().await,
            ConcreteConnector::Bedrock(connector) => connector.discover_models().await,
            ConcreteConnector::AzureOpenAi(connector) => connector.discover_models().await,
        };
        let models = models.map_err(|error| error.to_string())?;
        Ok(ProviderModelDiscovery {
            models,
            completeness: model_discovery_completeness(self.kind),
        })
    }

    pub async fn certify_capability(
        &self,
        provider_model: &str,
        capability: CompatibleCapability,
    ) -> Result<CapabilityCertificationEvidence, CompatibleCapabilityCertificationError> {
        match (&self.connector, self.kind) {
            (ConcreteConnector::OpenAi(connector), ProviderKind::OpenAiCompatible) => connector
                .certify_compatible_capability(provider_model, capability)
                .await
                .map(|()| CapabilityCertificationEvidence::LiveProbe),
            (ConcreteConnector::AzureOpenAi(connector), ProviderKind::AzureOpenAi) => connector
                .certify_deployment_capability(provider_model, capability)
                .await
                .map(|()| CapabilityCertificationEvidence::LiveProbe),
            (ConcreteConnector::OpenAi(connector), ProviderKind::OpenAi)
                if capability.surface == Surface::OpenAi =>
            {
                connector
                    .certify_native_openai_capability(provider_model, capability)
                    .await
                    .map(Into::into)
            }
            (ConcreteConnector::OpenAi(connector), ProviderKind::OpenAi) => {
                execute_native_capability_probe(
                    connector.as_ref(),
                    ProviderKind::OpenAi,
                    provider_model,
                    capability,
                )
                .await
                .map(|()| CapabilityCertificationEvidence::LiveProbe)
            }
            (ConcreteConnector::Anthropic(connector), ProviderKind::Anthropic) => {
                execute_native_capability_probe(
                    connector.as_ref(),
                    ProviderKind::Anthropic,
                    provider_model,
                    capability,
                )
                .await
                .map(|()| CapabilityCertificationEvidence::LiveProbe)
            }
            (ConcreteConnector::Gemini(connector), ProviderKind::Gemini) => {
                execute_native_capability_probe(
                    connector.as_ref(),
                    ProviderKind::Gemini,
                    provider_model,
                    capability,
                )
                .await
                .map(|()| CapabilityCertificationEvidence::LiveProbe)
            }
            (ConcreteConnector::Vertex(connector), ProviderKind::VertexAi) => {
                execute_native_capability_probe(
                    connector.as_ref(),
                    ProviderKind::VertexAi,
                    provider_model,
                    capability,
                )
                .await
                .map(|()| CapabilityCertificationEvidence::LiveProbe)
            }
            (ConcreteConnector::Bedrock(connector), ProviderKind::Bedrock) => {
                execute_native_capability_probe(
                    connector.as_ref(),
                    ProviderKind::Bedrock,
                    provider_model,
                    capability,
                )
                .await
                .map(|()| CapabilityCertificationEvidence::LiveProbe)
            }
            _ => Err(CompatibleCapabilityCertificationError::Unsupported),
        }
    }
}

const NATIVE_PROBE_TIMEOUT_MS: u64 = 10_000;
const MAX_NATIVE_PROBE_EVENTS: usize = 4_096;

async fn execute_native_capability_probe(
    transport: &dyn ProviderTransport,
    provider_kind: ProviderKind,
    provider_model: &str,
    capability: CompatibleCapability,
) -> Result<(), CompatibleCapabilityCertificationError> {
    let operation = native_probe_operation(provider_kind, capability)?;
    let request = ProviderRequest {
        metadata: RequestMetadata {
            request_id: RequestId::new(),
            operation: capability.operation,
            surface: capability.surface,
            mode: capability.mode,
        },
        attempt: AttemptPlan {
            generation_id: RuntimeGenerationId::new(),
            route_id: RouteId::new(),
            target_id: TargetId::new(),
            provider_id: ProviderId::new(),
            provider_kind,
            provider_model: provider_model.to_owned(),
            timeout: DurationMs::new(NATIVE_PROBE_TIMEOUT_MS),
            priority: 0,
        },
        operation,
        media: None,
    };
    let output = tokio::time::timeout(
        Duration::from_millis(NATIVE_PROBE_TIMEOUT_MS),
        transport.execute(request),
    )
    .await
    .map_err(|_| CompatibleCapabilityCertificationError::Transport {
        phase: TransportPhase::FirstByte,
        class: AttemptFailureClass::Timeout,
    })?
    .map_err(|error| CompatibleCapabilityCertificationError::Transport {
        phase: error.phase,
        class: error.class,
    })?;
    validate_native_probe_output(capability.operation, output).await
}

fn native_probe_operation(
    provider_kind: ProviderKind,
    capability: CompatibleCapability,
) -> Result<Operation, CompatibleCapabilityCertificationError> {
    let route = RouteSlug::parse("capability-probe")
        .map_err(|_| CompatibleCapabilityCertificationError::InvalidResult)?;
    let extensions = || SourceExtensions::new(capability.surface, Default::default());
    match (provider_kind, capability.operation, capability.mode) {
        (
            ProviderKind::OpenAi
            | ProviderKind::Anthropic
            | ProviderKind::Gemini
            | ProviderKind::VertexAi
            | ProviderKind::Bedrock,
            OperationKind::Generation,
            TransportMode::Unary | TransportMode::Streaming,
        ) => Ok(Operation::Generation(GenerationRequest {
            route,
            messages: vec![Message {
                role: MessageRole::User,
                content: vec![ContentPart::Text {
                    text: "OLP capability probe".to_owned(),
                }],
                name: None,
                tool_call_id: None,
                tool_calls: Vec::new(),
            }],
            parameters: GenerationParameters {
                max_output_tokens: Some(1),
                temperature: Some(0.0),
                stream: capability.mode == TransportMode::Streaming,
                ..GenerationParameters::default()
            },
            tools: Vec::new(),
            tool_choice: None,
            response_format: None,
            extensions: extensions(),
        })),
        (
            ProviderKind::OpenAi
            | ProviderKind::Anthropic
            | ProviderKind::Gemini
            | ProviderKind::VertexAi
            | ProviderKind::Bedrock,
            OperationKind::TokenCount,
            TransportMode::Unary,
        ) => Ok(Operation::TokenCount(TokenCountRequest {
            route,
            input: vec![ContentPart::Text {
                text: "OLP capability probe".to_owned(),
            }],
            extensions: extensions(),
        })),
        (ProviderKind::OpenAi, OperationKind::Embeddings, TransportMode::Unary)
            if capability.surface == Surface::OpenAi =>
        {
            Ok(Operation::Embeddings(EmbeddingsRequest {
                route,
                input: vec![EmbeddingInput::Text("OLP capability probe".to_owned())],
                dimensions: None,
                extensions: extensions(),
            }))
        }
        (ProviderKind::OpenAi, OperationKind::Moderation, TransportMode::Unary)
            if capability.surface == Surface::OpenAi =>
        {
            Ok(Operation::Moderation(ModerationRequest {
                route,
                input: vec![ContentPart::Text {
                    text: "OLP capability probe".to_owned(),
                }],
                extensions: extensions(),
            }))
        }
        _ => Err(CompatibleCapabilityCertificationError::Unsupported),
    }
}

async fn validate_native_probe_output(
    operation: OperationKind,
    output: ProviderOutput,
) -> Result<(), CompatibleCapabilityCertificationError> {
    match (operation, output) {
        (OperationKind::Generation, ProviderOutput::Events(mut events)) => {
            let mut validator = EventSequenceValidator::new();
            let deadline =
                tokio::time::Instant::now() + Duration::from_millis(NATIVE_PROBE_TIMEOUT_MS);
            let mut count = 0_usize;
            loop {
                let event = tokio::time::timeout_at(deadline, events.next())
                    .await
                    .map_err(|_| CompatibleCapabilityCertificationError::Transport {
                        phase: TransportPhase::Body,
                        class: AttemptFailureClass::Timeout,
                    })?;
                let Some(event) = event else {
                    break;
                };
                if count >= MAX_NATIVE_PROBE_EVENTS {
                    return Err(CompatibleCapabilityCertificationError::InvalidResult);
                }
                let event =
                    event.map_err(|error| CompatibleCapabilityCertificationError::Transport {
                        phase: error.phase,
                        class: error.class,
                    })?;
                if matches!(event.kind, CanonicalEventKind::Error { .. }) {
                    return Err(CompatibleCapabilityCertificationError::InvalidResult);
                }
                validator
                    .push(&event)
                    .map_err(|_| CompatibleCapabilityCertificationError::InvalidResult)?;
                count = count.saturating_add(1);
                if validator.is_complete() {
                    break;
                }
            }
            validator
                .finish()
                .map_err(|_| CompatibleCapabilityCertificationError::InvalidResult)
        }
        (OperationKind::TokenCount, ProviderOutput::Result(result))
            if matches!(&*result, CanonicalResult::TokenCount(_)) =>
        {
            Ok(())
        }
        (OperationKind::Embeddings, ProviderOutput::Result(result)) if matches!(&*result, CanonicalResult::Embeddings(value) if !value.data.is_empty() && value.data.iter().all(|item| !item.values.is_empty())) => {
            Ok(())
        }
        (OperationKind::Moderation, ProviderOutput::Result(result)) if matches!(&*result, CanonicalResult::Moderation(value) if !value.results.is_empty()) => {
            Ok(())
        }
        _ => Err(CompatibleCapabilityCertificationError::InvalidResult),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn spec(kind: ProviderKind, auth_mode: ProviderAuthMode) -> ConnectorSpec<'static> {
        ConnectorSpec {
            kind,
            endpoint: None,
            cloud_region: None,
            cloud_project: None,
            deployment: None,
            api_version: None,
            auth_mode,
            probe_model: None,
        }
    }

    #[test]
    fn credential_kind_keeps_text_and_byte_authentication_distinct() {
        assert_eq!(
            raw_credential_kind(spec(ProviderKind::OpenAi, ProviderAuthMode::ApiKey)).unwrap(),
            RawCredentialKind::Text
        );
        assert_eq!(
            raw_credential_kind(spec(
                ProviderKind::VertexAi,
                ProviderAuthMode::ApplicationDefault,
            ))
            .unwrap(),
            RawCredentialKind::None
        );
        assert_eq!(
            raw_credential_kind(spec(
                ProviderKind::VertexAi,
                ProviderAuthMode::ServiceAccount,
            ))
            .unwrap(),
            RawCredentialKind::Text
        );
        assert_eq!(
            raw_credential_kind(spec(ProviderKind::Bedrock, ProviderAuthMode::DefaultChain,))
                .unwrap(),
            RawCredentialKind::None
        );
        assert_eq!(
            raw_credential_kind(spec(ProviderKind::Bedrock, ProviderAuthMode::Static)).unwrap(),
            RawCredentialKind::Bytes
        );
    }

    #[test]
    fn public_factory_covers_every_provider_authentication_pairing() {
        let cases = [
            (
                ProviderConfig::OpenAi { endpoint: None },
                CredentialKind::ApiKey,
            ),
            (
                ProviderConfig::OpenAiCompatible {
                    endpoint: "https://provider.example.test/v1".to_owned(),
                },
                CredentialKind::ApiKey,
            ),
            (
                ProviderConfig::Anthropic {
                    endpoint: None,
                    api_version: None,
                },
                CredentialKind::ApiKey,
            ),
            (
                ProviderConfig::Gemini { endpoint: None },
                CredentialKind::ApiKey,
            ),
            (
                ProviderConfig::VertexAi {
                    project: "project".to_owned(),
                    location: "us-central1".to_owned(),
                    probe_model: "model".to_owned(),
                    auth_mode: ProviderAuthMode::ApplicationDefault,
                },
                CredentialKind::None,
            ),
            (
                ProviderConfig::VertexAi {
                    project: "project".to_owned(),
                    location: "us-central1".to_owned(),
                    probe_model: "model".to_owned(),
                    auth_mode: ProviderAuthMode::ServiceAccount,
                },
                CredentialKind::ServiceAccountJson,
            ),
            (
                ProviderConfig::Bedrock {
                    region: "us-east-1".to_owned(),
                    auth_mode: ProviderAuthMode::DefaultChain,
                },
                CredentialKind::None,
            ),
            (
                ProviderConfig::Bedrock {
                    region: "us-east-1".to_owned(),
                    auth_mode: ProviderAuthMode::Static,
                },
                CredentialKind::AwsStatic,
            ),
            (
                ProviderConfig::AzureOpenAi {
                    endpoint: "https://resource.openai.azure.com".to_owned(),
                    deployment: "deployment".to_owned(),
                    api_version: "2025-04-01-preview".to_owned(),
                },
                CredentialKind::ApiKey,
            ),
        ];

        for (config, expected) in cases {
            assert_eq!(ProviderFactory::credential_kind(&config).unwrap(), expected);
        }
    }

    #[test]
    fn semantic_credentials_are_redacted_and_mismatches_are_rejected() {
        let credential = ProviderCredential::ApiKey(Zeroizing::new("very-secret".to_owned()));
        let debug = format!("{credential:?}");
        assert!(debug.contains("[REDACTED]"));
        assert!(!debug.contains("very-secret"));

        let config = ProviderConfig::Bedrock {
            region: "us-east-1".to_owned(),
            auth_mode: ProviderAuthMode::Static,
        };
        let error = ProviderFactory::validate_credential(&config, &credential).unwrap_err();
        assert_eq!(
            error.to_string(),
            "provider credential does not match its authentication mode"
        );
        assert!(!error.to_string().contains("very-secret"));
    }

    #[test]
    fn certification_matrix_excludes_unprovable_compatible_tuples() {
        assert!(supports_capability_certification(
            ProviderKind::OpenAiCompatible,
            OperationKind::Generation,
            Surface::OpenAi,
            TransportMode::Streaming,
        ));
        assert!(supports_capability_certification(
            ProviderKind::OpenAiCompatible,
            OperationKind::Moderation,
            Surface::OpenAi,
            TransportMode::Unary,
        ));
        assert!(!supports_capability_certification(
            ProviderKind::OpenAiCompatible,
            OperationKind::Generation,
            Surface::Anthropic,
            TransportMode::Unary,
        ));
        assert!(!supports_capability_certification(
            ProviderKind::OpenAiCompatible,
            OperationKind::ImageGeneration,
            Surface::OpenAi,
            TransportMode::Unary,
        ));
        assert!(!supports_capability_certification(
            ProviderKind::AzureOpenAi,
            OperationKind::ImageGeneration,
            Surface::OpenAi,
            TransportMode::Unary,
        ));
    }

    #[test]
    fn discovery_completeness_is_conservative_per_provider_kind() {
        for kind in [
            ProviderKind::OpenAi,
            ProviderKind::Anthropic,
            ProviderKind::Gemini,
        ] {
            assert_eq!(
                model_discovery_completeness(kind),
                ModelDiscoveryCompleteness::Complete,
                "{kind:?}"
            );
        }
        for kind in [
            ProviderKind::OpenAiCompatible,
            ProviderKind::VertexAi,
            ProviderKind::Bedrock,
            ProviderKind::AzureOpenAi,
        ] {
            assert_eq!(
                model_discovery_completeness(kind),
                ModelDiscoveryCompleteness::Partial,
                "{kind:?}"
            );
        }
    }

    #[test]
    fn certifiable_capability_options_are_closed_per_provider_kind() {
        for (kind, expected_count) in [
            (ProviderKind::OpenAi, 25),
            (ProviderKind::OpenAiCompatible, 5),
            (ProviderKind::AzureOpenAi, 11),
            (ProviderKind::Anthropic, 9),
            (ProviderKind::Gemini, 9),
            (ProviderKind::VertexAi, 9),
            (ProviderKind::Bedrock, 9),
        ] {
            let capabilities = certifiable_capabilities(kind).collect::<Vec<_>>();
            assert_eq!(capabilities.len(), expected_count, "{kind:?}");
            assert!(capabilities.iter().all(|(operation, surface, mode)| {
                supports_capability_certification(kind, *operation, *surface, *mode)
            }));
        }
    }

    #[test]
    fn catalog_openai_test_override_is_available_for_native_and_compatible_providers() {
        let registry = OpenAiConnectorOverrideRegistry::default();
        let provider_id = Uuid::from_u128(1);
        registry.register(
            provider_id,
            OpenAiConnector::new(
                OpenAiConnectorConfig::default(),
                OpenAiApiKey::new("sk-test-key").unwrap(),
            ),
        );

        assert!(registry.get(provider_id, ProviderKind::OpenAi).is_some());
        assert!(
            registry
                .get(provider_id, ProviderKind::OpenAiCompatible)
                .is_some()
        );
        assert!(
            registry
                .get(provider_id, ProviderKind::AzureOpenAi)
                .is_none()
        );
    }

    #[test]
    fn bedrock_static_credential_validation_accepts_bytes() {
        let mut spec = spec(ProviderKind::Bedrock, ProviderAuthMode::Static);
        spec.cloud_region = Some("us-east-1");
        assert!(validate_connector_credential(
            spec,
            BorrowedCredential::Bytes(
                br#"{"access_key_id":"ABCDEFGHIJKLMNOP","secret_access_key":"abcdefghijklmnop"}"#,
            ),
        )
        .is_ok());
    }

    struct ExactNativeProbeTransport {
        expected_model: &'static str,
        expected_kind: ProviderKind,
        calls: std::sync::atomic::AtomicUsize,
    }

    impl ProviderTransport for ExactNativeProbeTransport {
        fn execute<'a>(
            &'a self,
            request: ProviderRequest,
        ) -> olp_domain::BoxFuture<'a, Result<ProviderOutput, olp_domain::TransportError>> {
            assert_eq!(request.attempt.provider_model, self.expected_model);
            assert_eq!(request.attempt.provider_kind, self.expected_kind);
            assert_eq!(request.metadata.surface, Surface::Gemini);
            assert_eq!(request.metadata.operation, OperationKind::TokenCount);
            self.calls.fetch_add(1, std::sync::atomic::Ordering::SeqCst);
            Box::pin(async {
                Ok(ProviderOutput::Result(Box::new(
                    CanonicalResult::TokenCount(olp_domain::TokenCountResult {
                        input_tokens: 3,
                        extensions: SourceExtensions::default(),
                    }),
                )))
            })
        }
    }

    #[tokio::test]
    async fn native_certification_executes_the_exact_model_and_tuple() {
        let transport = ExactNativeProbeTransport {
            expected_model: "exact-model-v2",
            expected_kind: ProviderKind::OpenAi,
            calls: std::sync::atomic::AtomicUsize::new(0),
        };
        execute_native_capability_probe(
            &transport,
            ProviderKind::OpenAi,
            "exact-model-v2",
            CompatibleCapability {
                operation: OperationKind::TokenCount,
                surface: Surface::Gemini,
                mode: TransportMode::Unary,
            },
        )
        .await
        .unwrap();
        assert_eq!(transport.calls.load(std::sync::atomic::Ordering::SeqCst), 1);

        assert!(matches!(
            native_probe_operation(
                ProviderKind::Anthropic,
                CompatibleCapability {
                    operation: OperationKind::Embeddings,
                    surface: Surface::OpenAi,
                    mode: TransportMode::Unary,
                },
            ),
            Err(CompatibleCapabilityCertificationError::Unsupported)
        ));
    }
}
