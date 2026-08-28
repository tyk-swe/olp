use crate::management::principal::ReadPrincipal;
use axum::{
    Json,
    extract::{Query, State},
};
use olp_db::usage::summary::Report;
use serde::Serialize;
use utoipa::ToSchema;

use super::{UsageQuery, UsageRangeCoverageResponse};
use crate::{
    bootstrap::mode_dependencies::ManagementState,
    management::operations::{
        helpers::map_operations, request_metadata::RequestMetadataConsumerStatusResponse,
    },
    management::permissions::require_permission,
    public_http::problem::Problem,
};
use olp_engine::domain::auth::Permission;

#[derive(Debug, Serialize, ToSchema)]
pub(in crate::management::operations) struct UsageSummaryResponse {
    request_count: u64,
    input_tokens: String,
    output_tokens: String,
    cached_input_tokens: String,
    media_units: String,
    estimated_cost: Option<String>,
    currency: Option<String>,
    unpriced_count: u64,
    incomplete_count: u64,
    /// Exact loss plus known in-flight lower bounds from unclean epochs.
    request_metadata_gap_events: u64,
    /// Unclean gateway epochs make completeness unknown even when their last
    /// durable in-flight lower bound was zero.
    uncertain_request_metadata_gap_count: u64,
    coverage: UsageRangeCoverageResponse,
    request_metadata_consumer: RequestMetadataConsumerStatusResponse,
    complete: bool,
}

impl From<Report> for UsageSummaryResponse {
    fn from(summary: Report) -> Self {
        Self {
            request_count: summary.request_count,
            input_tokens: summary.input_tokens,
            output_tokens: summary.output_tokens,
            cached_input_tokens: summary.cached_input_tokens,
            media_units: summary.media_units,
            estimated_cost: summary.estimated_cost,
            currency: summary.currency,
            unpriced_count: summary.unpriced_count,
            incomplete_count: summary.incomplete_count,
            request_metadata_gap_events: summary.request_metadata_gap_events,
            uncertain_request_metadata_gap_count: summary.uncertain_request_metadata_gap_count,
            coverage: summary.coverage.into(),
            request_metadata_consumer: summary.request_metadata_consumer.into(),
            complete: summary.complete,
        }
    }
}

#[utoipa::path(
    get,
    path = "/api/v1/usage/summary",
    tag = "usage",
    params(UsageQuery),
    responses((status = 200, description = "Usage and estimated-cost summary", body = UsageSummaryResponse))
)]
pub(in crate::management::operations) async fn usage_summary(
    State(state): State<ManagementState>,
    Query(query): Query<UsageQuery>,
    ReadPrincipal(principal): ReadPrincipal,
) -> Result<Json<UsageSummaryResponse>, Problem> {
    require_permission(&principal, Permission::ReadOperations)?;
    query.validate()?;
    let filters = query.filters()?;
    let summary = state
        .store()
        .usage_summary(&filters)
        .await
        .map_err(map_operations)?;
    Ok(Json(summary.into()))
}
