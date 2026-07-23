//! Canonical provider configuration capabilities and validation.

use serde::{Deserialize, Serialize};
use utoipa::ToSchema;

use crate::{ProviderAuthMode, ProviderKind, Surface};

#[derive(Clone, Copy, Debug, Deserialize, Eq, Hash, PartialEq, Serialize, ToSchema)]
#[serde(rename_all = "snake_case")]
pub enum ProviderConfigurationField {
    Endpoint,
    CloudRegion,
    CloudProject,
    Deployment,
    ApiVersion,
    Model,
}

impl ProviderConfigurationField {
    pub const ALL: [Self; 6] = [
        Self::Endpoint,
        Self::CloudRegion,
        Self::CloudProject,
        Self::Deployment,
        Self::ApiVersion,
        Self::Model,
    ];

    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::Endpoint => "endpoint",
            Self::CloudRegion => "cloud_region",
            Self::CloudProject => "cloud_project",
            Self::Deployment => "deployment",
            Self::ApiVersion => "api_version",
            Self::Model => "model",
        }
    }
}

#[derive(Clone, Copy, Debug, Deserialize, Eq, PartialEq, Serialize, ToSchema)]
#[serde(rename_all = "snake_case")]
pub enum CredentialRequirement {
    Required,
    Forbidden,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct ProviderAuthModeSpec {
    pub mode: ProviderAuthMode,
    pub label: &'static str,
    pub credential: CredentialRequirement,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct ProviderFieldSpec {
    pub field: ProviderConfigurationField,
    pub label: &'static str,
    pub required: bool,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct ProviderKindSpec {
    pub kind: ProviderKind,
    pub label: &'static str,
    pub description: &'static str,
    pub seed_surface: Option<Surface>,
    pub default_auth_mode: ProviderAuthMode,
    pub auth_modes: &'static [ProviderAuthModeSpec],
    pub fields: &'static [ProviderFieldSpec],
}

impl ProviderKindSpec {
    #[must_use]
    pub fn auth_mode(self, mode: ProviderAuthMode) -> Option<&'static ProviderAuthModeSpec> {
        self.auth_modes
            .iter()
            .find(|candidate| candidate.mode == mode)
    }

    #[must_use]
    pub fn field(self, field: ProviderConfigurationField) -> Option<&'static ProviderFieldSpec> {
        self.fields
            .iter()
            .find(|candidate| candidate.field == field)
    }
}

const API_KEY_AUTH: [ProviderAuthModeSpec; 1] = [ProviderAuthModeSpec {
    mode: ProviderAuthMode::ApiKey,
    label: "Stored API key",
    credential: CredentialRequirement::Required,
}];
const VERTEX_AUTH: [ProviderAuthModeSpec; 2] = [
    ProviderAuthModeSpec {
        mode: ProviderAuthMode::ApplicationDefault,
        label: "Application Default Credentials",
        credential: CredentialRequirement::Forbidden,
    },
    ProviderAuthModeSpec {
        mode: ProviderAuthMode::ServiceAccount,
        label: "Stored service account JSON",
        credential: CredentialRequirement::Required,
    },
];
const BEDROCK_AUTH: [ProviderAuthModeSpec; 2] = [
    ProviderAuthModeSpec {
        mode: ProviderAuthMode::DefaultChain,
        label: "AWS default chain",
        credential: CredentialRequirement::Forbidden,
    },
    ProviderAuthModeSpec {
        mode: ProviderAuthMode::Static,
        label: "Stored static AWS credential",
        credential: CredentialRequirement::Required,
    },
];

const MODEL_FIELD: ProviderFieldSpec = ProviderFieldSpec {
    field: ProviderConfigurationField::Model,
    label: "Seed model",
    required: false,
};
const COMMON_FIELDS: [ProviderFieldSpec; 1] = [MODEL_FIELD];
const COMPATIBLE_FIELDS: [ProviderFieldSpec; 2] = [
    ProviderFieldSpec {
        field: ProviderConfigurationField::Endpoint,
        label: "HTTPS endpoint",
        required: true,
    },
    MODEL_FIELD,
];
const VERTEX_FIELDS: [ProviderFieldSpec; 3] = [
    ProviderFieldSpec {
        field: ProviderConfigurationField::CloudProject,
        label: "Cloud project",
        required: true,
    },
    ProviderFieldSpec {
        field: ProviderConfigurationField::CloudRegion,
        label: "Cloud location",
        required: true,
    },
    ProviderFieldSpec {
        field: ProviderConfigurationField::Model,
        label: "Probe model",
        required: true,
    },
];
const BEDROCK_FIELDS: [ProviderFieldSpec; 2] = [
    ProviderFieldSpec {
        field: ProviderConfigurationField::CloudRegion,
        label: "AWS region",
        required: true,
    },
    MODEL_FIELD,
];
const AZURE_FIELDS: [ProviderFieldSpec; 4] = [
    ProviderFieldSpec {
        field: ProviderConfigurationField::Endpoint,
        label: "Resource endpoint",
        required: true,
    },
    ProviderFieldSpec {
        field: ProviderConfigurationField::Deployment,
        label: "Deployment",
        required: true,
    },
    ProviderFieldSpec {
        field: ProviderConfigurationField::ApiVersion,
        label: "API version",
        required: true,
    },
    MODEL_FIELD,
];

const PROVIDER_KIND_SPECS: [ProviderKindSpec; 7] = [
    ProviderKindSpec {
        kind: ProviderKind::OpenAi,
        label: "OpenAI",
        description: "Official OpenAI HTTPS API",
        seed_surface: Some(Surface::OpenAi),
        default_auth_mode: ProviderAuthMode::ApiKey,
        auth_modes: &API_KEY_AUTH,
        fields: &COMMON_FIELDS,
    },
    ProviderKindSpec {
        kind: ProviderKind::Anthropic,
        label: "Anthropic",
        description: "Native Messages API",
        seed_surface: Some(Surface::Anthropic),
        default_auth_mode: ProviderAuthMode::ApiKey,
        auth_modes: &API_KEY_AUTH,
        fields: &COMMON_FIELDS,
    },
    ProviderKindSpec {
        kind: ProviderKind::Gemini,
        label: "Gemini Developer API",
        description: "Google AI API key",
        seed_surface: Some(Surface::Gemini),
        default_auth_mode: ProviderAuthMode::ApiKey,
        auth_modes: &API_KEY_AUTH,
        fields: &COMMON_FIELDS,
    },
    ProviderKindSpec {
        kind: ProviderKind::VertexAi,
        label: "Vertex AI",
        description: "Google Cloud identity",
        seed_surface: Some(Surface::Gemini),
        default_auth_mode: ProviderAuthMode::ApplicationDefault,
        auth_modes: &VERTEX_AUTH,
        fields: &VERTEX_FIELDS,
    },
    ProviderKindSpec {
        kind: ProviderKind::Bedrock,
        label: "AWS Bedrock",
        description: "AWS default chain or static credentials",
        seed_surface: None,
        default_auth_mode: ProviderAuthMode::DefaultChain,
        auth_modes: &BEDROCK_AUTH,
        fields: &BEDROCK_FIELDS,
    },
    ProviderKindSpec {
        kind: ProviderKind::AzureOpenAi,
        label: "Azure OpenAI",
        description: "Azure deployment endpoint",
        seed_surface: Some(Surface::OpenAi),
        default_auth_mode: ProviderAuthMode::ApiKey,
        auth_modes: &API_KEY_AUTH,
        fields: &AZURE_FIELDS,
    },
    ProviderKindSpec {
        kind: ProviderKind::OpenAiCompatible,
        label: "OpenAI-compatible",
        description: "Explicit custom HTTPS endpoint",
        seed_surface: Some(Surface::OpenAi),
        default_auth_mode: ProviderAuthMode::ApiKey,
        auth_modes: &API_KEY_AUTH,
        fields: &COMPATIBLE_FIELDS,
    },
];

#[must_use]
pub const fn provider_kind_specs() -> &'static [ProviderKindSpec] {
    &PROVIDER_KIND_SPECS
}

#[must_use]
pub fn provider_kind_spec(kind: ProviderKind) -> &'static ProviderKindSpec {
    // The exhaustive match makes a newly added domain kind fail to compile here.
    let index = match kind {
        ProviderKind::OpenAi => 0,
        ProviderKind::Anthropic => 1,
        ProviderKind::Gemini => 2,
        ProviderKind::VertexAi => 3,
        ProviderKind::Bedrock => 4,
        ProviderKind::AzureOpenAi => 5,
        ProviderKind::OpenAiCompatible => 6,
    };
    &PROVIDER_KIND_SPECS[index]
}

#[derive(Clone, Copy, Debug)]
pub struct ProviderConfiguration<'a> {
    pub kind: ProviderKind,
    pub auth_mode: ProviderAuthMode,
    pub endpoint: Option<&'a str>,
    pub cloud_region: Option<&'a str>,
    pub cloud_project: Option<&'a str>,
    pub deployment: Option<&'a str>,
    pub api_version: Option<&'a str>,
    pub model: Option<&'a str>,
    /// `None` validates non-secret configuration only. Management writes use
    /// `Some` so credential-required and credential-forbidden rules are enforced.
    pub credential_present: Option<bool>,
}

impl<'a> ProviderConfiguration<'a> {
    fn value(self, field: ProviderConfigurationField) -> Option<&'a str> {
        match field {
            ProviderConfigurationField::Endpoint => self.endpoint,
            ProviderConfigurationField::CloudRegion => self.cloud_region,
            ProviderConfigurationField::CloudProject => self.cloud_project,
            ProviderConfigurationField::Deployment => self.deployment,
            ProviderConfigurationField::ApiVersion => self.api_version,
            ProviderConfigurationField::Model => self.model,
        }
    }
}

#[derive(Clone, Copy, Debug, Deserialize, Eq, Hash, PartialEq, Serialize, ToSchema)]
#[serde(rename_all = "snake_case")]
pub enum ProviderViolationField {
    AuthMode,
    Credential,
    Endpoint,
    CloudRegion,
    CloudProject,
    Deployment,
    ApiVersion,
    Model,
}

impl ProviderViolationField {
    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::AuthMode => "auth_mode",
            Self::Credential => "credential",
            Self::Endpoint => "endpoint",
            Self::CloudRegion => "cloud_region",
            Self::CloudProject => "cloud_project",
            Self::Deployment => "deployment",
            Self::ApiVersion => "api_version",
            Self::Model => "model",
        }
    }
}

impl From<ProviderConfigurationField> for ProviderViolationField {
    fn from(value: ProviderConfigurationField) -> Self {
        match value {
            ProviderConfigurationField::Endpoint => Self::Endpoint,
            ProviderConfigurationField::CloudRegion => Self::CloudRegion,
            ProviderConfigurationField::CloudProject => Self::CloudProject,
            ProviderConfigurationField::Deployment => Self::Deployment,
            ProviderConfigurationField::ApiVersion => Self::ApiVersion,
            ProviderConfigurationField::Model => Self::Model,
        }
    }
}

#[derive(Clone, Copy, Debug, Deserialize, Eq, PartialEq, Serialize, ToSchema)]
#[serde(rename_all = "snake_case")]
pub enum ProviderViolationCode {
    UnsupportedAuthMode,
    Required,
    Forbidden,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct ProviderConfigurationViolation {
    pub field: ProviderViolationField,
    pub code: ProviderViolationCode,
    pub detail: &'static str,
}

#[must_use]
pub fn validate_provider_configuration(
    configuration: ProviderConfiguration<'_>,
) -> Vec<ProviderConfigurationViolation> {
    let spec = provider_kind_spec(configuration.kind);
    let mut violations = Vec::new();

    let auth = spec.auth_mode(configuration.auth_mode);
    if auth.is_none() {
        violations.push(ProviderConfigurationViolation {
            field: ProviderViolationField::AuthMode,
            code: ProviderViolationCode::UnsupportedAuthMode,
            detail: unsupported_auth_detail(configuration.kind),
        });
    }

    for field in ProviderConfigurationField::ALL {
        match (spec.field(field), configuration.value(field)) {
            (Some(field_spec), value)
                if field_spec.required && value.is_none_or(|value| value.trim().is_empty()) =>
            {
                violations.push(ProviderConfigurationViolation {
                    field: field.into(),
                    code: ProviderViolationCode::Required,
                    detail: required_field_detail(configuration.kind, field),
                });
            }
            (None, Some(_)) => violations.push(ProviderConfigurationViolation {
                field: field.into(),
                code: ProviderViolationCode::Forbidden,
                detail: forbidden_field_detail(configuration.kind, field),
            }),
            _ => {}
        }
    }

    if let (Some(auth), Some(credential_present)) = (auth, configuration.credential_present) {
        match (auth.credential, credential_present) {
            (CredentialRequirement::Required, false) => {
                violations.push(ProviderConfigurationViolation {
                    field: ProviderViolationField::Credential,
                    code: ProviderViolationCode::Required,
                    detail: "This authentication mode requires a write-only credential.",
                });
            }
            (CredentialRequirement::Forbidden, true) => {
                violations.push(ProviderConfigurationViolation {
                    field: ProviderViolationField::Credential,
                    code: ProviderViolationCode::Forbidden,
                    detail: forbidden_credential_detail(configuration.auth_mode),
                });
            }
            _ => {}
        }
    }

    violations
}

const fn unsupported_auth_detail(kind: ProviderKind) -> &'static str {
    match kind {
        ProviderKind::VertexAi => "Use adc or service_account for Vertex AI.",
        ProviderKind::Bedrock => "Use default_chain or static for Bedrock.",
        ProviderKind::AzureOpenAi => "Azure OpenAI currently requires api_key authentication.",
        ProviderKind::OpenAi
        | ProviderKind::OpenAiCompatible
        | ProviderKind::Anthropic
        | ProviderKind::Gemini => "Provider authentication must be api_key.",
    }
}

const fn required_field_detail(
    kind: ProviderKind,
    field: ProviderConfigurationField,
) -> &'static str {
    match (kind, field) {
        (ProviderKind::OpenAiCompatible, ProviderConfigurationField::Endpoint) => {
            "An HTTPS endpoint is required."
        }
        (ProviderKind::VertexAi, ProviderConfigurationField::CloudProject) => {
            "Vertex AI requires a cloud project."
        }
        (ProviderKind::VertexAi, ProviderConfigurationField::CloudRegion) => {
            "Vertex AI requires a cloud region."
        }
        (ProviderKind::VertexAi, ProviderConfigurationField::Model) => {
            "Vertex AI requires an explicit model to probe."
        }
        (ProviderKind::Bedrock, ProviderConfigurationField::CloudRegion) => {
            "Bedrock requires an AWS region."
        }
        (ProviderKind::AzureOpenAi, ProviderConfigurationField::Endpoint) => {
            "Azure OpenAI requires an HTTPS resource endpoint."
        }
        (ProviderKind::AzureOpenAi, ProviderConfigurationField::Deployment) => {
            "Azure OpenAI requires a deployment name."
        }
        (ProviderKind::AzureOpenAi, ProviderConfigurationField::ApiVersion) => {
            "Azure OpenAI requires an API version."
        }
        _ => "This provider configuration field is required.",
    }
}

const fn forbidden_field_detail(
    kind: ProviderKind,
    field: ProviderConfigurationField,
) -> &'static str {
    match (kind, field) {
        (ProviderKind::OpenAi, ProviderConfigurationField::Endpoint) => {
            "Native OpenAI uses the official endpoint; use an OpenAI-compatible provider for a custom endpoint."
        }
        (ProviderKind::Anthropic, ProviderConfigurationField::Endpoint) => {
            "Native Anthropic uses the official endpoint."
        }
        (ProviderKind::Gemini, ProviderConfigurationField::Endpoint) => {
            "Gemini Developer API uses the official endpoint."
        }
        (ProviderKind::VertexAi, ProviderConfigurationField::Endpoint) => {
            "Vertex AI derives its regional Google endpoint from cloud_project and cloud_region."
        }
        (ProviderKind::Bedrock, ProviderConfigurationField::Endpoint) => {
            "Bedrock uses the official regional AWS endpoint; custom endpoints are not accepted."
        }
        (ProviderKind::AzureOpenAi, ProviderConfigurationField::CloudRegion) => {
            "Azure OpenAI does not accept a cloud region."
        }
        (ProviderKind::AzureOpenAi, ProviderConfigurationField::CloudProject) => {
            "Azure OpenAI does not accept a cloud project."
        }
        (ProviderKind::VertexAi, ProviderConfigurationField::Deployment) => {
            "Vertex AI does not accept a deployment field."
        }
        (ProviderKind::VertexAi, ProviderConfigurationField::ApiVersion) => {
            "Vertex AI does not accept an API-version field."
        }
        (ProviderKind::Bedrock, ProviderConfigurationField::CloudProject) => {
            "Bedrock does not accept a cloud project."
        }
        (ProviderKind::Bedrock, ProviderConfigurationField::Deployment) => {
            "Bedrock does not accept a deployment field."
        }
        (ProviderKind::Bedrock, ProviderConfigurationField::ApiVersion) => {
            "Bedrock does not accept an API-version field."
        }
        _ => "This provider does not accept this configuration field.",
    }
}

const fn forbidden_credential_detail(mode: ProviderAuthMode) -> &'static str {
    match mode {
        ProviderAuthMode::ApplicationDefault => "Do not submit a credential when using Vertex ADC.",
        ProviderAuthMode::DefaultChain => {
            "Do not submit a credential when using the AWS default chain."
        }
        ProviderAuthMode::ApiKey | ProviderAuthMode::ServiceAccount | ProviderAuthMode::Static => {
            "This authentication mode accepts a stored credential."
        }
    }
}

#[cfg(test)]
mod tests {
    use std::collections::HashSet;

    use super::*;

    fn valid(kind: ProviderKind, auth_mode: ProviderAuthMode) -> ProviderConfiguration<'static> {
        ProviderConfiguration {
            kind,
            auth_mode,
            endpoint: matches!(
                kind,
                ProviderKind::OpenAiCompatible | ProviderKind::AzureOpenAi
            )
            .then_some("https://example.test"),
            cloud_region: matches!(kind, ProviderKind::VertexAi | ProviderKind::Bedrock)
                .then_some("region"),
            cloud_project: (kind == ProviderKind::VertexAi).then_some("project"),
            deployment: (kind == ProviderKind::AzureOpenAi).then_some("deployment"),
            api_version: (kind == ProviderKind::AzureOpenAi).then_some("2026-01-01"),
            model: (kind == ProviderKind::VertexAi).then_some("model"),
            credential_present: Some(!matches!(
                auth_mode,
                ProviderAuthMode::ApplicationDefault | ProviderAuthMode::DefaultChain
            )),
        }
    }

    #[test]
    fn registry_contains_every_provider_kind_exactly_once() {
        let registered = provider_kind_specs()
            .iter()
            .map(|spec| spec.kind)
            .collect::<HashSet<_>>();
        assert_eq!(registered.len(), provider_kind_specs().len());
        assert_eq!(registered, ProviderKind::ALL.into_iter().collect());
    }

    #[test]
    fn every_declared_provider_auth_combination_is_valid() {
        for spec in provider_kind_specs() {
            for auth in spec.auth_modes {
                assert_eq!(
                    validate_provider_configuration(valid(spec.kind, auth.mode)),
                    []
                );
            }
            for auth in [
                ProviderAuthMode::ApiKey,
                ProviderAuthMode::ApplicationDefault,
                ProviderAuthMode::ServiceAccount,
                ProviderAuthMode::DefaultChain,
                ProviderAuthMode::Static,
            ] {
                let supports = spec
                    .auth_modes
                    .iter()
                    .any(|candidate| candidate.mode == auth);
                let violations = validate_provider_configuration(valid(spec.kind, auth));
                assert_eq!(
                    violations
                        .iter()
                        .any(|violation| violation.field == ProviderViolationField::AuthMode),
                    !supports
                );
            }
        }
    }

    #[test]
    fn required_forbidden_and_credential_rules_are_enforced() {
        for spec in provider_kind_specs() {
            for field in ProviderConfigurationField::ALL {
                let mut candidate = valid(spec.kind, spec.default_auth_mode);
                let slot = match field {
                    ProviderConfigurationField::Endpoint => &mut candidate.endpoint,
                    ProviderConfigurationField::CloudRegion => &mut candidate.cloud_region,
                    ProviderConfigurationField::CloudProject => &mut candidate.cloud_project,
                    ProviderConfigurationField::Deployment => &mut candidate.deployment,
                    ProviderConfigurationField::ApiVersion => &mut candidate.api_version,
                    ProviderConfigurationField::Model => &mut candidate.model,
                };
                let expected = match spec.field(field) {
                    Some(field) if field.required => {
                        *slot = None;
                        ProviderViolationCode::Required
                    }
                    None => {
                        *slot = Some("unexpected");
                        ProviderViolationCode::Forbidden
                    }
                    Some(_) => continue,
                };
                let violations = validate_provider_configuration(candidate);
                assert!(violations.iter().any(|violation| {
                    violation.field == ProviderViolationField::from(field)
                        && violation.code == expected
                }));
            }
        }
    }
}
