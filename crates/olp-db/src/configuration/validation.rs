use olp_engine::domain::{
    canonical::identity::OperationKind, ids::RouteSlug, routing::provider::ProviderKind,
};
use uuid::Uuid;

use super::{
    Error,
    resources::{CapabilityRecord, DiscoveredModelInput, UpdateProvider, helpers::LockedProvider},
};

/// Whether an update touches anything the connector transport depends on.
/// A pure rename keeps probe evidence and certification; everything else
/// invalidates both.
pub(crate) fn transport_changed(current: &LockedProvider, update: &UpdateProvider) -> bool {
    fn trimmed(value: &Option<String>) -> Option<&str> {
        value.as_deref().map(str::trim)
    }
    trimmed(&update.endpoint) != current.endpoint.as_deref()
        || trimmed(&update.cloud_region) != current.cloud_region.as_deref()
        || trimmed(&update.cloud_project) != current.cloud_project.as_deref()
        || trimmed(&update.deployment) != current.deployment.as_deref()
        || trimmed(&update.api_version) != current.api_version.as_deref()
        || update.auth_mode.as_str() != current.auth_mode
}

pub(crate) fn validate_provider_update(update: &UpdateProvider) -> Result<(), Error> {
    if update.name.trim().is_empty() || update.name.chars().count() > 100 {
        return Err(Error::Invalid(
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
            return Err(Error::Invalid("provider setting is too long".to_owned()));
        }
    }
    Ok(())
}

pub(crate) fn validate_model(model: &DiscoveredModelInput) -> Result<(), Error> {
    if model.upstream_model.trim().is_empty() || model.upstream_model.chars().count() > 200 {
        return Err(Error::Invalid(
            "model ID must contain 1-200 characters".to_owned(),
        ));
    }
    if model.display_name.trim().is_empty() || model.display_name.chars().count() > 200 {
        return Err(Error::Invalid(
            "model display name must contain 1-200 characters".to_owned(),
        ));
    }
    if model.enabled && model.capabilities.is_empty() {
        return Err(Error::Invalid(
            "enabled models require an explicit capability".to_owned(),
        ));
    }
    if model.capabilities.len() > MAX_MODEL_CAPABILITY_TUPLES {
        return Err(Error::Invalid(format!(
            "a model can declare at most {MAX_MODEL_CAPABILITY_TUPLES} capability tuples"
        )));
    }
    Ok(())
}

pub(crate) fn validate_provider_capability(
    provider_kind: &str,
    capability: &CapabilityRecord,
) -> Result<(), Error> {
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
        Err(Error::Invalid(format!(
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
) -> Result<(), Error> {
    RouteSlug::parse(slug.to_owned()).map_err(|error| Error::Invalid(error.to_string()))?;
    if operations.is_empty() || targets.is_empty() {
        return Err(Error::Invalid(
            "route operations and targets cannot be empty".to_owned(),
        ));
    }
    if overall_timeout_ms <= 0
        || max_attempts <= 0
        || usize::try_from(max_attempts).unwrap_or(usize::MAX) > targets.len()
    {
        return Err(Error::Invalid(
            "route deadlines or maximum attempts are invalid".to_owned(),
        ));
    }
    for operation in operations {
        if matches!(
            operation,
            OperationKind::ModelList | OperationKind::ModelGet
        ) {
            return Err(Error::Invalid(
                "model list and detail are installation-local APIs, not provider-routed operations"
                    .to_owned(),
            ));
        }
    }
    for (_, priority, weight, timeout) in targets {
        if *priority < 0 || *weight <= 0 || *timeout <= 0 || *timeout > overall_timeout_ms {
            return Err(Error::Invalid(
                "route target priority, weight, or timeout is invalid".to_owned(),
            ));
        }
    }
    Ok(())
}

/// Largest reviewed capability tuple set a single model may carry.
pub const MAX_MODEL_CAPABILITY_TUPLES: usize = 64;

/// Largest page any collection returns; the HTTP layer derives its bound
/// from this so the two cannot drift.
pub const MAX_PAGE_SIZE: i64 = 200;

pub(crate) fn checked_limit(limit: i64) -> Result<i64, Error> {
    if (1..=MAX_PAGE_SIZE).contains(&limit) {
        Ok(limit)
    } else {
        Err(Error::Invalid(format!(
            "page size must be between 1 and {MAX_PAGE_SIZE}"
        )))
    }
}

pub(crate) fn enforce_provider_revision_diff_limit(
    actual: usize,
    dimension: &'static str,
    maximum: usize,
) -> Result<(), Error> {
    if actual <= maximum {
        Ok(())
    } else {
        Err(Error::ProviderRevisionDiffTooLarge { dimension, maximum })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn capability(operation: &str, surface: &str, mode: &str) -> CapabilityRecord {
        CapabilityRecord {
            operation: operation.parse().unwrap(),
            surface: surface.parse().unwrap(),
            mode: mode.parse().unwrap(),
            source: olp_engine::domain::provider::CapabilitySource::Declared,
            certified_at: None,
        }
    }

    fn provider_update() -> UpdateProvider {
        UpdateProvider {
            name: "Provider".to_owned(),
            endpoint: Some("https://api.example.test".to_owned()),
            cloud_region: None,
            cloud_project: None,
            deployment: None,
            api_version: None,
            auth_mode: olp_engine::domain::provider::ProviderAuthMode::ApiKey,
        }
    }

    #[test]
    fn configuration_validators_reject_implicit_capabilities() {
        let model = DiscoveredModelInput {
            upstream_model: "model".to_owned(),
            display_name: "Model".to_owned(),
            enabled: true,
            capabilities: vec![],
        };
        assert!(validate_model(&model).is_err());
        assert!(
            "unknown"
                .parse::<olp_engine::domain::canonical::identity::Surface>()
                .is_err()
        );
    }

    fn locked_provider() -> LockedProvider {
        LockedProvider {
            etag: Uuid::nil(),
            state: "draft".to_owned(),
            kind: "openai_compatible".to_owned(),
            endpoint: Some("https://api.example.test".to_owned()),
            cloud_region: None,
            cloud_project: None,
            deployment: None,
            api_version: None,
            auth_mode: "api_key".to_owned(),
            active_credential_version_id: None,
            updated_at: chrono::Utc::now(),
            last_probe_at: None,
            last_probe_status: None,
        }
    }

    #[test]
    fn transport_change_ignores_renames_and_whitespace_but_not_connector_settings() {
        let current = locked_provider();
        let mut renamed = provider_update();
        renamed.name = "Renamed".to_owned();
        renamed.endpoint = Some("  https://api.example.test  ".to_owned());
        assert!(!transport_changed(&current, &renamed));

        let mutators: [fn(&mut UpdateProvider); 6] = [
            |update| update.endpoint = Some("https://other.example.test".to_owned()),
            |update| update.cloud_region = Some("eu-west-1".to_owned()),
            |update| update.cloud_project = Some("project".to_owned()),
            |update| update.deployment = Some("deployment".to_owned()),
            |update| update.api_version = Some("2024-06-01".to_owned()),
            |update| {
                update.auth_mode =
                    olp_engine::domain::provider::ProviderAuthMode::ApplicationDefault;
            },
        ];
        for mutate in mutators {
            let mut candidate = provider_update();
            mutate(&mut candidate);
            assert!(transport_changed(&current, &candidate));
        }
    }

    #[test]
    fn provider_revision_diff_ceiling_accepts_boundary_and_rejects_excess() {
        assert!(enforce_provider_revision_diff_limit(2_000, "models", 2_000).is_ok());
        assert!(matches!(
            enforce_provider_revision_diff_limit(2_001, "models", 2_000),
            Err(Error::ProviderRevisionDiffTooLarge {
                dimension: "models",
                maximum: 2_000,
            })
        ));
    }

    #[test]
    fn provider_and_model_text_limits_are_character_based_and_closed() {
        let mut update = provider_update();
        update.name = "é".repeat(100);
        update.endpoint = Some("é".repeat(2_000));
        validate_provider_update(&update).unwrap();

        let mutators: [fn(&mut UpdateProvider); 3] = [
            |update: &mut UpdateProvider| update.name = " ".to_owned(),
            |update: &mut UpdateProvider| update.name = "x".repeat(101),
            |update: &mut UpdateProvider| update.cloud_region = Some("x".repeat(2_001)),
        ];
        for mutate in mutators {
            let mut candidate = provider_update();
            mutate(&mut candidate);
            assert!(validate_provider_update(&candidate).is_err());
        }

        let valid = DiscoveredModelInput {
            upstream_model: "m".repeat(200),
            display_name: "M".repeat(200),
            enabled: true,
            capabilities: vec![capability("generation", "openai", "unary")],
        };
        validate_model(&valid).unwrap();

        let mut candidate = valid.clone();
        candidate.upstream_model = " ".to_owned();
        assert!(validate_model(&candidate).is_err());
        let mut candidate = valid.clone();
        candidate.display_name = "x".repeat(201);
        assert!(validate_model(&candidate).is_err());
        let mut candidate = valid;
        candidate.capabilities = std::iter::repeat_n(
            capability("generation", "openai", "unary"),
            MAX_MODEL_CAPABILITY_TUPLES + 1,
        )
        .collect();
        assert!(validate_model(&candidate).is_err());
    }

    #[test]
    fn route_validation_checks_page_deadline_attempt_and_target_boundaries() {
        let provider = Uuid::now_v7();
        let operation = OperationKind::Generation;
        let valid_target = (provider, 0, 1, 500);
        validate_route_input("primary", &[operation], 1_000, 1, &[valid_target]).unwrap();

        assert!(
            validate_route_input("INVALID SLUG", &[operation], 1_000, 1, &[valid_target]).is_err()
        );
        assert!(validate_route_input("primary", &[], 1_000, 1, &[valid_target]).is_err());
        assert!(validate_route_input("primary", &[operation], 1_000, 1, &[]).is_err());
        for (overall, attempts, targets) in [
            (0, 1, vec![valid_target]),
            (1_000, 0, vec![valid_target]),
            (1_000, 2, vec![valid_target]),
        ] {
            assert!(
                validate_route_input("primary", &[operation], overall, attempts, &targets).is_err()
            );
        }
        for invalid_target in [
            (provider, -1, 1, 500),
            (provider, 0, 0, 500),
            (provider, 0, 1, 0),
            (provider, 0, 1, 1_001),
        ] {
            assert!(
                validate_route_input("primary", &[operation], 1_000, 1, &[invalid_target]).is_err()
            );
        }

        assert_eq!(checked_limit(1).unwrap(), 1);
        assert_eq!(checked_limit(MAX_PAGE_SIZE).unwrap(), MAX_PAGE_SIZE);
        for invalid in [i64::MIN, 0, MAX_PAGE_SIZE + 1, i64::MAX] {
            assert!(checked_limit(invalid).is_err());
        }
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
                matches!(error, Error::Invalid(detail) if detail.contains("installation-local"))
            );
        }
    }

    #[test]
    fn provider_capability_matrix_allows_shared_canonical_cross_surface_tuples() {
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
