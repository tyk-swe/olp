use super::{error_mapping::map_configuration, state::ManagementState};
use crate::{application::provider_fields::ProviderConfigFields, public_http::problem::Problem};
use olp_db::security::aad::credential;
use olp_engine::providers::factory::{
    assembly::{Facade, Factory},
    configuration::CredentialKind,
    input::{provider_config, provider_credential},
};
use uuid::Uuid;
pub(crate) async fn provider_connector(
    state: &ManagementState,
    provider_id: Uuid,
) -> Result<Facade, Problem> {
    let store = state.store();
    let provider = store
        .get_provider(provider_id)
        .await
        .map_err(map_configuration)?;
    let config = provider_config(ProviderConfigFields::from(&provider).into())
        .map_err(|error| Problem::field_validation("provider", error.to_string()))?;
    #[cfg(any(test, feature = "test-util"))]
    if let Some(connector) = state.certification_probe_connector(provider_id, config.kind()) {
        return Ok(connector);
    }
    let plaintext = match Factory::credential_kind(&config)
        .map_err(|error| Problem::field_validation("provider", error.to_string()))?
    {
        CredentialKind::None => None,
        CredentialKind::ApiKey | CredentialKind::ServiceAccountJson | CredentialKind::AwsStatic => {
            let stored = store
                .active_provider_credential_secret(provider_id)
                .await
                .map_err(map_configuration)?;
            let master_key = state
                .master_key
                .as_ref()
                .ok_or_else(|| Problem::service_unavailable("master_key_not_configured"))?;
            Some(
                master_key
                    .open(
                        &stored.encrypted,
                        &credential(provider_id, stored.id, stored.version),
                    )
                    .map_err(|error| {
                        tracing::error!(%error, provider_id = %provider_id, "provider credential decryption failed");
                        Problem::internal()
                    })?,
            )
        }
    };
    let credential = provider_credential(
        &config,
        plaintext.as_ref().map(|plaintext| plaintext.as_slice()),
    )
    .map_err(|error| Problem::field_validation("provider", error.to_string()))?;
    Factory::create(
        config,
        credential,
        &state.provider_egress_policy,
        state.provider_response_limits,
    )
    .await
    .map_err(|error| Problem::field_validation("provider", error.to_string()))
}
