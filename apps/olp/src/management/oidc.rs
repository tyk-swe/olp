mod authorization;
mod callback;
mod claims;
mod configuration;
pub(crate) mod error;
mod helpers;
mod identities;
mod session;

use authorization::{
    OidcAuthorizationResponse, OidcLoginRequest, OidcReauthenticationRequest, begin_link,
    begin_login, begin_login_post, begin_reauthentication,
};
use axum::{Router, routing::get, routing::post};
use callback::callback;
use configuration::{
    OidcConfigurationRequest, OidcConfigurationResponse, OidcRoleMappingRequest,
    OidcRoleMappingResponse,
};
use configuration::{get_configuration, put_configuration};
use identities::{
    OidcIdentityListResponse, OidcIdentityResponse, list_identities, unlink_identity,
};
use utoipa::OpenApi;

use crate::{
    bootstrap::mode_dependencies::ManagementState, public_http::problem::Problem,
    public_http::public_auth_routes::PublicAuthRoute,
};

pub(crate) fn router() -> Router<ManagementState> {
    Router::new()
        .route(
            "/api/v1/oidc/configuration",
            get(get_configuration).put(put_configuration),
        )
        .route(
            PublicAuthRoute::OidcLoginGet.path(),
            get(begin_login).post(begin_login_post),
        )
        .route("/api/v1/oidc/link", post(begin_link))
        .route("/api/v1/oidc/reauthenticate", post(begin_reauthentication))
        .route("/api/v1/oidc/identities", get(list_identities))
        .route(
            "/api/v1/oidc/identities/{identity_id}",
            axum::routing::delete(unlink_identity),
        )
        .route("/api/v1/oidc/callback", get(callback))
}

#[derive(OpenApi)]
#[openapi(
    paths(
        configuration::get_configuration,
        configuration::put_configuration,
        authorization::begin_login,
        authorization::begin_login_post,
        authorization::begin_link,
        authorization::begin_reauthentication,
        identities::list_identities,
        identities::unlink_identity,
        callback::callback
    ),
    components(schemas(
        OidcConfigurationRequest,
        OidcConfigurationResponse,
        OidcRoleMappingRequest,
        OidcRoleMappingResponse,
        OidcAuthorizationResponse,
        OidcLoginRequest,
        OidcReauthenticationRequest,
        OidcIdentityResponse,
        OidcIdentityListResponse,
        Problem
    )),
    tags((name = "oidc"))
)]
pub(crate) struct OidcApiDoc;

pub(crate) fn openapi() -> utoipa::openapi::OpenApi {
    OidcApiDoc::openapi()
}

#[cfg(test)]
mod tests;
