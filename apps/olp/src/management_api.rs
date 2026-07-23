mod access;
mod auth;
mod common;
mod configuration;
mod openapi;

use access::{
    AcceptInvitationRequest, ChangePasswordRequest, CreateInvitationRequest,
    CreateInvitationResponse, EnrollPasswordRequest, InvitationListResponse, InvitationResponse,
    RecentAuthenticationRequest, UpdateProfileRequest, UpdateUserRoleRequest, UserDetailResponse,
    UserListResponse, accept_invitation, change_password, create_invitation, enroll_password,
    get_user, list_invitations, list_users, profile, recent_authentication, revoke_invitation,
    update_profile, update_user_role,
};
use auth::{
    AuthenticationCapabilities, LoginRequest, SessionDetailResponse, SessionListResponse,
    SessionResponse, SetupRequest, SetupStatus, UserResponse, authentication_capabilities,
    current_session, list_sessions, login, logout, revoke_session, setup, setup_status,
};
use axum::{Json, Router, routing::get, routing::post};
pub(crate) use common::{
    CSRF_HEADER, RECENT_AUTH_COOKIE, SETUP_TOKEN_HEADER, WriteOnlySecret,
    append_recent_auth_cookie, append_security_transition_cookies, clear_recent_auth_cookie,
    cookie, enforce_origin, idempotency_http_response, if_match, json_payload, map_persistence,
    prevent_sensitive_response_caching, reauthentication_required, require_idempotency_key,
    require_mutation_session, require_permission, require_read_session,
    validate_session_cookie_ttl,
};
pub(crate) use configuration::common::{map_configuration_resource, validation};
pub(crate) use olp_domain::Permission;
use utoipa::OpenApi;

use crate::{ManagementState, Problem};

pub fn router() -> Router<ManagementState> {
    Router::new()
        .route("/api/v1/openapi.json", get(openapi))
        .route(
            "/api/v1/auth/capabilities",
            get(authentication_capabilities),
        )
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
        .route(
            "/api/v1/profile/reauthenticate",
            post(recent_authentication),
        )
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
        .merge(configuration::router())
        .merge(crate::oidc::router())
        .merge(crate::operations::router())
        .merge(crate::playground::router())
}

#[derive(OpenApi)]
#[openapi(
    info(
        title = "OpenLLMProxy Management API",
        version = "1.0.0",
        description = "Management API for OpenLLMProxy."
    ),
    paths(
        auth::authentication_capabilities,
        auth::setup_status,
        auth::setup,
        auth::login,
        auth::current_session,
        auth::logout,
        access::profile,
        access::update_profile,
        access::recent_authentication,
        access::change_password,
        access::enroll_password,
        auth::list_sessions,
        auth::revoke_session,
        access::list_users,
        access::get_user,
        access::update_user_role,
        access::list_invitations,
        access::create_invitation,
        access::revoke_invitation,
        access::accept_invitation,
    ),
    components(schemas(
        AuthenticationCapabilities,
        SetupStatus,
        SetupRequest,
        LoginRequest,
        SessionResponse,
        UpdateProfileRequest,
        RecentAuthenticationRequest,
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
        Problem
    )),
    tags(
        (name = "setup"),
        (name = "sessions"),
        (name = "users"),
        (name = "invitations"),
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
    document.merge(configuration::ConfigurationApiDoc::openapi());
    document.merge(crate::playground::PlaygroundApiDoc::openapi());
    openapi::complete_openapi_contract(document)
}

#[cfg(test)]
mod tests;
