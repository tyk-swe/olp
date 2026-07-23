mod buffer;
mod delivery_health;
mod ingestion;
mod reconciliation;

pub use buffer::{
    RequestMetadataBufferSnapshot, RequestMetadataEmitError, RequestMetadataEmitter,
    RequestMetadataReceiver,
};
pub use delivery_health::{
    REQUEST_METADATA_CONSUMER_STALE_AFTER_SECONDS, RequestMetadataConsumerHealth,
    RequestMetadataConsumerState, RequestMetadataConsumerStatus,
};
pub use ingestion::{
    RequestAttemptMetadata, RequestMetadataEvent, RequestMetadataPersistenceOutcome,
};
pub use reconciliation::{
    REQUEST_METADATA_GATEWAY_EPOCH_STALE_AFTER_SECONDS, RequestMetadataEpochAcknowledgement,
    RequestMetadataEpochDetection, RequestMetadataEpochHealth, RequestMetadataGatewayEpochRecord,
    RequestMetadataGatewayEpochState, RequestMetadataLossReport,
};

pub(crate) const REQUEST_METADATA_EVENT_REPLAY_HORIZON_DAYS: i32 = 7;
pub(crate) const REQUEST_METADATA_EVENT_FUTURE_SKEW_MINUTES: i32 = 5;

#[cfg(test)]
use chrono::Utc;
#[cfg(test)]
use olp_domain::{OperationKind, Surface};
#[cfg(test)]
use rust_decimal::Decimal;
#[cfg(test)]
use std::sync::Arc;
#[cfg(test)]
use tokio::sync::watch;
#[cfg(test)]
use uuid::Uuid;

#[cfg(test)]
use buffer::RequestMetadataBufferHealth;
#[cfg(test)]
use reconciliation::request_metadata_gap_count_from_decimal;

#[cfg(test)]
mod tests;
