use crate::crypto::key_material::ApiKey;
use crate::runtime::publication::PublishedRuntimeRelease;
use chrono::DateTime;
use chrono::Utc;
use rust_decimal::Decimal;
use uuid::Uuid;

#[derive(Clone, Debug)]
pub struct ApiKeyRecord {
    pub id: Uuid,
    pub lookup_id: String,
    pub name: String,
    /// The operator who originally issued the key. API keys intentionally
    /// remain installation-scoped when that user is later deactivated.
    pub created_by: Uuid,
    pub created_by_email: String,
    pub scopes: Vec<String>,
    pub allowed_routes: Vec<String>,
    pub requests_per_minute: Option<i32>,
    pub tokens_per_minute: Option<i64>,
    pub max_concurrency: Option<i32>,
    pub daily_cost_limit: Option<Decimal>,
    pub monthly_cost_limit: Option<Decimal>,
    pub expires_at: Option<DateTime<Utc>>,
    pub revoked_at: Option<DateTime<Utc>>,
    pub rotated_at: Option<DateTime<Utc>>,
    pub etag: Uuid,
    pub created_at: DateTime<Utc>,
}

#[derive(Clone, Debug)]
pub struct ApiKeyRotationResult {
    pub id: Uuid,
    pub lookup_id: String,
    pub etag: Uuid,
    pub release: PublishedRuntimeRelease,
}

#[derive(Clone, Debug)]
pub struct ApiKeyMutationResult {
    pub etag: Uuid,
    pub release: PublishedRuntimeRelease,
}

#[derive(Clone, Debug)]
pub struct UpdateApiKeyInput {
    pub name: String,
    pub scopes: Vec<String>,
    pub allowed_routes: Vec<String>,
    pub requests_per_minute: Option<u32>,
    pub tokens_per_minute: Option<u64>,
    pub max_concurrency: Option<u32>,
    pub daily_cost_limit: Option<Decimal>,
    pub monthly_cost_limit: Option<Decimal>,
    pub expires_at: Option<DateTime<Utc>>,
}

#[derive(Debug)]
pub struct RotateApiKeyInput<'a> {
    pub id: Uuid,
    pub material: &'a ApiKey,
    pub expected_etag: Uuid,
    pub actor: Uuid,
    pub idempotency_key: &'a str,
    pub daily_cost_limit: Option<Option<Decimal>>,
    pub monthly_cost_limit: Option<Option<Decimal>>,
}
