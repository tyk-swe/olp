use axum::{
    http::{HeaderValue, header},
    response::Response,
};
use olp_storage::runtime::PublishedRuntimeRelease;
use serde::Serialize;
use utoipa::ToSchema;
use uuid::Uuid;

#[derive(Debug, Serialize, ToSchema)]
pub(crate) struct RuntimeGenerationResponse {
    #[schema(value_type = String, format = Uuid)]
    pub id: Uuid,
    pub sequence: i64,
}

impl From<&PublishedRuntimeRelease> for RuntimeGenerationResponse {
    fn from(release: &PublishedRuntimeRelease) -> Self {
        Self {
            id: release.generation_id,
            sequence: release.sequence,
        }
    }
}

pub(crate) fn prevent_sensitive_response_caching(response: &mut Response) {
    response
        .headers_mut()
        .insert(header::CACHE_CONTROL, HeaderValue::from_static("no-store"));
    response
        .headers_mut()
        .insert(header::PRAGMA, HeaderValue::from_static("no-cache"));
}
