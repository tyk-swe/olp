pub mod create;

pub mod manage;

pub mod policy;

pub(crate) fn router()
-> utoipa_axum::router::OpenApiRouter<crate::http::control::state::ManagementState> {
    utoipa_axum::router::OpenApiRouter::new()
        .routes(utoipa_axum::routes!(
            crate::access::api_keys::http::create::create_api_key
        ))
        .routes(utoipa_axum::routes!(
            crate::access::api_keys::http::create::revoke_api_key
        ))
        .routes(utoipa_axum::routes!(
            crate::access::api_keys::http::manage::get_api_key
        ))
        .routes(utoipa_axum::routes!(
            crate::access::api_keys::http::manage::list_api_keys
        ))
        .routes(utoipa_axum::routes!(
            crate::access::api_keys::http::manage::rotate_api_key
        ))
        .routes(utoipa_axum::routes!(
            crate::access::api_keys::http::manage::update_api_key
        ))
}
