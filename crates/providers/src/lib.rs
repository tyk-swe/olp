//! Provider-facing infrastructure: AI transports, discovery and certification,
//! OIDC HTTP, and the shared outbound-network security policy.

pub mod anthropic;
#[cfg_attr(not(any(test, feature = "test-util")), allow(dead_code))]
mod azure_openai;
#[cfg_attr(not(any(test, feature = "test-util")), allow(dead_code))]
mod bedrock;
mod connector;
mod endpoint;
mod factory;
pub mod gemini;
mod http_egress;
#[cfg(test)]
mod mock_server;
mod oidc;
pub mod openai;
mod transport_common;
mod transport_io;
#[cfg_attr(not(any(test, feature = "test-util")), allow(dead_code))]
mod vertex;

#[cfg(any(test, feature = "test-util"))]
pub mod test_support;

pub use anthropic::validate_operation as validate_anthropic_operation;
pub use bedrock::validate_operation as validate_bedrock_operation;
pub use connector::ConnectorTimeouts;
pub use endpoint::EndpointError as CommonEndpointError;
pub use factory::{
    CapabilityCertificationEvidence, CompatibleCapability, CompatibleCapabilityCertificationError,
    CredentialKind, OpenAiConnector, OpenAiConnectorOverrideRegistry, ProviderConfig,
    ProviderCredential, ProviderError, ProviderFacade, ProviderFactory, certifiable_capabilities,
    supports_capability_certification,
};
pub use gemini::validate_operation as validate_gemini_operation;
pub use oidc::{OidcNetworkError, OidcNetworkPolicy};
