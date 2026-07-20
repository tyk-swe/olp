mod api_keys;
mod auth;
mod common;
mod openapi;
mod providers;
mod routes;
mod team;

use api_keys::{CreateApiKeyRequest, CreateApiKeyResponse, create_api_key, revoke_api_key};
use auth::{
    LoginRequest, SessionDetailResponse, SessionListResponse, SessionResponse, SetupRequest,
    SetupStatus, UserResponse, current_session, list_sessions, login, logout, revoke_session,
    setup, setup_status,
};
use axum::{Json, Router, routing::get, routing::post};
use common::RuntimeGenerationResponse;
pub(crate) use common::{
    CSRF_HEADER, SETUP_TOKEN_HEADER, WriteOnlySecret, enforce_origin, idempotency_http_response,
    if_match, json_payload, map_persistence, require_idempotency_key, require_mutation_session,
    require_permission, require_read_session, require_store,
};
pub(crate) use olp_domain::Permission;
use providers::{
    CreateProviderRequest, ProviderActivationResponse, ProviderResponse, activate_provider,
    create_provider,
};
use routes::{
    CreateRouteDraftRequest, RouteActivationResponse, RouteDraftResponse, RouteTargetRequest,
    activate_route_draft, create_route_draft, validate_route_draft,
};
use team::{
    AcceptInvitationRequest, ChangePasswordRequest, CreateInvitationRequest,
    CreateInvitationResponse, EnrollPasswordRequest, InvitationListResponse, InvitationResponse,
    UpdateProfileRequest, UpdateUserRoleRequest, UserDetailResponse, UserListResponse,
    accept_invitation, change_password, create_invitation, enroll_password, get_user,
    list_invitations, list_users, profile, revoke_invitation, update_profile, update_user_role,
};
use utoipa::OpenApi;

use crate::{ApiState, Problem};

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
        auth::setup_status,
        auth::setup,
        auth::login,
        auth::current_session,
        auth::logout,
        team::profile,
        team::update_profile,
        team::change_password,
        team::enroll_password,
        auth::list_sessions,
        auth::revoke_session,
        team::list_users,
        team::get_user,
        team::update_user_role,
        team::list_invitations,
        team::create_invitation,
        team::revoke_invitation,
        team::accept_invitation,
        providers::create_provider,
        providers::activate_provider,
        api_keys::create_api_key,
        api_keys::revoke_api_key,
        routes::create_route_draft,
        routes::validate_route_draft,
        routes::activate_route_draft
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
    openapi::complete_openapi_contract(document)
}

#[cfg(test)]
mod tests;
