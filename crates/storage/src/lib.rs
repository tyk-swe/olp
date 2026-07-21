//! PostgreSQL authority and cryptographic storage primitives for OpenLLMProxy.
//!
//! This crate deliberately owns SQL, encryption, and durable event delivery. It
//! does not expose SQLx types through the core ports.

mod access;
mod configuration;
mod identity;
mod limits;
mod maintenance;
mod media_jobs;
mod oidc;
mod operations;
mod reencryption;
mod request_metadata;
mod runtime_compiler;
mod security;
mod store;
mod usage;
mod valkey;

pub use identity::{
    AcceptInvitation, AcceptedInvitation, IdentityError, InvitationCreated, InvitationRecord,
    NewInvitation, SessionRecord, UserRecord,
};
pub use limits::{DistributedLimiter, LimitDimension, LimitError, LimitLease, LimitRequest};
pub use maintenance::{MaintenanceError, MaintenanceReport};
pub use media_jobs::{
    MediaJobError, MediaJobFilters, MediaJobLifecycle, MediaJobOrder, MediaJobRecord,
    MediaJobState, MediaJobUpdate, MediaReconciliationPass, MediaReconciliationSummary,
    NewMediaJobReservation,
};
pub use oidc::{
    CompleteOidcLink, CompleteOidcLogin, NewOidcFlow, OidcAuthenticatedUser, OidcConfiguration,
    OidcError, OidcFlowMaterial, OidcFlowPurpose, OidcFlowRecord, OidcIdentityRecord,
    OidcRoleMapping, UpsertOidcConfiguration,
};
pub use operations::{
    AttemptRecord, AuditRecord, OperationsError, OperationsPage, PriceInput, PricingRevisionRecord,
    PrometheusOperationsSummary, ProviderHealthRecord, RequestDetail, RequestFilters,
    RequestRecord, RuntimeGenerationRecord, SettingRecord, TimestampCursor, UsageBreakdown,
    UsageBreakdownReport, UsageCompleteness, UsageDimension, UsageFilters, UsageGranularity,
    UsagePoint, UsageRangeCoverage, UsageSeries, UsageSummary,
};
pub use reencryption::{
    EncryptedTable, KeyVersionReference, MasterKeyEncryptionStatus, MasterKeyReencryptionBatch,
    MasterKeyVerification, ReencryptionError,
};
pub use request_metadata::{
    REQUEST_METADATA_CONSUMER_STALE_AFTER_SECONDS,
    REQUEST_METADATA_GATEWAY_EPOCH_STALE_AFTER_SECONDS, RequestAttemptMetadata,
    RequestMetadataBufferSnapshot, RequestMetadataConsumerHealth, RequestMetadataConsumerState,
    RequestMetadataConsumerStatus, RequestMetadataEmitError, RequestMetadataEmitter,
    RequestMetadataEpochAcknowledgement, RequestMetadataEpochDetection, RequestMetadataEpochHealth,
    RequestMetadataEvent, RequestMetadataGatewayEpochRecord, RequestMetadataGatewayEpochState,
    RequestMetadataLossReport, RequestMetadataPersistenceOutcome, RequestMetadataReceiver,
};
pub use runtime_compiler::RuntimeCompileError;
pub use security::{
    ApiKeyMaterial, AuthHmacKey, EncryptedSecret, InvitationMaterial, MasterKey, ParsedApiKey,
    SecurityError, SessionMaterial, constant_time_eq, credential_aad, hash_password,
    idempotency_replay_aad, idempotency_replay_scope, oidc_client_secret_aad,
    oidc_flow_payload_aad, verify_password,
};
pub use store::{
    IdempotencyOutcome, IdempotencyResponse, InstallationSetupInput, InstallationSetupResult,
    LocalPasswordUser, OutboxRecord, PersistenceError, PgStore, PublishedRuntimeRelease,
    ReplayableIdempotency, RequestMetadataGap, SessionPrincipal, idempotency_fingerprint,
    idempotency_secret_digest,
};
pub use valkey::{
    REQUEST_METADATA_STREAM, RuntimeHintPublisher, RuntimeHintSubscriber, ValkeyAdapterError,
    preflight_request_metadata_stream_upgrade, run_request_metadata_consumer,
};

/// Truncates a query result fetched with `limit + 1` and derives the cursor
/// from the last visible item only when another page exists.
fn split_page<T, C>(
    mut items: Vec<T>,
    limit: usize,
    cursor: impl FnOnce(&T) -> C,
) -> (Vec<T>, Option<C>) {
    let has_more = items.len() > limit;
    items.truncate(limit);
    let next_cursor = if has_more {
        items.last().map(cursor)
    } else {
        None
    };
    (items, next_cursor)
}

/// SQLx embeds and checks every migration at compile time. Migrations execute
/// only in `migrate`/`all` mode, never implicitly in a gateway process.
pub static MIGRATOR: sqlx::migrate::Migrator = sqlx::migrate!("./migrations");
pub use access::{AccessError, ApiKeyCreated, ApiKeyRevoked, NewApiKeyRecord};
pub use configuration::{
    ApiKeyMutationResult, ApiKeyRecord, ApiKeyRotationResult, CapabilityCertificationApplied,
    CapabilityCertificationOutcome, CapabilityRecord, ConfigurationError, ConfigurationPage,
    CredentialVersionRecord, DiscoveredModelInput, NewProviderDraft, NewRouteDraft, NewRouteTarget,
    ProviderActivated, ProviderDraftCreated, ProviderModelInventoryRecord, ProviderModelRecord,
    ProviderMutationResult, ProviderRecord, ProviderRevisionDiff, ProviderRevisionRecord,
    ReplaceRouteDraftInput, RotateApiKeyInput, RotateCredentialInput, RouteActivated,
    RouteDraftCreated, RouteDraftRecord, RouteRecord, RouteRevisionDiff, RouteRevisionRecord,
    RouteSimulation, RouteSimulationTarget, RouteTargetRecord, RuntimeProviderConfiguration,
    StoredCredentialSecret, UpdateApiKeyInput, UpdateProvider,
};

#[cfg(test)]
mod tests {
    use super::split_page;

    #[test]
    fn split_page_distinguishes_complete_and_overfetched_results() {
        assert_eq!(split_page(vec![1, 2], 3, |item| *item), (vec![1, 2], None));
        assert_eq!(split_page(vec![1, 2], 2, |item| *item), (vec![1, 2], None));
        assert_eq!(
            split_page(vec![1, 2, 3], 2, |item| *item),
            (vec![1, 2], Some(2))
        );
    }

    #[test]
    fn split_page_never_derives_a_cursor_without_a_visible_item() {
        assert_eq!(split_page(vec![1], 0, |item| *item), (Vec::new(), None));
    }
}
