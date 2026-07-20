//! Provider-facing infrastructure: AI transports, discovery and certification,
//! OIDC HTTP, and the shared outbound-network security policy.

#[cfg(not(any(test, feature = "test-util")))]
mod anthropic;
#[cfg(any(test, feature = "test-util"))]
pub mod anthropic;
mod azure_openai;
mod bedrock;
mod factory;
#[cfg(not(any(test, feature = "test-util")))]
mod gemini;
#[cfg(any(test, feature = "test-util"))]
pub mod gemini;
mod http_egress;
mod oidc;
#[cfg(not(any(test, feature = "test-util")))]
mod openai;
#[cfg(any(test, feature = "test-util"))]
pub mod openai;
mod transport_io;
mod vertex;

pub use anthropic::validate_operation as validate_anthropic_operation;
pub use bedrock::validate_operation as validate_bedrock_operation;
pub use factory::*;
pub use gemini::validate_operation as validate_gemini_operation;
pub use oidc::{OidcNetworkError, OidcNetworkPolicy};
