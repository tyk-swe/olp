use std::fmt;

use chrono::{DateTime, Utc};
use olp_engine::domain::canonical::identity::OperationKind;
use uuid::Uuid;

pub mod breakdown;
pub mod completeness;
pub(super) mod query;
pub mod series;
pub mod summary;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Granularity {
    Hour,
    Day,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Dimension {
    Route,
    Provider,
    Model,
    ApiKey,
    Operation,
}

#[derive(Clone, Debug)]
pub struct Filters {
    pub observed_after: DateTime<Utc>,
    pub observed_before: DateTime<Utc>,
    pub route_slug: Option<String>,
    pub provider_id: Option<Uuid>,
    pub upstream_model: Option<String>,
    pub api_key_id: Option<Uuid>,
    pub operation: Option<OperationKind>,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct Coverage {
    /// False only when a requested partial hour exists solely as a retained
    /// hourly aggregate and therefore cannot be sliced without guessing.
    pub range_complete: bool,
    /// Signals that returned totals cover only the exact, representable subset
    /// of the requested range. OLP never prorates hourly aggregates.
    pub approximate: bool,
    pub excluded_partial_aggregate_boundaries: u8,
}

impl fmt::Display for Dimension {
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

#[cfg(test)]
mod tests {
    use super::Dimension;

    #[test]
    fn usage_dimension_names_are_stable() {
        for (dimension, expected) in [
            (Dimension::Route, "route"),
            (Dimension::Provider, "provider"),
            (Dimension::Model, "model"),
            (Dimension::ApiKey, "api_key"),
            (Dimension::Operation, "operation"),
        ] {
            assert_eq!(dimension.to_string(), expected);
        }
    }
}
