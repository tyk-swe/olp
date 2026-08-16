use axum::{
    Json,
    extract::{Query, State},
    http::HeaderMap,
};
use chrono::{DateTime, Utc};
use olp_db::operations::audit::Record;
use olp_engine::domain::auth::Permission;
use serde::Serialize;
use utoipa::ToSchema;
use uuid::Uuid;

use super::helpers::{PageQuery, map_operations, page_limit, timestamp_cursor};
use crate::{
    bootstrap::mode_dependencies::ManagementState,
    management::{permissions::require_permission, sessions::require_read_session},
    public_http::problem::Problem,
};

#[derive(Debug, Serialize, ToSchema)]
pub(super) struct AuditEventResponse {
    #[schema(value_type = String, format = Uuid)]
    id: Uuid,
    #[schema(value_type = Option<String>, format = Uuid)]
    actor_user_id: Option<Uuid>,
    actor_email: Option<String>,
    action: String,
    resource_type: String,
    resource_id: Option<String>,
    outcome: String,
    occurred_at: DateTime<Utc>,
}

impl From<Record> for AuditEventResponse {
    fn from(record: Record) -> Self {
        Self {
            id: record.id,
            actor_user_id: record.actor_user_id,
            actor_email: record.actor_email,
            action: record.action,
            resource_type: record.resource_type,
            resource_id: record.resource_id,
            outcome: record.outcome,
            occurred_at: record.occurred_at,
        }
    }
}

#[derive(Debug, Serialize, ToSchema)]
pub(super) struct AuditListResponse {
    data: Vec<AuditEventResponse>,
    next_cursor: Option<String>,
}

#[utoipa::path(
    get,
    path = "/api/v1/audit",
    tag = "audit",
    params(PageQuery),
    responses((status = 200, description = "Audit page", body = AuditListResponse))
)]
pub(super) async fn list_audit_events(
    State(state): State<ManagementState>,
    headers: HeaderMap,
    Query(query): Query<PageQuery>,
) -> Result<Json<AuditListResponse>, Problem> {
    let principal = require_read_session(&state, &headers).await?;
    require_permission(&principal, Permission::ReadOperations)?;
    let cursor = timestamp_cursor(query.cursor.as_deref())?;
    let limit = page_limit(query.limit)?;
    let page = state
        .store()
        .audit_events(cursor.as_ref(), limit)
        .await
        .map_err(map_operations)?;
    Ok(Json(AuditListResponse {
        data: page.items.into_iter().map(Into::into).collect(),
        next_cursor: page.next_cursor,
    }))
}
