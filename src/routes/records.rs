use crate::protocols::canonical::identity::OperationKind;
use crate::protocols::canonical::identity::Surface;
use crate::protocols::canonical::identity::TransportMode;
use crate::providers::types::RouteDraftState;
use chrono::DateTime;
use chrono::Utc;
use uuid::Uuid;

#[derive(Clone, Debug)]
pub struct RouteTargetRecord {
    pub id: Uuid,
    pub routing_id: Uuid,
    pub provider_model_id: Uuid,
    pub provider_id: Uuid,
    pub provider_name: String,
    pub upstream_model: String,
    /// Whether the target's provider/model is still present and enabled in the
    /// provider's activated revision. Stored targets are always returned; this
    /// distinguishes a usable target from one whose provider was disabled or
    /// whose model left the activated revision.
    pub available: bool,
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
    /// Email of the operator who created the draft; absent once that user is
    /// removed.
    pub created_by_email: Option<String>,
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
    pub routing_policy: crate::routes::policy::RoutingPolicy,
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
    /// Email of the operator who created the route; absent once that user is
    /// removed.
    pub created_by_email: Option<String>,
    pub revision_count: u64,
    pub latest_revision: RouteRevisionRecord,
}

#[derive(Clone, Debug)]
pub struct RouteSimulationTarget {
    pub decision: Option<crate::inference::provider_selection::RoutingDecision>,
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
    pub routing_policy_changed: bool,
    pub routing_policy_before: crate::routes::policy::RoutingPolicy,
    pub routing_policy_after: crate::routes::policy::RoutingPolicy,
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
