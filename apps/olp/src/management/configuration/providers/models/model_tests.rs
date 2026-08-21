use chrono::Utc;
use olp_engine::domain::{
    canonical::identity::{OperationKind, Surface, TransportMode},
    ports::{AttemptFailureClass, TransportPhase},
    provider::CapabilitySource,
};

use super::*;

fn capability() -> CapabilityRecord {
    CapabilityRecord {
        operation: OperationKind::Generation,
        surface: Surface::OpenAi,
        mode: TransportMode::Unary,
        source: CapabilitySource::Certified,
        certified_at: Some(Utc::now()),
    }
}

#[test]
fn model_inventory_conversion_preserves_nested_capability_metadata() {
    let provider_id = Uuid::from_u128(1);
    let model_id = Uuid::from_u128(2);
    let capability_record = capability();
    let certified_at = capability_record.certified_at;
    let response = ProviderModelInventoryResponse::from(ProviderModelInventoryRecord {
        provider_id,
        provider_name: "production".to_owned(),
        provider_kind: ProviderKind::OpenAiCompatible,
        model: ProviderModelRecord {
            id: model_id,
            upstream_model: "upstream-model".to_owned(),
            display_name: "Display model".to_owned(),
            enabled: true,
            discovered_at: None,
            capabilities: vec![capability_record],
        },
    });

    assert_eq!(response.provider_id, provider_id);
    assert_eq!(response.provider_name, "production");
    assert_eq!(response.provider_kind, ProviderKind::OpenAiCompatible);
    assert_eq!(response.model.id, model_id);
    assert_eq!(response.model.upstream_model, "upstream-model");
    assert_eq!(response.model.display_name, "Display model");
    assert!(response.model.enabled);
    assert_eq!(response.model.capabilities.len(), 1);
    let capability = &response.model.capabilities[0];
    assert_eq!(capability.operation, "generation");
    assert_eq!(capability.surface, "openai");
    assert_eq!(capability.mode, "unary");
    assert_eq!(capability.source, "certified");
    assert_eq!(capability.certified_at, certified_at);
}

#[test]
fn capability_input_parses_closed_sets_and_identifies_the_bad_dimension() {
    let input = |operation: &str, surface: &str, mode: &str| CapabilityInput {
        operation: operation.to_owned(),
        surface: surface.to_owned(),
        mode: mode.to_owned(),
    };
    let parsed = capability_record(input("generation", "openai", "unary")).unwrap();
    assert_eq!(parsed.operation, OperationKind::Generation);
    assert_eq!(parsed.surface, Surface::OpenAi);
    assert_eq!(parsed.mode, TransportMode::Unary);
    assert_eq!(parsed.source, CapabilitySource::Declared);
    assert!(parsed.certified_at.is_none());
    let compatible = compatible_capability(&parsed).unwrap();
    assert_eq!(compatible.operation, parsed.operation);
    assert_eq!(compatible.surface, parsed.surface);
    assert_eq!(compatible.mode, parsed.mode);

    for (invalid, detail) in [
        (
            input("unknown", "openai", "unary"),
            "A reviewed operation is invalid.",
        ),
        (
            input("generation", "unknown", "unary"),
            "A reviewed surface is invalid.",
        ),
        (
            input("generation", "openai", "unknown"),
            "A reviewed mode is invalid.",
        ),
    ] {
        let problem = capability_record(invalid).unwrap_err();
        assert_eq!(problem.status, 422);
        assert_eq!(problem.errors["capabilities"], [detail]);
    }
}

#[test]
fn certification_results_distinguish_success_and_transport_failures() {
    let success = certification_item(capability(), Ok(CapabilityCertificationEvidence::LiveProbe));
    assert!(success.succeeded);
    assert!(success.error_code.is_none());
    assert!(success.detail.contains("production response codec"));

    for (class, code) in [
        (AttemptFailureClass::Connect, "connect_failed"),
        (AttemptFailureClass::Timeout, "timeout"),
        (AttemptFailureClass::RateLimit, "rate_limited"),
        (AttemptFailureClass::UpstreamServer, "upstream_server_error"),
        (
            AttemptFailureClass::UpstreamClient,
            "upstream_rejected_probe",
        ),
        (AttemptFailureClass::Protocol, "protocol_mismatch"),
        (AttemptFailureClass::Cancelled, "cancelled"),
        (AttemptFailureClass::Ambiguous, "ambiguous_result"),
    ] {
        let item = certification_item(
            capability(),
            Err(CompatibleCapabilityCertificationError::Transport {
                phase: TransportPhase::FirstByte,
                class,
            }),
        );
        assert!(!item.succeeded);
        assert_eq!(item.error_code.as_deref(), Some(code));
        assert_eq!(
            item.detail,
            "The live endpoint probe failed during FirstByte."
        );
    }

    for (error, code) in [
        (
            CompatibleCapabilityCertificationError::InvalidResult,
            "invalid_probe_result",
        ),
        (
            CompatibleCapabilityCertificationError::ModelNotDiscovered,
            "model_not_discovered",
        ),
    ] {
        let item = certification_item(capability(), Err(error));
        assert!(!item.succeeded);
        assert_eq!(item.error_code.as_deref(), Some(code));
    }
}
