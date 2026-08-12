use chrono::Utc;
use olp_engine::domain::{RuntimeGeneration, RuntimeGenerationId, RuntimeSnapshot};
use uuid::Uuid;

use super::{
    OutboxRecord, PublishedRuntimeRelease, RuntimeOutboxState, RuntimeOutboxStatus,
    releases::verify_release_envelope,
};

fn snapshot() -> RuntimeSnapshot {
    RuntimeSnapshot {
        generation: RuntimeGeneration {
            id: RuntimeGenerationId::new(),
            ordinal: 7,
            activated_at: Utc::now(),
        },
        providers: Default::default(),
        routes: Default::default(),
        api_keys: Default::default(),
    }
}

#[test]
fn release_envelope_binds_payload_id_and_sequence() {
    let snapshot = snapshot();
    let payload = serde_json::to_vec(&snapshot).unwrap();
    let id = snapshot.generation.id.as_uuid();
    assert!(verify_release_envelope(&payload, id, 7).is_ok());
    assert!(verify_release_envelope(&payload, Uuid::now_v7(), 7).is_err());
    assert!(verify_release_envelope(&payload, id, 8).is_err());
    assert!(verify_release_envelope(&payload, id, 0).is_err());
}

#[test]
fn durable_release_exposes_only_the_engine_activation_fields() {
    let generation_id = Uuid::now_v7();
    let release = PublishedRuntimeRelease {
        generation_id,
        sequence: 9,
        payload: b"sensitive-runtime-payload".to_vec(),
        payload_sha256: [3; 32],
        created_at: Utc::now(),
    };
    let candidate = release.activation_candidate();
    assert_eq!(candidate.generation_id, generation_id);
    assert_eq!(candidate.sequence, 9);
    assert_eq!(candidate.payload, b"sensitive-runtime-payload");
    assert!(!format!("{release:?}").contains("sensitive-runtime-payload"));

    let outbox = OutboxRecord {
        id: Uuid::now_v7(),
        topic: "runtime.release".to_owned(),
        aggregate_id: generation_id,
        payload: b"sensitive-outbox-payload".to_vec(),
        created_at: Utc::now(),
    };
    assert!(!format!("{outbox:?}").contains("sensitive-outbox-payload"));
}

#[test]
fn outbox_health_states_distinguish_completeness_and_abandoned_ownership() {
    let unknown = RuntimeOutboxStatus::unknown();
    assert_eq!(unknown.state, RuntimeOutboxState::Unknown);
    assert!(!unknown.complete());
    assert!(!unknown.ownership_abandoned());

    for (state, name, complete, abandoned) in [
        (RuntimeOutboxState::Unknown, "unknown", false, false),
        (RuntimeOutboxState::Healthy, "healthy", true, false),
        (RuntimeOutboxState::Backlogged, "backlogged", false, false),
        (RuntimeOutboxState::Stale, "stale", false, true),
    ] {
        let status = RuntimeOutboxStatus {
            state,
            owner_active: true,
            ..unknown
        };
        assert_eq!(state.as_str(), name);
        assert_eq!(status.complete(), complete);
        assert_eq!(status.ownership_abandoned(), abandoned);
    }

    assert!(
        !RuntimeOutboxStatus {
            state: RuntimeOutboxState::Stale,
            owner_active: false,
            ..unknown
        }
        .ownership_abandoned()
    );
}
