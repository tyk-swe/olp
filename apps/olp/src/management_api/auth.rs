use std::{collections::BTreeMap, fmt, net::SocketAddr};

use axum::{
    Json,
    extract::{ConnectInfo, Extension, Path, Query, State, rejection::JsonRejection},
    http::{HeaderMap, HeaderValue, StatusCode, header},
    response::{IntoResponse, Response},
};
use olp_domain::Permission;
use olp_storage::{InstallationSetupInput, SessionMaterial, hash_password, verify_password};
use serde::{Deserialize, Serialize};
use tokio::sync::{Semaphore, SemaphorePermit};
use tracing::error;
use utoipa::ToSchema;
use uuid::Uuid;
use zeroize::Zeroizing;

use super::common::*;
use crate::{
    ApiState, FieldErrors, FirstOwnerSetupAuthorized, Problem, public_auth_source_target_digests,
    request_cookies::{CSRF_COOKIE, SESSION_COOKIE},
};

pub(super) const PASSWORD_WORK_CONCURRENCY: usize = 4;
pub(super) const INVALID_LOGIN_RATE_LIMIT_TARGET: &str = "<invalid-local-login-target>";
static PASSWORD_WORK: Semaphore = Semaphore::const_new(PASSWORD_WORK_CONCURRENCY);

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
    State(state): State<ApiState>,
) -> Result<Response, Problem> {
    let oidc_login_enabled = require_store(&state)?
        .oidc_configuration()
        .await
        .map_err(crate::oidc::map_oidc)?
        .is_some_and(|configuration| configuration.enabled);
    let mut response = Json(AuthenticationCapabilities {
        local_login_enabled: state.local_login_enabled,
        oidc_login_enabled,
    })
    .into_response();
    prevent_sensitive_response_caching(&mut response);
    Ok(response)
}

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
    State(state): State<ApiState>,
) -> Result<Json<SetupStatus>, Problem> {
    let store = require_store(&state)?;
    let setup_required = store.setup_required().await.map_err(map_persistence)?;
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
    #[schema(value_type = String)]
    csrf_token: WriteOnlySecret,
}

impl fmt::Debug for SessionResponse {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("SessionResponse")
            .field("user", &self.user)
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
    State(state): State<ApiState>,
    Extension(FirstOwnerSetupAuthorized): Extension<FirstOwnerSetupAuthorized>,
    payload: Result<Json<SetupRequest>, JsonRejection>,
) -> Result<Response, Problem> {
    let store = require_store(&state)?;
    let request = json_payload(payload)?;
    validate_setup(&request)?;
    let password = Zeroizing::new(request.password.expose().to_owned());
    let password_hash = spawn_password_work(move || hash_password(&password))?
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
            olp_storage::PersistenceError::AlreadySetup => Problem::conflict(
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
    State(state): State<ApiState>,
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
    enforce_origin(&state, &headers)?;
    let request = json_payload(payload)?;
    let store = require_store(&state)?;
    // Admit every syntactically decoded login attempt before the inexpensive
    // validation branch below. Otherwise an attacker can rotate oversized
    // credentials to bypass the per-source budget while creating unbounded
    // failure-audit rows. Invalid targets are intentionally reduced to a
    // bounded source-local sentinel; valid email targets retain the
    // source-plus-target brute-force ceiling.
    let rate_limit_target = local_login_rate_limit_target(&request.email);
    let (source_digest, source_target_digest) = public_auth_source_target_digests(
        &state,
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
        Some(encoded) => verify_password(&password, &encoded),
        None => {
            let _ = hash_password(&password);
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
    )
}

#[utoipa::path(
    get,
    path = "/api/v1/sessions/current",
    tag = "sessions",
    responses(
        (status = 200, description = "Current session", body = SessionResponse),
        (status = 401, description = "No active session", body = Problem)
    )
)]
pub(super) async fn current_session(
    State(state): State<ApiState>,
    headers: HeaderMap,
) -> Result<Response, Problem> {
    let principal = require_read_session(&state, &headers).await?;
    let csrf_token = cookie(&headers, CSRF_COOKIE)?
        .filter(|csrf| SessionMaterial::verify_csrf(csrf, &principal.csrf_digest))
        .unwrap_or_default()
        .to_owned();
    let mut response = Json(SessionResponse {
        user: UserResponse {
            id: principal.user_id,
            email: principal.email,
            display_name: principal.display_name,
            role: principal.role,
        },
        csrf_token: WriteOnlySecret(csrf_token),
    })
    .into_response();
    prevent_sensitive_response_caching(&mut response);
    Ok(response)
}

#[utoipa::path(
    delete,
    path = "/api/v1/sessions/current",
    tag = "sessions",
    responses(
        (status = 204, description = "Session ended"),
        (status = 401, description = "No active session", body = Problem),
        (status = 403, description = "CSRF or origin check failed", body = Problem)
    )
)]
pub(super) async fn logout(
    State(state): State<ApiState>,
    headers: HeaderMap,
) -> Result<Response, Problem> {
    let principal = require_mutation_session(&state, &headers).await?;
    require_store(&state)?
        .revoke_session(principal.session_id, principal.user_id, false)
        .await
        .map_err(map_identity)?;

    let mut response = StatusCode::NO_CONTENT.into_response();
    expire_session_cookies(&mut response);
    Ok(response)
}

#[derive(Debug, Deserialize)]
pub(super) struct SessionPageQuery {
    cursor: Option<String>,
    limit: Option<u16>,
    user_id: Option<Uuid>,
}

#[derive(Debug, Serialize, ToSchema)]
pub(super) struct SessionDetailResponse {
    #[schema(value_type = String, format = Uuid)]
    pub id: Uuid,
    #[schema(value_type = String, format = Uuid)]
    pub user_id: Uuid,
    pub current: bool,
    pub expires_at: chrono::DateTime<chrono::Utc>,
    pub last_seen_at: chrono::DateTime<chrono::Utc>,
    pub created_at: chrono::DateTime<chrono::Utc>,
}

#[derive(Debug, Serialize, ToSchema)]
pub(super) struct SessionListResponse {
    pub data: Vec<SessionDetailResponse>,
    pub next_cursor: Option<String>,
}

#[utoipa::path(
    get,
    path = "/api/v1/sessions",
    tag = "sessions",
    params(
        ("cursor" = Option<String>, Query, description = "Opaque cursor returned by the previous page"),
        ("limit" = Option<u16>, Query, description = "Page size from 1 to 100"),
        ("user_id" = Option<Uuid>, Query, description = "Owner-only user filter; defaults to the current user")
    ),
    responses(
        (status = 200, description = "Active and unexpired sessions", body = SessionListResponse),
        (status = 403, description = "Only owners can inspect another user's sessions", body = Problem)
    )
)]
pub(super) async fn list_sessions(
    State(state): State<ApiState>,
    headers: HeaderMap,
    Query(query): Query<SessionPageQuery>,
) -> Result<Json<SessionListResponse>, Problem> {
    let principal = require_read_session(&state, &headers).await?;
    let user_id = query.user_id.unwrap_or(principal.user_id);
    if user_id != principal.user_id {
        require_permission(&principal, Permission::ManageSessions)?;
    }
    let (cursor, limit) = page_parameters(PageQuery {
        cursor: query.cursor,
        limit: query.limit,
    })?;
    let (sessions, next_cursor) = require_store(&state)?
        .list_sessions(user_id, cursor, limit)
        .await
        .map_err(map_identity)?;
    Ok(Json(SessionListResponse {
        data: sessions
            .into_iter()
            .map(|session| SessionDetailResponse {
                id: session.id,
                user_id: session.user_id,
                current: session.id == principal.session_id,
                expires_at: session.expires_at,
                last_seen_at: session.last_seen_at,
                created_at: session.created_at,
            })
            .collect(),
        next_cursor: next_cursor.map(|cursor| cursor.to_string()),
    }))
}

#[utoipa::path(
    delete,
    path = "/api/v1/sessions/{session_id}",
    tag = "sessions",
    params(("session_id" = Uuid, Path, description = "Session ID")),
    responses(
        (status = 204, description = "Session revoked"),
        (status = 403, description = "Only owners can revoke another user's session", body = Problem),
        (status = 404, description = "Session not found", body = Problem)
    )
)]
pub(super) async fn revoke_session(
    State(state): State<ApiState>,
    Path(session_id): Path<Uuid>,
    headers: HeaderMap,
) -> Result<Response, Problem> {
    let principal = require_mutation_session(&state, &headers).await?;
    let can_manage_all = require_permission(&principal, Permission::ManageSessions).is_ok();
    require_store(&state)?
        .revoke_session(session_id, principal.user_id, can_manage_all)
        .await
        .map_err(map_identity)?;
    let mut response = StatusCode::NO_CONTENT.into_response();
    if session_id == principal.session_id {
        expire_session_cookies(&mut response);
    }
    Ok(response)
}

pub(super) fn session_response(
    status: StatusCode,
    material: &SessionMaterial,
    user: UserResponse,
) -> Result<Response, Problem> {
    let cookie = format!(
        "{SESSION_COOKIE}={}; Path=/; Max-Age=43200; Secure; HttpOnly; SameSite=Lax",
        material.token()
    );
    let mut response = (
        status,
        Json(SessionResponse {
            user,
            csrf_token: WriteOnlySecret(material.csrf_token().to_owned()),
        }),
    )
        .into_response();
    response.headers_mut().insert(
        header::SET_COOKIE,
        HeaderValue::from_str(&cookie).map_err(|_| Problem::internal())?,
    );
    let csrf_cookie = format!(
        "{CSRF_COOKIE}={}; Path=/; Max-Age=43200; Secure; SameSite=Lax",
        material.csrf_token()
    );
    response.headers_mut().append(
        header::SET_COOKIE,
        HeaderValue::from_str(&csrf_cookie).map_err(|_| Problem::internal())?,
    );
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
