mod buffer;
mod helpers;
mod ingestion;
mod query;
mod reconciliation;

pub use buffer::{UsageBufferSnapshot, UsageEmitError, UsageEmitter, UsageReceiver};
pub use ingestion::{UsageAttempt, UsageEvent, UsagePersistenceOutcome};
pub use query::{
    USAGE_CONSUMER_STALE_AFTER_SECONDS, UsageConsumerHealth, UsageConsumerState,
    UsageConsumerStatus,
};
pub use reconciliation::{
    USAGE_GATEWAY_EPOCH_STALE_AFTER_SECONDS, UsageEpochAcknowledgement, UsageEpochDetection,
    UsageEpochHealth, UsageGatewayEpochRecord, UsageGatewayEpochState, UsageLossReport,
};

pub(crate) const USAGE_EVENT_REPLAY_HORIZON_DAYS: i64 = 7;
pub(crate) const USAGE_EVENT_FUTURE_SKEW_MINUTES: i64 = 5;

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
use buffer::UsageBufferHealth;
#[cfg(test)]
use helpers::usage_gap_count_from_decimal;

#[cfg(test)]
mod tests;
