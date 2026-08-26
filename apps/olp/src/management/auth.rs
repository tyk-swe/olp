use std::{collections::BTreeMap, fmt, net::SocketAddr, sync::LazyLock};

use axum::{
    Json,
    extract::{ConnectInfo, Extension, State, rejection::JsonRejection},
    http::{HeaderMap, StatusCode},
    response::{IntoResponse, Response},
};
use olp_db::{
    identity::InstallationSetupInput, security::password::hash, security::password::verify,
    security::session_material::CsrfMaterial, security::session_material::SessionMaterial,
    store::Store,
};
use serde::{Deserialize, Serialize};
use tokio::sync::{Semaphore, SemaphorePermit};
use tracing::{error, warn};
use utoipa::ToSchema;
use uuid::Uuid;
use zeroize::Zeroizing;

use super::{
    cookies::{
        append_session_cookies, clear_recent_auth_cookie, expire_session_cookies,
        validate_session_cookie_ttl,
    },
    error_mapping::{map_identity, map_persistence},
    json_payload::json_payload,
    response_policy::prevent_sensitive_response_caching,
    secrets::WriteOnlySecret,
    sessions::{cookie, enforce_origin, require_read_session},
};
use crate::{
    bootstrap::mode_dependencies::ManagementState,
    management::provenance::Provenance,
    public_http::problem::FieldErrors,
    public_http::problem::Problem,
    public_http::proxy::public_auth_source_target_digests,
    public_http::request_admission::FirstOwnerSetupAuthorized,
    public_http::request_cookies::{CSRF_COOKIE, SESSION_COOKIE},
};

pub(super) const INVALID_LOGIN_RATE_LIMIT_TARGET: &str = "<invalid-local-login-target>";
static PASSWORD_WORK: LazyLock<Semaphore> =
    LazyLock::new(|| Semaphore::new(password_work_concurrency()));

pub(super) fn password_work_concurrency() -> usize {
    // The upper bound caps memory pinned by unauthenticated Argon2 hashing
    // (each permit holds the full Argon2 working set); scaling with cores
    // must not turn many-core hosts into a pre-auth memory-exhaustion vector.
    std::thread::available_parallelism()
        .map(|parallelism| parallelism.get().div_ceil(2))
        .unwrap_or(4)
        .clamp(4, 8)
}

#[derive(Debug, Serialize, ToSchema)]
pub(super) struct AuthenticationCapabilities {
    pub local_login_enabled: bool,
    pub oidc_login_enabled: bool,
}

#[utoipa::path(
    get,
    path = "/api/v1/auth/capabilities",
    tag = "sessions",
    responses(
        (status = 200, description = "Public authentication capabilities", body = AuthenticationCapabilities),
        (status = 503, description = "PostgreSQL unavailable", body = Problem)
    )
)]
pub(super) async fn authentication_capabilities(
    State(state): State<ManagementState>,
) -> Result<Response, Problem> {
    let oidc_login_enabled = state
        .store()
        .oidc_configuration()
        .await
        .map_err(crate::management::oidc::error::map_oidc)?
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
pub(super) struct SetupStatus {
    pub setup_required: bool,
}

#[utoipa::path(
    get,
    path = "/api/v1/setup/status",
    tag = "setup",
    responses(
        (status = 200, description = "Installation setup state", body = SetupStatus),
        (status = 503, description = "PostgreSQL unavailable", body = Problem)
    )
)]
pub(super) async fn setup_status(
    State(state): State<ManagementState>,
) -> Result<Json<SetupStatus>, Problem> {
    let setup_required = state
        .store()
        .setup_required()
        .await
        .map_err(map_persistence)?;
    Ok(Json(SetupStatus { setup_required }))
}

#[derive(Deserialize, ToSchema)]
pub(super) struct SetupRequest {
    pub email: String,
    #[schema(value_type = String, write_only)]
    pub(super) password: WriteOnlySecret,
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
pub(super) struct UserResponse {
    #[schema(value_type = String, format = Uuid)]
    pub id: Uuid,
    pub email: String,
    pub display_name: String,
    pub role: String,
}

#[derive(Serialize, ToSchema)]
pub(super) struct SessionResponse {
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
    path = "/api/v1/setup",
    tag = "setup",
    params(
        ("X-OLP-Setup-Token" = String, Header, description = "One-time bootstrap token from OLP_BOOTSTRAP_TOKEN_FILE")
    ),
    request_body = SetupRequest,
    responses(
        (status = 201, description = "Owner and session created", body = SessionResponse),
        (status = 409, description = "Setup already completed", body = Problem),
        (status = 429, description = "Password work is rate limited", body = Problem),
        (status = 422, description = "Validation failed", body = Problem),
        (status = 503, description = "PostgreSQL unavailable", body = Problem)
    )
)]
pub(super) async fn setup(
    State(state): State<ManagementState>,
    Provenance(provenance): Provenance,
    Extension(FirstOwnerSetupAuthorized): Extension<FirstOwnerSetupAuthorized>,
    payload: Result<Json<SetupRequest>, JsonRejection>,
) -> Result<Response, Problem> {
    let store = state.store().with_provenance(&provenance);
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
    let (owner, _) = store
        .setup_installation_with_session(
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
            olp_db::error::Error::AlreadySetup => Problem::conflict(
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
pub(super) struct LoginRequest {
    pub email: String,
    #[schema(value_type = String, write_only)]
    pub(super) password: WriteOnlySecret,
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
    path = "/api/v1/sessions",
    tag = "sessions",
    request_body = LoginRequest,
    responses(
        (status = 201, description = "Session created", body = SessionResponse),
        (status = 401, description = "Invalid credentials", body = Problem),
        (status = 404, description = "Local password sign-in is disabled", body = Problem),
        (status = 429, description = "Authentication work is rate limited", body = Problem),
        (status = 422, description = "Validation failed", body = Problem)
    )
)]
pub(super) async fn login(
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
    let store = state.store().with_provenance(&provenance);
    // Admit every syntactically decoded login attempt before the inexpensive
    // validation branch below. Otherwise an attacker can rotate oversized
    // credentials to bypass the per-source budget while creating unbounded
    // failure-audit rows. Invalid targets are intentionally reduced to a
    // bounded source-local sentinel; valid email targets retain the
    // source-plus-target brute-force ceiling.
    let rate_limit_target = local_login_rate_limit_target(&request.email);
    let (source_digest, source_target_digest) = public_auth_source_target_digests(
        state.request_boundary(),
        &headers,
        connect_info.map(|Extension(ConnectInfo(peer))| peer),
        &rate_limit_target,
    )?;
    if !store
        .admit_local_login_attempt(source_digest, source_target_digest)
        .await
        .map_err(map_identity)?
    {
        return Err(public_auth_rate_limited());
    }
    if request.email.len() > 254 || request.password.expose().chars().count() > 1_024 {
        store
            .record_local_login_failure(None)
            .await
            .map_err(map_persistence)?;
        return Err(Problem::unauthorized("The email or password is incorrect."));
    }
    let user = store
        .local_password_user(&request.email)
        .await
        .map_err(map_persistence)?;
    let failure_actor = user.as_ref().map(|user| user.id);
    let password = Zeroizing::new(request.password.expose().to_owned());
    let encoded = user.as_ref().map(|user| user.password_hash.clone());
    // Perform an Argon2id operation even for an unknown account so account
    // existence is not exposed through a cheap timing distinction.
    let valid = spawn_password_work(move || match encoded {
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
    })?;
    let Some(user) = user.filter(|_| valid) else {
        store
            .record_local_login_failure(failure_actor)
            .await
            .map_err(map_persistence)?;
        return Err(Problem::unauthorized("The email or password is incorrect."));
    };

    let material = SessionMaterial::generate();
    store
        .create_session(user.id, &material, state.session_ttl)
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
        installation_name(&store).await?,
        state.session_ttl,
    )
}

#[utoipa::path(
    get,
    path = "/api/v1/sessions/current",
    tag = "sessions",
    responses(
        (status = 200, description = "Current session", body = SessionResponse),
        (status = 401, description = "No active session", body = Problem),
        (status = 409, description = "Another request recovered the session CSRF credential", body = Problem)
    )
)]
pub(super) async fn current_session(
    State(state): State<ManagementState>,
    Provenance(provenance): Provenance,
    headers: HeaderMap,
) -> Result<Response, Problem> {
    let principal = require_read_session(&state, &headers).await?;
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
        let rotated = state
            .store()
            .with_provenance(&provenance)
            .rotate_session_csrf(
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
        installation_name: installation_name(state.store()).await?,
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

pub(super) fn csrf_recovery_cas_failure_response(session_is_current: bool) -> Response {
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
    path = "/api/v1/sessions/current",
    tag = "sessions",
    responses(
        (status = 204, description = "Session ended and browser credentials expired"),
        (status = 403, description = "Origin check failed", body = Problem)
    )
)]
pub(super) async fn logout(
    State(state): State<ManagementState>,
    Provenance(provenance): Provenance,
    headers: HeaderMap,
) -> Result<Response, Problem> {
    enforce_origin(&state.public_origin, &headers)?;
    let parsed_token = cookie(&headers, SESSION_COOKIE);
    let mut response = match parsed_token {
        Ok(token) => {
            if let Some(token) = token
                && let Err(error) = state
                    .store()
                    .with_provenance(&provenance)
                    .revoke_session_by_token(token)
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
pub(super) async fn installation_name(store: &Store) -> Result<String, Problem> {
    store
        .installation_name()
        .await
        .map_err(map_persistence)?
        .ok_or_else(Problem::internal)
}

pub(super) fn session_response(
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

pub(super) fn validate_setup(request: &SetupRequest) -> Result<(), Problem> {
    let mut errors: FieldErrors = BTreeMap::new();
    let email = request.email.trim();
    if email.len() > 254 || !email.contains('@') || email.starts_with('@') || email.ends_with('@') {
        errors
            .entry("email".to_owned())
            .or_default()
            .push("Enter a valid email address.".to_owned());
    }
    if !(12..=1_024).contains(&request.password.expose().chars().count()) {
        errors
            .entry("password".to_owned())
            .or_default()
            .push("Use between 12 and 1,024 characters.".to_owned());
    }
    if request.display_name.trim().is_empty() || request.display_name.chars().count() > 100 {
        errors
            .entry("display_name".to_owned())
            .or_default()
            .push("Use between 1 and 100 characters.".to_owned());
    }
    if request.installation_name.trim().is_empty()
        || request.installation_name.chars().count() > 100
    {
        errors
            .entry("installation_name".to_owned())
            .or_default()
            .push("Use between 1 and 100 characters.".to_owned());
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
pub(super) fn local_login_rate_limit_target(email: &str) -> String {
    if email.len() > 254 {
        INVALID_LOGIN_RATE_LIMIT_TARGET.to_owned()
    } else {
        email.trim().to_lowercase()
    }
}

pub(super) fn acquire_password_work() -> Result<SemaphorePermit<'static>, Problem> {
    PASSWORD_WORK
        .try_acquire()
        .map_err(|_| public_auth_rate_limited())
}

pub(super) fn spawn_password_work<T>(
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

pub(super) fn public_auth_rate_limited() -> Problem {
    Problem::new(
        StatusCode::TOO_MANY_REQUESTS,
        "authentication_rate_limited",
        "Too many authentication attempts",
        "Too many authentication attempts are in progress. Wait before retrying.",
    )
}
