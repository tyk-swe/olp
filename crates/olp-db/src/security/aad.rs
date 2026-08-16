use uuid::Uuid;

#[must_use]
pub fn credential(provider_id: Uuid, credential_id: Uuid, version: u32) -> Vec<u8> {
    format!("olp:v2:provider:{provider_id}:credential:{credential_id}:v{version}").into_bytes()
}

#[must_use]
pub fn oidc_client_secret(configuration_id: Uuid) -> Vec<u8> {
    format!("olp:v2:oidc:{configuration_id}:client-secret").into_bytes()
}

#[must_use]
pub fn oidc_flow_payload(flow_id: Uuid) -> Vec<u8> {
    format!("olp:v2:oidc-flow:{flow_id}").into_bytes()
}

#[must_use]
pub fn idempotency_replay(actor: Uuid, operation: &str, key: &str) -> Vec<u8> {
    idempotency_replay_scope(actor, operation, key).into_bytes()
}

#[must_use]
pub fn idempotency_replay_scope(actor: Uuid, operation: &str, key: &str) -> String {
    format!("olp:v2:idempotency:{actor}:{operation}:{key}")
}
