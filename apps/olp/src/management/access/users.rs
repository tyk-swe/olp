use crate::management::principal::{MutationPrincipal, ReadPrincipal};
use axum::{
    Json,
    extract::{Path, Query, State, rejection::JsonRejection},
    http::HeaderMap,
    response::Response,
};
use chrono::{DateTime, Utc};
use olp_db::identity::UserRecord;
use olp_engine::domain::auth::{Permission, Role};
use serde::{Deserialize, Serialize};
use utoipa::ToSchema;
use uuid::Uuid;

use crate::management::{
    error_mapping::{map_identity, user_not_found},
    json_payload::json_payload,
    pagination::{PageQuery, page},
    permissions::{parse_user_role, require_permission},
    preconditions::{if_match, with_etag},
    provenance::Provenance,
};
use crate::{bootstrap::mode_dependencies::ManagementState, public_http::problem::Problem};

#[derive(Clone, Debug, Serialize, ToSchema)]
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
    pub items: Vec<UserDetailResponse>,
    pub next_cursor: Option<String>,
}

#[utoipa::path(
    get,
    path = "/api/v1/users",
    tag = "users",
    params(
        PageQuery,
    ),
    responses(
        (status = 200, description = "Users in the installation", body = UserListResponse),
        (status = 400, description = "Malformed query parameters, or an invalid cursor or page size", body = Problem),
        (status = 401, description = "No active session", body = Problem),
        (status = 403, description = "Role cannot view access", body = Problem)
    )
)]
pub(in crate::management) async fn list_users(
    State(state): State<ManagementState>,
    Query(query): Query<PageQuery>,
    ReadPrincipal(principal): ReadPrincipal,
) -> Result<Json<UserListResponse>, Problem> {
    require_permission(&principal, Permission::ReadAccess)?;
    let (cursor, limit) = page(query)?;
    let (users, next_cursor) = state
        .store()
        .list_users(cursor, limit)
        .await
        .map_err(map_identity)?;
    let items = users.into_iter().map(Into::into).collect::<Vec<_>>();
    Ok(Json(UserListResponse {
        data: items.clone(),
        items,
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
    ReadPrincipal(principal): ReadPrincipal,
) -> Result<Response, Problem> {
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
        (status = 409, description = "Last active owner cannot be demoted or deactivated, and no user may change their own role or deactivate themselves", body = Problem),
        (status = 412, description = "ETag mismatch", body = Problem),
        (status = 422, description = "Role is invalid", body = Problem)
    )
)]
pub(in crate::management) async fn update_user_role(
    State(state): State<ManagementState>,
    Provenance(provenance): Provenance,
    Path(user_id): Path<Uuid>,
    headers: HeaderMap,
    MutationPrincipal(principal): MutationPrincipal,
    payload: Result<Json<UpdateUserRoleRequest>, JsonRejection>,
) -> Result<Response, Problem> {
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
    guard_self_role_change(user_id == principal.user_id, role, &principal.role)?;
    let user = state
        .store()
        .with_provenance(&provenance)
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

/// Storage revokes every session of the user being updated, so a caller who
/// changes their own role loses the session that is still writing this
/// response — and, when they demote themselves out of `ManageAccess`, the
/// ability to undo it. Self-deactivation is refused for the same reason.
fn guard_self_role_change(
    is_current_user: bool,
    requested_role: Option<Role>,
    current_role: &str,
) -> Result<(), Problem> {
    let Some(requested_role) = requested_role else {
        return Ok(());
    };
    if !is_current_user
        || current_role
            .parse::<Role>()
            .is_ok_and(|role| role == requested_role)
    {
        return Ok(());
    }
    Err(Problem::conflict(
        "cannot_change_current_user_role",
        "Ask another owner to change this role; changing your own revokes every session you hold.",
    ))
}

#[cfg(test)]
mod tests {
    use super::{Role, guard_self_role_change};

    #[test]
    fn changing_another_users_role_is_allowed() {
        assert!(guard_self_role_change(false, Some(Role::Viewer), "owner").is_ok());
    }

    #[test]
    fn an_owner_cannot_demote_themselves() {
        let problem = guard_self_role_change(true, Some(Role::Viewer), "owner").unwrap_err();
        assert_eq!(problem.status, 409);
        assert!(problem.detail.contains("Ask another owner"));
    }

    #[test]
    fn a_self_update_that_keeps_the_same_role_or_omits_it_passes() {
        assert!(guard_self_role_change(true, Some(Role::Owner), "owner").is_ok());
        assert!(guard_self_role_change(true, None, "owner").is_ok());
    }

    #[test]
    fn an_unparseable_stored_role_is_treated_as_a_change() {
        assert!(guard_self_role_change(true, Some(Role::Owner), "superuser").is_err());
    }
}
