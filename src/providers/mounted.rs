use std::path::{Path, PathBuf};

use crate::crypto::secret_files::check_secret_permissions;
use crate::ids::ProviderId;
use crate::net::egress::EgressPolicy;
use crate::process::error::AppResult;
use crate::providers::configuration::ProviderConfiguration;
use crate::providers::connector::ResponseLimits;
use crate::providers::connectors::input::provider_credential;
use crate::runtime::transports::TransportRegistry;
use serde::Deserialize;
use zeroize::Zeroizing;

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct MountedConnectorConfig {
    providers: Vec<MountedConnector>,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct MountedConnector {
    provider_id: uuid::Uuid,
    configuration: ProviderConfiguration,
    model: Option<String>,
    credential_file: Option<PathBuf>,
}

pub(crate) async fn register_mounted_connectors(
    path: &Path,
    registry: &TransportRegistry,
    egress_policy: &EgressPolicy,
    response_limits: ResponseLimits,
) -> AppResult<()> {
    let config: MountedConnectorConfig = serde_json::from_slice(&tokio::fs::read(path).await?)?;
    for provider in config.providers {
        let mut configuration = provider.configuration;
        configuration.probe_model = provider.model;
        if let Some(violation) = crate::providers::validation::validate(
            &configuration,
            Some(provider.credential_file.is_some()),
        )
        .first()
        {
            return Err(std::io::Error::other(violation.detail).into());
        }
        let plaintext = if let Some(path) = provider.credential_file {
            check_secret_permissions(&path).await?;
            Some(Zeroizing::new(tokio::fs::read_to_string(path).await?))
        } else {
            None
        };
        let credential = provider_credential(
            &configuration,
            plaintext.as_ref().map(|secret| secret.trim().as_bytes()),
        )?;
        let transport = crate::providers::connectors::transport(
            configuration,
            credential,
            egress_policy,
            response_limits,
        )
        .await?;
        registry.register(ProviderId::from_uuid(provider.provider_id), transport);
    }
    Ok(())
}

#[cfg(test)]
mod tests;
