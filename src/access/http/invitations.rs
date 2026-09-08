use crate::access::principal::MutationPrincipal;
use crate::access::principal::ReadPrincipal;
use std::fmt;
use std::net::SocketAddr;

use crate::access::identity::AcceptInvitation;
use crate::access::identity::InvitationRecord;
use crate::access::identity::NewInvitation;
use crate::access::policy::Permission;
use crate::crypto::password::hash;
use crate::crypto::session_material::SessionMaterial;
use crate::database::idempotency::Replayable;
use crate::database::idempotency::Response as IdempotencyResponse;
use crate::database::idempotency::fingerprint;
use crate::database::idempotency::operations;
use axum::Json;
use axum::extract::ConnectInfo;
use axum::extract::Extension;
use axum::extract::Path;
use axum::extract::Query;
use axum::extract::State;
use axum::extract::rejection::JsonRejection;
use axum::http::HeaderMap;
use axum::http::StatusCode;
use axum::response::Response;
use chrono::DateTime;
use chrono::Utc;
use serde::Deserialize;
use serde::Serialize;
use tracing::error;
use utoipa::ToSchema;
use uuid::Uuid;
use zeroize::Zeroizing;

use crate::access::cookies::validate_session_cookie_ttl;
use crate::access::http::auth::SessionResponse;
use crate::access::http::auth::UserResponse;
use crate::access::http::auth::installation_name;
use crate::access::http::auth::public_auth_rate_limited;
use crate::access::http::auth::session_response;
use crate::access::http::auth::spawn_password_work;
use crate::access::permissions::parse_user_role;
use crate::access::permissions::require_permission;
use crate::access::sessions::enforce_origin;
use crate::http::control::error_mapping::map_identity;
use crate::http::control::error_mapping::map_persistence;
use crate::http::control::idempotency::MutationReply;
use crate::http::control::idempotency::ReplayableMutation;
use crate::http::control::idempotency::idempotency_http_response;
use crate::http::control::idempotency::require_idempotency_key;
use crate::http::control::json_payload::json_payload;
use crate::http::control::pagination::PageQuery;
use crate::http::control::pagination::page;
use crate::http::control::provenance::Provenance;
use crate::http::control::secrets::WriteOnlySecret;
use crate::http::control::state::ManagementState;
use crate::http::problem::FieldErrors;
use crate::http::problem::Problem;
use crate::http::proxy::public_auth_source_target_digests;

pub(crate) const INVALID_INVITATION_RATE_LIMIT_TARGET: &str = "<invalid-invitation-token>";

#[derive(Clone, Debug, Serialize, ToSchema)]
pub(crate) struct InvitationResponse {
    #[schema(value_type = String, format = Uuid)]
    pub id: Uuid,
    pub email: String,
    pub role: String,
    #[schema(value_type = String, format = Uuid)]
    pub invited_by: Uuid,
    /// Email of the operator who sent the invitation.
    pub invited_by_email: Option<String>,
    pub status: String,
    pub expires_at: DateTime<Utc>,
    pub accepted_at: Option<DateTime<Utc>>,
    /// Email of the account created by accepting the invitation.
    pub accepted_by_email: Option<String>,
    pub revoked_at: Option<DateTime<Utc>>,
    /// Email of the operator who revoked the invitation.
    pub revoked_by_email: Option<String>,
    pub created_at: DateTime<Utc>,
}

impl From<InvitationRecord> for InvitationResponse {
    fn from(invitation: InvitationRecord) -> Self {
        // An invitation that merely timed out reports "expired", including
        // after its pending-email reservation was released; only a deliberate
        // revocation reports "revoked".
        let status = if invitation.accepted_at.is_some() {
            "accepted"
        } else if invitation.revoked_at.is_some() {
            "revoked"
        } else if invitation.expired_at.is_some() || invitation.expires_at <= Utc::now() {
            "expired"
        } else {
            "pending"
        };
        Self {
            id: invitation.id,
            email: invitation.email,
            role: invitation.role.to_string(),
            invited_by: invitation.invited_by,
            invited_by_email: invitation.invited_by_email,
            status: status.to_owned(),
            expires_at: invitation.expires_at,
            accepted_at: invitation.accepted_at,
            accepted_by_email: invitation.accepted_by_email,
            revoked_at: invitation.revoked_at,
            revoked_by_email: invitation.revoked_by_email,
            created_at: invitation.created_at,
        }
    }
}

#[derive(Debug, Serialize, ToSchema)]
pub(crate) struct InvitationListResponse {
    pub items: Vec<InvitationResponse>,
    pub next_cursor: Option<String>,
}

#[derive(Debug, Deserialize, Serialize, ToSchema)]
pub(crate) struct CreateInvitationRequest {
    pub email: String,
    pub role: String,
    /// Invitation lifetime in hours. Defaults to seven days and is capped at
    /// thirty days.
    pub expires_in_hours: Option<u16>,
}

#[derive(Debug, Serialize, ToSchema)]
pub(crate) struct CreateInvitationResponse {
    pub invitation: InvitationResponse,
    /// Returned only by the invitation-creation response.
    #[schema(value_type = String, read_only)]
    token: WriteOnlySecret,
}

#[utoipa::path(
    get,
    path = "/api/v3/invitations",
    tag = "invitations",
    params(
        PageQuery,
    ),
    responses(
        (status = 200, description = "Invitation history", body = InvitationListResponse),
        (status = 400, description = "Malformed query parameters, or an invalid cursor or page size", body = Problem, content_type = "application/problem+json")
    ),
    security(("sessionCookie" = [])))]
pub(crate) async fn list_invitations(
    State(state): State<ManagementState>,
    Query(query): Query<PageQuery>,
    ReadPrincipal(principal): ReadPrincipal,
) -> Result<Json<InvitationListResponse>, Problem> {
    require_permission(&principal, Permission::ReadAccess)?;
    let (cursor, limit) = page(query)?;
    let (invitations, next_cursor) = crate::access::identity::invitations::list_invitations(
        &state.request_boundary.pool,
        cursor,
        limit,
    )
    .await
    .map_err(map_identity)?;
    let items = invitations.into_iter().map(Into::into).collect::<Vec<_>>();
    Ok(Json(InvitationListResponse {
        items,
        next_cursor: next_cursor.map(|cursor| cursor.to_string()),
    }))
}

#[utoipa::path(
    post,
    path = "/api/v3/invitations",
    tag = "invitations",
    params(("Idempotency-Key" = String, Header, description = "Unique invitation creation key")),
    request_body = CreateInvitationRequest,
    responses(
        (status = 201, description = "Invitation created; token is displayed once", body = CreateInvitationResponse, headers(("Location" = String, description = "Path of the created resource"))),
        (status = 400, description = "Idempotency-Key is missing or invalid", body = Problem, content_type = "application/problem+json"),
        (status = 409, description = "Member, pending invitation, or idempotency conflict", body = Problem, content_type = "application/problem+json"),
        (status = 422, description = "Invitation is invalid", body = Problem, content_type = "application/problem+json"),
        (status = 503, description = "Master key or database unavailable", body = Problem, content_type = "application/problem+json")
    ),
    security(("sessionCookie" = [], "csrfToken" = [])))]
pub(crate) async fn create_invitation(
    State(state): State<ManagementState>,
    Provenance(provenance): Provenance,
    headers: HeaderMap,
    MutationPrincipal(principal): MutationPrincipal,
    payload: Result<Json<CreateInvitationRequest>, JsonRejection>,
) -> Result<Response, Problem> {
    require_permission(&principal, Permission::ManageAccess)?;
    let request = json_payload(payload)?;
    let request_fingerprint = fingerprint(&request).map_err(map_persistence)?;
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
    let created = crate::access::identity::invitations::create_invitation(
        &state.request_boundary.pool,
        &provenance,
        NewInvitation {
            email: request.email,
            role,
            expires_at,
            actor: principal.user_id,
            idempotency_key,
        },
        Replayable::new(request_fingerprint, master_key),
        |created| {
            IdempotencyResponse::json(
                StatusCode::CREATED.as_u16(),
                &CreateInvitationResponse {
                    invitation: created.invitation.clone().into(),
                    token: WriteOnlySecret(created.material.token().to_owned()),
                },
                None,
            )
            .and_then(|response| {
                response.with_location(format!("/api/v3/invitations/{}", created.invitation.id))
            })
        },
    )
    .await
    .map_err(map_identity)?;
    idempotency_http_response(created)
}

#[utoipa::path(
    delete,
    path = "/api/v3/invitations/{invitation_id}",
    tag = "invitations",
    params(
        ("invitation_id" = Uuid, Path, description = "Invitation ID"),
        ("Idempotency-Key" = String, Header, description = "Unique invitation revocation key")
    ),
    responses(
        (status = 200, description = "Invitation revoked; an identical retry replays this response", body = InvitationResponse),
        (status = 400, description = "Idempotency-Key is missing or invalid", body = Problem, content_type = "application/problem+json"),
        (status = 409, description = "Invitation is already accepted or revoked, or the Idempotency-Key was already used for a different request", body = Problem, content_type = "application/problem+json")
    ),
    security(("sessionCookie" = [], "csrfToken" = [])))]
pub(crate) async fn revoke_invitation(
    State(state): State<ManagementState>,
    Provenance(provenance): Provenance,
    Path(invitation_id): Path<Uuid>,
    headers: HeaderMap,
    MutationPrincipal(principal): MutationPrincipal,
) -> Result<Response, Problem> {
    require_permission(&principal, Permission::ManageAccess)?;
    let state = &state;
    let provenance = &provenance;
    ReplayableMutation::new(
        state,
        principal.user_id,
        operations::INVITATION_REVOKE,
        &headers,
        &RevokeInvitationFingerprint { invitation_id },
    )?
    .run(|key| async move {
        let invitation = crate::access::identity::invitations::revoke_invitation(
            &state.request_boundary.pool,
            provenance,
            invitation_id,
            principal.user_id,
            &key,
        )
        .await
        .map_err(map_identity)?;
        Ok(MutationReply {
            status: StatusCode::OK,
            body: InvitationResponse::from(invitation),
            etag: None,
            location: None,
        })
    })
    .await
}

#[derive(Serialize)]
struct RevokeInvitationFingerprint {
    invitation_id: Uuid,
}

#[derive(Deserialize, ToSchema)]
pub(crate) struct AcceptInvitationRequest {
    #[schema(value_type = String, write_only)]
    pub(crate) token: WriteOnlySecret,
    pub display_name: String,
    #[schema(value_type = String, write_only)]
    pub(crate) password: WriteOnlySecret,
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
    path = "/api/v3/invitations/accept",
    tag = "invitations",
    request_body = AcceptInvitationRequest,
    responses(
        (status = 201, description = "Invitation accepted and authenticated session created", body = SessionResponse),
        (status = 409, description = "Email is already a member", body = Problem, content_type = "application/problem+json"),
        (status = 410, description = "Invitation is invalid, expired, revoked, or accepted", body = Problem, content_type = "application/problem+json"),
        (status = 429, description = "Password work is rate limited", body = Problem, content_type = "application/problem+json"),
        (status = 422, description = "Password or display name is invalid", body = Problem, content_type = "application/problem+json")
    ),
    security())]
pub(crate) async fn accept_invitation(
    State(state): State<ManagementState>,
    Provenance(provenance): Provenance,
    connect_info: Option<Extension<ConnectInfo<SocketAddr>>>,
    headers: HeaderMap,
    payload: Result<Json<AcceptInvitationRequest>, JsonRejection>,
) -> Result<Response, Problem> {
    enforce_origin(&state.public_origin, &headers)?;
    validate_session_cookie_ttl(state.session_ttl)?;
    let request = json_payload(payload)?;
    let pool = &state.request_boundary.pool;
    let (source_digest, source_target_digest) = public_auth_source_target_digests(
        &state.request_boundary,
        &headers,
        connect_info.map(|Extension(ConnectInfo(peer))| peer),
        invitation_rate_limit_target(request.token.expose()),
    )?;
    if !crate::access::identity::auth_admission::admit_invitation_acceptance_attempt(
        pool,
        source_digest,
        source_target_digest,
    )
    .await
    .map_err(map_identity)?
    {
        return Err(public_auth_rate_limited());
    }
    validate_invitation_acceptance(&request)?;
    let password = Zeroizing::new(request.password.expose().to_owned());
    let password_hash = spawn_password_work(move || hash(&password))?
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
    let accepted = crate::access::identity::invitations::accept_invitation(
        pool,
        &provenance,
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
        installation_name(pool).await?,
        state.session_ttl,
    )
}

/// Prevent an arbitrarily large malformed invitation token from becoming HMAC
/// input while still admitting it against the caller's source bucket.
pub(crate) fn invitation_rate_limit_target(token: &str) -> &str {
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
            vec!["The invitation token is invalid.".to_owned().into()],
        );
    }
    if !(12..=1_024).contains(&request.password.expose().chars().count()) {
        errors.insert(
            "password".to_owned(),
            vec!["Use between 12 and 1,024 characters.".to_owned().into()],
        );
    }
    if request.display_name.trim().is_empty() || request.display_name.chars().count() > 100 {
        errors.insert(
            "display_name".to_owned(),
            vec!["Use between 1 and 100 characters.".to_owned().into()],
        );
    }
    if errors.is_empty() {
        Ok(())
    } else {
        Err(Problem::validation(errors))
    }
}
