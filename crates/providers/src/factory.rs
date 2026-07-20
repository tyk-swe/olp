mod assembly;
mod certification;
mod configuration;
#[cfg(any(test, feature = "test-util"))]
mod overrides;

pub use crate::openai::{
    CompatibleCapability, CompatibleCapabilityCertificationError, OpenAiConnector,
};
pub use assembly::{ProviderFacade, ProviderFactory};
pub use certification::{
    CapabilityCertificationEvidence, certifiable_capabilities, supports_capability_certification,
};
pub use configuration::{CredentialKind, ProviderConfig, ProviderCredential, ProviderError};
#[cfg(any(test, feature = "test-util"))]
pub use overrides::OpenAiConnectorOverrideRegistry;

#[cfg(test)]
use crate::openai::{ConnectorConfig as OpenAiConnectorConfig, OpenAiApiKey};
#[cfg(test)]
use certification::{execute_native_capability_probe, native_probe_operation};
#[cfg(test)]
use configuration::{
    BorrowedCredential, ConnectorSpec, RawCredentialKind, raw_credential_kind,
    validate_connector_credential,
};
#[cfg(test)]
use olp_domain::{
    CanonicalResult, OperationKind, ProviderAuthMode, ProviderKind, ProviderOutput,
    ProviderRequest, ProviderTransport, SourceExtensions, Surface, TransportMode,
};
#[cfg(test)]
use uuid::Uuid;
#[cfg(test)]
use zeroize::Zeroizing;

#[cfg(test)]
mod tests;
