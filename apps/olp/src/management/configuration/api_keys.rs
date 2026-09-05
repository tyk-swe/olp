pub(in crate::management) mod create;
pub(in crate::management) mod manage;
mod policy;

pub(super) fn router() -> axum::Router<crate::management::state::ManagementState> {
    use axum::routing::{get, post};
    axum::Router::new()
        .route(
            "/api/v1/api-keys",
            get(manage::list_api_keys).post(create::create_api_key),
        )
        .route(
            "/api/v1/api-keys/{api_key_id}",
            get(manage::get_api_key).patch(manage::update_api_key),
        )
        .route(
            "/api/v1/api-keys/{api_key_id}/rotate",
            post(manage::rotate_api_key),
        )
        .route(
            "/api/v1/api-keys/{api_key_id}/revoke",
            post(create::revoke_api_key),
        )
}
