use super::*;

pub(super) async fn exercise(
    store: &PgStore,
    actor: Uuid,
    master_key: &MasterKey,
    provider_id: Uuid,
    revoked_etag: Uuid,
) {
    // Credentialless workload identity is a first-class active runtime mode.
    // It must not be forced through the encrypted static-credential join.
    let adc_provider_id = Uuid::now_v7();
    let adc_model_id = Uuid::now_v7();
    let adc_provider = store
        .create_provider_draft(
            NewProviderDraft {
                provider_id: adc_provider_id,
                credential_id: None,
                model_id: Some(adc_model_id),
                name: "vertex-workload-identity".to_owned(),
                kind: ProviderKind::VertexAi,
                endpoint: None,
                cloud_region: Some("us-central1".to_owned()),
                cloud_project: Some("project-workload".to_owned()),
                deployment: None,
                api_version: None,
                auth_mode: "adc".parse().unwrap(),
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
        store
            .activate_provider(
                adc_provider_id,
                adc_provider.etag,
                actor,
                "provider-activate-vertex-adc-without-probe-01",
            )
            .await,
        Err(ConfigurationError::ProviderIncomplete)
    ));
    certify_all_capabilities(store, adc_provider_id).await;
    store
        .record_provider_probe(
            adc_provider_id,
            adc_provider.etag,
            true,
            "workload identity probe succeeded",
            actor,
        )
        .await
        .unwrap();
    let adc_activated = store
        .activate_provider(
            adc_provider_id,
            adc_provider.etag,
            actor,
            "provider-activate-vertex-adc-01",
        )
        .await
        .unwrap();
    let adc_runtime: RuntimeSnapshot =
        serde_json::from_slice(&adc_activated.release.payload).unwrap();
    let active = store
        .runtime_provider_configurations(&adc_runtime)
        .await
        .unwrap();
    let adc = active
        .iter()
        .find(|provider| provider.provider_id.as_uuid() == adc_provider_id)
        .unwrap();
    assert_eq!(
        adc.auth_mode,
        olp_engine::domain::ProviderAuthMode::ApplicationDefault
    );
    assert_eq!(adc.cloud_project.as_deref(), Some("project-workload"));
    assert!(adc.credential_id.is_none());
    assert!(adc.credential_version.is_none());
    assert!(adc.encrypted_credential.is_none());

    assert!(matches!(
        store
            .disable_provider(
                provider_id,
                revoked_etag,
                actor,
                "provider-disable-while-referenced-01",
            )
            .await,
        Err(ConfigurationError::InUse)
    ));

    let replacement_route = store
        .create_route_draft(
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
    let (replacement_validated, _) = store
        .validate_route_draft(replacement_route.id, replacement_route.etag, actor)
        .await
        .unwrap();
    store
        .activate_route_draft(
            replacement_route.id,
            replacement_validated,
            actor,
            "route-replace-provider-reference-activate-01",
        )
        .await
        .unwrap();

    let disabled = store
        .disable_provider(
            provider_id,
            revoked_etag,
            actor,
            "provider-disable-after-route-replacement-01",
        )
        .await
        .unwrap();
    let disabled_release = disabled.release.as_ref().unwrap();
    let disabled_runtime: RuntimeSnapshot =
        serde_json::from_slice(&disabled_release.payload).unwrap();
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
        store.get_provider(provider_id).await.unwrap().state,
        olp_engine::domain::ProviderState::Disabled
    );

    let restored_provider_etag = store
        .restore_provider_as_draft(
            provider_id,
            disabled.etag,
            actor,
            "provider-restore-as-draft-01",
        )
        .await
        .unwrap();
    let restored_provider = store.get_provider(provider_id).await.unwrap();
    assert_eq!(
        restored_provider.state,
        olp_engine::domain::ProviderState::Draft
    );
    assert!(restored_provider.last_probe_at.is_none());
    assert!(restored_provider.last_probe_status.is_none());
    assert!(
        provider_models(store, provider_id)
            .await
            .iter()
            .all(|model| {
                model.capabilities.iter().all(|capability| {
                    capability.source == olp_engine::domain::CapabilitySource::Declared
                        && capability.certified_at.is_none()
                })
            })
    );
    assert!(matches!(
        store
            .activate_provider(
                provider_id,
                restored_provider_etag,
                actor,
                "provider-activate-restored-without-probe-01",
            )
            .await,
        Err(ConfigurationError::ProviderIncomplete)
    ));
    certify_all_capabilities(store, provider_id).await;
    store
        .record_provider_probe(
            provider_id,
            restored_provider_etag,
            true,
            "restored provider probe succeeded",
            actor,
        )
        .await
        .unwrap();
    store
        .activate_provider(
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
            &credential_aad(compatible_id, compatible_credential_id, 1),
        )
        .unwrap();
    let compatible = store
        .create_provider_draft(
            NewProviderDraft {
                provider_id: compatible_id,
                credential_id: Some(compatible_credential_id),
                model_id: Some(compatible_model_id),
                name: "compatible-draft".to_owned(),
                kind: ProviderKind::OpenAiCompatible,
                endpoint: Some("https://compatible.example/v1/".to_owned()),
                cloud_region: None,
                cloud_project: None,
                deployment: None,
                api_version: None,
                auth_mode: "api_key".parse().unwrap(),
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
        store
            .activate_provider(
                compatible_id,
                compatible.etag,
                actor,
                "provider-compatible-activate-declared-01",
            )
            .await
            .is_err()
    );
    let partial = store
        .apply_compatible_capability_certification(
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
    let partial_models = provider_models(store, compatible_id).await;
    assert_eq!(
        partial_models[0]
            .capabilities
            .iter()
            .filter(|capability| {
                capability.source == olp_engine::domain::CapabilitySource::Certified
            })
            .count(),
        1
    );
    assert!(partial_models[0].capabilities.iter().any(|capability| {
        capability.source == olp_engine::domain::CapabilitySource::Certified
            && capability.certified_at.is_some()
    }));
    assert!(
        store
            .activate_provider(
                compatible_id,
                partial.etag,
                actor,
                "provider-compatible-activate-partial-01",
            )
            .await
            .is_err()
    );
    let certified = store
        .apply_compatible_capability_certification(
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
    let edited_etag = store
        .set_provider_model_enabled(
            compatible_id,
            compatible_model_id,
            true,
            &[
                CapabilityRecord {
                    operation: "generation".parse().unwrap(),
                    surface: "openai".parse().unwrap(),
                    mode: "unary".parse().unwrap(),
                    source: olp_engine::domain::CapabilitySource::Declared,
                    certified_at: None,
                },
                CapabilityRecord {
                    operation: "generation".parse().unwrap(),
                    surface: "openai".parse().unwrap(),
                    mode: "streaming".parse().unwrap(),
                    source: olp_engine::domain::CapabilitySource::Declared,
                    certified_at: None,
                },
            ],
            certified.etag,
            actor,
        )
        .await
        .unwrap();
    assert!(
        provider_models(store, compatible_id).await[0]
            .capabilities
            .iter()
            .all(|capability| {
                capability.source == olp_engine::domain::CapabilitySource::Declared
                    && capability.certified_at.is_none()
            })
    );
    assert!(
        store
            .activate_provider(
                compatible_id,
                edited_etag,
                actor,
                "provider-compatible-activate-edited-01",
            )
            .await
            .is_err()
    );
    let recertified = store
        .apply_compatible_capability_certification(
            compatible_id,
            compatible_model_id,
            edited_etag,
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
    store
        .record_provider_probe(
            compatible_id,
            recertified.etag,
            true,
            "pre-patch compatible probe succeeded",
            actor,
        )
        .await
        .unwrap();
    let pre_patch = store.get_provider(compatible_id).await.unwrap();
    assert_eq!(pre_patch.last_probe_status.as_deref(), Some("succeeded"));
    assert!(
        provider_models(store, compatible_id).await[0]
            .capabilities
            .iter()
            .all(|capability| {
                capability.source == olp_engine::domain::CapabilitySource::Certified
                    && capability.certified_at.is_some()
            })
    );

    let patched_etag = store
        .update_provider(
            compatible_id,
            recertified.etag,
            &UpdateProvider {
                name: "compatible-draft".to_owned(),
                endpoint: Some("https://compatible-v2.example/v1/".to_owned()),
                cloud_region: None,
                cloud_project: None,
                deployment: None,
                api_version: None,
                auth_mode: "api_key".parse().unwrap(),
            },
            actor,
        )
        .await
        .unwrap();
    let patched = store.get_provider(compatible_id).await.unwrap();
    assert!(patched.last_probe_at.is_none());
    assert!(patched.last_probe_status.is_none());
    assert!(patched.last_probe_detail.is_none());
    assert!(
        provider_models(store, compatible_id).await[0]
            .capabilities
            .iter()
            .all(|capability| {
                capability.source == olp_engine::domain::CapabilitySource::Declared
                    && capability.certified_at.is_none()
            })
    );
    assert!(matches!(
        store
            .activate_provider(
                compatible_id,
                patched_etag,
                actor,
                "provider-compatible-activate-after-patch-01",
            )
            .await,
        Err(ConfigurationError::ProviderIncomplete)
    ));

    let post_patch_certified = store
        .apply_compatible_capability_certification(
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
        store
            .activate_provider(
                compatible_id,
                post_patch_certified.etag,
                actor,
                "provider-compatible-activate-without-fresh-probe-01",
            )
            .await,
        Err(ConfigurationError::ProviderIncomplete)
    ));
    store
        .record_provider_probe(
            compatible_id,
            post_patch_certified.etag,
            false,
            "post-patch compatible probe failed",
            actor,
        )
        .await
        .unwrap();
    assert!(matches!(
        store
            .activate_provider(
                compatible_id,
                post_patch_certified.etag,
                actor,
                "provider-compatible-activate-after-failed-probe-01",
            )
            .await,
        Err(ConfigurationError::ProviderIncomplete)
    ));
    store
        .record_provider_probe(
            compatible_id,
            post_patch_certified.etag,
            true,
            "post-patch compatible probe succeeded",
            actor,
        )
        .await
        .unwrap();
    store
        .activate_provider(
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
    .fetch_one(store.pool())
    .await
    .unwrap();
    assert_eq!(certification_audits, 4);
}
