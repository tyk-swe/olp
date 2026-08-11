use uuid::Uuid;

#[must_use]
pub fn credential_aad(provider_id: Uuid, credential_id: Uuid, version: u32) -> Vec<u8> {
    format!("olp:v2:provider:{provider_id}:credential:{credential_id}:v{version}").into_bytes()
}

#[must_use]
pub fn oidc_client_secret_aad(configuration_id: Uuid) -> Vec<u8> {
    format!("olp:v2:oidc:{configuration_id}:client-secret").into_bytes()
}

#[must_use]
pub fn oidc_flow_payload_aad(flow_id: Uuid) -> Vec<u8> {
    format!("olp:v2:oidc-flow:{flow_id}").into_bytes()
}

#[must_use]
pub fn idempotency_replay_aad(actor: Uuid, operation: &str, key: &str) -> Vec<u8> {
    idempotency_replay_scope(actor, operation, key).into_bytes()
}

#[must_use]
pub fn idempotency_replay_scope(actor: Uuid, operation: &str, key: &str) -> String {
    format!("olp:v2:idempotency:{actor}:{operation}:{key}")
}
