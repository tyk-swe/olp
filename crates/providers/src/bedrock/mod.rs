//! Native Amazon Bedrock connector.
//!
//! AWS dependencies intentionally live in this crate only. Inference uses the
//! official Bedrock Runtime SDK, which owns SigV4 signing and AWS event-stream
//! framing. Model discovery uses the official Bedrock control-plane SDK.

mod translate;
mod transport;

use std::{fmt, time::Duration};

use aws_config::{BehaviorVersion, Region, retry::RetryConfig, timeout::TimeoutConfig};
use aws_credential_types::Credentials;
use olp_domain::{Operation, TransportError};
use serde::Deserialize;
use thiserror::Error;
use zeroize::Zeroize;

pub use transport::BedrockConnector;

/// Validates that a canonical operation can be encoded by Bedrock without
/// making an AWS request or loading credentials. The gateway uses this before
/// transport selection so source-protocol semantics fail closed as a client
/// error instead of reaching an upstream connector.
pub fn validate_operation(operation: &Operation) -> Result<(), TransportError> {
    match operation {
        Operation::Generation(request) => translate::encode_generation(request).map(|_| ()),
        Operation::TokenCount(request) => translate::encode_token_count(request).map(|_| ()),
        _ => Err(olp_domain::TransportError {
            phase: olp_domain::TransportPhase::Connect,
            class: olp_domain::AttemptFailureClass::Protocol,
            response_committed: false,
            message: "Bedrock does not represent this canonical operation".to_owned(),
        }),
    }
}

const DEFAULT_CONNECT_TIMEOUT: Duration = Duration::from_secs(5);
const DEFAULT_FIRST_BYTE_TIMEOUT: Duration = Duration::from_secs(30);
const DEFAULT_IDLE_TIMEOUT: Duration = Duration::from_secs(60);
const MAX_STATIC_CREDENTIAL_DOCUMENT_BYTES: usize = 16 * 1_024;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct ConnectorTimeouts {
    /// DNS/TCP/TLS connection bound applied by the AWS SDK HTTP client.
    pub connect: Duration,
    /// Response-setup bound for streaming calls. Buffered unary SDK calls do
    /// not expose a separate first-body-byte phase and use the total attempt.
    pub first_byte: Duration,
    /// Resetting event idle bound for streams and SDK socket-read bound for
    /// buffered unary calls.
    pub idle: Duration,
}

impl Default for ConnectorTimeouts {
    fn default() -> Self {
        Self {
            connect: DEFAULT_CONNECT_TIMEOUT,
            first_byte: DEFAULT_FIRST_BYTE_TIMEOUT,
            idle: DEFAULT_IDLE_TIMEOUT,
        }
    }
}

#[derive(Clone, Debug)]
pub struct ConnectorConfig {
    region: String,
    timeouts: ConnectorTimeouts,
    endpoint_url: Option<String>,
}

impl ConnectorConfig {
    pub fn new(region: impl Into<String>) -> Result<Self, ConfigError> {
        let region = region.into();
        validate_region(&region)?;
        Ok(Self {
            region,
            timeouts: ConnectorTimeouts::default(),
            endpoint_url: None,
        })
    }

    #[cfg(test)]
    pub fn with_timeouts(mut self, timeouts: ConnectorTimeouts) -> Result<Self, ConfigError> {
        validate_timeouts(timeouts)?;
        self.timeouts = timeouts;
        Ok(self)
    }

    /// Overrides both Bedrock endpoints for an isolated local emulator.
    ///
    /// Production provider drafts do not expose this setting. It exists only
    /// for connector tests.
    #[cfg(test)]
    pub fn with_endpoint_url(
        mut self,
        endpoint_url: impl Into<String>,
    ) -> Result<Self, ConfigError> {
        let endpoint_url = endpoint_url.into();
        let parsed = endpoint_url
            .parse::<http::Uri>()
            .map_err(|_| ConfigError::InvalidEndpoint)?;
        if parsed.scheme_str() != Some("http") && parsed.scheme_str() != Some("https") {
            return Err(ConfigError::InvalidEndpoint);
        }
        if parsed.authority().is_none() {
            return Err(ConfigError::InvalidEndpoint);
        }
        self.endpoint_url = Some(endpoint_url);
        Ok(self)
    }
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct StaticCredentialDocument {
    access_key_id: String,
    secret_access_key: String,
    session_token: Option<String>,
}

impl Drop for StaticCredentialDocument {
    fn drop(&mut self) {
        self.access_key_id.zeroize();
        self.secret_access_key.zeroize();
        self.session_token.zeroize();
    }
}

/// Credential selection for an installed Bedrock provider.
pub enum BedrockCredentials {
    /// AWS environment/shared-profile/web-identity/container/instance chain.
    DefaultChain,
    /// An encrypted-at-rest JSON document decoded only while installing the
    /// runtime connector.
    Static(StaticCredentials),
}

impl fmt::Debug for BedrockCredentials {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::DefaultChain => formatter.write_str("BedrockCredentials::DefaultChain"),
            Self::Static(_) => formatter.write_str("BedrockCredentials::Static([REDACTED])"),
        }
    }
}

pub struct StaticCredentials {
    access_key_id: String,
    secret_access_key: String,
    session_token: Option<String>,
}

impl StaticCredentials {
    pub fn from_json(value: impl AsRef<[u8]>) -> Result<Self, CredentialError> {
        let value = value.as_ref();
        if value.len() > MAX_STATIC_CREDENTIAL_DOCUMENT_BYTES {
            return Err(CredentialError::InvalidDocument);
        }
        let document: StaticCredentialDocument =
            serde_json::from_slice(value).map_err(|_| CredentialError::InvalidDocument)?;
        validate_static_component(&document.access_key_id, 16, 256)?;
        validate_static_component(&document.secret_access_key, 16, 1_024)?;
        if let Some(token) = &document.session_token {
            validate_static_component(token, 1, 8_192)?;
        }
        Ok(Self {
            access_key_id: document.access_key_id.clone(),
            secret_access_key: document.secret_access_key.clone(),
            session_token: document.session_token.clone(),
        })
    }

    fn into_sdk(self) -> Credentials {
        Credentials::new(
            self.access_key_id.clone(),
            self.secret_access_key.clone(),
            self.session_token.clone(),
            None,
            "olp-bedrock-static",
        )
    }
}

impl Drop for StaticCredentials {
    fn drop(&mut self) {
        self.access_key_id.zeroize();
        self.secret_access_key.zeroize();
        self.session_token.zeroize();
    }
}

impl fmt::Debug for StaticCredentials {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str("StaticCredentials([REDACTED])")
    }
}

#[derive(Clone, Copy, Debug, Error, Eq, PartialEq)]
pub enum ConfigError {
    #[error("AWS region is invalid")]
    InvalidRegion,
    #[cfg(test)]
    #[error("connector timeouts must all be greater than zero")]
    ZeroTimeout,
    #[cfg(test)]
    #[error("connector endpoint override is invalid")]
    InvalidEndpoint,
}

#[derive(Clone, Copy, Debug, Error, Eq, PartialEq)]
pub enum CredentialError {
    #[error("static AWS credentials must be a bounded JSON object")]
    InvalidDocument,
    #[error("static AWS credential component is invalid")]
    InvalidComponent,
}

fn validate_region(region: &str) -> Result<(), ConfigError> {
    if region.is_empty()
        || region.len() > 64
        || region.starts_with('-')
        || region.ends_with('-')
        || region
            .bytes()
            .any(|byte| !byte.is_ascii_lowercase() && !byte.is_ascii_digit() && byte != b'-')
    {
        return Err(ConfigError::InvalidRegion);
    }
    Ok(())
}

#[cfg(test)]
fn validate_timeouts(timeouts: ConnectorTimeouts) -> Result<(), ConfigError> {
    if timeouts.connect.is_zero() || timeouts.first_byte.is_zero() || timeouts.idle.is_zero() {
        return Err(ConfigError::ZeroTimeout);
    }
    Ok(())
}

fn validate_static_component(
    value: &str,
    minimum: usize,
    maximum: usize,
) -> Result<(), CredentialError> {
    if value.len() < minimum
        || value.len() > maximum
        || value.chars().any(char::is_control)
        || value.trim() != value
    {
        return Err(CredentialError::InvalidComponent);
    }
    Ok(())
}

async fn sdk_config(
    config: &ConnectorConfig,
    credentials: BedrockCredentials,
) -> aws_config::SdkConfig {
    let retry = RetryConfig::standard().with_max_attempts(1);
    let timeout = TimeoutConfig::builder()
        .connect_timeout(config.timeouts.connect)
        .read_timeout(config.timeouts.idle)
        .build();
    let loader = aws_config::defaults(BehaviorVersion::latest())
        .region(Region::new(config.region.clone()))
        .retry_config(retry)
        .timeout_config(timeout);
    match credentials {
        BedrockCredentials::DefaultChain => loader.load().await,
        BedrockCredentials::Static(credentials) => {
            loader
                .credentials_provider(credentials.into_sdk())
                .load()
                .await
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn static_credentials_are_strict_and_redacted() {
        let credentials = StaticCredentials::from_json(
            br#"{"access_key_id":"AKIAEXAMPLEVALUE","secret_access_key":"secret-secret-secret","session_token":"token"}"#,
        )
        .unwrap();
        assert_eq!(format!("{credentials:?}"), "StaticCredentials([REDACTED])");
        assert!(
            StaticCredentials::from_json(
                br#"{"access_key_id":"short","secret_access_key":"also-short"}"#
            )
            .is_err()
        );
        assert!(StaticCredentials::from_json(br#"{"access_key_id":"AKIAEXAMPLEVALUE","secret_access_key":"secret-secret-secret","extra":true}"#).is_err());
        assert!(
            StaticCredentials::from_json(vec![b' '; MAX_STATIC_CREDENTIAL_DOCUMENT_BYTES + 1])
                .is_err()
        );
    }

    #[test]
    fn validates_region_and_deadlines() {
        assert!(ConnectorConfig::new("us-east-1").is_ok());
        assert!(ConnectorConfig::new("https://region").is_err());
        assert!(
            ConnectorConfig::new("us-east-1")
                .unwrap()
                .with_timeouts(ConnectorTimeouts {
                    connect: Duration::ZERO,
                    ..ConnectorTimeouts::default()
                })
                .is_err()
        );
    }
}
