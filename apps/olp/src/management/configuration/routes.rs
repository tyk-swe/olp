pub(in crate::management) mod create;
pub(in crate::management) mod manage;

pub(super) fn router() -> axum::Router<crate::management::state::ManagementState> {
    use axum::routing::{get, post};
    axum::Router::new()
        .route(
            "/api/v1/route-drafts",
            get(manage::list_route_drafts).post(create::create_route_draft),
        )
        .route(
            "/api/v1/route-drafts/{draft_id}",
            get(manage::get_route_draft)
                .put(manage::replace_route_draft)
                .delete(manage::delete_route_draft),
        )
        .route(
            "/api/v1/route-drafts/{draft_id}/simulate",
            post(manage::simulate_route_draft),
        )
        .route(
            "/api/v1/route-drafts/{draft_id}/validate",
            post(create::validate_route_draft),
        )
        .route(
            "/api/v1/route-drafts/{draft_id}/activate",
            post(create::activate_route_draft),
        )
        .route("/api/v1/routes", get(manage::list_routes))
        .route("/api/v1/routes/{route_id}", get(manage::get_route))
        .route(
            "/api/v1/routes/{route_id}/revisions",
            get(manage::list_route_revisions),
        )
        .route(
            "/api/v1/routes/{route_id}/revisions/diff",
            get(manage::diff_route_revisions),
        )
        .route(
            "/api/v1/routes/{route_id}/revisions/{revision_id}",
            get(manage::get_route_revision),
        )
        .route(
            "/api/v1/routes/{route_id}/revisions/{revision_id}/restore-as-draft",
            post(manage::restore_route_revision),
        )
}
