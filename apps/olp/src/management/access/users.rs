use axum::{
    Json,
    extract::{Path, Query, State, rejection::JsonRejection},
    http::HeaderMap,
    response::Response,
};
use chrono::{DateTime, Utc};
use olp_domain::Permission;
use olp_storage::identity::UserRecord;
use serde::{Deserialize, Serialize};
use utoipa::ToSchema;
use uuid::Uuid;

use crate::management::{
    error_mapping::{map_identity, user_not_found},
    json_payload::json_payload,
    pagination::{PageQuery, page},
    permissions::{parse_user_role, require_permission},
    preconditions::{if_match, with_etag},
    sessions::{require_mutation_session, require_read_session},
};
use crate::{ManagementState, Problem};

#[derive(Debug, Serialize, ToSchema)]
pub(in crate::management) struct UserDetailResponse {
    #[schema(value_type = String, format = Uuid)]
    pub id: Uuid,
    pub email: String,
    pub display_name: String,
    pub role: String,
    pub active: bool,
    #[schema(value_type = String, format = Uuid)]
    pub etag: Uuid,
    pub created_at: DateTime<Utc>,
    pub updated_at: DateTime<Utc>,
}

impl From<UserRecord> for UserDetailResponse {
    fn from(user: UserRecord) -> Self {
        Self {
            id: user.id,
            email: user.email,
            display_name: user.display_name,
            role: user.role.to_string(),
            active: user.active,
            etag: user.etag,
            created_at: user.created_at,
            updated_at: user.updated_at,
        }
    }
}

#[derive(Debug, Serialize, ToSchema)]
pub(in crate::management) struct UserListResponse {
    pub data: Vec<UserDetailResponse>,
    pub next_cursor: Option<String>,
}

#[utoipa::path(
    get,
    path = "/api/v1/users",
    tag = "users",
    params(
        ("cursor" = Option<String>, Query, description = "Opaque cursor returned by the previous page"),
        ("limit" = Option<u16>, Query, description = "Page size from 1 to 100")
    ),
    responses(
        (status = 200, description = "Users in the installation", body = UserListResponse),
        (status = 401, description = "No active session", body = Problem),
        (status = 403, description = "Role cannot view access", body = Problem)
    )
)]
pub(in crate::management) async fn list_users(
    State(state): State<ManagementState>,
    headers: HeaderMap,
    Query(query): Query<PageQuery>,
) -> Result<Json<UserListResponse>, Problem> {
    let principal = require_read_session(&state, &headers).await?;
    require_permission(&principal, Permission::ReadAccess)?;
    let (cursor, limit) = page(query)?;
    let (users, next_cursor) = state
        .store()
        .list_users(cursor, limit)
        .await
        .map_err(map_identity)?;
    Ok(Json(UserListResponse {
        data: users.into_iter().map(Into::into).collect(),
        next_cursor: next_cursor.map(|cursor| cursor.to_string()),
    }))
}

#[utoipa::path(
    get,
    path = "/api/v1/users/{user_id}",
    tag = "users",
    params(("user_id" = Uuid, Path, description = "User ID")),
    responses(
        (status = 200, description = "User", body = UserDetailResponse),
        (status = 404, description = "User not found", body = Problem)
    )
)]
pub(in crate::management) async fn get_user(
    State(state): State<ManagementState>,
    Path(user_id): Path<Uuid>,
    headers: HeaderMap,
) -> Result<Response, Problem> {
    let principal = require_read_session(&state, &headers).await?;
    require_permission(&principal, Permission::ReadAccess)?;
    let user = state
        .store()
        .user(user_id)
        .await
        .map_err(map_identity)?
        .ok_or_else(user_not_found)?;
    let etag = user.etag;
    with_etag(Json(UserDetailResponse::from(user)), etag)
}

#[derive(Debug, Deserialize, ToSchema)]
pub(in crate::management) struct UpdateUserRoleRequest {
    pub role: Option<String>,
    pub active: Option<bool>,
}

#[utoipa::path(
    patch,
    path = "/api/v1/users/{user_id}",
    tag = "users",
    params(
        ("user_id" = Uuid, Path, description = "User ID"),
        ("If-Match" = String, Header, description = "Current user ETag")
    ),
    request_body = UpdateUserRoleRequest,
    responses(
        (status = 200, description = "Role or active status updated; existing sessions were revoked", body = UserDetailResponse),
        (status = 409, description = "Last active owner cannot be demoted or deactivated", body = Problem),
        (status = 412, description = "ETag mismatch", body = Problem),
        (status = 422, description = "Role is invalid", body = Problem)
    )
)]
pub(in crate::management) async fn update_user_role(
    State(state): State<ManagementState>,
    Path(user_id): Path<Uuid>,
    headers: HeaderMap,
    payload: Result<Json<UpdateUserRoleRequest>, JsonRejection>,
) -> Result<Response, Problem> {
    let principal = require_mutation_session(&state, &headers).await?;
    require_permission(&principal, Permission::ManageAccess)?;
    let request = json_payload(payload)?;
    if request.role.is_none() && request.active.is_none() {
        return Err(Problem::field_validation(
            "user",
            "Provide a role or active status.",
        ));
    }
    if user_id == principal.user_id && request.active == Some(false) {
        return Err(Problem::conflict(
            "cannot_deactivate_current_user",
            "Transfer access from the current session before deactivating this user.",
        ));
    }
    let role = request.role.as_deref().map(parse_user_role).transpose()?;
    let user = state
        .store()
        .update_user_access(
            user_id,
            role,
            request.active,
            if_match(&headers)?,
            principal.user_id,
        )
        .await
        .map_err(map_identity)?;
    let etag = user.etag;
    with_etag(Json(UserDetailResponse::from(user)), etag)
}
