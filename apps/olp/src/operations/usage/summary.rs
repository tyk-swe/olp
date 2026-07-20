use axum::{
    Json,
    extract::{Query, State},
    http::HeaderMap,
};
use olp_storage::UsageSummary;
use serde::Serialize;
use utoipa::ToSchema;

use super::{UsageQuery, UsageRangeCoverageResponse};
use crate::{
    ApiState, Problem,
    management_api::{Permission, require_permission, require_read_session, require_store},
    operations::{
        helpers::map_operations, request_metadata::RequestMetadataConsumerStatusResponse,
    },
};

#[derive(Debug, Serialize, ToSchema)]
pub(in crate::operations) struct UsageSummaryResponse {
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

impl From<UsageSummary> for UsageSummaryResponse {
    fn from(summary: UsageSummary) -> Self {
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
pub(in crate::operations) async fn usage_summary(
    State(state): State<ApiState>,
    headers: HeaderMap,
    Query(query): Query<UsageQuery>,
) -> Result<Json<UsageSummaryResponse>, Problem> {
    let principal = require_read_session(&state, &headers).await?;
    require_permission(&principal, Permission::ReadOperations)?;
    query.validate()?;
    let filters = query.filters()?;
    let summary = require_store(&state)?
        .usage_summary(&filters)
        .await
        .map_err(map_operations)?;
    Ok(Json(summary.into()))
}
