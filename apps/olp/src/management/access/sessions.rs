use axum::{
    Json,
    extract::{Path, Query, State},
    http::{HeaderMap, StatusCode},
    response::{IntoResponse, Response},
};
use olp_engine::domain::Permission;
use serde::{Deserialize, Serialize};
use utoipa::ToSchema;
use uuid::Uuid;

use crate::management::{
    cookies::expire_session_cookies,
    error_mapping::map_identity,
    pagination::{PageQuery, page},
    permissions::require_permission,
    sessions::{require_mutation_session, require_read_session},
};
use crate::{ManagementState, Problem};

#[derive(Debug, Deserialize)]
pub(in crate::management) struct SessionPageQuery {
    cursor: Option<String>,
    limit: Option<u16>,
    user_id: Option<Uuid>,
}

#[derive(Debug, Serialize, ToSchema)]
pub(in crate::management) struct SessionDetailResponse {
    #[schema(value_type = String, format = Uuid)]
    pub id: Uuid,
    #[schema(value_type = String, format = Uuid)]
    pub user_id: Uuid,
    pub current: bool,
    pub expires_at: chrono::DateTime<chrono::Utc>,
    pub last_seen_at: chrono::DateTime<chrono::Utc>,
    pub created_at: chrono::DateTime<chrono::Utc>,
}

#[derive(Debug, Serialize, ToSchema)]
pub(in crate::management) struct SessionListResponse {
    pub data: Vec<SessionDetailResponse>,
    pub next_cursor: Option<String>,
}

#[utoipa::path(
    get,
    path = "/api/v1/sessions",
    tag = "sessions",
    params(
        ("cursor" = Option<String>, Query, description = "Opaque cursor returned by the previous page"),
        ("limit" = Option<u16>, Query, description = "Page size from 1 to 100"),
        ("user_id" = Option<Uuid>, Query, description = "Owner-only user filter; defaults to the current user")
    ),
    responses(
        (status = 200, description = "Active and unexpired sessions", body = SessionListResponse),
        (status = 403, description = "Only owners can inspect another user's sessions", body = Problem)
    )
)]
pub(in crate::management) async fn list_sessions(
    State(state): State<ManagementState>,
    headers: HeaderMap,
    Query(query): Query<SessionPageQuery>,
) -> Result<Json<SessionListResponse>, Problem> {
    let principal = require_read_session(&state, &headers).await?;
    let user_id = query.user_id.unwrap_or(principal.user_id);
    if user_id != principal.user_id {
        require_permission(&principal, Permission::ManageSessions)?;
    }
    let (cursor, limit) = page(PageQuery {
        cursor: query.cursor,
        limit: query.limit,
    })?;
    let (sessions, next_cursor) = state
        .store()
        .list_sessions(user_id, cursor, limit)
        .await
        .map_err(map_identity)?;
    Ok(Json(SessionListResponse {
        data: sessions
            .into_iter()
            .map(|session| SessionDetailResponse {
                id: session.id,
                user_id: session.user_id,
                current: session.id == principal.session_id,
                expires_at: session.expires_at,
                last_seen_at: session.last_seen_at,
                created_at: session.created_at,
            })
            .collect(),
        next_cursor: next_cursor.map(|cursor| cursor.to_string()),
    }))
}

#[utoipa::path(
    delete,
    path = "/api/v1/sessions/{session_id}",
    tag = "sessions",
    params(("session_id" = Uuid, Path, description = "Session ID")),
    responses(
        (status = 204, description = "Session revoked"),
        (status = 403, description = "Only owners can revoke another user's session", body = Problem),
        (status = 404, description = "Session not found", body = Problem)
    )
)]
pub(in crate::management) async fn revoke_session(
    State(state): State<ManagementState>,
    Path(session_id): Path<Uuid>,
    headers: HeaderMap,
) -> Result<Response, Problem> {
    let principal = require_mutation_session(&state, &headers).await?;
    let can_manage_all = require_permission(&principal, Permission::ManageSessions).is_ok();
    state
        .store()
        .revoke_session(session_id, principal.user_id, can_manage_all)
        .await
        .map_err(map_identity)?;
    let mut response = StatusCode::NO_CONTENT.into_response();
    if session_id == principal.session_id {
        expire_session_cookies(&mut response);
    }
    Ok(response)
}
