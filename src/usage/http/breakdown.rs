use crate::access::principal::ReadPrincipal;
use crate::usage::reports::Dimension;
use crate::usage::reports::breakdown::Item;
use axum::Json;
use axum::extract::Query;
use axum::extract::State;
use serde::Deserialize;
use serde::Serialize;
use utoipa::ToSchema;

use crate::access::permissions::require_permission;
use crate::access::policy::Permission;
use crate::http::control::operations::helpers::map_operations;
use crate::http::control::pagination::page_limit;
use crate::http::control::state::ManagementState;
use crate::http::problem::Problem;
use crate::usage::http::UsageQuery;
use crate::usage::http::UsageRangeCoverageResponse;

#[derive(Debug, Deserialize)]
pub(crate) struct UsageBreakdownQuery {
    #[serde(flatten)]
    usage: UsageQuery,
    dimension: String,
    limit: Option<u16>,
}

#[derive(Clone, Debug, Serialize, ToSchema)]
pub(crate) struct UsageBreakdownItem {
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
pub(crate) struct UsageBreakdownResponse {
    items: Vec<UsageBreakdownItem>,
    coverage: UsageRangeCoverageResponse,
}

#[utoipa::path(
    get,
    path = "/api/v3/usage/breakdown",
    tag = "usage",
    params(
        UsageQuery,
        ("dimension" = String, Query, description = "Break down by route, provider, model, api_key, or operation"),
        ("limit" = Option<u16>, Query, minimum = 1, maximum = 200, description = "Maximum number of breakdown rows")
    ),
    responses(
        (status = 200, description = "Usage breakdown", body = UsageBreakdownResponse),
        (status = 400, description = "Malformed query parameters, or an invalid cursor or page size", body = Problem, content_type = "application/problem+json")
    ),
    security(("sessionCookie" = [])))]
pub(crate) async fn usage_breakdown(
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
    let report = crate::usage::reports::breakdown::usage_breakdown(
        &state.request_boundary.pool,
        &filters,
        dimension,
        page_limit(query.limit)?,
    )
    .await
    .map_err(map_operations)?;
    let items = report.items.into_iter().map(Into::into).collect::<Vec<_>>();
    Ok(Json(UsageBreakdownResponse {
        items,
        coverage: report.coverage.into(),
    }))
}
