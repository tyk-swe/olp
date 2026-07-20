use std::{
    collections::BTreeMap,
    sync::{Arc, RwLock},
};

use olp_domain::ProviderKind;
use uuid::Uuid;

use crate::openai::OpenAiConnector;

use super::assembly::{ConcreteConnector, ConcreteProvider, ProviderFacade};

#[derive(Clone, Default)]
pub struct OpenAiConnectorOverrideRegistry {
    inner: Arc<RwLock<BTreeMap<Uuid, Arc<OpenAiConnector>>>>,
}

impl OpenAiConnectorOverrideRegistry {
    pub fn register(&self, provider_id: Uuid, connector: OpenAiConnector) {
        self.inner
            .write()
            .expect("certification probe connector registry lock poisoned")
            .insert(provider_id, Arc::new(connector));
    }

    pub fn get(&self, provider_id: Uuid, kind: ProviderKind) -> Option<ProviderFacade> {
        if !matches!(kind, ProviderKind::OpenAi | ProviderKind::OpenAiCompatible) {
            return None;
        }
        self.inner
            .read()
            .expect("certification probe connector registry lock poisoned")
            .get(&provider_id)
            .cloned()
            .map(|connector| ProviderFacade {
                inner: ConcreteProvider {
                    kind,
                    connector: ConcreteConnector::OpenAi(connector),
                },
            })
    }
}
