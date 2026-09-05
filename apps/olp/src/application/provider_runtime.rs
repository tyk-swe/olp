use super::{error::AppResult, provider_fields::ProviderConfigFields};
use olp_db::{
    runtime::provider_configuration::RuntimeProvider,
    security::{aad::credential, envelope::MasterKey},
};
use olp_engine::{
    domain::{
        ids::ProviderId,
        routing::{provider::ProviderKind, snapshot::Snapshot},
    },
    providers::factory::{
        assembly::Factory,
        configuration::{Config, Credential, CredentialKind},
        input::{provider_config, provider_credential},
    },
};
use zeroize::Zeroizing;
pub(crate) fn runtime_provider_config(
    provider: &RuntimeProvider,
    snapshot: &Snapshot,
) -> AppResult<Config> {
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
    Ok(provider_config(fields.into())?)
}

pub(crate) fn runtime_provider_credential(
    provider: &RuntimeProvider,
    config: &Config,
    master_key: &MasterKey,
) -> AppResult<Credential> {
    let credential_kind = match Factory::credential_kind(config) {
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
    provider: &RuntimeProvider,
    master_key: &MasterKey,
) -> AppResult<Zeroizing<Vec<u8>>> {
    let (Some(credential_id), Some(credential_version), Some(encrypted)) = (
        provider.credential_id,
        provider.credential_version,
        provider.encrypted_credential.as_ref(),
    ) else {
        return Err(std::io::Error::other("provider credential is missing").into());
    };
    let aad = credential(
        provider.provider_id.as_uuid(),
        credential_id,
        credential_version,
    );
    Ok(master_key.open(encrypted, &aad)?)
}

fn runtime_provider_model(snapshot: &Snapshot, provider_id: ProviderId) -> AppResult<String> {
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
    use std::collections::{BTreeMap, BTreeSet};

    use chrono::Utc;
    use olp_db::{
        runtime::provider_configuration::RuntimeProvider,
        security::{aad::credential, envelope::MasterKey},
    };
    use olp_engine::domain::{
        canonical::identity::{OperationKind, Surface, TransportMode},
        ids::{ProviderId, RuntimeGenerationId},
        provider::ProviderAuthMode,
        routing::{
            provider::{Capability, Provider, ProviderKind},
            snapshot::{RuntimeGeneration, Snapshot},
        },
    };
    use olp_engine::providers::factory::configuration::{Config, Credential};

    use super::{
        decrypt_provider_credential, runtime_provider_config, runtime_provider_credential,
    };
    use crate::application::provider_fields::ProviderConfigFields;
    use olp_engine::providers::factory::input::{provider_config, provider_credential};

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
        encrypted: Option<olp_db::security::envelope::EncryptedSecret>,
    ) -> RuntimeProvider {
        RuntimeProvider {
            provider_id,
            provider_revision_id: None,
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

    fn snapshot(provider_id: ProviderId, models: &[&str]) -> Snapshot {
        let capabilities = models
            .iter()
            .map(|model| {
                Capability::new(
                    *model,
                    OperationKind::Generation,
                    Surface::OpenAi,
                    TransportMode::Unary,
                )
            })
            .collect::<BTreeSet<_>>();
        Snapshot {
            generation: RuntimeGeneration {
                id: RuntimeGenerationId::new(),
                ordinal: 1,
                activated_at: Utc::now(),
            },
            providers: BTreeMap::from([(
                provider_id,
                Provider {
                    id: provider_id,
                    revision_id: None,
                    name: "provider".to_owned(),
                    kind: ProviderKind::VertexAi,
                    enabled: true,
                    active_credential: None,
                    capabilities,
                },
            )]),
            routes: BTreeMap::new(),
            api_keys: BTreeMap::new(),
        }
    }

    #[test]
    fn native_provider_defaults_remain_implicit() {
        assert!(matches!(
            provider_config(fields(ProviderKind::OpenAi).into()).unwrap(),
            Config::OpenAi { endpoint: None }
        ));
        assert!(matches!(
            provider_config(fields(ProviderKind::Anthropic).into()).unwrap(),
            Config::Anthropic {
                endpoint: None,
                api_version: None
            }
        ));
        assert!(matches!(
            provider_config(fields(ProviderKind::Gemini).into()).unwrap(),
            Config::Gemini { endpoint: None }
        ));
    }

    #[test]
    fn credential_representation_follows_factory_configuration() {
        let api_key_config = provider_config(fields(ProviderKind::OpenAi).into()).unwrap();
        assert!(matches!(
            provider_credential(&api_key_config, Some(b"api-key")).unwrap(),
            Credential::ApiKey(_)
        ));

        let mut vertex = fields(ProviderKind::VertexAi);
        vertex.cloud_project = Some("project");
        vertex.cloud_region = Some("region");
        vertex.probe_model = Some("model");
        vertex.auth_mode = ProviderAuthMode::ServiceAccount;
        let vertex = provider_config(vertex.into()).unwrap();
        assert!(matches!(
            provider_credential(&vertex, Some(b"{}")).unwrap(),
            Credential::ServiceAccountJson(_)
        ));

        let mut bedrock = fields(ProviderKind::Bedrock);
        bedrock.cloud_region = Some("us-east-1");
        bedrock.auth_mode = ProviderAuthMode::Static;
        let bedrock = provider_config(bedrock.into()).unwrap();
        assert!(matches!(
            provider_credential(&bedrock, Some(b"{}")).unwrap(),
            Credential::AwsStatic(_)
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
                &credential(provider_id.as_uuid(), credential_id, credential_version),
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
        let config = provider_config(fields(ProviderKind::OpenAi).into()).unwrap();
        let error = provider_credential(&config, Some(&[0xff, 0xfe])).unwrap_err();
        assert_eq!(error.to_string(), "provider credential is not valid UTF-8");
    }

    #[test]
    fn cloud_and_compatible_configuration_preserves_required_fields() {
        let mut compatible = fields(ProviderKind::OpenAiCompatible);
        compatible.endpoint = Some("https://inference.example.test/v1");
        assert!(matches!(
            provider_config(compatible.into()).unwrap(),
            Config::OpenAiCompatible { endpoint }
                if endpoint == "https://inference.example.test/v1"
        ));

        let mut vertex = fields(ProviderKind::VertexAi);
        vertex.cloud_project = Some("project");
        vertex.cloud_region = Some("us-central1");
        vertex.probe_model = Some("gemini-model");
        vertex.auth_mode = ProviderAuthMode::ApplicationDefault;
        assert!(matches!(
            provider_config(vertex.into()).unwrap(),
            Config::VertexAi { project, location, probe_model, auth_mode }
                if project == "project" && location == "us-central1"
                    && probe_model == "gemini-model"
                    && auth_mode == ProviderAuthMode::ApplicationDefault
        ));

        let mut azure = fields(ProviderKind::AzureOpenAi);
        azure.endpoint = Some("https://resource.openai.azure.com");
        azure.deployment = Some("deployment");
        azure.api_version = Some("2025-01-01-preview");
        assert!(matches!(
            provider_config(azure.into()).unwrap(),
            Config::AzureOpenAi { endpoint, deployment, api_version }
                if endpoint == "https://resource.openai.azure.com"
                    && deployment == "deployment" && api_version == "2025-01-01-preview"
        ));

        let mut missing_vertex_project = fields(ProviderKind::VertexAi);
        missing_vertex_project.auth_mode = ProviderAuthMode::ApplicationDefault;
        missing_vertex_project.cloud_region = Some("location");
        missing_vertex_project.probe_model = Some("model");
        let mut missing_bedrock_region = fields(ProviderKind::Bedrock);
        missing_bedrock_region.auth_mode = ProviderAuthMode::DefaultChain;
        let mut missing_azure_endpoint = fields(ProviderKind::AzureOpenAi);
        missing_azure_endpoint.deployment = Some("deployment");
        missing_azure_endpoint.api_version = Some("2025-01-01-preview");
        for (invalid, expected_field) in [
            (fields(ProviderKind::OpenAiCompatible), "endpoint"),
            (missing_vertex_project, "project"),
            (missing_bedrock_region, "region"),
            (missing_azure_endpoint, "endpoint"),
        ] {
            assert!(
                provider_config(invalid.into())
                    .unwrap_err()
                    .to_string()
                    .to_ascii_lowercase()
                    .contains(expected_field),
                "missing {expected_field} should be identified"
            );
        }
    }

    #[test]
    fn runtime_cloud_configuration_rejects_incomplete_snapshots() {
        let provider_id = ProviderId::from_uuid(uuid::Uuid::from_u128(21));
        let valid_snapshot = snapshot(provider_id, &["model-b", "model-a"]);
        let mut provider = runtime_provider_configuration(provider_id, None, None, None);
        provider.kind = ProviderKind::VertexAi;
        provider.auth_mode = ProviderAuthMode::ApplicationDefault;
        provider.cloud_project = Some("project".to_owned());
        provider.cloud_region = Some("location".to_owned());
        assert!(matches!(
            runtime_provider_config(&provider, &valid_snapshot).unwrap(),
            Config::VertexAi { probe_model, .. } if probe_model == "model-a"
        ));

        let missing_provider = snapshot(ProviderId::new(), &["model"]);
        assert_eq!(
            runtime_provider_config(&provider, &missing_provider)
                .unwrap_err()
                .to_string(),
            "runtime provider is missing"
        );
        assert_eq!(
            runtime_provider_config(&provider, &snapshot(provider_id, &[]))
                .unwrap_err()
                .to_string(),
            "provider has no configured model"
        );

        let mut missing_project = provider.clone();
        missing_project.cloud_project = None;
        let mut missing_location = provider.clone();
        missing_location.cloud_region = None;
        for (invalid, message) in [
            (missing_project, "Vertex provider cloud project is missing"),
            (
                missing_location,
                "Vertex provider cloud location is missing",
            ),
        ] {
            assert_eq!(
                runtime_provider_config(&invalid, &valid_snapshot)
                    .unwrap_err()
                    .to_string(),
                message
            );
        }
    }

    #[test]
    fn runtime_credentials_enforce_auth_mode_and_metadata() {
        let master_key = MasterKey::new(1, [3; 32]);
        let provider_id = ProviderId::from_uuid(uuid::Uuid::from_u128(31));
        let credential_id = uuid::Uuid::from_u128(32);
        let version = 2;
        let encrypted = master_key
            .seal(
                b"runtime-secret",
                &credential(provider_id.as_uuid(), credential_id, version),
            )
            .unwrap();
        let provider = runtime_provider_configuration(
            provider_id,
            Some(credential_id),
            Some(version),
            Some(encrypted),
        );
        let config = Config::OpenAi { endpoint: None };
        let credential = runtime_provider_credential(&provider, &config, &master_key).unwrap();
        assert!(matches!(credential, Credential::ApiKey(_)));
        let debug = format!("{credential:?}");
        assert!(debug.contains("[REDACTED]"));
        assert!(!debug.contains("runtime-secret"));

        let missing = runtime_provider_configuration(provider_id, None, None, None);
        assert_eq!(
            runtime_provider_credential(&missing, &config, &master_key)
                .unwrap_err()
                .to_string(),
            "provider credential is missing"
        );

        let mut default_chain = provider.clone();
        default_chain.kind = ProviderKind::Bedrock;
        default_chain.auth_mode = ProviderAuthMode::DefaultChain;
        let default_config = Config::Bedrock {
            region: "us-east-1".to_owned(),
            auth_mode: ProviderAuthMode::DefaultChain,
        };
        assert_eq!(
            runtime_provider_credential(&default_chain, &default_config, &master_key)
                .unwrap_err()
                .to_string(),
            "Bedrock default-chain provider must not store static credentials"
        );

        for (kind, config, message) in [
            (
                ProviderKind::VertexAi,
                Config::VertexAi {
                    project: "project".to_owned(),
                    location: "location".to_owned(),
                    probe_model: "model".to_owned(),
                    auth_mode: ProviderAuthMode::ApiKey,
                },
                "Vertex provider authentication mode is invalid",
            ),
            (
                ProviderKind::Bedrock,
                Config::Bedrock {
                    region: "us-east-1".to_owned(),
                    auth_mode: ProviderAuthMode::ApiKey,
                },
                "Bedrock provider authentication mode is invalid",
            ),
        ] {
            let mut invalid = provider.clone();
            invalid.kind = kind;
            assert_eq!(
                runtime_provider_credential(&invalid, &config, &master_key)
                    .unwrap_err()
                    .to_string(),
                message
            );
        }
    }
}
