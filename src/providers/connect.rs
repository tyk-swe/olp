use crate::crypto::aad::credential;
use crate::http::control::error_mapping::map_configuration;
use crate::http::control::state::ManagementState;
use crate::http::problem::Problem;
use crate::providers::connectors::ProviderConnector;
use crate::providers::connectors::configuration::CredentialKind;
use crate::providers::connectors::input::provider_credential;
use uuid::Uuid;
pub(crate) async fn provider_connector(
    state: &ManagementState,
    provider_id: Uuid,
) -> Result<ProviderConnector, Problem> {
    let pool = &state.request_boundary.pool;
    let provider = crate::providers::repository::get_provider(pool, provider_id)
        .await
        .map_err(map_configuration)?;
    let config = provider.configuration;
    #[cfg(any(test, feature = "test-util"))]
    if let Some(connector) = state.certification_probe_connector(provider_id, config.kind) {
        return Ok(connector);
    }
    let plaintext = match crate::providers::connectors::configuration::credential_kind(&config)
        .map_err(|error| Problem::field_validation("provider", error.to_string()))?
    {
        CredentialKind::None => None,
        CredentialKind::ApiKey | CredentialKind::ServiceAccountJson | CredentialKind::AwsStatic => {
            let stored =
                crate::providers::credentials::active_provider_credential_secret(pool, provider_id)
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
    crate::providers::connectors::create(
        config,
        credential,
        &state.provider_egress_policy,
        state.provider_response_limits,
    )
    .await
    .map_err(|error| Problem::field_validation("provider", error.to_string()))
}
