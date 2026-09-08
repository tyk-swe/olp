use crate::access::principal::ReadPrincipal;
use crate::usage::reports::Granularity;
use crate::usage::reports::series::Point;
use axum::Json;
use axum::extract::Query;
use axum::extract::State;
use chrono::DateTime;
use chrono::Utc;
use serde::Deserialize;
use serde::Serialize;
use utoipa::ToSchema;

use crate::access::permissions::require_permission;
use crate::access::policy::Permission;
use crate::http::control::operations::helpers::map_operations;
use crate::http::control::state::ManagementState;
use crate::http::problem::Problem;
use crate::usage::http::UsageQuery;
use crate::usage::http::UsageRangeCoverageResponse;

#[derive(Debug, Deserialize)]
pub(crate) struct UsageSeriesQuery {
    #[serde(flatten)]
    usage: UsageQuery,
    granularity: Option<String>,
}

#[derive(Clone, Debug, Serialize, ToSchema)]
pub(crate) struct UsagePointResponse {
    bucket: DateTime<Utc>,
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

impl From<Point> for UsagePointResponse {
    fn from(point: Point) -> Self {
        Self {
            bucket: point.bucket,
            request_count: point.request_count,
            input_tokens: point.input_tokens,
            output_tokens: point.output_tokens,
            cached_input_tokens: point.cached_input_tokens,
            media_units: point.media_units,
            estimated_cost: point.estimated_cost,
            currency: point.currency,
            unpriced_count: point.unpriced_count,
            incomplete_count: point.incomplete_count,
        }
    }
}

#[derive(Debug, Serialize, ToSchema)]
pub(crate) struct UsageTimeSeriesResponse {
    items: Vec<UsagePointResponse>,
    coverage: UsageRangeCoverageResponse,
}

#[utoipa::path(
    get,
    path = "/api/v3/usage/time-series",
    tag = "usage",
    params(
        UsageQuery,
        ("granularity" = Option<String>, Query, description = "Bucket size: hour or day")
    ),
    responses((status = 200, description = "Usage time series", body = UsageTimeSeriesResponse))
, security(("sessionCookie" = [])))]
pub(crate) async fn usage_time_series(
    State(state): State<ManagementState>,
    Query(query): Query<UsageSeriesQuery>,
    ReadPrincipal(principal): ReadPrincipal,
) -> Result<Json<UsageTimeSeriesResponse>, Problem> {
    require_permission(&principal, Permission::ReadOperations)?;
    query.usage.validate()?;
    let granularity = match query.granularity.as_deref().unwrap_or("hour") {
        "hour" => Granularity::Hour,
        "day" => Granularity::Day,
        _ => {
            return Err(Problem::bad_request(
                "invalid_granularity",
                "Granularity must be hour or day.",
            ));
        }
    };
    let filters = query.usage.filters()?;
    let series = crate::usage::reports::series::usage_series(
        &state.request_boundary.pool,
        &filters,
        granularity,
    )
    .await
    .map_err(map_operations)?;
    let items = series
        .points
        .into_iter()
        .map(Into::into)
        .collect::<Vec<_>>();
    Ok(Json(UsageTimeSeriesResponse {
        items,
        coverage: series.coverage.into(),
    }))
}
