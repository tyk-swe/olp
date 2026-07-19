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
use thiserror::Error;
use uuid::Uuid;

use crate::{
    ApiKeyMaterial, EncryptedSecret, IdempotencyOutcome, IdempotencyResponse, PersistenceError,
    PgStore, PublishedRelease, ReplayableIdempotency, RuntimeCompileError,
    catalog_validation::{
        checked_limit, enforce_provider_revision_diff_limit, validate_capability, validate_model,
        validate_provider_capability, validate_provider_update, validate_route_input,
    },
    runtime_compiler::{compile_and_publish_runtime_in_transaction, prepare_runtime_mutation},
    split_page,
    store::{
        ReplayableIdempotencyClaim, claim_idempotency, claim_replayable_idempotency,
        complete_idempotency, complete_replayable_idempotency,
    },
};

/// Maximum number of models loaded from either immutable provider revision
/// while producing an in-memory revision diff.
pub const PROVIDER_REVISION_DIFF_MODEL_LIMIT: usize = 2_000;
/// Maximum number of capability tuples loaded from either immutable provider
/// revision while producing an in-memory revision diff.
pub const PROVIDER_REVISION_DIFF_CAPABILITY_LIMIT: usize = 32_000;

#[derive(Debug, Error)]
pub enum CatalogError {
    #[error(transparent)]
    Persistence(#[from] PersistenceError),
    #[error(transparent)]
    RuntimeCompile(#[from] RuntimeCompileError),
    #[error("catalog resource does not exist")]
    NotFound,
    #[error("catalog ETag does not match")]
    PreconditionFailed,
    #[error("catalog resource is in use")]
    InUse,
    #[error("catalog mutation is invalid: {0}")]
    Invalid(String),
    #[error("provider revision diff exceeds the {maximum} {dimension} per-revision server limit")]
    ProviderRevisionDiffTooLarge {
        dimension: &'static str,
        maximum: usize,
    },
    #[error("this idempotency key has already been used")]
    IdempotencyConflict,
    #[error("an operation with this idempotency key is still in progress")]
    IdempotencyInProgress,
}

impl From<sqlx::Error> for CatalogError {
    fn from(error: sqlx::Error) -> Self {
        Self::Persistence(PersistenceError::Database(error))
    }
}

#[derive(Clone, Debug)]
pub struct CatalogPage<T> {
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
    pub run_id: Uuid,
    pub etag: Uuid,
    pub certified_at: DateTime<Utc>,
    pub certified_count: usize,
    pub attempted_count: usize,
}

#[derive(Clone, Debug)]
pub struct CapabilityCertificationStarted {
    pub run_id: Uuid,
    pub upstream_model: String,
    pub capabilities: Vec<CapabilityRecord>,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct CapabilityCertificationResult {
    pub operation: OperationKind,
    pub surface: Surface,
    pub mode: TransportMode,
    pub succeeded: bool,
    pub evidence_kind: Option<String>,
    pub error_code: Option<String>,
    pub detail: String,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum ProviderModelDiscoveryOrigin {
    Manual,
    Upstream,
    Scheduled,
}

impl ProviderModelDiscoveryOrigin {
    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::Manual => "manual",
            Self::Upstream => "upstream",
            Self::Scheduled => "scheduled",
        }
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum ProviderModelDiscoveryCompleteness {
    Complete,
    Partial,
}

impl ProviderModelDiscoveryCompleteness {
    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::Complete => "complete",
            Self::Partial => "partial",
        }
    }

    #[must_use]
    pub const fn is_complete(self) -> bool {
        matches!(self, Self::Complete)
    }
}

#[derive(Clone, Debug)]
pub struct ProviderModelDiscoveryApplied {
    pub etag: Uuid,
    pub completed_at: DateTime<Utc>,
    pub observed_model_count: usize,
    pub added_model_count: usize,
    pub renamed_model_count: usize,
    pub newly_missing_model_count: usize,
    pub completeness: ProviderModelDiscoveryCompleteness,
}

#[derive(Debug)]
pub struct ReconcileProviderModelDiscoveryInput<'a> {
    pub provider_id: Uuid,
    pub expected_etag: Uuid,
    pub models: &'a [DiscoveredModelInput],
    pub origin: ProviderModelDiscoveryOrigin,
    pub completeness: ProviderModelDiscoveryCompleteness,
    pub actor: Option<Uuid>,
    pub claim_id: Option<Uuid>,
}

#[derive(Clone, Debug)]
pub struct ScheduledModelDiscoveryClaim {
    pub provider_id: Uuid,
    pub expected_etag: Uuid,
    pub claim_id: Uuid,
}

#[derive(Clone, Debug)]
pub struct ProviderModelRecord {
    pub id: Uuid,
    pub upstream_model: String,
    pub display_name: String,
    pub enabled: bool,
    pub discovered_at: Option<DateTime<Utc>>,
    pub inventory_source: String,
    pub availability: String,
    pub first_seen_at: Option<DateTime<Utc>>,
    pub last_seen_at: Option<DateTime<Utc>>,
    pub missing_since: Option<DateTime<Utc>>,
    pub last_certification_status: Option<String>,
    pub last_certification_at: Option<DateTime<Utc>>,
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
pub struct ProviderCatalogRecord {
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
    pub probe_ready: bool,
    pub created_at: DateTime<Utc>,
    pub updated_at: DateTime<Utc>,
    pub model_count: u64,
    pub enabled_model_count: u64,
    pub capability_count: u64,
    pub certified_capability_count: u64,
    pub enabled_capability_count: u64,
    pub enabled_certified_capability_count: u64,
    pub missing_model_count: u64,
    pub invalid_enabled_model_count: u64,
    pub last_model_discovery_at: Option<DateTime<Utc>>,
    pub last_model_discovery_status: Option<String>,
    /// First configured model used only for connector probes that require one.
    pub probe_model: Option<String>,
}

#[derive(Clone, Debug)]
pub struct ProviderRevisionCatalogRecord {
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
pub struct UpdateProviderCatalog {
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
    pub release: Option<PublishedRelease>,
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
    pub provider_model: String,
    pub priority: i32,
    pub weight: i32,
    pub timeout_ms: i32,
    pub position: i32,
}

#[derive(Clone, Debug)]
pub struct RouteDraftCatalogRecord {
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
pub struct ReplaceRouteDraftCatalogInput {
    pub slug: String,
    pub operations: Vec<OperationKind>,
    pub overall_timeout_ms: i32,
    pub max_attempts: i16,
    pub targets: Vec<(Uuid, i32, i32, i32)>,
}

#[derive(Clone, Debug)]
pub struct RouteRevisionCatalogRecord {
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
pub struct RouteCatalogRecord {
    pub id: Uuid,
    pub slug: String,
    pub created_at: DateTime<Utc>,
    pub revision_count: u64,
    pub latest_revision: RouteRevisionCatalogRecord,
}

#[derive(Clone, Debug)]
pub struct RouteSimulationTarget {
    pub target_id: Uuid,
    pub provider_id: Uuid,
    pub provider_name: String,
    pub provider_model: String,
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
pub struct ApiKeyCatalogRecord {
    pub id: Uuid,
    pub lookup_id: String,
    pub name: String,
    /// The operator who originally issued the key. API keys intentionally
    /// remain team-scoped when that user is later deactivated.
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
    pub release: PublishedRelease,
}

#[derive(Clone, Debug)]
pub struct ApiKeyMutationResult {
    pub etag: Uuid,
    pub release: PublishedRelease,
}

#[derive(Clone, Debug)]
pub struct UpdateApiKeyCatalogInput {
    pub name: String,
    pub scopes: Vec<String>,
    pub allowed_routes: Vec<String>,
    pub requests_per_minute: Option<u32>,
    pub tokens_per_minute: Option<u64>,
    pub max_concurrency: Option<u32>,
    pub expires_at: Option<DateTime<Utc>>,
}

#[derive(Debug)]
pub struct RotateApiKeyCatalogInput<'a> {
    pub id: Uuid,
    pub material: &'a ApiKeyMaterial,
    pub expected_etag: Uuid,
    pub actor: Uuid,
    pub idempotency_key: &'a str,
}

impl PgStore {
    pub async fn list_provider_catalog(
        &self,
        cursor: Option<Uuid>,
        limit: i64,
    ) -> Result<CatalogPage<ProviderCatalogRecord>, CatalogError> {
        let limit = checked_limit(limit)?;
        let rows = sqlx::query(
            "SELECT p.id, p.name, p.kind, p.state::text AS state, p.endpoint, p.cloud_region, \
                    p.cloud_project, p.deployment, p.api_version, p.auth_mode, p.connector_ready, \
                    p.etag, ar.revision AS active_revision, \
                    (p.state = 'draft'::provider_state AND p.active_revision_id IS NOT NULL) \
                      AS pending_activation, \
                    p.active_credential_version_id AS draft_credential_id, \
                    draft_cv.version AS draft_credential_version, \
                    ar.credential_version_id AS runtime_credential_id, \
                    runtime_cv.version AS runtime_credential_version, \
                     p.last_probe_at, p.last_probe_status, p.last_probe_detail, \
                     (p.last_probe_status = 'succeeded' AND p.last_probe_at IS NOT NULL \
                       AND p.last_probe_context_id = p.certification_context_id) AS probe_ready, \
                     p.last_model_discovery_at, p.last_model_discovery_status, \
                     p.created_at, p.updated_at, \
                     stats.model_count, stats.enabled_model_count, stats.capability_count, \
                     stats.certified_capability_count, stats.enabled_capability_count, \
                     stats.enabled_certified_capability_count, stats.missing_model_count, \
                     stats.invalid_enabled_model_count, \
                     probe.upstream_model AS probe_model \
             FROM providers p \
             LEFT JOIN provider_credential_versions draft_cv \
               ON draft_cv.id = p.active_credential_version_id \
             LEFT JOIN provider_revisions ar ON ar.id = p.active_revision_id \
             LEFT JOIN provider_credential_versions runtime_cv \
               ON runtime_cv.id = ar.credential_version_id \
             LEFT JOIN LATERAL ( \
                 SELECT COUNT(DISTINCT pm.id)::bigint AS model_count, \
                        COUNT(DISTINCT pm.id) FILTER (WHERE pm.enabled)::bigint \
                          AS enabled_model_count, \
                        COUNT(mc.provider_model_id)::bigint AS capability_count, \
                         COUNT(mc.provider_model_id) FILTER (WHERE mc.source = 'certified' \
                             AND mc.certification_context_id = p.certification_context_id \
                             AND mc.review_revision = pm.review_revision)::bigint \
                           AS certified_capability_count, \
                         COUNT(mc.provider_model_id) FILTER (WHERE pm.enabled)::bigint \
                           AS enabled_capability_count, \
                         COUNT(mc.provider_model_id) FILTER (WHERE pm.enabled \
                             AND mc.source = 'certified' \
                             AND mc.certification_context_id = p.certification_context_id \
                             AND mc.review_revision = pm.review_revision)::bigint \
                           AS enabled_certified_capability_count, \
                         COUNT(DISTINCT pm.id) FILTER (WHERE pm.availability = 'missing')::bigint \
                           AS missing_model_count, \
                         COUNT(DISTINCT pm.id) FILTER (WHERE pm.enabled AND ( \
                           pm.availability <> 'available' OR mc.provider_model_id IS NULL \
                           OR mc.source <> 'certified' \
                           OR mc.certification_context_id IS DISTINCT FROM p.certification_context_id \
                           OR mc.review_revision IS DISTINCT FROM pm.review_revision))::bigint \
                           AS invalid_enabled_model_count \
                 FROM provider_models pm \
                 LEFT JOIN model_capabilities mc ON mc.provider_model_id = pm.id \
                 WHERE pm.provider_id = p.id \
             ) stats ON true \
             LEFT JOIN LATERAL ( \
                 SELECT pm.upstream_model FROM provider_models pm \
                 WHERE pm.provider_id = p.id ORDER BY pm.id LIMIT 1 \
             ) probe ON true \
             WHERE ($1::uuid IS NULL OR p.id > $1) ORDER BY p.id LIMIT $2",
        )
        .bind(cursor)
        .bind(limit + 1)
        .fetch_all(self.pool())
        .await?;
        let (rows, next_cursor) = split_page(rows, limit as usize, |row| row.get::<Uuid, _>("id"));
        let items = rows
            .into_iter()
            .map(provider_catalog_from_row)
            .collect::<Result<Vec<_>, _>>()?;
        Ok(CatalogPage { items, next_cursor })
    }

    pub async fn get_provider_catalog(
        &self,
        provider_id: Uuid,
    ) -> Result<ProviderCatalogRecord, CatalogError> {
        let row = sqlx::query(
            "SELECT p.id, p.name, p.kind, p.state::text AS state, p.endpoint, p.cloud_region, \
                    p.cloud_project, p.deployment, p.api_version, p.auth_mode, p.connector_ready, \
                    p.etag, ar.revision AS active_revision, \
                    (p.state = 'draft'::provider_state AND p.active_revision_id IS NOT NULL) \
                      AS pending_activation, \
                    p.active_credential_version_id AS draft_credential_id, \
                    draft_cv.version AS draft_credential_version, \
                    ar.credential_version_id AS runtime_credential_id, \
                    runtime_cv.version AS runtime_credential_version, \
                     p.last_probe_at, p.last_probe_status, \
                     p.last_probe_detail, \
                     (p.last_probe_status = 'succeeded' AND p.last_probe_at IS NOT NULL \
                       AND p.last_probe_context_id = p.certification_context_id) AS probe_ready, \
                     p.last_model_discovery_at, p.last_model_discovery_status, \
                     p.created_at, p.updated_at, \
                     stats.model_count, stats.enabled_model_count, stats.capability_count, \
                     stats.certified_capability_count, stats.enabled_capability_count, \
                     stats.enabled_certified_capability_count, stats.missing_model_count, \
                     stats.invalid_enabled_model_count, \
                     probe.upstream_model AS probe_model \
             FROM providers p LEFT JOIN provider_credential_versions draft_cv \
               ON draft_cv.id = p.active_credential_version_id \
             LEFT JOIN provider_revisions ar ON ar.id = p.active_revision_id \
             LEFT JOIN provider_credential_versions runtime_cv \
               ON runtime_cv.id = ar.credential_version_id \
             LEFT JOIN LATERAL ( \
                 SELECT COUNT(DISTINCT pm.id)::bigint AS model_count, \
                        COUNT(DISTINCT pm.id) FILTER (WHERE pm.enabled)::bigint \
                          AS enabled_model_count, \
                        COUNT(mc.provider_model_id)::bigint AS capability_count, \
                         COUNT(mc.provider_model_id) FILTER (WHERE mc.source = 'certified' \
                             AND mc.certification_context_id = p.certification_context_id \
                             AND mc.review_revision = pm.review_revision)::bigint \
                           AS certified_capability_count, \
                         COUNT(mc.provider_model_id) FILTER (WHERE pm.enabled)::bigint \
                           AS enabled_capability_count, \
                         COUNT(mc.provider_model_id) FILTER (WHERE pm.enabled \
                             AND mc.source = 'certified' \
                             AND mc.certification_context_id = p.certification_context_id \
                             AND mc.review_revision = pm.review_revision)::bigint \
                           AS enabled_certified_capability_count, \
                         COUNT(DISTINCT pm.id) FILTER (WHERE pm.availability = 'missing')::bigint \
                           AS missing_model_count, \
                         COUNT(DISTINCT pm.id) FILTER (WHERE pm.enabled AND ( \
                           pm.availability <> 'available' OR mc.provider_model_id IS NULL \
                           OR mc.source <> 'certified' \
                           OR mc.certification_context_id IS DISTINCT FROM p.certification_context_id \
                           OR mc.review_revision IS DISTINCT FROM pm.review_revision))::bigint \
                           AS invalid_enabled_model_count \
                 FROM provider_models pm \
                 LEFT JOIN model_capabilities mc ON mc.provider_model_id = pm.id \
                 WHERE pm.provider_id = p.id \
             ) stats ON true \
             LEFT JOIN LATERAL ( \
                 SELECT pm.upstream_model FROM provider_models pm \
                 WHERE pm.provider_id = p.id ORDER BY pm.id LIMIT 1 \
             ) probe ON true \
             WHERE p.id = $1",
        )
        .bind(provider_id)
        .fetch_optional(self.pool())
        .await?
        .ok_or(CatalogError::NotFound)?;
        provider_catalog_from_row(row)
    }

    pub async fn list_provider_models_catalog(
        &self,
        provider_id: Uuid,
        cursor: Option<Uuid>,
        limit: i64,
    ) -> Result<CatalogPage<ProviderModelRecord>, CatalogError> {
        let limit = checked_limit(limit)?;
        ensure_provider_exists(self, provider_id).await?;
        let rows = sqlx::query(
            "SELECT pm.id, pm.upstream_model, pm.display_name, pm.enabled, pm.discovered_at, \
                    pm.inventory_source, pm.availability, pm.first_seen_at, pm.last_seen_at, \
                    pm.missing_since, cert.status AS last_certification_status, \
                    cert.completed_at AS last_certification_at \
              FROM provider_models pm LEFT JOIN LATERAL ( \
                SELECT status, completed_at FROM capability_certification_runs \
                WHERE provider_model_id = pm.id ORDER BY started_at DESC LIMIT 1 \
              ) cert ON true WHERE pm.provider_id = $1 \
                AND ($2::uuid IS NULL OR pm.id > $2) ORDER BY pm.id LIMIT $3",
        )
        .bind(provider_id)
        .bind(cursor)
        .bind(limit + 1)
        .fetch_all(self.pool())
        .await?;
        let (rows, next_cursor) = split_page(rows, limit as usize, |row| row.get::<Uuid, _>("id"));
        let items = self.provider_models_from_rows(rows).await?;
        Ok(CatalogPage { items, next_cursor })
    }

    pub async fn list_provider_model_inventory_catalog(
        &self,
        cursor: Option<Uuid>,
        limit: i64,
        enabled: Option<bool>,
    ) -> Result<CatalogPage<ProviderModelInventoryRecord>, CatalogError> {
        let limit = checked_limit(limit)?;
        let rows = sqlx::query(
            "SELECT pm.id, pm.upstream_model, pm.display_name, pm.enabled, pm.discovered_at, \
                    pm.inventory_source, pm.availability, pm.first_seen_at, pm.last_seen_at, \
                    pm.missing_since, cert.status AS last_certification_status, \
                    cert.completed_at AS last_certification_at, \
                    p.id AS provider_id, p.name AS provider_name, p.kind AS provider_kind \
              FROM provider_models pm JOIN providers p ON p.id = pm.provider_id \
              LEFT JOIN LATERAL ( \
                SELECT status, completed_at FROM capability_certification_runs \
                WHERE provider_model_id = pm.id ORDER BY started_at DESC LIMIT 1 \
              ) cert ON true \
             WHERE ($1::uuid IS NULL OR pm.id > $1) \
               AND ($2::boolean IS NULL OR pm.enabled = $2) \
             ORDER BY pm.id LIMIT $3",
        )
        .bind(cursor)
        .bind(enabled)
        .bind(limit + 1)
        .fetch_all(self.pool())
        .await?;
        let (rows, next_cursor) = split_page(rows, limit as usize, |row| row.get::<Uuid, _>("id"));
        let mut providers = rows
            .iter()
            .map(|row| {
                Ok((
                    row.get::<Uuid, _>("id"),
                    (
                        row.get::<Uuid, _>("provider_id"),
                        row.get::<String, _>("provider_name"),
                        row.get::<String, _>("provider_kind")
                            .parse()
                            .map_err(|_| PersistenceError::InvalidStoredValue("provider kind"))?,
                    ),
                ))
            })
            .collect::<Result<BTreeMap<_, _>, CatalogError>>()?;
        let items = self
            .provider_models_from_rows(rows)
            .await?
            .into_iter()
            .map(|model| {
                let (provider_id, provider_name, provider_kind) = providers
                    .remove(&model.id)
                    .expect("provider metadata exists for every model row");
                ProviderModelInventoryRecord {
                    provider_id,
                    provider_name,
                    provider_kind,
                    model,
                }
            })
            .collect();
        Ok(CatalogPage { items, next_cursor })
    }

    pub async fn get_provider_model_catalog(
        &self,
        provider_id: Uuid,
        model_id: Uuid,
    ) -> Result<ProviderModelRecord, CatalogError> {
        let rows = sqlx::query(
            "SELECT pm.id, pm.upstream_model, pm.display_name, pm.enabled, pm.discovered_at, \
                    pm.inventory_source, pm.availability, pm.first_seen_at, pm.last_seen_at, \
                    pm.missing_since, cert.status AS last_certification_status, \
                    cert.completed_at AS last_certification_at \
              FROM provider_models pm LEFT JOIN LATERAL ( \
                SELECT status, completed_at FROM capability_certification_runs \
                WHERE provider_model_id = pm.id ORDER BY started_at DESC LIMIT 1 \
              ) cert ON true WHERE pm.provider_id = $1 AND pm.id = $2",
        )
        .bind(provider_id)
        .bind(model_id)
        .fetch_all(self.pool())
        .await?;
        self.provider_models_from_rows(rows)
            .await?
            .into_iter()
            .next()
            .ok_or(CatalogError::NotFound)
    }

    pub async fn list_provider_revisions_catalog(
        &self,
        provider_id: Uuid,
        cursor: Option<Uuid>,
        limit: i64,
    ) -> Result<CatalogPage<ProviderRevisionCatalogRecord>, CatalogError> {
        let limit = checked_limit(limit)?;
        let exists: bool =
            sqlx::query_scalar("SELECT EXISTS (SELECT 1 FROM providers WHERE id = $1)")
                .bind(provider_id)
                .fetch_one(self.pool())
                .await?;
        if !exists {
            return Err(CatalogError::NotFound);
        }
        let before_revision: Option<i32> = match cursor {
            Some(cursor) => Some(
                sqlx::query_scalar(
                    "SELECT revision FROM provider_revisions WHERE provider_id = $1 AND id = $2",
                )
                .bind(provider_id)
                .bind(cursor)
                .fetch_optional(self.pool())
                .await?
                .ok_or_else(|| {
                    CatalogError::Invalid(
                        "provider-revision pagination cursor is invalid".to_owned(),
                    )
                })?,
            ),
            None => None,
        };
        let rows = sqlx::query(
            "SELECT pr.id, pr.provider_id, pr.revision, pr.name, pr.kind, pr.endpoint, \
                    pr.cloud_region, pr.cloud_project, pr.deployment, pr.api_version, \
                    pr.auth_mode, pr.connector_ready, pr.credential_version_id, \
                    cv.version AS credential_version, pr.source_etag, pr.activated_by, \
                    pr.activated_at, stats.model_count, stats.enabled_model_count, \
                    stats.capability_count, stats.certified_capability_count \
             FROM provider_revisions pr \
             LEFT JOIN provider_credential_versions cv ON cv.id = pr.credential_version_id \
             LEFT JOIN LATERAL ( \
                 SELECT COUNT(DISTINCT prm.id)::bigint AS model_count, \
                        COUNT(DISTINCT prm.id) FILTER (WHERE prm.enabled)::bigint \
                          AS enabled_model_count, \
                        COUNT(prc.provider_revision_model_id)::bigint AS capability_count, \
                        COUNT(prc.provider_revision_model_id) \
                          FILTER (WHERE prc.source = 'certified')::bigint \
                          AS certified_capability_count \
                 FROM provider_revision_models prm \
                 LEFT JOIN provider_revision_capabilities prc \
                   ON prc.provider_revision_model_id = prm.id \
                 WHERE prm.provider_revision_id = pr.id \
             ) stats ON true \
             WHERE pr.provider_id = $1 \
             AND ($2::int IS NULL OR pr.revision < $2) \
             ORDER BY pr.revision DESC LIMIT $3",
        )
        .bind(provider_id)
        .bind(before_revision)
        .bind(limit + 1)
        .fetch_all(self.pool())
        .await?;
        let (rows, next_cursor) = split_page(rows, limit as usize, |row| row.get::<Uuid, _>("id"));
        let revisions = rows
            .into_iter()
            .map(provider_revision_catalog_from_row)
            .collect::<Result<Vec<_>, _>>()?;
        Ok(CatalogPage {
            items: revisions,
            next_cursor,
        })
    }

    pub async fn get_provider_revision_catalog(
        &self,
        provider_id: Uuid,
        revision_id: Uuid,
    ) -> Result<ProviderRevisionCatalogRecord, CatalogError> {
        let row = sqlx::query(
            "SELECT pr.id, pr.provider_id, pr.revision, pr.name, pr.kind, pr.endpoint, \
                    pr.cloud_region, pr.cloud_project, pr.deployment, pr.api_version, \
                    pr.auth_mode, pr.connector_ready, pr.credential_version_id, \
                    cv.version AS credential_version, pr.source_etag, pr.activated_by, \
                    pr.activated_at, stats.model_count, stats.enabled_model_count, \
                    stats.capability_count, stats.certified_capability_count \
             FROM provider_revisions pr \
             LEFT JOIN provider_credential_versions cv ON cv.id = pr.credential_version_id \
             LEFT JOIN LATERAL ( \
                 SELECT COUNT(DISTINCT prm.id)::bigint AS model_count, \
                        COUNT(DISTINCT prm.id) FILTER (WHERE prm.enabled)::bigint \
                          AS enabled_model_count, \
                        COUNT(prc.provider_revision_model_id)::bigint AS capability_count, \
                        COUNT(prc.provider_revision_model_id) \
                          FILTER (WHERE prc.source = 'certified')::bigint \
                          AS certified_capability_count \
                 FROM provider_revision_models prm \
                 LEFT JOIN provider_revision_capabilities prc \
                   ON prc.provider_revision_model_id = prm.id \
                 WHERE prm.provider_revision_id = pr.id \
             ) stats ON true \
             WHERE pr.provider_id = $1 AND pr.id = $2",
        )
        .bind(provider_id)
        .bind(revision_id)
        .fetch_optional(self.pool())
        .await?
        .ok_or(CatalogError::NotFound)?;
        provider_revision_catalog_from_row(row)
    }

    pub async fn list_provider_revision_models_catalog(
        &self,
        provider_id: Uuid,
        revision_id: Uuid,
        cursor: Option<Uuid>,
        limit: i64,
    ) -> Result<CatalogPage<ProviderModelRecord>, CatalogError> {
        let limit = checked_limit(limit)?;
        ensure_provider_revision_exists(self, provider_id, revision_id).await?;
        let rows = sqlx::query(
            "SELECT id AS revision_model_id, source_provider_model_id, upstream_model, \
                    display_name, enabled, discovered_at \
             FROM provider_revision_models WHERE provider_revision_id = $1 \
               AND ($2::uuid IS NULL OR id > $2) ORDER BY id LIMIT $3",
        )
        .bind(revision_id)
        .bind(cursor)
        .bind(limit + 1)
        .fetch_all(self.pool())
        .await?;
        let (rows, next_cursor) = split_page(rows, limit as usize, |row| {
            row.get::<Uuid, _>("revision_model_id")
        });
        let items = self.provider_revision_models_from_rows(rows, None).await?;
        Ok(CatalogPage { items, next_cursor })
    }

    async fn provider_models_from_rows(
        &self,
        rows: Vec<PgRow>,
    ) -> Result<Vec<ProviderModelRecord>, CatalogError> {
        let model_ids = rows
            .iter()
            .map(|row| row.get::<Uuid, _>("id"))
            .collect::<Vec<_>>();
        let capability_rows = if model_ids.is_empty() {
            Vec::new()
        } else {
            sqlx::query(
                "SELECT provider_model_id, operation, surface, mode, source, certified_at \
                 FROM model_capabilities WHERE provider_model_id = ANY($1::uuid[]) \
                 ORDER BY provider_model_id, operation, surface, mode",
            )
            .bind(&model_ids)
            .fetch_all(self.pool())
            .await?
        };
        let mut capabilities = BTreeMap::<Uuid, Vec<CapabilityRecord>>::new();
        for row in capability_rows {
            capabilities
                .entry(row.get("provider_model_id"))
                .or_default()
                .push(capability_from_row(&row)?);
        }
        Ok(rows
            .into_iter()
            .map(|row| {
                let id = row.get("id");
                ProviderModelRecord {
                    id,
                    upstream_model: row.get("upstream_model"),
                    display_name: row.get("display_name"),
                    enabled: row.get("enabled"),
                    discovered_at: row.get("discovered_at"),
                    inventory_source: row.get("inventory_source"),
                    availability: row.get("availability"),
                    first_seen_at: row.get("first_seen_at"),
                    last_seen_at: row.get("last_seen_at"),
                    missing_since: row.get("missing_since"),
                    last_certification_status: row.get("last_certification_status"),
                    last_certification_at: row.get("last_certification_at"),
                    capabilities: capabilities.remove(&id).unwrap_or_default(),
                }
            })
            .collect())
    }

    async fn provider_revision_models_from_rows(
        &self,
        rows: Vec<PgRow>,
        capability_limit: Option<usize>,
    ) -> Result<Vec<ProviderModelRecord>, CatalogError> {
        let revision_model_ids = rows
            .iter()
            .map(|row| row.get::<Uuid, _>("revision_model_id"))
            .collect::<Vec<_>>();
        let capability_rows = if revision_model_ids.is_empty() {
            Vec::new()
        } else if let Some(limit) = capability_limit {
            sqlx::query(
                "SELECT provider_revision_model_id, operation, surface, mode, source, certified_at \
                 FROM provider_revision_capabilities \
                 WHERE provider_revision_model_id = ANY($1::uuid[]) \
                 ORDER BY provider_revision_model_id, operation, surface, mode LIMIT $2",
            )
            .bind(&revision_model_ids)
            .bind(limit as i64 + 1)
            .fetch_all(self.pool())
            .await?
        } else {
            sqlx::query(
                "SELECT provider_revision_model_id, operation, surface, mode, source, certified_at \
                 FROM provider_revision_capabilities \
                 WHERE provider_revision_model_id = ANY($1::uuid[]) \
                 ORDER BY provider_revision_model_id, operation, surface, mode",
            )
            .bind(&revision_model_ids)
            .fetch_all(self.pool())
            .await?
        };
        if let Some(limit) = capability_limit {
            enforce_provider_revision_diff_limit(
                capability_rows.len(),
                "capability tuples",
                limit,
            )?;
        }
        let mut capabilities = BTreeMap::<Uuid, Vec<CapabilityRecord>>::new();
        for row in capability_rows {
            capabilities
                .entry(row.get("provider_revision_model_id"))
                .or_default()
                .push(capability_from_row(&row)?);
        }
        Ok(rows
            .into_iter()
            .map(|row| {
                let revision_model_id = row.get("revision_model_id");
                ProviderModelRecord {
                    id: row.get("source_provider_model_id"),
                    upstream_model: row.get("upstream_model"),
                    display_name: row.get("display_name"),
                    enabled: row.get("enabled"),
                    discovered_at: row.get("discovered_at"),
                    inventory_source: "revision".to_owned(),
                    availability: "available".to_owned(),
                    first_seen_at: row.get("discovered_at"),
                    last_seen_at: row.get("discovered_at"),
                    missing_since: None,
                    last_certification_status: None,
                    last_certification_at: None,
                    capabilities: capabilities.remove(&revision_model_id).unwrap_or_default(),
                }
            })
            .collect())
    }

    async fn all_provider_revision_models_catalog(
        &self,
        revision_id: Uuid,
    ) -> Result<Vec<ProviderModelRecord>, CatalogError> {
        let rows = sqlx::query(
            "SELECT id AS revision_model_id, source_provider_model_id, upstream_model, \
                    display_name, enabled, discovered_at \
             FROM provider_revision_models WHERE provider_revision_id = $1 ORDER BY id LIMIT $2",
        )
        .bind(revision_id)
        .bind(PROVIDER_REVISION_DIFF_MODEL_LIMIT as i64 + 1)
        .fetch_all(self.pool())
        .await?;
        enforce_provider_revision_diff_limit(
            rows.len(),
            "models",
            PROVIDER_REVISION_DIFF_MODEL_LIMIT,
        )?;
        self.provider_revision_models_from_rows(rows, Some(PROVIDER_REVISION_DIFF_CAPABILITY_LIMIT))
            .await
    }

    pub async fn diff_provider_revisions_catalog(
        &self,
        provider_id: Uuid,
        from_id: Uuid,
        to_id: Uuid,
    ) -> Result<ProviderRevisionDiff, CatalogError> {
        let from = self
            .get_provider_revision_catalog(provider_id, from_id)
            .await?;
        let to = self
            .get_provider_revision_catalog(provider_id, to_id)
            .await?;
        for revision in [&from, &to] {
            enforce_provider_revision_diff_limit(
                usize::try_from(revision.model_count).unwrap_or(usize::MAX),
                "models",
                PROVIDER_REVISION_DIFF_MODEL_LIMIT,
            )?;
            enforce_provider_revision_diff_limit(
                usize::try_from(revision.capability_count).unwrap_or(usize::MAX),
                "capability tuples",
                PROVIDER_REVISION_DIFF_CAPABILITY_LIMIT,
            )?;
        }
        let from_model_records = self.all_provider_revision_models_catalog(from_id).await?;
        let to_model_records = self.all_provider_revision_models_catalog(to_id).await?;
        let from_models = provider_revision_model_map(&from_model_records);
        let to_models = provider_revision_model_map(&to_model_records);
        let from_capabilities = provider_revision_capability_set(&from_model_records);
        let to_capabilities = provider_revision_capability_set(&to_model_records);
        Ok(ProviderRevisionDiff {
            from_revision: from.revision,
            to_revision: to.revision,
            name_changed: from.name != to.name,
            endpoint_changed: from.endpoint != to.endpoint,
            cloud_context_changed: from.cloud_region != to.cloud_region
                || from.cloud_project != to.cloud_project,
            deployment_changed: from.deployment != to.deployment,
            api_version_changed: from.api_version != to.api_version,
            connector_changed: from.kind != to.kind
                || from.auth_mode != to.auth_mode
                || from.connector_ready != to.connector_ready,
            credential_changed: from.credential_version_id != to.credential_version_id,
            models_added: to_models
                .keys()
                .filter(|model| !from_models.contains_key(*model))
                .cloned()
                .collect(),
            models_removed: from_models
                .keys()
                .filter(|model| !to_models.contains_key(*model))
                .cloned()
                .collect(),
            models_changed: to_models
                .iter()
                .filter_map(|(model, state)| {
                    from_models
                        .get(model)
                        .filter(|previous| *previous != state)
                        .map(|_| model.clone())
                })
                .collect(),
            capabilities_added: to_capabilities
                .difference(&from_capabilities)
                .cloned()
                .collect(),
            capabilities_removed: from_capabilities
                .difference(&to_capabilities)
                .cloned()
                .collect(),
        })
    }

    /// Restores only non-secret provider configuration and declared capability
    /// tuples. The provider's currently selected, non-revoked credential is
    /// preserved; the historical revision credential is never selected.
    pub async fn restore_provider_revision_as_draft(
        &self,
        provider_id: Uuid,
        revision_id: Uuid,
        expected_etag: Uuid,
        actor: Uuid,
        idempotency_key: &str,
    ) -> Result<ProviderCatalogRecord, CatalogError> {
        let revision = self
            .get_provider_revision_catalog(provider_id, revision_id)
            .await?;
        let mut transaction = self.pool().begin().await?;
        if !claim_idempotency(
            &mut transaction,
            actor,
            "provider_revision.restore_as_draft",
            idempotency_key,
        )
        .await?
        {
            return Err(CatalogError::IdempotencyConflict);
        }
        let provider = sqlx::query(
            "SELECT etag, kind, active_credential_version_id \
             FROM providers WHERE id = $1 FOR UPDATE",
        )
        .bind(provider_id)
        .fetch_optional(&mut *transaction)
        .await?
        .ok_or(CatalogError::NotFound)?;
        if provider.get::<Uuid, _>("etag") != expected_etag {
            return Err(CatalogError::PreconditionFailed);
        }
        if provider.get::<String, _>("kind") != revision.kind.as_str() {
            return Err(CatalogError::Invalid(
                "a historical revision cannot change the provider connector kind".to_owned(),
            ));
        }
        let selected_credential: Option<Uuid> = provider.get("active_credential_version_id");
        let selected_credential = if let Some(credential_id) = selected_credential {
            sqlx::query_scalar::<_, Uuid>(
                "SELECT id FROM provider_credential_versions \
                 WHERE id = $1 AND provider_id = $2 AND revoked_at IS NULL",
            )
            .bind(credential_id)
            .bind(provider_id)
            .fetch_optional(&mut *transaction)
            .await?
        } else {
            None
        };
        let etag = Uuid::now_v7();
        sqlx::query(
            "UPDATE providers SET name = $1, endpoint = $2, cloud_region = $3, \
                    cloud_project = $4, deployment = $5, api_version = $6, auth_mode = $7, \
                    connector_ready = $8, active_credential_version_id = $9, \
                    state = 'draft'::provider_state, etag = $10, certification_context_id = uuidv7(), \
                    updated_at = now(), last_probe_at = NULL, last_probe_status = NULL, \
                    last_probe_detail = NULL, last_probe_context_id = NULL \
             WHERE id = $11",
        )
        .bind(&revision.name)
        .bind(&revision.endpoint)
        .bind(&revision.cloud_region)
        .bind(&revision.cloud_project)
        .bind(&revision.deployment)
        .bind(&revision.api_version)
        .bind(revision.auth_mode.as_str())
        .bind(revision.connector_ready)
        .bind(selected_credential)
        .bind(etag)
        .bind(provider_id)
        .execute(&mut *transaction)
        .await?;
        sqlx::query(
            "UPDATE provider_models SET enabled = false, review_revision = uuidv7() \
             WHERE provider_id = $1",
        )
        .bind(provider_id)
        .execute(&mut *transaction)
        .await?;
        sqlx::query(
            "UPDATE provider_models pm SET upstream_model = prm.upstream_model, \
                    display_name = prm.display_name, enabled = prm.enabled, \
                    discovered_at = prm.discovered_at \
             FROM provider_revision_models prm \
             WHERE prm.provider_revision_id = $1 \
               AND pm.id = prm.source_provider_model_id AND pm.provider_id = $2",
        )
        .bind(revision_id)
        .bind(provider_id)
        .execute(&mut *transaction)
        .await?;
        sqlx::query(
            "DELETE FROM model_capabilities WHERE provider_model_id IN \
               (SELECT id FROM provider_models WHERE provider_id = $1)",
        )
        .bind(provider_id)
        .execute(&mut *transaction)
        .await?;
        sqlx::query(
            "INSERT INTO model_capabilities \
               (provider_model_id, operation, surface, mode, source, certified_at) \
             SELECT prm.source_provider_model_id, prc.operation, prc.surface, prc.mode, \
                    'declared', NULL \
             FROM provider_revision_models prm \
             JOIN provider_revision_capabilities prc \
               ON prc.provider_revision_model_id = prm.id \
             WHERE prm.provider_revision_id = $1",
        )
        .bind(revision_id)
        .execute(&mut *transaction)
        .await?;
        audit_in_transaction(
            &mut transaction,
            actor,
            "provider_revision.restore_as_draft",
            "provider",
            provider_id,
            "success",
        )
        .await?;
        complete_idempotency(
            &mut transaction,
            actor,
            "provider_revision.restore_as_draft",
            idempotency_key,
            &provider_id.to_string(),
        )
        .await?;
        transaction.commit().await?;
        let restored = self.get_provider_catalog(provider_id).await?;
        debug_assert_eq!(restored.etag, etag);
        Ok(restored)
    }

    pub async fn update_provider_catalog(
        &self,
        provider_id: Uuid,
        expected_etag: Uuid,
        update: &UpdateProviderCatalog,
        actor: Uuid,
    ) -> Result<Uuid, CatalogError> {
        validate_provider_update(update)?;
        let etag = Uuid::now_v7();
        let mut transaction = self.pool().begin().await?;
        let result = sqlx::query(
            "UPDATE providers SET name = $1, endpoint = $2, cloud_region = $3, cloud_project = $4, \
                    deployment = $5, api_version = $6, auth_mode = $7, \
                    active_credential_version_id = CASE \
                      WHEN $7 IN ('adc', 'default_chain') THEN NULL \
                      ELSE active_credential_version_id END, \
                    state = 'draft'::provider_state, etag = $8, certification_context_id = uuidv7(), \
                    updated_at = now(), last_probe_at = NULL, last_probe_status = NULL, \
                    last_probe_detail = NULL, last_probe_context_id = NULL \
             WHERE id = $9 AND etag = $10 AND state <> 'disabled'::provider_state",
        )
        .bind(update.name.trim())
        .bind(update.endpoint.as_deref().map(str::trim))
        .bind(update.cloud_region.as_deref().map(str::trim))
        .bind(update.cloud_project.as_deref().map(str::trim))
        .bind(update.deployment.as_deref().map(str::trim))
        .bind(update.api_version.as_deref().map(str::trim))
        .bind(update.auth_mode.as_str())
        .bind(etag)
        .bind(provider_id)
        .bind(expected_etag)
        .execute(&mut *transaction)
        .await?;
        if result.rows_affected() != 1 {
            let current =
                sqlx::query("SELECT etag, state::text AS state FROM providers WHERE id = $1")
                    .bind(provider_id)
                    .fetch_optional(&mut *transaction)
                    .await?
                    .ok_or(CatalogError::NotFound)?;
            return Err(if current.get::<Uuid, _>("etag") != expected_etag {
                CatalogError::PreconditionFailed
            } else {
                CatalogError::InUse
            });
        }
        clear_provider_model_capability_evidence(&mut transaction, provider_id).await?;
        audit_in_transaction(
            &mut transaction,
            actor,
            "provider.update",
            "provider",
            provider_id,
            "success",
        )
        .await?;
        transaction.commit().await?;
        Ok(etag)
    }

    pub async fn disable_provider_catalog(
        &self,
        provider_id: Uuid,
        expected_etag: Uuid,
        actor: Uuid,
        idempotency_key: &str,
    ) -> Result<ProviderMutationResult, CatalogError> {
        let mut transaction = self
            .pool()
            .begin_with("BEGIN ISOLATION LEVEL READ COMMITTED")
            .await?;
        prepare_runtime_mutation(&mut transaction).await?;
        if !claim_idempotency(&mut transaction, actor, "provider.disable", idempotency_key).await? {
            return Err(CatalogError::IdempotencyConflict);
        }
        // Serialize against the short reservation INSERT so the decision and
        // runtime publication cannot race a newly committed upstream job.
        sqlx::query("LOCK TABLE async_media_jobs IN SHARE MODE")
            .execute(&mut *transaction)
            .await?;
        let has_live_media_jobs: bool = sqlx::query_scalar(
            "SELECT EXISTS (SELECT 1 FROM async_media_jobs
             WHERE provider_id = $1 AND lifecycle_state <> 'deleted')",
        )
        .bind(provider_id)
        .fetch_one(&mut *transaction)
        .await?;
        if has_live_media_jobs {
            return Err(CatalogError::InUse);
        }
        let referenced: bool = sqlx::query_scalar(
            "SELECT EXISTS ( \
               SELECT 1 FROM routes r \
               JOIN LATERAL (SELECT id FROM route_revisions WHERE route_id = r.id \
                             ORDER BY revision DESC LIMIT 1) rr ON true \
               JOIN route_revision_targets rt ON rt.route_revision_id = rr.id \
               JOIN provider_models pm ON pm.id = rt.provider_model_id \
               WHERE pm.provider_id = $1 \
             )",
        )
        .bind(provider_id)
        .fetch_one(&mut *transaction)
        .await?;
        if referenced {
            return Err(CatalogError::InUse);
        }
        let etag = Uuid::now_v7();
        let updated = sqlx::query(
            "UPDATE providers SET state = 'disabled'::provider_state, active_revision_id = NULL, \
                    etag = $1, updated_at = now() \
             WHERE id = $2 AND etag = $3 AND state <> 'disabled'::provider_state \
               AND active_revision_id IS NOT NULL",
        )
        .bind(etag)
        .bind(provider_id)
        .bind(expected_etag)
        .execute(&mut *transaction)
        .await?;
        if updated.rows_affected() != 1 {
            let row = sqlx::query("SELECT etag FROM providers WHERE id = $1")
                .bind(provider_id)
                .fetch_optional(&mut *transaction)
                .await?
                .ok_or(CatalogError::NotFound)?;
            return Err(if row.get::<Uuid, _>("etag") != expected_etag {
                CatalogError::PreconditionFailed
            } else {
                CatalogError::InUse
            });
        }
        audit_in_transaction(
            &mut transaction,
            actor,
            "provider.disable",
            "provider",
            provider_id,
            "success",
        )
        .await?;
        complete_idempotency(
            &mut transaction,
            actor,
            "provider.disable",
            idempotency_key,
            &provider_id.to_string(),
        )
        .await?;
        let release = compile_and_publish_runtime_in_transaction(&mut transaction, actor).await?;
        transaction.commit().await?;
        Ok(ProviderMutationResult {
            etag,
            release: Some(release),
        })
    }

    pub async fn restore_provider_as_draft_catalog(
        &self,
        provider_id: Uuid,
        expected_etag: Uuid,
        actor: Uuid,
        idempotency_key: &str,
    ) -> Result<Uuid, CatalogError> {
        let mut transaction = self.pool().begin().await?;
        if !claim_idempotency(
            &mut transaction,
            actor,
            "provider.restore_as_draft",
            idempotency_key,
        )
        .await?
        {
            return Err(CatalogError::IdempotencyConflict);
        }
        let etag = Uuid::now_v7();
        let updated = sqlx::query(
            "UPDATE providers SET state = 'draft'::provider_state, etag = $1, \
                    certification_context_id = uuidv7(), updated_at = now(), last_probe_at = NULL, \
                    last_probe_status = NULL, last_probe_detail = NULL, last_probe_context_id = NULL \
             WHERE id = $2 AND etag = $3 AND state = 'disabled'::provider_state",
        )
        .bind(etag)
        .bind(provider_id)
        .bind(expected_etag)
        .execute(&mut *transaction)
        .await?;
        if updated.rows_affected() != 1 {
            let row = sqlx::query("SELECT etag FROM providers WHERE id = $1")
                .bind(provider_id)
                .fetch_optional(&mut *transaction)
                .await?
                .ok_or(CatalogError::NotFound)?;
            return Err(if row.get::<Uuid, _>("etag") != expected_etag {
                CatalogError::PreconditionFailed
            } else {
                CatalogError::InUse
            });
        }
        clear_provider_model_capability_evidence(&mut transaction, provider_id).await?;
        audit_in_transaction(
            &mut transaction,
            actor,
            "provider.restore_as_draft",
            "provider",
            provider_id,
            "success",
        )
        .await?;
        complete_idempotency(
            &mut transaction,
            actor,
            "provider.restore_as_draft",
            idempotency_key,
            &provider_id.to_string(),
        )
        .await?;
        transaction.commit().await?;
        Ok(etag)
    }

    /// Returns the next candidate version without enforcing an HTTP
    /// precondition. The transactional rotation still checks both the ETag and
    /// candidate version after claiming idempotency; this allows an identical
    /// retry with the original ETag to reach its persisted replay response.
    pub async fn next_credential_version_candidate(
        &self,
        provider_id: Uuid,
    ) -> Result<u32, CatalogError> {
        let next_version: i32 = sqlx::query_scalar(
            "SELECT COALESCE(max(cv.version), 0) + 1 \
             FROM providers p LEFT JOIN provider_credential_versions cv ON cv.provider_id = p.id \
             WHERE p.id = $1 GROUP BY p.id",
        )
        .bind(provider_id)
        .fetch_optional(self.pool())
        .await?
        .ok_or(CatalogError::NotFound)?;
        u32::try_from(next_version)
            .map_err(|_| CatalogError::Invalid("credential version overflow".to_owned()))
    }

    pub async fn rotate_provider_credential<F>(
        &self,
        provider_id: Uuid,
        input: RotateCredentialInput,
        replay: ReplayableIdempotency<'_>,
        build_response: F,
    ) -> Result<IdempotencyOutcome<ProviderMutationResult>, CatalogError>
    where
        F: FnOnce(&ProviderMutationResult) -> Result<IdempotencyResponse, PersistenceError>,
    {
        let mut transaction = self
            .pool()
            .begin_with("BEGIN ISOLATION LEVEL READ COMMITTED")
            .await?;
        match claim_replayable_idempotency(
            &mut transaction,
            input.actor,
            "provider.rotate_credential",
            &input.idempotency_key,
            replay.request_fingerprint(),
            replay.master_key(),
        )
        .await?
        {
            ReplayableIdempotencyClaim::Execute => {
                prepare_runtime_mutation(&mut transaction).await?;
            }
            ReplayableIdempotencyClaim::Replay(response) => {
                transaction.rollback().await?;
                return Ok(IdempotencyOutcome::Replayed(response));
            }
            ReplayableIdempotencyClaim::Conflict => {
                transaction.rollback().await?;
                return Err(CatalogError::IdempotencyConflict);
            }
            ReplayableIdempotencyClaim::InProgress => {
                transaction.rollback().await?;
                return Err(CatalogError::IdempotencyInProgress);
            }
        }
        let database_version = i32::try_from(input.version)
            .ok()
            .filter(|value| *value > 0)
            .ok_or_else(|| CatalogError::Invalid("credential version is invalid".to_owned()))?;
        let key_version = i32::try_from(input.encrypted.key_version)
            .ok()
            .filter(|value| *value > 0)
            .ok_or_else(|| CatalogError::Invalid("master-key version is invalid".to_owned()))?;
        let provider = sqlx::query(
            "SELECT etag, state::text AS state, COALESCE((SELECT max(version) FROM \
             provider_credential_versions WHERE provider_id = $1), 0) + 1 AS next_version \
             FROM providers WHERE id = $1 FOR UPDATE",
        )
        .bind(provider_id)
        .fetch_optional(&mut *transaction)
        .await?
        .ok_or(CatalogError::NotFound)?;
        if provider.get::<Uuid, _>("etag") != input.expected_etag {
            return Err(CatalogError::PreconditionFailed);
        }
        if provider.get::<String, _>("state") == "disabled" {
            return Err(CatalogError::InUse);
        }
        if provider.get::<i32, _>("next_version") != database_version {
            return Err(CatalogError::PreconditionFailed);
        }
        sqlx::query(
            "INSERT INTO provider_credential_versions \
             (id, provider_id, version, ciphertext, nonce, master_key_version, created_by) \
             VALUES ($1, $2, $3, $4, $5, $6, $7)",
        )
        .bind(input.credential_id)
        .bind(provider_id)
        .bind(database_version)
        .bind(&input.encrypted.ciphertext)
        .bind(input.encrypted.nonce.to_vec())
        .bind(key_version)
        .bind(input.actor)
        .execute(&mut *transaction)
        .await?;
        let etag = Uuid::now_v7();
        sqlx::query(
            "UPDATE providers SET active_credential_version_id = $1, \
                    state = 'draft'::provider_state, etag = $2, certification_context_id = uuidv7(), \
                    updated_at = now(), last_probe_at = NULL, last_probe_status = NULL, \
                    last_probe_detail = NULL, last_probe_context_id = NULL \
             WHERE id = $3",
        )
        .bind(input.credential_id)
        .bind(etag)
        .bind(provider_id)
        .execute(&mut *transaction)
        .await?;
        clear_provider_model_capability_evidence(&mut transaction, provider_id).await?;
        audit_in_transaction(
            &mut transaction,
            input.actor,
            "provider.rotate_credential",
            "provider",
            provider_id,
            "success",
        )
        .await?;
        let result = ProviderMutationResult {
            etag,
            release: None,
        };
        let response = build_response(&result)?;
        complete_replayable_idempotency(
            &mut transaction,
            input.actor,
            "provider.rotate_credential",
            &input.idempotency_key,
            replay.request_fingerprint(),
            replay.master_key(),
            &response,
        )
        .await?;
        transaction.commit().await?;
        Ok(IdempotencyOutcome::Executed {
            value: result,
            response,
        })
    }

    pub async fn list_provider_credentials(
        &self,
        provider_id: Uuid,
        cursor: Option<Uuid>,
        limit: i64,
    ) -> Result<CatalogPage<CredentialVersionRecord>, CatalogError> {
        let limit = checked_limit(limit)?;
        let exists: bool =
            sqlx::query_scalar("SELECT EXISTS (SELECT 1 FROM providers WHERE id = $1)")
                .bind(provider_id)
                .fetch_one(self.pool())
                .await?;
        if !exists {
            return Err(CatalogError::NotFound);
        }
        let before_version: Option<i32> = match cursor {
            Some(cursor) => Some(
                sqlx::query_scalar(
                    "SELECT version FROM provider_credential_versions \
                     WHERE provider_id = $1 AND id = $2",
                )
                .bind(provider_id)
                .bind(cursor)
                .fetch_optional(self.pool())
                .await?
                .ok_or_else(|| {
                    CatalogError::Invalid("credential pagination cursor is invalid".to_owned())
                })?,
            ),
            None => None,
        };
        let items = sqlx::query(
            "SELECT cv.id, cv.version, cv.id = ar.credential_version_id AS active, \
                    (p.state = 'draft'::provider_state \
                     AND cv.id = p.active_credential_version_id) AS draft_selected, \
                    cv.created_at, cv.revoked_at FROM provider_credential_versions cv \
             JOIN providers p ON p.id = cv.provider_id \
             LEFT JOIN provider_revisions ar ON ar.id = p.active_revision_id \
             WHERE cv.provider_id = $1 \
             AND ($2::int IS NULL OR cv.version < $2) \
             ORDER BY cv.version DESC LIMIT $3",
        )
        .bind(provider_id)
        .bind(before_version)
        .bind(limit + 1)
        .fetch_all(self.pool())
        .await?
        .into_iter()
        .map(|row| CredentialVersionRecord {
            id: row.get("id"),
            version: row.get("version"),
            active: row.get("active"),
            draft_selected: row.get("draft_selected"),
            created_at: row.get("created_at"),
            revoked_at: row.get("revoked_at"),
        })
        .collect::<Vec<_>>();
        let (items, next_cursor) = split_page(items, limit as usize, |item| item.id);
        Ok(CatalogPage { items, next_cursor })
    }

    pub async fn active_provider_credential_secret(
        &self,
        provider_id: Uuid,
    ) -> Result<StoredCredentialSecret, CatalogError> {
        let row = sqlx::query(
            "SELECT cv.id, cv.version, cv.ciphertext, cv.nonce, cv.master_key_version \
             FROM providers p JOIN provider_credential_versions cv \
               ON cv.id = p.active_credential_version_id \
             WHERE p.id = $1 AND cv.revoked_at IS NULL",
        )
        .bind(provider_id)
        .fetch_optional(self.pool())
        .await?
        .ok_or(CatalogError::NotFound)?;
        let nonce: Vec<u8> = row.get("nonce");
        let nonce: [u8; 12] = nonce
            .try_into()
            .map_err(|_| CatalogError::Invalid("stored credential nonce is invalid".to_owned()))?;
        let version = u32::try_from(row.get::<i32, _>("version")).map_err(|_| {
            CatalogError::Invalid("stored credential version is invalid".to_owned())
        })?;
        let key_version = u32::try_from(row.get::<i32, _>("master_key_version")).map_err(|_| {
            CatalogError::Invalid("stored master-key version is invalid".to_owned())
        })?;
        Ok(StoredCredentialSecret {
            id: row.get("id"),
            version,
            encrypted: EncryptedSecret {
                key_version,
                nonce,
                ciphertext: row.get("ciphertext"),
            },
        })
    }

    pub async fn revoke_provider_credential(
        &self,
        provider_id: Uuid,
        credential_id: Uuid,
        expected_etag: Uuid,
        actor: Uuid,
        idempotency_key: &str,
    ) -> Result<Uuid, CatalogError> {
        let mut transaction = self.pool().begin().await?;
        if !claim_idempotency(
            &mut transaction,
            actor,
            "provider.revoke_credential",
            idempotency_key,
        )
        .await?
        {
            return Err(CatalogError::IdempotencyConflict);
        }
        let provider = sqlx::query(
            "SELECT p.etag, p.active_credential_version_id, \
                    ar.credential_version_id AS activated_credential_version_id \
             FROM providers p LEFT JOIN provider_revisions ar ON ar.id = p.active_revision_id \
             WHERE p.id = $1 FOR UPDATE OF p",
        )
        .bind(provider_id)
        .fetch_optional(&mut *transaction)
        .await?
        .ok_or(CatalogError::NotFound)?;
        if provider.get::<Uuid, _>("etag") != expected_etag {
            return Err(CatalogError::PreconditionFailed);
        }
        if provider.get::<Option<Uuid>, _>("active_credential_version_id") == Some(credential_id)
            || provider.get::<Option<Uuid>, _>("activated_credential_version_id")
                == Some(credential_id)
        {
            return Err(CatalogError::InUse);
        }
        // Historic jobs carry their immutable provider revision. Even an
        // otherwise inactive credential remains lifecycle authority until
        // every job that used it has a durable deletion tombstone.
        let used_by_live_media_job: bool = sqlx::query_scalar(
            "SELECT EXISTS (
               SELECT 1 FROM async_media_jobs j
               JOIN provider_revisions pr ON pr.id = j.provider_revision_id
               WHERE j.provider_id = $1 AND j.lifecycle_state <> 'deleted'
                 AND pr.credential_version_id = $2
             )",
        )
        .bind(provider_id)
        .bind(credential_id)
        .fetch_one(&mut *transaction)
        .await?;
        if used_by_live_media_job {
            return Err(CatalogError::InUse);
        }
        let result = sqlx::query(
            "UPDATE provider_credential_versions SET revoked_at = COALESCE(revoked_at, now()) \
             WHERE id = $1 AND provider_id = $2",
        )
        .bind(credential_id)
        .bind(provider_id)
        .execute(&mut *transaction)
        .await?;
        if result.rows_affected() != 1 {
            return Err(CatalogError::NotFound);
        }
        let etag = Uuid::now_v7();
        sqlx::query("UPDATE providers SET etag = $1, updated_at = now() WHERE id = $2")
            .bind(etag)
            .bind(provider_id)
            .execute(&mut *transaction)
            .await?;
        audit_in_transaction(
            &mut transaction,
            actor,
            "provider.revoke_credential",
            "provider",
            provider_id,
            "success",
        )
        .await?;
        complete_idempotency(
            &mut transaction,
            actor,
            "provider.revoke_credential",
            idempotency_key,
            &credential_id.to_string(),
        )
        .await?;
        transaction.commit().await?;
        Ok(etag)
    }

    pub async fn record_provider_probe(
        &self,
        provider_id: Uuid,
        expected_etag: Uuid,
        succeeded: bool,
        detail: &str,
        actor: Uuid,
    ) -> Result<DateTime<Utc>, CatalogError> {
        let detail = detail.trim();
        if detail.chars().count() > 500 {
            return Err(CatalogError::Invalid(
                "probe detail exceeds 500 characters".to_owned(),
            ));
        }
        let at = Utc::now();
        let mut transaction = self.pool().begin().await?;
        let result = sqlx::query(
            "UPDATE providers SET last_probe_at = $1, last_probe_status = $2, \
                    last_probe_detail = $3, last_probe_context_id = certification_context_id \
             WHERE id = $4 AND etag = $5",
        )
        .bind(at)
        .bind(if succeeded { "succeeded" } else { "failed" })
        .bind(detail)
        .bind(provider_id)
        .bind(expected_etag)
        .execute(&mut *transaction)
        .await?;
        if result.rows_affected() != 1 {
            let current_etag: Option<Uuid> =
                sqlx::query_scalar("SELECT etag FROM providers WHERE id = $1")
                    .bind(provider_id)
                    .fetch_optional(&mut *transaction)
                    .await?;
            return Err(if current_etag.is_some() {
                CatalogError::PreconditionFailed
            } else {
                CatalogError::NotFound
            });
        }
        audit_in_transaction(
            &mut transaction,
            actor,
            "provider.probe",
            "provider",
            provider_id,
            if succeeded { "success" } else { "failure" },
        )
        .await?;
        transaction.commit().await?;
        Ok(at)
    }

    /// Applies an inventory observation without rewriting reviewed models or
    /// their certification evidence. Only a complete, non-manual observation
    /// can make a previously upstream-confirmed model missing.
    pub async fn reconcile_provider_model_discovery(
        &self,
        input: ReconcileProviderModelDiscoveryInput<'_>,
    ) -> Result<ProviderModelDiscoveryApplied, CatalogError> {
        let ReconcileProviderModelDiscoveryInput {
            provider_id,
            expected_etag,
            models,
            origin,
            completeness,
            actor,
            claim_id,
        } = input;
        if origin == ProviderModelDiscoveryOrigin::Manual && models.is_empty() {
            return Err(CatalogError::Invalid(
                "manual model declaration requires at least one model".to_owned(),
            ));
        }
        let mut names: BTreeSet<&str> = BTreeSet::new();
        for model in models {
            validate_model(model)?;
            if !names.insert(model.upstream_model.trim()) {
                return Err(CatalogError::Invalid(
                    "model names must be unique".to_owned(),
                ));
            }
        }

        let now = Utc::now();
        let mut transaction = self.pool().begin().await?;
        let provider = sqlx::query(
            "SELECT etag, state::text AS state, kind, model_discovery_claim_id \
             FROM providers WHERE id = $1 FOR UPDATE",
        )
        .bind(provider_id)
        .fetch_optional(&mut *transaction)
        .await?
        .ok_or(CatalogError::NotFound)?;
        if provider.get::<Uuid, _>("etag") != expected_etag {
            return Err(CatalogError::PreconditionFailed);
        }
        if provider.get::<String, _>("state") == "disabled" {
            return Err(CatalogError::InUse);
        }
        if let Some(claim_id) = claim_id
            && provider.get::<Option<Uuid>, _>("model_discovery_claim_id") != Some(claim_id)
        {
            return Err(CatalogError::PreconditionFailed);
        }
        let existing = sqlx::query(
            "SELECT id, upstream_model, display_name, inventory_source, availability, \
                    consecutive_missing_runs \
             FROM provider_models WHERE provider_id = $1 FOR UPDATE",
        )
        .bind(provider_id)
        .fetch_all(&mut *transaction)
        .await?
        .into_iter()
        .map(|row| {
            Ok::<_, CatalogError>((
                row.get::<String, _>("upstream_model"),
                (
                    row.get::<Uuid, _>("id"),
                    row.get::<String, _>("display_name"),
                    row.get::<String, _>("inventory_source"),
                    row.get::<String, _>("availability"),
                    row.get::<i32, _>("consecutive_missing_runs"),
                ),
            ))
        })
        .collect::<Result<BTreeMap<_, _>, _>>()?;

        let automatic_source = origin != ProviderModelDiscoveryOrigin::Manual;
        let source = if automatic_source {
            "upstream"
        } else {
            "manual"
        };
        let mut added_model_count = 0_usize;
        let mut renamed_model_count = 0_usize;
        let mut missing_model_count = 0_usize;
        let mut catalog_changed = false;
        let mut stage_draft = false;

        for model in models {
            let model_name = model.upstream_model.trim();
            let display_name = model.display_name.trim();
            if let Some((model_id, existing_display_name, inventory_source, availability, _)) =
                existing.get(model_name)
            {
                let renamed = existing_display_name != display_name;
                let reappeared = availability == "missing";
                let source_changed = (automatic_source && inventory_source != "upstream")
                    || (!automatic_source && inventory_source == "legacy");
                if renamed {
                    renamed_model_count += 1;
                    catalog_changed = true;
                }
                if source_changed {
                    catalog_changed = true;
                }
                if reappeared {
                    // An absence followed by a reappearance is not proof that
                    // the old capability behavior is still valid.
                    clear_model_capability_evidence(&mut transaction, *model_id).await?;
                    sqlx::query(
                        "UPDATE provider_models SET review_revision = uuidv7() WHERE id = $1",
                    )
                    .bind(model_id)
                    .execute(&mut *transaction)
                    .await?;
                    catalog_changed = true;
                    stage_draft = true;
                }
                sqlx::query(
                    "UPDATE provider_models SET display_name = $1, \
                            inventory_source = CASE \
                                WHEN $2 = 'upstream' THEN 'upstream' \
                                WHEN inventory_source = 'legacy' THEN 'manual' \
                                ELSE inventory_source END, \
                            availability = 'available', first_seen_at = COALESCE(first_seen_at, $3), \
                            last_seen_at = $3, missing_since = NULL, consecutive_missing_runs = 0, \
                            discovered_at = COALESCE(discovered_at, $3) \
                     WHERE id = $4",
                )
                .bind(display_name)
                .bind(source)
                .bind(now)
                .bind(model_id)
                .execute(&mut *transaction)
                .await?;
            } else {
                sqlx::query(
                    "INSERT INTO provider_models \
                     (id, provider_id, upstream_model, display_name, enabled, discovered_at, \
                      inventory_source, availability, first_seen_at, last_seen_at) \
                     VALUES ($1, $2, $3, $4, false, $5, $6, 'available', $5, $5)",
                )
                .bind(Uuid::now_v7())
                .bind(provider_id)
                .bind(model_name)
                .bind(display_name)
                .bind(now)
                .bind(source)
                .execute(&mut *transaction)
                .await?;
                added_model_count += 1;
                catalog_changed = true;
            }
        }

        if automatic_source && completeness.is_complete() {
            for (model_name, (model_id, _, inventory_source, availability, misses)) in &existing {
                if !matches!(
                    inventory_source.as_str(),
                    "upstream" | "configured" | "legacy"
                ) || names.contains(model_name.as_str())
                {
                    continue;
                }
                let next_misses = misses.saturating_add(1);
                let newly_missing = availability != "missing" && next_misses >= 2;
                if inventory_source != "upstream" {
                    catalog_changed = true;
                }
                sqlx::query(
                    "UPDATE provider_models SET inventory_source = 'upstream', \
                            consecutive_missing_runs = $1, \
                            missing_since = COALESCE(missing_since, $2), \
                            availability = CASE WHEN $1 >= 2 THEN 'missing' ELSE availability END, \
                            review_revision = CASE \
                                WHEN availability <> 'missing' AND $1 >= 2 THEN uuidv7() \
                                ELSE review_revision END \
                     WHERE id = $3",
                )
                .bind(next_misses)
                .bind(now)
                .bind(model_id)
                .execute(&mut *transaction)
                .await?;
                if newly_missing {
                    // Preserve evidence in the immutable active revision, but
                    // do not advertise it as current mutable-draft evidence.
                    clear_model_capability_evidence(&mut transaction, *model_id).await?;
                    missing_model_count += 1;
                    catalog_changed = true;
                    stage_draft = true;
                }
            }
        }

        let etag = if catalog_changed {
            let etag = Uuid::now_v7();
            sqlx::query(
                "UPDATE providers SET etag = $1, \
                        state = CASE WHEN $2 THEN 'draft'::provider_state ELSE state END, \
                        updated_at = now(), last_model_discovery_at = $3, \
                        last_model_discovery_status = 'succeeded', \
                        model_discovery_claim_id = NULL, model_discovery_claimed_until = NULL \
                 WHERE id = $4",
            )
            .bind(etag)
            .bind(stage_draft)
            .bind(now)
            .bind(provider_id)
            .execute(&mut *transaction)
            .await?;
            etag
        } else {
            sqlx::query(
                "UPDATE providers SET last_model_discovery_at = $1, \
                        last_model_discovery_status = 'succeeded', \
                        model_discovery_claim_id = NULL, model_discovery_claimed_until = NULL \
                 WHERE id = $2",
            )
            .bind(now)
            .bind(provider_id)
            .execute(&mut *transaction)
            .await?;
            expected_etag
        };
        sqlx::query(
            "INSERT INTO provider_model_discovery_runs \
             (id, provider_id, actor_user_id, origin, completeness, status, expected_etag, \
              observed_model_count, added_model_count, renamed_model_count, missing_model_count, \
              started_at, completed_at) \
             VALUES ($1, $2, $3, $4, $5, 'succeeded', $6, $7, $8, $9, $10, $11, $11)",
        )
        .bind(Uuid::now_v7())
        .bind(provider_id)
        .bind(actor)
        .bind(origin.as_str())
        .bind(completeness.as_str())
        .bind(expected_etag)
        .bind(
            i32::try_from(models.len()).map_err(|_| {
                CatalogError::Invalid("discovery model count is invalid".to_owned())
            })?,
        )
        .bind(i32::try_from(added_model_count).expect("model count fits i32"))
        .bind(i32::try_from(renamed_model_count).expect("model count fits i32"))
        .bind(i32::try_from(missing_model_count).expect("model count fits i32"))
        .bind(now)
        .execute(&mut *transaction)
        .await?;
        audit_optional_in_transaction(
            &mut transaction,
            actor,
            "provider.model_discovery",
            "provider",
            provider_id,
            "success",
        )
        .await?;
        transaction.commit().await?;
        Ok(ProviderModelDiscoveryApplied {
            etag,
            completed_at: now,
            observed_model_count: models.len(),
            added_model_count,
            renamed_model_count,
            newly_missing_model_count: missing_model_count,
            completeness,
        })
    }

    /// Compatibility wrapper for storage callers that previously performed a
    /// destructive discovery replacement. It now behaves as an additive manual
    /// declaration and preserves reviewed capability evidence.
    pub async fn discover_provider_models(
        &self,
        provider_id: Uuid,
        expected_etag: Uuid,
        models: &[DiscoveredModelInput],
        actor: Uuid,
    ) -> Result<Uuid, CatalogError> {
        self.reconcile_provider_model_discovery(ReconcileProviderModelDiscoveryInput {
            provider_id,
            expected_etag,
            models,
            origin: ProviderModelDiscoveryOrigin::Manual,
            completeness: ProviderModelDiscoveryCompleteness::Partial,
            actor: Some(actor),
            claim_id: None,
        })
        .await
        .map(|result| result.etag)
    }

    /// Claims a bounded set of providers due for automatic inventory refresh.
    /// The lease is database-backed so horizontally scaled control-plane pods
    /// cannot issue duplicate credentialed catalog calls for one provider.
    pub async fn claim_due_model_discoveries(
        &self,
        due_before: DateTime<Utc>,
        claim_until: DateTime<Utc>,
        limit: i64,
    ) -> Result<Vec<ScheduledModelDiscoveryClaim>, CatalogError> {
        if !(1..=16).contains(&limit) {
            return Err(CatalogError::Invalid(
                "model discovery claim limit must be between 1 and 16".to_owned(),
            ));
        }
        let mut transaction = self.pool().begin().await?;
        let rows = sqlx::query(
            "WITH candidates AS ( \
                SELECT id FROM providers \
                WHERE state <> 'disabled'::provider_state \
                  AND kind IN ('open_ai', 'anthropic', 'gemini') \
                  AND (last_model_discovery_at IS NULL OR last_model_discovery_at <= $1) \
                  AND (model_discovery_claimed_until IS NULL OR model_discovery_claimed_until <= $2) \
                ORDER BY last_model_discovery_at NULLS FIRST, id \
                FOR UPDATE SKIP LOCKED LIMIT $3 \
              ) \
              UPDATE providers p SET model_discovery_claim_id = uuidv7(), \
                  model_discovery_claimed_until = $4 \
              FROM candidates c WHERE p.id = c.id \
              RETURNING p.id, p.etag, p.model_discovery_claim_id",
        )
        .bind(due_before)
        .bind(Utc::now())
        .bind(limit)
        .bind(claim_until)
        .fetch_all(&mut *transaction)
        .await?;
        transaction.commit().await?;
        Ok(rows
            .into_iter()
            .map(|row| ScheduledModelDiscoveryClaim {
                provider_id: row.get("id"),
                expected_etag: row.get("etag"),
                claim_id: row
                    .get::<Option<Uuid>, _>("model_discovery_claim_id")
                    .expect("claimed provider always has a claim ID"),
            })
            .collect())
    }

    pub async fn record_scheduled_model_discovery_failure(
        &self,
        claim: &ScheduledModelDiscoveryClaim,
        detail: &str,
    ) -> Result<(), CatalogError> {
        self.finalize_scheduled_model_discovery(claim, "failed", detail)
            .await
    }

    pub async fn record_scheduled_model_discovery_superseded(
        &self,
        claim: &ScheduledModelDiscoveryClaim,
        detail: &str,
    ) -> Result<(), CatalogError> {
        self.finalize_scheduled_model_discovery(claim, "superseded", detail)
            .await
    }

    async fn finalize_scheduled_model_discovery(
        &self,
        claim: &ScheduledModelDiscoveryClaim,
        status: &'static str,
        detail: &str,
    ) -> Result<(), CatalogError> {
        let detail = detail.trim().chars().take(500).collect::<String>();
        let now = Utc::now();
        let mut transaction = self.pool().begin().await?;
        let provider = sqlx::query(
            "SELECT etag, model_discovery_claim_id FROM providers WHERE id = $1 FOR UPDATE",
        )
        .bind(claim.provider_id)
        .fetch_optional(&mut *transaction)
        .await?;
        let Some(provider) = provider else {
            return Err(CatalogError::NotFound);
        };
        let etag_matches = provider.get::<Uuid, _>("etag") == claim.expected_etag;
        let claim_id_matches =
            provider.get::<Option<Uuid>, _>("model_discovery_claim_id") == Some(claim.claim_id);
        if claim_id_matches {
            sqlx::query(
                "UPDATE providers SET last_model_discovery_at = \
                        CASE WHEN $1 THEN $2 ELSE last_model_discovery_at END, \
                        last_model_discovery_status = \
                        CASE WHEN $1 THEN $3 ELSE last_model_discovery_status END, \
                        model_discovery_claim_id = NULL, model_discovery_claimed_until = NULL \
                 WHERE id = $4",
            )
            .bind(etag_matches)
            .bind(now)
            .bind(status)
            .bind(claim.provider_id)
            .execute(&mut *transaction)
            .await?;
        }
        sqlx::query(
            "INSERT INTO provider_model_discovery_runs \
             (id, provider_id, origin, completeness, status, expected_etag, observed_model_count, \
              detail, started_at, completed_at) \
             VALUES ($1, $2, 'scheduled', 'partial', $3, $4, 0, $5, $6, $6)",
        )
        .bind(Uuid::now_v7())
        .bind(claim.provider_id)
        .bind(status)
        .bind(claim.expected_etag)
        .bind(&detail)
        .bind(now)
        .execute(&mut *transaction)
        .await?;
        audit_optional_in_transaction(
            &mut transaction,
            None,
            "provider.model_discovery",
            "provider",
            claim.provider_id,
            status,
        )
        .await?;
        transaction.commit().await?;
        Ok(())
    }

    pub async fn set_provider_model_enabled(
        &self,
        provider_id: Uuid,
        model_id: Uuid,
        enabled: bool,
        capabilities: &[CapabilityRecord],
        expected_etag: Uuid,
        actor: Uuid,
    ) -> Result<Uuid, CatalogError> {
        if enabled && capabilities.is_empty() {
            return Err(CatalogError::Invalid(
                "enabled models require at least one reviewed capability".to_owned(),
            ));
        }
        if capabilities.len() > 16 {
            return Err(CatalogError::Invalid(
                "a model can declare at most 16 capability tuples".to_owned(),
            ));
        }
        let mut unique = BTreeSet::new();
        for capability in capabilities {
            validate_capability(capability)?;
            let tuple = (
                capability.operation.as_str(),
                capability.surface.as_str(),
                capability.mode.as_str(),
            );
            if !unique.insert(tuple) {
                return Err(CatalogError::Invalid(
                    "model capabilities must be unique".to_owned(),
                ));
            }
        }
        let mut transaction = self.pool().begin().await?;
        let provider = sqlx::query(
            "SELECT etag, state::text AS state, kind FROM providers WHERE id = $1 FOR UPDATE",
        )
        .bind(provider_id)
        .fetch_optional(&mut *transaction)
        .await?
        .ok_or(CatalogError::NotFound)?;
        if provider.get::<Uuid, _>("etag") != expected_etag {
            return Err(CatalogError::PreconditionFailed);
        }
        if provider.get::<String, _>("state") == "disabled" {
            return Err(CatalogError::InUse);
        }
        let provider_kind: String = provider.get("kind");
        for capability in capabilities {
            validate_provider_capability(&provider_kind, capability)?;
        }
        let review_revision = Uuid::now_v7();
        let result = sqlx::query(
            "UPDATE provider_models SET enabled = $1, review_revision = $2 \
             WHERE id = $3 AND provider_id = $4 \
               AND (NOT $1 OR availability = 'available')",
        )
        .bind(enabled)
        .bind(review_revision)
        .bind(model_id)
        .bind(provider_id)
        .execute(&mut *transaction)
        .await?;
        if result.rows_affected() != 1 {
            let exists: bool = sqlx::query_scalar(
                "SELECT EXISTS (SELECT 1 FROM provider_models WHERE id = $1 AND provider_id = $2)",
            )
            .bind(model_id)
            .bind(provider_id)
            .fetch_one(&mut *transaction)
            .await?;
            return Err(if exists {
                CatalogError::Invalid(
                    "a model missing from the authoritative upstream inventory cannot be enabled"
                        .to_owned(),
                )
            } else {
                CatalogError::NotFound
            });
        }
        sqlx::query("DELETE FROM model_capabilities WHERE provider_model_id = $1")
            .bind(model_id)
            .execute(&mut *transaction)
            .await?;
        for capability in capabilities {
            sqlx::query(
                "INSERT INTO model_capabilities \
                 (provider_model_id, operation, surface, mode, source, certified_at) \
                 VALUES ($1, $2, $3, $4, $5, CASE WHEN $5 = 'certified' THEN now() ELSE NULL END)",
            )
            .bind(model_id)
            .bind(capability.operation.as_str())
            .bind(capability.surface.as_str())
            .bind(capability.mode.as_str())
            .bind(capability.source.as_str())
            .execute(&mut *transaction)
            .await?;
        }
        let etag = Uuid::now_v7();
        sqlx::query(
            "UPDATE providers SET etag = $1, state = 'draft'::provider_state, updated_at = now() \
             WHERE id = $2",
        )
        .bind(etag)
        .bind(provider_id)
        .execute(&mut *transaction)
        .await?;
        audit_in_transaction(
            &mut transaction,
            actor,
            "provider.model.update",
            "provider_model",
            model_id,
            "success",
        )
        .await?;
        transaction.commit().await?;
        Ok(etag)
    }

    /// Reserves a durable certification run before any provider probes are
    /// sent. The run binds evidence to transport/credential context and the
    /// exact reviewed tuple set rather than to unrelated catalog ETag changes.
    pub async fn begin_capability_certification(
        &self,
        provider_id: Uuid,
        model_id: Uuid,
        expected_etag: Uuid,
        actor: Uuid,
    ) -> Result<CapabilityCertificationStarted, CatalogError> {
        let mut transaction = self.pool().begin().await?;
        let provider = sqlx::query(
            "SELECT etag, state::text AS state, kind, certification_context_id, \
                    last_probe_status, last_probe_context_id \
             FROM providers WHERE id = $1 FOR UPDATE",
        )
        .bind(provider_id)
        .fetch_optional(&mut *transaction)
        .await?
        .ok_or(CatalogError::NotFound)?;
        if provider.get::<Uuid, _>("etag") != expected_etag {
            return Err(CatalogError::PreconditionFailed);
        }
        if provider.get::<String, _>("state") != "draft" {
            return Err(CatalogError::InUse);
        }
        let certification_context_id: Uuid = provider.get("certification_context_id");
        if provider.get::<String, _>("kind") != "open_ai_compatible"
            && (provider
                .get::<Option<String>, _>("last_probe_status")
                .as_deref()
                != Some("succeeded")
                || provider.get::<Option<Uuid>, _>("last_probe_context_id")
                    != Some(certification_context_id))
        {
            return Err(CatalogError::Invalid(
                "native capability certification requires a successful credentialed probe of the current transport and credential context"
                    .to_owned(),
            ));
        }

        let model = sqlx::query(
            "SELECT upstream_model, discovered_at, availability, review_revision \
             FROM provider_models WHERE id = $1 AND provider_id = $2 FOR UPDATE",
        )
        .bind(model_id)
        .bind(provider_id)
        .fetch_optional(&mut *transaction)
        .await?
        .ok_or(CatalogError::NotFound)?;
        if model.get::<String, _>("availability") != "available" {
            return Err(CatalogError::Invalid(
                "a model missing from the authoritative upstream inventory cannot be certified"
                    .to_owned(),
            ));
        }
        if provider.get::<String, _>("kind") != "open_ai_compatible"
            && model
                .get::<Option<DateTime<Utc>>, _>("discovered_at")
                .is_none()
        {
            return Err(CatalogError::Invalid(
                "native capability certification requires a discovered provider model".to_owned(),
            ));
        }
        let capability_rows = sqlx::query(
            "SELECT operation, surface, mode, source, certified_at \
             FROM model_capabilities WHERE provider_model_id = $1 FOR UPDATE",
        )
        .bind(model_id)
        .fetch_all(&mut *transaction)
        .await?;
        if capability_rows.is_empty() || capability_rows.len() > 16 {
            return Err(CatalogError::Invalid(
                "review between 1 and 16 capability tuples before certification".to_owned(),
            ));
        }
        let capabilities = capability_rows
            .iter()
            .map(capability_from_row)
            .collect::<Result<Vec<_>, _>>()?;
        let review_revision: Uuid = model.get("review_revision");
        // A cancelled HTTP request or control-plane restart must not leave a
        // tuple set permanently un-certifiable. Only the matching context and
        // review revision are superseded here; newer work remains untouched.
        sqlx::query(
            "UPDATE capability_certification_runs SET status = 'superseded', completed_at = now() \
             WHERE provider_model_id = $1 AND certification_context_id = $2 \
               AND review_revision = $3 AND status = 'running' \
               AND lease_expires_at <= now()",
        )
        .bind(model_id)
        .bind(certification_context_id)
        .bind(review_revision)
        .execute(&mut *transaction)
        .await?;
        let already_running: bool = sqlx::query_scalar(
            "SELECT EXISTS (SELECT 1 FROM capability_certification_runs \
             WHERE provider_model_id = $1 AND certification_context_id = $2 \
               AND review_revision = $3 AND status = 'running')",
        )
        .bind(model_id)
        .bind(certification_context_id)
        .bind(review_revision)
        .fetch_one(&mut *transaction)
        .await?;
        if already_running {
            return Err(CatalogError::InUse);
        }
        let run_id = Uuid::now_v7();
        sqlx::query(
            "INSERT INTO capability_certification_runs \
             (id, provider_id, provider_model_id, actor_user_id, certification_context_id, \
              review_revision, status, attempted_count, lease_expires_at) \
             VALUES ($1, $2, $3, $4, $5, $6, 'running', $7, now() + interval '15 minutes')",
        )
        .bind(run_id)
        .bind(provider_id)
        .bind(model_id)
        .bind(actor)
        .bind(certification_context_id)
        .bind(review_revision)
        .bind(i32::try_from(capabilities.len()).expect("capability count fits i32"))
        .execute(&mut *transaction)
        .await?;
        transaction.commit().await?;
        Ok(CapabilityCertificationStarted {
            run_id,
            upstream_model: model.get("upstream_model"),
            capabilities,
        })
    }

    /// Completes a run reserved by [`Self::begin_capability_certification`].
    /// If a connector, credential, or review change raced the probes, evidence
    /// remains auditable but is marked superseded instead of becoming active.
    pub async fn complete_capability_certification(
        &self,
        run_id: Uuid,
        outcomes: &[CapabilityCertificationResult],
    ) -> Result<CapabilityCertificationApplied, CatalogError> {
        let submitted = certification_result_tuples(outcomes)?;
        let mut transaction = self.pool().begin().await?;
        let run = sqlx::query(
            "SELECT provider_id, provider_model_id, actor_user_id, certification_context_id, \
                    review_revision, status \
             FROM capability_certification_runs WHERE id = $1",
        )
        .bind(run_id)
        .fetch_optional(&mut *transaction)
        .await?
        .ok_or(CatalogError::NotFound)?;
        let provider_id: Uuid = run.get("provider_id");
        let model_id: Uuid = run.get("provider_model_id");
        let actor: Option<Uuid> = run.get("actor_user_id");
        let run_context: Uuid = run.get("certification_context_id");
        let run_review: Uuid = run.get("review_revision");
        let provider = sqlx::query(
            "SELECT state::text AS state, kind, certification_context_id FROM providers \
             WHERE id = $1 FOR UPDATE",
        )
        .bind(provider_id)
        .fetch_optional(&mut *transaction)
        .await?
        .ok_or(CatalogError::NotFound)?;
        let model = sqlx::query(
            "SELECT review_revision, availability FROM provider_models \
             WHERE id = $1 AND provider_id = $2 FOR UPDATE",
        )
        .bind(model_id)
        .bind(provider_id)
        .fetch_optional(&mut *transaction)
        .await?
        .ok_or(CatalogError::NotFound)?;
        let locked_run = sqlx::query(
            "SELECT status, lease_expires_at FROM capability_certification_runs WHERE id = $1 FOR UPDATE",
        )
        .bind(run_id)
        .fetch_optional(&mut *transaction)
        .await?
        .ok_or(CatalogError::NotFound)?;
        if locked_run.get::<String, _>("status") != "running" {
            return Err(CatalogError::InUse);
        }
        let current = sqlx::query(
            "SELECT operation, surface, mode FROM model_capabilities \
             WHERE provider_model_id = $1 FOR UPDATE",
        )
        .bind(model_id)
        .fetch_all(&mut *transaction)
        .await?
        .into_iter()
        .map(certification_tuple_from_row)
        .collect::<Result<BTreeSet<_>, _>>()?;
        let current_context: Uuid = provider.get("certification_context_id");
        let current_review: Uuid = model.get("review_revision");
        let superseded = locked_run.get::<DateTime<Utc>, _>("lease_expires_at") <= Utc::now()
            || provider.get::<String, _>("state") != "draft"
            || current_context != run_context
            || current_review != run_review
            || model.get::<String, _>("availability") != "available"
            || current != submitted;
        if superseded {
            insert_certification_results(&mut transaction, run_id, outcomes).await?;
            sqlx::query(
                "UPDATE capability_certification_runs SET status = 'superseded', completed_at = now() \
                 WHERE id = $1",
            )
            .bind(run_id)
            .execute(&mut *transaction)
            .await?;
            audit_optional_in_transaction(
                &mut transaction,
                actor,
                "provider.model.certify",
                "provider_model",
                model_id,
                "superseded",
            )
            .await?;
            transaction.commit().await?;
            return Err(CatalogError::PreconditionFailed);
        }

        let certified_at = Utc::now();
        clear_model_capability_evidence(&mut transaction, model_id).await?;
        let mut certified_count = 0_usize;
        for outcome in outcomes.iter().filter(|outcome| outcome.succeeded) {
            let evidence_kind = outcome.evidence_kind.as_deref().ok_or_else(|| {
                CatalogError::Invalid("successful certification lacks evidence".to_owned())
            })?;
            let updated = sqlx::query(
                "UPDATE model_capabilities SET source = 'certified', certified_at = $1, \
                        certification_context_id = $2, review_revision = $3, \
                        certification_run_id = $4, certification_evidence_kind = $5 \
                 WHERE provider_model_id = $6 AND operation = $7 AND surface = $8 AND mode = $9",
            )
            .bind(certified_at)
            .bind(run_context)
            .bind(run_review)
            .bind(run_id)
            .bind(evidence_kind)
            .bind(model_id)
            .bind(outcome.operation.as_str())
            .bind(outcome.surface.as_str())
            .bind(outcome.mode.as_str())
            .execute(&mut *transaction)
            .await?;
            if updated.rows_affected() != 1 {
                return Err(CatalogError::PreconditionFailed);
            }
            certified_count += 1;
        }
        insert_certification_results(&mut transaction, run_id, outcomes).await?;
        let status = if certified_count == outcomes.len() {
            "succeeded"
        } else if certified_count == 0 {
            "failed"
        } else {
            "partial"
        };
        sqlx::query(
            "UPDATE capability_certification_runs SET status = $1, certified_count = $2, \
                    completed_at = $3 WHERE id = $4",
        )
        .bind(status)
        .bind(i32::try_from(certified_count).expect("capability count fits i32"))
        .bind(certified_at)
        .bind(run_id)
        .execute(&mut *transaction)
        .await?;
        let etag = Uuid::now_v7();
        // A successful compatible-endpoint certification is itself a bounded
        // live request through the selected model and current credential. This
        // is the connectivity proof needed when such an endpoint deliberately
        // has no `/models` API for the ordinary probe path.
        let certification_proves_connectivity =
            provider.get::<String, _>("kind") == "open_ai_compatible" && certified_count > 0;
        sqlx::query(
            "UPDATE providers SET etag = $1, \
                    last_probe_at = CASE WHEN $2 THEN $3 ELSE last_probe_at END, \
                    last_probe_status = CASE WHEN $2 THEN 'succeeded' ELSE last_probe_status END, \
                    last_probe_detail = CASE WHEN $2 THEN \
                        'A successful compatible capability certification proved the current connector context.' \
                        ELSE last_probe_detail END, \
                    last_probe_context_id = CASE WHEN $2 THEN $4 ELSE last_probe_context_id END \
             WHERE id = $5",
        )
            .bind(etag)
            .bind(certification_proves_connectivity)
            .bind(certified_at)
            .bind(run_context)
            .bind(provider_id)
            .execute(&mut *transaction)
            .await?;
        audit_optional_in_transaction(
            &mut transaction,
            actor,
            "provider.model.certify",
            "provider_model",
            model_id,
            status,
        )
        .await?;
        transaction.commit().await?;
        Ok(CapabilityCertificationApplied {
            run_id,
            etag,
            certified_at,
            certified_count,
            attempted_count: outcomes.len(),
        })
    }

    /// Legacy storage callers submit Boolean-only outcomes. Keep that API
    /// while storing a complete, durable run with explicit legacy evidence.
    pub async fn apply_compatible_capability_certification(
        &self,
        provider_id: Uuid,
        model_id: Uuid,
        expected_etag: Uuid,
        actor: Uuid,
        outcomes: &[CapabilityCertificationOutcome],
    ) -> Result<CapabilityCertificationApplied, CatalogError> {
        let started = self
            .begin_capability_certification(provider_id, model_id, expected_etag, actor)
            .await?;
        let results = outcomes
            .iter()
            .map(|outcome| CapabilityCertificationResult {
                operation: outcome.operation,
                surface: outcome.surface,
                mode: outcome.mode,
                succeeded: outcome.succeeded,
                evidence_kind: outcome.succeeded.then(|| "legacy".to_owned()),
                error_code: (!outcome.succeeded).then(|| "legacy_failure".to_owned()),
                detail: "Legacy storage certification result.".to_owned(),
            })
            .collect::<Vec<_>>();
        self.complete_capability_certification(started.run_id, &results)
            .await
    }

    pub async fn list_route_draft_catalog(
        &self,
        cursor: Option<Uuid>,
        limit: i64,
    ) -> Result<CatalogPage<RouteDraftCatalogRecord>, CatalogError> {
        let limit = checked_limit(limit)?;
        let rows = sqlx::query(
            "SELECT id FROM route_drafts WHERE ($1::uuid IS NULL OR id > $1) ORDER BY id LIMIT $2",
        )
        .bind(cursor)
        .bind(limit + 1)
        .fetch_all(self.pool())
        .await?;
        let (rows, next_cursor) = split_page(rows, limit as usize, |row| row.get::<Uuid, _>("id"));
        let ids: Vec<Uuid> = rows.into_iter().map(|row| row.get("id")).collect();
        let mut items = Vec::with_capacity(ids.len());
        for id in ids {
            items.push(self.get_route_draft_catalog(id).await?);
        }
        Ok(CatalogPage { items, next_cursor })
    }

    pub async fn get_route_draft_catalog(
        &self,
        draft_id: Uuid,
    ) -> Result<RouteDraftCatalogRecord, CatalogError> {
        let row = sqlx::query(
            "SELECT id, routing_id, slug, state::text AS state, overall_timeout_ms, max_attempts, etag, \
                    based_on_revision_id, created_at, updated_at FROM route_drafts WHERE id = $1",
        )
        .bind(draft_id)
        .fetch_optional(self.pool())
        .await?
        .ok_or(CatalogError::NotFound)?;
        Ok(RouteDraftCatalogRecord {
            id: row.get("id"),
            routing_id: row.get("routing_id"),
            slug: row.get("slug"),
            state: row
                .get::<String, _>("state")
                .parse()
                .map_err(|_| PersistenceError::InvalidStoredValue("route draft state"))?,
            overall_timeout_ms: row.get("overall_timeout_ms"),
            max_attempts: row.get("max_attempts"),
            etag: row.get("etag"),
            based_on_revision_id: row.get("based_on_revision_id"),
            operations: draft_operations(self.pool(), draft_id).await?,
            targets: draft_targets(self.pool(), draft_id).await?,
            created_at: row.get("created_at"),
            updated_at: row.get("updated_at"),
        })
    }

    pub async fn replace_route_draft_catalog(
        &self,
        draft_id: Uuid,
        expected_etag: Uuid,
        input: &ReplaceRouteDraftCatalogInput,
        actor: Uuid,
    ) -> Result<Uuid, CatalogError> {
        validate_route_input(
            &input.slug,
            &input.operations,
            input.overall_timeout_ms,
            input.max_attempts,
            &input.targets,
        )?;
        let mut transaction = self.pool().begin().await?;
        let lineage_slug: Option<String> = sqlx::query_scalar(
            "SELECT rr.slug FROM route_drafts rd \
             JOIN route_revisions rr ON rr.id = rd.based_on_revision_id \
             WHERE rd.id = $1",
        )
        .bind(draft_id)
        .fetch_optional(&mut *transaction)
        .await?;
        if lineage_slug
            .as_deref()
            .is_some_and(|lineage_slug| lineage_slug != input.slug.as_str())
        {
            return Err(CatalogError::Invalid(
                "a restored route draft must retain its original stable slug".to_owned(),
            ));
        }
        let etag = Uuid::now_v7();
        let result = sqlx::query(
            "UPDATE route_drafts SET slug = $1, overall_timeout_ms = $2, max_attempts = $3, \
                    state = 'draft'::route_draft_state, etag = $4, updated_at = now() \
             WHERE id = $5 AND etag = $6",
        )
        .bind(&input.slug)
        .bind(input.overall_timeout_ms)
        .bind(input.max_attempts)
        .bind(etag)
        .bind(draft_id)
        .bind(expected_etag)
        .execute(&mut *transaction)
        .await?;
        if result.rows_affected() != 1 {
            let exists: bool =
                sqlx::query_scalar("SELECT EXISTS (SELECT 1 FROM route_drafts WHERE id = $1)")
                    .bind(draft_id)
                    .fetch_one(&mut *transaction)
                    .await?;
            return Err(if exists {
                CatalogError::PreconditionFailed
            } else {
                CatalogError::NotFound
            });
        }
        sqlx::query("DELETE FROM route_draft_operations WHERE route_draft_id = $1")
            .bind(draft_id)
            .execute(&mut *transaction)
            .await?;
        sqlx::query("DELETE FROM route_draft_targets WHERE route_draft_id = $1")
            .bind(draft_id)
            .execute(&mut *transaction)
            .await?;
        for operation in &input.operations {
            sqlx::query(
                "INSERT INTO route_draft_operations (route_draft_id, operation) VALUES ($1, $2)",
            )
            .bind(draft_id)
            .bind(operation.as_str())
            .execute(&mut *transaction)
            .await?;
        }
        for (position, (provider_model_id, priority, weight, timeout_ms)) in
            input.targets.iter().enumerate()
        {
            let enabled: bool = sqlx::query_scalar(
                "SELECT EXISTS (SELECT 1 FROM providers p \
                 JOIN provider_revision_models prm ON prm.provider_revision_id = p.active_revision_id \
                 WHERE prm.source_provider_model_id = $1 AND prm.enabled \
                   AND p.state <> 'disabled'::provider_state)",
            )
            .bind(provider_model_id)
            .fetch_one(&mut *transaction)
            .await?;
            if !enabled {
                return Err(CatalogError::Invalid(format!(
                    "provider model {provider_model_id} is not active"
                )));
            }
            sqlx::query(
                "INSERT INTO route_draft_targets \
                 (id, routing_id, route_draft_id, provider_model_id, priority, weight, timeout_ms, position) \
                 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)",
            )
            .bind(Uuid::now_v7())
            .bind(Uuid::now_v7())
            .bind(draft_id)
            .bind(provider_model_id)
            .bind(priority)
            .bind(weight)
            .bind(timeout_ms)
            .bind(
                i32::try_from(position)
                    .map_err(|_| CatalogError::Invalid("too many targets".to_owned()))?,
            )
            .execute(&mut *transaction)
            .await?;
        }
        audit_in_transaction(
            &mut transaction,
            actor,
            "route.update_draft",
            "route_draft",
            draft_id,
            "success",
        )
        .await?;
        transaction.commit().await?;
        Ok(etag)
    }

    pub async fn delete_route_draft_catalog(
        &self,
        draft_id: Uuid,
        expected_etag: Uuid,
        actor: Uuid,
    ) -> Result<(), CatalogError> {
        let mut transaction = self.pool().begin().await?;
        let referenced: bool = sqlx::query_scalar(
            "SELECT EXISTS (SELECT 1 FROM route_revisions WHERE source_draft_id = $1)",
        )
        .bind(draft_id)
        .fetch_one(&mut *transaction)
        .await?;
        if referenced {
            return Err(CatalogError::InUse);
        }
        let result = sqlx::query("DELETE FROM route_drafts WHERE id = $1 AND etag = $2")
            .bind(draft_id)
            .bind(expected_etag)
            .execute(&mut *transaction)
            .await?;
        if result.rows_affected() != 1 {
            let exists: bool =
                sqlx::query_scalar("SELECT EXISTS (SELECT 1 FROM route_drafts WHERE id = $1)")
                    .bind(draft_id)
                    .fetch_one(&mut *transaction)
                    .await?;
            return Err(if exists {
                CatalogError::PreconditionFailed
            } else {
                CatalogError::NotFound
            });
        }
        audit_in_transaction(
            &mut transaction,
            actor,
            "route.delete_draft",
            "route_draft",
            draft_id,
            "success",
        )
        .await?;
        transaction.commit().await?;
        Ok(())
    }

    pub async fn simulate_route_draft_catalog(
        &self,
        draft_id: Uuid,
        operation: OperationKind,
        surface: Surface,
        mode: TransportMode,
        seed: &str,
    ) -> Result<RouteSimulation, CatalogError> {
        if seed.is_empty() || seed.len() > 256 {
            return Err(CatalogError::Invalid(
                "simulation seed must contain 1-256 bytes".to_owned(),
            ));
        }
        let draft = self.get_route_draft_catalog(draft_id).await?;
        if !draft.operations.contains(&operation) {
            return Err(CatalogError::Invalid(format!(
                "route does not support {operation}"
            )));
        }
        let scoring_route_id = RouteId::from_uuid(draft.routing_id);
        let maximum = usize::try_from(draft.max_attempts).unwrap_or_default();
        let mut ranked: BTreeMap<i32, Vec<(f64, RouteTargetRecord)>> = BTreeMap::new();
        let mut ineligible = Vec::new();
        for target in draft.targets {
            let capability: bool = sqlx::query_scalar(
                "SELECT EXISTS (SELECT 1 FROM providers p \
                 JOIN provider_revision_models prm ON prm.provider_revision_id = p.active_revision_id \
                 JOIN provider_revision_capabilities prc \
                   ON prc.provider_revision_model_id = prm.id \
                 WHERE prm.source_provider_model_id = $1 AND prc.operation = $2 \
                   AND prc.surface = $3 AND prc.mode = $4 AND prm.enabled \
                   AND prc.source = 'certified' AND p.state <> 'disabled'::provider_state)",
            )
            .bind(target.provider_model_id)
            .bind(operation.as_str())
            .bind(surface.as_str())
            .bind(mode.as_str())
            .fetch_one(self.pool())
            .await?;
            if capability {
                let weight = u32::try_from(target.weight)
                    .ok()
                    .and_then(NonZeroU32::new)
                    .ok_or_else(|| {
                        CatalogError::Invalid("route target weight is invalid".to_owned())
                    })?;
                let score = weighted_rendezvous_score(
                    scoring_route_id,
                    TargetId::from_uuid(target.routing_id),
                    weight,
                    operation,
                    surface,
                    mode,
                    seed.as_bytes(),
                );
                ranked
                    .entry(target.priority)
                    .or_default()
                    .push((score, target));
            } else {
                ineligible.push(RouteSimulationTarget {
                    target_id: target.id,
                    provider_id: target.provider_id,
                    provider_name: target.provider_name,
                    provider_model: target.provider_model,
                    priority: target.priority,
                    eligible: false,
                    reason: Some(
                        "missing exact capability or provider/model is disabled".to_owned(),
                    ),
                    attempt: None,
                });
            }
        }
        let mut targets = Vec::new();
        for (_, mut group) in ranked {
            group.sort_by(|left, right| {
                right
                    .0
                    .total_cmp(&left.0)
                    .then_with(|| left.1.routing_id.cmp(&right.1.routing_id))
            });
            for (_, target) in group {
                let attempt = (targets.len() < maximum).then_some(targets.len() + 1);
                targets.push(RouteSimulationTarget {
                    target_id: target.id,
                    provider_id: target.provider_id,
                    provider_name: target.provider_name,
                    provider_model: target.provider_model,
                    priority: target.priority,
                    eligible: true,
                    reason: attempt
                        .is_none()
                        .then(|| "eligible but beyond max_attempts".to_owned()),
                    attempt,
                });
            }
        }
        targets.extend(ineligible);
        Ok(RouteSimulation {
            deterministic_seed: seed.to_owned(),
            operation,
            surface,
            mode,
            targets,
        })
    }

    pub async fn list_route_revisions_catalog(
        &self,
        route_id: Uuid,
        cursor: Option<Uuid>,
        limit: i64,
    ) -> Result<CatalogPage<RouteRevisionCatalogRecord>, CatalogError> {
        let limit = checked_limit(limit)?;
        let exists: bool = sqlx::query_scalar("SELECT EXISTS (SELECT 1 FROM routes WHERE id = $1)")
            .bind(route_id)
            .fetch_one(self.pool())
            .await?;
        if !exists {
            return Err(CatalogError::NotFound);
        }
        let before_revision: Option<i32> = match cursor {
            Some(cursor) => Some(
                sqlx::query_scalar(
                    "SELECT revision FROM route_revisions WHERE route_id = $1 AND id = $2",
                )
                .bind(route_id)
                .bind(cursor)
                .fetch_optional(self.pool())
                .await?
                .ok_or_else(|| {
                    CatalogError::Invalid("route-revision pagination cursor is invalid".to_owned())
                })?,
            ),
            None => None,
        };
        let ids: Vec<Uuid> = sqlx::query_scalar(
            "SELECT id FROM route_revisions WHERE route_id = $1 \
             AND ($2::int IS NULL OR revision < $2) \
             ORDER BY revision DESC LIMIT $3",
        )
        .bind(route_id)
        .bind(before_revision)
        .bind(limit + 1)
        .fetch_all(self.pool())
        .await?;
        let (ids, next_cursor) = split_page(ids, limit as usize, |id| *id);
        let mut revisions = Vec::with_capacity(ids.len());
        for id in ids {
            revisions.push(self.get_route_revision_catalog(route_id, id).await?);
        }
        Ok(CatalogPage {
            items: revisions,
            next_cursor,
        })
    }

    pub async fn list_routes_catalog(
        &self,
        cursor: Option<Uuid>,
        limit: i64,
    ) -> Result<CatalogPage<RouteCatalogRecord>, CatalogError> {
        let limit = checked_limit(limit)?;
        let rows = sqlx::query(
            "SELECT id FROM routes WHERE ($1::uuid IS NULL OR id > $1)
             ORDER BY id LIMIT $2",
        )
        .bind(cursor)
        .bind(limit + 1)
        .fetch_all(self.pool())
        .await?;
        let (rows, next_cursor) = split_page(rows, limit as usize, |row| row.get::<Uuid, _>("id"));
        let ids = rows
            .into_iter()
            .map(|row| row.get::<Uuid, _>("id"))
            .collect::<Vec<_>>();
        let mut items = Vec::with_capacity(ids.len());
        for id in ids {
            items.push(self.get_route_catalog(id).await?);
        }
        Ok(CatalogPage { items, next_cursor })
    }

    pub async fn get_route_catalog(&self, id: Uuid) -> Result<RouteCatalogRecord, CatalogError> {
        let row = sqlx::query(
            "SELECT r.id, r.slug, r.created_at,
                    (SELECT rr.id FROM route_revisions rr WHERE rr.route_id = r.id
                     ORDER BY rr.revision DESC LIMIT 1) AS latest_revision_id,
                    (SELECT count(*) FROM route_revisions rr WHERE rr.route_id = r.id)::bigint
                      AS revision_count
             FROM routes r WHERE r.id = $1",
        )
        .bind(id)
        .fetch_optional(self.pool())
        .await?
        .ok_or(CatalogError::NotFound)?;
        let latest_revision_id: Option<Uuid> = row.get("latest_revision_id");
        let latest_revision_id = latest_revision_id.ok_or_else(|| {
            CatalogError::Invalid("activated route has no immutable revision".to_owned())
        })?;
        let revision_count = u64::try_from(row.get::<i64, _>("revision_count"))
            .map_err(|_| CatalogError::Invalid("route revision count is invalid".to_owned()))?;
        Ok(RouteCatalogRecord {
            id: row.get("id"),
            slug: row.get("slug"),
            created_at: row.get("created_at"),
            revision_count,
            latest_revision: self
                .get_route_revision_catalog(id, latest_revision_id)
                .await?,
        })
    }

    pub async fn get_route_revision_catalog(
        &self,
        route_id: Uuid,
        revision_id: Uuid,
    ) -> Result<RouteRevisionCatalogRecord, CatalogError> {
        let row = sqlx::query(
            "SELECT id, routing_id, route_id, revision, slug, overall_timeout_ms, max_attempts, source_draft_id, \
                    activated_by, activated_at FROM route_revisions WHERE route_id = $1 AND id = $2",
        )
        .bind(route_id)
        .bind(revision_id)
        .fetch_optional(self.pool())
        .await?
        .ok_or(CatalogError::NotFound)?;
        Ok(RouteRevisionCatalogRecord {
            id: row.get("id"),
            routing_id: row.get("routing_id"),
            route_id: row.get("route_id"),
            revision: row.get("revision"),
            slug: row.get("slug"),
            overall_timeout_ms: row.get("overall_timeout_ms"),
            max_attempts: row.get("max_attempts"),
            source_draft_id: row.get("source_draft_id"),
            activated_by: row.get("activated_by"),
            activated_at: row.get("activated_at"),
            operations: revision_operations(self.pool(), revision_id).await?,
            targets: revision_targets(self.pool(), revision_id).await?,
        })
    }

    pub async fn diff_route_revisions_catalog(
        &self,
        route_id: Uuid,
        from_id: Uuid,
        to_id: Uuid,
    ) -> Result<RouteRevisionDiff, CatalogError> {
        let from = self.get_route_revision_catalog(route_id, from_id).await?;
        let to = self.get_route_revision_catalog(route_id, to_id).await?;
        let from_operations: BTreeSet<_> = from.operations.iter().cloned().collect();
        let to_operations: BTreeSet<_> = to.operations.iter().cloned().collect();
        let from_targets = revision_target_map(&from.targets);
        let to_targets = revision_target_map(&to.targets);
        Ok(RouteRevisionDiff {
            from_revision: from.revision,
            to_revision: to.revision,
            slug_changed: from.slug != to.slug,
            timeout_changed: from.overall_timeout_ms != to.overall_timeout_ms,
            max_attempts_changed: from.max_attempts != to.max_attempts,
            operations_added: to_operations
                .difference(&from_operations)
                .copied()
                .collect(),
            operations_removed: from_operations
                .difference(&to_operations)
                .copied()
                .collect(),
            targets_added: to_targets
                .keys()
                .filter(|key| !from_targets.contains_key(*key))
                .cloned()
                .collect(),
            targets_removed: from_targets
                .keys()
                .filter(|key| !to_targets.contains_key(*key))
                .cloned()
                .collect(),
            targets_changed: to_targets
                .iter()
                .filter_map(|(key, value)| {
                    from_targets
                        .get(key)
                        .filter(|old| *old != value)
                        .map(|_| key.clone())
                })
                .collect(),
        })
    }

    pub async fn restore_route_revision_as_draft(
        &self,
        route_id: Uuid,
        revision_id: Uuid,
        actor: Uuid,
        idempotency_key: &str,
    ) -> Result<RouteDraftCatalogRecord, CatalogError> {
        let revision = self
            .get_route_revision_catalog(route_id, revision_id)
            .await?;
        let mut transaction = self.pool().begin().await?;
        if !claim_idempotency(
            &mut transaction,
            actor,
            "route.restore_as_draft",
            idempotency_key,
        )
        .await?
        {
            return Err(CatalogError::IdempotencyConflict);
        }
        let id = Uuid::now_v7();
        let etag = Uuid::now_v7();
        sqlx::query(
            "INSERT INTO route_drafts \
             (id, routing_id, slug, state, overall_timeout_ms, max_attempts, etag, based_on_revision_id, created_by) \
             VALUES ($1, $2, $3, 'draft'::route_draft_state, $4, $5, $6, $7, $8)",
        )
        .bind(id)
        .bind(revision.routing_id)
        .bind(&revision.slug)
        .bind(revision.overall_timeout_ms)
        .bind(revision.max_attempts)
        .bind(etag)
        .bind(revision_id)
        .bind(actor)
        .execute(&mut *transaction)
        .await?;
        sqlx::query(
            "INSERT INTO route_draft_operations (route_draft_id, operation) \
             SELECT $1, operation FROM route_revision_operations WHERE route_revision_id = $2",
        )
        .bind(id)
        .bind(revision_id)
        .execute(&mut *transaction)
        .await?;
        sqlx::query(
            "INSERT INTO route_draft_targets \
             (id, routing_id, route_draft_id, provider_model_id, priority, weight, timeout_ms, position) \
             SELECT uuidv7(), routing_id, $1, provider_model_id, priority, weight, timeout_ms, position \
             FROM route_revision_targets WHERE route_revision_id = $2",
        )
        .bind(id)
        .bind(revision_id)
        .execute(&mut *transaction)
        .await?;
        audit_in_transaction(
            &mut transaction,
            actor,
            "route.restore_as_draft",
            "route_draft",
            id,
            "success",
        )
        .await?;
        complete_idempotency(
            &mut transaction,
            actor,
            "route.restore_as_draft",
            idempotency_key,
            &id.to_string(),
        )
        .await?;
        transaction.commit().await?;
        self.get_route_draft_catalog(id).await
    }

    pub async fn list_api_key_catalog(
        &self,
        cursor: Option<Uuid>,
        limit: i64,
    ) -> Result<CatalogPage<ApiKeyCatalogRecord>, CatalogError> {
        let limit = checked_limit(limit)?;
        let rows = sqlx::query(
            "SELECT id FROM api_keys WHERE ($1::uuid IS NULL OR id > $1) ORDER BY id LIMIT $2",
        )
        .bind(cursor)
        .bind(limit + 1)
        .fetch_all(self.pool())
        .await?;
        let (rows, next_cursor) = split_page(rows, limit as usize, |row| row.get::<Uuid, _>("id"));
        let ids: Vec<Uuid> = rows.into_iter().map(|row| row.get("id")).collect();
        let mut items = Vec::with_capacity(ids.len());
        for id in ids {
            items.push(self.get_api_key_catalog(id).await?);
        }
        Ok(CatalogPage { items, next_cursor })
    }

    pub async fn get_api_key_catalog(&self, id: Uuid) -> Result<ApiKeyCatalogRecord, CatalogError> {
        let row = sqlx::query(
            "SELECT k.id, k.lookup_id, k.name, k.created_by, u.email AS created_by_email, \
                    k.requests_per_minute, k.tokens_per_minute, k.max_concurrency, k.expires_at, \
                    k.revoked_at, k.rotated_at, k.etag, k.created_at \
             FROM api_keys k JOIN users u ON u.id = k.created_by WHERE k.id = $1",
        )
        .bind(id)
        .fetch_optional(self.pool())
        .await?
        .ok_or(CatalogError::NotFound)?;
        Ok(ApiKeyCatalogRecord {
            id: row.get("id"),
            lookup_id: row.get("lookup_id"),
            name: row.get("name"),
            created_by: row.get("created_by"),
            created_by_email: row.get("created_by_email"),
            scopes: sqlx::query_scalar("SELECT scope FROM api_key_scopes WHERE api_key_id = $1 ORDER BY scope")
                .bind(id).fetch_all(self.pool()).await?,
            allowed_routes: sqlx::query_scalar("SELECT route_slug FROM api_key_route_allowlist WHERE api_key_id = $1 ORDER BY route_slug")
                .bind(id).fetch_all(self.pool()).await?,
            requests_per_minute: row.get("requests_per_minute"),
            tokens_per_minute: row.get("tokens_per_minute"),
            max_concurrency: row.get("max_concurrency"),
            expires_at: row.get("expires_at"),
            revoked_at: row.get("revoked_at"),
            rotated_at: row.get("rotated_at"),
            etag: row.get("etag"),
            created_at: row.get("created_at"),
        })
    }

    pub async fn update_api_key_catalog(
        &self,
        id: Uuid,
        expected_etag: Uuid,
        input: &UpdateApiKeyCatalogInput,
        actor: Uuid,
    ) -> Result<ApiKeyMutationResult, CatalogError> {
        let name = input.name.trim();
        if name.is_empty() || name.chars().count() > 100 {
            return Err(CatalogError::Invalid(
                "API-key name must contain 1-100 characters".to_owned(),
            ));
        }
        if input.scopes.is_empty() {
            return Err(CatalogError::Invalid(
                "at least one API-key scope is required".to_owned(),
            ));
        }
        let scopes = input
            .scopes
            .iter()
            .map(String::as_str)
            .collect::<BTreeSet<_>>();
        if scopes.len() != input.scopes.len()
            || !scopes
                .iter()
                .all(|scope| matches!(*scope, "inference" | "models_read"))
        {
            return Err(CatalogError::Invalid(
                "API-key scopes must be unique inference or models_read values".to_owned(),
            ));
        }
        let allowed_routes = input
            .allowed_routes
            .iter()
            .map(|route| {
                RouteSlug::parse(route.clone()).map_err(|error| {
                    CatalogError::Invalid(format!("invalid allowlisted route: {error}"))
                })
            })
            .collect::<Result<BTreeSet<_>, _>>()?;
        if allowed_routes.len() != input.allowed_routes.len() {
            return Err(CatalogError::Invalid(
                "allowlisted routes must be unique".to_owned(),
            ));
        }
        if input
            .expires_at
            .is_some_and(|expiration| expiration <= Utc::now())
        {
            return Err(CatalogError::Invalid(
                "API-key expiration must be in the future".to_owned(),
            ));
        }
        let requests_per_minute = input
            .requests_per_minute
            .map(i32::try_from)
            .transpose()
            .map_err(|_| CatalogError::Invalid("RPM limit is too large".to_owned()))?;
        let tokens_per_minute = input
            .tokens_per_minute
            .map(i64::try_from)
            .transpose()
            .map_err(|_| CatalogError::Invalid("TPM limit is too large".to_owned()))?;
        let max_concurrency = input
            .max_concurrency
            .map(i32::try_from)
            .transpose()
            .map_err(|_| CatalogError::Invalid("concurrency limit is too large".to_owned()))?;
        if requests_per_minute == Some(0)
            || tokens_per_minute == Some(0)
            || max_concurrency == Some(0)
        {
            return Err(CatalogError::Invalid(
                "hard limits must be positive when configured".to_owned(),
            ));
        }

        let mut transaction = self
            .pool()
            .begin_with("BEGIN ISOLATION LEVEL READ COMMITTED")
            .await?;
        prepare_runtime_mutation(&mut transaction).await?;
        for route in &allowed_routes {
            let exists: bool =
                sqlx::query_scalar("SELECT EXISTS (SELECT 1 FROM routes WHERE slug = $1)")
                    .bind(route.as_str())
                    .fetch_one(&mut *transaction)
                    .await?;
            if !exists {
                return Err(CatalogError::Invalid(format!(
                    "allowlisted route {route} is not active"
                )));
            }
        }
        let etag = Uuid::now_v7();
        let updated = sqlx::query(
            "UPDATE api_keys SET name = $1, requests_per_minute = $2, tokens_per_minute = $3, \
                    max_concurrency = $4, expires_at = $5, etag = $6 \
             WHERE id = $7 AND etag = $8 AND revoked_at IS NULL \
               AND (expires_at IS NULL OR expires_at > now())",
        )
        .bind(name)
        .bind(requests_per_minute)
        .bind(tokens_per_minute)
        .bind(max_concurrency)
        .bind(input.expires_at)
        .bind(etag)
        .bind(id)
        .bind(expected_etag)
        .execute(&mut *transaction)
        .await?;
        if updated.rows_affected() != 1 {
            let row =
                sqlx::query("SELECT etag, revoked_at, expires_at FROM api_keys WHERE id = $1")
                    .bind(id)
                    .fetch_optional(&mut *transaction)
                    .await?
                    .ok_or(CatalogError::NotFound)?;
            if row.get::<Uuid, _>("etag") != expected_etag {
                return Err(CatalogError::PreconditionFailed);
            }
            return Err(CatalogError::Invalid(
                "revoked or expired keys cannot be updated".to_owned(),
            ));
        }
        sqlx::query("DELETE FROM api_key_scopes WHERE api_key_id = $1")
            .bind(id)
            .execute(&mut *transaction)
            .await?;
        for scope in scopes {
            sqlx::query("INSERT INTO api_key_scopes (api_key_id, scope) VALUES ($1, $2)")
                .bind(id)
                .bind(scope)
                .execute(&mut *transaction)
                .await?;
        }
        sqlx::query("DELETE FROM api_key_route_allowlist WHERE api_key_id = $1")
            .bind(id)
            .execute(&mut *transaction)
            .await?;
        for route in allowed_routes {
            sqlx::query(
                "INSERT INTO api_key_route_allowlist (api_key_id, route_slug) VALUES ($1, $2)",
            )
            .bind(id)
            .bind(route.as_str())
            .execute(&mut *transaction)
            .await?;
        }
        audit_in_transaction(
            &mut transaction,
            actor,
            "api_key.update",
            "api_key",
            id,
            "success",
        )
        .await?;
        let release = compile_and_publish_runtime_in_transaction(&mut transaction, actor).await?;
        transaction.commit().await?;
        Ok(ApiKeyMutationResult { etag, release })
    }

    pub async fn rotate_api_key_catalog<F>(
        &self,
        input: RotateApiKeyCatalogInput<'_>,
        replay: ReplayableIdempotency<'_>,
        build_response: F,
    ) -> Result<IdempotencyOutcome<ApiKeyRotationResult>, CatalogError>
    where
        F: FnOnce(&ApiKeyRotationResult) -> Result<IdempotencyResponse, PersistenceError>,
    {
        let RotateApiKeyCatalogInput {
            id,
            material,
            expected_etag,
            actor,
            idempotency_key,
        } = input;
        let mut transaction = self
            .pool()
            .begin_with("BEGIN ISOLATION LEVEL READ COMMITTED")
            .await?;
        match claim_replayable_idempotency(
            &mut transaction,
            actor,
            "api_key.rotate",
            idempotency_key,
            replay.request_fingerprint(),
            replay.master_key(),
        )
        .await?
        {
            ReplayableIdempotencyClaim::Execute => {
                prepare_runtime_mutation(&mut transaction).await?;
            }
            ReplayableIdempotencyClaim::Replay(response) => {
                transaction.rollback().await?;
                return Ok(IdempotencyOutcome::Replayed(response));
            }
            ReplayableIdempotencyClaim::Conflict => {
                transaction.rollback().await?;
                return Err(CatalogError::IdempotencyConflict);
            }
            ReplayableIdempotencyClaim::InProgress => {
                transaction.rollback().await?;
                return Err(CatalogError::IdempotencyInProgress);
            }
        }
        let etag = Uuid::now_v7();
        let result = sqlx::query(
            "UPDATE api_keys SET lookup_id = $1, secret_digest = $2, etag = $3, rotated_at = now() \
             WHERE id = $4 AND etag = $5 AND revoked_at IS NULL \
               AND (expires_at IS NULL OR expires_at > now())",
        )
        .bind(&material.lookup_id)
        .bind(material.digest.to_vec())
        .bind(etag)
        .bind(id)
        .bind(expected_etag)
        .execute(&mut *transaction)
        .await?;
        if result.rows_affected() != 1 {
            let row =
                sqlx::query("SELECT etag, revoked_at, expires_at FROM api_keys WHERE id = $1")
                    .bind(id)
                    .fetch_optional(&mut *transaction)
                    .await?
                    .ok_or(CatalogError::NotFound)?;
            if row.get::<Uuid, _>("etag") != expected_etag {
                return Err(CatalogError::PreconditionFailed);
            }
            return Err(CatalogError::Invalid(
                "revoked or expired keys cannot be rotated".to_owned(),
            ));
        }
        audit_in_transaction(
            &mut transaction,
            actor,
            "api_key.rotate",
            "api_key",
            id,
            "success",
        )
        .await?;
        let release = compile_and_publish_runtime_in_transaction(&mut transaction, actor).await?;
        let result = ApiKeyRotationResult {
            id,
            lookup_id: material.lookup_id.clone(),
            etag,
            release,
        };
        let response = build_response(&result)?;
        complete_replayable_idempotency(
            &mut transaction,
            actor,
            "api_key.rotate",
            idempotency_key,
            replay.request_fingerprint(),
            replay.master_key(),
            &response,
        )
        .await?;
        transaction.commit().await?;
        Ok(IdempotencyOutcome::Executed {
            value: result,
            response,
        })
    }
}

fn provider_catalog_from_row(row: PgRow) -> Result<ProviderCatalogRecord, CatalogError> {
    let active_revision = row
        .get::<Option<i32>, _>("active_revision")
        .map(u32::try_from)
        .transpose()
        .map_err(|_| CatalogError::Invalid("provider revision is invalid".to_owned()))?;
    Ok(ProviderCatalogRecord {
        id: row.get("id"),
        name: row.get("name"),
        kind: row
            .get::<String, _>("kind")
            .parse()
            .map_err(|_| PersistenceError::InvalidStoredValue("provider kind"))?,
        state: row
            .get::<String, _>("state")
            .parse()
            .map_err(|_| PersistenceError::InvalidStoredValue("provider state"))?,
        endpoint: row.get("endpoint"),
        cloud_region: row.get("cloud_region"),
        cloud_project: row.get("cloud_project"),
        deployment: row.get("deployment"),
        api_version: row.get("api_version"),
        auth_mode: row
            .get::<String, _>("auth_mode")
            .parse()
            .map_err(|_| PersistenceError::InvalidStoredValue("provider authentication mode"))?,
        connector_ready: row.get("connector_ready"),
        etag: row.get("etag"),
        active_revision,
        pending_activation: row.get("pending_activation"),
        draft_credential_id: row.get("draft_credential_id"),
        draft_credential_version: row.get("draft_credential_version"),
        runtime_credential_id: row.get("runtime_credential_id"),
        runtime_credential_version: row.get("runtime_credential_version"),
        last_probe_at: row.get("last_probe_at"),
        last_probe_status: row.get("last_probe_status"),
        last_probe_detail: row.get("last_probe_detail"),
        probe_ready: row.get("probe_ready"),
        created_at: row.get("created_at"),
        updated_at: row.get("updated_at"),
        model_count: catalog_count(&row, "model_count")?,
        enabled_model_count: catalog_count(&row, "enabled_model_count")?,
        capability_count: catalog_count(&row, "capability_count")?,
        certified_capability_count: catalog_count(&row, "certified_capability_count")?,
        enabled_capability_count: catalog_count(&row, "enabled_capability_count")?,
        enabled_certified_capability_count: catalog_count(
            &row,
            "enabled_certified_capability_count",
        )?,
        missing_model_count: catalog_count(&row, "missing_model_count")?,
        invalid_enabled_model_count: catalog_count(&row, "invalid_enabled_model_count")?,
        last_model_discovery_at: row.get("last_model_discovery_at"),
        last_model_discovery_status: row.get("last_model_discovery_status"),
        probe_model: row.get("probe_model"),
    })
}

fn provider_revision_catalog_from_row(
    row: PgRow,
) -> Result<ProviderRevisionCatalogRecord, CatalogError> {
    Ok(ProviderRevisionCatalogRecord {
        id: row.get("id"),
        provider_id: row.get("provider_id"),
        revision: row.get("revision"),
        name: row.get("name"),
        kind: row
            .get::<String, _>("kind")
            .parse()
            .map_err(|_| PersistenceError::InvalidStoredValue("provider revision kind"))?,
        endpoint: row.get("endpoint"),
        cloud_region: row.get("cloud_region"),
        cloud_project: row.get("cloud_project"),
        deployment: row.get("deployment"),
        api_version: row.get("api_version"),
        auth_mode: row.get::<String, _>("auth_mode").parse().map_err(|_| {
            PersistenceError::InvalidStoredValue("provider revision authentication mode")
        })?,
        connector_ready: row.get("connector_ready"),
        credential_version_id: row.get("credential_version_id"),
        credential_version: row.get("credential_version"),
        source_etag: row.get("source_etag"),
        activated_by: row.get("activated_by"),
        activated_at: row.get("activated_at"),
        model_count: catalog_count(&row, "model_count")?,
        enabled_model_count: catalog_count(&row, "enabled_model_count")?,
        capability_count: catalog_count(&row, "capability_count")?,
        certified_capability_count: catalog_count(&row, "certified_capability_count")?,
    })
}

fn capability_from_row(row: &PgRow) -> Result<CapabilityRecord, CatalogError> {
    Ok(CapabilityRecord {
        operation: row
            .get::<String, _>("operation")
            .parse()
            .map_err(|_| PersistenceError::InvalidStoredValue("capability operation"))?,
        surface: row
            .get::<String, _>("surface")
            .parse()
            .map_err(|_| PersistenceError::InvalidStoredValue("capability surface"))?,
        mode: row
            .get::<String, _>("mode")
            .parse()
            .map_err(|_| PersistenceError::InvalidStoredValue("capability transport mode"))?,
        source: row
            .get::<String, _>("source")
            .parse()
            .map_err(|_| PersistenceError::InvalidStoredValue("capability source"))?,
        certified_at: row.get("certified_at"),
    })
}

fn catalog_count(row: &PgRow, column: &str) -> Result<u64, CatalogError> {
    u64::try_from(row.get::<i64, _>(column)).map_err(|_| {
        CatalogError::Invalid(format!(
            "stored provider {column} is outside the supported range"
        ))
    })
}

async fn ensure_provider_exists(store: &PgStore, provider_id: Uuid) -> Result<(), CatalogError> {
    let exists: bool = sqlx::query_scalar("SELECT EXISTS (SELECT 1 FROM providers WHERE id = $1)")
        .bind(provider_id)
        .fetch_one(store.pool())
        .await?;
    exists.then_some(()).ok_or(CatalogError::NotFound)
}

async fn ensure_provider_revision_exists(
    store: &PgStore,
    provider_id: Uuid,
    revision_id: Uuid,
) -> Result<(), CatalogError> {
    let exists: bool = sqlx::query_scalar(
        "SELECT EXISTS (SELECT 1 FROM provider_revisions \
         WHERE provider_id = $1 AND id = $2)",
    )
    .bind(provider_id)
    .bind(revision_id)
    .fetch_one(store.pool())
    .await?;
    exists.then_some(()).ok_or(CatalogError::NotFound)
}

fn certification_tuple_from_row(
    row: PgRow,
) -> Result<(OperationKind, Surface, TransportMode), CatalogError> {
    Ok((
        row.get::<String, _>("operation")
            .parse()
            .map_err(|_| PersistenceError::InvalidStoredValue("capability operation"))?,
        row.get::<String, _>("surface")
            .parse()
            .map_err(|_| PersistenceError::InvalidStoredValue("capability surface"))?,
        row.get::<String, _>("mode")
            .parse()
            .map_err(|_| PersistenceError::InvalidStoredValue("capability transport mode"))?,
    ))
}

fn certification_result_tuples(
    outcomes: &[CapabilityCertificationResult],
) -> Result<BTreeSet<(OperationKind, Surface, TransportMode)>, CatalogError> {
    if outcomes.is_empty() || outcomes.len() > 16 {
        return Err(CatalogError::Invalid(
            "certification requires 1-16 reviewed capability tuples".to_owned(),
        ));
    }
    let mut submitted = BTreeSet::new();
    for outcome in outcomes {
        validate_capability(&CapabilityRecord {
            operation: outcome.operation,
            surface: outcome.surface,
            mode: outcome.mode,
            source: CapabilitySource::Declared,
            certified_at: None,
        })?;
        if outcome.detail.chars().count() > 500
            || outcome
                .error_code
                .as_deref()
                .is_some_and(|code| code.chars().count() > 100)
            || (outcome.succeeded
                && (outcome.evidence_kind.is_none() || outcome.error_code.is_some()))
            || (!outcome.succeeded && outcome.evidence_kind.is_some())
        {
            return Err(CatalogError::Invalid(
                "certification result evidence is invalid".to_owned(),
            ));
        }
        if !submitted.insert((outcome.operation, outcome.surface, outcome.mode)) {
            return Err(CatalogError::Invalid(
                "certification capability tuples must be unique".to_owned(),
            ));
        }
    }
    Ok(submitted)
}

async fn insert_certification_results(
    transaction: &mut Transaction<'_, Postgres>,
    run_id: Uuid,
    outcomes: &[CapabilityCertificationResult],
) -> Result<(), CatalogError> {
    for outcome in outcomes {
        sqlx::query(
            "INSERT INTO capability_certification_results \
             (certification_run_id, operation, surface, mode, succeeded, evidence_kind, error_code, detail) \
             VALUES ($1, $2, $3, $4, $5, $6, $7, $8)",
        )
        .bind(run_id)
        .bind(outcome.operation.as_str())
        .bind(outcome.surface.as_str())
        .bind(outcome.mode.as_str())
        .bind(outcome.succeeded)
        .bind(&outcome.evidence_kind)
        .bind(&outcome.error_code)
        .bind(&outcome.detail)
        .execute(&mut **transaction)
        .await?;
    }
    Ok(())
}

async fn audit_optional_in_transaction(
    transaction: &mut Transaction<'_, Postgres>,
    actor: Option<Uuid>,
    action: &str,
    resource_type: &str,
    resource_id: Uuid,
    outcome: &str,
) -> Result<(), CatalogError> {
    sqlx::query(
        "INSERT INTO audit_events (id, actor_user_id, action, resource_type, resource_id, outcome) \
         VALUES ($1, $2, $3, $4, $5, $6)",
    )
    .bind(Uuid::now_v7())
    .bind(actor)
    .bind(action)
    .bind(resource_type)
    .bind(resource_id.to_string())
    .bind(outcome)
    .execute(&mut **transaction)
    .await?;
    Ok(())
}

async fn audit_in_transaction(
    transaction: &mut Transaction<'_, Postgres>,
    actor: Uuid,
    action: &str,
    resource_type: &str,
    resource_id: Uuid,
    outcome: &str,
) -> Result<(), CatalogError> {
    audit_optional_in_transaction(
        transaction,
        Some(actor),
        action,
        resource_type,
        resource_id,
        outcome,
    )
    .await
}

/// Resets capability evidence for a single model back to declared defaults.
async fn clear_model_capability_evidence(
    transaction: &mut Transaction<'_, Postgres>,
    model_id: Uuid,
) -> Result<(), CatalogError> {
    sqlx::query(
        "UPDATE model_capabilities SET source = 'declared', certified_at = NULL, \
                certification_context_id = NULL, review_revision = NULL, \
                certification_run_id = NULL, certification_evidence_kind = NULL \
         WHERE provider_model_id = $1",
    )
    .bind(model_id)
    .execute(&mut **transaction)
    .await?;
    Ok(())
}

/// Resets capability evidence for every model owned by a provider.
async fn clear_provider_model_capability_evidence(
    transaction: &mut Transaction<'_, Postgres>,
    provider_id: Uuid,
) -> Result<(), CatalogError> {
    sqlx::query(
        "UPDATE model_capabilities SET source = 'declared', certified_at = NULL, \
                certification_context_id = NULL, review_revision = NULL, \
                certification_run_id = NULL, certification_evidence_kind = NULL \
         WHERE provider_model_id IN (SELECT id FROM provider_models WHERE provider_id = $1)",
    )
    .bind(provider_id)
    .execute(&mut **transaction)
    .await?;
    Ok(())
}

async fn draft_operations(
    pool: &sqlx::PgPool,
    id: Uuid,
) -> Result<Vec<OperationKind>, CatalogError> {
    sqlx::query_scalar(
        "SELECT operation FROM route_draft_operations WHERE route_draft_id = $1 ORDER BY operation",
    )
    .bind(id)
    .fetch_all(pool)
    .await?
    .into_iter()
    .map(|value: String| {
        value
            .parse()
            .map_err(|_| PersistenceError::InvalidStoredValue("route draft operation").into())
    })
    .collect()
}

async fn revision_operations(
    pool: &sqlx::PgPool,
    id: Uuid,
) -> Result<Vec<OperationKind>, CatalogError> {
    sqlx::query_scalar("SELECT operation FROM route_revision_operations WHERE route_revision_id = $1 ORDER BY operation")
        .bind(id).fetch_all(pool).await?
        .into_iter()
        .map(|value: String| value.parse().map_err(|_| PersistenceError::InvalidStoredValue("route revision operation").into()))
        .collect()
}

async fn draft_targets(
    pool: &sqlx::PgPool,
    id: Uuid,
) -> Result<Vec<RouteTargetRecord>, CatalogError> {
    target_rows(
        sqlx::query(
            "SELECT rdt.id, rdt.routing_id, rdt.provider_model_id, p.id AS provider_id, pr.name AS provider_name, \
                    prm.upstream_model AS provider_model, rdt.priority, rdt.weight, rdt.timeout_ms, rdt.position \
             FROM route_draft_targets rdt \
             JOIN provider_models pm ON pm.id = rdt.provider_model_id \
             JOIN providers p ON p.id = pm.provider_id \
             JOIN provider_revisions pr ON pr.id = p.active_revision_id \
             JOIN provider_revision_models prm ON prm.provider_revision_id = pr.id \
               AND prm.source_provider_model_id = pm.id \
             WHERE rdt.route_draft_id = $1 ORDER BY rdt.position",
        ).bind(id).fetch_all(pool).await?
    )
}

async fn revision_targets(
    pool: &sqlx::PgPool,
    id: Uuid,
) -> Result<Vec<RouteTargetRecord>, CatalogError> {
    target_rows(
        sqlx::query(
            "SELECT rrt.id, rrt.routing_id, rrt.provider_model_id, p.id AS provider_id, pr.name AS provider_name, \
                    prm.upstream_model AS provider_model, rrt.priority, rrt.weight, rrt.timeout_ms, rrt.position \
             FROM route_revision_targets rrt \
             JOIN provider_models pm ON pm.id = rrt.provider_model_id \
             JOIN providers p ON p.id = pm.provider_id \
             JOIN provider_revisions pr ON pr.id = p.active_revision_id \
             JOIN provider_revision_models prm ON prm.provider_revision_id = pr.id \
               AND prm.source_provider_model_id = pm.id \
             WHERE rrt.route_revision_id = $1 ORDER BY rrt.position",
        ).bind(id).fetch_all(pool).await?
    )
}

fn target_rows(rows: Vec<sqlx::postgres::PgRow>) -> Result<Vec<RouteTargetRecord>, CatalogError> {
    Ok(rows
        .into_iter()
        .map(|row| RouteTargetRecord {
            id: row.get("id"),
            routing_id: row.get("routing_id"),
            provider_model_id: row.get("provider_model_id"),
            provider_id: row.get("provider_id"),
            provider_name: row.get("provider_name"),
            provider_model: row.get("provider_model"),
            priority: row.get("priority"),
            weight: row.get("weight"),
            timeout_ms: row.get("timeout_ms"),
            position: row.get("position"),
        })
        .collect())
}

fn provider_revision_model_map(
    models: &[ProviderModelRecord],
) -> BTreeMap<String, (String, bool, Option<DateTime<Utc>>)> {
    models
        .iter()
        .map(|model| {
            (
                model.upstream_model.clone(),
                (
                    model.display_name.clone(),
                    model.enabled,
                    model.discovered_at,
                ),
            )
        })
        .collect()
}

fn provider_revision_capability_set(models: &[ProviderModelRecord]) -> BTreeSet<String> {
    models
        .iter()
        .flat_map(|model| {
            model.capabilities.iter().map(move |capability| {
                format!(
                    "{}/{}/{}/{}",
                    model.upstream_model, capability.operation, capability.surface, capability.mode
                )
            })
        })
        .collect()
}

fn revision_target_map(targets: &[RouteTargetRecord]) -> BTreeMap<String, (i32, i32, i32, i32)> {
    targets
        .iter()
        .map(|target| {
            (
                format!("{}/{}", target.provider_id, target.provider_model),
                (
                    target.priority,
                    target.weight,
                    target.timeout_ms,
                    target.position,
                ),
            )
        })
        .collect()
}
