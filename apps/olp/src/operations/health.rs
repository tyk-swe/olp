use axum::{
    Json,
    extract::{Query, State},
    http::HeaderMap,
};
use chrono::{DateTime, Utc};
use olp_storage::ProviderHealthRecord;
use serde::{Deserialize, Serialize};
use utoipa::{IntoParams, ToSchema};
use uuid::Uuid;

use super::helpers::{map_operations, page_limit};
use crate::{
    FieldErrors, HealthResponse, ManagementState, Problem,
    management_api::{Permission, require_permission, require_read_session},
};

#[utoipa::path(
    get,
    path = "/api/v1/health/ready",
    tag = "health",
    responses(
        (status = 200, description = "Cached readiness snapshot for an authenticated management session", body = HealthResponse),
        (status = 401, description = "Authentication required", body = Problem),
        (status = 403, description = "Insufficient role", body = Problem),
        (status = 503, description = "Readiness snapshot is stale or unavailable", body = Problem)
    )
)]
pub(super) async fn management_readiness(
    State(state): State<ManagementState>,
    headers: HeaderMap,
) -> Result<Json<HealthResponse>, Problem> {
    let principal = require_read_session(&state, &headers).await?;
    require_permission(&principal, Permission::ReadOperations)?;
    Ok(Json(state.cached_readiness()?))
}

#[derive(Debug, Deserialize, IntoParams)]
#[into_params(parameter_in = Query)]
pub(super) struct ProviderHealthQuery {
    #[param(minimum = 1, maximum = 1440, default = 15)]
    window_minutes: Option<u16>,
    #[param(value_type = Option<String>, format = Uuid)]
    cursor: Option<String>,
    #[param(minimum = 1, maximum = 200, default = 50)]
    limit: Option<u16>,
}

#[derive(Debug, Serialize, ToSchema)]
pub(super) struct ProviderHealthItem {
    #[schema(value_type = String, format = Uuid)]
    provider_id: Uuid,
    provider_name: String,
    provider_kind: olp_domain::ProviderKind,
    provider_state: String,
    status: String,
    last_probe_at: Option<DateTime<Utc>>,
    last_probe_status: Option<String>,
    last_probe_detail: Option<String>,
    last_attempt_at: Option<DateTime<Utc>>,
    attempt_count: u64,
    success_count: u64,
    rate_limit_count: u64,
    server_error_count: u64,
    transport_error_count: u64,
    average_latency_ms: Option<f64>,
}

impl From<ProviderHealthRecord> for ProviderHealthItem {
    fn from(record: ProviderHealthRecord) -> Self {
        Self {
            provider_id: record.provider_id,
            provider_name: record.provider_name,
            provider_kind: record.provider_kind,
            provider_state: record.provider_state.to_string(),
            status: record.status,
            last_probe_at: record.last_probe_at,
            last_probe_status: record.last_probe_status,
            last_probe_detail: record.last_probe_detail,
            last_attempt_at: record.last_attempt_at,
            attempt_count: record.attempt_count,
            success_count: record.success_count,
            rate_limit_count: record.rate_limit_count,
            server_error_count: record.server_error_count,
            transport_error_count: record.transport_error_count,
            average_latency_ms: record.average_latency_ms,
        }
    }
}

#[derive(Debug, Serialize, ToSchema)]
pub(super) struct ProviderHealthResponse {
    window_minutes: u16,
    data: Vec<ProviderHealthItem>,
    next_cursor: Option<String>,
}

#[utoipa::path(
    get,
    path = "/api/v1/provider-health",
    tag = "health",
    params(ProviderHealthQuery),
    responses(
        (status = 200, description = "Probe and rolling-attempt provider health", body = ProviderHealthResponse),
        (status = 401, description = "Authentication required", body = Problem),
        (status = 403, description = "Insufficient role", body = Problem)
    )
)]
pub(super) async fn provider_health(
    State(state): State<ManagementState>,
    headers: HeaderMap,
    Query(query): Query<ProviderHealthQuery>,
) -> Result<Json<ProviderHealthResponse>, Problem> {
    let principal = require_read_session(&state, &headers).await?;
    require_permission(&principal, Permission::ReadOperations)?;
    let window_minutes = query.window_minutes.unwrap_or(15);
    if !(1..=1_440).contains(&window_minutes) {
        let mut fields = FieldErrors::new();
        fields.insert(
            "window_minutes".to_owned(),
            vec!["Window must be between 1 and 1440 minutes.".to_owned()],
        );
        return Err(Problem::validation(fields));
    }
    let cursor = query
        .cursor
        .as_deref()
        .map(Uuid::parse_str)
        .transpose()
        .map_err(|_| Problem::bad_request("invalid_cursor", "The cursor is invalid."))?;
    let page = state
        .store()
        .provider_health(window_minutes, cursor, page_limit(query.limit)?)
        .await
        .map_err(map_operations)?;
    Ok(Json(ProviderHealthResponse {
        window_minutes,
        data: page.items.into_iter().map(Into::into).collect(),
        next_cursor: page.next_cursor,
    }))
}
