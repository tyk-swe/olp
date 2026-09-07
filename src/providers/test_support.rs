//! Deterministic local connector assembly for the provider conformance suite.
//!
//! This module is available only with `test-util`. It keeps emulator endpoint
//! overrides and fixed fake credentials out of production configuration while
//! exercising the same connector construction used at runtime.

use std::sync::Arc;

use crate::inference::transport::BoxFuture;
use crate::providers::runtime_model::ProviderKind;
use zeroize::Zeroizing;

use crate::net::egress::EgressPolicy;
use crate::providers::bedrock::ConnectorConfig as BedrockConnectorConfig;
use crate::providers::bedrock::Credentials;
use crate::providers::bedrock::StaticCredentials;
use crate::providers::bedrock::transport::Connector as BedrockConnector;
use crate::providers::configuration::ProviderConfiguration;
use crate::providers::connectors::ProviderConnector;
use crate::providers::connectors::configuration::Credential;
use crate::providers::gemini::BearerTokenError;
use crate::providers::gemini::BearerTokenProvider;
use crate::providers::gemini::SecretBearerToken;
use crate::providers::vertex::Connector as VertexConnector;
use crate::providers::vertex::ConnectorConfig as VertexConnectorConfig;

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
/// HTTP API-key connectors use shared configuration. Vertex and
/// Bedrock use their existing cloud-emulator seams because their production
/// configuration deliberately has no user-selectable endpoint field.
pub async fn local_provider(
    kind: ProviderKind,
    origin: &str,
) -> Result<ProviderConnector, LocalProviderError> {
    let connector = match kind {
        ProviderKind::OpenAi => {
            api_key_connector(ProviderConfiguration {
                kind: crate::providers::runtime_model::ProviderKind::OpenAi,
                endpoint: Some(format!("{origin}/v1")),
                ..ProviderConfiguration::new(crate::providers::runtime_model::ProviderKind::OpenAi)
            })
            .await?
        }
        ProviderKind::OpenAiCompatible => {
            api_key_connector(ProviderConfiguration {
                kind: crate::providers::runtime_model::ProviderKind::OpenAiCompatible,
                endpoint: Some(format!("{origin}/v1")),
                ..ProviderConfiguration::new(
                    crate::providers::runtime_model::ProviderKind::OpenAiCompatible,
                )
            })
            .await?
        }
        ProviderKind::Anthropic => {
            api_key_connector(ProviderConfiguration {
                kind: crate::providers::runtime_model::ProviderKind::Anthropic,
                endpoint: Some(format!("{origin}/v1/")),
                api_version: None,
                ..ProviderConfiguration::new(
                    crate::providers::runtime_model::ProviderKind::Anthropic,
                )
            })
            .await?
        }
        ProviderKind::Gemini => {
            api_key_connector(ProviderConfiguration {
                kind: crate::providers::runtime_model::ProviderKind::Gemini,
                endpoint: Some(format!("{origin}/v1beta/")),
                ..ProviderConfiguration::new(crate::providers::runtime_model::ProviderKind::Gemini)
            })
            .await?
        }
        ProviderKind::AzureOpenAi => {
            api_key_connector(ProviderConfiguration {
                kind: crate::providers::runtime_model::ProviderKind::AzureOpenAi,
                endpoint: Some(origin.to_owned()),
                deployment: Some("conformance-deployment".to_owned()),
                api_version: Some("2024-10-21".to_owned()),
                ..ProviderConfiguration::new(
                    crate::providers::runtime_model::ProviderKind::AzureOpenAi,
                )
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
                crate::providers::connector::Timeouts::default(),
            );
            ProviderConnector::from_local_vertex(VertexConnector::with_token_provider(
                config,
                Arc::new(StaticToken),
            ))
        }
        ProviderKind::Bedrock => {
            let config = BedrockConnectorConfig::new("us-east-1")
                .and_then(|config| {
                    config.with_timeouts(crate::providers::connector::Timeouts::default())
                })
                .and_then(|config| config.with_endpoint_url(origin))
                .map_err(|error| LocalProviderError(error.to_string()))?;
            let document = format!(
                r#"{{"access_key_id":"{BEDROCK_ACCESS_KEY}","secret_access_key":"{BEDROCK_SECRET_KEY}"}}"#
            );
            let credentials = StaticCredentials::from_json(document)
                .map_err(|error| LocalProviderError(error.to_string()))?;
            ProviderConnector::from_local_bedrock(
                BedrockConnector::new(config, Credentials::Static(credentials)).await,
            )
        }
    };
    Ok(connector)
}

async fn api_key_connector(
    config: ProviderConfiguration,
) -> Result<ProviderConnector, LocalProviderError> {
    crate::providers::connectors::create(
        config,
        Credential::ApiKey(Zeroizing::new(API_KEY.to_owned())),
        &EgressPolicy::unsafe_test_targets(),
        crate::providers::connector::ResponseLimits::default(),
    )
    .await
    .map_err(|error| LocalProviderError(error.to_string()))
}

#[derive(Debug, thiserror::Error)]
#[error("local provider assembly failed: {0}")]
pub struct LocalProviderError(String);
