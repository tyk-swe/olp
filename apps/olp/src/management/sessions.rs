use axum::http::{HeaderMap, StatusCode, header};
use olp_storage::{authentication::SessionPrincipal, security::SessionMaterial};
use tracing::warn;

use crate::{
    ManagementState, Problem,
    public_http::request_cookies::{RequestCookies, SESSION_COOKIE},
};

use super::error_mapping::map_persistence;

pub(crate) const CSRF_HEADER: &str = "x-csrf-token";
pub(crate) const SETUP_TOKEN_HEADER: &str = "x-olp-setup-token";

pub(crate) fn enforce_origin(
    public_origin: &crate::PublicOrigin,
    headers: &HeaderMap,
) -> Result<(), Problem> {
    let origin = headers
        .get(header::ORIGIN)
        .and_then(|value| value.to_str().ok())
        .ok_or_else(|| Problem::forbidden("origin_required", "An Origin header is required."))?;
    if !public_origin.matches_header(origin) {
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
    enforce_origin(&state.public_origin, headers)?;
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
