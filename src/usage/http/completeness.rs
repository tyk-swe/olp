use crate::access::principal::ReadPrincipal;
use crate::usage::reports::completeness::Report;
use axum::Json;
use axum::extract::Query;
use axum::extract::State;
use serde::Serialize;
use utoipa::ToSchema;

use crate::access::permissions::require_permission;
use crate::access::policy::Permission;
use crate::http::control::operations::helpers::map_operations;
use crate::http::control::state::ManagementState;
use crate::http::problem::Problem;
use crate::usage::delivery_http::RequestMetadataConsumerStatusResponse;
use crate::usage::http::UsageQuery;
use crate::usage::http::UsageRangeCoverageResponse;

#[derive(Debug, Serialize, ToSchema)]
pub(crate) struct UsageCompletenessResponse {
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

impl From<Report> for UsageCompletenessResponse {
    fn from(summary: Report) -> Self {
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
    path = "/api/v3/usage/completeness",
    tag = "usage",
    params(UsageQuery),
    responses((status = 200, description = "Usage and pricing completeness", body = UsageCompletenessResponse))
, security(("sessionCookie" = [])))]
pub(crate) async fn usage_completeness(
    State(state): State<ManagementState>,
    Query(query): Query<UsageQuery>,
    ReadPrincipal(principal): ReadPrincipal,
) -> Result<Json<UsageCompletenessResponse>, Problem> {
    require_permission(&principal, Permission::ReadOperations)?;
    query.validate()?;
    let filters = query.filters()?;
    let summary = crate::usage::reports::completeness::usage_completeness(
        &state.request_boundary.pool,
        &filters,
    )
    .await
    .map_err(map_operations)?;
    Ok(Json(summary.into()))
}
