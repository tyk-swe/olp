use crate::http::request_admission::public::PublicAdmission;
use crate::inference::executor::Executor;
use crate::observability::cache::ObservabilityCache;
use crate::observability::metrics::RequestMetadataLossCounters;
use crate::process::mode::ApiMode;
use sqlx::PgPool;
use std::sync::Arc;
use std::sync::atomic::AtomicU64;
use std::sync::atomic::Ordering;
/// State installed only on the separately bound private listener.
#[derive(Clone)]
pub struct ObservabilityState {
    pub(crate) pool: PgPool,
    pub(crate) inference: Arc<Executor>,
    pub(crate) public_admission: PublicAdmission,
    pub(crate) media_reconciliation_gaps: Arc<AtomicU64>,
    pub(crate) request_metadata_loss: RequestMetadataLossCounters,
    pub(crate) mode: ApiMode,
    pub(crate) observability: ObservabilityCache,
}

impl ObservabilityState {
    #[must_use]
    pub(crate) fn media_reconciliation_gap_count(&self) -> u64 {
        self.media_reconciliation_gaps.load(Ordering::Relaxed)
    }
}
