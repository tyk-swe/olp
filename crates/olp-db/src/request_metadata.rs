mod delivery_health;
mod ingestion;
mod reconciliation;
mod writer;

pub use delivery_health::{
    REQUEST_METADATA_CONSUMER_STALE_AFTER_SECONDS, RequestMetadataConsumerHealth,
    RequestMetadataConsumerState, RequestMetadataConsumerStatus,
};
pub use ingestion::RequestMetadataPersistenceOutcome;
pub use reconciliation::{
    REQUEST_METADATA_GATEWAY_EPOCH_STALE_AFTER_SECONDS, RequestMetadataEpochAcknowledgement,
    RequestMetadataEpochDetection, RequestMetadataEpochHealth, RequestMetadataGap,
    RequestMetadataGatewayEpochRecord, RequestMetadataGatewayEpochState, RequestMetadataLossReport,
};
pub use writer::{run_request_metadata_writer, run_request_metadata_writer_connecting};

pub(crate) const REQUEST_METADATA_EVENT_REPLAY_HORIZON_DAYS: i32 = 7;
pub(crate) const REQUEST_METADATA_EVENT_FUTURE_SKEW_MINUTES: i32 = 5;

#[cfg(test)]
use chrono::Utc;
#[cfg(test)]
use rust_decimal::Decimal;

#[cfg(test)]
use reconciliation::request_metadata_gap_count_from_decimal;

#[cfg(test)]
mod tests;
