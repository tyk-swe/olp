use std::{
    collections::BTreeSet,
    fmt,
    num::{NonZeroU32, NonZeroU64},
};

use chrono::{DateTime, Utc};
use serde::{Deserialize, Serialize};
use thiserror::Error;

use crate::domain::{
    canonical::identity::OperationKind,
    ids::{ApiKeyId, ApiKeyLookupId, RouteSlug},
};

closed_string_enum! {
    pub enum Role {
        Owner => "owner",
        Operator => "operator",
        Developer => "developer",
        Viewer => "viewer",
    }
    parse_error InvalidRole => |_| InvalidRole;
}

#[derive(Clone, Copy, Debug, Error, Eq, PartialEq)]
#[error("invalid fixed user role")]
pub struct InvalidRole;

#[derive(Clone, Copy, Debug, Deserialize, Eq, Hash, Ord, PartialEq, PartialOrd, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum Permission {
    ReadConfiguration,
    ManageProviders,
    ManageRoutes,
    ManageApiKeys,
    ReadAccess,
    ManageAccess,
    ManageSessions,
    ReadOperations,
    UsePlayground,
    ManageSettings,
    ManagePricing,
}

impl Permission {
    pub const ALL: [Self; 11] = [
        Self::ReadConfiguration,
        Self::ManageProviders,
        Self::ManageRoutes,
        Self::ManageApiKeys,
        Self::ReadAccess,
        Self::ManageAccess,
        Self::ManageSessions,
        Self::ReadOperations,
        Self::UsePlayground,
        Self::ManageSettings,
        Self::ManagePricing,
    ];
}

impl Role {
    #[must_use]
    pub const fn allows(self, permission: Permission) -> bool {
        match self {
            Self::Owner => true,
            Self::Operator => matches!(
                permission,
                Permission::ManageProviders
                    | Permission::ManageRoutes
                    | Permission::ManageApiKeys
                    | Permission::ReadAccess
                    | Permission::ReadConfiguration
                    | Permission::ReadOperations
                    | Permission::UsePlayground
                    | Permission::ManageSettings
                    | Permission::ManagePricing
            ),
            Self::Developer => matches!(
                permission,
                Permission::ReadConfiguration
                    | Permission::ManageApiKeys
                    | Permission::ReadOperations
                    | Permission::UsePlayground
            ),
            Self::Viewer => matches!(
                permission,
                Permission::ReadConfiguration | Permission::ReadOperations
            ),
        }
    }
}

#[derive(Clone, Copy, Debug, Deserialize, Eq, Hash, Ord, PartialEq, PartialOrd, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum ApiKeyScope {
    Inference,
    ModelsRead,
}

/// Closed authorization capabilities for public gateway endpoints.
///
/// These capabilities intentionally remain separate from [`OperationKind`],
/// which continues to describe routing, provider support, and accounting.
#[derive(Clone, Copy, Debug, Eq, Hash, Ord, PartialEq, PartialOrd)]
pub enum GatewayCapability {
    Inference,
    ModelsRead,
}

impl GatewayCapability {
    pub const ALL: [Self; 2] = [Self::Inference, Self::ModelsRead];
}

impl ApiKeyScope {
    pub const ALL: [Self; 2] = [Self::Inference, Self::ModelsRead];

    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::Inference => "inference",
            Self::ModelsRead => "models_read",
        }
    }

    /// Returns whether this scope positively grants `capability`.
    ///
    /// The fully enumerated matrix is deliberate: adding either a scope or a
    /// capability requires an explicit decision for every combination.
    #[must_use]
    pub const fn permits(self, capability: GatewayCapability) -> bool {
        match (self, capability) {
            (Self::Inference, GatewayCapability::Inference)
            | (Self::ModelsRead, GatewayCapability::ModelsRead) => true,
            (Self::Inference, GatewayCapability::ModelsRead)
            | (Self::ModelsRead, GatewayCapability::Inference) => false,
        }
    }
}

/// Maps the canonical operation dimension to its required gateway
/// authorization capability.
///
/// This exhaustive positive mapping prevents a new operation from inheriting
/// authorization merely because it is not a model-read operation.
#[must_use]
pub const fn gateway_capability_for_operation(operation: OperationKind) -> GatewayCapability {
    match operation {
        OperationKind::Generation
        | OperationKind::Embeddings
        | OperationKind::Moderation
        | OperationKind::ImageGeneration
        | OperationKind::ImageEdit
        | OperationKind::ImageVariation
        | OperationKind::Speech
        | OperationKind::Transcription
        | OperationKind::TokenCount
        | OperationKind::VideoCreate
        | OperationKind::VideoList
        | OperationKind::VideoGet
        | OperationKind::VideoContent
        | OperationKind::VideoDelete => GatewayCapability::Inference,
        OperationKind::ModelList | OperationKind::ModelGet => GatewayCapability::ModelsRead,
    }
}

#[derive(Clone, Copy, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum ApiKeyStatus {
    Active,
    Revoked,
}

#[derive(Clone, Copy, Deserialize, Eq, PartialEq, Serialize)]
#[serde(transparent)]
pub struct ApiKeyDigest([u8; 32]);

impl ApiKeyDigest {
    #[must_use]
    pub const fn new(value: [u8; 32]) -> Self {
        Self(value)
    }

    #[must_use]
    pub const fn as_bytes(&self) -> &[u8; 32] {
        &self.0
    }
}

impl fmt::Debug for ApiKeyDigest {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str("ApiKeyDigest([REDACTED])")
    }
}

#[derive(Clone, Copy, Debug, Default, Deserialize, Eq, PartialEq, Serialize)]
pub struct ApiKeyLimits {
    pub requests_per_minute: Option<NonZeroU32>,
    pub tokens_per_minute: Option<NonZeroU64>,
    pub concurrency: Option<NonZeroU32>,
}

impl ApiKeyLimits {
    #[must_use]
    pub const fn has_hard_limits(self) -> bool {
        self.requests_per_minute.is_some()
            || self.tokens_per_minute.is_some()
            || self.concurrency.is_some()
    }
}

#[derive(Clone, Debug, Deserialize, Serialize)]
pub struct ApiKey {
    pub id: ApiKeyId,
    pub lookup_id: ApiKeyLookupId,
    pub digest: ApiKeyDigest,
    pub status: ApiKeyStatus,
    pub expires_at: Option<DateTime<Utc>>,
    #[serde(default)]
    pub scopes: BTreeSet<ApiKeyScope>,
    #[serde(default)]
    pub allowed_routes: BTreeSet<RouteSlug>,
    #[serde(default)]
    pub limits: ApiKeyLimits,
}

pub fn authorize_api_key(
    key: &ApiKey,
    route: Option<&RouteSlug>,
    endpoint_capability: Option<GatewayCapability>,
    required_capability: GatewayCapability,
    now: DateTime<Utc>,
) -> Result<(), ApiKeyAuthorizationError> {
    if key.status == ApiKeyStatus::Revoked {
        return Err(ApiKeyAuthorizationError::Revoked);
    }
    if key.expires_at.is_some_and(|expiration| expiration <= now) {
        return Err(ApiKeyAuthorizationError::Expired);
    }
    if endpoint_capability != Some(required_capability) {
        return Err(ApiKeyAuthorizationError::CapabilityNotDeclared {
            required: required_capability,
        });
    }
    if !key
        .scopes
        .iter()
        .any(|scope| scope.permits(required_capability))
    {
        return Err(ApiKeyAuthorizationError::MissingScope {
            capability: required_capability,
        });
    }
    if let Some(route) = route
        && !key.allowed_routes.is_empty()
        && !key.allowed_routes.contains(route)
    {
        return Err(ApiKeyAuthorizationError::RouteNotAllowed {
            route: route.clone(),
        });
    }
    Ok(())
}

#[derive(Clone, Debug, Error, Eq, PartialEq)]
pub enum ApiKeyAuthorizationError {
    #[error("endpoint does not declare required gateway capability {required:?}")]
    CapabilityNotDeclared { required: GatewayCapability },
    #[error("API key is revoked")]
    Revoked,
    #[error("API key is expired")]
    Expired,
    #[error("API key scope does not permit gateway capability {capability:?}")]
    MissingScope { capability: GatewayCapability },
    #[error("API key does not allow route {route}")]
    RouteNotAllowed { route: RouteSlug },
}

#[cfg(test)]
mod tests {
    use super::*;

    const fn expected_scope_capability(scope: ApiKeyScope, capability: GatewayCapability) -> bool {
        match (scope, capability) {
            (ApiKeyScope::Inference, GatewayCapability::Inference)
            | (ApiKeyScope::ModelsRead, GatewayCapability::ModelsRead) => true,
            (ApiKeyScope::Inference, GatewayCapability::ModelsRead)
            | (ApiKeyScope::ModelsRead, GatewayCapability::Inference) => false,
        }
    }

    #[test]
    fn api_key_scope_capability_matrix_is_exhaustive() {
        for scope in ApiKeyScope::ALL {
            for capability in GatewayCapability::ALL {
                assert_eq!(
                    scope.permits(capability),
                    expected_scope_capability(scope, capability),
                    "unexpected API-key decision for {scope:?}/{capability:?}"
                );
            }
        }
    }

    #[test]
    fn inference_scope_does_not_grant_a_non_inference_capability() {
        let hypothetical_non_inference_action = GatewayCapability::ModelsRead;
        assert!(!ApiKeyScope::Inference.permits(hypothetical_non_inference_action));
    }

    #[test]
    fn every_operation_has_an_explicit_gateway_capability() {
        for operation in OperationKind::ALL {
            let expected = match operation {
                OperationKind::Generation
                | OperationKind::Embeddings
                | OperationKind::TokenCount
                | OperationKind::ImageGeneration
                | OperationKind::ImageEdit
                | OperationKind::ImageVariation
                | OperationKind::Speech
                | OperationKind::Transcription
                | OperationKind::VideoCreate
                | OperationKind::VideoList
                | OperationKind::VideoGet
                | OperationKind::VideoContent
                | OperationKind::VideoDelete
                | OperationKind::Moderation => GatewayCapability::Inference,
                OperationKind::ModelList | OperationKind::ModelGet => GatewayCapability::ModelsRead,
            };
            assert_eq!(gateway_capability_for_operation(operation), expected);
        }
    }

    const fn expected(role: Role, permission: Permission) -> bool {
        match role {
            Role::Owner => match permission {
                Permission::ReadConfiguration
                | Permission::ManageProviders
                | Permission::ManageRoutes
                | Permission::ManageApiKeys
                | Permission::ReadAccess
                | Permission::ManageAccess
                | Permission::ManageSessions
                | Permission::ReadOperations
                | Permission::UsePlayground
                | Permission::ManageSettings
                | Permission::ManagePricing => true,
            },
            Role::Operator => match permission {
                Permission::ReadConfiguration
                | Permission::ManageProviders
                | Permission::ManageRoutes
                | Permission::ManageApiKeys
                | Permission::ReadAccess
                | Permission::ReadOperations
                | Permission::UsePlayground
                | Permission::ManageSettings
                | Permission::ManagePricing => true,
                Permission::ManageAccess | Permission::ManageSessions => false,
            },
            Role::Developer => match permission {
                Permission::ReadConfiguration
                | Permission::ManageApiKeys
                | Permission::ReadOperations
                | Permission::UsePlayground => true,
                Permission::ManageProviders
                | Permission::ManageRoutes
                | Permission::ReadAccess
                | Permission::ManageAccess
                | Permission::ManageSessions
                | Permission::ManageSettings
                | Permission::ManagePricing => false,
            },
            Role::Viewer => match permission {
                Permission::ReadConfiguration | Permission::ReadOperations => true,
                Permission::ManageProviders
                | Permission::ManageRoutes
                | Permission::ManageApiKeys
                | Permission::ReadAccess
                | Permission::ManageAccess
                | Permission::ManageSessions
                | Permission::UsePlayground
                | Permission::ManageSettings
                | Permission::ManagePricing => false,
            },
        }
    }

    #[test]
    fn fixed_role_permission_matrix_is_exhaustive() {
        for role in Role::ALL {
            for permission in Permission::ALL {
                assert_eq!(
                    role.allows(permission),
                    expected(role, permission),
                    "unexpected authorization decision for {role}/{permission:?}"
                );
            }
        }
    }

    #[test]
    fn role_storage_strings_are_closed_and_stable() {
        for role in Role::ALL {
            assert_eq!(role.as_str().parse::<Role>(), Ok(role));
            assert_eq!(role.to_string(), role.as_str());
        }
        assert_eq!("administrator".parse::<Role>(), Err(InvalidRole));
    }

    #[test]
    fn access_permission_strings_are_stable() {
        assert_eq!(
            serde_json::to_string(&Permission::ReadAccess).unwrap(),
            r#""read_access""#
        );
        assert_eq!(
            serde_json::to_string(&Permission::ManageAccess).unwrap(),
            r#""manage_access""#
        );
    }
}
