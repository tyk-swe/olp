use serde::{Deserialize, Serialize};
use sqlx::FromRow;
use utoipa::ToSchema;

use crate::providers::{runtime_model::ProviderKind, types::ProviderAuthMode};

#[derive(Clone, Debug, Deserialize, Serialize, ToSchema, FromRow, Eq, PartialEq)]
#[serde(deny_unknown_fields)]
pub struct ProviderConfiguration {
    #[sqlx(try_from = "String")]
    pub kind: ProviderKind,
    #[serde(default)]
    #[sqlx(json, default)]
    pub options: crate::providers::options::ConnectionOptions,
    pub endpoint: Option<String>,
    pub cloud_region: Option<String>,
    pub cloud_project: Option<String>,
    pub deployment: Option<String>,
    pub api_version: Option<String>,
    #[sqlx(try_from = "String")]
    pub auth_mode: ProviderAuthMode,
    #[sqlx(default)]
    #[serde(skip)]
    #[schema(ignore)]
    pub probe_model: Option<String>,
}

impl ProviderConfiguration {
    pub fn new(kind: ProviderKind) -> Self {
        Self {
            kind,
            options: Default::default(),
            auth_mode: crate::providers::validation::provider_kind_spec(kind).default_auth_mode,
            probe_model: None,
            endpoint: None,
            cloud_region: None,
            cloud_project: None,
            deployment: None,
            api_version: None,
        }
    }
}

impl TryFrom<String> for ProviderKind {
    type Error = crate::providers::runtime_model::InvalidProviderKind;

    fn try_from(value: String) -> Result<Self, Self::Error> {
        value.parse()
    }
}

impl TryFrom<String> for ProviderAuthMode {
    type Error = crate::providers::types::ClosedSetParseError;

    fn try_from(value: String) -> Result<Self, Self::Error> {
        value.parse()
    }
}
