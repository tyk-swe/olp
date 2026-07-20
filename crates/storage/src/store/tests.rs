use super::runtime::verify_release_envelope;
use super::*;
use olp_domain::{RuntimeGeneration, RuntimeGenerationId, RuntimeSnapshot};

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
fn sensitive_repository_records_redact_debug_output() {
    let password = PasswordUser {
        id: Uuid::now_v7(),
        email: "owner@example.test".into(),
        display_name: "Owner".into(),
        password_hash: "secret-hash".into(),
        role: "owner".into(),
    };
    assert!(!format!("{password:?}").contains("secret-hash"));

    let mut principal = SessionPrincipal {
        session_id: Uuid::now_v7(),
        user_id: Uuid::now_v7(),
        email: "owner@example.test".into(),
        display_name: "Owner".into(),
        role: "owner".into(),
        csrf_digest: vec![1, 2, 3, 4],
        expires_at: Utc::now(),
    };
    assert!(!format!("{principal:?}").contains("1, 2, 3, 4"));
    principal.csrf_digest.clear();

    let response = IdempotencyResponse::json(
        201,
        &serde_json::json!({"secret": "one-time-secret"}),
        Some("\"etag\"".to_owned()),
    )
    .unwrap();
    assert!(!format!("{response:?}").contains("one-time-secret"));
}

#[test]
fn typed_idempotency_fingerprints_are_stable_and_request_bound() {
    let first = idempotency_fingerprint(&serde_json::json!({
        "name": "key",
        "scopes": ["inference"]
    }))
    .unwrap();
    let identical = idempotency_fingerprint(&serde_json::json!({
        "name": "key",
        "scopes": ["inference"]
    }))
    .unwrap();
    let changed = idempotency_fingerprint(&serde_json::json!({
        "name": "changed",
        "scopes": ["inference"]
    }))
    .unwrap();
    assert_eq!(first, identical);
    assert_ne!(first, changed);
}
