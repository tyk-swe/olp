use std::{
    collections::{BTreeMap, BTreeSet},
    num::NonZeroU32,
};

use chrono::{DateTime, Utc};
use olp_domain::{
    CapabilitySource, OperationKind, ProviderAuthMode, ProviderKind, ProviderState,
    RouteDraftState, RouteId, RouteSlug, Surface, TargetId, TransportMode,
    weighted_rendezvous_score,
};
use sqlx::{Postgres, Row, Transaction, postgres::PgRow};
use uuid::Uuid;

use crate::{
    ApiKeyMaterial, EncryptedSecret, IdempotencyOutcome, IdempotencyResponse, PersistenceError,
    PgStore, PublishedRuntimeRelease, ReplayableIdempotency,
    runtime_compiler::{compile_and_publish_runtime_in_transaction, prepare_runtime_mutation},
    split_page,
    store::{
        ReplayableIdempotencyClaim, claim_idempotency, claim_replayable_idempotency,
        complete_idempotency, complete_replayable_idempotency,
    },
};

use super::{
    ConfigurationError,
    validation::{
        checked_limit, enforce_provider_revision_diff_limit, validate_capability, validate_model,
        validate_provider_capability, validate_provider_update, validate_route_input,
    },
};

mod api_keys;
mod credentials;
mod helpers;
mod models;
mod providers;
mod revisions;
mod routes;

/// Maximum number of models loaded from either immutable provider revision
/// while producing an in-memory revision diff.
pub const PROVIDER_REVISION_DIFF_MODEL_LIMIT: usize = 2_000;
/// Maximum number of capability tuples loaded from either immutable provider
/// revision while producing an in-memory revision diff.
pub const PROVIDER_REVISION_DIFF_CAPABILITY_LIMIT: usize = 32_000;

#[derive(Clone, Debug)]
pub struct ConfigurationPage<T> {
    pub items: Vec<T>,
    pub next_cursor: Option<Uuid>,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct CapabilityRecord {
    pub operation: OperationKind,
    pub surface: Surface,
    pub mode: TransportMode,
    pub source: CapabilitySource,
    pub certified_at: Option<DateTime<Utc>>,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct CapabilityCertificationOutcome {
    pub operation: OperationKind,
    pub surface: Surface,
    pub mode: TransportMode,
    pub succeeded: bool,
}

#[derive(Clone, Debug)]
pub struct CapabilityCertificationApplied {
    pub etag: Uuid,
    pub certified_at: DateTime<Utc>,
    pub certified_count: usize,
    pub attempted_count: usize,
}

#[derive(Clone, Debug)]
pub struct ProviderModelRecord {
    pub id: Uuid,
    pub upstream_model: String,
    pub display_name: String,
    pub enabled: bool,
    pub discovered_at: Option<DateTime<Utc>>,
    pub capabilities: Vec<CapabilityRecord>,
}

#[derive(Clone, Debug)]
pub struct ProviderModelInventoryRecord {
    pub provider_id: Uuid,
    pub provider_name: String,
    pub provider_kind: ProviderKind,
    pub model: ProviderModelRecord,
}

#[derive(Clone, Debug)]
pub struct ProviderRecord {
    pub id: Uuid,
    pub name: String,
    pub kind: ProviderKind,
    pub state: ProviderState,
    pub endpoint: Option<String>,
    pub cloud_region: Option<String>,
    pub cloud_project: Option<String>,
    pub deployment: Option<String>,
    pub api_version: Option<String>,
    pub auth_mode: ProviderAuthMode,
    pub connector_ready: bool,
    pub etag: Uuid,
    pub active_revision: Option<u32>,
    pub pending_activation: bool,
    pub draft_credential_id: Option<Uuid>,
    pub draft_credential_version: Option<i32>,
    pub runtime_credential_id: Option<Uuid>,
    pub runtime_credential_version: Option<i32>,
    pub last_probe_at: Option<DateTime<Utc>>,
    pub last_probe_status: Option<String>,
    pub last_probe_detail: Option<String>,
    pub created_at: DateTime<Utc>,
    pub updated_at: DateTime<Utc>,
    pub model_count: u64,
    pub enabled_model_count: u64,
    pub capability_count: u64,
    pub certified_capability_count: u64,
    /// First configured model used only for connector probes that require one.
    pub probe_model: Option<String>,
}

#[derive(Clone, Debug)]
pub struct ProviderRevisionRecord {
    pub id: Uuid,
    pub provider_id: Uuid,
    pub revision: i32,
    pub name: String,
    pub kind: ProviderKind,
    pub endpoint: Option<String>,
    pub cloud_region: Option<String>,
    pub cloud_project: Option<String>,
    pub deployment: Option<String>,
    pub api_version: Option<String>,
    pub auth_mode: ProviderAuthMode,
    pub connector_ready: bool,
    pub credential_version_id: Option<Uuid>,
    pub credential_version: Option<i32>,
    pub source_etag: Uuid,
    pub activated_by: Uuid,
    pub activated_at: DateTime<Utc>,
    pub model_count: u64,
    pub enabled_model_count: u64,
    pub capability_count: u64,
    pub certified_capability_count: u64,
}

#[derive(Clone, Debug)]
pub struct ProviderRevisionDiff {
    pub from_revision: i32,
    pub to_revision: i32,
    pub name_changed: bool,
    pub endpoint_changed: bool,
    pub cloud_context_changed: bool,
    pub deployment_changed: bool,
    pub api_version_changed: bool,
    pub connector_changed: bool,
    pub credential_changed: bool,
    pub models_added: Vec<String>,
    pub models_removed: Vec<String>,
    pub models_changed: Vec<String>,
    pub capabilities_added: Vec<String>,
    pub capabilities_removed: Vec<String>,
}

#[derive(Clone, Debug)]
pub struct UpdateProvider {
    pub name: String,
    pub endpoint: Option<String>,
    pub cloud_region: Option<String>,
    pub cloud_project: Option<String>,
    pub deployment: Option<String>,
    pub api_version: Option<String>,
    pub auth_mode: ProviderAuthMode,
}

#[derive(Clone, Debug)]
pub struct CredentialVersionRecord {
    pub id: Uuid,
    pub version: i32,
    /// Credential referenced by the immutable runtime-active revision.
    pub active: bool,
    /// Credential selected by the mutable provider draft for its next revision.
    pub draft_selected: bool,
    pub created_at: DateTime<Utc>,
    pub revoked_at: Option<DateTime<Utc>>,
}

#[derive(Clone, Debug)]
pub struct StoredCredentialSecret {
    pub id: Uuid,
    pub version: u32,
    pub encrypted: EncryptedSecret,
}

#[derive(Clone, Debug)]
pub struct RotateCredentialInput {
    pub credential_id: Uuid,
    pub version: u32,
    pub encrypted: EncryptedSecret,
    pub expected_etag: Uuid,
    pub actor: Uuid,
    pub idempotency_key: String,
}

#[derive(Clone, Debug)]
pub struct ProviderMutationResult {
    pub etag: Uuid,
    pub release: Option<PublishedRuntimeRelease>,
}

#[derive(Clone, Debug)]
pub struct DiscoveredModelInput {
    pub upstream_model: String,
    pub display_name: String,
    pub enabled: bool,
    pub capabilities: Vec<CapabilityRecord>,
}

#[derive(Clone, Debug)]
pub struct RouteTargetRecord {
    pub id: Uuid,
    pub routing_id: Uuid,
    pub provider_model_id: Uuid,
    pub provider_id: Uuid,
    pub provider_name: String,
    pub upstream_model: String,
    pub priority: i32,
    pub weight: i32,
    pub timeout_ms: i32,
    pub position: i32,
}

#[derive(Clone, Debug)]
pub struct RouteDraftRecord {
    pub id: Uuid,
    pub routing_id: Uuid,
    pub slug: String,
    pub state: RouteDraftState,
    pub overall_timeout_ms: i32,
    pub max_attempts: i16,
    pub etag: Uuid,
    pub based_on_revision_id: Option<Uuid>,
    pub operations: Vec<OperationKind>,
    pub targets: Vec<RouteTargetRecord>,
    pub created_at: DateTime<Utc>,
    pub updated_at: DateTime<Utc>,
}

#[derive(Clone, Debug)]
pub struct ReplaceRouteDraftInput {
    pub slug: String,
    pub operations: Vec<OperationKind>,
    pub overall_timeout_ms: i32,
    pub max_attempts: i16,
    pub targets: Vec<(Uuid, i32, i32, i32)>,
}

#[derive(Clone, Debug)]
pub struct RouteRevisionRecord {
    pub id: Uuid,
    pub routing_id: Uuid,
    pub route_id: Uuid,
    pub revision: i32,
    pub slug: String,
    pub overall_timeout_ms: i32,
    pub max_attempts: i16,
    pub source_draft_id: Uuid,
    pub activated_by: Uuid,
    pub activated_at: DateTime<Utc>,
    pub operations: Vec<OperationKind>,
    pub targets: Vec<RouteTargetRecord>,
}

#[derive(Clone, Debug)]
pub struct RouteRecord {
    pub id: Uuid,
    pub slug: String,
    pub created_at: DateTime<Utc>,
    pub revision_count: u64,
    pub latest_revision: RouteRevisionRecord,
}

#[derive(Clone, Debug)]
pub struct RouteSimulationTarget {
    pub target_id: Uuid,
    pub provider_id: Uuid,
    pub provider_name: String,
    pub upstream_model: String,
    pub priority: i32,
    pub eligible: bool,
    pub reason: Option<String>,
    pub attempt: Option<usize>,
}

#[derive(Clone, Debug)]
pub struct RouteSimulation {
    pub deterministic_seed: String,
    pub operation: OperationKind,
    pub surface: Surface,
    pub mode: TransportMode,
    pub targets: Vec<RouteSimulationTarget>,
}

#[derive(Clone, Debug)]
pub struct RouteRevisionDiff {
    pub from_revision: i32,
    pub to_revision: i32,
    pub slug_changed: bool,
    pub timeout_changed: bool,
    pub max_attempts_changed: bool,
    pub operations_added: Vec<OperationKind>,
    pub operations_removed: Vec<OperationKind>,
    pub targets_added: Vec<String>,
    pub targets_removed: Vec<String>,
    pub targets_changed: Vec<String>,
}

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
    pub expires_at: Option<DateTime<Utc>>,
}

#[derive(Debug)]
pub struct RotateApiKeyInput<'a> {
    pub id: Uuid,
    pub material: &'a ApiKeyMaterial,
    pub expected_etag: Uuid,
    pub actor: Uuid,
    pub idempotency_key: &'a str,
}
