use olp_db::{
    configuration::resources::ProviderRecord, runtime::provider_configuration::RuntimeProvider,
};
use olp_engine::domain::{
    provider::ProviderAuthMode, provider_configuration::Configuration,
    routing::provider::ProviderKind,
};
/// Application-owned provider fields before they cross into `olp-engine::providers`.
#[derive(Clone, Copy, Debug)]
pub(crate) struct ProviderConfigFields<'a> {
    pub kind: ProviderKind,
    pub endpoint: Option<&'a str>,
    pub cloud_region: Option<&'a str>,
    pub cloud_project: Option<&'a str>,
    pub deployment: Option<&'a str>,
    pub api_version: Option<&'a str>,
    pub auth_mode: ProviderAuthMode,
    pub probe_model: Option<&'a str>,
}

impl<'a> From<&'a ProviderRecord> for ProviderConfigFields<'a> {
    fn from(provider: &'a ProviderRecord) -> Self {
        Self {
            kind: provider.kind,
            endpoint: provider.endpoint.as_deref(),
            cloud_region: provider.cloud_region.as_deref(),
            cloud_project: provider.cloud_project.as_deref(),
            deployment: provider.deployment.as_deref(),
            api_version: provider.api_version.as_deref(),
            auth_mode: provider.auth_mode,
            probe_model: provider.probe_model.as_deref(),
        }
    }
}

impl<'a> From<&'a RuntimeProvider> for ProviderConfigFields<'a> {
    fn from(provider: &'a RuntimeProvider) -> Self {
        Self {
            kind: provider.kind,
            endpoint: provider.endpoint.as_deref(),
            cloud_region: provider.cloud_region.as_deref(),
            cloud_project: provider.cloud_project.as_deref(),
            deployment: provider.deployment.as_deref(),
            api_version: provider.api_version.as_deref(),
            auth_mode: provider.auth_mode,
            probe_model: None,
        }
    }
}

impl<'a> From<ProviderConfigFields<'a>> for Configuration<'a> {
    fn from(fields: ProviderConfigFields<'a>) -> Self {
        Self {
            kind: fields.kind,
            auth_mode: fields.auth_mode,
            endpoint: fields.endpoint,
            cloud_region: fields.cloud_region,
            cloud_project: fields.cloud_project,
            deployment: fields.deployment,
            api_version: fields.api_version,
            model: fields.probe_model,
            credential_present: None,
        }
    }
}
