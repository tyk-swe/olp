use std::{fmt, net::SocketAddr};

use axum::{
    Json,
    extract::{ConnectInfo, Extension, Path, Query, State, rejection::JsonRejection},
    http::{HeaderMap, HeaderValue, StatusCode, header},
    response::{IntoResponse, Response},
};
use chrono::{DateTime, Utc};
use olp_domain::Permission;
use olp_storage::{
    AcceptInvitation, IdempotencyResponse, InvitationRecord, NewInvitation, ReplayableIdempotency,
    SessionMaterial, UserRecord, hash_password, idempotency_fingerprint, verify_password,
};
use serde::{Deserialize, Serialize};
use tracing::error;
use utoipa::ToSchema;
use uuid::Uuid;
use zeroize::Zeroizing;

use super::{
    auth::{
        SessionResponse, UserResponse, public_auth_rate_limited, session_response,
        spawn_password_work,
    },
    common::*,
};
use crate::{ApiState, FieldErrors, Problem, public_auth_source_target_digests};

pub(super) const INVALID_INVITATION_RATE_LIMIT_TARGET: &str = "<invalid-invitation-token>";

#[utoipa::path(
    get,
    path = "/api/v1/profile",
    tag = "users",
    responses(
        (status = 200, description = "Current user profile", body = UserDetailResponse),
        (status = 401, description = "No active session", body = Problem)
    )
)]
pub(super) async fn profile(
    State(state): State<ApiState>,
    headers: HeaderMap,
) -> Result<Response, Problem> {
    let principal = require_read_session(&state, &headers).await?;
    let user = require_store(&state)?
        .user(principal.user_id)
        .await
        .map_err(map_identity)?
        .ok_or_else(user_not_found)?;
    let etag = user.etag;
    let mut response = Json(UserDetailResponse::from(user)).into_response();
    response.headers_mut().insert(
        header::ETAG,
        HeaderValue::from_str(&format!("\"{etag}\"")).map_err(|_| Problem::internal())?,
    );
    Ok(response)
}

#[derive(Debug, Deserialize, ToSchema)]
pub(super) struct UpdateProfileRequest {
    pub display_name: String,
}

#[utoipa::path(
    patch,
    path = "/api/v1/profile",
    tag = "users",
    params(("If-Match" = String, Header, description = "Current profile ETag")),
    request_body = UpdateProfileRequest,
    responses(
        (status = 200, description = "Profile updated", body = UserDetailResponse),
        (status = 412, description = "ETag mismatch", body = Problem),
        (status = 422, description = "Display name is invalid", body = Problem)
    )
)]
pub(super) async fn update_profile(
    State(state): State<ApiState>,
    headers: HeaderMap,
    payload: Result<Json<UpdateProfileRequest>, JsonRejection>,
) -> Result<Response, Problem> {
    let principal = require_mutation_session(&state, &headers).await?;
    let request = json_payload(payload)?;
    let user = require_store(&state)?
        .update_profile(
            principal.user_id,
            &request.display_name,
            if_match(&headers)?,
        )
        .await
        .map_err(map_identity)?;
    let etag = user.etag;
    let mut response = Json(UserDetailResponse::from(user)).into_response();
    response.headers_mut().insert(
        header::ETAG,
        HeaderValue::from_str(&format!("\"{etag}\"")).map_err(|_| Problem::internal())?,
    );
    Ok(response)
}

#[derive(Deserialize, ToSchema)]
pub(super) struct ChangePasswordRequest {
    #[schema(value_type = String, write_only)]
    current_password: WriteOnlySecret,
    #[schema(value_type = String, write_only)]
    new_password: WriteOnlySecret,
}

impl fmt::Debug for ChangePasswordRequest {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("ChangePasswordRequest")
            .field("current_password", &"[REDACTED]")
            .field("new_password", &"[REDACTED]")
            .finish()
    }
}

#[utoipa::path(
    post,
    path = "/api/v1/profile/password",
    tag = "users",
    params(("If-Match" = String, Header, description = "Current profile ETag")),
    request_body = ChangePasswordRequest,
    responses(
        (status = 200, description = "Local password changed; other sessions revoked", body = UserDetailResponse),
        (status = 403, description = "Current password is invalid or local auth is unavailable", body = Problem),
        (status = 429, description = "Password work is rate limited", body = Problem),
        (status = 412, description = "ETag mismatch", body = Problem),
        (status = 422, description = "New password is invalid", body = Problem)
    )
)]
pub(super) async fn change_password(
    State(state): State<ApiState>,
    headers: HeaderMap,
    payload: Result<Json<ChangePasswordRequest>, JsonRejection>,
) -> Result<Response, Problem> {
    let principal = require_mutation_session(&state, &headers).await?;
    let request = json_payload(payload)?;
    if !(12..=1_024).contains(&request.new_password.expose().chars().count()) {
        let mut errors = FieldErrors::new();
        errors.insert(
            "new_password".to_owned(),
            vec!["Use between 12 and 1,024 characters.".to_owned()],
        );
        return Err(Problem::validation(errors));
    }
    let local = require_store(&state)?
        .local_password_user(&principal.email)
        .await
        .map_err(map_persistence)?
        .ok_or_else(|| {
            Problem::forbidden(
                "local_password_unavailable",
                "This profile does not have a local password.",
            )
        })?;
    let current_password = Zeroizing::new(request.current_password.expose().to_owned());
    let new_password = Zeroizing::new(request.new_password.expose().to_owned());
    let current_hash = local.password_hash;
    let password_hash = spawn_password_work(move || {
        if !verify_password(&current_password, &current_hash) {
            return Ok(None);
        }
        hash_password(&new_password).map(Some)
    })?
    .await
    .map_err(|error| {
        error!(%error, "password change task failed");
        Problem::internal()
    })?
    .map_err(|error| {
        error!(%error, "new password hashing failed");
        Problem::internal()
    })?;
    let Some(password_hash) = password_hash else {
        return Err(Problem::forbidden(
            "current_password_invalid",
            "The current password is invalid.",
        ));
    };
    let user = require_store(&state)?
        .update_local_password(
            principal.user_id,
            &password_hash,
            if_match(&headers)?,
            principal.session_id,
        )
        .await
        .map_err(map_identity)?;
    let etag = user.etag;
    let mut response = Json(UserDetailResponse::from(user)).into_response();
    response.headers_mut().insert(
        header::ETAG,
        HeaderValue::from_str(&format!("\"{etag}\"")).map_err(|_| Problem::internal())?,
    );
    Ok(response)
}

#[derive(Deserialize, ToSchema)]
#[serde(deny_unknown_fields)]
pub(super) struct EnrollPasswordRequest {
    #[schema(value_type = String, write_only)]
    new_password: WriteOnlySecret,
}

impl fmt::Debug for EnrollPasswordRequest {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("EnrollPasswordRequest")
            .field("new_password", &"[REDACTED]")
            .finish()
    }
}

#[utoipa::path(
    post,
    path = "/api/v1/profile/password/enroll",
    tag = "users",
    params(("If-Match" = String, Header, description = "Current profile ETag")),
    request_body = EnrollPasswordRequest,
    responses(
        (status = 200, description = "First local password enrolled; other sessions revoked", body = UserDetailResponse),
        (status = 409, description = "A local password is already configured", body = Problem),
        (status = 429, description = "Password work is rate limited", body = Problem),
        (status = 412, description = "ETag mismatch", body = Problem),
        (status = 422, description = "New password is invalid", body = Problem)
    )
)]
pub(super) async fn enroll_password(
    State(state): State<ApiState>,
    headers: HeaderMap,
    payload: Result<Json<EnrollPasswordRequest>, JsonRejection>,
) -> Result<Response, Problem> {
    let principal = require_mutation_session(&state, &headers).await?;
    let request = json_payload(payload)?;
    if !(12..=1_024).contains(&request.new_password.expose().chars().count()) {
        let mut errors = FieldErrors::new();
        errors.insert(
            "new_password".to_owned(),
            vec!["Use between 12 and 1,024 characters.".to_owned()],
        );
        return Err(Problem::validation(errors));
    }
    let new_password = Zeroizing::new(request.new_password.expose().to_owned());
    let password_hash = spawn_password_work(move || hash_password(&new_password))?
        .await
        .map_err(|error| {
            error!(%error, "password enrollment task failed");
            Problem::internal()
        })?
        .map_err(|error| {
            error!(%error, "enrolled password hashing failed");
            Problem::internal()
        })?;
    let user = require_store(&state)?
        .enroll_local_password(
            principal.user_id,
            &password_hash,
            if_match(&headers)?,
            principal.session_id,
        )
        .await
        .map_err(map_identity)?;
    let etag = user.etag;
    let mut response = Json(UserDetailResponse::from(user)).into_response();
    response.headers_mut().insert(
        header::ETAG,
        HeaderValue::from_str(&format!("\"{etag}\"")).map_err(|_| Problem::internal())?,
    );
    Ok(response)
}

#[derive(Debug, Serialize, ToSchema)]
pub(super) struct UserDetailResponse {
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
pub(super) struct UserListResponse {
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
pub(super) async fn list_users(
    State(state): State<ApiState>,
    headers: HeaderMap,
    Query(query): Query<PageQuery>,
) -> Result<Json<UserListResponse>, Problem> {
    let principal = require_read_session(&state, &headers).await?;
    require_permission(&principal, Permission::ReadAccess)?;
    let (cursor, limit) = page_parameters(query)?;
    let (users, next_cursor) = require_store(&state)?
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
pub(super) async fn get_user(
    State(state): State<ApiState>,
    Path(user_id): Path<Uuid>,
    headers: HeaderMap,
) -> Result<Response, Problem> {
    let principal = require_read_session(&state, &headers).await?;
    require_permission(&principal, Permission::ReadAccess)?;
    let user = require_store(&state)?
        .user(user_id)
        .await
        .map_err(map_identity)?
        .ok_or_else(user_not_found)?;
    let etag = user.etag;
    let mut response = Json(UserDetailResponse::from(user)).into_response();
    response.headers_mut().insert(
        header::ETAG,
        HeaderValue::from_str(&format!("\"{etag}\"")).map_err(|_| Problem::internal())?,
    );
    Ok(response)
}

#[derive(Debug, Deserialize, ToSchema)]
pub(super) struct UpdateUserRoleRequest {
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
pub(super) async fn update_user_role(
    State(state): State<ApiState>,
    Path(user_id): Path<Uuid>,
    headers: HeaderMap,
    payload: Result<Json<UpdateUserRoleRequest>, JsonRejection>,
) -> Result<Response, Problem> {
    let principal = require_mutation_session(&state, &headers).await?;
    require_permission(&principal, Permission::ManageAccess)?;
    let request = json_payload(payload)?;
    if request.role.is_none() && request.active.is_none() {
        let mut errors = FieldErrors::new();
        errors.insert(
            "user".to_owned(),
            vec!["Provide a role or active status.".to_owned()],
        );
        return Err(Problem::validation(errors));
    }
    if user_id == principal.user_id && request.active == Some(false) {
        return Err(Problem::conflict(
            "cannot_deactivate_current_user",
            "Transfer access from the current session before deactivating this user.",
        ));
    }
    let role = request.role.as_deref().map(parse_user_role).transpose()?;
    let user = require_store(&state)?
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
    let mut response = Json(UserDetailResponse::from(user)).into_response();
    response.headers_mut().insert(
        header::ETAG,
        HeaderValue::from_str(&format!("\"{etag}\"")).map_err(|_| Problem::internal())?,
    );
    Ok(response)
}

#[derive(Debug, Serialize, ToSchema)]
pub(super) struct InvitationResponse {
    #[schema(value_type = String, format = Uuid)]
    pub id: Uuid,
    pub email: String,
    pub role: String,
    #[schema(value_type = String, format = Uuid)]
    pub invited_by: Uuid,
    pub status: String,
    pub expires_at: DateTime<Utc>,
    pub accepted_at: Option<DateTime<Utc>>,
    pub revoked_at: Option<DateTime<Utc>>,
    pub created_at: DateTime<Utc>,
}

impl From<InvitationRecord> for InvitationResponse {
    fn from(invitation: InvitationRecord) -> Self {
        let status = if invitation.accepted_at.is_some() {
            "accepted"
        } else if invitation.revoked_at.is_some() {
            "revoked"
        } else if invitation.expires_at <= Utc::now() {
            "expired"
        } else {
            "pending"
        };
        Self {
            id: invitation.id,
            email: invitation.email,
            role: invitation.role.to_string(),
            invited_by: invitation.invited_by,
            status: status.to_owned(),
            expires_at: invitation.expires_at,
            accepted_at: invitation.accepted_at,
            revoked_at: invitation.revoked_at,
            created_at: invitation.created_at,
        }
    }
}

#[derive(Debug, Serialize, ToSchema)]
pub(super) struct InvitationListResponse {
    pub data: Vec<InvitationResponse>,
    pub next_cursor: Option<String>,
}

#[derive(Debug, Deserialize, Serialize, ToSchema)]
pub(super) struct CreateInvitationRequest {
    pub email: String,
    pub role: String,
    /// Invitation lifetime in hours. Defaults to seven days and is capped at
    /// thirty days.
    pub expires_in_hours: Option<u16>,
}

#[derive(Debug, Serialize, ToSchema)]
pub(super) struct CreateInvitationResponse {
    pub invitation: InvitationResponse,
    /// Returned only by the invitation-creation response.
    #[schema(value_type = String, read_only)]
    token: WriteOnlySecret,
}

#[utoipa::path(
    get,
    path = "/api/v1/invitations",
    tag = "invitations",
    params(
        ("cursor" = Option<String>, Query, description = "Opaque cursor returned by the previous page"),
        ("limit" = Option<u16>, Query, description = "Page size from 1 to 100")
    ),
    responses((status = 200, description = "Invitation history", body = InvitationListResponse))
)]
pub(super) async fn list_invitations(
    State(state): State<ApiState>,
    headers: HeaderMap,
    Query(query): Query<PageQuery>,
) -> Result<Json<InvitationListResponse>, Problem> {
    let principal = require_read_session(&state, &headers).await?;
    require_permission(&principal, Permission::ReadAccess)?;
    let (cursor, limit) = page_parameters(query)?;
    let (invitations, next_cursor) = require_store(&state)?
        .list_invitations(cursor, limit)
        .await
        .map_err(map_identity)?;
    Ok(Json(InvitationListResponse {
        data: invitations.into_iter().map(Into::into).collect(),
        next_cursor: next_cursor.map(|cursor| cursor.to_string()),
    }))
}

#[utoipa::path(
    post,
    path = "/api/v1/invitations",
    tag = "invitations",
    params(("Idempotency-Key" = String, Header, description = "Unique invitation creation key")),
    request_body = CreateInvitationRequest,
    responses(
        (status = 201, description = "Invitation created; token is displayed once", body = CreateInvitationResponse),
        (status = 409, description = "Member, pending invitation, or idempotency conflict", body = Problem),
        (status = 422, description = "Invitation is invalid", body = Problem),
        (status = 503, description = "Master key or database unavailable", body = Problem)
    )
)]
pub(super) async fn create_invitation(
    State(state): State<ApiState>,
    headers: HeaderMap,
    payload: Result<Json<CreateInvitationRequest>, JsonRejection>,
) -> Result<Response, Problem> {
    let principal = require_mutation_session(&state, &headers).await?;
    require_permission(&principal, Permission::ManageAccess)?;
    let request = json_payload(payload)?;
    let request_fingerprint = idempotency_fingerprint(&request).map_err(map_persistence)?;
    let idempotency_key = require_idempotency_key(&headers)?.to_owned();
    let master_key = state
        .master_key
        .as_deref()
        .ok_or_else(|| Problem::service_unavailable("master_key_not_configured"))?;
    let role = parse_user_role(&request.role)?;
    let hours = request.expires_in_hours.unwrap_or(7 * 24);
    if !(1..=30 * 24).contains(&hours) {
        let mut errors = FieldErrors::new();
        errors.insert(
            "expires_in_hours".to_owned(),
            vec!["Use a value between 1 and 720 hours.".to_owned()],
        );
        return Err(Problem::validation(errors));
    }
    let expires_at = Utc::now()
        .checked_add_signed(chrono::Duration::hours(i64::from(hours)))
        .ok_or_else(Problem::internal)?;
    let created = require_store(&state)?
        .create_invitation(
            NewInvitation {
                email: request.email,
                role,
                expires_at,
                actor: principal.user_id,
                idempotency_key,
            },
            ReplayableIdempotency::new(request_fingerprint, master_key),
            |created| {
                IdempotencyResponse::json(
                    StatusCode::CREATED.as_u16(),
                    &CreateInvitationResponse {
                        invitation: created.invitation.clone().into(),
                        token: WriteOnlySecret(created.material.token().to_owned()),
                    },
                    None,
                )
            },
        )
        .await
        .map_err(map_identity)?;
    idempotency_http_response(created)
}

#[utoipa::path(
    delete,
    path = "/api/v1/invitations/{invitation_id}",
    tag = "invitations",
    params(
        ("invitation_id" = Uuid, Path, description = "Invitation ID"),
        ("Idempotency-Key" = String, Header, description = "Unique invitation revocation key")
    ),
    responses(
        (status = 200, description = "Invitation revoked", body = InvitationResponse),
        (status = 409, description = "Invitation is already accepted or revoked", body = Problem)
    )
)]
pub(super) async fn revoke_invitation(
    State(state): State<ApiState>,
    Path(invitation_id): Path<Uuid>,
    headers: HeaderMap,
) -> Result<Json<InvitationResponse>, Problem> {
    let principal = require_mutation_session(&state, &headers).await?;
    require_permission(&principal, Permission::ManageAccess)?;
    let invitation = require_store(&state)?
        .revoke_invitation(
            invitation_id,
            principal.user_id,
            require_idempotency_key(&headers)?,
        )
        .await
        .map_err(map_identity)?;
    Ok(Json(invitation.into()))
}

#[derive(Deserialize, ToSchema)]
pub(super) struct AcceptInvitationRequest {
    #[schema(value_type = String, write_only)]
    pub(super) token: WriteOnlySecret,
    pub display_name: String,
    #[schema(value_type = String, write_only)]
    pub(super) password: WriteOnlySecret,
}

impl fmt::Debug for AcceptInvitationRequest {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("AcceptInvitationRequest")
            .field("token", &"[REDACTED]")
            .field("display_name", &self.display_name)
            .field("password", &"[REDACTED]")
            .finish()
    }
}

#[utoipa::path(
    post,
    path = "/api/v1/invitations/accept",
    tag = "invitations",
    request_body = AcceptInvitationRequest,
    responses(
        (status = 201, description = "Invitation accepted and authenticated session created", body = SessionResponse),
        (status = 409, description = "Email is already a member", body = Problem),
        (status = 410, description = "Invitation is invalid, expired, revoked, or accepted", body = Problem),
        (status = 429, description = "Password work is rate limited", body = Problem),
        (status = 422, description = "Password or display name is invalid", body = Problem)
    )
)]
pub(super) async fn accept_invitation(
    State(state): State<ApiState>,
    connect_info: Option<Extension<ConnectInfo<SocketAddr>>>,
    headers: HeaderMap,
    payload: Result<Json<AcceptInvitationRequest>, JsonRejection>,
) -> Result<Response, Problem> {
    enforce_origin(&state, &headers)?;
    let request = json_payload(payload)?;
    let store = require_store(&state)?;
    let (source_digest, source_target_digest) = public_auth_source_target_digests(
        &state,
        &headers,
        connect_info.map(|Extension(ConnectInfo(peer))| peer),
        invitation_rate_limit_target(request.token.expose()),
    )?;
    if !store
        .admit_invitation_acceptance_attempt(source_digest, source_target_digest)
        .await
        .map_err(map_identity)?
    {
        return Err(public_auth_rate_limited());
    }
    validate_invitation_acceptance(&request)?;
    let password = Zeroizing::new(request.password.expose().to_owned());
    let password_hash = spawn_password_work(move || hash_password(&password))?
        .await
        .map_err(|error| {
            error!(%error, "invited-user password hashing task failed");
            Problem::internal()
        })?
        .map_err(|error| {
            error!(%error, "invited-user password hashing failed");
            Problem::internal()
        })?;
    let material = SessionMaterial::generate();
    let accepted = store
        .accept_invitation(
            AcceptInvitation {
                token: request.token.expose().to_owned(),
                display_name: request.display_name,
                password_hash,
            },
            &material,
            state.session_ttl,
        )
        .await
        .map_err(map_identity)?;
    session_response(
        StatusCode::CREATED,
        &material,
        UserResponse {
            id: accepted.user.id,
            email: accepted.user.email,
            display_name: accepted.user.display_name,
            role: accepted.user.role.to_string(),
        },
    )
}

/// Prevent an arbitrarily large malformed invitation token from becoming HMAC
/// input while still admitting it against the caller's source bucket.
pub(super) fn invitation_rate_limit_target(token: &str) -> &str {
    if token.len() == 43 {
        token
    } else {
        INVALID_INVITATION_RATE_LIMIT_TARGET
    }
}

fn validate_invitation_acceptance(request: &AcceptInvitationRequest) -> Result<(), Problem> {
    let mut errors = FieldErrors::new();
    if request.token.expose().len() != 43 {
        errors.insert(
            "token".to_owned(),
            vec!["The invitation token is invalid.".to_owned()],
        );
    }
    if !(12..=1_024).contains(&request.password.expose().chars().count()) {
        errors.insert(
            "password".to_owned(),
            vec!["Use between 12 and 1,024 characters.".to_owned()],
        );
    }
    if request.display_name.trim().is_empty() || request.display_name.chars().count() > 100 {
        errors.insert(
            "display_name".to_owned(),
            vec!["Use between 1 and 100 characters.".to_owned()],
        );
    }
    if errors.is_empty() {
        Ok(())
    } else {
        Err(Problem::validation(errors))
    }
}
