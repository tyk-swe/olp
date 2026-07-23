use axum::{
    Json,
    extract::{Query, State},
    http::HeaderMap,
};
use chrono::{DateTime, Utc};
use olp_storage::{UsageGranularity, UsagePoint};
use serde::{Deserialize, Serialize};
use utoipa::ToSchema;

use super::{UsageQuery, UsageRangeCoverageResponse};
use crate::{
    ManagementState, Problem,
    management_api::{Permission, require_permission, require_read_session},
    operations::helpers::map_operations,
};

#[derive(Debug, Deserialize)]
pub(in crate::operations) struct UsageSeriesQuery {
    #[serde(flatten)]
    usage: UsageQuery,
    granularity: Option<String>,
}

#[derive(Debug, Serialize, ToSchema)]
pub(in crate::operations) struct UsagePointResponse {
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

impl From<UsagePoint> for UsagePointResponse {
    fn from(point: UsagePoint) -> Self {
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
pub(in crate::operations) struct UsageTimeSeriesResponse {
    data: Vec<UsagePointResponse>,
    coverage: UsageRangeCoverageResponse,
}

#[utoipa::path(
    get,
    path = "/api/v1/usage/time-series",
    tag = "usage",
    params(
        UsageQuery,
        ("granularity" = Option<String>, Query, description = "Bucket size: hour or day")
    ),
    responses((status = 200, description = "Usage time series", body = UsageTimeSeriesResponse))
)]
pub(in crate::operations) async fn usage_time_series(
    State(state): State<ManagementState>,
    headers: HeaderMap,
    Query(query): Query<UsageSeriesQuery>,
) -> Result<Json<UsageTimeSeriesResponse>, Problem> {
    let principal = require_read_session(&state, &headers).await?;
    require_permission(&principal, Permission::ReadOperations)?;
    query.usage.validate()?;
    let granularity = match query.granularity.as_deref().unwrap_or("hour") {
        "hour" => UsageGranularity::Hour,
        "day" => UsageGranularity::Day,
        _ => {
            return Err(Problem::bad_request(
                "invalid_granularity",
                "Granularity must be hour or day.",
            ));
        }
    };
    let filters = query.usage.filters()?;
    let series = state
        .store()
        .usage_series(&filters, granularity)
        .await
        .map_err(map_operations)?;
    Ok(Json(UsageTimeSeriesResponse {
        data: series.points.into_iter().map(Into::into).collect(),
        coverage: series.coverage.into(),
    }))
}
