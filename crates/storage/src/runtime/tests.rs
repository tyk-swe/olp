use chrono::Utc;
use olp_domain::{RuntimeGeneration, RuntimeGenerationId, RuntimeSnapshot};
use uuid::Uuid;

use super::releases::verify_release_envelope;

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
