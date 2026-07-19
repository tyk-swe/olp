//! Provider-facing infrastructure: AI transports, discovery and certification,
//! OIDC HTTP, and the shared outbound-network security policy.

#[cfg(not(any(test, feature = "test-util")))]
#[allow(dead_code)]
mod anthropic;
#[cfg(any(test, feature = "test-util"))]
pub mod anthropic;
#[allow(dead_code)]
mod azure_openai;
#[allow(dead_code)]
mod bedrock;
mod factory;
#[cfg(not(any(test, feature = "test-util")))]
#[allow(dead_code)]
mod gemini;
#[cfg(any(test, feature = "test-util"))]
pub mod gemini;
mod http_egress;
mod oidc;
#[cfg(not(any(test, feature = "test-util")))]
#[allow(dead_code)]
mod openai;
#[cfg(any(test, feature = "test-util"))]
pub mod openai;
#[allow(dead_code)]
mod vertex;

pub use anthropic::AnthropicConnector;
pub use anthropic::validate_operation as validate_anthropic_operation;
pub use bedrock::validate_operation as validate_bedrock_operation;
pub use factory::*;
pub use gemini::validate_operation as validate_gemini_operation;
pub use oidc::{OidcNetworkError, OidcNetworkPolicy};
