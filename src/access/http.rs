pub mod auth;

pub mod invitations;

pub mod profile;

pub mod sessions;

pub mod users;

pub(crate) fn router()
-> utoipa_axum::router::OpenApiRouter<crate::http::control::state::ManagementState> {
    utoipa_axum::router::OpenApiRouter::new()
        .routes(utoipa_axum::routes!(
            crate::access::http::auth::authentication_capabilities
        ))
        .routes(utoipa_axum::routes!(
            crate::access::http::auth::current_session
        ))
        .routes(utoipa_axum::routes!(crate::access::http::auth::login))
        .routes(utoipa_axum::routes!(crate::access::http::auth::logout))
        .routes(utoipa_axum::routes!(crate::access::http::auth::setup))
        .routes(utoipa_axum::routes!(
            crate::access::http::auth::setup_status
        ))
        .routes(utoipa_axum::routes!(
            crate::access::http::invitations::accept_invitation
        ))
        .routes(utoipa_axum::routes!(
            crate::access::http::invitations::create_invitation
        ))
        .routes(utoipa_axum::routes!(
            crate::access::http::invitations::list_invitations
        ))
        .routes(utoipa_axum::routes!(
            crate::access::http::invitations::revoke_invitation
        ))
        .routes(utoipa_axum::routes!(
            crate::access::http::profile::change_password
        ))
        .routes(utoipa_axum::routes!(
            crate::access::http::profile::enroll_password
        ))
        .routes(utoipa_axum::routes!(crate::access::http::profile::profile))
        .routes(utoipa_axum::routes!(
            crate::access::http::profile::recent_authentication
        ))
        .routes(utoipa_axum::routes!(
            crate::access::http::profile::update_profile
        ))
        .routes(utoipa_axum::routes!(
            crate::access::http::sessions::list_sessions
        ))
        .routes(utoipa_axum::routes!(
            crate::access::http::sessions::revoke_session
        ))
        .routes(utoipa_axum::routes!(crate::access::http::users::get_user))
        .routes(utoipa_axum::routes!(crate::access::http::users::list_users))
        .routes(utoipa_axum::routes!(
            crate::access::http::users::update_user_role
        ))
}
