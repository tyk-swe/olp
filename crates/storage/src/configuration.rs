use chrono::{DateTime, Utc};
use olp_domain::{OperationKind, ProviderAuthMode, ProviderId, ProviderKind, RouteSlug, Surface};
use thiserror::Error;
use uuid::Uuid;

use crate::{EncryptedSecret, PersistenceError, PublishedRuntimeRelease, RuntimeCompileError};

mod provider_lifecycle;
mod resources;
mod route_lifecycle;
mod validation;

pub use resources::{
    ApiKeyMutationResult, ApiKeyRecord, ApiKeyRotationResult, CapabilityCertificationApplied,
    CapabilityCertificationOutcome, CapabilityRecord, ConfigurationPage, CredentialVersionRecord,
    DiscoveredModelInput, ProviderModelInventoryRecord, ProviderModelRecord,
    ProviderMutationResult, ProviderRecord, ProviderRevisionDiff, ProviderRevisionRecord,
    ReplaceRouteDraftInput, RotateApiKeyInput, RotateCredentialInput, RouteDraftRecord,
    RouteRecord, RouteRevisionDiff, RouteRevisionRecord, RouteSimulation, RouteSimulationTarget,
    RouteTargetRecord, StoredCredentialSecret, UpdateApiKeyInput, UpdateProvider,
};

#[derive(Debug, Error)]
pub enum ConfigurationError {
    #[error(transparent)]
    Persistence(#[from] PersistenceError),
    #[error("stored encrypted credential is malformed")]
    InvalidCredential,
    #[error("provider does not exist")]
    ProviderNotFound,
    #[error("provider cannot be activated without a credential and enabled model")]
    ProviderIncomplete,
    #[error("provider ETag does not match")]
    PreconditionFailed,
    #[error("configuration resource does not exist")]
    NotFound,
    #[error("configuration resource is in use")]
    InUse,
    #[error("configuration mutation is invalid: {0}")]
    Invalid(String),
    #[error("provider revision diff exceeds the {maximum} {dimension} per-revision server limit")]
    ProviderRevisionDiffTooLarge {
        dimension: &'static str,
        maximum: usize,
    },
    #[error("route draft does not exist")]
    RouteNotFound,
    #[error("route draft is not validated")]
    RouteNotValidated,
    #[error("route draft is invalid: {0}")]
    InvalidRoute(String),
    #[error(transparent)]
    RuntimeCompile(#[from] RuntimeCompileError),
    #[error("this idempotency key has already been used")]
    IdempotencyConflict,
    #[error("an operation with this idempotency key is still in progress")]
    IdempotencyInProgress,
}

impl From<sqlx::Error> for ConfigurationError {
    fn from(error: sqlx::Error) -> Self {
        Self::Persistence(PersistenceError::Database(error))
    }
}

#[derive(Debug)]
pub struct NewProviderDraft {
    pub provider_id: Uuid,
    pub credential_id: Option<Uuid>,
    pub model_id: Option<Uuid>,
    pub name: String,
    pub kind: ProviderKind,
    pub endpoint: Option<String>,
    pub cloud_region: Option<String>,
    pub cloud_project: Option<String>,
    pub deployment: Option<String>,
    pub api_version: Option<String>,
    pub auth_mode: ProviderAuthMode,
    pub connector_ready: bool,
    pub credential: Option<EncryptedSecret>,
    pub model: Option<String>,
    pub display_name: Option<String>,
    pub model_enabled: bool,
    pub surface: Option<Surface>,
    pub actor: Uuid,
    pub idempotency_key: String,
}

#[derive(Debug, Clone)]
pub struct ProviderDraftCreated {
    pub provider_id: Uuid,
    pub credential_id: Option<Uuid>,
    pub model_id: Option<Uuid>,
    pub etag: Uuid,
    pub created_at: DateTime<Utc>,
}

#[derive(Debug, Clone)]
pub struct ProviderActivated {
    pub etag: Uuid,
    pub release: PublishedRuntimeRelease,
}

#[derive(Debug, Clone)]
pub struct RuntimeProviderConfiguration {
    pub provider_id: ProviderId,
    pub kind: ProviderKind,
    pub endpoint: Option<String>,
    pub cloud_region: Option<String>,
    pub cloud_project: Option<String>,
    pub deployment: Option<String>,
    pub api_version: Option<String>,
    pub auth_mode: ProviderAuthMode,
    pub credential_id: Option<Uuid>,
    pub credential_version: Option<u32>,
    pub encrypted_credential: Option<EncryptedSecret>,
}

#[derive(Debug, Clone)]
pub struct NewRouteTarget {
    pub provider_id: Uuid,
    pub upstream_model: String,
    pub priority: u16,
    pub weight: u32,
    pub timeout_ms: u64,
}

#[derive(Debug)]
pub struct NewRouteDraft {
    pub slug: String,
    pub operations: Vec<OperationKind>,
    pub overall_timeout_ms: u64,
    pub max_attempts: u16,
    pub targets: Vec<NewRouteTarget>,
    pub actor: Uuid,
    pub idempotency_key: String,
}

#[derive(Debug, Clone)]
pub struct RouteDraftCreated {
    pub id: Uuid,
    pub slug: RouteSlug,
    pub etag: Uuid,
    pub created_at: DateTime<Utc>,
}

#[derive(Debug, Clone)]
pub struct RouteActivated {
    pub route_id: Uuid,
    pub revision_id: Uuid,
    pub revision: i32,
    pub release: PublishedRuntimeRelease,
}

#[cfg(test)]
mod tests;
