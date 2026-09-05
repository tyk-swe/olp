use olp_engine::domain::{ids::ProviderId, ports::ProviderTransport};
use std::{
    collections::BTreeMap,
    sync::{Arc, RwLock},
};
#[derive(Clone, Default)]
pub struct TransportRegistry {
    inner: Arc<RwLock<BTreeMap<ProviderId, Arc<dyn ProviderTransport>>>>,
}

impl TransportRegistry {
    pub fn register(&self, provider_id: ProviderId, transport: Arc<dyn ProviderTransport>) {
        self.inner
            .write()
            .expect("transport registry lock poisoned")
            .insert(provider_id, transport);
    }

    #[must_use]
    pub fn snapshot(&self) -> BTreeMap<ProviderId, Arc<dyn ProviderTransport>> {
        self.inner
            .read()
            .expect("transport registry lock poisoned")
            .clone()
    }
}
