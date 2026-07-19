use std::{
    collections::{BTreeMap, BTreeSet},
    fmt,
    net::SocketAddr,
    num::{NonZeroU32, NonZeroU64},
};

use axum::{
    Json, Router,
    body::Body,
    extract::{ConnectInfo, Extension, Path, Query, State, rejection::JsonRejection},
    http::{HeaderMap, HeaderValue, StatusCode, header},
    response::{IntoResponse, Response},
    routing::{get, post},
};
use chrono::{DateTime, Utc};
pub(crate) use olp_domain::Permission;
use olp_domain::{ApiKeyLimits, ApiKeyScope, OperationKind, ProviderKind, Role, RouteSlug};
use olp_providers::{ProviderConfig, ProviderCredential, ProviderError, ProviderFactory};
use olp_storage::{
    AcceptInvitation, AccessError, ConfigurationError, IdempotencyOutcome, IdempotencyResponse,
    InvitationRecord, NewApiKeyRecord, NewInvitation, NewOwner, NewProviderDraft, NewRouteDraft,
    NewRouteTarget, PersistenceError, PgStore, ReplayableIdempotency, SessionMaterial,
    SessionPrincipal, TeamError, UserRecord, credential_aad, hash_password,
    idempotency_fingerprint, idempotency_secret_digest, verify_password,
};
use serde::{Deserialize, Serialize, Serializer};
use tokio::sync::{Semaphore, SemaphorePermit};
use tracing::{error, warn};
use utoipa::{OpenApi, ToSchema};
use uuid::Uuid;
use zeroize::{Zeroize, Zeroizing};

use crate::{
    ApiState, FieldErrors, FirstOwnerSetupAuthorized, Problem, public_auth_source_target_digests,
};

const SESSION_COOKIE: &str = "__Host-olp_session";
const CSRF_COOKIE: &str = "__Host-olp_csrf";
pub(crate) const CSRF_HEADER: &str = "x-csrf-token";
pub(crate) const SETUP_TOKEN_HEADER: &str = "x-olp-setup-token";
const PASSWORD_WORK_CONCURRENCY: usize = 4;
const INVALID_LOGIN_RATE_LIMIT_TARGET: &str = "<invalid-local-login-target>";
const INVALID_INVITATION_RATE_LIMIT_TARGET: &str = "<invalid-invitation-token>";
static PASSWORD_WORK: Semaphore = Semaphore::const_new(PASSWORD_WORK_CONCURRENCY);

pub fn router() -> Router<ApiState> {
    Router::new()
        .route("/api/v1/openapi.json", get(openapi))
        .route("/api/v1/setup/status", get(setup_status))
        .route("/api/v1/setup", post(setup))
        .route("/api/v1/sessions", get(list_sessions).post(login))
        .route(
            "/api/v1/sessions/current",
            get(current_session).delete(logout),
        )
        .route(
            "/api/v1/sessions/{session_id}",
            axum::routing::delete(revoke_session),
        )
        .route("/api/v1/profile", get(profile).patch(update_profile))
        .route("/api/v1/profile/password", post(change_password))
        .route("/api/v1/profile/password/enroll", post(enroll_password))
        .route("/api/v1/users", get(list_users))
        .route(
            "/api/v1/users/{user_id}",
            get(get_user).patch(update_user_role),
        )
        .route(
            "/api/v1/invitations",
            get(list_invitations).post(create_invitation),
        )
        .route("/api/v1/invitations/accept", post(accept_invitation))
        .route(
            "/api/v1/invitations/{invitation_id}",
            axum::routing::delete(revoke_invitation),
        )
        .route("/api/v1/providers", post(create_provider))
        .route(
            "/api/v1/providers/{provider_id}/activate",
            post(activate_provider),
        )
        .route("/api/v1/api-keys", post(create_api_key))
        .route("/api/v1/api-keys/{api_key_id}/revoke", post(revoke_api_key))
        .route("/api/v1/route-drafts", post(create_route_draft))
        .route(
            "/api/v1/route-drafts/{draft_id}/validate",
            post(validate_route_draft),
        )
        .route(
            "/api/v1/route-drafts/{draft_id}/activate",
            post(activate_route_draft),
        )
}

#[derive(OpenApi)]
#[openapi(
    info(
        title = "OpenLLMProxy Management API",
        version = "1.0.0",
        description = "Management API for OpenLLMProxy."
    ),
    paths(
        setup_status,
        setup,
        login,
        current_session,
        logout,
        profile,
        update_profile,
        change_password,
        enroll_password,
        list_sessions,
        revoke_session,
        list_users,
        get_user,
        update_user_role,
        list_invitations,
        create_invitation,
        revoke_invitation,
        accept_invitation,
        create_provider,
        activate_provider,
        create_api_key,
        revoke_api_key,
        create_route_draft,
        validate_route_draft,
        activate_route_draft
    ),
    components(schemas(
        SetupStatus,
        SetupRequest,
        LoginRequest,
        SessionResponse,
        UpdateProfileRequest,
        ChangePasswordRequest,
        EnrollPasswordRequest,
        UserResponse,
        UserDetailResponse,
        UserListResponse,
        UpdateUserRoleRequest,
        InvitationResponse,
        InvitationListResponse,
        CreateInvitationRequest,
        CreateInvitationResponse,
        AcceptInvitationRequest,
        SessionDetailResponse,
        SessionListResponse,
        CreateProviderRequest,
        ProviderResponse,
        ProviderActivationResponse,
        CreateApiKeyRequest,
        CreateApiKeyResponse,
        RuntimeGenerationResponse,
        CreateRouteDraftRequest,
        RouteTargetRequest,
        RouteDraftResponse,
        RouteActivationResponse,
        Problem
    )),
    tags(
        (name = "setup"),
        (name = "sessions"),
        (name = "users"),
        (name = "invitations"),
        (name = "providers"),
        (name = "api-keys"),
        (name = "routes")
    )
)]
pub struct ManagementApiDoc;

async fn openapi() -> Json<serde_json::Value> {
    Json(management_openapi())
}

#[must_use]
pub fn management_openapi() -> serde_json::Value {
    let mut document = ManagementApiDoc::openapi();
    document.merge(crate::oidc::openapi());
    document.merge(crate::operations::OperationsApiDoc::openapi());
    document.merge(crate::catalog::CatalogApiDoc::openapi());
    document.merge(crate::playground::PlaygroundApiDoc::openapi());
    complete_openapi_contract(document)
}

fn complete_openapi_contract(document: utoipa::openapi::OpenApi) -> serde_json::Value {
    let mut value = serde_json::to_value(document).expect("generated OpenAPI is serializable");
    let components = value
        .get_mut("components")
        .and_then(serde_json::Value::as_object_mut)
        .expect("generated OpenAPI has components");
    components.insert(
        "securitySchemes".to_owned(),
        serde_json::json!({
            "sessionCookie": {
                "type": "apiKey",
                "in": "cookie",
                "name": SESSION_COOKIE,
                "description": "Opaque PostgreSQL-backed management session."
            },
            "csrfToken": {
                "type": "apiKey",
                "in": "header",
                "name": CSRF_HEADER,
                "description": "Double-submit CSRF token required with authenticated mutations."
            },
            "bootstrapSetupToken": {
                "type": "apiKey",
                "in": "header",
                "name": "X-OLP-Setup-Token",
                "description": "One-time bootstrap token required only while creating the first installation owner."
            }
        }),
    );

    let public_operations = [
        ("/api/v1/setup/status", "get"),
        ("/api/v1/setup", "post"),
        ("/api/v1/sessions", "post"),
        ("/api/v1/invitations/accept", "post"),
        ("/api/v1/oidc/login", "get"),
        ("/api/v1/oidc/callback", "get"),
    ];
    let paths = value
        .get_mut("paths")
        .and_then(serde_json::Value::as_object_mut)
        .expect("generated OpenAPI has paths");
    for (path, item) in paths {
        let Some(methods) = item.as_object_mut() else {
            continue;
        };
        for (method, operation) in methods {
            if !matches!(method.as_str(), "get" | "post" | "put" | "patch" | "delete") {
                continue;
            }
            let Some(operation) = operation.as_object_mut() else {
                continue;
            };
            let is_public = public_operations
                .iter()
                .any(|(public_path, public_method)| path == public_path && method == public_method);
            let is_bootstrap_setup = path == "/api/v1/setup" && method == "post";
            operation.insert(
                "security".to_owned(),
                if is_bootstrap_setup {
                    serde_json::json!([{ "bootstrapSetupToken": [] }])
                } else if is_public {
                    serde_json::json!([])
                } else if matches!(method.as_str(), "post" | "put" | "patch" | "delete") {
                    serde_json::json!([{ "sessionCookie": [], "csrfToken": [] }])
                } else {
                    serde_json::json!([{ "sessionCookie": [] }])
                },
            );

            let has_if_match = operation
                .get("parameters")
                .and_then(serde_json::Value::as_array)
                .is_some_and(|parameters| {
                    parameters.iter().any(|parameter| {
                        parameter.get("name").and_then(serde_json::Value::as_str)
                            == Some("If-Match")
                    })
                });
            if let Some(responses) = operation
                .get_mut("responses")
                .and_then(serde_json::Value::as_object_mut)
            {
                for (status, response) in responses.iter_mut() {
                    normalize_problem_content(response);
                    if has_if_match && status.starts_with('2') {
                        response
                            .as_object_mut()
                            .expect("OpenAPI response is an object")
                            .entry("headers")
                            .or_insert_with(|| serde_json::json!({}))
                            .as_object_mut()
                            .expect("OpenAPI response headers are an object")
                            .insert(
                                "ETag".to_owned(),
                                serde_json::json!({
                                    "description": "Current strong entity tag.",
                                    "schema": { "type": "string" }
                                }),
                            );
                    }
                }
                if !is_public {
                    responses
                        .entry("401")
                        .or_insert_with(|| problem_response("Authentication required."));
                    responses.entry("403").or_insert_with(|| {
                        problem_response(
                            "The session lacks permission or mutation CSRF/origin checks failed.",
                        )
                    });
                }
                responses
                    .entry("500")
                    .or_insert_with(|| problem_response("The request could not be completed."));
            }
        }
    }
    // Utoipa's typed OpenAPI model is intentionally narrower than OpenAPI
    // 3.1 in a few extension points (notably response-header schemas). The
    // generated contract is the JSON document served and drift-checked by OLP,
    // so retain the standards-compliant transformed value instead of trying to
    // deserialize it back through that lossy model.
    value
}

fn normalize_problem_content(response: &mut serde_json::Value) {
    let Some(content) = response
        .get_mut("content")
        .and_then(serde_json::Value::as_object_mut)
    else {
        return;
    };
    let is_problem = content.get("application/json").is_some_and(|media| {
        media
            .pointer("/schema/$ref")
            .and_then(serde_json::Value::as_str)
            .is_some_and(|reference| reference.ends_with("/Problem"))
    });
    if is_problem && let Some(media) = content.remove("application/json") {
        content.insert("application/problem+json".to_owned(), media);
    }
}

fn problem_response(description: &str) -> serde_json::Value {
    serde_json::json!({
        "description": description,
        "content": {
            "application/problem+json": {
                "schema": { "$ref": "#/components/schemas/Problem" }
            }
        }
    })
}

#[derive(Debug, Serialize, ToSchema)]
pub struct SetupStatus {
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
async fn setup_status(State(state): State<ApiState>) -> Result<Json<SetupStatus>, Problem> {
    let store = require_store(&state)?;
    let setup_required = store.setup_required().await.map_err(map_persistence)?;
    Ok(Json(SetupStatus { setup_required }))
}

#[derive(Deserialize, ToSchema)]
pub struct SetupRequest {
    pub email: String,
    #[schema(value_type = String, write_only)]
    password: WriteOnlySecret,
    pub display_name: String,
    #[serde(default = "default_organization")]
    pub organization_name: String,
}

impl fmt::Debug for SetupRequest {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("SetupRequest")
            .field("email", &self.email)
            .field("password", &"[REDACTED]")
            .field("display_name", &self.display_name)
            .field("organization_name", &self.organization_name)
            .finish()
    }
}

fn default_organization() -> String {
    "OpenLLMProxy".to_owned()
}

#[derive(Debug, Serialize, ToSchema)]
pub struct UserResponse {
    #[schema(value_type = String, format = Uuid)]
    pub id: Uuid,
    pub email: String,
    pub display_name: String,
    pub role: String,
}

#[derive(Serialize, ToSchema)]
pub struct SessionResponse {
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
async fn setup(
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
        .setup_owner_with_session(
            NewOwner {
                organization_name: request.organization_name,
                email: request.email,
                display_name: request.display_name,
                password_hash,
            },
            &material,
            state.session_ttl,
        )
        .await
        .map_err(|error| match error {
            PersistenceError::AlreadySetup => Problem::conflict(
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
pub struct LoginRequest {
    pub email: String,
    #[schema(value_type = String, write_only)]
    password: WriteOnlySecret,
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
        (status = 429, description = "Authentication work is rate limited", body = Problem),
        (status = 422, description = "Validation failed", body = Problem)
    )
)]
async fn login(
    State(state): State<ApiState>,
    connect_info: Option<Extension<ConnectInfo<SocketAddr>>>,
    headers: HeaderMap,
    payload: Result<Json<LoginRequest>, JsonRejection>,
) -> Result<Response, Problem> {
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
        .map_err(map_team)?
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
        .password_user(&request.email)
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
async fn current_session(
    State(state): State<ApiState>,
    headers: HeaderMap,
) -> Result<Response, Problem> {
    let principal = require_read_session(&state, &headers).await?;
    let csrf_token = cookie(&headers, CSRF_COOKIE)
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
async fn logout(State(state): State<ApiState>, headers: HeaderMap) -> Result<Response, Problem> {
    let principal = require_mutation_session(&state, &headers).await?;
    require_store(&state)?
        .revoke_session(principal.session_id, principal.user_id, false)
        .await
        .map_err(map_team)?;

    let mut response = StatusCode::NO_CONTENT.into_response();
    expire_session_cookies(&mut response);
    Ok(response)
}

#[utoipa::path(
    get,
    path = "/api/v1/profile",
    tag = "users",
    responses(
        (status = 200, description = "Current user profile", body = UserDetailResponse),
        (status = 401, description = "No active session", body = Problem)
    )
)]
async fn profile(State(state): State<ApiState>, headers: HeaderMap) -> Result<Response, Problem> {
    let principal = require_read_session(&state, &headers).await?;
    let user = require_store(&state)?
        .user(principal.user_id)
        .await
        .map_err(map_team)?
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
pub struct UpdateProfileRequest {
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
async fn update_profile(
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
        .map_err(map_team)?;
    let etag = user.etag;
    let mut response = Json(UserDetailResponse::from(user)).into_response();
    response.headers_mut().insert(
        header::ETAG,
        HeaderValue::from_str(&format!("\"{etag}\"")).map_err(|_| Problem::internal())?,
    );
    Ok(response)
}

#[derive(Deserialize, ToSchema)]
pub struct ChangePasswordRequest {
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
async fn change_password(
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
        .password_user(&principal.email)
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
        .map_err(map_team)?;
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
pub struct EnrollPasswordRequest {
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
async fn enroll_password(
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
        .map_err(map_team)?;
    let etag = user.etag;
    let mut response = Json(UserDetailResponse::from(user)).into_response();
    response.headers_mut().insert(
        header::ETAG,
        HeaderValue::from_str(&format!("\"{etag}\"")).map_err(|_| Problem::internal())?,
    );
    Ok(response)
}

#[derive(Debug, Deserialize)]
struct PageQuery {
    cursor: Option<String>,
    limit: Option<u16>,
}

#[derive(Debug, Serialize, ToSchema)]
pub struct UserDetailResponse {
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
pub struct UserListResponse {
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
        (status = 403, description = "Role cannot view the team", body = Problem)
    )
)]
async fn list_users(
    State(state): State<ApiState>,
    headers: HeaderMap,
    Query(query): Query<PageQuery>,
) -> Result<Json<UserListResponse>, Problem> {
    let principal = require_read_session(&state, &headers).await?;
    require_permission(&principal, Permission::ReadTeam)?;
    let (cursor, limit) = page_parameters(query)?;
    let (users, next_cursor) = require_store(&state)?
        .list_users(cursor, limit)
        .await
        .map_err(map_team)?;
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
async fn get_user(
    State(state): State<ApiState>,
    Path(user_id): Path<Uuid>,
    headers: HeaderMap,
) -> Result<Response, Problem> {
    let principal = require_read_session(&state, &headers).await?;
    require_permission(&principal, Permission::ReadTeam)?;
    let user = require_store(&state)?
        .user(user_id)
        .await
        .map_err(map_team)?
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
pub struct UpdateUserRoleRequest {
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
async fn update_user_role(
    State(state): State<ApiState>,
    Path(user_id): Path<Uuid>,
    headers: HeaderMap,
    payload: Result<Json<UpdateUserRoleRequest>, JsonRejection>,
) -> Result<Response, Problem> {
    let principal = require_mutation_session(&state, &headers).await?;
    require_permission(&principal, Permission::ManageTeam)?;
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
        .map_err(map_team)?;
    let etag = user.etag;
    let mut response = Json(UserDetailResponse::from(user)).into_response();
    response.headers_mut().insert(
        header::ETAG,
        HeaderValue::from_str(&format!("\"{etag}\"")).map_err(|_| Problem::internal())?,
    );
    Ok(response)
}

#[derive(Debug, Serialize, ToSchema)]
pub struct InvitationResponse {
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
pub struct InvitationListResponse {
    pub data: Vec<InvitationResponse>,
    pub next_cursor: Option<String>,
}

#[derive(Debug, Deserialize, Serialize, ToSchema)]
pub struct CreateInvitationRequest {
    pub email: String,
    pub role: String,
    /// Invitation lifetime in hours. Defaults to seven days and is capped at
    /// thirty days.
    pub expires_in_hours: Option<u16>,
}

#[derive(Debug, Serialize, ToSchema)]
pub struct CreateInvitationResponse {
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
async fn list_invitations(
    State(state): State<ApiState>,
    headers: HeaderMap,
    Query(query): Query<PageQuery>,
) -> Result<Json<InvitationListResponse>, Problem> {
    let principal = require_read_session(&state, &headers).await?;
    require_permission(&principal, Permission::ReadTeam)?;
    let (cursor, limit) = page_parameters(query)?;
    let (invitations, next_cursor) = require_store(&state)?
        .list_invitations(cursor, limit)
        .await
        .map_err(map_team)?;
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
async fn create_invitation(
    State(state): State<ApiState>,
    headers: HeaderMap,
    payload: Result<Json<CreateInvitationRequest>, JsonRejection>,
) -> Result<Response, Problem> {
    let principal = require_mutation_session(&state, &headers).await?;
    require_permission(&principal, Permission::ManageTeam)?;
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
        .map_err(map_team)?;
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
async fn revoke_invitation(
    State(state): State<ApiState>,
    Path(invitation_id): Path<Uuid>,
    headers: HeaderMap,
) -> Result<Json<InvitationResponse>, Problem> {
    let principal = require_mutation_session(&state, &headers).await?;
    require_permission(&principal, Permission::ManageTeam)?;
    let invitation = require_store(&state)?
        .revoke_invitation(
            invitation_id,
            principal.user_id,
            require_idempotency_key(&headers)?,
        )
        .await
        .map_err(map_team)?;
    Ok(Json(invitation.into()))
}

#[derive(Deserialize, ToSchema)]
pub struct AcceptInvitationRequest {
    #[schema(value_type = String, write_only)]
    token: WriteOnlySecret,
    pub display_name: String,
    #[schema(value_type = String, write_only)]
    password: WriteOnlySecret,
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
async fn accept_invitation(
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
        .map_err(map_team)?
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
        .map_err(map_team)?;
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

#[derive(Debug, Deserialize)]
struct SessionPageQuery {
    cursor: Option<String>,
    limit: Option<u16>,
    user_id: Option<Uuid>,
}

#[derive(Debug, Serialize, ToSchema)]
pub struct SessionDetailResponse {
    #[schema(value_type = String, format = Uuid)]
    pub id: Uuid,
    #[schema(value_type = String, format = Uuid)]
    pub user_id: Uuid,
    pub current: bool,
    pub expires_at: DateTime<Utc>,
    pub last_seen_at: DateTime<Utc>,
    pub created_at: DateTime<Utc>,
}

#[derive(Debug, Serialize, ToSchema)]
pub struct SessionListResponse {
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
async fn list_sessions(
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
        .map_err(map_team)?;
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
async fn revoke_session(
    State(state): State<ApiState>,
    Path(session_id): Path<Uuid>,
    headers: HeaderMap,
) -> Result<Response, Problem> {
    let principal = require_mutation_session(&state, &headers).await?;
    let can_manage_all = require_permission(&principal, Permission::ManageSessions).is_ok();
    require_store(&state)?
        .revoke_session(session_id, principal.user_id, can_manage_all)
        .await
        .map_err(map_team)?;
    let mut response = StatusCode::NO_CONTENT.into_response();
    if session_id == principal.session_id {
        expire_session_cookies(&mut response);
    }
    Ok(response)
}

#[derive(Deserialize)]
pub(crate) struct WriteOnlySecret(String);

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

#[derive(Debug, Deserialize, ToSchema)]
pub struct CreateProviderRequest {
    pub name: String,
    /// `open_ai` uses the official endpoint; `open_ai_compatible` requires an
    /// explicit HTTPS endpoint and live certification of reviewed capabilities.
    pub kind: String,
    pub endpoint: Option<String>,
    pub cloud_region: Option<String>,
    pub cloud_project: Option<String>,
    pub deployment: Option<String>,
    pub api_version: Option<String>,
    pub auth_mode: Option<String>,
    #[schema(value_type = String, write_only, required = false)]
    credential: Option<WriteOnlySecret>,
    #[serde(rename = "api_key")]
    #[schema(ignore)]
    legacy_api_key: Option<WriteOnlySecret>,
    /// Optional seed/probe model. Vertex AI requires one because its publisher
    /// model collection has no list operation; other connectors can discover
    /// models after the draft is created.
    pub model: Option<String>,
    pub display_name: Option<String>,
}

#[derive(Serialize)]
struct CreateProviderFingerprint<'a> {
    name: &'a str,
    kind: &'a str,
    endpoint: Option<&'a str>,
    cloud_region: Option<&'a str>,
    cloud_project: Option<&'a str>,
    deployment: Option<&'a str>,
    api_version: Option<&'a str>,
    auth_mode: Option<&'a str>,
    credential_sha256: Option<[u8; 32]>,
    model: Option<&'a str>,
    display_name: Option<&'a str>,
}

impl<'a> From<&'a CreateProviderRequest> for CreateProviderFingerprint<'a> {
    fn from(request: &'a CreateProviderRequest) -> Self {
        Self {
            name: &request.name,
            kind: &request.kind,
            endpoint: request.endpoint.as_deref(),
            cloud_region: request.cloud_region.as_deref(),
            cloud_project: request.cloud_project.as_deref(),
            deployment: request.deployment.as_deref(),
            api_version: request.api_version.as_deref(),
            auth_mode: request.auth_mode.as_deref(),
            credential_sha256: request
                .credential
                .as_ref()
                .map(|credential| idempotency_secret_digest(credential.expose().as_bytes())),
            model: request.model.as_deref(),
            display_name: request.display_name.as_deref(),
        }
    }
}

fn connector_validation(error: impl ToString) -> Problem {
    let mut errors = FieldErrors::new();
    errors.insert("endpoint".to_owned(), vec![error.to_string()]);
    Problem::validation(errors)
}

fn connector_credential_validation(error: impl ToString) -> Problem {
    let mut errors = FieldErrors::new();
    errors.insert("credential".to_owned(), vec![error.to_string()]);
    Problem::validation(errors)
}

fn bedrock_region_validation(error: impl ToString) -> Problem {
    let mut errors = FieldErrors::new();
    errors.insert("cloud_region".to_owned(), vec![error.to_string()]);
    Problem::validation(errors)
}

fn bedrock_credential_validation(error: impl ToString) -> Problem {
    let mut errors = FieldErrors::new();
    errors.insert("credential".to_owned(), vec![error.to_string()]);
    Problem::validation(errors)
}

fn provider_connector_validation(kind: ProviderKind, error: ProviderError) -> Problem {
    match error {
        ProviderError::Configuration(detail) if kind == ProviderKind::Bedrock => {
            bedrock_region_validation(detail)
        }
        ProviderError::Configuration(detail) => connector_validation(detail),
        ProviderError::Credential(detail) if kind == ProviderKind::Bedrock => {
            bedrock_credential_validation(detail)
        }
        ProviderError::Credential(detail) => connector_credential_validation(detail),
    }
}

fn reject_create_field(errors: &mut FieldErrors, field: &str, present: bool, detail: &str) {
    if present {
        errors
            .entry(field.to_owned())
            .or_default()
            .push(detail.to_owned());
    }
}

fn reject_create_cloud_fields(errors: &mut FieldErrors, request: &CreateProviderRequest) {
    for (field, present) in [
        ("cloud_region", request.cloud_region.is_some()),
        ("cloud_project", request.cloud_project.is_some()),
        ("deployment", request.deployment.is_some()),
        ("api_version", request.api_version.is_some()),
    ] {
        reject_create_field(
            errors,
            field,
            present,
            "This connector does not accept cloud project, region, deployment, or API-version fields.",
        );
    }
}

fn require_create_auth_mode(errors: &mut FieldErrors, actual: &str, expected: &str) {
    if actual != expected {
        errors
            .entry("auth_mode".to_owned())
            .or_default()
            .push(format!("Provider authentication must be {expected}."));
    }
}

#[derive(Debug, Serialize, ToSchema)]
pub struct ProviderResponse {
    #[schema(value_type = String, format = Uuid)]
    pub id: Uuid,
    pub name: String,
    pub kind: String,
    pub state: String,
    pub model: Option<String>,
    #[schema(value_type = String, format = Uuid)]
    pub etag: Uuid,
}

#[utoipa::path(
    post,
    path = "/api/v1/providers",
    tag = "providers",
    request_body = CreateProviderRequest,
    params(("Idempotency-Key" = String, Header, description = "Unique provider-draft creation key")),
    responses(
        (status = 201, description = "Provider draft created", body = ProviderResponse),
        (status = 400, description = "Idempotency-Key is missing or invalid", body = Problem),
        (status = 401, description = "No active session", body = Problem),
        (status = 403, description = "Insufficient role, CSRF, or origin failure", body = Problem),
        (status = 409, description = "Idempotency-Key was already used or is in progress", body = Problem),
        (status = 422, description = "Validation failed", body = Problem),
        (status = 503, description = "Master key or database unavailable", body = Problem)
    )
)]
async fn create_provider(
    State(state): State<ApiState>,
    headers: HeaderMap,
    payload: Result<Json<CreateProviderRequest>, JsonRejection>,
) -> Result<Response, Problem> {
    let principal = require_mutation_session(&state, &headers).await?;
    require_provider_manager(&principal)?;
    let idempotency_key = require_idempotency_key(&headers)?.to_owned();
    let request = json_payload(payload)?;
    let request_fingerprint = idempotency_fingerprint(&CreateProviderFingerprint::from(&request))
        .map_err(map_persistence)?;
    let master_key = state
        .master_key
        .as_deref()
        .ok_or_else(|| Problem::service_unavailable("master_key_not_configured"))?;
    let mut errors = FieldErrors::new();
    reject_create_field(
        &mut errors,
        "api_key",
        request.legacy_api_key.is_some(),
        "api_key is no longer accepted; use credential.",
    );
    if request.name.trim().is_empty() || request.name.chars().count() > 100 {
        errors
            .entry("name".to_owned())
            .or_default()
            .push("Use between 1 and 100 characters.".to_owned());
    }
    if request
        .model
        .as_ref()
        .is_some_and(|model| model.trim().is_empty() || model.chars().count() > 200)
    {
        errors
            .entry("model".to_owned())
            .or_default()
            .push("Use between 1 and 200 characters.".to_owned());
    }
    if request.model.is_none() && request.display_name.is_some() {
        errors
            .entry("display_name".to_owned())
            .or_default()
            .push("A display name requires a seed model.".to_owned());
    }
    if request.credential.as_ref().is_some_and(|credential| {
        credential.expose().trim().is_empty() || credential.expose().len() > 8_192
    }) {
        errors
            .entry("credential".to_owned())
            .or_default()
            .push("Provide a credential no larger than 8 KiB.".to_owned());
    }
    let (kind, base_url, surface) = match request.kind.as_str() {
        "open_ai" => (
            ProviderKind::OpenAi,
            request
                .endpoint
                .clone()
                .unwrap_or_else(|| "https://api.openai.com/v1/".to_owned()),
            Some("open_ai"),
        ),
        "open_ai_compatible" => {
            if let Some(endpoint) = request.endpoint.clone() {
                (ProviderKind::OpenAiCompatible, endpoint, Some("open_ai"))
            } else {
                errors
                    .entry("endpoint".to_owned())
                    .or_default()
                    .push("An HTTPS endpoint is required.".to_owned());
                (
                    ProviderKind::OpenAiCompatible,
                    String::new(),
                    Some("open_ai"),
                )
            }
        }
        "anthropic" => (
            ProviderKind::Anthropic,
            request
                .endpoint
                .clone()
                .unwrap_or_else(|| "https://api.anthropic.com/v1/".to_owned()),
            Some("anthropic"),
        ),
        "gemini" => (
            ProviderKind::Gemini,
            request
                .endpoint
                .clone()
                .unwrap_or_else(|| "https://generativelanguage.googleapis.com/v1beta/".to_owned()),
            Some("gemini"),
        ),
        "vertex_ai" => (
            ProviderKind::VertexAi,
            request.endpoint.clone().unwrap_or_default(),
            Some("gemini"),
        ),
        "azure_open_ai" => (
            ProviderKind::AzureOpenAi,
            request.endpoint.clone().unwrap_or_default(),
            Some("open_ai"),
        ),
        "bedrock" => (
            ProviderKind::Bedrock,
            request.endpoint.clone().unwrap_or_default(),
            None,
        ),
        _ => {
            errors
                .entry("kind".to_owned())
                .or_default()
                .push(
                    "Use open_ai, open_ai_compatible, anthropic, gemini, vertex_ai, azure_open_ai, or bedrock."
                        .to_owned(),
                );
            (ProviderKind::OpenAi, String::new(), Some("open_ai"))
        }
    };
    let auth_mode = request.auth_mode.clone().unwrap_or_else(|| match kind {
        ProviderKind::VertexAi => "adc".to_owned(),
        ProviderKind::Bedrock => "default_chain".to_owned(),
        _ => "api_key".to_owned(),
    });
    let credential_required = matches!(
        kind,
        ProviderKind::OpenAi
            | ProviderKind::OpenAiCompatible
            | ProviderKind::Anthropic
            | ProviderKind::Gemini
            | ProviderKind::AzureOpenAi
    ) || matches!(
        (kind, auth_mode.as_str()),
        (ProviderKind::VertexAi, "service_account") | (ProviderKind::Bedrock, "static")
    );
    if credential_required && request.credential.is_none() {
        errors
            .entry("credential".to_owned())
            .or_default()
            .push("This authentication mode requires a write-only credential.".to_owned());
    }
    match kind {
        ProviderKind::OpenAi => {
            require_create_auth_mode(&mut errors, &auth_mode, "api_key");
            reject_create_field(
                &mut errors,
                "endpoint",
                request.endpoint.is_some(),
                "Native OpenAI uses the official endpoint; use an OpenAI-compatible provider for a custom endpoint.",
            );
            reject_create_cloud_fields(&mut errors, &request);
        }
        ProviderKind::OpenAiCompatible => {
            require_create_auth_mode(&mut errors, &auth_mode, "api_key");
            reject_create_cloud_fields(&mut errors, &request);
        }
        ProviderKind::Anthropic => {
            require_create_auth_mode(&mut errors, &auth_mode, "api_key");
            reject_create_field(
                &mut errors,
                "endpoint",
                request.endpoint.is_some(),
                "Native Anthropic uses the official endpoint.",
            );
            reject_create_cloud_fields(&mut errors, &request);
        }
        ProviderKind::Gemini => {
            require_create_auth_mode(&mut errors, &auth_mode, "api_key");
            reject_create_field(
                &mut errors,
                "endpoint",
                request.endpoint.is_some(),
                "Gemini Developer API uses the official endpoint.",
            );
            reject_create_cloud_fields(&mut errors, &request);
        }
        ProviderKind::VertexAi => {
            if request.model.is_none() {
                errors
                    .entry("model".to_owned())
                    .or_default()
                    .push("Vertex AI requires an explicit model to probe.".to_owned());
            }
            if request.cloud_project.as_deref().is_none_or(str::is_empty) {
                errors
                    .entry("cloud_project".to_owned())
                    .or_default()
                    .push("Vertex AI requires a cloud project.".to_owned());
            }
            if request.cloud_region.as_deref().is_none_or(str::is_empty) {
                errors
                    .entry("cloud_region".to_owned())
                    .or_default()
                    .push("Vertex AI requires a cloud region.".to_owned());
            }
            if !matches!(auth_mode.as_str(), "adc" | "service_account") {
                errors
                    .entry("auth_mode".to_owned())
                    .or_default()
                    .push("Use adc or service_account for Vertex AI.".to_owned());
            }
            if auth_mode == "adc" && request.credential.is_some() {
                errors
                    .entry("credential".to_owned())
                    .or_default()
                    .push("Do not submit a credential when using Vertex ADC.".to_owned());
            }
            if request.endpoint.is_some() {
                errors
                    .entry("endpoint".to_owned())
                    .or_default()
                    .push(
                        "Vertex AI derives its regional Google endpoint from cloud_project and cloud_region."
                            .to_owned(),
                    );
            }
            reject_create_field(
                &mut errors,
                "deployment",
                request.deployment.is_some(),
                "Vertex AI does not accept a deployment field.",
            );
            reject_create_field(
                &mut errors,
                "api_version",
                request.api_version.is_some(),
                "Vertex AI does not accept an API-version field.",
            );
        }
        ProviderKind::Bedrock => {
            if request.cloud_region.as_deref().is_none_or(str::is_empty) {
                errors
                    .entry("cloud_region".to_owned())
                    .or_default()
                    .push("Bedrock requires an AWS region.".to_owned());
            }
            if !matches!(auth_mode.as_str(), "default_chain" | "static") {
                errors
                    .entry("auth_mode".to_owned())
                    .or_default()
                    .push("Use default_chain or static for Bedrock.".to_owned());
            }
            if request.endpoint.is_some() {
                errors
                    .entry("endpoint".to_owned())
                    .or_default()
                    .push(
                        "Bedrock uses the official regional AWS endpoint; custom endpoints are not accepted."
                            .to_owned(),
                    );
            }
            reject_create_field(
                &mut errors,
                "cloud_project",
                request.cloud_project.is_some(),
                "Bedrock does not accept a cloud project.",
            );
            reject_create_field(
                &mut errors,
                "deployment",
                request.deployment.is_some(),
                "Bedrock does not accept a deployment field.",
            );
            reject_create_field(
                &mut errors,
                "api_version",
                request.api_version.is_some(),
                "Bedrock does not accept an API-version field.",
            );
            if auth_mode == "default_chain" && request.credential.is_some() {
                errors.entry("credential".to_owned()).or_default().push(
                    "Do not submit a credential when using the AWS default chain.".to_owned(),
                );
            }
        }
        ProviderKind::AzureOpenAi => {
            if base_url.is_empty() {
                errors
                    .entry("endpoint".to_owned())
                    .or_default()
                    .push("Azure OpenAI requires an HTTPS resource endpoint.".to_owned());
            }
            if request.deployment.as_deref().is_none_or(str::is_empty) {
                errors
                    .entry("deployment".to_owned())
                    .or_default()
                    .push("Azure OpenAI requires a deployment name.".to_owned());
            }
            if request.api_version.as_deref().is_none_or(str::is_empty) {
                errors
                    .entry("api_version".to_owned())
                    .or_default()
                    .push("Azure OpenAI requires an API version.".to_owned());
            }
            if auth_mode != "api_key" {
                errors
                    .entry("auth_mode".to_owned())
                    .or_default()
                    .push("Azure OpenAI currently requires api_key authentication.".to_owned());
            }
            reject_create_field(
                &mut errors,
                "cloud_region",
                request.cloud_region.is_some(),
                "Azure OpenAI does not accept a cloud region.",
            );
            reject_create_field(
                &mut errors,
                "cloud_project",
                request.cloud_project.is_some(),
                "Azure OpenAI does not accept a cloud project.",
            );
        }
    }
    if !errors.is_empty() {
        return Err(Problem::validation(errors));
    }
    let parsed_auth_mode = auth_mode.parse().map_err(|_| Problem::internal())?;
    let required = |value: Option<String>| value.ok_or_else(Problem::internal);
    let config = match kind {
        ProviderKind::OpenAi => ProviderConfig::OpenAi {
            endpoint: Some(base_url.clone()),
        },
        ProviderKind::OpenAiCompatible => ProviderConfig::OpenAiCompatible {
            endpoint: base_url.clone(),
        },
        ProviderKind::Anthropic => ProviderConfig::Anthropic {
            endpoint: Some(base_url.clone()),
            api_version: request.api_version.clone(),
        },
        ProviderKind::Gemini => ProviderConfig::Gemini {
            endpoint: Some(base_url.clone()),
        },
        ProviderKind::VertexAi => ProviderConfig::VertexAi {
            project: required(request.cloud_project.clone())?,
            location: required(request.cloud_region.clone())?,
            probe_model: required(request.model.clone())?,
            auth_mode: parsed_auth_mode,
        },
        ProviderKind::Bedrock => ProviderConfig::Bedrock {
            region: required(request.cloud_region.clone())?,
            auth_mode: parsed_auth_mode,
        },
        ProviderKind::AzureOpenAi => ProviderConfig::AzureOpenAi {
            endpoint: base_url.clone(),
            deployment: required(request.deployment.clone())?,
            api_version: required(request.api_version.clone())?,
        },
    };
    let credential = match (kind, parsed_auth_mode, request.credential.as_ref()) {
        (_, _, None) => ProviderCredential::None,
        (ProviderKind::Bedrock, olp_domain::ProviderAuthMode::Static, Some(credential)) => {
            ProviderCredential::AwsStatic(Zeroizing::new(credential.expose().as_bytes().to_vec()))
        }
        (
            ProviderKind::VertexAi,
            olp_domain::ProviderAuthMode::ServiceAccount,
            Some(credential),
        ) => ProviderCredential::ServiceAccountJson(Zeroizing::new(credential.expose().to_owned())),
        (_, _, Some(credential)) => {
            ProviderCredential::ApiKey(Zeroizing::new(credential.expose().to_owned()))
        }
    };
    let transport = ProviderFactory::transport(config, credential)
        .await
        .map_err(|error| provider_connector_validation(kind, error))?;
    let connector_available = true;
    let provider_id = Uuid::now_v7();
    let credential_id = request.credential.as_ref().map(|_| Uuid::now_v7());
    let model_id = request.model.as_ref().map(|_| Uuid::now_v7());
    let encrypted = match (&request.credential, credential_id) {
        (Some(credential), Some(credential_id)) => Some(
            master_key
                .seal(
                    credential.expose().as_bytes(),
                    &credential_aad(provider_id, credential_id, 1),
                )
                .map_err(|error| {
                    error!(%error, "provider credential encryption failed");
                    Problem::internal()
                })?,
        ),
        (None, None) => None,
        _ => return Err(Problem::internal()),
    };
    let response_name = request.name.clone();
    let response_kind = request.kind.clone();
    let response_model = request.model.clone();
    let created = require_store(&state)?
        .create_provider_draft(
            NewProviderDraft {
                provider_id,
                credential_id,
                model_id,
                name: request.name.clone(),
                kind,
                endpoint: matches!(
                    kind,
                    ProviderKind::OpenAiCompatible | ProviderKind::AzureOpenAi
                )
                .then_some(base_url),
                cloud_region: request.cloud_region.clone(),
                cloud_project: request.cloud_project.clone(),
                deployment: request.deployment.clone(),
                api_version: request.api_version.clone(),
                auth_mode: auth_mode.parse().map_err(|_| Problem::internal())?,
                connector_ready: connector_available,
                credential: encrypted,
                model: request.model.clone(),
                display_name: request.model.as_ref().map(|model| {
                    request
                        .display_name
                        .clone()
                        .unwrap_or_else(|| model.clone())
                }),
                model_enabled: connector_available && request.model.is_some(),
                surface: request
                    .model
                    .as_ref()
                    .and(surface)
                    .map(str::parse)
                    .transpose()
                    .map_err(|_| Problem::internal())?,
                actor: principal.user_id,
                idempotency_key,
            },
            ReplayableIdempotency::new(request_fingerprint, master_key),
            |created| {
                IdempotencyResponse::json(
                    StatusCode::CREATED.as_u16(),
                    &ProviderResponse {
                        id: created.provider_id,
                        name: response_name,
                        kind: response_kind,
                        state: "draft".to_owned(),
                        model: response_model,
                        etag: created.etag,
                    },
                    Some(format!("\"{}\"", created.etag)),
                )
            },
        )
        .await
        .map_err(map_configuration)?;
    let executed_provider_id = match &created {
        IdempotencyOutcome::Executed { value, .. } => Some(value.provider_id),
        IdempotencyOutcome::Replayed(_) => None,
    };
    if let Some(provider_id) = executed_provider_id {
        state
            .transports
            .register(olp_domain::ProviderId::from_uuid(provider_id), transport);
    }
    idempotency_http_response(created)
}

#[utoipa::path(
    post,
    path = "/api/v1/providers/{provider_id}/activate",
    tag = "providers",
    params(
        ("provider_id" = Uuid, Path, description = "Provider ID"),
        ("If-Match" = String, Header, description = "Current provider ETag"),
        ("Idempotency-Key" = String, Header, description = "Unique activation key")
    ),
    responses(
        (status = 200, description = "Provider activated", body = ProviderActivationResponse),
        (status = 400, description = "Required header is missing or invalid", body = Problem),
        (status = 409, description = "Idempotency-Key was already used", body = Problem),
        (status = 412, description = "ETag mismatch", body = Problem),
        (status = 422, description = "Provider is incomplete", body = Problem)
    )
)]
async fn activate_provider(
    State(state): State<ApiState>,
    axum::extract::Path(provider_id): axum::extract::Path<Uuid>,
    headers: HeaderMap,
) -> Result<Response, Problem> {
    let principal = require_mutation_session(&state, &headers).await?;
    require_provider_manager(&principal)?;
    let expected_etag = if_match(&headers)?;
    let idempotency_key = require_idempotency_key(&headers)?;
    let activated = require_store(&state)?
        .activate_provider(
            provider_id,
            expected_etag,
            principal.user_id,
            idempotency_key,
        )
        .await
        .map_err(map_configuration)?;
    let mut response = (
        StatusCode::OK,
        Json(ProviderActivationResponse {
            id: provider_id,
            state: "active".to_owned(),
            etag: activated.etag,
            runtime_generation: RuntimeGenerationResponse {
                id: activated.release.generation_id,
                sequence: activated.release.sequence,
            },
        }),
    )
        .into_response();
    response.headers_mut().insert(
        header::ETAG,
        HeaderValue::from_str(&format!("\"{}\"", activated.etag))
            .map_err(|_| Problem::internal())?,
    );
    Ok(response)
}

#[derive(Debug, Serialize, ToSchema)]
pub struct ProviderActivationResponse {
    #[schema(value_type = String, format = Uuid)]
    pub id: Uuid,
    pub state: String,
    #[schema(value_type = String, format = Uuid)]
    pub etag: Uuid,
    pub runtime_generation: RuntimeGenerationResponse,
}

#[derive(Debug, Deserialize, Serialize, ToSchema)]
pub struct CreateApiKeyRequest {
    pub name: String,
    #[serde(default = "default_key_scopes")]
    pub scopes: Vec<String>,
    #[serde(default)]
    pub allowed_routes: Vec<String>,
    pub requests_per_minute: Option<u32>,
    pub tokens_per_minute: Option<u64>,
    pub max_concurrency: Option<u32>,
    pub expires_at: Option<DateTime<Utc>>,
}

fn default_key_scopes() -> Vec<String> {
    vec!["inference".to_owned()]
}

#[derive(Serialize, ToSchema)]
pub struct CreateApiKeyResponse {
    #[schema(value_type = String, format = Uuid)]
    pub id: Uuid,
    pub lookup_id: String,
    /// Returned only by this creation response.
    #[schema(value_type = String)]
    secret: WriteOnlySecret,
    pub runtime_generation: RuntimeGenerationResponse,
}

impl fmt::Debug for CreateApiKeyResponse {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("CreateApiKeyResponse")
            .field("id", &self.id)
            .field("lookup_id", &self.lookup_id)
            .field("secret", &"[REDACTED]")
            .field("runtime_generation", &self.runtime_generation)
            .finish()
    }
}

#[derive(Debug, Serialize, ToSchema)]
pub struct RuntimeGenerationResponse {
    #[schema(value_type = String, format = Uuid)]
    pub id: Uuid,
    pub sequence: i64,
}

#[utoipa::path(
    post,
    path = "/api/v1/api-keys",
    tag = "api-keys",
    request_body = CreateApiKeyRequest,
    params(("Idempotency-Key" = String, Header, description = "Unique creation key")),
    responses(
        (status = 201, description = "API key created; secret is shown once", body = CreateApiKeyResponse),
        (status = 403, description = "Insufficient role, CSRF, or origin failure", body = Problem),
        (status = 409, description = "Idempotency conflict or operation in progress", body = Problem),
        (status = 422, description = "Validation failed", body = Problem),
        (status = 503, description = "Master key, key hasher, or database unavailable", body = Problem)
    )
)]
async fn create_api_key(
    State(state): State<ApiState>,
    headers: HeaderMap,
    payload: Result<Json<CreateApiKeyRequest>, JsonRejection>,
) -> Result<Response, Problem> {
    let principal = require_mutation_session(&state, &headers).await?;
    require_key_manager(&principal)?;
    let idempotency_key = require_idempotency_key(&headers)?.to_owned();
    let request = json_payload(payload)?;
    let request_fingerprint = idempotency_fingerprint(&request).map_err(map_persistence)?;
    let mut errors = FieldErrors::new();
    if request.name.trim().is_empty() || request.name.chars().count() > 100 {
        errors.insert(
            "name".to_owned(),
            vec!["Use between 1 and 100 characters.".to_owned()],
        );
    }
    let scopes = request
        .scopes
        .iter()
        .map(|scope| match scope.as_str() {
            "inference" => Ok(ApiKeyScope::Inference),
            "models_read" => Ok(ApiKeyScope::ModelsRead),
            _ => Err(scope.clone()),
        })
        .collect::<Result<Vec<_>, _>>()
        .map_err(|scope| {
            let mut scope_errors = FieldErrors::new();
            scope_errors.insert("scopes".to_owned(), vec![format!("Unknown scope {scope}.")]);
            Problem::validation(scope_errors)
        })?;
    if scopes.is_empty() {
        errors.insert(
            "scopes".to_owned(),
            vec!["Select at least one scope.".to_owned()],
        );
    } else if scopes.iter().copied().collect::<BTreeSet<_>>().len() != scopes.len() {
        errors.insert(
            "scopes".to_owned(),
            vec!["Scope entries must be unique.".to_owned()],
        );
    }
    let allowed_routes = request
        .allowed_routes
        .iter()
        .map(|slug| RouteSlug::parse(slug.clone()))
        .collect::<Result<Vec<_>, _>>()
        .map_err(|error| {
            let mut route_errors = FieldErrors::new();
            route_errors.insert("allowed_routes".to_owned(), vec![error.to_string()]);
            Problem::validation(route_errors)
        })?;
    if allowed_routes
        .iter()
        .cloned()
        .collect::<BTreeSet<_>>()
        .len()
        != allowed_routes.len()
    {
        errors.insert(
            "allowed_routes".to_owned(),
            vec!["Route allowlist entries must be unique.".to_owned()],
        );
    }
    let limits = ApiKeyLimits {
        requests_per_minute: request.requests_per_minute.and_then(NonZeroU32::new),
        tokens_per_minute: request.tokens_per_minute.and_then(NonZeroU64::new),
        concurrency: request.max_concurrency.and_then(NonZeroU32::new),
    };
    if request.requests_per_minute == Some(0) {
        errors.insert(
            "requests_per_minute".to_owned(),
            vec!["Use a positive limit or omit the field.".to_owned()],
        );
    }
    if request.tokens_per_minute == Some(0) {
        errors.insert(
            "tokens_per_minute".to_owned(),
            vec!["Use a positive limit or omit the field.".to_owned()],
        );
    }
    if request.max_concurrency == Some(0) {
        errors.insert(
            "max_concurrency".to_owned(),
            vec!["Use a positive limit or omit the field.".to_owned()],
        );
    }
    if !errors.is_empty() {
        return Err(Problem::validation(errors));
    }
    let master_key = state
        .master_key
        .as_deref()
        .ok_or_else(|| Problem::service_unavailable("master_key_not_configured"))?;
    let hasher = state
        .key_hasher
        .as_ref()
        .ok_or_else(|| Problem::service_unavailable("key_hash_key_not_configured"))?;
    let material = hasher.generate_api_key();
    let secret = WriteOnlySecret(material.expose_once().to_owned());
    let record = NewApiKeyRecord {
        name: request.name,
        material,
        scopes,
        allowed_routes,
        limits,
        expires_at: request.expires_at,
        actor: principal.user_id,
        idempotency_key,
    };
    let created = require_store(&state)?
        .create_api_key_record(
            &record,
            ReplayableIdempotency::new(request_fingerprint, master_key),
            move |created| {
                IdempotencyResponse::json(
                    StatusCode::CREATED.as_u16(),
                    &CreateApiKeyResponse {
                        id: created.id,
                        lookup_id: created.lookup_id.clone(),
                        secret,
                        runtime_generation: RuntimeGenerationResponse {
                            id: created.release.generation_id,
                            sequence: created.release.sequence,
                        },
                    },
                    Some(format!("\"{}\"", created.etag)),
                )
            },
        )
        .await
        .map_err(map_access)?;
    idempotency_http_response(created)
}

#[utoipa::path(
    post,
    path = "/api/v1/api-keys/{api_key_id}/revoke",
    tag = "api-keys",
    params(
        ("api_key_id" = Uuid, Path, description = "API key ID"),
        ("If-Match" = String, Header, description = "Current API-key ETag"),
        ("Idempotency-Key" = String, Header, description = "Unique revocation key")
    ),
    responses(
        (status = 200, description = "API key revoked and new runtime published", body = RuntimeGenerationResponse),
        (status = 404, description = "API key not found", body = Problem),
        (status = 412, description = "ETag mismatch", body = Problem)
    )
)]
async fn revoke_api_key(
    State(state): State<ApiState>,
    axum::extract::Path(api_key_id): axum::extract::Path<Uuid>,
    headers: HeaderMap,
) -> Result<Response, Problem> {
    let principal = require_mutation_session(&state, &headers).await?;
    require_key_manager(&principal)?;
    let idempotency_key = require_idempotency_key(&headers)?;
    let revoked = require_store(&state)?
        .revoke_api_key_record(
            api_key_id,
            if_match(&headers)?,
            principal.user_id,
            idempotency_key,
        )
        .await
        .map_err(map_access)?;
    let mut response = Json(RuntimeGenerationResponse {
        id: revoked.release.generation_id,
        sequence: revoked.release.sequence,
    })
    .into_response();
    response.headers_mut().insert(
        header::ETAG,
        HeaderValue::from_str(&format!("\"{}\"", revoked.etag)).map_err(|_| Problem::internal())?,
    );
    Ok(response)
}

#[derive(Debug, Deserialize, Serialize, ToSchema)]
pub struct CreateRouteDraftRequest {
    pub slug: String,
    #[serde(default = "default_route_operations")]
    pub operations: Vec<String>,
    pub overall_timeout_ms: u64,
    pub max_attempts: u16,
    pub targets: Vec<RouteTargetRequest>,
}

fn default_route_operations() -> Vec<String> {
    vec!["generation".to_owned()]
}

#[derive(Debug, Deserialize, Serialize, ToSchema)]
pub struct RouteTargetRequest {
    #[schema(value_type = String, format = Uuid)]
    pub provider_id: Uuid,
    pub provider_model: String,
    pub priority: u16,
    pub weight: u32,
    pub timeout_ms: u64,
}

#[derive(Debug, Serialize, ToSchema)]
pub struct RouteDraftResponse {
    #[schema(value_type = String, format = Uuid)]
    pub id: Uuid,
    pub slug: String,
    pub state: String,
    #[schema(value_type = String, format = Uuid)]
    pub etag: Uuid,
}

#[derive(Debug, Serialize, ToSchema)]
pub struct RouteActivationResponse {
    #[schema(value_type = String, format = Uuid)]
    pub route_id: Uuid,
    #[schema(value_type = String, format = Uuid)]
    pub revision_id: Uuid,
    pub revision: i32,
    pub runtime_generation: RuntimeGenerationResponse,
}

#[utoipa::path(
    post,
    path = "/api/v1/route-drafts",
    tag = "routes",
    request_body = CreateRouteDraftRequest,
    params(("Idempotency-Key" = String, Header, description = "Unique route-draft creation key")),
    responses(
        (status = 201, description = "Route draft created", body = RouteDraftResponse),
        (status = 400, description = "Idempotency-Key is missing or invalid", body = Problem),
        (status = 409, description = "Idempotency-Key was already used or is in progress", body = Problem),
        (status = 422, description = "Route draft is invalid", body = Problem),
        (status = 503, description = "Master key or database unavailable", body = Problem)
    )
)]
async fn create_route_draft(
    State(state): State<ApiState>,
    headers: HeaderMap,
    payload: Result<Json<CreateRouteDraftRequest>, JsonRejection>,
) -> Result<Response, Problem> {
    let principal = require_mutation_session(&state, &headers).await?;
    require_route_manager(&principal)?;
    let idempotency_key = require_idempotency_key(&headers)?.to_owned();
    let request = json_payload(payload)?;
    let request_fingerprint = idempotency_fingerprint(&request).map_err(map_persistence)?;
    let master_key = state
        .master_key
        .as_deref()
        .ok_or_else(|| Problem::service_unavailable("master_key_not_configured"))?;
    let operations = request
        .operations
        .iter()
        .map(|operation| {
            operation
                .parse::<OperationKind>()
                .map_err(|_| operation.clone())
        })
        .collect::<Result<Vec<_>, _>>()
        .map_err(|operation| {
            let mut errors = FieldErrors::new();
            errors.insert(
                "operations".to_owned(),
                vec![format!(
                    "Operation {operation} is not supported by the operation model."
                )],
            );
            Problem::validation(errors)
        })?;
    let targets = request
        .targets
        .into_iter()
        .map(|target| NewRouteTarget {
            provider_id: target.provider_id,
            provider_model: target.provider_model,
            priority: target.priority,
            weight: target.weight,
            timeout_ms: target.timeout_ms,
        })
        .collect();
    let created = require_store(&state)?
        .create_route_draft(
            NewRouteDraft {
                slug: request.slug,
                operations,
                overall_timeout_ms: request.overall_timeout_ms,
                max_attempts: request.max_attempts,
                targets,
                actor: principal.user_id,
                idempotency_key,
            },
            ReplayableIdempotency::new(request_fingerprint, master_key),
            |created| {
                IdempotencyResponse::json(
                    StatusCode::CREATED.as_u16(),
                    &RouteDraftResponse {
                        id: created.id,
                        slug: created.slug.to_string(),
                        state: "draft".to_owned(),
                        etag: created.etag,
                    },
                    Some(format!("\"{}\"", created.etag)),
                )
            },
        )
        .await
        .map_err(map_configuration)?;
    idempotency_http_response(created)
}

#[utoipa::path(
    post,
    path = "/api/v1/route-drafts/{draft_id}/validate",
    tag = "routes",
    params(
        ("draft_id" = Uuid, Path, description = "Route draft ID"),
        ("If-Match" = String, Header, description = "Current route-draft ETag")
    ),
    responses(
        (status = 200, description = "Route draft validated", body = RouteDraftResponse),
        (status = 412, description = "ETag mismatch", body = Problem),
        (status = 422, description = "Eligibility validation failed", body = Problem)
    )
)]
async fn validate_route_draft(
    State(state): State<ApiState>,
    axum::extract::Path(draft_id): axum::extract::Path<Uuid>,
    headers: HeaderMap,
) -> Result<Response, Problem> {
    let principal = require_mutation_session(&state, &headers).await?;
    require_route_manager(&principal)?;
    let (etag, slug) = require_store(&state)?
        .validate_route_draft(draft_id, if_match(&headers)?, principal.user_id)
        .await
        .map_err(map_configuration)?;
    let mut response = (
        StatusCode::OK,
        Json(RouteDraftResponse {
            id: draft_id,
            slug: slug.to_string(),
            state: "validated".to_owned(),
            etag,
        }),
    )
        .into_response();
    response.headers_mut().insert(
        header::ETAG,
        HeaderValue::from_str(&format!("\"{etag}\"")).map_err(|_| Problem::internal())?,
    );
    Ok(response)
}

#[utoipa::path(
    post,
    path = "/api/v1/route-drafts/{draft_id}/activate",
    tag = "routes",
    params(
        ("draft_id" = Uuid, Path, description = "Route draft ID"),
        ("If-Match" = String, Header, description = "Validated route-draft ETag"),
        ("Idempotency-Key" = String, Header, description = "Unique activation key")
    ),
    responses(
        (status = 200, description = "Route activated and runtime published", body = RouteActivationResponse),
        (status = 409, description = "Draft has not been validated", body = Problem),
        (status = 412, description = "ETag mismatch", body = Problem)
    )
)]
async fn activate_route_draft(
    State(state): State<ApiState>,
    axum::extract::Path(draft_id): axum::extract::Path<Uuid>,
    headers: HeaderMap,
) -> Result<Response, Problem> {
    let principal = require_mutation_session(&state, &headers).await?;
    require_route_manager(&principal)?;
    let idempotency_key = require_idempotency_key(&headers)?;
    let expected_etag = if_match(&headers)?;
    let activated = require_store(&state)?
        .activate_route_draft(draft_id, expected_etag, principal.user_id, idempotency_key)
        .await
        .map_err(map_configuration)?;
    let mut response = Json(RouteActivationResponse {
        route_id: activated.route_id,
        revision_id: activated.revision_id,
        revision: activated.revision,
        runtime_generation: RuntimeGenerationResponse {
            id: activated.release.generation_id,
            sequence: activated.release.sequence,
        },
    })
    .into_response();
    response.headers_mut().insert(
        header::ETAG,
        HeaderValue::from_str(&format!("\"{expected_etag}\"")).map_err(|_| Problem::internal())?,
    );
    Ok(response)
}

fn session_response(
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

fn prevent_sensitive_response_caching(response: &mut Response) {
    response
        .headers_mut()
        .insert(header::CACHE_CONTROL, HeaderValue::from_static("no-store"));
    response
        .headers_mut()
        .insert(header::PRAGMA, HeaderValue::from_static("no-cache"));
}

fn validate_setup(request: &SetupRequest) -> Result<(), Problem> {
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
    if request.organization_name.trim().is_empty()
        || request.organization_name.chars().count() > 100
    {
        errors
            .entry("organization_name".to_owned())
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
fn local_login_rate_limit_target(email: &str) -> String {
    if email.len() > 254 {
        INVALID_LOGIN_RATE_LIMIT_TARGET.to_owned()
    } else {
        email.trim().to_lowercase()
    }
}

/// Prevent an arbitrarily large malformed invitation token from becoming HMAC
/// input while still admitting it against the caller's source bucket.
fn invitation_rate_limit_target(token: &str) -> &str {
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

fn page_parameters(query: PageQuery) -> Result<(Option<Uuid>, i64), Problem> {
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

fn parse_user_role(role: &str) -> Result<Role, Problem> {
    role.parse().map_err(|_| {
        let mut errors = FieldErrors::new();
        errors.insert(
            "role".to_owned(),
            vec!["Use owner, operator, developer, or viewer.".to_owned()],
        );
        Problem::validation(errors)
    })
}

fn expire_session_cookies(response: &mut Response) {
    response.headers_mut().insert(
        header::SET_COOKIE,
        HeaderValue::from_static(
            "__Host-olp_session=; Path=/; Max-Age=0; Secure; HttpOnly; SameSite=Lax",
        ),
    );
    response.headers_mut().append(
        header::SET_COOKIE,
        HeaderValue::from_static("__Host-olp_csrf=; Path=/; Max-Age=0; Secure; SameSite=Lax"),
    );
}

pub(crate) fn enforce_origin(state: &ApiState, headers: &HeaderMap) -> Result<(), Problem> {
    let origin = headers
        .get(header::ORIGIN)
        .and_then(|value| value.to_str().ok())
        .ok_or_else(|| Problem::forbidden("origin_required", "An Origin header is required."))?;
    if origin.trim_end_matches('/') != state.public_origin.as_ref() {
        warn!(%origin, "rejected cross-origin management mutation");
        return Err(Problem::forbidden(
            "origin_not_allowed",
            "The request origin is not allowed.",
        ));
    }
    Ok(())
}

fn session_cookie(headers: &HeaderMap) -> Result<&str, Problem> {
    cookie(headers, SESSION_COOKIE)
        .ok_or_else(|| Problem::unauthorized("The session cookie is missing."))
}

fn cookie<'a>(headers: &'a HeaderMap, expected_name: &str) -> Option<&'a str> {
    headers
        .get(header::COOKIE)
        .and_then(|value| value.to_str().ok())
        .and_then(|cookies| {
            cookies.split(';').find_map(|cookie| {
                let (name, value) = cookie.trim().split_once('=')?;
                (name == expected_name).then_some(value)
            })
        })
}

pub(crate) fn require_store(state: &ApiState) -> Result<&PgStore, Problem> {
    state
        .store
        .as_ref()
        .ok_or_else(|| Problem::service_unavailable("database_not_configured"))
}

pub(crate) async fn require_read_session(
    state: &ApiState,
    headers: &HeaderMap,
) -> Result<SessionPrincipal, Problem> {
    let token = session_cookie(headers)?;
    require_store(state)?
        .session_principal(token)
        .await
        .map_err(map_persistence)?
        .ok_or_else(|| Problem::unauthorized("The session is missing or expired."))
}

pub(crate) async fn require_mutation_session(
    state: &ApiState,
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

fn require_provider_manager(principal: &SessionPrincipal) -> Result<(), Problem> {
    require_permission(principal, Permission::ManageProviders)
}

fn require_key_manager(principal: &SessionPrincipal) -> Result<(), Problem> {
    require_permission(principal, Permission::ManageApiKeys)
}

fn require_route_manager(principal: &SessionPrincipal) -> Result<(), Problem> {
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

fn map_configuration(error: ConfigurationError) -> Problem {
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
    }
}

fn map_access(error: AccessError) -> Problem {
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

fn user_not_found() -> Problem {
    Problem::new(
        StatusCode::NOT_FOUND,
        "user_not_found",
        "User not found",
        "The user does not exist.",
    )
}

fn acquire_password_work() -> Result<SemaphorePermit<'static>, Problem> {
    PASSWORD_WORK
        .try_acquire()
        .map_err(|_| public_auth_rate_limited())
}

fn spawn_password_work<T>(
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

fn public_auth_rate_limited() -> Problem {
    Problem::new(
        StatusCode::TOO_MANY_REQUESTS,
        "authentication_rate_limited",
        "Too many authentication attempts",
        "Too many authentication attempts are in progress. Wait before retrying.",
    )
}

fn map_team(error: TeamError) -> Problem {
    match error {
        TeamError::Persistence(error) => map_persistence(error),
        TeamError::Invalid(detail) => {
            let mut errors = FieldErrors::new();
            errors.insert("identity".to_owned(), vec![detail]);
            Problem::validation(errors)
        }
        TeamError::NotFound => Problem::new(
            StatusCode::NOT_FOUND,
            "identity_resource_not_found",
            "Identity resource not found",
            "The requested identity resource does not exist.",
        ),
        TeamError::PreconditionFailed => Problem::new(
            StatusCode::PRECONDITION_FAILED,
            "etag_mismatch",
            "Precondition failed",
            "The user changed after it was loaded. Refresh and retry.",
        ),
        TeamError::LastOwner => Problem::conflict(
            "last_owner_required",
            "The last active owner cannot be demoted.",
        ),
        TeamError::EmailAlreadyMember => Problem::conflict(
            "email_already_member",
            "A user with this email already belongs to the installation.",
        ),
        TeamError::PendingInvitationExists => Problem::conflict(
            "pending_invitation_exists",
            "A pending invitation already exists for this email.",
        ),
        TeamError::InvitationUnavailable => Problem::new(
            StatusCode::GONE,
            "invitation_unavailable",
            "Invitation unavailable",
            "The invitation is invalid, expired, revoked, or already accepted.",
        ),
        TeamError::SessionForbidden => Problem::forbidden(
            "permission_denied",
            "Only an owner can revoke another user's session.",
        ),
        TeamError::CorruptIdentity => {
            error!("stored identity data contains an unknown role");
            Problem::internal()
        }
        TeamError::IdempotencyConflict => Problem::conflict(
            "idempotency_key_reused",
            "This Idempotency-Key has already been used for this invitation operation.",
        ),
        TeamError::IdempotencyInProgress => Problem::conflict(
            "idempotency_in_progress",
            "An operation with this Idempotency-Key is still in progress.",
        ),
        TeamError::LocalPasswordUnavailable => Problem::forbidden(
            "local_password_unavailable",
            "This profile does not have a local password.",
        ),
        TeamError::LocalPasswordAlreadyConfigured => Problem::conflict(
            "local_password_already_configured",
            "A local password is already configured. Use the password-change operation.",
        ),
    }
}

pub(crate) fn map_persistence(error: PersistenceError) -> Problem {
    error!(%error, "management persistence operation failed");
    Problem::service_unavailable("database_unavailable")
}

pub(crate) fn json_payload<T>(payload: Result<Json<T>, JsonRejection>) -> Result<T, Problem> {
    payload.map(|Json(value)| value).map_err(|error| {
        Problem::bad_request("invalid_json", format!("The JSON body is invalid: {error}"))
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::{path::PathBuf, sync::Arc};

    fn state() -> ApiState {
        ApiState::new(
            crate::ApiMode::Control,
            None,
            Arc::new(crate::RuntimeManager::empty()),
            "https://olp.example.test",
            PathBuf::from("console"),
        )
    }

    fn principal(role: &str) -> SessionPrincipal {
        SessionPrincipal {
            session_id: Uuid::now_v7(),
            user_id: Uuid::now_v7(),
            email: "person@example.test".to_owned(),
            display_name: "Person".to_owned(),
            role: role.to_owned(),
            csrf_digest: vec![0; 32],
            expires_at: Utc::now() + chrono::Duration::hours(1),
        }
    }

    #[test]
    fn setup_validation_returns_field_errors() {
        let problem = validate_setup(&SetupRequest {
            email: "bad".into(),
            password: WriteOnlySecret("short".into()),
            display_name: "".into(),
            organization_name: "".into(),
        })
        .unwrap_err();
        assert_eq!(problem.status, 422);
        assert_eq!(problem.errors.len(), 4);
    }

    #[test]
    fn malformed_public_auth_targets_use_bounded_source_local_sentinels() {
        assert_eq!(
            local_login_rate_limit_target(&"a".repeat(255)),
            INVALID_LOGIN_RATE_LIMIT_TARGET
        );
        assert_eq!(
            local_login_rate_limit_target(" Owner@Example.test "),
            "owner@example.test"
        );
        assert_eq!(
            invitation_rate_limit_target(&"x".repeat(44)),
            INVALID_INVITATION_RATE_LIMIT_TARGET
        );
        assert_eq!(
            invitation_rate_limit_target(&"x".repeat(43)),
            "x".repeat(43)
        );
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn unauthenticated_password_work_remains_bounded_after_request_cancellation() {
        let barrier = Arc::new(std::sync::Barrier::new(2));
        let task_barrier = Arc::clone(&barrier);
        let (started, started_receiver) = std::sync::mpsc::channel();
        let (completed, completed_receiver) = std::sync::mpsc::channel();
        let task = spawn_password_work(move || {
            started.send(()).unwrap();
            task_barrier.wait();
            completed.send(()).unwrap();
        })
        .unwrap();
        started_receiver
            .recv_timeout(std::time::Duration::from_secs(1))
            .unwrap();
        drop(task);

        let permits = (1..PASSWORD_WORK_CONCURRENCY)
            .map(|_| acquire_password_work().unwrap())
            .collect::<Vec<_>>();
        assert_eq!(acquire_password_work().unwrap_err().status, 429);
        barrier.wait();
        completed_receiver
            .recv_timeout(std::time::Duration::from_secs(1))
            .unwrap();
        drop(permits);
        tokio::time::timeout(std::time::Duration::from_secs(1), async {
            loop {
                if acquire_password_work().is_ok() {
                    break;
                }
                tokio::task::yield_now().await;
            }
        })
        .await
        .unwrap();
    }

    #[test]
    fn native_provider_create_shape_rejects_custom_and_cloud_fields() {
        let request = CreateProviderRequest {
            name: "native".to_owned(),
            kind: "open_ai".to_owned(),
            endpoint: Some("https://proxy.example.test/v1".to_owned()),
            cloud_region: Some("region".to_owned()),
            cloud_project: None,
            deployment: None,
            api_version: None,
            auth_mode: Some("custom".to_owned()),
            credential: Some(WriteOnlySecret("sk-test-secret".to_owned())),
            legacy_api_key: None,
            model: None,
            display_name: None,
        };
        let mut errors = FieldErrors::new();
        require_create_auth_mode(
            &mut errors,
            request.auth_mode.as_deref().unwrap(),
            "api_key",
        );
        reject_create_field(
            &mut errors,
            "endpoint",
            request.endpoint.is_some(),
            "Native OpenAI uses the official endpoint.",
        );
        reject_create_cloud_fields(&mut errors, &request);
        assert!(errors.contains_key("endpoint"));
        assert!(errors.contains_key("cloud_region"));
        assert!(errors.contains_key("auth_mode"));
    }

    #[test]
    fn mutations_require_exact_origin() {
        let state = state();
        let mut headers = HeaderMap::new();
        assert!(enforce_origin(&state, &headers).is_err());
        headers.insert(
            header::ORIGIN,
            HeaderValue::from_static("https://evil.test"),
        );
        assert!(enforce_origin(&state, &headers).is_err());
        headers.insert(
            header::ORIGIN,
            HeaderValue::from_static("https://olp.example.test"),
        );
        assert!(enforce_origin(&state, &headers).is_ok());
    }

    #[test]
    fn cookie_parser_uses_only_host_session_cookie() {
        let mut headers = HeaderMap::new();
        headers.insert(
            header::COOKIE,
            HeaderValue::from_static("other=x; __Host-olp_session=secret; theme=dark"),
        );
        assert_eq!(session_cookie(&headers).unwrap(), "secret");
    }

    #[test]
    fn idempotency_key_requires_url_safe_header_value() {
        let mut headers = HeaderMap::new();
        assert_eq!(require_idempotency_key(&headers).unwrap_err().status, 400);

        headers.insert("idempotency-key", HeaderValue::from_static("1234567"));
        assert_eq!(require_idempotency_key(&headers).unwrap_err().status, 400);

        headers.insert("idempotency-key", HeaderValue::from_static("12345678"));
        assert_eq!(require_idempotency_key(&headers).unwrap(), "12345678");

        headers.insert(
            "idempotency-key",
            HeaderValue::from_static("contains/slash"),
        );
        assert_eq!(require_idempotency_key(&headers).unwrap_err().status, 400);

        headers.insert(
            "idempotency-key",
            HeaderValue::from_static("provider-create_01.v2"),
        );
        assert_eq!(
            require_idempotency_key(&headers).unwrap(),
            "provider-create_01.v2"
        );
    }

    #[test]
    fn if_match_requires_one_strong_quoted_uuid_etag() {
        let id = Uuid::now_v7();
        let mut headers = HeaderMap::new();
        assert_eq!(if_match(&headers).unwrap_err().status, 428);
        headers.insert(
            header::IF_MATCH,
            HeaderValue::from_str(&format!("\"{id}\"")).unwrap(),
        );
        assert_eq!(if_match(&headers).unwrap(), id);
        headers.insert(
            header::IF_MATCH,
            HeaderValue::from_str(&id.to_string()).unwrap(),
        );
        assert_eq!(if_match(&headers).unwrap_err().status, 400);
        headers.insert(header::IF_MATCH, HeaderValue::from_static("*"));
        assert_eq!(if_match(&headers).unwrap_err().status, 400);
    }

    #[test]
    fn create_draft_openapi_contract_requires_idempotency_and_documents_conflict() {
        let document = serde_json::to_value(ManagementApiDoc::openapi()).unwrap();
        for path in ["/api/v1/providers", "/api/v1/route-drafts"] {
            let post = &document["paths"][path]["post"];
            let parameters = post["parameters"].as_array().unwrap();
            assert!(parameters.iter().any(|parameter| {
                parameter["name"] == "Idempotency-Key"
                    && parameter["in"] == "header"
                    && parameter["required"] == true
            }));
            assert!(post["responses"].get("409").is_some());
        }
    }

    #[test]
    fn idempotency_reuse_is_an_rfc9457_conflict() {
        let response = map_configuration(ConfigurationError::IdempotencyConflict).into_response();
        assert_eq!(response.status(), StatusCode::CONFLICT);
        assert_eq!(
            response.headers().get(header::CONTENT_TYPE).unwrap(),
            "application/problem+json"
        );
    }

    #[test]
    fn replayable_responses_are_never_cacheable() {
        let response = idempotency_http_response(IdempotencyOutcome::<()>::Replayed(
            IdempotencyResponse::new(
                StatusCode::CREATED.as_u16(),
                Some("application/json".to_owned()),
                None,
                br#"{"secret":"shown-once"}"#.to_vec(),
            )
            .expect("fixed replay fixture is within response bounds"),
        ))
        .unwrap();
        assert_eq!(
            response.headers().get(header::CACHE_CONTROL).unwrap(),
            "no-store"
        );
        assert_eq!(response.headers().get(header::PRAGMA).unwrap(), "no-cache");
    }

    #[test]
    fn route_guard_delegates_every_role_permission_pair_to_core() {
        for role in Role::ALL {
            let principal = principal(role.as_str());
            for permission in Permission::ALL {
                assert_eq!(
                    require_permission(&principal, permission).is_ok(),
                    role.allows(permission),
                    "HTTP guard diverged for {role}/{permission:?}"
                );
            }
        }
        assert!(require_permission(&principal("unknown"), Permission::ReadOperations).is_err());
    }

    #[test]
    fn identity_contract_documents_preconditions_and_one_time_secrets() {
        let document = serde_json::to_value(ManagementApiDoc::openapi()).unwrap();
        let update = &document["paths"]["/api/v1/users/{user_id}"]["patch"];
        assert!(
            update["parameters"]
                .as_array()
                .unwrap()
                .iter()
                .any(|parameter| {
                    parameter["name"] == "If-Match"
                        && parameter["in"] == "header"
                        && parameter["required"] == true
                })
        );
        let create = &document["paths"]["/api/v1/invitations"]["post"];
        assert!(
            create["parameters"]
                .as_array()
                .unwrap()
                .iter()
                .any(|parameter| {
                    parameter["name"] == "Idempotency-Key"
                        && parameter["in"] == "header"
                        && parameter["required"] == true
                })
        );
        assert_eq!(
            document["components"]["schemas"]["CreateInvitationResponse"]["properties"]["token"]["readOnly"],
            true
        );
    }

    #[test]
    fn management_dto_debug_output_redacts_plaintext_secrets() {
        let setup = SetupRequest {
            email: "owner@example.test".into(),
            password: WriteOnlySecret("correct horse battery staple".into()),
            display_name: "Owner".into(),
            organization_name: "OLP".into(),
        };
        assert!(!format!("{setup:?}").contains("correct horse"));

        let login = LoginRequest {
            email: "owner@example.test".into(),
            password: WriteOnlySecret("another plaintext password".into()),
        };
        assert!(!format!("{login:?}").contains("another plaintext"));

        let response = CreateApiKeyResponse {
            id: Uuid::now_v7(),
            lookup_id: "olp_lookup".into(),
            secret: WriteOnlySecret("olp_secret_once".into()),
            runtime_generation: RuntimeGenerationResponse {
                id: Uuid::now_v7(),
                sequence: 1,
            },
        };
        assert!(!format!("{response:?}").contains("olp_secret_once"));

        let acceptance = AcceptInvitationRequest {
            token: WriteOnlySecret("sensitive-invitation-token".into()),
            display_name: "Invited person".into(),
            password: WriteOnlySecret("sensitive-local-password".into()),
        };
        let output = format!("{acceptance:?}");
        assert!(!output.contains("sensitive-invitation-token"));
        assert!(!output.contains("sensitive-local-password"));
    }
}
