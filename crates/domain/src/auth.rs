use std::{
    collections::BTreeSet,
    fmt,
    num::{NonZeroU32, NonZeroU64},
    str::FromStr,
};

use chrono::{DateTime, Utc};
use serde::{Deserialize, Serialize};
use thiserror::Error;

use crate::{ApiKeyId, ApiKeyLookupId, OperationKind, RouteSlug};

#[derive(Clone, Copy, Debug, Deserialize, Eq, Hash, Ord, PartialEq, PartialOrd, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum Role {
    Owner,
    Operator,
    Developer,
    Viewer,
}

impl Role {
    pub const ALL: [Self; 4] = [Self::Owner, Self::Operator, Self::Developer, Self::Viewer];

    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::Owner => "owner",
            Self::Operator => "operator",
            Self::Developer => "developer",
            Self::Viewer => "viewer",
        }
    }
}

impl FromStr for Role {
    type Err = InvalidRole;

    fn from_str(value: &str) -> Result<Self, Self::Err> {
        match value {
            "owner" => Ok(Self::Owner),
            "operator" => Ok(Self::Operator),
            "developer" => Ok(Self::Developer),
            "viewer" => Ok(Self::Viewer),
            _ => Err(InvalidRole),
        }
    }
}

impl fmt::Display for Role {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(self.as_str())
    }
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
    ReadTeam,
    ManageTeam,
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
        Self::ReadTeam,
        Self::ManageTeam,
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
                    | Permission::ReadTeam
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

pub fn validate_owner_change(
    current_role: Role,
    new_role: Option<Role>,
    current_owner_count: usize,
) -> Result<(), OwnerInvariantError> {
    let removes_owner = current_role == Role::Owner && new_role != Some(Role::Owner);
    if removes_owner && current_owner_count <= 1 {
        return Err(OwnerInvariantError::LastOwner);
    }
    Ok(())
}

#[derive(Clone, Copy, Debug, Error, Eq, PartialEq)]
pub enum OwnerInvariantError {
    #[error("the installation must retain at least one owner")]
    LastOwner,
}

#[derive(Clone, Copy, Debug, Deserialize, Eq, Hash, Ord, PartialEq, PartialOrd, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum ApiKeyScope {
    Inference,
    ModelsRead,
}

impl ApiKeyScope {
    #[must_use]
    pub const fn permits(self, operation: OperationKind) -> bool {
        match self {
            Self::Inference => !matches!(
                operation,
                OperationKind::ModelList | OperationKind::ModelGet
            ),
            Self::ModelsRead => matches!(
                operation,
                OperationKind::ModelList | OperationKind::ModelGet
            ),
        }
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
    operation: OperationKind,
    now: DateTime<Utc>,
) -> Result<(), ApiKeyAuthorizationError> {
    if key.status == ApiKeyStatus::Revoked {
        return Err(ApiKeyAuthorizationError::Revoked);
    }
    if key.expires_at.is_some_and(|expiration| expiration <= now) {
        return Err(ApiKeyAuthorizationError::Expired);
    }
    if !key.scopes.iter().any(|scope| scope.permits(operation)) {
        return Err(ApiKeyAuthorizationError::MissingScope { operation });
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
    #[error("API key is revoked")]
    Revoked,
    #[error("API key is expired")]
    Expired,
    #[error("API key scope does not permit operation {operation:?}")]
    MissingScope { operation: OperationKind },
    #[error("API key does not allow route {route}")]
    RouteNotAllowed { route: RouteSlug },
}

#[cfg(test)]
mod tests {
    use super::*;

    const fn expected(role: Role, permission: Permission) -> bool {
        match role {
            Role::Owner => match permission {
                Permission::ReadConfiguration
                | Permission::ManageProviders
                | Permission::ManageRoutes
                | Permission::ManageApiKeys
                | Permission::ReadTeam
                | Permission::ManageTeam
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
                | Permission::ReadTeam
                | Permission::ReadOperations
                | Permission::UsePlayground
                | Permission::ManageSettings
                | Permission::ManagePricing => true,
                Permission::ManageTeam | Permission::ManageSessions => false,
            },
            Role::Developer => match permission {
                Permission::ReadConfiguration
                | Permission::ManageApiKeys
                | Permission::ReadOperations
                | Permission::UsePlayground => true,
                Permission::ManageProviders
                | Permission::ManageRoutes
                | Permission::ReadTeam
                | Permission::ManageTeam
                | Permission::ManageSessions
                | Permission::ManageSettings
                | Permission::ManagePricing => false,
            },
            Role::Viewer => match permission {
                Permission::ReadConfiguration | Permission::ReadOperations => true,
                Permission::ManageProviders
                | Permission::ManageRoutes
                | Permission::ManageApiKeys
                | Permission::ReadTeam
                | Permission::ManageTeam
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
}
