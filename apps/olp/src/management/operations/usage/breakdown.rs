use crate::management::principal::ReadPrincipal;
use axum::{
    Json,
    extract::{Query, State},
};
use olp_db::{usage::Dimension, usage::breakdown::Item};
use serde::{Deserialize, Serialize};
use utoipa::ToSchema;

use super::{UsageQuery, UsageRangeCoverageResponse};
use crate::{
    management::{
        operations::helpers::map_operations, pagination::page_limit,
        permissions::require_permission, state::ManagementState,
    },
    public_http::problem::Problem,
};
use olp_engine::domain::auth::Permission;

#[derive(Debug, Deserialize)]
pub(in crate::management::operations) struct UsageBreakdownQuery {
    #[serde(flatten)]
    usage: UsageQuery,
    dimension: String,
    limit: Option<u16>,
}

#[derive(Clone, Debug, Serialize, ToSchema)]
pub(in crate::management::operations) struct UsageBreakdownItem {
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

impl From<Item> for UsageBreakdownItem {
    fn from(item: Item) -> Self {
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
pub(in crate::management::operations) struct UsageBreakdownResponse {
    data: Vec<UsageBreakdownItem>,
    items: Vec<UsageBreakdownItem>,
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
    responses(
        (status = 200, description = "Usage breakdown", body = UsageBreakdownResponse),
        (status = 400, description = "Malformed query parameters, or an invalid cursor or page size", body = Problem)
    )
)]
pub(in crate::management::operations) async fn usage_breakdown(
    State(state): State<ManagementState>,
    Query(query): Query<UsageBreakdownQuery>,
    ReadPrincipal(principal): ReadPrincipal,
) -> Result<Json<UsageBreakdownResponse>, Problem> {
    require_permission(&principal, Permission::ReadOperations)?;
    query.usage.validate()?;
    let dimension = match query.dimension.as_str() {
        "route" => Dimension::Route,
        "provider" => Dimension::Provider,
        "model" => Dimension::Model,
        "api_key" => Dimension::ApiKey,
        "operation" => Dimension::Operation,
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
    let items = report.items.into_iter().map(Into::into).collect::<Vec<_>>();
    Ok(Json(UsageBreakdownResponse {
        data: items.clone(),
        items,
        coverage: report.coverage.into(),
    }))
}
