//! Canonical provider configuration capabilities and validation.

use serde::{Deserialize, Serialize};
use utoipa::ToSchema;

use crate::domain::{
    canonical::identity::Surface, provider::ProviderAuthMode, routing::provider::ProviderKind,
};

#[derive(Clone, Copy, Debug, Deserialize, Eq, Hash, PartialEq, Serialize, ToSchema)]
#[serde(rename_all = "snake_case")]
#[schema(as = ProviderConfigurationField)]
pub enum Field {
    Endpoint,
    CloudRegion,
    CloudProject,
    Deployment,
    ApiVersion,
    Model,
}

impl Field {
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
    pub field: Field,
    pub label: &'static str,
    pub required: bool,
}

/// Immutable, reviewed onboarding values for the generic OpenAI-compatible
/// connector. A preset resolves to ordinary provider configuration and carries
/// no model or operation eligibility.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct ProviderPresetSpec {
    pub id: &'static str,
    pub label: &'static str,
    pub description: &'static str,
    pub endpoint: &'static str,
    pub auth_mode: ProviderAuthMode,
    pub maintainer: &'static str,
    pub documentation_label: &'static str,
    pub documentation_url: &'static str,
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
    pub presets: &'static [ProviderPresetSpec],
}

impl ProviderKindSpec {
    #[must_use]
    pub fn auth_mode(self, mode: ProviderAuthMode) -> Option<&'static ProviderAuthModeSpec> {
        self.auth_modes
            .iter()
            .find(|candidate| candidate.mode == mode)
    }

    #[must_use]
    pub fn field(self, field: Field) -> Option<&'static ProviderFieldSpec> {
        self.fields
            .iter()
            .find(|candidate| candidate.field == field)
    }

    #[must_use]
    pub fn preset(self, id: &str) -> Option<&'static ProviderPresetSpec> {
        self.presets.iter().find(|candidate| candidate.id == id)
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
    field: Field::Model,
    label: "Seed model",
    required: false,
};
const COMMON_FIELDS: [ProviderFieldSpec; 1] = [MODEL_FIELD];
const COMPATIBLE_FIELDS: [ProviderFieldSpec; 2] = [
    ProviderFieldSpec {
        field: Field::Endpoint,
        label: "HTTPS endpoint",
        required: true,
    },
    MODEL_FIELD,
];
const VERTEX_FIELDS: [ProviderFieldSpec; 3] = [
    ProviderFieldSpec {
        field: Field::CloudProject,
        label: "Cloud project",
        required: true,
    },
    ProviderFieldSpec {
        field: Field::CloudRegion,
        label: "Cloud location",
        required: true,
    },
    ProviderFieldSpec {
        field: Field::Model,
        label: "Probe model",
        required: true,
    },
];
const BEDROCK_FIELDS: [ProviderFieldSpec; 2] = [
    ProviderFieldSpec {
        field: Field::CloudRegion,
        label: "AWS region",
        required: true,
    },
    MODEL_FIELD,
];
const AZURE_FIELDS: [ProviderFieldSpec; 4] = [
    ProviderFieldSpec {
        field: Field::Endpoint,
        label: "Resource endpoint",
        required: true,
    },
    ProviderFieldSpec {
        field: Field::Deployment,
        label: "Deployment",
        required: true,
    },
    ProviderFieldSpec {
        field: Field::ApiVersion,
        label: "API version",
        required: true,
    },
    MODEL_FIELD,
];

const NO_PRESETS: [ProviderPresetSpec; 0] = [];
const OPENAI_COMPATIBLE_PRESETS: [ProviderPresetSpec; 6] = [
    ProviderPresetSpec {
        id: "groq",
        label: "Groq",
        description: "Low-latency inference through Groq's OpenAI-compatible API.",
        endpoint: "https://api.groq.com/openai/v1",
        auth_mode: ProviderAuthMode::ApiKey,
        maintainer: "Groq",
        documentation_label: "OpenAI Compatibility",
        documentation_url: "https://console.groq.com/docs/openai",
    },
    ProviderPresetSpec {
        id: "mistral_ai",
        label: "Mistral AI",
        description: "Mistral models through the official OpenAI-compatible API surface.",
        endpoint: "https://api.mistral.ai/v1",
        auth_mode: ProviderAuthMode::ApiKey,
        maintainer: "Mistral AI",
        documentation_label: "Migration from OpenAI",
        documentation_url: "https://docs.mistral.ai/resources/migration-guides",
    },
    ProviderPresetSpec {
        id: "together_ai",
        label: "Together AI",
        description: "Hosted open models through Together AI's OpenAI-compatible API.",
        endpoint: "https://api.together.ai/v1",
        auth_mode: ProviderAuthMode::ApiKey,
        maintainer: "Together AI",
        documentation_label: "OpenAI API Compatibility",
        documentation_url: "https://docs.together.ai/docs/openai-api-compatibility",
    },
    ProviderPresetSpec {
        id: "xai",
        label: "xAI",
        description: "Grok models through xAI's OpenAI-compatible API.",
        endpoint: "https://api.x.ai/v1",
        auth_mode: ProviderAuthMode::ApiKey,
        maintainer: "xAI",
        documentation_label: "API Reference",
        documentation_url: "https://docs.x.ai/docs/api-reference",
    },
    ProviderPresetSpec {
        id: "cerebras",
        label: "Cerebras",
        description: "Cerebras inference through its OpenAI-compatible API.",
        endpoint: "https://api.cerebras.ai/v1",
        auth_mode: ProviderAuthMode::ApiKey,
        maintainer: "Cerebras",
        documentation_label: "Using OpenAI with Cerebras",
        documentation_url: "https://inference-docs.cerebras.ai/resources/openai",
    },
    ProviderPresetSpec {
        id: "openrouter",
        label: "OpenRouter",
        description: "Multi-provider model access through OpenRouter's OpenAI-compatible API.",
        endpoint: "https://openrouter.ai/api/v1",
        auth_mode: ProviderAuthMode::ApiKey,
        maintainer: "OpenRouter",
        documentation_label: "API Reference Overview",
        documentation_url: "https://openrouter.ai/docs/api/reference/overview",
    },
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
        presets: &NO_PRESETS,
    },
    ProviderKindSpec {
        kind: ProviderKind::Anthropic,
        label: "Anthropic",
        description: "Native Messages API",
        seed_surface: Some(Surface::Anthropic),
        default_auth_mode: ProviderAuthMode::ApiKey,
        auth_modes: &API_KEY_AUTH,
        fields: &COMMON_FIELDS,
        presets: &NO_PRESETS,
    },
    ProviderKindSpec {
        kind: ProviderKind::Gemini,
        label: "Gemini Developer API",
        description: "Google AI API key",
        seed_surface: Some(Surface::Gemini),
        default_auth_mode: ProviderAuthMode::ApiKey,
        auth_modes: &API_KEY_AUTH,
        fields: &COMMON_FIELDS,
        presets: &NO_PRESETS,
    },
    ProviderKindSpec {
        kind: ProviderKind::VertexAi,
        label: "Vertex AI",
        description: "Google Cloud identity",
        seed_surface: Some(Surface::Gemini),
        default_auth_mode: ProviderAuthMode::ApplicationDefault,
        auth_modes: &VERTEX_AUTH,
        fields: &VERTEX_FIELDS,
        presets: &NO_PRESETS,
    },
    ProviderKindSpec {
        kind: ProviderKind::Bedrock,
        label: "AWS Bedrock",
        description: "AWS default chain or static credentials",
        seed_surface: None,
        default_auth_mode: ProviderAuthMode::DefaultChain,
        auth_modes: &BEDROCK_AUTH,
        fields: &BEDROCK_FIELDS,
        presets: &NO_PRESETS,
    },
    ProviderKindSpec {
        kind: ProviderKind::AzureOpenAi,
        label: "Azure OpenAI",
        description: "Azure deployment endpoint",
        seed_surface: Some(Surface::OpenAi),
        default_auth_mode: ProviderAuthMode::ApiKey,
        auth_modes: &API_KEY_AUTH,
        fields: &AZURE_FIELDS,
        presets: &NO_PRESETS,
    },
    ProviderKindSpec {
        kind: ProviderKind::OpenAiCompatible,
        label: "OpenAI-compatible",
        description: "Explicit custom HTTPS endpoint",
        seed_surface: Some(Surface::OpenAi),
        default_auth_mode: ProviderAuthMode::ApiKey,
        auth_modes: &API_KEY_AUTH,
        fields: &COMPATIBLE_FIELDS,
        presets: &OPENAI_COMPATIBLE_PRESETS,
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
pub struct Configuration<'a> {
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

impl<'a> Configuration<'a> {
    fn value(self, field: Field) -> Option<&'a str> {
        match field {
            Field::Endpoint => self.endpoint,
            Field::CloudRegion => self.cloud_region,
            Field::CloudProject => self.cloud_project,
            Field::Deployment => self.deployment,
            Field::ApiVersion => self.api_version,
            Field::Model => self.model,
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

impl From<Field> for ProviderViolationField {
    fn from(value: Field) -> Self {
        match value {
            Field::Endpoint => Self::Endpoint,
            Field::CloudRegion => Self::CloudRegion,
            Field::CloudProject => Self::CloudProject,
            Field::Deployment => Self::Deployment,
            Field::ApiVersion => Self::ApiVersion,
            Field::Model => Self::Model,
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

impl ProviderViolationCode {
    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::UnsupportedAuthMode => "unsupported_auth_mode",
            Self::Required => "required",
            Self::Forbidden => "forbidden",
        }
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct Violation {
    pub field: ProviderViolationField,
    pub code: ProviderViolationCode,
    pub detail: &'static str,
}

#[must_use]
pub fn validate(configuration: Configuration<'_>) -> Vec<Violation> {
    let spec = provider_kind_spec(configuration.kind);
    let mut violations = Vec::new();

    let auth = spec.auth_mode(configuration.auth_mode);
    if auth.is_none() {
        violations.push(Violation {
            field: ProviderViolationField::AuthMode,
            code: ProviderViolationCode::UnsupportedAuthMode,
            detail: unsupported_auth_detail(configuration.kind),
        });
    }

    for field in Field::ALL {
        match (spec.field(field), configuration.value(field)) {
            (Some(field_spec), value)
                if field_spec.required && value.is_none_or(|value| value.trim().is_empty()) =>
            {
                violations.push(Violation {
                    field: field.into(),
                    code: ProviderViolationCode::Required,
                    detail: required_field_detail(configuration.kind, field),
                });
            }
            (None, Some(_)) => violations.push(Violation {
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
                violations.push(Violation {
                    field: ProviderViolationField::Credential,
                    code: ProviderViolationCode::Required,
                    detail: "This authentication mode requires a write-only credential.",
                });
            }
            (CredentialRequirement::Forbidden, true) => {
                violations.push(Violation {
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

const fn required_field_detail(kind: ProviderKind, field: Field) -> &'static str {
    match (kind, field) {
        (ProviderKind::OpenAiCompatible, Field::Endpoint) => "An HTTPS endpoint is required.",
        (ProviderKind::VertexAi, Field::CloudProject) => "Vertex AI requires a cloud project.",
        (ProviderKind::VertexAi, Field::CloudRegion) => "Vertex AI requires a cloud region.",
        (ProviderKind::VertexAi, Field::Model) => "Vertex AI requires an explicit model to probe.",
        (ProviderKind::Bedrock, Field::CloudRegion) => "Bedrock requires an AWS region.",
        (ProviderKind::AzureOpenAi, Field::Endpoint) => {
            "Azure OpenAI requires an HTTPS resource endpoint."
        }
        (ProviderKind::AzureOpenAi, Field::Deployment) => {
            "Azure OpenAI requires a deployment name."
        }
        (ProviderKind::AzureOpenAi, Field::ApiVersion) => "Azure OpenAI requires an API version.",
        _ => "This provider configuration field is required.",
    }
}

const fn forbidden_field_detail(kind: ProviderKind, field: Field) -> &'static str {
    match (kind, field) {
        (ProviderKind::OpenAi, Field::Endpoint) => {
            "Native OpenAI uses the official endpoint; use an OpenAI-compatible provider for a custom endpoint."
        }
        (ProviderKind::Anthropic, Field::Endpoint) => {
            "Native Anthropic uses the official endpoint."
        }
        (ProviderKind::Gemini, Field::Endpoint) => {
            "Gemini Developer API uses the official endpoint."
        }
        (ProviderKind::VertexAi, Field::Endpoint) => {
            "Vertex AI derives its regional Google endpoint from cloud_project and cloud_region."
        }
        (ProviderKind::Bedrock, Field::Endpoint) => {
            "Bedrock uses the official regional AWS endpoint; custom endpoints are not accepted."
        }
        (ProviderKind::AzureOpenAi, Field::CloudRegion) => {
            "Azure OpenAI does not accept a cloud region."
        }
        (ProviderKind::AzureOpenAi, Field::CloudProject) => {
            "Azure OpenAI does not accept a cloud project."
        }
        (ProviderKind::VertexAi, Field::Deployment) => {
            "Vertex AI does not accept a deployment field."
        }
        (ProviderKind::VertexAi, Field::ApiVersion) => {
            "Vertex AI does not accept an API-version field."
        }
        (ProviderKind::Bedrock, Field::CloudProject) => "Bedrock does not accept a cloud project.",
        (ProviderKind::Bedrock, Field::Deployment) => "Bedrock does not accept a deployment field.",
        (ProviderKind::Bedrock, Field::ApiVersion) => {
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
    use url::Url;

    fn valid(kind: ProviderKind, auth_mode: ProviderAuthMode) -> Configuration<'static> {
        Configuration {
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
    fn compatible_preset_catalog_is_normalized_safe_and_supported() {
        let compatible = provider_kind_spec(ProviderKind::OpenAiCompatible);
        assert!(!compatible.presets.is_empty());
        assert!(
            provider_kind_specs()
                .iter()
                .filter(|spec| spec.kind != ProviderKind::OpenAiCompatible)
                .all(|spec| spec.presets.is_empty())
        );

        let mut ids = HashSet::new();
        let mut labels = HashSet::new();
        let mut endpoints = HashSet::new();
        for preset in compatible.presets {
            assert!((1..=64).contains(&preset.id.chars().count()));
            assert!(preset.id.bytes().all(|byte| {
                byte.is_ascii_lowercase() || byte.is_ascii_digit() || byte == b'_'
            }));
            assert_ne!(preset.id, "custom");
            assert!(ids.insert(preset.id));

            assert_eq!(preset.label, preset.label.trim());
            assert!((1..=40).contains(&preset.label.chars().count()));
            assert!(labels.insert(preset.label.to_lowercase()));
            assert_eq!(preset.description, preset.description.trim());
            assert!((1..=160).contains(&preset.description.chars().count()));
            assert_eq!(preset.maintainer, preset.maintainer.trim());
            assert!((1..=80).contains(&preset.maintainer.chars().count()));
            assert_eq!(
                preset.documentation_label,
                preset.documentation_label.trim()
            );
            assert!((1..=80).contains(&preset.documentation_label.chars().count()));

            let endpoint = Url::parse(preset.endpoint).expect("preset endpoint must be a URL");
            assert_eq!(endpoint.scheme(), "https");
            assert!(endpoint.host_str().is_some());
            assert!(endpoint.username().is_empty());
            assert!(endpoint.password().is_none());
            assert!(endpoint.query().is_none());
            assert!(endpoint.fragment().is_none());
            assert!(endpoints.insert(endpoint.as_str().trim_end_matches('/').to_owned()));

            let documentation =
                Url::parse(preset.documentation_url).expect("preset documentation must be a URL");
            assert_eq!(documentation.scheme(), "https");
            assert!(documentation.host_str().is_some());
            assert!(documentation.username().is_empty());
            assert!(documentation.password().is_none());

            let auth = compatible
                .auth_mode(preset.auth_mode)
                .expect("preset authentication must be supported by its provider kind");
            assert_eq!(auth.credential, CredentialRequirement::Required);
            assert_eq!(compatible.preset(preset.id), Some(preset));
        }
    }

    #[test]
    fn every_declared_provider_auth_combination_is_valid() {
        for spec in provider_kind_specs() {
            for auth in spec.auth_modes {
                assert_eq!(validate(valid(spec.kind, auth.mode)), []);
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
                let violations = validate(valid(spec.kind, auth));
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
            for field in Field::ALL {
                let mut candidate = valid(spec.kind, spec.default_auth_mode);
                let slot = match field {
                    Field::Endpoint => &mut candidate.endpoint,
                    Field::CloudRegion => &mut candidate.cloud_region,
                    Field::CloudProject => &mut candidate.cloud_project,
                    Field::Deployment => &mut candidate.deployment,
                    Field::ApiVersion => &mut candidate.api_version,
                    Field::Model => &mut candidate.model,
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
                let violations = validate(candidate);
                assert!(violations.iter().any(|violation| {
                    violation.field == ProviderViolationField::from(field)
                        && violation.code == expected
                }));
            }
        }
    }

    #[test]
    fn violation_code_wire_strings_are_pinned() {
        // These strings cross the API boundary as `error_codes` values, so a
        // rename here silently breaks every client that branches on them.
        for (code, expected) in [
            (
                ProviderViolationCode::UnsupportedAuthMode,
                "unsupported_auth_mode",
            ),
            (ProviderViolationCode::Required, "required"),
            (ProviderViolationCode::Forbidden, "forbidden"),
        ] {
            assert_eq!(code.as_str(), expected);
            assert_eq!(
                serde_json::to_string(&code).unwrap(),
                format!("\"{expected}\"")
            );
        }
    }
}
