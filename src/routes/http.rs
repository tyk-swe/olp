pub mod create;

pub mod manage;

pub(crate) fn router()
-> utoipa_axum::router::OpenApiRouter<crate::http::control::state::ManagementState> {
    utoipa_axum::router::OpenApiRouter::new()
        .routes(utoipa_axum::routes!(
            crate::routes::http::create::activate_route_draft
        ))
        .routes(utoipa_axum::routes!(
            crate::routes::http::create::create_route_draft
        ))
        .routes(utoipa_axum::routes!(
            crate::routes::http::create::validate_route_draft
        ))
        .routes(utoipa_axum::routes!(
            crate::routes::http::manage::delete_route_draft
        ))
        .routes(utoipa_axum::routes!(
            crate::routes::http::manage::diff_route_revisions
        ))
        .routes(utoipa_axum::routes!(crate::routes::http::manage::get_route))
        .routes(utoipa_axum::routes!(
            crate::routes::http::manage::get_route_draft
        ))
        .routes(utoipa_axum::routes!(
            crate::routes::http::manage::get_route_revision
        ))
        .routes(utoipa_axum::routes!(
            crate::routes::http::manage::list_route_drafts
        ))
        .routes(utoipa_axum::routes!(
            crate::routes::http::manage::list_route_revisions
        ))
        .routes(utoipa_axum::routes!(
            crate::routes::http::manage::list_routes
        ))
        .routes(utoipa_axum::routes!(
            crate::routes::http::manage::replace_route_draft
        ))
        .routes(utoipa_axum::routes!(
            crate::routes::http::manage::restore_route_revision
        ))
        .routes(utoipa_axum::routes!(
            crate::routes::http::manage::simulate_route_draft
        ))
}
