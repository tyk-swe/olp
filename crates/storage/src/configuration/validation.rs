use olp_domain::{OperationKind, ProviderKind, RouteSlug};
use uuid::Uuid;

use super::{
    ConfigurationError,
    resources::{CapabilityRecord, DiscoveredModelInput, UpdateProvider},
};

pub(crate) fn validate_provider_update(update: &UpdateProvider) -> Result<(), ConfigurationError> {
    if update.name.trim().is_empty() || update.name.chars().count() > 100 {
        return Err(ConfigurationError::Invalid(
            "provider name must contain 1-100 characters".to_owned(),
        ));
    }
    for value in [
        &update.endpoint,
        &update.cloud_region,
        &update.cloud_project,
        &update.deployment,
        &update.api_version,
    ]
    .into_iter()
    .flatten()
    {
        if value.chars().count() > 2_000 {
            return Err(ConfigurationError::Invalid(
                "provider setting is too long".to_owned(),
            ));
        }
    }
    Ok(())
}

pub(crate) fn validate_model(model: &DiscoveredModelInput) -> Result<(), ConfigurationError> {
    if model.upstream_model.trim().is_empty() || model.upstream_model.chars().count() > 200 {
        return Err(ConfigurationError::Invalid(
            "model ID must contain 1-200 characters".to_owned(),
        ));
    }
    if model.display_name.trim().is_empty() || model.display_name.chars().count() > 200 {
        return Err(ConfigurationError::Invalid(
            "model display name must contain 1-200 characters".to_owned(),
        ));
    }
    if model.enabled && model.capabilities.is_empty() {
        return Err(ConfigurationError::Invalid(
            "enabled models require an explicit capability".to_owned(),
        ));
    }
    if model.capabilities.len() > 16 {
        return Err(ConfigurationError::Invalid(
            "a model can declare at most 16 capability tuples".to_owned(),
        ));
    }
    Ok(())
}

pub(crate) fn validate_capability(capability: &CapabilityRecord) -> Result<(), ConfigurationError> {
    let _ = capability;
    Ok(())
}

pub(crate) fn validate_provider_capability(
    provider_kind: &str,
    capability: &CapabilityRecord,
) -> Result<(), ConfigurationError> {
    let supported = provider_kind
        .parse::<ProviderKind>()
        .ok()
        .zip(Some(capability.operation))
        .zip(Some(capability.surface))
        .zip(Some(capability.mode))
        .is_some_and(|(((provider_kind, operation), surface), mode)| {
            provider_kind.supports_capability(operation, surface, mode)
        });
    if supported {
        Ok(())
    } else {
        Err(ConfigurationError::Invalid(format!(
            "provider kind {provider_kind} cannot serve {} on {} in {} mode",
            capability.operation, capability.surface, capability.mode
        )))
    }
}

pub(crate) fn validate_route_input(
    slug: &str,
    operations: &[OperationKind],
    overall_timeout_ms: i32,
    max_attempts: i16,
    targets: &[(Uuid, i32, i32, i32)],
) -> Result<(), ConfigurationError> {
    RouteSlug::parse(slug.to_owned())
        .map_err(|error| ConfigurationError::Invalid(error.to_string()))?;
    if operations.is_empty() || targets.is_empty() {
        return Err(ConfigurationError::Invalid(
            "route operations and targets cannot be empty".to_owned(),
        ));
    }
    if overall_timeout_ms <= 0
        || max_attempts <= 0
        || usize::try_from(max_attempts).unwrap_or(usize::MAX) > targets.len()
    {
        return Err(ConfigurationError::Invalid(
            "route deadlines or maximum attempts are invalid".to_owned(),
        ));
    }
    for operation in operations {
        if matches!(
            operation,
            OperationKind::ModelList | OperationKind::ModelGet
        ) {
            return Err(ConfigurationError::Invalid(
                "model list and detail are installation-local APIs, not provider-routed operations"
                    .to_owned(),
            ));
        }
    }
    for (_, priority, weight, timeout) in targets {
        if *priority < 0 || *weight <= 0 || *timeout <= 0 || *timeout > overall_timeout_ms {
            return Err(ConfigurationError::Invalid(
                "route target priority, weight, or timeout is invalid".to_owned(),
            ));
        }
    }
    Ok(())
}

pub(crate) fn checked_limit(limit: i64) -> Result<i64, ConfigurationError> {
    if (1..=100).contains(&limit) {
        Ok(limit)
    } else {
        Err(ConfigurationError::Invalid(
            "page size must be between 1 and 100".to_owned(),
        ))
    }
}

pub(crate) fn enforce_provider_revision_diff_limit(
    actual: usize,
    dimension: &'static str,
    maximum: usize,
) -> Result<(), ConfigurationError> {
    if actual <= maximum {
        Ok(())
    } else {
        Err(ConfigurationError::ProviderRevisionDiffTooLarge { dimension, maximum })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn configuration_validators_reject_implicit_capabilities() {
        let model = DiscoveredModelInput {
            upstream_model: "model".to_owned(),
            display_name: "Model".to_owned(),
            enabled: true,
            capabilities: vec![],
        };
        assert!(validate_model(&model).is_err());
        assert!("unknown".parse::<olp_domain::Surface>().is_err());
    }

    #[test]
    fn provider_revision_diff_ceiling_accepts_boundary_and_rejects_excess() {
        assert!(enforce_provider_revision_diff_limit(2_000, "models", 2_000).is_ok());
        assert!(matches!(
            enforce_provider_revision_diff_limit(2_001, "models", 2_000),
            Err(ConfigurationError::ProviderRevisionDiffTooLarge {
                dimension: "models",
                maximum: 2_000,
            })
        ));
    }

    #[test]
    fn route_drafts_reject_installation_local_model_operations() {
        for operation in ["model_list", "model_get"] {
            let error = validate_route_input(
                "model-route",
                &[operation.parse().unwrap()],
                1_000,
                1,
                &[(Uuid::now_v7(), 0, 1, 500)],
            )
            .unwrap_err();
            assert!(
                matches!(error, ConfigurationError::Invalid(detail) if detail.contains("installation-local"))
            );
        }
    }

    #[test]
    fn provider_capability_matrix_allows_shared_canonical_cross_surface_tuples() {
        fn capability(operation: &str, surface: &str, mode: &str) -> CapabilityRecord {
            CapabilityRecord {
                operation: operation.parse().unwrap(),
                surface: surface.parse().unwrap(),
                mode: mode.parse().unwrap(),
                source: olp_domain::CapabilitySource::Declared,
                certified_at: None,
            }
        }

        for (kind, operation, surface, mode) in [
            ("openai", "generation", "openai", "streaming"),
            ("openai", "generation", "anthropic", "unary"),
            ("openai_compatible", "embeddings", "openai", "unary"),
            ("openai_compatible", "generation", "gemini", "streaming"),
            ("azure_openai", "image_generation", "openai", "streaming"),
            ("azure_openai", "token_count", "anthropic", "unary"),
            ("anthropic", "generation", "anthropic", "streaming"),
            ("anthropic", "token_count", "openai", "unary"),
            ("gemini", "generation", "gemini", "streaming"),
            ("gemini", "generation", "anthropic", "unary"),
            ("vertex_ai", "token_count", "openai", "unary"),
            ("bedrock", "generation", "openai", "unary"),
            ("bedrock", "generation", "anthropic", "streaming"),
            ("bedrock", "token_count", "gemini", "unary"),
        ] {
            assert!(
                validate_provider_capability(kind, &capability(operation, surface, mode)).is_ok(),
                "expected {kind}/{operation}/{surface}/{mode} to be supported"
            );
        }

        for (kind, operation, surface, mode) in [
            ("openai", "embeddings", "anthropic", "unary"),
            ("openai_compatible", "moderation", "gemini", "unary"),
            ("azure_openai", "image_generation", "anthropic", "unary"),
            ("anthropic", "embeddings", "anthropic", "unary"),
            ("anthropic", "generation", "openai", "async"),
            ("gemini", "token_count", "gemini", "streaming"),
            ("vertex_ai", "image_generation", "gemini", "unary"),
            ("bedrock", "embeddings", "openai", "unary"),
            ("bedrock", "generation", "openai", "async"),
        ] {
            assert!(
                validate_provider_capability(kind, &capability(operation, surface, mode)).is_err(),
                "expected {kind}/{operation}/{surface}/{mode} to be rejected"
            );
        }
    }
}
