mod access;
mod auth;
mod configuration;
pub(crate) mod cookies;
pub(crate) mod error_mapping;
pub(crate) mod idempotency;
pub(crate) mod json_payload;
pub(crate) mod oidc;
mod openapi;
pub(crate) mod operations;
pub(crate) mod pagination;
pub(crate) mod permissions;
pub(crate) mod playground;
pub(crate) mod preconditions;
pub(crate) mod response_policy;
pub(crate) mod secrets;
pub(crate) mod sessions;

use access::{
    invitations::{
        AcceptInvitationRequest, CreateInvitationRequest, CreateInvitationResponse,
        InvitationListResponse, InvitationResponse, accept_invitation, create_invitation,
        list_invitations, revoke_invitation,
    },
    profile::{
        ChangePasswordRequest, EnrollPasswordRequest, RecentAuthenticationRequest,
        UpdateProfileRequest, change_password, enroll_password, profile, recent_authentication,
        update_profile,
    },
    sessions::{SessionDetailResponse, SessionListResponse, list_sessions, revoke_session},
    users::{
        UpdateUserRoleRequest, UserDetailResponse, UserListResponse, get_user, list_users,
        update_user_role,
    },
};
use auth::{
    AuthenticationCapabilities, LoginRequest, SessionResponse, SetupRequest, SetupStatus,
    UserResponse, authentication_capabilities, current_session, login, logout, setup, setup_status,
};
use axum::{Json, Router, routing::get, routing::post};
pub(crate) use configuration::common::{map_configuration_resource, validation};
pub(crate) use cookies::{
    RECENT_AUTH_COOKIE, append_recent_auth_cookie, append_security_transition_cookies,
    clear_recent_auth_cookie, validate_session_cookie_ttl,
};
pub(crate) use error_mapping::map_persistence;
pub(crate) use idempotency::{idempotency_http_response, require_idempotency_key};
pub(crate) use json_payload::json_payload;
pub(crate) use olp_domain::Permission;
pub(crate) use pagination::{PageQuery, page};
pub(crate) use permissions::require_permission;
pub(crate) use preconditions::{if_match, optional_if_match, with_etag};
pub(crate) use response_policy::{RuntimeGenerationResponse, prevent_sensitive_response_caching};
pub(crate) use secrets::WriteOnlySecret;
pub(crate) use sessions::{
    CSRF_HEADER, SETUP_TOKEN_HEADER, cookie, enforce_origin, reauthentication_required,
    require_mutation_session, require_read_session,
};
use utoipa::OpenApi;

use crate::{ManagementState, Problem, public_http::public_auth_routes::PublicAuthRoute};

pub fn router() -> Router<ManagementState> {
    Router::new()
        .route("/api/v1/openapi.json", get(openapi))
        .route(
            "/api/v1/auth/capabilities",
            get(authentication_capabilities),
        )
        .route("/api/v1/setup/status", get(setup_status))
        .route(PublicAuthRoute::FirstOwnerSetup.path(), post(setup))
        .route(
            PublicAuthRoute::PasswordLogin.path(),
            get(list_sessions).post(login),
        )
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
        .route(
            PublicAuthRoute::InvitationAcceptance.path(),
            post(accept_invitation),
        )
        .route(
            "/api/v1/invitations/{invitation_id}",
            axum::routing::delete(revoke_invitation),
        )
        .route(
            "/api/v1/teams",
            get(access::scoped::list_teams).post(access::scoped::create_team),
        )
        .route(
            "/api/v1/teams/{team_id}",
            get(access::scoped::get_team).patch(access::scoped::update_team),
        )
        .route(
            "/api/v1/teams/{team_id}/members",
            get(access::scoped::list_team_memberships),
        )
        .route(
            "/api/v1/teams/{team_id}/members/{user_id}",
            axum::routing::put(access::scoped::put_team_membership)
                .delete(access::scoped::remove_team_membership),
        )
        .route(
            "/api/v1/projects",
            get(access::scoped::list_projects).post(access::scoped::create_project),
        )
        .route(
            "/api/v1/projects/{project_id}",
            get(access::scoped::get_project).patch(access::scoped::update_project),
        )
        .route(
            "/api/v1/projects/{project_id}/members",
            get(access::scoped::list_project_memberships),
        )
        .route(
            "/api/v1/projects/{project_id}/members/{user_id}",
            axum::routing::put(access::scoped::put_project_membership)
                .delete(access::scoped::remove_project_membership),
        )
        .route(
            "/api/v1/service-accounts",
            get(access::scoped::list_service_accounts).post(access::scoped::create_service_account),
        )
        .route(
            "/api/v1/service-accounts/{service_account_id}",
            get(access::scoped::get_service_account).patch(access::scoped::update_service_account),
        )
        .merge(configuration::router())
        .merge(crate::management::oidc::router())
        .merge(crate::management::operations::router())
        .merge(crate::management::playground::router())
}

#[derive(OpenApi)]
#[openapi(
    info(
        title = "OpenLLMProxy Management API",
        version = "1.0.0",
        description = "Management API for OpenLLMProxy."
    ),
    paths(
        openapi,
        auth::authentication_capabilities,
        auth::setup_status,
        auth::setup,
        auth::login,
        auth::current_session,
        auth::logout,
        access::profile::profile,
        access::profile::update_profile,
        access::profile::recent_authentication,
        access::profile::change_password,
        access::profile::enroll_password,
        access::sessions::list_sessions,
        access::sessions::revoke_session,
        access::users::list_users,
        access::users::get_user,
        access::users::update_user_role,
        access::invitations::list_invitations,
        access::invitations::create_invitation,
        access::invitations::revoke_invitation,
        access::invitations::accept_invitation,
        access::scoped::list_teams,
        access::scoped::get_team,
        access::scoped::create_team,
        access::scoped::update_team,
        access::scoped::list_projects,
        access::scoped::get_project,
        access::scoped::create_project,
        access::scoped::update_project,
        access::scoped::list_service_accounts,
        access::scoped::get_service_account,
        access::scoped::create_service_account,
        access::scoped::update_service_account,
        access::scoped::list_team_memberships,
        access::scoped::put_team_membership,
        access::scoped::remove_team_membership,
        access::scoped::list_project_memberships,
        access::scoped::put_project_membership,
        access::scoped::remove_project_membership,
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
        access::scoped::TeamResponse,
        access::scoped::TeamListResponse,
        access::scoped::ProjectResponse,
        access::scoped::ProjectListResponse,
        access::scoped::ServiceAccountResponse,
        access::scoped::ServiceAccountListResponse,
        access::scoped::MembershipResponse,
        access::scoped::MembershipListResponse,
        access::scoped::MembershipRemovalResponse,
        access::scoped::CreateTeamRequest,
        access::scoped::CreateProjectRequest,
        access::scoped::CreateServiceAccountRequest,
        access::scoped::UpdateScopedResourceRequest,
        access::scoped::PutMembershipRequest,
        access::scoped::TeamMutationResponse,
        access::scoped::ProjectMutationResponse,
        access::scoped::ServiceAccountMutationResponse,
        Problem
    )),
    tags(
        (name = "setup"),
        (name = "sessions"),
        (name = "users"),
        (name = "invitations"),
        (name = "teams"),
        (name = "projects"),
        (name = "service-accounts"),
        (name = "memberships"),
    )
)]
pub struct ManagementApiDoc;

#[utoipa::path(
    get,
    path = "/api/v1/openapi.json",
    responses((
        status = 200,
        description = "OpenAPI document for the management API",
        body = serde_json::Value,
        content_type = "application/json"
    ))
)]
async fn openapi() -> Json<serde_json::Value> {
    Json(management_openapi())
}

#[must_use]
pub fn management_openapi() -> serde_json::Value {
    let mut document = ManagementApiDoc::openapi();
    document.merge(crate::management::oidc::openapi());
    document.merge(crate::management::operations::OperationsApiDoc::openapi());
    document.merge(configuration::ConfigurationApiDoc::openapi());
    document.merge(crate::management::playground::PlaygroundApiDoc::openapi());
    openapi::complete_openapi_contract(document)
}

#[cfg(test)]
mod tests;
