//! Provider-facing infrastructure: AI transports, discovery and certification,
//! OIDC HTTP, and the shared outbound-network security policy.

pub mod anthropic;
#[cfg_attr(not(any(test, feature = "test-util")), allow(dead_code))]
mod azure_openai;
#[cfg_attr(not(any(test, feature = "test-util")), allow(dead_code))]
pub mod bedrock;
pub mod connector;
pub mod endpoint;
pub mod factory;
pub mod gemini;
mod http_egress;
#[cfg(test)]
mod mock_server;
pub mod oidc;
pub mod openai;
mod transport_common;
mod transport_io;
#[cfg_attr(not(any(test, feature = "test-util")), allow(dead_code))]
mod vertex;

#[cfg(any(test, feature = "test-util"))]
pub mod test_support;
