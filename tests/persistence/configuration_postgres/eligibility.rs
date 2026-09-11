use super::*;

pub(super) async fn exercise(
    pool: &PgPool,
    actor: Uuid,
    master_key: &MasterKey,
    provider_id: Uuid,
    revoked_etag: Uuid,
) {
    // Credentialless workload identity is a first-class active runtime mode.
    // It must not be forced through the encrypted static-credential join.
    let adc_provider_id = Uuid::now_v7();
    let adc_model_id = Uuid::now_v7();
    let adc_provider = olp::providers::lifecycle::create_provider_draft(
        pool,
        &olp::database::RequestProvenance::default(),
        NewProviderDraft {
            provider_id: adc_provider_id,
            credential_id: None,
            model_id: Some(adc_model_id),
            name: "vertex-workload-identity".to_owned(),
            configuration: olp::providers::configuration::ProviderConfiguration {
                options: Default::default(),
                kind: ProviderKind::VertexAi,
                endpoint: None,
                cloud_region: Some("us-central1".to_owned()),
                cloud_project: Some("project-workload".to_owned()),
                deployment: None,
                api_version: None,
                auth_mode: "adc".parse().unwrap(),
                probe_model: None,
            },

            connector_ready: true,
            credential: None,
            model: Some("gemini-2.5-flash".to_owned()),
            display_name: Some("Gemini 2.5 Flash".to_owned()),
            model_enabled: true,
            surface: Some("gemini".parse().unwrap()),
            actor,
            idempotency_key: "provider-vertex-adc-active-01".to_owned(),
        },
        test_replay(master_key, "provider-vertex-adc-active-01"),
        empty_created_response,
    )
    .await
    .unwrap()
    .expect_executed();
    assert!(matches!(
        olp::providers::lifecycle::activate_provider(
            pool,
            &olp::database::RequestProvenance::default(),
            adc_provider_id,
            adc_provider.etag,
            actor,
            "provider-activate-vertex-adc-without-probe-01",
        )
        .await,
        Err(Error::ProviderIncomplete)
    ));
    certify_all_capabilities(pool, adc_provider_id).await;
    olp::providers::repository::record_provider_probe(
        pool,
        &olp::database::RequestProvenance::default(),
        adc_provider_id,
        adc_provider.etag,
        true,
        "workload identity probe succeeded",
        actor,
    )
    .await
    .unwrap();
    let adc_activated = olp::providers::lifecycle::activate_provider(
        pool,
        &olp::database::RequestProvenance::default(),
        adc_provider_id,
        adc_provider.etag,
        actor,
        "provider-activate-vertex-adc-01",
    )
    .await
    .unwrap();
    let adc_runtime: Snapshot =
        Snapshot::from_persisted_slice(&adc_activated.release.payload).unwrap();
    let active = olp::providers::runtime::runtime_provider_configurations(pool, &adc_runtime)
        .await
        .unwrap();
    let adc = active
        .iter()
        .find(|provider| provider.provider_id.as_uuid() == adc_provider_id)
        .unwrap();
    assert_eq!(
        adc.configuration.auth_mode,
        olp::providers::types::ProviderAuthMode::ApplicationDefault
    );
    assert_eq!(
        adc.configuration.cloud_project.as_deref(),
        Some("project-workload")
    );
    assert!(adc.credential_id.is_none());
    assert!(adc.credential_version.is_none());
    assert!(adc.encrypted_credential.is_none());

    assert!(matches!(
        olp::providers::repository::disable_provider(
            pool,
            &olp::database::RequestProvenance::default(),
            provider_id,
            revoked_etag,
            actor,
            "provider-disable-while-referenced-01",
        )
        .await,
        Err(Error::InUse)
    ));

    let replacement_route = olp::routes::drafts::create_route_draft(
        pool,
        &olp::database::RequestProvenance::default(),
        NewRouteDraft {
            slug: "default".to_owned(),
            operations: vec![OperationKind::Generation],
            overall_timeout_ms: 30_000,
            max_attempts: 1,
            targets: vec![NewRouteTarget {
                provider_id: adc_provider_id,
                upstream_model: "gemini-2.5-flash".to_owned(),
                priority: 0,
                weight: 1,
                timeout_ms: 20_000,
            }],
            actor,
            idempotency_key: "route-replace-provider-reference-01".to_owned(),
        },
        test_replay(master_key, "route-replace-provider-reference-01"),
        empty_created_response,
    )
    .await
    .unwrap()
    .expect_executed();
    let (replacement_validated, _) = olp::routes::drafts::validate_route_draft(
        pool,
        &olp::database::RequestProvenance::default(),
        replacement_route.id,
        replacement_route.etag,
        actor,
    )
    .await
    .unwrap();
    olp::routes::drafts::activate_route_draft(
        pool,
        &olp::database::RequestProvenance::default(),
        replacement_route.id,
        replacement_validated,
        actor,
        "route-replace-provider-reference-activate-01",
    )
    .await
    .unwrap();

    let disabled = olp::providers::repository::disable_provider(
        pool,
        &olp::database::RequestProvenance::default(),
        provider_id,
        revoked_etag,
        actor,
        "provider-disable-after-route-replacement-01",
    )
    .await
    .unwrap();
    let disabled_release = disabled.release.as_ref().unwrap();
    let disabled_runtime: Snapshot =
        Snapshot::from_persisted_slice(&disabled_release.payload).unwrap();
    assert!(
        !disabled_runtime
            .providers
            .contains_key(&ProviderId::from_uuid(provider_id))
    );
    assert!(
        disabled_runtime
            .providers
            .contains_key(&ProviderId::from_uuid(adc_provider_id))
    );
    assert_eq!(
        olp::providers::repository::get_provider(pool, provider_id)
            .await
            .unwrap()
            .state,
        olp::providers::types::ProviderState::Disabled
    );

    super::disabled_guards::exercise(pool, actor, master_key, provider_id, disabled.etag).await;

    let restored_provider_etag = olp::providers::repository::restore_provider_as_draft(
        pool,
        &olp::database::RequestProvenance::default(),
        provider_id,
        disabled.etag,
        actor,
        "provider-restore-as-draft-01",
    )
    .await
    .unwrap();
    let restored_provider = olp::providers::repository::get_provider(pool, provider_id)
        .await
        .unwrap();
    assert_eq!(
        restored_provider.state,
        olp::providers::types::ProviderState::Draft
    );
    assert!(restored_provider.last_probe_at.is_none());
    assert!(restored_provider.last_probe_status.is_none());
    assert!(
        provider_models(pool, provider_id)
            .await
            .iter()
            .all(|model| {
                model.capabilities.iter().all(|capability| {
                    capability.source == olp::providers::types::CapabilitySource::Declared
                        && capability.certified_at.is_none()
                })
            })
    );
    assert!(matches!(
        olp::providers::lifecycle::activate_provider(
            pool,
            &olp::database::RequestProvenance::default(),
            provider_id,
            restored_provider_etag,
            actor,
            "provider-activate-restored-without-probe-01",
        )
        .await,
        Err(Error::ProviderIncomplete)
    ));
    certify_all_capabilities(pool, provider_id).await;
    olp::providers::repository::record_provider_probe(
        pool,
        &olp::database::RequestProvenance::default(),
        provider_id,
        restored_provider_etag,
        true,
        "restored provider probe succeeded",
        actor,
    )
    .await
    .unwrap();
    olp::providers::lifecycle::activate_provider(
        pool,
        &olp::database::RequestProvenance::default(),
        provider_id,
        restored_provider_etag,
        actor,
        "provider-activate-restored-01",
    )
    .await
    .unwrap();

    // Keep the workload-identity activation token live for the ETag assertion
    // below; probe evidence itself must not mutate it.
    assert_ne!(adc_activated.etag, adc_provider.etag);

    // Generic compatible endpoints cannot become runtime-eligible from a
    // browser declaration. Only exact tuples backed by server probe evidence
    // are promoted, and any failed tuple keeps activation closed.
    let compatible_id = Uuid::now_v7();
    let compatible_credential_id = Uuid::now_v7();
    let compatible_model_id = Uuid::now_v7();
    let compatible_secret = master_key
        .seal(
            b"compatible-secret",
            &credential(compatible_id, compatible_credential_id, 1),
        )
        .unwrap();
    let compatible = olp::providers::lifecycle::create_provider_draft(
        pool,
        &olp::database::RequestProvenance::default(),
        NewProviderDraft {
            provider_id: compatible_id,
            credential_id: Some(compatible_credential_id),
            model_id: Some(compatible_model_id),
            name: "compatible-draft".to_owned(),
            configuration: olp::providers::configuration::ProviderConfiguration {
                options: Default::default(),
                kind: ProviderKind::OpenAiCompatible,
                endpoint: Some("https://compatible.example/v1/".to_owned()),
                cloud_region: None,
                cloud_project: None,
                deployment: None,
                api_version: None,
                auth_mode: "api_key".parse().unwrap(),
                probe_model: None,
            },

            connector_ready: true,
            credential: Some(compatible_secret),
            model: Some("compatible-model".to_owned()),
            display_name: Some("Compatible Model".to_owned()),
            model_enabled: true,
            surface: Some("openai".parse().unwrap()),
            actor,
            idempotency_key: "provider-compatible-create-01".to_owned(),
        },
        test_replay(master_key, "provider-compatible-create-01"),
        empty_created_response,
    )
    .await
    .unwrap()
    .expect_executed();
    assert!(
        olp::providers::lifecycle::activate_provider(
            pool,
            &olp::database::RequestProvenance::default(),
            compatible_id,
            compatible.etag,
            actor,
            "provider-compatible-activate-declared-01",
        )
        .await
        .is_err()
    );
    let partial = olp::providers::models::apply_compatible_capability_certification(
        pool,
        &olp::database::RequestProvenance::default(),
        compatible_id,
        compatible_model_id,
        compatible.etag,
        actor,
        &[
            CapabilityCertificationOutcome {
                operation: "generation".parse().unwrap(),
                surface: "openai".parse().unwrap(),
                mode: "unary".parse().unwrap(),
                succeeded: true,
            },
            CapabilityCertificationOutcome {
                operation: "generation".parse().unwrap(),
                surface: "openai".parse().unwrap(),
                mode: "streaming".parse().unwrap(),
                succeeded: false,
            },
        ],
    )
    .await
    .unwrap();
    assert_eq!(partial.certified_count, 1);
    let partial_models = provider_models(pool, compatible_id).await;
    assert_eq!(
        partial_models[0]
            .capabilities
            .iter()
            .filter(|capability| {
                capability.source == olp::providers::types::CapabilitySource::Certified
            })
            .count(),
        1
    );
    assert!(partial_models[0].capabilities.iter().any(|capability| {
        capability.source == olp::providers::types::CapabilitySource::Certified
            && capability.certified_at.is_some()
    }));
    assert!(
        olp::providers::lifecycle::activate_provider(
            pool,
            &olp::database::RequestProvenance::default(),
            compatible_id,
            partial.etag,
            actor,
            "provider-compatible-activate-partial-01",
        )
        .await
        .is_err()
    );
    let certified = olp::providers::models::apply_compatible_capability_certification(
        pool,
        &olp::database::RequestProvenance::default(),
        compatible_id,
        compatible_model_id,
        partial.etag,
        actor,
        &[
            CapabilityCertificationOutcome {
                operation: "generation".parse().unwrap(),
                surface: "openai".parse().unwrap(),
                mode: "unary".parse().unwrap(),
                succeeded: true,
            },
            CapabilityCertificationOutcome {
                operation: "generation".parse().unwrap(),
                surface: "openai".parse().unwrap(),
                mode: "streaming".parse().unwrap(),
                succeeded: true,
            },
        ],
    )
    .await
    .unwrap();
    assert_eq!(certified.certified_count, 2);
    let generation_tuples = [
        CapabilityRecord {
            operation: "generation".parse().unwrap(),
            surface: "openai".parse().unwrap(),
            mode: "unary".parse().unwrap(),
            source: olp::providers::types::CapabilitySource::Declared,
            certified_at: None,
        },
        CapabilityRecord {
            operation: "generation".parse().unwrap(),
            surface: "openai".parse().unwrap(),
            mode: "streaming".parse().unwrap(),
            source: olp::providers::types::CapabilitySource::Declared,
            certified_at: None,
        },
    ];
    let all_certified = |capabilities: &[CapabilityRecord]| {
        capabilities.iter().all(|capability| {
            capability.source == olp::providers::types::CapabilitySource::Certified
                && capability.certified_at.is_some()
        })
    };
    // Re-reviewing the identical tuple set keeps the certification evidence.
    let edited_etag = olp::providers::models::set_provider_model_enabled(
        pool,
        &olp::database::RequestProvenance::default(),
        compatible_id,
        compatible_model_id,
        true,
        &generation_tuples,
        certified.etag,
        actor,
    )
    .await
    .unwrap();
    let edited_models = provider_models(pool, compatible_id).await;
    assert_eq!(edited_models[0].capabilities.len(), 2);
    assert!(all_certified(&edited_models[0].capabilities));
    // Only a new tuple starts as declared; the surviving tuples stay certified.
    let mut extended_tuples = generation_tuples.to_vec();
    extended_tuples.push(CapabilityRecord {
        operation: "embeddings".parse().unwrap(),
        surface: "openai".parse().unwrap(),
        mode: "unary".parse().unwrap(),
        source: olp::providers::types::CapabilitySource::Declared,
        certified_at: None,
    });
    let extended_etag = olp::providers::models::set_provider_model_enabled(
        pool,
        &olp::database::RequestProvenance::default(),
        compatible_id,
        compatible_model_id,
        true,
        &extended_tuples,
        edited_etag,
        actor,
    )
    .await
    .unwrap();
    let extended_models = provider_models(pool, compatible_id).await;
    assert_eq!(extended_models[0].capabilities.len(), 3);
    for capability in &extended_models[0].capabilities {
        let expected = if capability.operation.as_str() == "embeddings" {
            olp::providers::types::CapabilitySource::Declared
        } else {
            olp::providers::types::CapabilitySource::Certified
        };
        assert_eq!(capability.source, expected);
    }
    assert!(matches!(
        olp::providers::lifecycle::activate_provider(
            pool,
            &olp::database::RequestProvenance::default(),
            compatible_id,
            extended_etag,
            actor,
            "provider-compatible-activate-extended-01",
        )
        .await,
        Err(Error::ProviderIncomplete)
    ));
    // Disabling the model and dropping the declared tuple keeps the rest.
    let disabled_etag = olp::providers::models::set_provider_model_enabled(
        pool,
        &olp::database::RequestProvenance::default(),
        compatible_id,
        compatible_model_id,
        false,
        &generation_tuples,
        extended_etag,
        actor,
    )
    .await
    .unwrap();
    let disabled_models = provider_models(pool, compatible_id).await;
    assert!(!disabled_models[0].enabled);
    assert_eq!(disabled_models[0].capabilities.len(), 2);
    assert!(all_certified(&disabled_models[0].capabilities));
    let reenabled_etag = olp::providers::models::set_provider_model_enabled(
        pool,
        &olp::database::RequestProvenance::default(),
        compatible_id,
        compatible_model_id,
        true,
        &generation_tuples,
        disabled_etag,
        actor,
    )
    .await
    .unwrap();
    assert!(matches!(
        olp::providers::lifecycle::activate_provider(
            pool,
            &olp::database::RequestProvenance::default(),
            compatible_id,
            reenabled_etag,
            actor,
            "provider-compatible-activate-unprobed-01",
        )
        .await,
        Err(Error::ProviderIncomplete)
    ));
    olp::providers::repository::record_provider_probe(
        pool,
        &olp::database::RequestProvenance::default(),
        compatible_id,
        reenabled_etag,
        true,
        "pre-patch compatible probe succeeded",
        actor,
    )
    .await
    .unwrap();
    let pre_patch = olp::providers::repository::get_provider(pool, compatible_id)
        .await
        .unwrap();
    assert_eq!(pre_patch.last_probe_status.as_deref(), Some("succeeded"));
    assert!(all_certified(
        &provider_models(pool, compatible_id).await[0].capabilities
    ));

    // A rename is not a transport change: probe evidence and certification
    // survive and the draft activates without a new probe.
    let renamed_etag = olp::providers::repository::update_provider(
        pool,
        &olp::database::RequestProvenance::default(),
        compatible_id,
        reenabled_etag,
        &UpdateProvider {
            name: "compatible-renamed".to_owned(),
            configuration: olp::providers::configuration::ProviderConfiguration {
                options: Default::default(),
                endpoint: Some("https://compatible.example/v1/".to_owned()),
                cloud_region: None,
                cloud_project: None,
                deployment: None,
                api_version: None,
                auth_mode: "api_key".parse().unwrap(),
                kind: ProviderKind::OpenAi,
                probe_model: None,
            },
        },
        actor,
    )
    .await
    .unwrap();
    let renamed = olp::providers::repository::get_provider(pool, compatible_id)
        .await
        .unwrap();
    assert_eq!(renamed.name, "compatible-renamed");
    assert_eq!(renamed.state, olp::providers::types::ProviderState::Draft);
    assert_eq!(renamed.updated_at, pre_patch.updated_at);
    assert_eq!(renamed.last_probe_at, pre_patch.last_probe_at);
    assert_eq!(renamed.last_probe_status.as_deref(), Some("succeeded"));
    assert!(all_certified(
        &provider_models(pool, compatible_id).await[0].capabilities
    ));
    let renamed_activation = olp::providers::lifecycle::activate_provider(
        pool,
        &olp::database::RequestProvenance::default(),
        compatible_id,
        renamed_etag,
        actor,
        "provider-compatible-activate-renamed-01",
    )
    .await
    .unwrap();

    let patched_etag = olp::providers::repository::update_provider(
        pool,
        &olp::database::RequestProvenance::default(),
        compatible_id,
        renamed_activation.etag,
        &UpdateProvider {
            name: "compatible-draft".to_owned(),
            configuration: olp::providers::configuration::ProviderConfiguration {
                options: Default::default(),
                endpoint: Some("https://compatible-v2.example/v1/".to_owned()),
                cloud_region: None,
                cloud_project: None,
                deployment: None,
                api_version: None,
                auth_mode: "api_key".parse().unwrap(),
                kind: ProviderKind::OpenAi,
                probe_model: None,
            },
        },
        actor,
    )
    .await
    .unwrap();
    let patched = olp::providers::repository::get_provider(pool, compatible_id)
        .await
        .unwrap();
    assert!(patched.last_probe_at.is_none());
    assert!(patched.last_probe_status.is_none());
    assert!(patched.last_probe_detail.is_none());
    assert!(
        provider_models(pool, compatible_id).await[0]
            .capabilities
            .iter()
            .all(|capability| {
                capability.source == olp::providers::types::CapabilitySource::Declared
                    && capability.certified_at.is_none()
            })
    );
    assert!(matches!(
        olp::providers::lifecycle::activate_provider(
            pool,
            &olp::database::RequestProvenance::default(),
            compatible_id,
            patched_etag,
            actor,
            "provider-compatible-activate-after-patch-01",
        )
        .await,
        Err(Error::ProviderIncomplete)
    ));

    let post_patch_certified = olp::providers::models::apply_compatible_capability_certification(
        pool,
        &olp::database::RequestProvenance::default(),
        compatible_id,
        compatible_model_id,
        patched_etag,
        actor,
        &[
            CapabilityCertificationOutcome {
                operation: "generation".parse().unwrap(),
                surface: "openai".parse().unwrap(),
                mode: "unary".parse().unwrap(),
                succeeded: true,
            },
            CapabilityCertificationOutcome {
                operation: "generation".parse().unwrap(),
                surface: "openai".parse().unwrap(),
                mode: "streaming".parse().unwrap(),
                succeeded: true,
            },
        ],
    )
    .await
    .unwrap();
    assert!(matches!(
        olp::providers::lifecycle::activate_provider(
            pool,
            &olp::database::RequestProvenance::default(),
            compatible_id,
            post_patch_certified.etag,
            actor,
            "provider-compatible-activate-without-fresh-probe-01",
        )
        .await,
        Err(Error::ProviderIncomplete)
    ));
    olp::providers::repository::record_provider_probe(
        pool,
        &olp::database::RequestProvenance::default(),
        compatible_id,
        post_patch_certified.etag,
        false,
        "post-patch compatible probe failed",
        actor,
    )
    .await
    .unwrap();
    assert!(matches!(
        olp::providers::lifecycle::activate_provider(
            pool,
            &olp::database::RequestProvenance::default(),
            compatible_id,
            post_patch_certified.etag,
            actor,
            "provider-compatible-activate-after-failed-probe-01",
        )
        .await,
        Err(Error::ProviderIncomplete)
    ));
    olp::providers::repository::record_provider_probe(
        pool,
        &olp::database::RequestProvenance::default(),
        compatible_id,
        post_patch_certified.etag,
        true,
        "post-patch compatible probe succeeded",
        actor,
    )
    .await
    .unwrap();
    olp::providers::lifecycle::activate_provider(
        pool,
        &olp::database::RequestProvenance::default(),
        compatible_id,
        post_patch_certified.etag,
        actor,
        "provider-compatible-activate-certified-01",
    )
    .await
    .unwrap();
    let certification_audits: i64 = sqlx::query_scalar(
        "SELECT count(*) FROM audit_events WHERE action = 'provider.model.certify' \
         AND resource_id = $1",
    )
    .bind(compatible_model_id.to_string())
    .fetch_one(pool)
    .await
    .unwrap();
    assert_eq!(certification_audits, 3);
}
