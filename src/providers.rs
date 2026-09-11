//! Provider-facing infrastructure: AI transports, discovery and certification,
//! OIDC HTTP, and the shared outbound-network security policy.

pub mod anthropic;

pub mod azure_openai;

pub mod bedrock;

pub mod connect;

pub mod connector;

pub mod credentials;

pub mod endpoint;

pub mod error;

pub mod connectors;

pub mod gemini;

pub mod http;

pub mod lifecycle;

#[cfg(test)]
pub mod mock_server;

pub mod models;

pub mod mounted;

pub mod openai;

pub mod queries;

#[cfg(test)]
pub mod record_tests;

pub mod record_validation;

pub mod records;

pub mod repository;

pub mod revisions;

pub mod runtime;

pub mod runtime_config;

pub mod runtime_model;

#[cfg(any(test, feature = "test-util"))]
pub mod test_support;

pub mod transport_common;

pub mod transport_io;

pub mod types;

pub mod validation;

pub mod vertex;

pub mod configuration;

pub mod catalog;
pub mod options;

pub(crate) mod http_options;

pub mod pool;

pub mod pool_store;

pub mod pool_transport;

pub(crate) mod profiles;

#[cfg(test)]
mod profiles_tests;
