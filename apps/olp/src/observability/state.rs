use super::{cache::ObservabilityCache, metrics::RequestMetadataLossCounters};
use crate::{application::mode::ApiMode, public_http::request_admission::public::PublicAdmission};
use olp_db::store::Store;
use olp_engine::{
    domain::ports::MediaSpool,
    inference::{
        limits::ReloadableLimiter, request_metadata::Emitter, runtime::Manager, service::Service,
    },
};
use std::sync::{
    Arc,
    atomic::{AtomicU64, Ordering},
};
/// State installed only on the separately bound private listener.
#[derive(Clone)]
pub struct ObservabilityState {
    pub(crate) store: Store,
    pub(crate) inference: Arc<Service>,
    pub(crate) public_admission: PublicAdmission,
    pub(crate) media_reconciliation_gaps: Arc<AtomicU64>,
    pub(crate) request_metadata_loss: RequestMetadataLossCounters,
    pub(crate) mode: ApiMode,
    pub(crate) observability: ObservabilityCache,
}

impl ObservabilityState {
    #[must_use]
    pub(crate) const fn store(&self) -> &Store {
        &self.store
    }

    #[must_use]
    pub(crate) fn runtime(&self) -> &Manager {
        self.inference.runtime()
    }

    #[must_use]
    pub(crate) fn limiter(&self) -> &ReloadableLimiter {
        self.inference.limiter()
    }

    #[must_use]
    pub(crate) fn circuits(&self) -> &olp_engine::inference::circuit::Breaker {
        self.inference.circuits()
    }

    #[must_use]
    pub(crate) fn request_metadata(&self) -> Option<&Emitter> {
        self.inference.request_metadata()
    }

    #[must_use]
    pub(crate) fn media_reconciliation_gap_count(&self) -> u64 {
        self.media_reconciliation_gaps.load(Ordering::Relaxed)
    }

    #[must_use]
    pub(crate) fn media_spool(&self) -> &Arc<dyn MediaSpool> {
        self.inference.media_spool()
    }

    #[must_use]
    pub(crate) const fn request_metadata_loss_counters(&self) -> &RequestMetadataLossCounters {
        &self.request_metadata_loss
    }
}
