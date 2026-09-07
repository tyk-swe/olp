use super::*;
use serde_json::json;
use std::io::Write as _;
use tempfile::NamedTempFile;

#[tokio::test]
async fn mounted_provider_uses_shared_configuration_and_a_separate_secret_file() {
    let provider_id = uuid::Uuid::now_v7();
    let mut credential = NamedTempFile::new().unwrap();
    writeln!(credential, "test-mounted-openai-secret").unwrap();
    let mut config = NamedTempFile::new().unwrap();
    serde_json::to_writer(
        &mut config,
        &json!({"providers": [{
            "provider_id": provider_id,
            "configuration": {"kind": "openai", "auth_mode": "api_key"},
            "credential_file": credential.path()
        }]}),
    )
    .unwrap();
    let registry = TransportRegistry::default();
    register_mounted_connectors(
        config.path(),
        &registry,
        &EgressPolicy::default(),
        ResponseLimits::default(),
    )
    .await
    .unwrap();
    assert!(
        registry
            .snapshot()
            .contains_key(&ProviderId::from_uuid(provider_id))
    );
}

#[tokio::test]
async fn mounted_default_identity_rejects_a_stored_credential_before_reading_it() {
    let mut config = NamedTempFile::new().unwrap();
    serde_json::to_writer(&mut config, &json!({"providers": [{
        "provider_id": uuid::Uuid::now_v7(),
        "configuration": {"kind": "vertex_ai", "auth_mode": "adc", "cloud_project": "example", "cloud_region": "us-central1"},
        "model": "example-model",
        "credential_file": "/missing-credential-that-must-not-be-read"
    }]})).unwrap();
    let error = register_mounted_connectors(
        config.path(),
        &TransportRegistry::default(),
        &EgressPolicy::default(),
        ResponseLimits::default(),
    )
    .await
    .unwrap_err();
    assert!(error.to_string().contains("Do not submit a credential"));
}

#[test]
fn retired_vendor_specific_connector_lists_are_refused() {
    assert!(serde_json::from_value::<MountedConnectorConfig>(json!({"openai": []})).is_err());
}
