use std::fmt;

use chrono::{DateTime, Utc};
use olp_domain::Role;
use thiserror::Error;
use uuid::Uuid;

use crate::{InvitationMaterial, PersistenceError};

mod accounts;
mod auth_admission;
mod invitations;

#[derive(Debug, Error)]
pub enum IdentityError {
    #[error(transparent)]
    Persistence(#[from] PersistenceError),
    #[error("identity input is invalid: {0}")]
    Invalid(String),
    #[error("identity resource was not found")]
    NotFound,
    #[error("the resource changed after it was read")]
    PreconditionFailed,
    #[error("the last active owner cannot be demoted")]
    LastOwner,
    #[error("a user with this email already exists")]
    EmailAlreadyMember,
    #[error("a pending invitation for this email already exists")]
    PendingInvitationExists,
    #[error("the invitation is invalid, expired, or no longer pending")]
    InvitationUnavailable,
    #[error("the current user cannot revoke this session")]
    SessionForbidden,
    #[error("stored identity data is invalid")]
    CorruptIdentity,
    #[error("this idempotency key has already been used")]
    IdempotencyConflict,
    #[error("an operation with this idempotency key is still in progress")]
    IdempotencyInProgress,
    #[error("local password authentication is unavailable for this user")]
    LocalPasswordUnavailable,
    #[error("a local password is already configured for this user")]
    LocalPasswordAlreadyConfigured,
    #[error("recent authentication is required for this security change")]
    RecentAuthenticationRequired,
    #[error("the initiating session is no longer current")]
    SessionUnavailable,
}

impl From<sqlx::Error> for IdentityError {
    fn from(error: sqlx::Error) -> Self {
        Self::Persistence(PersistenceError::Database(error))
    }
}

#[derive(Debug, Clone)]
pub struct UserRecord {
    pub id: Uuid,
    pub email: String,
    pub display_name: String,
    pub role: Role,
    pub active: bool,
    pub etag: Uuid,
    pub created_at: DateTime<Utc>,
    pub updated_at: DateTime<Utc>,
}

#[derive(Debug, Clone)]
pub struct InvitationRecord {
    pub id: Uuid,
    pub email: String,
    pub role: Role,
    pub invited_by: Uuid,
    pub expires_at: DateTime<Utc>,
    pub accepted_at: Option<DateTime<Utc>>,
    pub revoked_at: Option<DateTime<Utc>>,
    pub created_at: DateTime<Utc>,
}

#[derive(Debug)]
pub struct InvitationCreated {
    pub invitation: InvitationRecord,
    pub material: InvitationMaterial,
}

#[derive(Debug)]
pub struct NewInvitation {
    pub email: String,
    pub role: Role,
    pub expires_at: DateTime<Utc>,
    pub actor: Uuid,
    pub idempotency_key: String,
}

pub struct AcceptInvitation {
    pub token: String,
    pub display_name: String,
    pub password_hash: String,
}

impl fmt::Debug for AcceptInvitation {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("AcceptInvitation")
            .field("token", &"[REDACTED]")
            .field("display_name", &self.display_name)
            .field("password_hash", &"[REDACTED]")
            .finish()
    }
}

#[derive(Debug, Clone)]
pub struct AcceptedInvitation {
    pub user: UserRecord,
    pub invitation_id: Uuid,
    pub session_id: Uuid,
}

#[derive(Debug, Clone)]
pub struct PasswordSessionRotation {
    pub user: UserRecord,
    pub session_id: Uuid,
}

#[derive(Debug, Clone)]
pub struct SessionRecord {
    pub id: Uuid,
    pub user_id: Uuid,
    pub expires_at: DateTime<Utc>,
    pub last_seen_at: DateTime<Utc>,
    pub created_at: DateTime<Utc>,
}

fn parse_role(value: String) -> Result<Role, IdentityError> {
    value.parse().map_err(|_| IdentityError::CorruptIdentity)
}

async fn insert_audit(
    transaction: &mut sqlx::Transaction<'_, sqlx::Postgres>,
    actor: Uuid,
    action: &str,
    resource_type: &str,
    resource_id: &str,
) -> Result<(), sqlx::Error> {
    sqlx::query!(
        "INSERT INTO audit_events \
         (id, actor_user_id, action, resource_type, resource_id, outcome) \
         VALUES ($1, $2, $3, $4, $5, 'success')",
        Uuid::now_v7(),
        actor,
        action,
        resource_type,
        resource_id
    )
    .execute(&mut **transaction)
    .await?;
    Ok(())
}

#[cfg(test)]
mod tests;
