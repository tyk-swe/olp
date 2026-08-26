use std::fmt;

use chrono::{DateTime, Utc};
use olp_engine::domain::auth::Role;
use thiserror::Error;
use uuid::Uuid;

use crate::{error::Error as PersistenceError, security::session_material::InvitationMaterial};

mod accounts;
mod auth_admission;
mod installation;
pub(crate) mod invitations;
mod setup;

pub struct InstallationSetupInput {
    pub installation_name: String,
    pub email: String,
    pub display_name: String,
    pub password_hash: String,
}

impl fmt::Debug for InstallationSetupInput {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("InstallationSetupInput")
            .field("installation_name", &self.installation_name)
            .field("email", &self.email)
            .field("display_name", &self.display_name)
            .field("password_hash", &"[REDACTED]")
            .finish()
    }
}

#[derive(Debug, Clone)]
pub struct InstallationSetupResult {
    pub user_id: Uuid,
    pub email: String,
    pub display_name: String,
    pub created_at: DateTime<Utc>,
}

#[derive(Debug, Error)]
pub enum Error {
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

impl From<sqlx::Error> for Error {
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
    /// Set when the invitation timed out and its pending-email reservation was
    /// released. Distinct from `revoked_at`, which records operator intent.
    pub expired_at: Option<DateTime<Utc>>,
    pub created_at: DateTime<Utc>,
    /// Emails of the operators behind each transition; absent once the user is
    /// removed, or while the transition has not happened.
    pub invited_by_email: Option<String>,
    pub accepted_by_email: Option<String>,
    pub revoked_by_email: Option<String>,
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

fn parse_role(value: String) -> Result<Role, Error> {
    value.parse().map_err(|_| Error::CorruptIdentity)
}

#[cfg(test)]
mod tests;
