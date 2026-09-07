use crate::access::principal::ReadPrincipal;
use std::collections::BTreeMap;
use std::fmt;
use std::net::SocketAddr;
use std::sync::LazyLock;

use crate::access::identity::InstallationSetupInput;
use crate::crypto::password::hash;
use crate::crypto::password::verify;
use crate::crypto::session_material::CsrfMaterial;
use crate::crypto::session_material::SessionMaterial;
use axum::Json;
use axum::extract::ConnectInfo;
use axum::extract::Extension;
use axum::extract::State;
use axum::extract::rejection::JsonRejection;
use axum::http::HeaderMap;
use axum::http::StatusCode;
use axum::response::IntoResponse;
use axum::response::Response;
use serde::Deserialize;
use serde::Serialize;
use sqlx::PgPool;
use tokio::sync::Semaphore;
use tokio::sync::SemaphorePermit;
use tracing::error;
use tracing::warn;
use utoipa::ToSchema;
use uuid::Uuid;
use zeroize::Zeroizing;

use crate::access::cookies::append_session_cookies;
use crate::access::cookies::clear_recent_auth_cookie;
use crate::access::cookies::expire_session_cookies;
use crate::access::cookies::validate_session_cookie_ttl;
use crate::access::sessions::cookie;
use crate::access::sessions::enforce_origin;
use crate::access::sessions::require_read_session;
use crate::http::control::error_mapping::map_identity;
use crate::http::control::error_mapping::map_persistence;
use crate::http::control::json_payload::json_payload;
use crate::http::control::provenance::Provenance;
use crate::http::control::response_policy::prevent_sensitive_response_caching;
use crate::http::control::secrets::WriteOnlySecret;
use crate::http::control::state::ManagementState;
use crate::http::problem::FieldErrors;
use crate::http::problem::Problem;
use crate::http::proxy::public_auth_source_target_digests;
use crate::http::request_admission::FirstOwnerSetupAuthorized;
use crate::http::request_cookies::CSRF_COOKIE;
use crate::http::request_cookies::SESSION_COOKIE;

pub(crate) const INVALID_LOGIN_RATE_LIMIT_TARGET: &str = "<invalid-local-login-target>";
static PASSWORD_WORK: LazyLock<Semaphore> =
    LazyLock::new(|| Semaphore::new(password_work_concurrency()));

pub(crate) fn password_work_concurrency() -> usize {
    // The upper bound caps memory pinned by unauthenticated Argon2 hashing
    // (each permit holds the full Argon2 working set); scaling with cores
    // must not turn many-core hosts into a pre-auth memory-exhaustion vector.
    std::thread::available_parallelism()
        .map(|parallelism| parallelism.get().div_ceil(2))
        .unwrap_or(4)
        .clamp(4, 8)
}

#[derive(Debug, Serialize, ToSchema)]
pub(crate) struct AuthenticationCapabilities {
    pub local_login_enabled: bool,
    pub oidc_login_enabled: bool,
}

#[utoipa::path(
    get,
    path = "/api/v3/auth/capabilities",
    tag = "sessions",
    responses(
        (status = 200, description = "Public authentication capabilities", body = AuthenticationCapabilities),
        (status = 503, description = "PostgreSQL unavailable", body = Problem, content_type = "application/problem+json")
    ),
    security())]
pub(crate) async fn authentication_capabilities(
    State(state): State<ManagementState>,
) -> Result<Response, Problem> {
    let oidc_login_enabled = crate::access::oidc::repository::configuration::oidc_configuration(
        &state.request_boundary.pool,
    )
    .await
    .map_err(crate::access::oidc::http::error::map_oidc)?
    .is_some_and(|configuration| configuration.enabled);
    let mut response = Json(AuthenticationCapabilities {
        local_login_enabled: state.local_login_enabled,
        oidc_login_enabled,
    })
    .into_response();
    prevent_sensitive_response_caching(&mut response);
    Ok(response)
}

/// Unauthenticated first-run probe. It deliberately carries nothing but the
/// boolean: the installation name is an authenticated detail the console reads
/// from `SessionResponse`.
#[derive(Debug, Serialize, ToSchema)]
pub(crate) struct SetupStatus {
    pub setup_required: bool,
}

#[utoipa::path(
    get,
    path = "/api/v3/setup/status",
    tag = "setup",
    responses(
        (status = 200, description = "Installation setup state", body = SetupStatus),
        (status = 503, description = "PostgreSQL unavailable", body = Problem, content_type = "application/problem+json")
    ),
    security())]
pub(crate) async fn setup_status(
    State(state): State<ManagementState>,
) -> Result<Json<SetupStatus>, Problem> {
    let setup_required =
        crate::access::identity::setup::setup_required(&state.request_boundary.pool)
            .await
            .map_err(map_persistence)?;
    Ok(Json(SetupStatus { setup_required }))
}

#[derive(Deserialize, ToSchema)]
pub(crate) struct SetupRequest {
    pub email: String,
    #[schema(value_type = String, write_only)]
    pub(crate) password: WriteOnlySecret,
    pub display_name: String,
    #[serde(default = "default_installation_name")]
    pub installation_name: String,
}

impl fmt::Debug for SetupRequest {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("SetupRequest")
            .field("email", &self.email)
            .field("password", &"[REDACTED]")
            .field("display_name", &self.display_name)
            .field("installation_name", &self.installation_name)
            .finish()
    }
}

fn default_installation_name() -> String {
    "OpenLLMProxy".to_owned()
}

#[derive(Debug, Serialize, ToSchema)]
pub(crate) struct UserResponse {
    #[schema(value_type = String, format = Uuid)]
    pub id: Uuid,
    pub email: String,
    pub display_name: String,
    pub role: String,
}

#[derive(Serialize, ToSchema)]
pub(crate) struct SessionResponse {
    pub user: UserResponse,
    pub installation_name: String,
    #[schema(value_type = String)]
    csrf_token: WriteOnlySecret,
}

impl fmt::Debug for SessionResponse {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("SessionResponse")
            .field("user", &self.user)
            .field("installation_name", &self.installation_name)
            .field("csrf_token", &"[REDACTED]")
            .finish()
    }
}

#[utoipa::path(
    post,
    path = "/api/v3/setup",
    tag = "setup",
    params(
        ("X-OLP-Setup-Token" = String, Header, description = "One-time bootstrap token from OLP_BOOTSTRAP_TOKEN_FILE")
    ),
    request_body = SetupRequest,
    responses(
        (status = 201, description = "Owner and session created", body = SessionResponse),
        (status = 409, description = "Setup already completed", body = Problem, content_type = "application/problem+json"),
        (status = 429, description = "Password work is rate limited", body = Problem, content_type = "application/problem+json"),
        (status = 422, description = "Validation failed", body = Problem, content_type = "application/problem+json"),
        (status = 503, description = "PostgreSQL unavailable", body = Problem, content_type = "application/problem+json")
    ),
    security(("bootstrapSetupToken" = [])))]
pub(crate) async fn setup(
    State(state): State<ManagementState>,
    Provenance(provenance): Provenance,
    Extension(FirstOwnerSetupAuthorized): Extension<FirstOwnerSetupAuthorized>,
    payload: Result<Json<SetupRequest>, JsonRejection>,
) -> Result<Response, Problem> {
    let pool = &state.request_boundary.pool;
    validate_session_cookie_ttl(state.session_ttl)?;
    let request = json_payload(payload)?;
    validate_setup(&request)?;
    let password = Zeroizing::new(request.password.expose().to_owned());
    let password_hash = spawn_password_work(move || hash(&password))?
        .await
        .map_err(|error| {
            error!(%error, "password hashing task failed");
            Problem::internal()
        })?
        .map_err(|error| {
            error!(%error, "password hashing failed");
            Problem::internal()
        })?;

    let material = SessionMaterial::generate();
    let installation_name = request.installation_name.trim().to_owned();
    let (owner, _) = crate::access::identity::setup::setup_installation_with_session(
        pool,
        &provenance,
        InstallationSetupInput {
            installation_name: request.installation_name,
            email: request.email,
            display_name: request.display_name,
            password_hash,
        },
        &material,
        state.session_ttl,
    )
    .await
    .map_err(|error| match error {
        crate::database::error::Error::AlreadySetup => Problem::conflict(
            "setup_already_completed",
            "This installation already has an owner.",
        ),
        other => map_persistence(other),
    })?;
    state.clear_bootstrap_token().await;
    session_response(
        StatusCode::CREATED,
        &material,
        UserResponse {
            id: owner.user_id,
            email: owner.email,
            display_name: owner.display_name,
            role: "owner".to_owned(),
        },
        installation_name,
        state.session_ttl,
    )
}

#[derive(Deserialize, ToSchema)]
pub(crate) struct LoginRequest {
    pub email: String,
    #[schema(value_type = String, write_only)]
    pub(crate) password: WriteOnlySecret,
}

impl fmt::Debug for LoginRequest {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("LoginRequest")
            .field("email", &self.email)
            .field("password", &"[REDACTED]")
            .finish()
    }
}

#[utoipa::path(
    post,
    path = "/api/v3/sessions",
    tag = "sessions",
    request_body = LoginRequest,
    responses(
        (status = 201, description = "Session created", body = SessionResponse),
        (status = 401, description = "Invalid credentials", body = Problem, content_type = "application/problem+json"),
        (status = 404, description = "Local password sign-in is disabled", body = Problem, content_type = "application/problem+json"),
        (status = 429, description = "Authentication work is rate limited", body = Problem, content_type = "application/problem+json"),
        (status = 422, description = "Validation failed", body = Problem, content_type = "application/problem+json")
    ),
    security())]
pub(crate) async fn login(
    State(state): State<ManagementState>,
    Provenance(provenance): Provenance,
    connect_info: Option<Extension<ConnectInfo<SocketAddr>>>,
    headers: HeaderMap,
    payload: Result<Json<LoginRequest>, JsonRejection>,
) -> Result<Response, Problem> {
    if !state.local_login_enabled {
        return Err(Problem::new(
            StatusCode::NOT_FOUND,
            "local_login_disabled",
            "Local sign-in disabled",
            "Password-based local sign-in is disabled for this installation.",
        ));
    }
    enforce_origin(&state.public_origin, &headers)?;
    let request = json_payload(payload)?;
    validate_session_cookie_ttl(state.session_ttl)?;
    let pool = &state.request_boundary.pool;
    // Admit every syntactically decoded login attempt before the inexpensive
    // validation branch below. Otherwise an attacker can rotate oversized
    // credentials to bypass the per-source budget while creating unbounded
    // failure-audit rows. Invalid targets are intentionally reduced to a
    // bounded source-local sentinel; valid email targets retain the
    // source-plus-target brute-force ceiling.
    let rate_limit_target = local_login_rate_limit_target(&request.email);
    let (source_digest, source_target_digest) = public_auth_source_target_digests(
        &state.request_boundary,
        &headers,
        connect_info.map(|Extension(ConnectInfo(peer))| peer),
        &rate_limit_target,
    )?;
    if !crate::access::identity::auth_admission::admit_local_login_attempt(
        pool,
        source_digest,
        source_target_digest,
    )
    .await
    .map_err(map_identity)?
    {
        return Err(public_auth_rate_limited());
    }
    if request.email.len() > 254 || request.password.expose().chars().count() > 1_024 {
        crate::access::authentication::sessions::record_local_login_failure(
            pool,
            &provenance,
            None,
        )
        .await
        .map_err(map_persistence)?;
        return Err(Problem::unauthorized("The email or password is incorrect."));
    }
    let user = crate::access::authentication::sessions::local_password_user(pool, &request.email)
        .await
        .map_err(map_persistence)?;
    let failure_actor = user.as_ref().map(|user| user.id);
    let password = Zeroizing::new(request.password.expose().to_owned());
    let encoded = user.as_ref().map(|user| user.password_hash.clone());
    let valid = verify_local_password(password, encoded).await?;
    let Some(user) = user.filter(|_| valid) else {
        crate::access::authentication::sessions::record_local_login_failure(
            pool,
            &provenance,
            failure_actor,
        )
        .await
        .map_err(map_persistence)?;
        return Err(Problem::unauthorized("The email or password is incorrect."));
    };

    let material = SessionMaterial::generate();
    crate::access::authentication::sessions::create_session(
        pool,
        &provenance,
        user.id,
        user.security_version,
        &material,
        state.session_ttl,
    )
    .await
    .map_err(map_persistence)?;
    session_response(
        StatusCode::CREATED,
        &material,
        UserResponse {
            id: user.id,
            email: user.email,
            display_name: user.display_name,
            role: user.role,
        },
        installation_name(pool).await?,
        state.session_ttl,
    )
}

async fn verify_local_password(
    password: Zeroizing<String>,
    encoded: Option<String>,
) -> Result<bool, Problem> {
    // Unknown accounts also incur Argon2 work to avoid revealing account existence.
    spawn_password_work(move || match encoded {
        Some(encoded) => verify(&password, &encoded),
        None => {
            let _ = hash(&password);
            false
        }
    })?
    .await
    .map_err(|error| {
        error!(%error, "password verification task failed");
        Problem::internal()
    })
}

#[utoipa::path(
    get,
    path = "/api/v3/sessions/current",
    tag = "sessions",
    responses(
        (status = 200, description = "Current session", body = SessionResponse),
        (status = 401, description = "No active session", body = Problem, content_type = "application/problem+json"),
        (status = 409, description = "Another request recovered the session CSRF credential", body = Problem, content_type = "application/problem+json")
    ),
    security(("sessionCookie" = [])))]
pub(crate) async fn current_session(
    State(state): State<ManagementState>,
    Provenance(provenance): Provenance,
    headers: HeaderMap,
    ReadPrincipal(principal): ReadPrincipal,
) -> Result<Response, Problem> {
    let supplied_csrf = cookie(&headers, CSRF_COOKIE)?
        .filter(|csrf| SessionMaterial::verify_csrf(csrf, &principal.csrf_digest));
    let replacement = supplied_csrf.is_none().then(CsrfMaterial::generate);
    let remaining = principal.expires_at - chrono::Utc::now();
    if replacement.is_some() && validate_session_cookie_ttl(remaining).is_err() {
        // This request was authenticated with the session that arrived in its
        // Cookie header, but a concurrent login or security transition can
        // replace the browser's credentials before this response arrives.
        // Never expire browser-wide cookie names from this recovery path: a
        // delayed S1 response must not erase a newer S2 session.
        let mut response =
            Problem::unauthorized("The session is too close to expiry to recover.").into_response();
        prevent_sensitive_response_caching(&mut response);
        return Ok(response);
    }
    if let Some(replacement) = replacement.as_ref() {
        let rotated = crate::access::authentication::sessions::rotate_session_csrf(
            &state.request_boundary.pool,
            &provenance,
            principal.session_id,
            principal.user_id,
            principal.security_version,
            &principal.csrf_digest,
            replacement,
        )
        .await
        .map_err(map_persistence)?;
        if !rotated {
            let session_is_current = match require_read_session(&state, &headers).await {
                Ok(_) => true,
                Err(problem) if problem.status == StatusCode::UNAUTHORIZED.as_u16() => false,
                Err(problem) => return Err(problem),
            };
            return Ok(csrf_recovery_cas_failure_response(session_is_current));
        }
    }
    let csrf_token = supplied_csrf
        .map(str::to_owned)
        .or_else(|| {
            replacement
                .as_ref()
                .map(|material| material.token().to_owned())
        })
        .ok_or_else(Problem::internal)?;
    let mut response = Json(SessionResponse {
        user: UserResponse {
            id: principal.user_id,
            email: principal.email,
            display_name: principal.display_name,
            role: principal.role,
        },
        installation_name: installation_name(&state.request_boundary.pool).await?,
        csrf_token: WriteOnlySecret(csrf_token),
    })
    .into_response();
    // Do not write a browser-wide CSRF cookie while recovering an older
    // request. A later security transition can install a new session between
    // the CAS above and response delivery, and a delayed recovery response
    // would otherwise overwrite that new session's CSRF cookie. The returned
    // token is used by the currently running console; a fresh page load can
    // recover again if the browser has no matching CSRF cookie.
    prevent_sensitive_response_caching(&mut response);
    Ok(response)
}

pub(crate) fn csrf_recovery_cas_failure_response(session_is_current: bool) -> Response {
    let mut response = if session_is_current {
        Problem::conflict(
            "csrf_recovery_in_progress",
            "Another request recovered this session's CSRF credential. Retry with the current browser credentials.",
        )
        .into_response()
    } else {
        Problem::unauthorized("The session changed while its CSRF credential was being recovered.")
            .into_response()
    };
    prevent_sensitive_response_caching(&mut response);
    response
}

#[utoipa::path(
    delete,
    path = "/api/v3/sessions/current",
    tag = "sessions",
    responses(
        (status = 204, description = "Session ended and browser credentials expired"),
        (status = 403, description = "Origin check failed", body = Problem, content_type = "application/problem+json")
    ),
    security(("sessionCookie" = [], "csrfToken" = [])))]
pub(crate) async fn logout(
    State(state): State<ManagementState>,
    Provenance(provenance): Provenance,
    headers: HeaderMap,
) -> Result<Response, Problem> {
    enforce_origin(&state.public_origin, &headers)?;
    let parsed_token = cookie(&headers, SESSION_COOKIE);
    let mut response = match parsed_token {
        Ok(token) => {
            if let Some(token) = token
                && let Err(error) =
                    crate::access::authentication::sessions::revoke_session_by_token(
                        &state.request_boundary.pool,
                        &provenance,
                        token,
                    )
                    .await
            {
                // Logout is intentionally idempotent and fail-closed in the browser.
                // A transient database failure must not prevent credential expiry.
                warn!(%error, "server-side logout revocation failed");
            }
            StatusCode::NO_CONTENT.into_response()
        }
        Err(problem) => problem.into_response(),
    };
    expire_session_cookies(&mut response);
    prevent_sensitive_response_caching(&mut response);
    Ok(response)
}

/// Reads the installation name every authenticated response carries. A live
/// session implies the installation row exists.
pub(crate) async fn installation_name(pool: &PgPool) -> Result<String, Problem> {
    crate::access::identity::installation::installation_name(pool)
        .await
        .map_err(map_persistence)?
        .ok_or_else(Problem::internal)
}

pub(crate) fn session_response(
    status: StatusCode,
    material: &SessionMaterial,
    user: UserResponse,
    installation_name: String,
    session_ttl: chrono::Duration,
) -> Result<Response, Problem> {
    let mut response = (
        status,
        Json(SessionResponse {
            user,
            installation_name,
            csrf_token: WriteOnlySecret(material.csrf_token().to_owned()),
        }),
    )
        .into_response();
    append_session_cookies(&mut response, material, session_ttl)?;
    clear_recent_auth_cookie(&mut response);
    prevent_sensitive_response_caching(&mut response);
    Ok(response)
}

pub(crate) fn validate_setup(request: &SetupRequest) -> Result<(), Problem> {
    let mut errors: FieldErrors = BTreeMap::new();
    let email = request.email.trim();
    if email.len() > 254 || !email.contains('@') || email.starts_with('@') || email.ends_with('@') {
        errors
            .entry("email".to_owned())
            .or_default()
            .push("Enter a valid email address.".to_owned().into());
    }
    if !(12..=1_024).contains(&request.password.expose().chars().count()) {
        errors
            .entry("password".to_owned())
            .or_default()
            .push("Use between 12 and 1,024 characters.".to_owned().into());
    }
    if request.display_name.trim().is_empty() || request.display_name.chars().count() > 100 {
        errors
            .entry("display_name".to_owned())
            .or_default()
            .push("Use between 1 and 100 characters.".to_owned().into());
    }
    if request.installation_name.trim().is_empty()
        || request.installation_name.chars().count() > 100
    {
        errors
            .entry("installation_name".to_owned())
            .or_default()
            .push("Use between 1 and 100 characters.".to_owned().into());
    }
    if errors.is_empty() {
        Ok(())
    } else {
        Err(Problem::validation(errors))
    }
}

/// Returns a bounded, normalized identity for local-login rate admission.
/// Deliberately malformed/oversized emails do not need their own target
/// buckets, but must still consume the caller's source quota.
pub(crate) fn local_login_rate_limit_target(email: &str) -> String {
    if email.len() > 254 {
        INVALID_LOGIN_RATE_LIMIT_TARGET.to_owned()
    } else {
        email.trim().to_lowercase()
    }
}

pub(crate) fn acquire_password_work() -> Result<SemaphorePermit<'static>, Problem> {
    PASSWORD_WORK
        .try_acquire()
        .map_err(|_| public_auth_rate_limited())
}

pub(crate) fn spawn_password_work<T>(
    work: impl FnOnce() -> T + Send + 'static,
) -> Result<tokio::task::JoinHandle<T>, Problem>
where
    T: Send + 'static,
{
    let permit = acquire_password_work()?;
    Ok(tokio::task::spawn_blocking(move || {
        let _permit = permit;
        work()
    }))
}

pub(crate) fn public_auth_rate_limited() -> Problem {
    Problem::new(
        StatusCode::TOO_MANY_REQUESTS,
        "authentication_rate_limited",
        "Too many authentication attempts",
        "Too many authentication attempts are in progress. Wait before retrying.",
    )
}
