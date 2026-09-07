use super::*;

/// A disabled provider is out of the runtime and stays out until an operator
/// restores it deliberately. Every mutation that parks a provider back in
/// `draft` - credential rotation, discovery, capability review - must refuse it
/// rather than resurrect it, or `restore_as_draft` stops being the only way
/// back and the disable is silently undone by a routine edit.
pub(super) async fn exercise(
    pool: &PgPool,
    actor: Uuid,
    master_key: &MasterKey,
    provider_id: Uuid,
    disabled_etag: Uuid,
) {
    let model_id = provider_models(pool, provider_id).await[0].id;

    let next_version =
        olp::providers::credentials::next_credential_version_candidate(pool, provider_id)
            .await
            .unwrap();
    let rotated_credential_id = Uuid::now_v7();
    let rotated_encrypted = master_key
        .seal(
            b"secret-for-a-disabled-provider",
            &credential(provider_id, rotated_credential_id, next_version),
        )
        .unwrap();
    assert!(matches!(
        olp::providers::credentials::rotate_provider_credential(
            pool,
            &olp::database::RequestProvenance::default(),
            provider_id,
            RotateCredentialInput {
                credential_id: rotated_credential_id,
                version: next_version,
                encrypted: rotated_encrypted,
                expected_etag: disabled_etag,
                actor,
                idempotency_key: "provider-rotate-while-disabled-01".to_owned(),
            },
            test_replay(master_key, "provider-rotate-while-disabled-01"),
            empty_created_response,
        )
        .await,
        Err(Error::InUse)
    ));

    assert!(matches!(
        olp::providers::models::discover_provider_models(
            pool,
            &olp::database::RequestProvenance::default(),
            provider_id,
            disabled_etag,
            &[DiscoveredModelInput {
                upstream_model: "gpt-test".to_owned(),
                display_name: "GPT Test".to_owned(),
                enabled: true,
                capabilities: vec![CapabilityRecord {
                    operation: "generation".parse().unwrap(),
                    surface: "openai".parse().unwrap(),
                    mode: "unary".parse().unwrap(),
                    source: olp::providers::types::CapabilitySource::Declared,
                    certified_at: None,
                }],
            }],
            actor,
        )
        .await,
        Err(Error::InUse)
    ));

    assert!(matches!(
        olp::providers::models::set_provider_model_enabled(
            pool,
            &olp::database::RequestProvenance::default(),
            provider_id,
            model_id,
            false,
            &[CapabilityRecord {
                operation: "generation".parse().unwrap(),
                surface: "openai".parse().unwrap(),
                mode: "unary".parse().unwrap(),
                source: olp::providers::types::CapabilitySource::Declared,
                certified_at: None,
            }],
            disabled_etag,
            actor,
        )
        .await,
        Err(Error::InUse)
    ));

    // Certification is already fenced to `draft`, so it reports the same
    // refusal for the same reason.
    assert!(matches!(
        olp::providers::models::apply_compatible_capability_certification(
            pool,
            &olp::database::RequestProvenance::default(),
            provider_id,
            model_id,
            disabled_etag,
            actor,
            &[CapabilityCertificationOutcome {
                operation: "generation".parse().unwrap(),
                surface: "openai".parse().unwrap(),
                mode: "unary".parse().unwrap(),
                succeeded: true,
            }],
        )
        .await,
        Err(Error::InUse)
    ));

    // Every refusal left the provider disabled and its ETag untouched, so the
    // restore that follows still has a valid precondition.
    let still_disabled = olp::providers::repository::get_provider(pool, provider_id)
        .await
        .unwrap();
    assert_eq!(
        still_disabled.state,
        olp::providers::types::ProviderState::Disabled
    );
    assert_eq!(still_disabled.etag, disabled_etag);
    assert_eq!(
        olp::providers::credentials::list_provider_credentials(pool, provider_id, None, 100)
            .await
            .unwrap()
            .items
            .iter()
            .filter(|version| version.id == rotated_credential_id)
            .count(),
        0
    );
}
