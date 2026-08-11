//! Deterministic local connector assembly for the provider conformance crate.
//!
//! This module is available only with `test-util`. It keeps emulator endpoint
//! overrides and fixed fake credentials out of production configuration while
//! exercising the same connector objects and factory assembly used at runtime.

use std::{fmt, sync::Arc};

use crate::domain::{BoxFuture, ProviderKind};
use zeroize::Zeroizing;

use crate::providers::{
    ProviderFacade, ProviderFactory,
    bedrock::{
        BedrockConnector, BedrockCredentials, ConnectorConfig as BedrockConnectorConfig,
        StaticCredentials,
    },
    factory::{ProviderConfig, ProviderCredential},
    gemini::{BearerTokenError, BearerTokenProvider, SecretBearerToken},
    vertex::{ConnectorConfig as VertexConnectorConfig, VertexConnector},
};

pub const API_KEY: &str = "olp-conformance-secret";
pub const VERTEX_TOKEN: &str = "olp-conformance-vertex-token";
pub const BEDROCK_ACCESS_KEY: &str = "AKIAOLPCONFORMANCE";
pub const BEDROCK_SECRET_KEY: &str = "olp-conformance-secret-key";

#[derive(Debug)]
struct StaticToken;

impl BearerTokenProvider for StaticToken {
    fn token<'a>(&'a self) -> BoxFuture<'a, Result<SecretBearerToken, BearerTokenError>> {
        Box::pin(async { SecretBearerToken::new(VERTEX_TOKEN) })
    }
}

/// Builds a real connector against one loopback emulator origin.
///
/// HTTP API-key connectors pass through [`ProviderFactory`]. Vertex and
/// Bedrock use their existing cloud-emulator seams because their production
/// configuration deliberately has no user-selectable endpoint field.
pub async fn local_provider(
    kind: ProviderKind,
    origin: &str,
) -> Result<LocalProvider, LocalProviderError> {
    let facade = match kind {
        ProviderKind::OpenAi => {
            factory_provider(ProviderConfig::OpenAi {
                endpoint: Some(format!("{origin}/v1")),
            })
            .await?
        }
        ProviderKind::OpenAiCompatible => {
            factory_provider(ProviderConfig::OpenAiCompatible {
                endpoint: format!("{origin}/v1"),
            })
            .await?
        }
        ProviderKind::Anthropic => {
            factory_provider(ProviderConfig::Anthropic {
                endpoint: Some(format!("{origin}/v1/")),
                api_version: None,
            })
            .await?
        }
        ProviderKind::Gemini => {
            factory_provider(ProviderConfig::Gemini {
                endpoint: Some(format!("{origin}/v1beta/")),
            })
            .await?
        }
        ProviderKind::AzureOpenAi => {
            factory_provider(ProviderConfig::AzureOpenAi {
                endpoint: origin.to_owned(),
                deployment: "conformance-deployment".to_owned(),
                api_version: "2024-10-21".to_owned(),
            })
            .await?
        }
        ProviderKind::VertexAi => {
            let base = format!(
                "{origin}/v1/projects/conformance-project/locations/us-central1/publishers/google/"
            );
            let config = VertexConnectorConfig::for_local_test(
                "conformance-project",
                "us-central1",
                "conformance-model",
                &base,
                crate::providers::gemini::ConnectorTimeouts::default(),
            );
            ProviderFacade::from_local_vertex(VertexConnector::with_token_provider(
                config,
                Arc::new(StaticToken),
            ))
        }
        ProviderKind::Bedrock => {
            let config = BedrockConnectorConfig::new("us-east-1")
                .and_then(|config| {
                    config.with_timeouts(crate::providers::bedrock::ConnectorTimeouts::default())
                })
                .and_then(|config| config.with_endpoint_url(origin))
                .map_err(|error| LocalProviderError(error.to_string()))?;
            let document = format!(
                r#"{{"access_key_id":"{BEDROCK_ACCESS_KEY}","secret_access_key":"{BEDROCK_SECRET_KEY}"}}"#
            );
            let credentials = StaticCredentials::from_json(document)
                .map_err(|error| LocalProviderError(error.to_string()))?;
            ProviderFacade::from_local_bedrock(
                BedrockConnector::new(config, BedrockCredentials::Static(credentials)).await,
            )
        }
    };
    Ok(LocalProvider(facade))
}

async fn factory_provider(config: ProviderConfig) -> Result<ProviderFacade, LocalProviderError> {
    ProviderFactory::create_with_unsafe_test_endpoints(
        config,
        ProviderCredential::ApiKey(Zeroizing::new(API_KEY.to_owned())),
    )
    .await
    .map_err(|error| LocalProviderError(error.to_string()))
}

pub struct LocalProvider(ProviderFacade);

impl LocalProvider {
    #[must_use]
    pub const fn facade(&self) -> &ProviderFacade {
        &self.0
    }

    #[must_use]
    pub fn into_transport(self) -> Arc<dyn crate::domain::ProviderTransport> {
        self.0.into_transport()
    }
}

impl fmt::Debug for LocalProvider {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str("LocalProvider([REDACTED])")
    }
}

#[derive(Debug, thiserror::Error)]
#[error("local provider assembly failed: {0}")]
pub struct LocalProviderError(String);
