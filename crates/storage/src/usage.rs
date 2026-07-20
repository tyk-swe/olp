use std::fmt;

use chrono::{DateTime, Utc};
use olp_domain::OperationKind;
use uuid::Uuid;

mod breakdown;
mod completeness;
pub(super) mod query;
mod series;
mod summary;

pub use breakdown::{UsageBreakdown, UsageBreakdownReport};
pub use completeness::UsageCompleteness;
pub use series::{UsagePoint, UsageSeries};
pub use summary::UsageSummary;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum UsageGranularity {
    Hour,
    Day,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum UsageDimension {
    Route,
    Provider,
    Model,
    ApiKey,
    Operation,
}

#[derive(Clone, Debug)]
pub struct UsageFilters {
    pub observed_after: DateTime<Utc>,
    pub observed_before: DateTime<Utc>,
    pub route_slug: Option<String>,
    pub provider_id: Option<Uuid>,
    pub upstream_model: Option<String>,
    pub api_key_id: Option<Uuid>,
    pub operation: Option<OperationKind>,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct UsageRangeCoverage {
    /// False only when a requested partial hour exists solely as a retained
    /// hourly aggregate and therefore cannot be sliced without guessing.
    pub range_complete: bool,
    /// Signals that returned totals cover only the exact, representable subset
    /// of the requested range. OLP never prorates hourly aggregates.
    pub approximate: bool,
    pub excluded_partial_aggregate_boundaries: u8,
}

impl fmt::Display for UsageDimension {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self {
            Self::Route => "route",
            Self::Provider => "provider",
            Self::Model => "model",
            Self::ApiKey => "api_key",
            Self::Operation => "operation",
        })
    }
}
