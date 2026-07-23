use olp_domain::{
    ProviderAuthMode, ProviderConfiguration, ProviderId, ProviderKind, RuntimeSnapshot,
    validate_provider_configuration,
};
use olp_providers::{
    CredentialKind, ProviderConfig, ProviderCredential, ProviderError, ProviderFacade,
    ProviderFactory,
};
use olp_storage::{MasterKey, ProviderRecord, RuntimeProviderConfiguration, credential_aad};
use uuid::Uuid;
use zeroize::Zeroizing;

use crate::{
    ManagementState, Problem,
    cli::AppResult,
    management_api::{map_configuration_resource, validation},
};

/// Application-owned provider fields before they cross into `olp-providers`.
#[derive(Clone, Copy, Debug)]
pub(crate) struct ProviderConfigFields<'a> {
    pub kind: ProviderKind,
    pub endpoint: Option<&'a str>,
    pub cloud_region: Option<&'a str>,
    pub cloud_project: Option<&'a str>,
    pub deployment: Option<&'a str>,
    pub api_version: Option<&'a str>,
    pub auth_mode: ProviderAuthMode,
    pub probe_model: Option<&'a str>,
}

impl<'a> From<&'a ProviderRecord> for ProviderConfigFields<'a> {
    fn from(provider: &'a ProviderRecord) -> Self {
        Self {
            kind: provider.kind,
            endpoint: provider.endpoint.as_deref(),
            cloud_region: provider.cloud_region.as_deref(),
            cloud_project: provider.cloud_project.as_deref(),
            deployment: provider.deployment.as_deref(),
            api_version: provider.api_version.as_deref(),
            auth_mode: provider.auth_mode,
            probe_model: provider.probe_model.as_deref(),
        }
    }
}

impl<'a> From<&'a RuntimeProviderConfiguration> for ProviderConfigFields<'a> {
    fn from(provider: &'a RuntimeProviderConfiguration) -> Self {
        Self {
            kind: provider.kind,
            endpoint: provider.endpoint.as_deref(),
            cloud_region: provider.cloud_region.as_deref(),
            cloud_project: provider.cloud_project.as_deref(),
            deployment: provider.deployment.as_deref(),
            api_version: provider.api_version.as_deref(),
            auth_mode: provider.auth_mode,
            probe_model: None,
        }
    }
}

pub(crate) fn provider_config(
    fields: ProviderConfigFields<'_>,
) -> Result<ProviderConfig, ProviderError> {
    if let Some(violation) = validate_provider_configuration(ProviderConfiguration {
        kind: fields.kind,
        auth_mode: fields.auth_mode,
        endpoint: fields.endpoint,
        cloud_region: fields.cloud_region,
        cloud_project: fields.cloud_project,
        deployment: fields.deployment,
        api_version: fields.api_version,
        model: fields.probe_model,
        credential_present: None,
    })
    .into_iter()
    .next()
    {
        return Err(ProviderError::Configuration(violation.detail.to_owned()));
    }

    let required = |value: Option<&str>, message: &'static str| {
        value
            .map(str::to_owned)
            .ok_or_else(|| ProviderError::Configuration(message.to_owned()))
    };

    Ok(match fields.kind {
        ProviderKind::OpenAi => ProviderConfig::OpenAi {
            endpoint: fields.endpoint.map(str::to_owned),
        },
        ProviderKind::OpenAiCompatible => ProviderConfig::OpenAiCompatible {
            endpoint: required(fields.endpoint, "OpenAI-compatible endpoint is missing")?,
        },
        ProviderKind::Anthropic => ProviderConfig::Anthropic {
            endpoint: fields.endpoint.map(str::to_owned),
            api_version: fields.api_version.map(str::to_owned),
        },
        ProviderKind::Gemini => ProviderConfig::Gemini {
            endpoint: fields.endpoint.map(str::to_owned),
        },
        ProviderKind::VertexAi => ProviderConfig::VertexAi {
            project: required(fields.cloud_project, "Vertex AI project is missing")?,
            location: required(fields.cloud_region, "Vertex AI location is missing")?,
            probe_model: required(fields.probe_model, "Vertex AI probe model is missing")?,
            auth_mode: fields.auth_mode,
        },
        ProviderKind::Bedrock => ProviderConfig::Bedrock {
            region: required(fields.cloud_region, "Bedrock AWS region is missing")?,
            auth_mode: fields.auth_mode,
        },
        ProviderKind::AzureOpenAi => ProviderConfig::AzureOpenAi {
            endpoint: required(fields.endpoint, "Azure OpenAI endpoint is missing")?,
            deployment: required(fields.deployment, "Azure OpenAI deployment is missing")?,
            api_version: required(fields.api_version, "Azure OpenAI API version is missing")?,
        },
    })
}

pub(crate) fn provider_credential(
    config: &ProviderConfig,
    plaintext: Option<&[u8]>,
) -> Result<ProviderCredential, ProviderError> {
    match (ProviderFactory::credential_kind(config)?, plaintext) {
        (CredentialKind::None, _) | (_, None) => Ok(ProviderCredential::None),
        (CredentialKind::ApiKey, Some(plaintext)) => Ok(ProviderCredential::ApiKey(
            Zeroizing::new(secret_text(plaintext)?.to_owned()),
        )),
        (CredentialKind::ServiceAccountJson, Some(plaintext)) => {
            Ok(ProviderCredential::ServiceAccountJson(Zeroizing::new(
                secret_text(plaintext)?.to_owned(),
            )))
        }
        (CredentialKind::AwsStatic, Some(plaintext)) => Ok(ProviderCredential::AwsStatic(
            Zeroizing::new(plaintext.to_vec()),
        )),
    }
}

pub(crate) async fn provider_connector(
    state: &ManagementState,
    provider_id: Uuid,
) -> Result<ProviderFacade, Problem> {
    let store = state.store();
    let provider = store
        .get_provider(provider_id)
        .await
        .map_err(map_configuration_resource)?;
    let config = provider_config((&provider).into())
        .map_err(|error| validation("provider", &error.to_string()))?;
    if let Some(connector) = state.certification_probe_connector(provider_id, config.kind()) {
        return Ok(connector);
    }
    let plaintext = match ProviderFactory::credential_kind(&config)
        .map_err(|error| validation("provider", &error.to_string()))?
    {
        CredentialKind::None => None,
        CredentialKind::ApiKey | CredentialKind::ServiceAccountJson | CredentialKind::AwsStatic => {
            let stored = store
                .active_provider_credential_secret(provider_id)
                .await
                .map_err(map_configuration_resource)?;
            let master_key = state
                .master_key
                .as_ref()
                .ok_or_else(|| Problem::service_unavailable("master_key_not_configured"))?;
            Some(
                master_key
                    .open(
                        &stored.encrypted,
                        &credential_aad(provider_id, stored.id, stored.version),
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
    .map_err(|error| validation("provider", &error.to_string()))?;
    ProviderFactory::create(config, credential)
        .await
        .map_err(|error| validation("provider", &error.to_string()))
}

pub(crate) fn runtime_provider_config(
    provider: &RuntimeProviderConfiguration,
    snapshot: &RuntimeSnapshot,
) -> AppResult<ProviderConfig> {
    let probe_model =
        match provider.kind {
            ProviderKind::VertexAi => {
                provider.cloud_project.as_deref().ok_or_else(|| {
                    std::io::Error::other("Vertex provider cloud project is missing")
                })?;
                provider.cloud_region.as_deref().ok_or_else(|| {
                    std::io::Error::other("Vertex provider cloud location is missing")
                })?;
                Some(runtime_provider_model(snapshot, provider.provider_id)?)
            }
            ProviderKind::Bedrock => {
                provider.cloud_region.as_deref().ok_or_else(|| {
                    std::io::Error::other("Bedrock provider AWS region is missing")
                })?;
                None
            }
            ProviderKind::AzureOpenAi => {
                provider.endpoint.as_deref().ok_or_else(|| {
                    std::io::Error::other("Azure OpenAI resource endpoint is missing")
                })?;
                provider
                    .deployment
                    .as_deref()
                    .ok_or_else(|| std::io::Error::other("Azure OpenAI deployment is missing"))?;
                provider
                    .api_version
                    .as_deref()
                    .ok_or_else(|| std::io::Error::other("Azure OpenAI API version is missing"))?;
                None
            }
            _ => None,
        };
    let mut fields = ProviderConfigFields::from(provider);
    fields.probe_model = probe_model.as_deref();
    Ok(provider_config(fields)?)
}

pub(crate) fn runtime_provider_credential(
    provider: &RuntimeProviderConfiguration,
    config: &ProviderConfig,
    master_key: &MasterKey,
) -> AppResult<ProviderCredential> {
    let credential_kind = match ProviderFactory::credential_kind(config) {
        Ok(kind) => kind,
        Err(error) => match provider.kind {
            ProviderKind::VertexAi => {
                return Err(std::io::Error::other(
                    "Vertex provider authentication mode is invalid",
                )
                .into());
            }
            ProviderKind::Bedrock => {
                return Err(std::io::Error::other(
                    "Bedrock provider authentication mode is invalid",
                )
                .into());
            }
            _ => return Err(error.into()),
        },
    };
    let plaintext = match credential_kind {
        CredentialKind::ApiKey | CredentialKind::ServiceAccountJson | CredentialKind::AwsStatic => {
            Some(decrypt_provider_credential(provider, master_key)?)
        }
        CredentialKind::None => {
            if provider.kind == ProviderKind::Bedrock && provider.encrypted_credential.is_some() {
                return Err(std::io::Error::other(
                    "Bedrock default-chain provider must not store static credentials",
                )
                .into());
            }
            None
        }
    };
    Ok(provider_credential(
        config,
        plaintext.as_ref().map(|plaintext| plaintext.as_slice()),
    )?)
}

fn decrypt_provider_credential(
    provider: &RuntimeProviderConfiguration,
    master_key: &MasterKey,
) -> AppResult<Zeroizing<Vec<u8>>> {
    let (Some(credential_id), Some(credential_version), Some(encrypted)) = (
        provider.credential_id,
        provider.credential_version,
        provider.encrypted_credential.as_ref(),
    ) else {
        return Err(std::io::Error::other("provider credential is missing").into());
    };
    let aad = credential_aad(
        provider.provider_id.as_uuid(),
        credential_id,
        credential_version,
    );
    Ok(master_key.open(encrypted, &aad)?)
}

fn secret_text(secret: &[u8]) -> Result<&str, ProviderError> {
    std::str::from_utf8(secret)
        .map_err(|_| ProviderError::Credential("provider credential is not valid UTF-8".to_owned()))
}

fn runtime_provider_model(
    snapshot: &RuntimeSnapshot,
    provider_id: ProviderId,
) -> AppResult<String> {
    snapshot
        .providers
        .get(&provider_id)
        .ok_or_else(|| std::io::Error::other("runtime provider is missing"))?
        .capabilities
        .iter()
        .map(|capability| capability.model.clone())
        .next()
        .ok_or_else(|| std::io::Error::other("provider has no configured model").into())
}

#[cfg(test)]
mod tests {
    use olp_domain::{ProviderAuthMode, ProviderId, ProviderKind};
    use olp_providers::{ProviderConfig, ProviderCredential};
    use olp_storage::{MasterKey, RuntimeProviderConfiguration, credential_aad};

    use super::{
        ProviderConfigFields, decrypt_provider_credential, provider_config, provider_credential,
    };

    fn fields(kind: ProviderKind) -> ProviderConfigFields<'static> {
        ProviderConfigFields {
            kind,
            endpoint: None,
            cloud_region: None,
            cloud_project: None,
            deployment: None,
            api_version: None,
            auth_mode: ProviderAuthMode::ApiKey,
            probe_model: None,
        }
    }

    fn runtime_provider_configuration(
        provider_id: ProviderId,
        credential_id: Option<uuid::Uuid>,
        credential_version: Option<u32>,
        encrypted: Option<olp_storage::EncryptedSecret>,
    ) -> RuntimeProviderConfiguration {
        RuntimeProviderConfiguration {
            provider_id,
            kind: ProviderKind::OpenAi,
            endpoint: None,
            cloud_region: None,
            cloud_project: None,
            deployment: None,
            api_version: None,
            auth_mode: ProviderAuthMode::ApiKey,
            credential_id,
            credential_version,
            encrypted_credential: encrypted,
        }
    }

    #[test]
    fn native_provider_defaults_remain_implicit() {
        assert!(matches!(
            provider_config(fields(ProviderKind::OpenAi)).unwrap(),
            ProviderConfig::OpenAi { endpoint: None }
        ));
        assert!(matches!(
            provider_config(fields(ProviderKind::Anthropic)).unwrap(),
            ProviderConfig::Anthropic {
                endpoint: None,
                api_version: None
            }
        ));
        assert!(matches!(
            provider_config(fields(ProviderKind::Gemini)).unwrap(),
            ProviderConfig::Gemini { endpoint: None }
        ));
    }

    #[test]
    fn credential_representation_follows_factory_configuration() {
        let api_key_config = provider_config(fields(ProviderKind::OpenAi)).unwrap();
        assert!(matches!(
            provider_credential(&api_key_config, Some(b"api-key")).unwrap(),
            ProviderCredential::ApiKey(_)
        ));

        let mut vertex = fields(ProviderKind::VertexAi);
        vertex.cloud_project = Some("project");
        vertex.cloud_region = Some("region");
        vertex.probe_model = Some("model");
        vertex.auth_mode = ProviderAuthMode::ServiceAccount;
        let vertex = provider_config(vertex).unwrap();
        assert!(matches!(
            provider_credential(&vertex, Some(b"{}")).unwrap(),
            ProviderCredential::ServiceAccountJson(_)
        ));

        let mut bedrock = fields(ProviderKind::Bedrock);
        bedrock.cloud_region = Some("us-east-1");
        bedrock.auth_mode = ProviderAuthMode::Static;
        let bedrock = provider_config(bedrock).unwrap();
        assert!(matches!(
            provider_credential(&bedrock, Some(b"{}")).unwrap(),
            ProviderCredential::AwsStatic(_)
        ));
    }

    #[test]
    fn provider_credentials_bind_every_identity_field_and_require_metadata() {
        let master_key = MasterKey::new(1, [7; 32]);
        let provider_id = ProviderId::from_uuid(uuid::Uuid::from_u128(10));
        let credential_id = uuid::Uuid::from_u128(11);
        let credential_version = 7;
        let plaintext = b"provider-api-key";
        let encrypted = master_key
            .seal(
                plaintext,
                &credential_aad(provider_id.as_uuid(), credential_id, credential_version),
            )
            .unwrap();
        let record = runtime_provider_configuration(
            provider_id,
            Some(credential_id),
            Some(credential_version),
            Some(encrypted),
        );

        assert_eq!(
            &*decrypt_provider_credential(&record, &master_key).unwrap(),
            plaintext
        );

        let mut altered_provider = record.clone();
        altered_provider.provider_id = ProviderId::from_uuid(uuid::Uuid::from_u128(12));
        let mut altered_credential = record.clone();
        altered_credential.credential_id = Some(uuid::Uuid::from_u128(13));
        let mut altered_version = record.clone();
        altered_version.credential_version = Some(credential_version + 1);
        let mut altered_envelope = record.clone();
        altered_envelope
            .encrypted_credential
            .as_mut()
            .unwrap()
            .key_version = 2;
        let mut missing_id = record.clone();
        missing_id.credential_id = None;
        let mut missing_version = record.clone();
        missing_version.credential_version = None;
        let mut missing_envelope = record;
        missing_envelope.encrypted_credential = None;

        for invalid in [
            altered_provider,
            altered_credential,
            altered_version,
            altered_envelope,
            missing_id,
            missing_version,
            missing_envelope,
        ] {
            assert!(decrypt_provider_credential(&invalid, &master_key).is_err());
        }
    }

    #[test]
    fn text_credentials_must_be_utf8() {
        let config = provider_config(fields(ProviderKind::OpenAi)).unwrap();
        let error = provider_credential(&config, Some(&[0xff, 0xfe])).unwrap_err();
        assert_eq!(error.to_string(), "provider credential is not valid UTF-8");
    }
}
