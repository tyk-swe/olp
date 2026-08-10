use serde::{Deserialize, Serialize};
use utoipa::ToSchema;

use crate::{ProjectId, ServiceAccountId, TeamId, UserId};

#[derive(
    Clone, Copy, Debug, Deserialize, Eq, Hash, Ord, PartialEq, PartialOrd, Serialize, ToSchema,
)]
#[serde(rename_all = "snake_case")]
pub enum ApiKeyOwnerKind {
    User,
    ServiceAccount,
}

impl ApiKeyOwnerKind {
    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::User => "user",
            Self::ServiceAccount => "service_account",
        }
    }

    #[must_use]
    pub const fn with_id(self, id: uuid::Uuid) -> ApiKeyOwner {
        match self {
            Self::User => ApiKeyOwner::User(UserId::from_uuid(id)),
            Self::ServiceAccount => ApiKeyOwner::ServiceAccount(ServiceAccountId::from_uuid(id)),
        }
    }
}

#[derive(Clone, Copy, Debug, Deserialize, Eq, Hash, Ord, PartialEq, PartialOrd, Serialize)]
#[serde(tag = "kind", content = "id", rename_all = "snake_case")]
pub enum ApiKeyOwner {
    User(UserId),
    ServiceAccount(ServiceAccountId),
}

impl Default for ApiKeyOwner {
    fn default() -> Self {
        Self::User(UserId::from_uuid(uuid::Uuid::nil()))
    }
}

impl ApiKeyOwner {
    #[must_use]
    pub const fn from_ids(
        user_id: Option<uuid::Uuid>,
        service_account_id: Option<uuid::Uuid>,
    ) -> Option<Self> {
        match (user_id, service_account_id) {
            (Some(id), None) => Some(Self::User(UserId::from_uuid(id))),
            (None, Some(id)) => Some(Self::ServiceAccount(ServiceAccountId::from_uuid(id))),
            _ => None,
        }
    }

    #[must_use]
    pub const fn kind(self) -> ApiKeyOwnerKind {
        match self {
            Self::User(_) => ApiKeyOwnerKind::User,
            Self::ServiceAccount(_) => ApiKeyOwnerKind::ServiceAccount,
        }
    }

    #[must_use]
    pub const fn id(self) -> uuid::Uuid {
        match self {
            Self::User(id) => id.as_uuid(),
            Self::ServiceAccount(id) => id.as_uuid(),
        }
    }

    #[must_use]
    pub const fn split_ids(self) -> (Option<uuid::Uuid>, Option<uuid::Uuid>) {
        match self {
            Self::User(id) => (Some(id.as_uuid()), None),
            Self::ServiceAccount(id) => (None, Some(id.as_uuid())),
        }
    }
}

#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
pub struct Team {
    pub id: TeamId,
    pub name: String,
    pub active: bool,
}

#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
pub struct Project {
    pub id: ProjectId,
    pub team_id: TeamId,
    pub name: String,
    pub active: bool,
}

#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
pub struct ServiceAccount {
    pub id: ServiceAccountId,
    pub team_id: TeamId,
    pub project_id: ProjectId,
    pub name: String,
    pub active: bool,
}

#[derive(
    Clone, Copy, Debug, Deserialize, Eq, Hash, Ord, PartialEq, PartialOrd, Serialize, ToSchema,
)]
#[serde(rename_all = "snake_case")]
pub enum ScopedRole {
    Admin,
    Member,
}

impl ScopedRole {
    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::Admin => "admin",
            Self::Member => "member",
        }
    }
}

#[derive(Clone, Copy, Debug, Deserialize, Eq, PartialEq, Serialize)]
pub struct TeamMembership {
    pub team_id: TeamId,
    pub user_id: UserId,
    pub role: ScopedRole,
}

#[derive(Clone, Copy, Debug, Deserialize, Eq, PartialEq, Serialize)]
pub struct ProjectMembership {
    pub project_id: ProjectId,
    pub team_id: TeamId,
    pub user_id: UserId,
    pub role: ScopedRole,
}
