//! Logical credential slots, independent of encrypted secret versions.
use serde::{Deserialize, Serialize};
use utoipa::ToSchema;
use uuid::Uuid;

#[derive(Clone, Debug, Deserialize, Serialize, ToSchema, Eq, PartialEq)]
#[serde(default, deny_unknown_fields)]
pub struct CredentialSlot {
    pub id: Uuid,
    pub name: String,
    pub enabled: bool,
    pub priority: u16,
    pub weight: u32,
    pub credential_version_id: Option<Uuid>,
    pub allowed_models: Vec<String>,
    pub allowed_routes: Vec<String>,
    pub allowed_api_keys: Vec<Uuid>,
    pub requests_per_minute: Option<u32>,
    pub tokens_per_minute: Option<u64>,
    pub max_concurrency: Option<u32>,
}

impl Default for CredentialSlot {
    fn default() -> Self {
        Self {
            id: Uuid::nil(),
            name: String::new(),
            enabled: true,
            priority: 0,
            weight: 1,
            credential_version_id: None,
            allowed_models: Vec::new(),
            allowed_routes: Vec::new(),
            allowed_api_keys: Vec::new(),
            requests_per_minute: None,
            tokens_per_minute: None,
            max_concurrency: None,
        }
    }
}

impl CredentialSlot {
    pub fn validate(&self) -> Result<(), String> {
        if self.name.trim().is_empty()
            || self.name.len() > 100
            || self.weight == 0
            || self.weight > i32::MAX as u32
        {
            return Err("Slot requires a name and positive weight".into());
        }
        if self.allowed_models.len() > 2_000
            || self.allowed_routes.len() > 100
            || self.allowed_api_keys.len() > 100
        {
            return Err("Credential restrictions exceed the collection limit".into());
        }
        for route in &self.allowed_routes {
            crate::ids::RouteSlug::parse(route.clone()).map_err(|_| "Invalid allowed route")?;
        }
        self.limits()
            .validate()
            .map_err(|_| "Invalid credential quota".to_owned())
    }
    /// Per-slot quotas share the connection quota representation and checks.
    pub fn limits(&self) -> crate::providers::options::ConnectionLimits {
        crate::providers::options::ConnectionLimits {
            requests_per_minute: self.requests_per_minute,
            tokens_per_minute: self.tokens_per_minute,
            max_concurrency: self.max_concurrency,
        }
    }
    pub fn permits(&self, model: &str, route: &str, key: Option<Uuid>) -> bool {
        self.enabled
            && (self.allowed_models.is_empty() || self.allowed_models.iter().any(|s| s == model))
            && (self.allowed_routes.is_empty() || self.allowed_routes.iter().any(|s| s == route))
            && (self.allowed_api_keys.is_empty()
                || key.is_some_and(|id| self.allowed_api_keys.contains(&id)))
    }
}
