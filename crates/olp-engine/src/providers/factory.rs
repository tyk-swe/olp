mod assembly;
mod certification;
mod configuration;
mod overrides;

pub use crate::providers::openai::{
    CompatibleCapability, CompatibleCapabilityCertificationError, OpenAiConnector,
};
pub use assembly::{ProviderFacade, ProviderFactory};
pub use certification::{
    CapabilityCertificationEvidence, certifiable_capabilities, supports_capability_certification,
};
pub use configuration::{CredentialKind, ProviderConfig, ProviderCredential, ProviderError};
pub use overrides::OpenAiConnectorOverrideRegistry;

#[cfg(test)]
use crate::domain::{
    CanonicalResult, OperationKind, ProviderAuthMode, ProviderKind, ProviderOutput,
    ProviderRequest, ProviderTransport, SourceExtensions, Surface, TransportMode,
};
#[cfg(test)]
use crate::providers::openai::{ConnectorConfig as OpenAiConnectorConfig, OpenAiApiKey};
#[cfg(test)]
use certification::{execute_native_capability_probe, native_probe_operation};
#[cfg(test)]
use configuration::{
    BorrowedCredential, ConnectorSpec, RawCredentialKind, raw_credential_kind,
    validate_connector_credential,
};
#[cfg(test)]
use uuid::Uuid;
#[cfg(test)]
use zeroize::Zeroizing;

#[cfg(test)]
mod tests;
