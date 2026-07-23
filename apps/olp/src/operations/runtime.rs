use axum::{
    Json,
    extract::{Query, State},
    http::HeaderMap,
};
use chrono::{DateTime, Utc};
use olp_storage::RuntimeGenerationRecord;
use serde::Serialize;
use utoipa::ToSchema;
use uuid::Uuid;

use super::helpers::{PageQuery, map_operations, page_limit};
use crate::{
    ManagementState, Problem,
    management_api::{Permission, require_permission, require_read_session},
};

#[derive(Debug, Serialize, ToSchema)]
pub(super) struct RuntimeGenerationItem {
    #[schema(value_type = String, format = Uuid)]
    id: Uuid,
    sequence: u64,
    sha256: String,
    #[schema(value_type = String, format = Uuid)]
    created_by: Uuid,
    created_by_email: String,
    created_at: DateTime<Utc>,
}

impl From<RuntimeGenerationRecord> for RuntimeGenerationItem {
    fn from(record: RuntimeGenerationRecord) -> Self {
        Self {
            id: record.id,
            sequence: record.sequence,
            sha256: record.sha256_hex,
            created_by: record.created_by,
            created_by_email: record.created_by_email,
            created_at: record.created_at,
        }
    }
}

#[derive(Debug, Serialize, ToSchema)]
pub(super) struct RuntimeGenerationListResponse {
    data: Vec<RuntimeGenerationItem>,
    next_cursor: Option<String>,
}

#[utoipa::path(
    get,
    path = "/api/v1/runtime-generations",
    tag = "runtime",
    params(PageQuery),
    responses((status = 200, description = "Runtime generations", body = RuntimeGenerationListResponse))
)]
pub(super) async fn list_runtime_generations(
    State(state): State<ManagementState>,
    headers: HeaderMap,
    Query(query): Query<PageQuery>,
) -> Result<Json<RuntimeGenerationListResponse>, Problem> {
    let principal = require_read_session(&state, &headers).await?;
    require_permission(&principal, Permission::ReadOperations)?;
    let before = query
        .cursor
        .as_deref()
        .map(str::parse::<u64>)
        .transpose()
        .map_err(|_| Problem::bad_request("invalid_cursor", "The cursor is invalid."))?;
    let limit = page_limit(query.limit)?;
    let page = state
        .store()
        .runtime_generations(before, limit)
        .await
        .map_err(map_operations)?;
    Ok(Json(RuntimeGenerationListResponse {
        data: page.items.into_iter().map(Into::into).collect(),
        next_cursor: page.next_cursor,
    }))
}
