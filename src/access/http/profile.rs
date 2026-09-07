use crate::access::principal::MutationPrincipal;
use crate::access::principal::ReadPrincipal;
use std::fmt;

use crate::access::authentication::RecentAuthPurpose;
use crate::access::authentication::SessionSecurityContext;
use crate::crypto::password::hash;
use crate::crypto::password::verify;
use crate::crypto::session_material::RecentAuthMaterial;
use crate::crypto::session_material::SessionMaterial;
use axum::Json;
use axum::extract::State;
use axum::extract::rejection::JsonRejection;
use axum::http::HeaderMap;
use axum::http::StatusCode;
use axum::response::IntoResponse;
use axum::response::Response;
use chrono::Duration;
use serde::Deserialize;
use tracing::error;
use utoipa::ToSchema;
use uuid::Uuid;
use zeroize::Zeroizing;

use crate::access::cookies::append_recent_auth_cookie;
use crate::access::cookies::append_security_transition_cookies;
use crate::access::cookies::validate_session_cookie_ttl;
use crate::access::http::auth::spawn_password_work;
use crate::access::http::users::UserDetailResponse;
use crate::access::sessions::cookie;
use crate::access::sessions::reauthentication_required;
use crate::http::control::error_mapping::map_identity;
use crate::http::control::error_mapping::map_persistence;
use crate::http::control::error_mapping::user_not_found;
use crate::http::control::json_payload::json_payload;
use crate::http::control::preconditions::if_match;
use crate::http::control::preconditions::with_etag;
use crate::http::control::provenance::Provenance;
use crate::http::control::response_policy::prevent_sensitive_response_caching;
use crate::http::control::secrets::WriteOnlySecret;
use crate::http::control::state::ManagementState;
use crate::http::problem::Problem;
use crate::http::request_cookies::RECENT_AUTH_COOKIE;

const RECENT_AUTH_TTL: Duration = Duration::minutes(5);

#[utoipa::path(
    get,
    path = "/api/v3/profile",
    tag = "users",
    responses(
        (status = 200, description = "Current user profile", body = UserDetailResponse),
        (status = 401, description = "No active session", body = Problem, content_type = "application/problem+json")
    ),
    security(("sessionCookie" = [])))]
pub(crate) async fn profile(
    State(state): State<ManagementState>,
    ReadPrincipal(principal): ReadPrincipal,
) -> Result<Response, Problem> {
    let user =
        crate::access::identity::accounts::user(&state.request_boundary.pool, principal.user_id)
            .await
            .map_err(map_identity)?
            .ok_or_else(user_not_found)?;
    let etag = user.etag;
    with_etag(Json(UserDetailResponse::from(user)), etag)
}

#[derive(Debug, Deserialize, ToSchema)]
pub(crate) struct UpdateProfileRequest {
    pub display_name: String,
}

#[utoipa::path(
    patch,
    path = "/api/v3/profile",
    tag = "users",
    params(("If-Match" = String, Header, description = "Current profile ETag")),
    request_body = UpdateProfileRequest,
    responses(
        (status = 200, description = "Profile updated", body = UserDetailResponse),
        (status = 412, description = "ETag mismatch", body = Problem, content_type = "application/problem+json"),
        (status = 422, description = "Display name is invalid", body = Problem, content_type = "application/problem+json")
    ),
    security(("sessionCookie" = [], "csrfToken" = [])))]
pub(crate) async fn update_profile(
    State(state): State<ManagementState>,
    Provenance(provenance): Provenance,
    headers: HeaderMap,
    MutationPrincipal(principal): MutationPrincipal,
    payload: Result<Json<UpdateProfileRequest>, JsonRejection>,
) -> Result<Response, Problem> {
    let request = json_payload(payload)?;
    let user = crate::access::identity::accounts::update_profile(
        &state.request_boundary.pool,
        &provenance,
        principal.user_id,
        &request.display_name,
        if_match(&headers)?,
    )
    .await
    .map_err(map_identity)?;
    let etag = user.etag;
    with_etag(Json(UserDetailResponse::from(user)), etag)
}

#[derive(Deserialize, ToSchema)]
#[serde(deny_unknown_fields)]
pub(crate) struct RecentAuthenticationRequest {
    #[schema(value_type = String, write_only)]
    current_password: WriteOnlySecret,
    /// Exact security operation authorized by this one-time grant.
    purpose: String,
    #[schema(value_type = Option<String>, format = Uuid)]
    resource_id: Option<Uuid>,
}

impl fmt::Debug for RecentAuthenticationRequest {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("RecentAuthenticationRequest")
            .field("current_password", &"[REDACTED]")
            .field("purpose", &self.purpose)
            .field("resource_id", &self.resource_id)
            .finish()
    }
}

#[utoipa::path(
    post,
    path = "/api/v3/profile/reauthenticate",
    tag = "users",
    request_body = RecentAuthenticationRequest,
    responses(
        (status = 204, description = "One-time recent-authentication grant issued"),
        (status = 401, description = "Session changed while authenticating", body = Problem, content_type = "application/problem+json"),
        (status = 403, description = "Current password is invalid or local auth is unavailable", body = Problem, content_type = "application/problem+json"),
        (status = 422, description = "Purpose or resource binding is invalid", body = Problem, content_type = "application/problem+json"),
        (status = 429, description = "Password work is rate limited", body = Problem, content_type = "application/problem+json")
    ),
    security(("sessionCookie" = [], "csrfToken" = [])))]
pub(crate) async fn recent_authentication(
    State(state): State<ManagementState>,
    Provenance(provenance): Provenance,
    MutationPrincipal(principal): MutationPrincipal,
    payload: Result<Json<RecentAuthenticationRequest>, JsonRejection>,
) -> Result<Response, Problem> {
    let request = json_payload(payload)?;
    let purpose = RecentAuthPurpose::parse(&request.purpose).ok_or_else(|| {
        Problem::field_validation(
            "purpose",
            "Use password_enrollment, oidc_link, or oidc_unlink.",
        )
    })?;
    if request.resource_id.is_some() != purpose.requires_resource() {
        return Err(Problem::field_validation(
            "resource_id",
            if purpose.requires_resource() {
                "The exact identity ID is required for OIDC unlinking."
            } else {
                "This operation must not include a resource ID."
            },
        ));
    }
    if request.current_password.expose().chars().count() > 1_024 {
        return Err(Problem::forbidden(
            "current_password_invalid",
            "The current password is invalid.",
        ));
    }
    let local = crate::access::authentication::sessions::local_password_user(
        &state.request_boundary.pool,
        &principal.email,
    )
    .await
    .map_err(map_persistence)?
    .filter(|user| user.id == principal.user_id)
    .ok_or_else(|| {
        Problem::forbidden(
            "local_password_unavailable",
            "This profile does not have a local password.",
        )
    })?;
    let password = Zeroizing::new(request.current_password.expose().to_owned());
    let encoded = local.password_hash;
    let valid = spawn_password_work(move || verify(&password, &encoded))?
        .await
        .map_err(|error| {
            error!(%error, "recent-authentication password task failed");
            Problem::internal()
        })?;
    if !valid {
        return Err(Problem::forbidden(
            "current_password_invalid",
            "The current password is invalid.",
        ));
    }
    let material = RecentAuthMaterial::generate();
    let installed = crate::access::authentication::issue_recent_authentication(
        &state.request_boundary.pool,
        &provenance,
        SessionSecurityContext {
            session_id: principal.session_id,
            user_id: principal.user_id,
            security_version: principal.security_version,
        },
        purpose,
        request.resource_id,
        &material,
        RECENT_AUTH_TTL,
    )
    .await
    .map_err(map_persistence)?;
    if !installed {
        return Err(Problem::unauthorized(
            "The session changed while recent authentication was being recorded.",
        ));
    }
    let mut response = StatusCode::NO_CONTENT.into_response();
    append_recent_auth_cookie(&mut response, &material, RECENT_AUTH_TTL)?;
    prevent_sensitive_response_caching(&mut response);
    Ok(response)
}

#[derive(Deserialize, ToSchema)]
pub(crate) struct ChangePasswordRequest {
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

fn validate_new_password(password: &WriteOnlySecret) -> Result<(), Problem> {
    if (12..=1_024).contains(&password.expose().chars().count()) {
        return Ok(());
    }
    Err(Problem::field_validation(
        "new_password",
        "Use between 12 and 1,024 characters.",
    ))
}

#[utoipa::path(
    post,
    path = "/api/v3/profile/password",
    tag = "users",
    params(("If-Match" = String, Header, description = "Current profile ETag")),
    request_body = ChangePasswordRequest,
    responses(
        (status = 200, description = "Local password changed; every previous session revoked and this browser rotated", body = UserDetailResponse),
        (status = 403, description = "Current password is invalid or local auth is unavailable", body = Problem, content_type = "application/problem+json"),
        (status = 429, description = "Password work is rate limited", body = Problem, content_type = "application/problem+json"),
        (status = 412, description = "ETag mismatch", body = Problem, content_type = "application/problem+json"),
        (status = 422, description = "New password is invalid", body = Problem, content_type = "application/problem+json")
    ),
    security(("sessionCookie" = [], "csrfToken" = [])))]
pub(crate) async fn change_password(
    State(state): State<ManagementState>,
    Provenance(provenance): Provenance,
    headers: HeaderMap,
    MutationPrincipal(principal): MutationPrincipal,
    payload: Result<Json<ChangePasswordRequest>, JsonRejection>,
) -> Result<Response, Problem> {
    validate_session_cookie_ttl(state.session_ttl)?;
    let request = json_payload(payload)?;
    let expected_etag = if_match(&headers)?;
    validate_new_password(&request.new_password)?;
    let local = crate::access::authentication::sessions::local_password_user(
        &state.request_boundary.pool,
        &principal.email,
    )
    .await
    .map_err(map_persistence)?
    .filter(|user| user.id == principal.user_id)
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
        if !verify(&current_password, &current_hash) {
            return Ok(None);
        }
        hash(&new_password).map(Some)
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
    let replacement = SessionMaterial::generate();
    let rotation = crate::access::identity::accounts::update_local_password(
        &state.request_boundary.pool,
        &provenance,
        &password_hash,
        expected_etag,
        SessionSecurityContext {
            session_id: principal.session_id,
            user_id: principal.user_id,
            security_version: principal.security_version,
        },
        &replacement,
        state.session_ttl,
    )
    .await
    .map_err(map_identity)?;
    let etag = rotation.user.etag;
    let mut response = with_etag(Json(UserDetailResponse::from(rotation.user)), etag)?;
    append_security_transition_cookies(&mut response, &replacement, state.session_ttl)?;
    Ok(response)
}

#[derive(Deserialize, ToSchema)]
#[serde(deny_unknown_fields)]
pub(crate) struct EnrollPasswordRequest {
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
    path = "/api/v3/profile/password/enroll",
    tag = "users",
    params(("If-Match" = String, Header, description = "Current profile ETag")),
    request_body = EnrollPasswordRequest,
    responses(
        (status = 200, description = "First local password enrolled; every previous session revoked and this browser rotated", body = UserDetailResponse),
        (status = 409, description = "A local password is already configured", body = Problem, content_type = "application/problem+json"),
        (status = 428, description = "Recent authentication is required", body = Problem, content_type = "application/problem+json"),
        (status = 429, description = "Password work is rate limited", body = Problem, content_type = "application/problem+json"),
        (status = 412, description = "ETag mismatch", body = Problem, content_type = "application/problem+json"),
        (status = 422, description = "New password is invalid", body = Problem, content_type = "application/problem+json")
    ),
    security(("sessionCookie" = [], "csrfToken" = [])))]
pub(crate) async fn enroll_password(
    State(state): State<ManagementState>,
    Provenance(provenance): Provenance,
    headers: HeaderMap,
    MutationPrincipal(principal): MutationPrincipal,
    payload: Result<Json<EnrollPasswordRequest>, JsonRejection>,
) -> Result<Response, Problem> {
    validate_session_cookie_ttl(state.session_ttl)?;
    let recent_auth = cookie(&headers, RECENT_AUTH_COOKIE)?
        .filter(|token| token.len() == 43)
        .ok_or_else(reauthentication_required)?;
    let recent_auth_token_digest = RecentAuthMaterial::digest_token(recent_auth);
    let request = json_payload(payload)?;
    let expected_etag = if_match(&headers)?;
    validate_new_password(&request.new_password)?;
    let new_password = Zeroizing::new(request.new_password.expose().to_owned());
    let password_hash = spawn_password_work(move || hash(&new_password))?
        .await
        .map_err(|error| {
            error!(%error, "password enrollment task failed");
            Problem::internal()
        })?
        .map_err(|error| {
            error!(%error, "enrolled password hashing failed");
            Problem::internal()
        })?;
    let replacement = SessionMaterial::generate();
    let rotation = crate::access::identity::accounts::enroll_local_password(
        &state.request_boundary.pool,
        &provenance,
        &password_hash,
        expected_etag,
        SessionSecurityContext {
            session_id: principal.session_id,
            user_id: principal.user_id,
            security_version: principal.security_version,
        },
        recent_auth_token_digest,
        &replacement,
        state.session_ttl,
    )
    .await
    .map_err(map_identity)?;
    let etag = rotation.user.etag;
    let mut response = with_etag(Json(UserDetailResponse::from(rotation.user)), etag)?;
    append_security_transition_cookies(&mut response, &replacement, state.session_ttl)?;
    Ok(response)
}
