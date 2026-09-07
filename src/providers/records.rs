use crate::crypto::envelope::EncryptedSecret;
use crate::protocols::canonical::identity::OperationKind;
use crate::protocols::canonical::identity::Surface;
use crate::protocols::canonical::identity::TransportMode;
use crate::providers::runtime_model::ProviderKind;
use crate::providers::types::CapabilitySource;
use crate::providers::types::ProviderState;
use crate::runtime::publication::PublishedRuntimeRelease;
use chrono::DateTime;
use chrono::Utc;
use uuid::Uuid;

/// Maximum number of models loaded from either immutable provider revision
/// while producing an in-memory revision diff.
pub const PROVIDER_REVISION_DIFF_MODEL_LIMIT: usize = 2_000;

/// Maximum number of capability tuples loaded from either immutable provider
/// revision while producing an in-memory revision diff.
pub const PROVIDER_REVISION_DIFF_CAPABILITY_LIMIT: usize = 32_000;

#[derive(Clone, Debug)]
pub struct ProviderModelPage {
    pub provider: ProviderRecord,
    pub items: Vec<ProviderModelRecord>,
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
    pub configuration: crate::providers::configuration::ProviderConfiguration,
    pub state: ProviderState,

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
    /// Email of the operator who created the provider; absent once that user
    /// is removed.
    pub created_by_email: Option<String>,
}

#[derive(Clone, Debug)]
pub struct ProviderRevisionRecord {
    pub id: Uuid,
    pub provider_id: Uuid,
    pub revision: i32,
    pub name: String,
    pub configuration: crate::providers::configuration::ProviderConfiguration,

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
    pub configuration: crate::providers::configuration::ProviderConfiguration,
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
