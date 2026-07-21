use std::{collections::BTreeMap, path::Path, sync::Arc};

use crate::{
    ProviderConfig, ProviderCredential, ProviderFactory, TransportRegistry,
    provider_adapter::{runtime_provider_config, runtime_provider_credential},
};
use olp_domain::{ProviderId, ProviderTransport, RuntimeSnapshot};
use olp_storage::{MasterKey, PgStore};
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
        let config = runtime_provider_config(&provider, snapshot)?;
        let credential = runtime_provider_credential(&provider, &config, master_key)?;
        let transport = ProviderFactory::transport(config, credential).await?;
        transports.insert(provider.provider_id, transport);
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use std::{io::Write as _, path::Path};

    use crate::TransportRegistry;
    use olp_domain::ProviderId;
    use serde_json::json;
    use tempfile::NamedTempFile;

    use super::{MountedConnectorConfig, register_mounted_connectors};

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
}
