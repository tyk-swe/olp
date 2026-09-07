use crate::usage::reports::Coverage;
use crate::usage::reports::Filters;
use chrono::DateTime;
use chrono::Utc;
use serde::Deserialize;
use serde::Serialize;
use utoipa::IntoParams;
use utoipa::ToSchema;
use uuid::Uuid;

use crate::http::control::operations::helpers::validate_time_range;
use crate::http::problem::Problem;

#[derive(Debug, Deserialize, IntoParams, ToSchema)]
#[into_params(parameter_in = Query)]
pub(crate) struct UsageQuery {
    pub(crate) start: DateTime<Utc>,
    pub(crate) end: DateTime<Utc>,
    pub(crate) route: Option<String>,
    #[param(value_type = Option<String>, format = Uuid)]
    pub(crate) provider_id: Option<Uuid>,
    pub(crate) model: Option<String>,
    #[param(value_type = Option<String>, format = Uuid)]
    pub(crate) api_key_id: Option<Uuid>,
    pub(crate) operation: Option<String>,
}

impl UsageQuery {
    pub(crate) fn filters(&self) -> Result<Filters, Problem> {
        Ok(Filters {
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

    pub(crate) fn validate(&self) -> Result<(), Problem> {
        validate_time_range("start", self.start, "end", self.end)
    }
}

#[derive(Debug, Serialize, ToSchema)]
pub(crate) struct UsageRangeCoverageResponse {
    range_complete: bool,
    approximate: bool,
    excluded_partial_aggregate_boundaries: u8,
}

impl From<Coverage> for UsageRangeCoverageResponse {
    fn from(coverage: Coverage) -> Self {
        Self {
            range_complete: coverage.range_complete,
            approximate: coverage.approximate,
            excluded_partial_aggregate_boundaries: coverage.excluded_partial_aggregate_boundaries,
        }
    }
}

pub mod breakdown;

pub mod completeness;

pub mod series;

pub mod summary;

pub(crate) fn router()
-> utoipa_axum::router::OpenApiRouter<crate::http::control::state::ManagementState> {
    utoipa_axum::router::OpenApiRouter::new()
        .routes(utoipa_axum::routes!(
            crate::usage::http::breakdown::usage_breakdown
        ))
        .routes(utoipa_axum::routes!(
            crate::usage::http::completeness::usage_completeness
        ))
        .routes(utoipa_axum::routes!(
            crate::usage::http::series::usage_time_series
        ))
        .routes(utoipa_axum::routes!(
            crate::usage::http::summary::usage_summary
        ))
}
