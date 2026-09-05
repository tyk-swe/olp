use super::{
    error::Error as InferenceError, execution::RequiredTarget, principal::Principal,
    selection::select_representable_attempts_filtered, service::Service,
};
use crate::domain::{
    auth::{ApiKey, GatewayCapability},
    canonical::{
        identity::{OperationKind, Surface, TransportMode},
        requests::Operation,
    },
    ids::RouteSlug,
    routing::provider::Provider,
};
use std::collections::BTreeSet;
impl Service {
    pub fn select_video_create_target(
        &self,
        principal: &Principal,
        operation: &Operation,
        local_job_id: uuid::Uuid,
    ) -> Result<(ApiKey, RouteSlug, RequiredTarget), InferenceError> {
        let route_slug = operation
            .route()
            .cloned()
            .ok_or_else(|| InferenceError::invalid_request("A route model is required."))?;
        let key =
            self.authorize_principal(principal, GatewayCapability::Inference, Some(&route_slug))?;
        let snapshot = principal.runtime();
        let route = snapshot
            .routes
            .get(&route_slug)
            .ok_or_else(|| InferenceError::resource_not_found("route_not_found"))?;
        let attempt = select_representable_attempts_filtered(
            snapshot,
            &route_slug,
            operation,
            Surface::OpenAi,
            TransportMode::Async,
            local_job_id.as_bytes(),
            |provider, target| {
                self.circuits().is_selectable(target.id)
                    && video_lifecycle_supported(
                        &route.operations,
                        provider,
                        &target.upstream_model,
                    )
            },
        )?
        .into_iter()
        .next()
        .ok_or_else(|| InferenceError::unavailable("no_eligible_provider"))?;
        Ok((
            key.clone(),
            route_slug,
            RequiredTarget {
                provider_id: attempt.provider_id.as_uuid(),
                upstream_model: attempt.upstream_model,
            },
        ))
    }
}
fn video_lifecycle_supported(
    route_operations: &BTreeSet<OperationKind>,
    provider: &Provider,
    model: &str,
) -> bool {
    [
        OperationKind::VideoGet,
        OperationKind::VideoContent,
        OperationKind::VideoDelete,
    ]
    .into_iter()
    .all(|operation| {
        route_operations.contains(&operation)
            && provider.supports(model, operation, Surface::OpenAi, TransportMode::Unary)
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::domain::{
        ids::ProviderId,
        routing::provider::{Capability, ProviderKind},
    };
    #[test]
    fn video_create_requires_exact_lifecycle_capabilities() {
        let model = "video-model";
        let lifecycle = [
            OperationKind::VideoGet,
            OperationKind::VideoContent,
            OperationKind::VideoDelete,
        ];
        let mut operations = lifecycle.into_iter().collect::<BTreeSet<_>>();
        let mut provider = Provider {
            id: ProviderId::new(),
            revision_id: None,
            name: "video-provider".into(),
            kind: ProviderKind::OpenAi,
            enabled: true,
            active_credential: None,
            capabilities: lifecycle
                .into_iter()
                .map(|operation| {
                    Capability::new(model, operation, Surface::OpenAi, TransportMode::Unary)
                })
                .collect(),
        };
        assert!(video_lifecycle_supported(&operations, &provider, model));

        operations.remove(&OperationKind::VideoDelete);
        assert!(!video_lifecycle_supported(&operations, &provider, model));
        operations.insert(OperationKind::VideoDelete);
        provider.capabilities.clear();
        assert!(!video_lifecycle_supported(&operations, &provider, model));
    }
}
