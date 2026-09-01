use chrono::{DateTime, Utc};
use olp_engine::domain::{
    canonical::identity::{OperationKind, Surface},
    ids::{ProviderId, RouteSlug},
    provider::ProviderAuthMode,
    routing::provider::ProviderKind,
};
use thiserror::Error;
use uuid::Uuid;

use crate::{
    error::Error as PersistenceError,
    runtime::{PublishedRuntimeRelease, compiler::RuntimeCompileError},
    security::envelope::EncryptedSecret,
};

mod provider_lifecycle;
pub mod resources;
mod route_lifecycle;
mod validation;
pub use validation::{MAX_MODEL_CAPABILITY_TUPLES, MAX_PAGE_SIZE};

#[derive(Debug, Error)]
pub enum Error {
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
    #[error("route draft is invalid: {0}")]
    InvalidRoute(String),
    #[error(transparent)]
    RuntimeCompile(#[from] RuntimeCompileError),
    #[error("this idempotency key has already been used")]
    IdempotencyConflict,
    #[error("an operation with this idempotency key is still in progress")]
    IdempotencyInProgress,
}

impl From<sqlx::Error> for Error {
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
pub struct RuntimeProvider {
    pub provider_id: ProviderId,
    pub provider_revision_id: Option<Uuid>,
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
    /// The draft is consumed by activation: it returns to `draft` under this
    /// new ETag, so a second activation has to revalidate first instead of
    /// minting a byte-identical revision.
    pub draft_etag: Uuid,
    pub release: PublishedRuntimeRelease,
}

#[cfg(test)]
mod tests;
