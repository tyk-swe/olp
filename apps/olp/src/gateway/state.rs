use crate::public_http::{body_limits::BodyLimits, state::RequestBoundaryState};
#[cfg(test)]
use olp_db::security::key_material::AuthHmacKey;
use olp_db::store::Store;
#[cfg(any(test, feature = "test-util"))]
use olp_engine::inference::runtime::Manager;
#[cfg(test)]
use olp_engine::inference::{limits::ReloadableLimiter, request_metadata::Emitter};
use olp_engine::{domain::ports::MediaSpool, inference::service::Service};
use std::sync::Arc;
/// Gateway HTTP dependencies plus the shared inference service.
#[derive(Clone)]
pub struct GatewayState {
    pub(crate) request_boundary: RequestBoundaryState,
    pub(crate) cors_allowed_origins: Arc<[axum::http::HeaderValue]>,
    pub media_jobs: crate::application::media_jobs::MediaJobs,
}

impl GatewayState {
    #[must_use]
    pub fn store(&self) -> &Store {
        &self.request_boundary.store
    }

    #[must_use]
    #[cfg(any(test, feature = "test-util"))]
    pub fn runtime(&self) -> &Manager {
        self.inference().runtime()
    }

    #[must_use]
    pub(crate) fn inference(&self) -> &Service {
        &self.request_boundary.inference
    }

    #[must_use]
    #[cfg(test)]
    pub(crate) fn limiter(&self) -> &ReloadableLimiter {
        self.inference().limiter()
    }

    #[must_use]
    pub(crate) fn media_spool(&self) -> &Arc<dyn MediaSpool> {
        self.inference().media_spool()
    }

    #[must_use]
    pub(crate) const fn body_limits(&self) -> BodyLimits {
        self.request_boundary.body_limits
    }

    #[must_use]
    #[cfg(test)]
    pub(crate) fn request_boundary(&self) -> &RequestBoundaryState {
        &self.request_boundary
    }

    #[must_use]
    #[cfg(test)]
    pub(crate) fn auth_hmac_key(&self) -> &Arc<AuthHmacKey> {
        &self.request_boundary.auth_hmac_key
    }

    #[cfg(test)]
    pub(crate) async fn verify_bootstrap_token(&self, supplied: Option<&str>) -> Option<bool> {
        self.request_boundary.verify_bootstrap_token(supplied).await
    }

    #[cfg(test)]
    pub(crate) async fn clear_bootstrap_token(&self) {
        self.request_boundary.clear_bootstrap_token().await;
    }

    #[cfg(test)]
    pub(crate) fn replace_request_metadata_for_test(&mut self, emitter: Emitter) {
        Arc::make_mut(&mut self.request_boundary.inference).replace_request_metadata(Some(emitter));
    }

    #[cfg(test)]
    pub(crate) fn replace_media_spool_for_test(&mut self, media_spool: Arc<dyn MediaSpool>) {
        Arc::make_mut(&mut self.request_boundary.inference).replace_media_spool(media_spool);
    }

    #[cfg(test)]
    pub(crate) fn replace_auth_hmac_key_for_test(&mut self, auth_hmac_key: Arc<AuthHmacKey>) {
        self.request_boundary.auth_hmac_key = auth_hmac_key;
    }
}
