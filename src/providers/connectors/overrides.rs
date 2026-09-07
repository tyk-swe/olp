use std::collections::BTreeMap;
use std::sync::Arc;
use std::sync::RwLock;

use crate::providers::runtime_model::ProviderKind;
use uuid::Uuid;

use crate::providers::openai::transport::Connector;

use crate::providers::connectors::ProviderConnector;

#[derive(Clone, Default)]
pub struct Registry {
    inner: Arc<RwLock<BTreeMap<Uuid, Arc<Connector>>>>,
}

impl Registry {
    pub fn register(&self, provider_id: Uuid, connector: Connector) {
        self.inner
            .write()
            .expect("certification probe connector registry lock poisoned")
            .insert(provider_id, Arc::new(connector));
    }

    pub fn get(&self, provider_id: Uuid, kind: ProviderKind) -> Option<ProviderConnector> {
        if !matches!(kind, ProviderKind::OpenAi | ProviderKind::OpenAiCompatible) {
            return None;
        }
        self.inner
            .read()
            .expect("certification probe connector registry lock poisoned")
            .get(&provider_id)
            .cloned()
            .map(|connector| match kind {
                ProviderKind::OpenAiCompatible => ProviderConnector::OpenAiCompatible(connector),
                _ => ProviderConnector::OpenAi(connector),
            })
    }
}
