use chrono::{DateTime, Utc};
use olp_domain::{OperationKind, Surface};
use serde::{Deserialize, Serialize};
use thiserror::Error;
use uuid::Uuid;

mod lifecycle;
mod queries;
mod reconciliation;

const MAX_PAGE_SIZE: u16 = 200;

#[derive(Debug, Error)]
pub enum MediaJobError {
    #[error("database operation failed")]
    Database(#[from] sqlx::Error),
    #[error("media job was not found")]
    NotFound,
    #[error("media job changed; refresh and retry")]
    PreconditionFailed,
    #[error("upstream media job identity conflicts with stored metadata")]
    UpstreamIdentityConflict,
    #[error("media job input is invalid: {0}")]
    Invalid(String),
}

#[derive(Clone, Copy, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum MediaJobLifecycle {
    Creating,
    Active,
    CreateAmbiguous,
    CreateCleanupPending,
    DeletePending,
    Deleted,
}

impl MediaJobLifecycle {
    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::Creating => "creating",
            Self::Active => "active",
            Self::CreateAmbiguous => "create_ambiguous",
            Self::CreateCleanupPending => "create_cleanup_pending",
            Self::DeletePending => "delete_pending",
            Self::Deleted => "deleted",
        }
    }

    fn parse(value: &str) -> Result<Self, MediaJobError> {
        match value {
            "creating" => Ok(Self::Creating),
            "active" => Ok(Self::Active),
            "create_ambiguous" => Ok(Self::CreateAmbiguous),
            "create_cleanup_pending" => Ok(Self::CreateCleanupPending),
            "delete_pending" => Ok(Self::DeletePending),
            "deleted" => Ok(Self::Deleted),
            _ => Err(MediaJobError::Invalid(
                "database returned an unknown media job lifecycle".to_owned(),
            )),
        }
    }

    #[must_use]
    pub const fn needs_reconciliation(self) -> bool {
        matches!(
            self,
            Self::Creating
                | Self::CreateAmbiguous
                | Self::CreateCleanupPending
                | Self::DeletePending
        )
    }
}

#[derive(Clone, Copy, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum MediaJobState {
    Queued,
    Running,
    Succeeded,
    Failed,
    Cancelled,
}

impl MediaJobState {
    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::Queued => "queued",
            Self::Running => "running",
            Self::Succeeded => "succeeded",
            Self::Failed => "failed",
            Self::Cancelled => "cancelled",
        }
    }

    fn parse(value: &str) -> Result<Self, MediaJobError> {
        match value {
            "queued" => Ok(Self::Queued),
            "running" => Ok(Self::Running),
            "succeeded" => Ok(Self::Succeeded),
            "failed" => Ok(Self::Failed),
            "cancelled" => Ok(Self::Cancelled),
            _ => Err(MediaJobError::Invalid(
                "database returned an unknown media job state".to_owned(),
            )),
        }
    }
}

#[derive(Clone, Debug)]
pub struct NewMediaJobReservation {
    pub id: Uuid,
    /// Runtime generation pinned before the upstream side effect. Production
    /// releases resolve this to an immutable provider revision in PostgreSQL.
    pub runtime_generation_id: Uuid,
    pub api_key_id: Uuid,
    pub provider_id: Uuid,
    pub provider_model: String,
    pub route_slug: String,
    pub operation: OperationKind,
    pub surface: Surface,
}

#[derive(Clone, Debug, Default)]
pub struct MediaJobFilters {
    pub api_key_id: Option<Uuid>,
    pub provider_id: Option<Uuid>,
    pub route_slug: Option<String>,
    /// Optional multi-route allowlist used by client-facing lifecycle lists.
    /// Empty means all routes.
    pub route_slugs: Vec<String>,
    pub operation: Option<OperationKind>,
    pub surface: Option<Surface>,
    pub state: Option<MediaJobState>,
    pub lifecycle: Option<MediaJobLifecycle>,
    pub created_after: Option<DateTime<Utc>>,
    pub created_before: Option<DateTime<Utc>>,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum MediaJobOrder {
    Ascending,
    Descending,
}

#[derive(Clone, Debug)]
pub struct MediaJobRecord {
    pub id: Uuid,
    pub upstream_job_id: Option<String>,
    pub api_key_id: Uuid,
    pub provider_id: Uuid,
    pub provider_name: String,
    pub provider_model: String,
    pub route_slug: String,
    pub operation: OperationKind,
    pub surface: Surface,
    pub state: MediaJobState,
    pub lifecycle: MediaJobLifecycle,
    pub progress_percent: Option<f32>,
    pub content_available: bool,
    pub expires_at: Option<DateTime<Utc>>,
    pub error_class: Option<String>,
    pub completed_at: Option<DateTime<Utc>>,
    pub last_polled_at: Option<DateTime<Utc>>,
    pub reconciliation_error: Option<String>,
    pub deleted_at: Option<DateTime<Utc>>,
    pub runtime_generation_id: Option<Uuid>,
    pub provider_revision_id: Option<Uuid>,
    pub reconciliation_claim_id: Option<Uuid>,
    pub reconciliation_attempts: u32,
    pub next_reconciliation_at: DateTime<Utc>,
    pub last_reconciliation_at: Option<DateTime<Utc>>,
    pub etag: Uuid,
    pub created_at: DateTime<Utc>,
    pub updated_at: DateTime<Utc>,
}

#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub struct MediaReconciliationSummary {
    pub pending: u64,
    pub stale: u64,
    pub failed: u64,
    pub unbound: u64,
    pub oldest_pending_at: Option<DateTime<Utc>>,
}

#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub struct MediaReconciliationPass {
    pub claimed: u16,
    pub completed: u16,
    pub failed: u16,
}

#[derive(Clone, Debug)]
pub struct MediaJobUpdate {
    pub state: MediaJobState,
    pub progress_percent: Option<f32>,
    pub content_available: bool,
    pub expires_at: Option<DateTime<Utc>>,
    pub error_class: Option<String>,
    pub last_polled_at: DateTime<Utc>,
}

#[cfg(test)]
mod tests;
