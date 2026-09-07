pub mod authorization;

pub mod callback;

pub mod claims;

pub mod configuration;

pub mod error;

pub mod helpers;

pub mod identities;

pub mod session;

#[cfg(test)]
pub mod tests;

pub(crate) fn router()
-> utoipa_axum::router::OpenApiRouter<crate::http::control::state::ManagementState> {
    utoipa_axum::router::OpenApiRouter::new()
        .routes(utoipa_axum::routes!(
            crate::access::oidc::http::authorization::begin_link
        ))
        .routes(utoipa_axum::routes!(
            crate::access::oidc::http::authorization::begin_login
        ))
        .routes(utoipa_axum::routes!(
            crate::access::oidc::http::authorization::begin_login_post
        ))
        .routes(utoipa_axum::routes!(
            crate::access::oidc::http::authorization::begin_reauthentication
        ))
        .routes(utoipa_axum::routes!(
            crate::access::oidc::http::callback::callback
        ))
        .routes(utoipa_axum::routes!(
            crate::access::oidc::http::configuration::get_configuration
        ))
        .routes(utoipa_axum::routes!(
            crate::access::oidc::http::configuration::put_configuration
        ))
        .routes(utoipa_axum::routes!(
            crate::access::oidc::http::identities::list_identities
        ))
        .routes(utoipa_axum::routes!(
            crate::access::oidc::http::identities::unlink_identity
        ))
}
