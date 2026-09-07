#[cfg(test)]
use crate::crypto::key_material::AuthHmacKey;
use crate::http::state::RequestBoundaryState;
#[cfg(test)]
use crate::inference::transport::MediaSpool;
#[cfg(test)]
use crate::limits::admission::ReloadableLimiter;
#[cfg(any(test, feature = "test-util"))]
use crate::runtime::manager::Manager;
#[cfg(test)]
use crate::usage::emitter::Emitter;
use std::sync::Arc;
/// Gateway HTTP dependencies plus the shared inference service.
#[derive(Clone)]
pub struct GatewayState {
    pub(crate) request_boundary: RequestBoundaryState,
    pub(crate) cors_allowed_origins: Arc<[axum::http::HeaderValue]>,
    pub media_jobs: crate::media::service::MediaJobs,
}

impl GatewayState {
    #[must_use]
    #[cfg(any(test, feature = "test-util"))]
    pub fn runtime(&self) -> &Manager {
        &self.request_boundary.inference.runtime
    }

    #[must_use]
    #[cfg(test)]
    pub(crate) fn limiter(&self) -> &ReloadableLimiter {
        &self.request_boundary.inference.limiter
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
