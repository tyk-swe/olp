use std::{collections::BTreeMap, path::Path, sync::Arc};

use crate::{
    CredentialKind, ProviderConfig, ProviderCredential, ProviderFactory, TransportRegistry,
};
use olp_domain::{ProviderId, ProviderKind, ProviderTransport, RuntimeSnapshot};
use olp_storage::{MasterKey, PgStore, RuntimeProviderConfiguration, credential_aad};
use serde::Deserialize;
use zeroize::Zeroizing;

use crate::cli::{AppResult, check_secret_permissions};

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct MountedConnectorConfig {
    #[serde(default)]
    openai: Vec<MountedOpenAiConnector>,
    #[serde(default)]
    azure_openai: Vec<MountedAzureOpenAiConnector>,
    #[serde(default)]
    vertex: Vec<MountedVertexConnector>,
    #[serde(default)]
    bedrock: Vec<MountedBedrockConnector>,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct MountedOpenAiConnector {
    provider_id: uuid::Uuid,
    #[serde(default = "default_openai_base_url")]
    base_url: String,
    credential_file: std::path::PathBuf,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct MountedAzureOpenAiConnector {
    provider_id: uuid::Uuid,
    endpoint: String,
    deployment: String,
    api_version: String,
    credential_file: std::path::PathBuf,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct MountedVertexConnector {
    provider_id: uuid::Uuid,
    project: String,
    location: String,
    model: String,
    #[serde(default = "default_vertex_auth_mode")]
    auth_mode: String,
    credential_file: Option<std::path::PathBuf>,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct MountedBedrockConnector {
    provider_id: uuid::Uuid,
    region: String,
    #[serde(default = "default_bedrock_auth_mode")]
    auth_mode: String,
    credential_file: Option<std::path::PathBuf>,
}

fn default_vertex_auth_mode() -> String {
    "adc".to_owned()
}

fn default_bedrock_auth_mode() -> String {
    "default_chain".to_owned()
}

fn default_openai_base_url() -> String {
    "https://api.openai.com/v1/".to_owned()
}

pub(crate) async fn register_mounted_connectors(
    path: &Path,
    registry: &TransportRegistry,
) -> AppResult<()> {
    let bytes = tokio::fs::read(path).await?;
    let config: MountedConnectorConfig = serde_json::from_slice(&bytes)?;
    for provider in config.openai {
        check_secret_permissions(&provider.credential_file).await?;
        let secret = Zeroizing::new(tokio::fs::read_to_string(&provider.credential_file).await?);
        let transport = ProviderFactory::transport(
            ProviderConfig::OpenAi {
                endpoint: Some(provider.base_url),
            },
            ProviderCredential::ApiKey(Zeroizing::new(secret.trim().to_owned())),
        )
        .await?;
        registry.register(ProviderId::from_uuid(provider.provider_id), transport);
    }
    for provider in config.azure_openai {
        check_secret_permissions(&provider.credential_file).await?;
        let secret = Zeroizing::new(tokio::fs::read_to_string(&provider.credential_file).await?);
        let transport = ProviderFactory::transport(
            ProviderConfig::AzureOpenAi {
                endpoint: provider.endpoint,
                deployment: provider.deployment,
                api_version: provider.api_version,
            },
            ProviderCredential::ApiKey(Zeroizing::new(secret.trim().to_owned())),
        )
        .await?;
        registry.register(ProviderId::from_uuid(provider.provider_id), transport);
    }
    for provider in config.vertex {
        let secret = match (provider.auth_mode.as_str(), provider.credential_file) {
            ("adc", None) => None,
            ("service_account", Some(path)) => {
                check_secret_permissions(&path).await?;
                Some(Zeroizing::new(tokio::fs::read_to_string(path).await?))
            }
            ("adc", Some(_)) => {
                return Err(std::io::Error::other(
                    "Vertex ADC connector must not configure a credential file",
                )
                .into());
            }
            ("service_account", None) => {
                return Err(std::io::Error::other(
                    "Vertex service_account connector requires a credential file",
                )
                .into());
            }
            _ => {
                return Err(std::io::Error::other(
                    "Vertex connector auth_mode must be adc or service_account",
                )
                .into());
            }
        };
        let credential = secret.map_or(ProviderCredential::None, |secret| {
            ProviderCredential::ServiceAccountJson(secret)
        });
        let transport = ProviderFactory::transport(
            ProviderConfig::VertexAi {
                project: provider.project,
                location: provider.location,
                probe_model: provider.model,
                auth_mode: provider.auth_mode.parse()?,
            },
            credential,
        )
        .await?;
        registry.register(ProviderId::from_uuid(provider.provider_id), transport);
    }
    for provider in config.bedrock {
        let secret = match (provider.auth_mode.as_str(), provider.credential_file) {
            ("default_chain", None) => None,
            ("static", Some(path)) => {
                check_secret_permissions(&path).await?;
                Some(Zeroizing::new(tokio::fs::read(path).await?))
            }
            ("default_chain", Some(_)) => {
                return Err(std::io::Error::other(
                    "Bedrock default_chain connector must not configure a credential file",
                )
                .into());
            }
            ("static", None) => {
                return Err(std::io::Error::other(
                    "Bedrock static connector requires a credential file",
                )
                .into());
            }
            _ => {
                return Err(std::io::Error::other(
                    "Bedrock connector auth_mode must be default_chain or static",
                )
                .into());
            }
        };
        let credential = secret.map_or(ProviderCredential::None, ProviderCredential::AwsStatic);
        let transport = ProviderFactory::transport(
            ProviderConfig::Bedrock {
                region: provider.region,
                auth_mode: provider.auth_mode.parse()?,
            },
            credential,
        )
        .await?;
        registry.register(ProviderId::from_uuid(provider.provider_id), transport);
    }
    Ok(())
}

pub(crate) async fn load_runtime_transports(
    store: &PgStore,
    master_key: &MasterKey,
    snapshot: &RuntimeSnapshot,
    transports: &mut BTreeMap<ProviderId, Arc<dyn ProviderTransport>>,
) -> AppResult<()> {
    for provider in store.runtime_provider_configurations(snapshot).await? {
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
                    provider.deployment.as_deref().ok_or_else(|| {
                        std::io::Error::other("Azure OpenAI deployment is missing")
                    })?;
                    provider.api_version.as_deref().ok_or_else(|| {
                        std::io::Error::other("Azure OpenAI API version is missing")
                    })?;
                    None
                }
                _ => None,
            };
        let config = provider_config(&provider, probe_model)?;
        let credential_kind = match ProviderFactory::credential_kind(&config) {
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
        let decrypted = match credential_kind {
            CredentialKind::ApiKey
            | CredentialKind::ServiceAccountJson
            | CredentialKind::AwsStatic => {
                Some(decrypt_provider_credential(&provider, master_key)?)
            }
            CredentialKind::None => {
                if provider.kind == ProviderKind::Bedrock && provider.encrypted_credential.is_some()
                {
                    return Err(std::io::Error::other(
                        "Bedrock default-chain provider must not store static credentials",
                    )
                    .into());
                }
                None
            }
        };
        let credential = match credential_kind {
            CredentialKind::ApiKey => ProviderCredential::ApiKey(Zeroizing::new(
                secret_text(decrypted.as_ref().expect("API key is decrypted").as_slice())?
                    .to_owned(),
            )),
            CredentialKind::ServiceAccountJson => {
                ProviderCredential::ServiceAccountJson(Zeroizing::new(
                    secret_text(
                        decrypted
                            .as_ref()
                            .expect("service account is decrypted")
                            .as_slice(),
                    )?
                    .to_owned(),
                ))
            }
            CredentialKind::AwsStatic => ProviderCredential::AwsStatic(Zeroizing::new(
                decrypted
                    .as_ref()
                    .expect("AWS credential is decrypted")
                    .as_slice()
                    .to_vec(),
            )),
            CredentialKind::None => ProviderCredential::None,
        };
        let transport = ProviderFactory::transport(config, credential).await?;
        transports.insert(provider.provider_id, transport);
    }
    Ok(())
}

fn provider_config(
    provider: &RuntimeProviderConfiguration,
    probe_model: Option<String>,
) -> AppResult<ProviderConfig> {
    let required = |value: Option<&String>, message: &'static str| -> AppResult<String> {
        value.cloned().ok_or_else(|| {
            Box::new(std::io::Error::other(message)) as Box<dyn std::error::Error + Send + Sync>
        })
    };
    Ok(match provider.kind {
        ProviderKind::OpenAi => ProviderConfig::OpenAi {
            endpoint: provider.endpoint.clone(),
        },
        ProviderKind::OpenAiCompatible => ProviderConfig::OpenAiCompatible {
            endpoint: required(
                provider.endpoint.as_ref(),
                "OpenAI-compatible endpoint is missing",
            )?,
        },
        ProviderKind::Anthropic => ProviderConfig::Anthropic {
            endpoint: provider.endpoint.clone(),
            api_version: provider.api_version.clone(),
        },
        ProviderKind::Gemini => ProviderConfig::Gemini {
            endpoint: provider.endpoint.clone(),
        },
        ProviderKind::VertexAi => ProviderConfig::VertexAi {
            project: required(
                provider.cloud_project.as_ref(),
                "Vertex AI project is missing",
            )?,
            location: required(
                provider.cloud_region.as_ref(),
                "Vertex AI location is missing",
            )?,
            probe_model: probe_model
                .ok_or_else(|| std::io::Error::other("Vertex AI model is missing"))?,
            auth_mode: provider.auth_mode,
        },
        ProviderKind::Bedrock => ProviderConfig::Bedrock {
            region: required(
                provider.cloud_region.as_ref(),
                "Bedrock AWS region is missing",
            )?,
            auth_mode: provider.auth_mode,
        },
        ProviderKind::AzureOpenAi => ProviderConfig::AzureOpenAi {
            endpoint: required(
                provider.endpoint.as_ref(),
                "Azure OpenAI endpoint is missing",
            )?,
            deployment: required(
                provider.deployment.as_ref(),
                "Azure OpenAI deployment is missing",
            )?,
            api_version: required(
                provider.api_version.as_ref(),
                "Azure OpenAI API version is missing",
            )?,
        },
    })
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

fn secret_text(secret: &[u8]) -> AppResult<&str> {
    std::str::from_utf8(secret)
        .map_err(|_| std::io::Error::other("provider credential is not valid UTF-8").into())
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
    use std::{io::Write as _, path::Path};

    use crate::TransportRegistry;
    use olp_domain::{ProviderId, ProviderKind};
    use olp_storage::{MasterKey, RuntimeProviderConfiguration, credential_aad};
    use serde_json::json;
    use tempfile::NamedTempFile;

    use super::{
        MountedConnectorConfig, decrypt_provider_credential, register_mounted_connectors,
        secret_text,
    };

    fn write_temp_file(contents: impl AsRef<[u8]>) -> NamedTempFile {
        let mut file = NamedTempFile::new().unwrap();
        file.write_all(contents.as_ref()).unwrap();
        file
    }

    fn write_connector_config(value: serde_json::Value) -> NamedTempFile {
        write_temp_file(serde_json::to_vec(&value).unwrap())
    }

    #[cfg(unix)]
    fn set_file_mode(path: &Path, mode: u32) {
        use std::os::unix::fs::PermissionsExt;

        std::fs::set_permissions(path, std::fs::Permissions::from_mode(mode)).unwrap();
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
            auth_mode: olp_domain::ProviderAuthMode::ApiKey,
            credential_id,
            credential_version,
            encrypted_credential: encrypted,
        }
    }

    #[tokio::test]
    async fn mounted_openai_and_azure_credentials_are_trimmed_before_registration() {
        let openai_credential = write_temp_file(b"  sk-mounted-test-key\n");
        let azure_credential = write_temp_file(b"\nazure-mounted-test-key  ");
        #[cfg(unix)]
        {
            set_file_mode(openai_credential.path(), 0o600);
            set_file_mode(azure_credential.path(), 0o600);
        }
        let openai_provider_uuid = uuid::Uuid::from_u128(1);
        let azure_provider_uuid = uuid::Uuid::from_u128(2);
        let config = write_connector_config(json!({
            "openai": [{
                "provider_id": openai_provider_uuid,
                "credential_file": openai_credential.path(),
            }],
            "azure_openai": [{
                "provider_id": azure_provider_uuid,
                "endpoint": "https://example.openai.azure.com/",
                "deployment": "test-deployment",
                "api_version": "2024-10-21",
                "credential_file": azure_credential.path(),
            }],
        }));
        let registry = TransportRegistry::default();

        register_mounted_connectors(config.path(), &registry)
            .await
            .unwrap();

        let snapshot = registry.snapshot();
        assert!(snapshot.contains_key(&ProviderId::from_uuid(openai_provider_uuid)));
        assert!(snapshot.contains_key(&ProviderId::from_uuid(azure_provider_uuid)));
        assert_eq!(snapshot.len(), 2);
    }

    #[test]
    fn connector_config_json_is_strict() {
        assert!(serde_json::from_str::<MountedConnectorConfig>("{}").is_ok());

        let invalid_documents = [
            r#"{"unexpected":[]}"#,
            r#"{"openai":{}}"#,
            r#"{"openai":[{"provider_id":"00000000-0000-0000-0000-000000000001","credential_file":"/run/secrets/openai","unexpected":true}]}"#,
            r#"{"openai":[{"provider_id":"not-a-uuid","credential_file":"/run/secrets/openai"}]}"#,
            r#"{} trailing"#,
        ];

        for document in invalid_documents {
            assert!(
                serde_json::from_str::<MountedConnectorConfig>(document).is_err(),
                "unexpectedly accepted {document}"
            );
        }
    }

    #[tokio::test]
    async fn connector_auth_modes_require_matching_credential_files() {
        let provider_id = uuid::Uuid::from_u128(2);
        let unused_path = "/not/read/by-invalid-auth-mode";
        let cases = [
            (
                json!({"vertex": [{
                    "provider_id": provider_id,
                    "project": "project-1",
                    "location": "us-central1",
                    "model": "gemini-2.0-flash",
                    "auth_mode": "adc",
                    "credential_file": unused_path,
                }]}),
                "Vertex ADC connector must not configure a credential file",
            ),
            (
                json!({"vertex": [{
                    "provider_id": provider_id,
                    "project": "project-1",
                    "location": "us-central1",
                    "model": "gemini-2.0-flash",
                    "auth_mode": "service_account",
                }]}),
                "Vertex service_account connector requires a credential file",
            ),
            (
                json!({"vertex": [{
                    "provider_id": provider_id,
                    "project": "project-1",
                    "location": "us-central1",
                    "model": "gemini-2.0-flash",
                    "auth_mode": "ambient",
                }]}),
                "Vertex connector auth_mode must be adc or service_account",
            ),
            (
                json!({"bedrock": [{
                    "provider_id": provider_id,
                    "region": "us-east-1",
                    "auth_mode": "default_chain",
                    "credential_file": unused_path,
                }]}),
                "Bedrock default_chain connector must not configure a credential file",
            ),
            (
                json!({"bedrock": [{
                    "provider_id": provider_id,
                    "region": "us-east-1",
                    "auth_mode": "static",
                }]}),
                "Bedrock static connector requires a credential file",
            ),
            (
                json!({"bedrock": [{
                    "provider_id": provider_id,
                    "region": "us-east-1",
                    "auth_mode": "instance_role",
                }]}),
                "Bedrock connector auth_mode must be default_chain or static",
            ),
        ];

        for (document, expected_error) in cases {
            let config = write_connector_config(document);
            let error = register_mounted_connectors(config.path(), &TransportRegistry::default())
                .await
                .unwrap_err();
            assert_eq!(error.to_string(), expected_error);
        }
    }

    #[tokio::test]
    async fn connector_configuration_errors_do_not_expose_mounted_secrets() {
        let openai_secret = "mounted-openai secret value";
        let openai_credential = write_temp_file(openai_secret);
        #[cfg(unix)]
        set_file_mode(openai_credential.path(), 0o600);
        let openai_config = write_connector_config(json!({
            "openai": [{
                "provider_id": uuid::Uuid::from_u128(3),
                "credential_file": openai_credential.path(),
            }]
        }));
        let openai_error =
            register_mounted_connectors(openai_config.path(), &TransportRegistry::default())
                .await
                .unwrap_err()
                .to_string();
        assert!(!openai_error.contains(openai_secret));

        let bedrock_access_key = "MOUNTEDACCESSKEY1";
        let bedrock_secret = "mounted-bedrock-secret";
        let bedrock_credential = write_temp_file(format!(
            r#"{{"access_key_id":"{bedrock_access_key}","secret_access_key":"{bedrock_secret}","unexpected":true}}"#
        ));
        #[cfg(unix)]
        set_file_mode(bedrock_credential.path(), 0o600);
        let bedrock_config = write_connector_config(json!({
            "bedrock": [{
                "provider_id": uuid::Uuid::from_u128(4),
                "region": "us-east-1",
                "auth_mode": "static",
                "credential_file": bedrock_credential.path(),
            }]
        }));
        let bedrock_error =
            register_mounted_connectors(bedrock_config.path(), &TransportRegistry::default())
                .await
                .unwrap_err()
                .to_string();
        assert!(!bedrock_error.contains(bedrock_access_key));
        assert!(!bedrock_error.contains(bedrock_secret));
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
    fn decrypted_provider_credentials_must_be_utf8() {
        let master_key = MasterKey::new(1, [9; 32]);
        let provider_id = ProviderId::from_uuid(uuid::Uuid::from_u128(20));
        let credential_id = uuid::Uuid::from_u128(21);
        let credential_version = 1;
        let encrypted = master_key
            .seal(
                &[0xff, 0xfe],
                &credential_aad(provider_id.as_uuid(), credential_id, credential_version),
            )
            .unwrap();
        let record = runtime_provider_configuration(
            provider_id,
            Some(credential_id),
            Some(credential_version),
            Some(encrypted),
        );

        let plaintext = decrypt_provider_credential(&record, &master_key).unwrap();
        let error = secret_text(&plaintext).unwrap_err();
        assert_eq!(error.to_string(), "provider credential is not valid UTF-8");
    }
}
