use chrono::{DateTime, Utc};
use olp_storage::{UsageConsumerStatus, UsageFilters, UsageRangeCoverage};
use serde::{Deserialize, Serialize};
use utoipa::{IntoParams, ToSchema};
use uuid::Uuid;

use super::helpers::validate_time_range;
use crate::Problem;

pub(super) mod breakdown;
pub(super) mod completeness;
pub(super) mod epochs;
pub(super) mod series;
pub(super) mod summary;

#[derive(Debug, Deserialize, IntoParams, ToSchema)]
#[into_params(parameter_in = Query)]
pub(super) struct UsageQuery {
    pub(super) start: DateTime<Utc>,
    pub(super) end: DateTime<Utc>,
    pub(super) route: Option<String>,
    #[param(value_type = Option<String>, format = Uuid)]
    pub(super) provider_id: Option<Uuid>,
    pub(super) model: Option<String>,
    #[param(value_type = Option<String>, format = Uuid)]
    pub(super) api_key_id: Option<Uuid>,
    pub(super) operation: Option<String>,
}

impl UsageQuery {
    pub(super) fn filters(&self) -> Result<UsageFilters, Problem> {
        Ok(UsageFilters {
            observed_after: self.start,
            observed_before: self.end,
            route_slug: self.route.clone(),
            provider_id: self.provider_id,
            upstream_model: self.model.clone(),
            api_key_id: self.api_key_id,
            operation: self
                .operation
                .as_deref()
                .map(str::parse)
                .transpose()
                .map_err(|_| {
                    Problem::bad_request("invalid_operation", "The operation filter is invalid.")
                })?,
        })
    }

    pub(super) fn validate(&self) -> Result<(), Problem> {
        validate_time_range("start", self.start, "end", self.end)
    }
}

#[derive(Debug, Serialize, ToSchema)]
pub(super) struct UsageRangeCoverageResponse {
    range_complete: bool,
    approximate: bool,
    excluded_partial_aggregate_boundaries: u8,
}

impl From<UsageRangeCoverage> for UsageRangeCoverageResponse {
    fn from(coverage: UsageRangeCoverage) -> Self {
        Self {
            range_complete: coverage.range_complete,
            approximate: coverage.approximate,
            excluded_partial_aggregate_boundaries: coverage.excluded_partial_aggregate_boundaries,
        }
    }
}

#[derive(Debug, Serialize, ToSchema)]
pub(super) struct UsageConsumerStatusResponse {
    state: String,
    pending_events: u64,
    lag_events: u64,
    oldest_pending_at: Option<DateTime<Utc>>,
    checked_at: Option<DateTime<Utc>>,
    heartbeat_age_seconds: Option<u64>,
}

impl From<UsageConsumerStatus> for UsageConsumerStatusResponse {
    fn from(consumer: UsageConsumerStatus) -> Self {
        Self {
            state: consumer.state.as_str().to_owned(),
            pending_events: consumer.pending_events,
            lag_events: consumer.lag_events,
            oldest_pending_at: consumer.oldest_pending_at,
            checked_at: consumer.checked_at,
            heartbeat_age_seconds: consumer.heartbeat_age_seconds,
        }
    }
}
