use std::fmt;

use axum::{
    Json,
    body::Body,
    extract::rejection::JsonRejection,
    http::{HeaderMap, HeaderValue, StatusCode, header},
    response::Response,
};
use olp_domain::{Permission, Role};
use olp_storage::{
    AccessError, ConfigurationError, IdempotencyOutcome, IdentityError, PersistenceError,
    RecentAuthMaterial, SessionMaterial, SessionPrincipal,
};
use serde::{Deserialize, Serialize, Serializer};
use tracing::{error, warn};
use utoipa::ToSchema;
use uuid::Uuid;
use zeroize::Zeroize;

use crate::{
    FieldErrors, ManagementState, Problem,
    request_cookies::{CSRF_COOKIE, RequestCookies, SESSION_COOKIE},
};

pub(crate) use crate::request_cookies::RECENT_AUTH_COOKIE;
pub(crate) const CSRF_HEADER: &str = "x-csrf-token";
pub(crate) const SETUP_TOKEN_HEADER: &str = "x-olp-setup-token";

#[derive(Deserialize)]
pub(crate) struct WriteOnlySecret(pub(super) String);

impl Serialize for WriteOnlySecret {
    fn serialize<S>(&self, serializer: S) -> Result<S::Ok, S::Error>
    where
        S: Serializer,
    {
        serializer.serialize_str(&self.0)
    }
}

impl WriteOnlySecret {
    pub(crate) fn new(value: String) -> Self {
        Self(value)
    }

    pub(crate) fn expose(&self) -> &str {
        &self.0
    }
}

impl Drop for WriteOnlySecret {
    fn drop(&mut self) {
        self.0.zeroize();
    }
}

impl fmt::Debug for WriteOnlySecret {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str("WriteOnlySecret([REDACTED])")
    }
}

#[derive(Debug, Serialize, ToSchema)]
pub(crate) struct RuntimeGenerationResponse {
    #[schema(value_type = String, format = Uuid)]
    pub id: Uuid,
    pub sequence: i64,
}

#[derive(Debug, Deserialize)]
pub(super) struct PageQuery {
    pub cursor: Option<String>,
    pub limit: Option<u16>,
}

pub(crate) fn prevent_sensitive_response_caching(response: &mut Response) {
    response
        .headers_mut()
        .insert(header::CACHE_CONTROL, HeaderValue::from_static("no-store"));
    response
        .headers_mut()
        .insert(header::PRAGMA, HeaderValue::from_static("no-cache"));
}

pub(super) fn page_parameters(query: PageQuery) -> Result<(Option<Uuid>, i64), Problem> {
    let cursor = query
        .cursor
        .map(|cursor| {
            Uuid::parse_str(&cursor).map_err(|_| {
                Problem::bad_request(
                    "invalid_cursor",
                    "The pagination cursor is invalid or malformed.",
                )
            })
        })
        .transpose()?;
    let limit = query.limit.unwrap_or(50);
    if !(1..=100).contains(&limit) {
        return Err(Problem::bad_request(
            "invalid_page_size",
            "Page size must be between 1 and 100.",
        ));
    }
    Ok((cursor, i64::from(limit)))
}

pub(super) fn parse_user_role(role: &str) -> Result<Role, Problem> {
    role.parse().map_err(|_| {
        let mut errors = FieldErrors::new();
        errors.insert(
            "role".to_owned(),
            vec!["Use owner, operator, developer, or viewer.".to_owned()],
        );
        Problem::validation(errors)
    })
}

pub(crate) fn append_session_cookies(
    response: &mut Response,
    material: &SessionMaterial,
    ttl: chrono::Duration,
) -> Result<(), Problem> {
    let max_age = cookie_max_age(ttl)?;
    append_set_cookie(
        response,
        format!(
            "{SESSION_COOKIE}={}; Path=/; Max-Age={max_age}; Secure; HttpOnly; SameSite=Lax",
            material.token()
        ),
    )?;
    append_set_cookie(
        response,
        format!(
            "{CSRF_COOKIE}={}; Path=/; Max-Age={max_age}; Secure; SameSite=Lax",
            material.csrf_token()
        ),
    )?;
    Ok(())
}

pub(crate) fn append_security_transition_cookies(
    response: &mut Response,
    material: &SessionMaterial,
    ttl: chrono::Duration,
) -> Result<(), Problem> {
    append_session_cookies(response, material, ttl)?;
    response.headers_mut().insert(
        CSRF_HEADER,
        HeaderValue::from_str(material.csrf_token()).map_err(|_| Problem::internal())?,
    );
    clear_recent_auth_cookie(response);
    prevent_sensitive_response_caching(response);
    Ok(())
}

pub(crate) fn append_recent_auth_cookie(
    response: &mut Response,
    material: &RecentAuthMaterial,
    ttl: chrono::Duration,
) -> Result<(), Problem> {
    let max_age = cookie_max_age(ttl)?;
    append_set_cookie(
        response,
        format!(
            "{RECENT_AUTH_COOKIE}={}; Path=/; Max-Age={max_age}; Secure; HttpOnly; SameSite=Lax",
            material.token()
        ),
    )
}

pub(crate) fn clear_recent_auth_cookie(response: &mut Response) {
    append_static_cookie(
        response,
        "__Host-olp_recent_auth=; Path=/; Max-Age=0; Secure; HttpOnly; SameSite=Lax",
    );
}

pub(crate) fn expire_session_cookies(response: &mut Response) {
    append_static_cookie(
        response,
        "__Host-olp_session=; Path=/; Max-Age=0; Secure; HttpOnly; SameSite=Lax",
    );
    append_static_cookie(
        response,
        "__Host-olp_csrf=; Path=/; Max-Age=0; Secure; SameSite=Lax",
    );
    clear_recent_auth_cookie(response);
}

pub(crate) fn validate_session_cookie_ttl(ttl: chrono::Duration) -> Result<(), Problem> {
    cookie_max_age(ttl).map(|_| ())
}

fn cookie_max_age(ttl: chrono::Duration) -> Result<i64, Problem> {
    let seconds = ttl.num_seconds();
    if !(1..=i64::from(i32::MAX)).contains(&seconds) {
        error!(seconds, "session cookie lifetime is not representable");
        return Err(Problem::internal());
    }
    Ok(seconds)
}

fn append_set_cookie(response: &mut Response, cookie: String) -> Result<(), Problem> {
    response.headers_mut().append(
        header::SET_COOKIE,
        HeaderValue::from_str(&cookie).map_err(|_| Problem::internal())?,
    );
    Ok(())
}

fn append_static_cookie(response: &mut Response, cookie: &'static str) {
    response
        .headers_mut()
        .append(header::SET_COOKIE, HeaderValue::from_static(cookie));
}

pub(crate) fn enforce_origin(
    state: &crate::GatewayState,
    headers: &HeaderMap,
) -> Result<(), Problem> {
    let origin = headers
        .get(header::ORIGIN)
        .and_then(|value| value.to_str().ok())
        .ok_or_else(|| Problem::forbidden("origin_required", "An Origin header is required."))?;
    if !state.public_origin.matches_header(origin) {
        warn!(%origin, "rejected cross-origin management mutation");
        return Err(Problem::forbidden(
            "origin_not_allowed",
            "The request origin is not allowed.",
        ));
    }
    Ok(())
}

pub(crate) fn session_cookie(headers: &HeaderMap) -> Result<&str, Problem> {
    cookie(headers, SESSION_COOKIE)?
        .ok_or_else(|| Problem::unauthorized("The session cookie is missing."))
}

pub(crate) fn cookie<'a>(
    headers: &'a HeaderMap,
    expected_name: &str,
) -> Result<Option<&'a str>, Problem> {
    Ok(RequestCookies::parse(headers)?.get(expected_name))
}

pub(crate) async fn require_read_session(
    state: &ManagementState,
    headers: &HeaderMap,
) -> Result<SessionPrincipal, Problem> {
    let token = session_cookie(headers)?;
    state
        .store()
        .session_principal(token)
        .await
        .map_err(map_persistence)?
        .ok_or_else(|| Problem::unauthorized("The session is missing or expired."))
}

pub(crate) async fn require_mutation_session(
    state: &ManagementState,
    headers: &HeaderMap,
) -> Result<SessionPrincipal, Problem> {
    enforce_origin(state, headers)?;
    let principal = require_read_session(state, headers).await?;
    let csrf = headers
        .get(CSRF_HEADER)
        .and_then(|value| value.to_str().ok())
        .ok_or_else(|| Problem::forbidden("csrf_required", "A CSRF token is required."))?;
    if !SessionMaterial::verify_csrf(csrf, &principal.csrf_digest) {
        return Err(Problem::forbidden(
            "csrf_invalid",
            "The CSRF token is invalid.",
        ));
    }
    Ok(principal)
}

pub(crate) fn reauthentication_required() -> Problem {
    Problem::new(
        StatusCode::PRECONDITION_REQUIRED,
        "reauthentication_required",
        "Recent authentication required",
        "Authenticate again in this browser before changing account security settings.",
    )
}

pub(crate) fn require_permission(
    principal: &SessionPrincipal,
    permission: Permission,
) -> Result<(), Problem> {
    let role = principal.role.parse::<Role>().map_err(|_| {
        error!(user_id = %principal.user_id, "session contains an unknown fixed role");
        Problem::forbidden(
            "permission_denied",
            "The current role cannot perform this operation.",
        )
    })?;
    if role.allows(permission) {
        Ok(())
    } else {
        Err(Problem::forbidden(
            "permission_denied",
            "The current role cannot perform this operation.",
        ))
    }
}

pub(super) fn require_provider_manager(principal: &SessionPrincipal) -> Result<(), Problem> {
    require_permission(principal, Permission::ManageProviders)
}

pub(super) fn require_key_manager(principal: &SessionPrincipal) -> Result<(), Problem> {
    require_permission(principal, Permission::ManageApiKeys)
}

pub(super) fn require_route_manager(principal: &SessionPrincipal) -> Result<(), Problem> {
    require_permission(principal, Permission::ManageRoutes)
}

pub(crate) fn require_idempotency_key(headers: &HeaderMap) -> Result<&str, Problem> {
    let value = headers
        .get("idempotency-key")
        .and_then(|value| value.to_str().ok())
        .ok_or_else(|| {
            Problem::bad_request(
                "idempotency_key_required",
                "An Idempotency-Key header is required.",
            )
        })?;
    if !(8..=128).contains(&value.len())
        || !value
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'-' | b'_' | b'.'))
    {
        return Err(Problem::bad_request(
            "invalid_idempotency_key",
            "Idempotency-Key must be 8-128 URL-safe ASCII characters.",
        ));
    }
    Ok(value)
}

pub(crate) fn if_match(headers: &HeaderMap) -> Result<Uuid, Problem> {
    let value = headers
        .get(header::IF_MATCH)
        .and_then(|value| value.to_str().ok())
        .ok_or_else(|| {
            Problem::new(
                StatusCode::PRECONDITION_REQUIRED,
                "if_match_required",
                "Precondition required",
                "Supply the current ETag in If-Match.",
            )
        })?;
    let value = value
        .strip_prefix('"')
        .and_then(|value| value.strip_suffix('"'))
        .ok_or_else(|| {
            Problem::bad_request("invalid_if_match", "If-Match must be a strong UUID ETag.")
        })?;
    Uuid::parse_str(value).map_err(|_| {
        Problem::bad_request("invalid_if_match", "If-Match must contain one UUID ETag.")
    })
}

pub(super) fn map_configuration(error: ConfigurationError) -> Problem {
    match error {
        ConfigurationError::ProviderNotFound => Problem::new(
            StatusCode::NOT_FOUND,
            "provider_not_found",
            "Provider not found",
            "The provider does not exist.",
        ),
        ConfigurationError::ProviderIncomplete => {
            let mut errors = FieldErrors::new();
            errors.insert(
                "provider".to_owned(),
                vec!["A credential and enabled model are required before activation; OpenAI-compatible model capabilities must also be live-certified.".to_owned()],
            );
            Problem::validation(errors)
        }
        ConfigurationError::PreconditionFailed => Problem::new(
            StatusCode::PRECONDITION_FAILED,
            "etag_mismatch",
            "Precondition failed",
            "The provider changed after it was loaded. Refresh and retry.",
        ),
        ConfigurationError::Persistence(error) => map_persistence(error),
        ConfigurationError::RouteNotFound => Problem::new(
            StatusCode::NOT_FOUND,
            "route_draft_not_found",
            "Route draft not found",
            "The route draft does not exist.",
        ),
        ConfigurationError::RouteNotValidated => Problem::conflict(
            "route_not_validated",
            "Validate the route draft before activation.",
        ),
        ConfigurationError::InvalidRoute(detail) => {
            let mut errors = FieldErrors::new();
            errors.insert("route".to_owned(), vec![detail]);
            Problem::validation(errors)
        }
        ConfigurationError::RuntimeCompile(error) => {
            error!(%error, "runtime compilation failed");
            Problem::internal()
        }
        ConfigurationError::InvalidCredential => {
            error!("stored provider credential is malformed");
            Problem::internal()
        }
        ConfigurationError::IdempotencyConflict => Problem::conflict(
            "idempotency_key_reused",
            "This Idempotency-Key has already been used for this operation.",
        ),
        ConfigurationError::IdempotencyInProgress => Problem::conflict(
            "idempotency_in_progress",
            "An operation with this Idempotency-Key is still in progress.",
        ),
        ConfigurationError::NotFound => Problem::new(
            StatusCode::NOT_FOUND,
            "configuration_resource_not_found",
            "Resource not found",
            "The requested configuration resource does not exist.",
        ),
        ConfigurationError::InUse => Problem::conflict(
            "configuration_resource_in_use",
            "The resource is active or referenced and cannot be removed.",
        ),
        ConfigurationError::Invalid(detail) => {
            let mut errors = FieldErrors::new();
            errors.insert("configuration".to_owned(), vec![detail]);
            Problem::validation(errors)
        }
        ConfigurationError::ProviderRevisionDiffTooLarge { dimension, maximum } => {
            let mut errors = FieldErrors::new();
            errors.insert(
                "revisions".to_owned(),
                vec![format!(
                    "provider revision diff supports at most {maximum} {dimension} per revision"
                )],
            );
            Problem::validation(errors)
        }
    }
}

pub(super) fn map_access(error: AccessError) -> Problem {
    match error {
        AccessError::Persistence(error) => map_persistence(error),
        AccessError::RuntimeCompile(error) => {
            error!(%error, "runtime compilation failed after API key change");
            Problem::internal()
        }
        AccessError::Invalid(detail) => {
            let mut errors = FieldErrors::new();
            errors.insert("api_key".to_owned(), vec![detail]);
            Problem::validation(errors)
        }
        AccessError::NotFound => Problem::new(
            StatusCode::NOT_FOUND,
            "api_key_not_found",
            "API key not found",
            "The API key does not exist.",
        ),
        AccessError::PreconditionFailed => Problem::new(
            StatusCode::PRECONDITION_FAILED,
            "etag_mismatch",
            "Precondition failed",
            "The API key changed after it was loaded. Refresh and retry.",
        ),
        AccessError::IdempotencyConflict => Problem::conflict(
            "idempotency_key_reused",
            "This Idempotency-Key has already been used for that API key operation.",
        ),
        AccessError::IdempotencyInProgress => Problem::conflict(
            "idempotency_in_progress",
            "An operation with this Idempotency-Key is still in progress.",
        ),
    }
}

pub(crate) fn idempotency_http_response<T>(
    outcome: IdempotencyOutcome<T>,
) -> Result<Response, Problem> {
    let replay = match outcome {
        IdempotencyOutcome::Executed { response, .. } | IdempotencyOutcome::Replayed(response) => {
            response
        }
    };
    let (status, content_type, etag, body) = replay.into_parts();
    let mut response = Response::new(Body::from(body));
    *response.status_mut() = StatusCode::from_u16(status).map_err(|_| Problem::internal())?;
    if let Some(content_type) = content_type {
        response.headers_mut().insert(
            header::CONTENT_TYPE,
            HeaderValue::from_str(&content_type).map_err(|_| Problem::internal())?,
        );
    }
    if let Some(etag) = etag {
        response.headers_mut().insert(
            header::ETAG,
            HeaderValue::from_str(&etag).map_err(|_| Problem::internal())?,
        );
    }
    prevent_sensitive_response_caching(&mut response);
    Ok(response)
}

pub(super) fn user_not_found() -> Problem {
    Problem::new(
        StatusCode::NOT_FOUND,
        "user_not_found",
        "User not found",
        "The user does not exist.",
    )
}

pub(super) fn map_identity(error: IdentityError) -> Problem {
    match error {
        IdentityError::Persistence(error) => map_persistence(error),
        IdentityError::Invalid(detail) => {
            let mut errors = FieldErrors::new();
            errors.insert("identity".to_owned(), vec![detail]);
            Problem::validation(errors)
        }
        IdentityError::NotFound => Problem::new(
            StatusCode::NOT_FOUND,
            "identity_resource_not_found",
            "Identity resource not found",
            "The requested identity resource does not exist.",
        ),
        IdentityError::PreconditionFailed => Problem::new(
            StatusCode::PRECONDITION_FAILED,
            "etag_mismatch",
            "Precondition failed",
            "The user changed after it was loaded. Refresh and retry.",
        ),
        IdentityError::LastOwner => Problem::conflict(
            "last_owner_required",
            "The last active owner cannot be demoted.",
        ),
        IdentityError::EmailAlreadyMember => Problem::conflict(
            "email_already_member",
            "A user with this email already belongs to the installation.",
        ),
        IdentityError::PendingInvitationExists => Problem::conflict(
            "pending_invitation_exists",
            "A pending invitation already exists for this email.",
        ),
        IdentityError::InvitationUnavailable => Problem::new(
            StatusCode::GONE,
            "invitation_unavailable",
            "Invitation unavailable",
            "The invitation is invalid, expired, revoked, or already accepted.",
        ),
        IdentityError::SessionForbidden => Problem::forbidden(
            "permission_denied",
            "Only an owner can revoke another user's session.",
        ),
        IdentityError::CorruptIdentity => {
            error!("stored identity data contains an unknown role");
            Problem::internal()
        }
        IdentityError::IdempotencyConflict => Problem::conflict(
            "idempotency_key_reused",
            "This Idempotency-Key has already been used for this invitation operation.",
        ),
        IdentityError::IdempotencyInProgress => Problem::conflict(
            "idempotency_in_progress",
            "An operation with this Idempotency-Key is still in progress.",
        ),
        IdentityError::LocalPasswordUnavailable => Problem::forbidden(
            "local_password_unavailable",
            "This profile does not have a local password.",
        ),
        IdentityError::LocalPasswordAlreadyConfigured => Problem::conflict(
            "local_password_already_configured",
            "A local password is already configured. Use the password-change operation.",
        ),
        IdentityError::RecentAuthenticationRequired => reauthentication_required(),
        IdentityError::SessionUnavailable => Problem::unauthorized(
            "The session changed while the security operation was in progress.",
        ),
    }
}

pub(crate) fn map_persistence(error: PersistenceError) -> Problem {
    match error {
        PersistenceError::SessionUnavailable => {
            Problem::unauthorized("The session is missing, expired, or no longer current.")
        }
        PersistenceError::InvalidSessionTtl | PersistenceError::InvalidRecentAuthentication => {
            error!(%error, "invalid server authentication configuration");
            Problem::internal()
        }
        other => {
            error!(error = %other, "management persistence operation failed");
            Problem::service_unavailable("database_unavailable")
        }
    }
}

pub(crate) fn json_payload<T>(payload: Result<Json<T>, JsonRejection>) -> Result<T, Problem> {
    payload.map(|Json(value)| value).map_err(|error| {
        Problem::bad_request("invalid_json", format!("The JSON body is invalid: {error}"))
    })
}
