use crate::access::policy::Permission;
use crate::access::principal::MutationPrincipal;
use crate::access::principal::ReadPrincipal;
use axum::Json;
use axum::extract::Path;
use axum::extract::Query;
use axum::extract::State;
use axum::http::StatusCode;
use axum::response::IntoResponse;
use axum::response::Response;
use serde::Deserialize;
use serde::Serialize;
use utoipa::ToSchema;
use uuid::Uuid;

use crate::access::cookies::expire_session_cookies;
use crate::access::permissions::require_permission;
use crate::http::control::error_mapping::map_identity;
use crate::http::control::pagination::PageQuery;
use crate::http::control::pagination::page;
use crate::http::control::provenance::Provenance;
use crate::http::control::state::ManagementState;
use crate::http::problem::Problem;

#[derive(Debug, Deserialize)]
pub(crate) struct SessionPageQuery {
    cursor: Option<String>,
    limit: Option<u16>,
    user_id: Option<Uuid>,
}

#[derive(Clone, Debug, Serialize, ToSchema)]
pub(crate) struct SessionDetailResponse {
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
pub(crate) struct SessionListResponse {
    pub items: Vec<SessionDetailResponse>,
    pub next_cursor: Option<String>,
}

#[utoipa::path(
    get,
    path = "/api/v3/sessions",
    tag = "sessions",
    params(
        ("cursor" = Option<String>, Query, description = "Opaque cursor returned by the previous page"),
        ("limit" = Option<u16>, Query, minimum = 1, maximum = 200, description = "Page size from 1 to 200; defaults to 50"),
        ("user_id" = Option<Uuid>, Query, description = "Owner-only user filter; defaults to the current user")
    ),
    responses(
        (status = 200, description = "Active and unexpired sessions", body = SessionListResponse),
        (status = 400, description = "Malformed query parameters, or an invalid cursor or page size", body = Problem, content_type = "application/problem+json"),
        (status = 403, description = "Only owners can inspect another user's sessions", body = Problem, content_type = "application/problem+json")
    ),
    security(("sessionCookie" = [])))]
pub(crate) async fn list_sessions(
    State(state): State<ManagementState>,
    Query(query): Query<SessionPageQuery>,
    ReadPrincipal(principal): ReadPrincipal,
) -> Result<Json<SessionListResponse>, Problem> {
    let user_id = query.user_id.unwrap_or(principal.user_id);
    if user_id != principal.user_id {
        require_permission(&principal, Permission::ManageSessions)?;
    }
    let (cursor, limit) = page(PageQuery {
        cursor: query.cursor,
        limit: query.limit,
    })?;
    let (sessions, next_cursor) = crate::access::identity::accounts::list_sessions(
        &state.request_boundary.pool,
        user_id,
        cursor,
        limit,
    )
    .await
    .map_err(map_identity)?;
    let items = sessions
        .into_iter()
        .map(|session| SessionDetailResponse {
            id: session.id,
            user_id: session.user_id,
            current: session.id == principal.session_id,
            expires_at: session.expires_at,
            last_seen_at: session.last_seen_at,
            created_at: session.created_at,
        })
        .collect::<Vec<_>>();
    Ok(Json(SessionListResponse {
        items,
        next_cursor: next_cursor.map(|cursor| cursor.to_string()),
    }))
}

#[utoipa::path(
    delete,
    path = "/api/v3/sessions/{session_id}",
    tag = "sessions",
    params(("session_id" = Uuid, Path, description = "Session ID")),
    responses(
        (status = 204, description = "Session revoked"),
        (status = 403, description = "Only owners can revoke another user's session", body = Problem, content_type = "application/problem+json"),
        (status = 404, description = "Session not found", body = Problem, content_type = "application/problem+json")
    ),
    security(("sessionCookie" = [], "csrfToken" = [])))]
pub(crate) async fn revoke_session(
    State(state): State<ManagementState>,
    Provenance(provenance): Provenance,
    Path(session_id): Path<Uuid>,
    MutationPrincipal(principal): MutationPrincipal,
) -> Result<Response, Problem> {
    let can_manage_all = require_permission(&principal, Permission::ManageSessions).is_ok();
    crate::access::identity::accounts::revoke_session(
        &state.request_boundary.pool,
        &provenance,
        session_id,
        principal.user_id,
        can_manage_all,
    )
    .await
    .map_err(map_identity)?;
    let mut response = StatusCode::NO_CONTENT.into_response();
    if session_id == principal.session_id {
        expire_session_cookies(&mut response);
    }
    Ok(response)
}
