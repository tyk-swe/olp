//! Provider-facing infrastructure: AI transports, discovery and certification,
//! OIDC HTTP, and the shared outbound-network security policy.

pub mod anthropic;
mod azure_openai;
pub mod bedrock;
pub mod connector;
pub mod endpoint;
pub mod factory;
pub mod gemini;
pub mod http_egress;
#[cfg(test)]
mod mock_server;
pub mod oidc;
pub mod openai;
mod transport_common;
mod transport_io;
mod vertex;

#[cfg(any(test, feature = "test-util"))]
pub mod test_support;
