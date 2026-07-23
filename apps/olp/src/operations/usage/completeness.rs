use axum::{
    Json,
    extract::{Query, State},
    http::HeaderMap,
};
use olp_storage::UsageCompleteness;
use serde::Serialize;
use utoipa::ToSchema;

use super::{UsageQuery, UsageRangeCoverageResponse};
use crate::{
    ManagementState, Problem,
    management_api::{Permission, require_permission, require_read_session},
    operations::{
        helpers::map_operations, request_metadata::RequestMetadataConsumerStatusResponse,
    },
};

#[derive(Debug, Serialize, ToSchema)]
pub(in crate::operations) struct UsageCompletenessResponse {
    request_count: u64,
    priced_count: u64,
    unpriced_count: u64,
    incomplete_count: u64,
    request_metadata_gap_events: u64,
    uncertain_request_metadata_gap_count: u64,
    estimated_cost: Option<String>,
    currency: Option<String>,
    coverage: UsageRangeCoverageResponse,
    request_metadata_consumer: RequestMetadataConsumerStatusResponse,
    complete: bool,
}

impl From<UsageCompleteness> for UsageCompletenessResponse {
    fn from(summary: UsageCompleteness) -> Self {
        Self {
            request_count: summary.request_count,
            priced_count: summary.priced_count,
            unpriced_count: summary.unpriced_count,
            incomplete_count: summary.incomplete_count,
            request_metadata_gap_events: summary.request_metadata_gap_events,
            uncertain_request_metadata_gap_count: summary.uncertain_request_metadata_gap_count,
            estimated_cost: summary.estimated_cost,
            currency: summary.currency,
            coverage: summary.coverage.into(),
            request_metadata_consumer: summary.request_metadata_consumer.into(),
            complete: summary.complete,
        }
    }
}

#[utoipa::path(
    get,
    path = "/api/v1/usage/completeness",
    tag = "usage",
    params(UsageQuery),
    responses((status = 200, description = "Usage and pricing completeness", body = UsageCompletenessResponse))
)]
pub(in crate::operations) async fn usage_completeness(
    State(state): State<ManagementState>,
    headers: HeaderMap,
    Query(query): Query<UsageQuery>,
) -> Result<Json<UsageCompletenessResponse>, Problem> {
    let principal = require_read_session(&state, &headers).await?;
    require_permission(&principal, Permission::ReadOperations)?;
    query.validate()?;
    let filters = query.filters()?;
    let summary = state
        .store()
        .usage_completeness(&filters)
        .await
        .map_err(map_operations)?;
    Ok(Json(summary.into()))
}
