use axum::Json;

use crate::http::control::state::ManagementState;

#[utoipa::path(
    get,
    path = "/api/v3/openapi.json",
    responses((
        status = 200,
        description = "OpenAPI document for the management API",
        body = serde_json::Value,
        content_type = "application/json"
    ))
, security())]
async fn openapi() -> Json<serde_json::Value> {
    Json(openapi::document())
}

pub mod configuration;

pub mod error_mapping;

pub mod idempotency;

pub mod json_payload;

pub mod openapi;

pub mod operations;

pub mod pagination;

pub mod preconditions;

pub mod provenance;

pub mod response_policy;

pub mod secrets;

pub mod state;

#[cfg(test)]
pub mod tests;

pub fn router() -> axum::Router<ManagementState> {
    routes().split_for_parts().0
}

pub(crate) fn routes() -> utoipa_axum::router::OpenApiRouter<ManagementState> {
    utoipa_axum::router::OpenApiRouter::new()
        .routes(utoipa_axum::routes!(openapi))
        .merge(crate::access::api_keys::http::router())
        .merge(crate::access::audit_http::router())
        .merge(crate::access::http::router())
        .merge(crate::access::oidc::http::router())
        .merge(crate::inference::playground::router())
        .merge(crate::media::http::router())
        .merge(crate::observability::health_http::router())
        .merge(crate::providers::http::router())
        .merge(crate::routes::http::router())
        .merge(crate::runtime::http::router())
        .merge(crate::settings::http::router())
        .merge(crate::usage::delivery_http::router())
        .merge(crate::usage::history_http::router())
        .merge(crate::usage::http::router())
        .merge(crate::usage::pricing_http::router())
}
