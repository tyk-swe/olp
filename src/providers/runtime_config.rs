use crate::crypto::aad::credential;
use crate::crypto::envelope::MasterKey;
use crate::ids::ProviderId;
use crate::inference::transport::ProviderTransport;
use crate::net::egress::EgressPolicy;
use crate::process::error::AppResult;
use crate::providers::configuration::ProviderConfiguration;
use crate::providers::connector::ResponseLimits;
use crate::providers::connectors::configuration::Credential;
use crate::providers::connectors::configuration::CredentialKind;
use crate::providers::connectors::input::provider_credential;
use crate::providers::runtime::RuntimeProvider;
use crate::providers::runtime_model::ProviderKind;
use crate::runtime::snapshot::Snapshot;
use std::collections::BTreeMap;
use std::sync::Arc;
use zeroize::Zeroizing;
pub(crate) async fn load_runtime_transports(
    providers: &[RuntimeProvider],
    master_key: Option<&MasterKey>,
    snapshot: &Snapshot,
    transports: &mut BTreeMap<ProviderId, Arc<dyn ProviderTransport>>,
    egress_policy: &EgressPolicy,
    response_limits: ResponseLimits,
    limiter: &crate::limits::admission::ReloadableLimiter,
) -> AppResult<()> {
    if master_key.is_none()
        && snapshot
            .routing
            .credentials
            .iter()
            .any(|(provider, slots)| {
                slots
                    .iter()
                    .any(|slot| slot.enabled && slot.id != provider.as_uuid())
            })
    {
        return Err(std::io::Error::other(
            "OLP_MASTER_KEY_FILE is required for named credential slots",
        )
        .into());
    }
    let mut pools = BTreeMap::<ProviderId, crate::providers::pool_transport::PoolTransport>::new();
    for provider in providers {
        let transport = if let Some(master_key) = master_key {
            let config = runtime_provider_config(provider, snapshot)?;
            let credential = runtime_provider_credential(provider, &config, master_key)?;
            crate::providers::connectors::transport(
                config,
                credential,
                egress_policy,
                response_limits,
            )
            .await?
        } else {
            transports
                .get(&provider.provider_id)
                .cloned()
                .ok_or_else(|| {
                    std::io::Error::other(format!(
                        "Mounted transport is missing for provider {}",
                        provider.provider_id
                    ))
                })?
        };
        pools
            .entry(provider.provider_id)
            .or_insert_with(|| {
                crate::providers::pool_transport::PoolTransport::for_provider(
                    snapshot,
                    provider.provider_id,
                    provider.configuration.options.clone(),
                    snapshot
                        .providers
                        .get(&provider.provider_id)
                        .and_then(|p| p.active_credential.map(|c| c.as_uuid())),
                    limiter,
                )
            })
            .transports
            .insert(provider.credential_id, transport);
    }
    // A published empty pool still needs a transport entry so the release can
    // install, but must never reconstruct a disabled credential as a fallback.
    for provider in snapshot.providers.values() {
        if let Some(slots) = snapshot.routing.credentials.get(&provider.id)
            && !slots.iter().any(|slot| slot.enabled)
        {
            pools.entry(provider.id).or_insert_with(|| {
                crate::providers::pool_transport::PoolTransport::for_provider(
                    snapshot,
                    provider.id,
                    snapshot
                        .routing
                        .providers
                        .get(&provider.id)
                        .cloned()
                        .unwrap_or_default(),
                    provider
                        .active_credential
                        .map(|credential| credential.as_uuid()),
                    limiter,
                )
            });
        }
    }
    for (id, pool) in pools {
        transports.insert(id, Arc::new(pool));
    }
    Ok(())
}

pub(crate) fn runtime_provider_config(
    provider: &RuntimeProvider,
    snapshot: &Snapshot,
) -> AppResult<ProviderConfiguration> {
    let mut config = provider.configuration.clone();
    if config.kind == ProviderKind::VertexAi {
        config.probe_model = Some(runtime_provider_model(snapshot, provider.provider_id)?);
    }
    if let Some(violation) = crate::providers::validation::validate(&config, None).first() {
        return Err(std::io::Error::other(violation.detail).into());
    }
    Ok(config)
}

pub(crate) fn runtime_provider_credential(
    provider: &RuntimeProvider,
    config: &ProviderConfiguration,
    master_key: &MasterKey,
) -> AppResult<Credential> {
    let credential_kind = match crate::providers::connectors::configuration::credential_kind(config)
    {
        Ok(kind) => kind,
        Err(error) => match provider.configuration.kind {
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
            if provider.configuration.kind == ProviderKind::Bedrock
                && provider.encrypted_credential.is_some()
            {
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
    use std::collections::BTreeMap;
    use std::collections::BTreeSet;

    use crate::crypto::aad::credential;
    use crate::crypto::envelope::MasterKey;
    use crate::ids::ProviderId;
    use crate::ids::RuntimeGenerationId;
    use crate::protocols::canonical::identity::OperationKind;
    use crate::protocols::canonical::identity::Surface;
    use crate::protocols::canonical::identity::TransportMode;
    use crate::providers::configuration::ProviderConfiguration;
    use crate::providers::connectors::configuration::Credential;
    use crate::providers::runtime::RuntimeProvider;
    use crate::providers::runtime_model::Capability;
    use crate::providers::runtime_model::Provider;
    use crate::providers::runtime_model::ProviderKind;
    use crate::providers::types::ProviderAuthMode;
    use crate::runtime::snapshot::RuntimeGeneration;
    use crate::runtime::snapshot::Snapshot;
    use chrono::Utc;

    use crate::providers::connectors::input::provider_credential;
    use crate::providers::runtime_config::decrypt_provider_credential;
    use crate::providers::runtime_config::runtime_provider_config;
    use crate::providers::runtime_config::runtime_provider_credential;

    fn runtime_provider_configuration(
        provider_id: ProviderId,
        credential_id: Option<uuid::Uuid>,
        credential_version: Option<u32>,
        encrypted: Option<crate::crypto::envelope::EncryptedSecret>,
    ) -> RuntimeProvider {
        RuntimeProvider {
            provider_id,
            provider_revision_id: uuid::Uuid::now_v7(),
            configuration: crate::providers::configuration::ProviderConfiguration {
                options: Default::default(),
                kind: ProviderKind::OpenAi,
                endpoint: None,
                cloud_region: None,
                cloud_project: None,
                deployment: None,
                api_version: None,
                auth_mode: ProviderAuthMode::ApiKey,
                probe_model: None,
            },

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
            routing: Default::default(),
            generation: RuntimeGeneration {
                id: RuntimeGenerationId::new(),
                ordinal: 1,
                activated_at: Utc::now(),
            },
            providers: BTreeMap::from([(
                provider_id,
                Provider {
                    id: provider_id,
                    revision_id: uuid::Uuid::now_v7(),
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
        let config = ProviderConfiguration::new(ProviderKind::OpenAi);
        let error = provider_credential(&config, Some(&[0xff, 0xfe])).unwrap_err();
        assert_eq!(error.to_string(), "provider credential is not valid UTF-8");
    }

    #[test]
    fn runtime_cloud_configuration_rejects_incomplete_snapshots() {
        let provider_id = ProviderId::from_uuid(uuid::Uuid::from_u128(21));
        let valid_snapshot = snapshot(provider_id, &["model-b", "model-a"]);
        let mut provider = runtime_provider_configuration(provider_id, None, None, None);
        provider.configuration.kind = ProviderKind::VertexAi;
        provider.configuration.auth_mode = ProviderAuthMode::ApplicationDefault;
        provider.configuration.cloud_project = Some("project".to_owned());
        provider.configuration.cloud_region = Some("location".to_owned());
        assert_eq!(
            runtime_provider_config(&provider, &valid_snapshot)
                .unwrap()
                .probe_model
                .as_deref(),
            Some("model-a")
        );

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
        missing_project.configuration.cloud_project = None;
        let mut missing_location = provider.clone();
        missing_location.configuration.cloud_region = None;
        for (invalid, message) in [
            (missing_project, "Vertex AI requires a cloud project."),
            (missing_location, "Vertex AI requires a cloud region."),
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
        let config = ProviderConfiguration {
            options: Default::default(),
            kind: crate::providers::runtime_model::ProviderKind::OpenAi,
            endpoint: None,
            ..ProviderConfiguration::new(crate::providers::runtime_model::ProviderKind::OpenAi)
        };
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
        default_chain.configuration.kind = ProviderKind::Bedrock;
        default_chain.configuration.auth_mode = ProviderAuthMode::DefaultChain;
        let default_config = ProviderConfiguration {
            options: Default::default(),
            kind: crate::providers::runtime_model::ProviderKind::Bedrock,
            cloud_region: Some("us-east-1".to_owned()),
            auth_mode: ProviderAuthMode::DefaultChain,
            ..ProviderConfiguration::new(crate::providers::runtime_model::ProviderKind::Bedrock)
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
                ProviderConfiguration {
                    options: Default::default(),
                    kind: crate::providers::runtime_model::ProviderKind::VertexAi,
                    cloud_project: Some("project".to_owned()),
                    cloud_region: Some("location".to_owned()),
                    probe_model: Some("model".to_owned()),
                    auth_mode: ProviderAuthMode::ApiKey,
                    ..ProviderConfiguration::new(
                        crate::providers::runtime_model::ProviderKind::VertexAi,
                    )
                },
                "Vertex provider authentication mode is invalid",
            ),
            (
                ProviderKind::Bedrock,
                ProviderConfiguration {
                    options: Default::default(),
                    kind: crate::providers::runtime_model::ProviderKind::Bedrock,
                    cloud_region: Some("us-east-1".to_owned()),
                    auth_mode: ProviderAuthMode::ApiKey,
                    ..ProviderConfiguration::new(
                        crate::providers::runtime_model::ProviderKind::Bedrock,
                    )
                },
                "Bedrock provider authentication mode is invalid",
            ),
        ] {
            let mut invalid = provider.clone();
            invalid.configuration.kind = kind;
            assert_eq!(
                runtime_provider_credential(&invalid, &config, &master_key)
                    .unwrap_err()
                    .to_string(),
                message
            );
        }
    }
}
