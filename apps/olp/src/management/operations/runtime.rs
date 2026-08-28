use axum::{
    Json,
    extract::{Query, State},
};
use chrono::{DateTime, Utc};
use olp_db::operations::runtime::GenerationRecord;
use olp_engine::domain::auth::Permission;
use serde::Serialize;
use utoipa::ToSchema;
use uuid::Uuid;

use super::helpers::{map_operations, page_limit};
use crate::{
    bootstrap::mode_dependencies::ManagementState,
    management::{
        pagination::PageQuery, permissions::require_permission, principal::ReadPrincipal,
    },
    public_http::problem::Problem,
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

impl From<GenerationRecord> for RuntimeGenerationItem {
    fn from(record: GenerationRecord) -> Self {
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
    items: Vec<RuntimeGenerationItem>,
    next_cursor: Option<String>,
}

#[utoipa::path(
    get,
    path = "/api/v1/runtime-generations",
    tag = "runtime",
    params(PageQuery),
    responses(
        (status = 200, description = "Runtime generations", body = RuntimeGenerationListResponse),
        (status = 400, description = "Malformed query parameters, or an invalid cursor or page size", body = Problem)
    )
)]
pub(super) async fn list_runtime_generations(
    State(state): State<ManagementState>,
    Query(query): Query<PageQuery>,
    ReadPrincipal(principal): ReadPrincipal,
) -> Result<Json<RuntimeGenerationListResponse>, Problem> {
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
        items: page.items.into_iter().map(Into::into).collect(),
        next_cursor: page.next_cursor,
    }))
}
