use axum::{
    http::{HeaderValue, header},
    response::Response,
};
use olp_db::{security::RecentAuthMaterial, security::SessionMaterial};
use tracing::error;

use crate::{
    Problem,
    public_http::request_cookies::{CSRF_COOKIE, SESSION_COOKIE},
};

use super::{response_policy::prevent_sensitive_response_caching, sessions::CSRF_HEADER};

pub(crate) use crate::public_http::request_cookies::RECENT_AUTH_COOKIE;

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
