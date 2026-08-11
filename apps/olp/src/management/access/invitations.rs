use std::{fmt, net::SocketAddr};

use axum::{
    Json,
    extract::{ConnectInfo, Extension, Path, Query, State, rejection::JsonRejection},
    http::{HeaderMap, StatusCode},
    response::Response,
};
use chrono::{DateTime, Utc};
use olp_db::{
    idempotency::IdempotencyResponse,
    idempotency::ReplayableIdempotency,
    idempotency::idempotency_fingerprint,
    identity::{AcceptInvitation, InvitationRecord, NewInvitation},
    security::{SessionMaterial, hash_password},
};
use olp_engine::domain::Permission;
use serde::{Deserialize, Serialize};
use tracing::error;
use utoipa::ToSchema;
use uuid::Uuid;
use zeroize::Zeroizing;

use crate::management::{
    auth::{
        SessionResponse, UserResponse, public_auth_rate_limited, session_response,
        spawn_password_work,
    },
    cookies::validate_session_cookie_ttl,
    error_mapping::{map_identity, map_persistence},
    idempotency::{idempotency_http_response, require_idempotency_key},
    json_payload::json_payload,
    pagination::{PageQuery, page},
    permissions::{parse_user_role, require_permission},
    secrets::WriteOnlySecret,
    sessions::{enforce_origin, require_mutation_session, require_read_session},
};
use crate::{FieldErrors, ManagementState, Problem, public_auth_source_target_digests};

pub(in crate::management) const INVALID_INVITATION_RATE_LIMIT_TARGET: &str =
    "<invalid-invitation-token>";

#[derive(Debug, Serialize, ToSchema)]
pub(in crate::management) struct InvitationResponse {
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
pub(in crate::management) struct InvitationListResponse {
    pub data: Vec<InvitationResponse>,
    pub next_cursor: Option<String>,
}

#[derive(Debug, Deserialize, Serialize, ToSchema)]
pub(in crate::management) struct CreateInvitationRequest {
    pub email: String,
    pub role: String,
    /// Invitation lifetime in hours. Defaults to seven days and is capped at
    /// thirty days.
    pub expires_in_hours: Option<u16>,
}

#[derive(Debug, Serialize, ToSchema)]
pub(in crate::management) struct CreateInvitationResponse {
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
pub(in crate::management) async fn list_invitations(
    State(state): State<ManagementState>,
    headers: HeaderMap,
    Query(query): Query<PageQuery>,
) -> Result<Json<InvitationListResponse>, Problem> {
    let principal = require_read_session(&state, &headers).await?;
    require_permission(&principal, Permission::ReadAccess)?;
    let (cursor, limit) = page(query)?;
    let (invitations, next_cursor) = state
        .store()
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
pub(in crate::management) async fn create_invitation(
    State(state): State<ManagementState>,
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
        return Err(Problem::field_validation(
            "expires_in_hours",
            "Use a value between 1 and 720 hours.",
        ));
    }
    let expires_at = Utc::now()
        .checked_add_signed(chrono::Duration::hours(i64::from(hours)))
        .ok_or_else(Problem::internal)?;
    let created = state
        .store()
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
pub(in crate::management) async fn revoke_invitation(
    State(state): State<ManagementState>,
    Path(invitation_id): Path<Uuid>,
    headers: HeaderMap,
) -> Result<Json<InvitationResponse>, Problem> {
    let principal = require_mutation_session(&state, &headers).await?;
    require_permission(&principal, Permission::ManageAccess)?;
    let invitation = state
        .store()
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
pub(in crate::management) struct AcceptInvitationRequest {
    #[schema(value_type = String, write_only)]
    pub(in crate::management) token: WriteOnlySecret,
    pub display_name: String,
    #[schema(value_type = String, write_only)]
    pub(in crate::management) password: WriteOnlySecret,
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
pub(in crate::management) async fn accept_invitation(
    State(state): State<ManagementState>,
    connect_info: Option<Extension<ConnectInfo<SocketAddr>>>,
    headers: HeaderMap,
    payload: Result<Json<AcceptInvitationRequest>, JsonRejection>,
) -> Result<Response, Problem> {
    enforce_origin(&state.public_origin, &headers)?;
    validate_session_cookie_ttl(state.session_ttl)?;
    let request = json_payload(payload)?;
    let store = state.store();
    let (source_digest, source_target_digest) = public_auth_source_target_digests(
        state.request_boundary(),
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
        state.session_ttl,
    )
}

/// Prevent an arbitrarily large malformed invitation token from becoming HMAC
/// input while still admitting it against the caller's source bucket.
pub(in crate::management) fn invitation_rate_limit_target(token: &str) -> &str {
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
