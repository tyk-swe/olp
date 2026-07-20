use axum::{
    Json,
    extract::{Query, State},
    http::HeaderMap,
};
use olp_storage::UsageCompleteness;
use serde::Serialize;
use utoipa::ToSchema;

use super::{UsageConsumerStatusResponse, UsageQuery, UsageRangeCoverageResponse};
use crate::{
    ApiState, Problem,
    management::{Permission, require_permission, require_read_session, require_store},
    operations::helpers::map_operations,
};

#[derive(Debug, Serialize, ToSchema)]
pub(in crate::operations) struct UsageCompletenessResponse {
    request_count: u64,
    priced_count: u64,
    unpriced_count: u64,
    incomplete_count: u64,
    ingestion_gap_events: u64,
    uncertain_gap_count: u64,
    estimated_cost: Option<String>,
    currency: Option<String>,
    coverage: UsageRangeCoverageResponse,
    consumer: UsageConsumerStatusResponse,
    complete: bool,
}

impl From<UsageCompleteness> for UsageCompletenessResponse {
    fn from(summary: UsageCompleteness) -> Self {
        Self {
            request_count: summary.request_count,
            priced_count: summary.priced_count,
            unpriced_count: summary.unpriced_count,
            incomplete_count: summary.incomplete_count,
            ingestion_gap_events: summary.ingestion_gap_events,
            uncertain_gap_count: summary.uncertain_gap_count,
            estimated_cost: summary.estimated_cost,
            currency: summary.currency,
            coverage: summary.coverage.into(),
            consumer: summary.consumer.into(),
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
    State(state): State<ApiState>,
    headers: HeaderMap,
    Query(query): Query<UsageQuery>,
) -> Result<Json<UsageCompletenessResponse>, Problem> {
    let principal = require_read_session(&state, &headers).await?;
    require_permission(&principal, Permission::ReadOperations)?;
    query.validate()?;
    let filters = query.filters()?;
    let summary = require_store(&state)?
        .usage_completeness(&filters)
        .await
        .map_err(map_operations)?;
    Ok(Json(summary.into()))
}
