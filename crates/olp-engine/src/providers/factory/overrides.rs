use std::{
    collections::BTreeMap,
    sync::{Arc, RwLock},
};

use crate::domain::routing::provider::ProviderKind;
use uuid::Uuid;

use crate::providers::openai::transport::Connector;

use super::assembly::{ConcreteConnector, Facade};

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

    pub fn get(&self, provider_id: Uuid, kind: ProviderKind) -> Option<Facade> {
        if !matches!(kind, ProviderKind::OpenAi | ProviderKind::OpenAiCompatible) {
            return None;
        }
        self.inner
            .read()
            .expect("certification probe connector registry lock poisoned")
            .get(&provider_id)
            .cloned()
            .map(|connector| Facade {
                kind,
                connector: ConcreteConnector::OpenAi(connector),
            })
    }
}
