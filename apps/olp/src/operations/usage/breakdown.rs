use axum::{
    Json,
    extract::{Query, State},
    http::HeaderMap,
};
use olp_storage::{UsageBreakdown, UsageDimension};
use serde::{Deserialize, Serialize};
use utoipa::ToSchema;

use super::{UsageQuery, UsageRangeCoverageResponse};
use crate::{
    ManagementState, Problem,
    management_api::{Permission, require_permission, require_read_session},
    operations::helpers::{map_operations, page_limit},
};

#[derive(Debug, Deserialize)]
pub(in crate::operations) struct UsageBreakdownQuery {
    #[serde(flatten)]
    usage: UsageQuery,
    dimension: String,
    limit: Option<u16>,
}

#[derive(Debug, Serialize, ToSchema)]
pub(in crate::operations) struct UsageBreakdownItem {
    dimension: String,
    request_count: u64,
    input_tokens: String,
    output_tokens: String,
    cached_input_tokens: String,
    media_units: String,
    estimated_cost: Option<String>,
    currency: Option<String>,
    unpriced_count: u64,
    incomplete_count: u64,
}

impl From<UsageBreakdown> for UsageBreakdownItem {
    fn from(item: UsageBreakdown) -> Self {
        Self {
            dimension: item.dimension,
            request_count: item.request_count,
            input_tokens: item.input_tokens,
            output_tokens: item.output_tokens,
            cached_input_tokens: item.cached_input_tokens,
            media_units: item.media_units,
            estimated_cost: item.estimated_cost,
            currency: item.currency,
            unpriced_count: item.unpriced_count,
            incomplete_count: item.incomplete_count,
        }
    }
}

#[derive(Debug, Serialize, ToSchema)]
pub(in crate::operations) struct UsageBreakdownResponse {
    data: Vec<UsageBreakdownItem>,
    coverage: UsageRangeCoverageResponse,
}

#[utoipa::path(
    get,
    path = "/api/v1/usage/breakdown",
    tag = "usage",
    params(
        UsageQuery,
        ("dimension" = String, Query, description = "Break down by route, provider, model, api_key, or operation"),
        ("limit" = Option<u16>, Query, minimum = 1, maximum = 200, description = "Maximum number of breakdown rows")
    ),
    responses((status = 200, description = "Usage breakdown", body = UsageBreakdownResponse))
)]
pub(in crate::operations) async fn usage_breakdown(
    State(state): State<ManagementState>,
    headers: HeaderMap,
    Query(query): Query<UsageBreakdownQuery>,
) -> Result<Json<UsageBreakdownResponse>, Problem> {
    let principal = require_read_session(&state, &headers).await?;
    require_permission(&principal, Permission::ReadOperations)?;
    query.usage.validate()?;
    let dimension = match query.dimension.as_str() {
        "route" => UsageDimension::Route,
        "provider" => UsageDimension::Provider,
        "model" => UsageDimension::Model,
        "api_key" => UsageDimension::ApiKey,
        "operation" => UsageDimension::Operation,
        _ => {
            return Err(Problem::bad_request(
                "invalid_dimension",
                "Dimension must be route, provider, model, api_key, or operation.",
            ));
        }
    };
    let filters = query.usage.filters()?;
    let report = state
        .store()
        .usage_breakdown(&filters, dimension, page_limit(query.limit)?)
        .await
        .map_err(map_operations)?;
    Ok(Json(UsageBreakdownResponse {
        data: report.items.into_iter().map(Into::into).collect(),
        coverage: report.coverage.into(),
    }))
}
