use std::fmt;

use chrono::{DateTime, Duration, Utc};
use olp_domain::Role;
use sqlx::Row;
use thiserror::Error;
use uuid::Uuid;

use crate::{
    IdempotencyOutcome, IdempotencyResponse, InvitationMaterial, PersistenceError, PgStore,
    ReplayableIdempotency, SessionMaterial, split_page,
    store::{
        ReplayableIdempotencyClaim, claim_idempotency, claim_replayable_idempotency,
        complete_idempotency, complete_replayable_idempotency,
    },
};

const MAX_PAGE_SIZE: i64 = 100;
const IDENTITY_EMAIL_LOCK_SEED: i64 = 0x4f4c_505f_4944;
const LOCAL_LOGIN_SOURCE_ATTEMPTS_PER_MINUTE: i32 = 60;
const INVITATION_SOURCE_ATTEMPTS_PER_MINUTE: i32 = 30;
const OIDC_LOGIN_SOURCE_ATTEMPTS_PER_MINUTE: i32 = 60;
const SOURCE_TARGET_ATTEMPTS_PER_MINUTE: i32 = 5;
const PUBLIC_AUTH_RESOURCE_ATTEMPTS_PER_MINUTE: i32 = 10_000;
const PUBLIC_AUTH_DELETE_BATCH: i64 = 1_000;

#[derive(Debug, Error)]
pub enum TeamError {
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
}

impl From<sqlx::Error> for TeamError {
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
pub struct SessionRecord {
    pub id: Uuid,
    pub user_id: Uuid,
    pub expires_at: DateTime<Utc>,
    pub last_seen_at: DateTime<Utc>,
    pub created_at: DateTime<Utc>,
}

impl PgStore {
    /// Atomically admits an unauthenticated local-login attempt across every
    /// control-plane replica. The caller supplies domain-separated, keyed
    /// digests for the client source and source-plus-submitted-email pair.
    pub async fn admit_local_login_attempt(
        &self,
        source_digest: [u8; 32],
        source_target_digest: [u8; 32],
    ) -> Result<bool, TeamError> {
        self.admit_public_auth_attempt(
            "local_login",
            source_digest,
            Some(source_target_digest),
            LOCAL_LOGIN_SOURCE_ATTEMPTS_PER_MINUTE,
        )
        .await
    }

    /// Atomically admits an unauthenticated invitation-acceptance attempt
    /// without retaining the submitted invitation token. The target digest is
    /// bound to the source so one attacker cannot exhaust another source's
    /// attempt budget for the same invitation.
    pub async fn admit_invitation_acceptance_attempt(
        &self,
        source_digest: [u8; 32],
        source_target_digest: [u8; 32],
    ) -> Result<bool, TeamError> {
        self.admit_public_auth_attempt(
            "invitation_acceptance",
            source_digest,
            Some(source_target_digest),
            INVITATION_SOURCE_ATTEMPTS_PER_MINUTE,
        )
        .await
    }

    /// Admits an unauthenticated OIDC login initiation. Login starts need a
    /// source-only budget because no password or invitation target is present.
    pub async fn admit_oidc_login_attempt(
        &self,
        source_digest: [u8; 32],
    ) -> Result<bool, TeamError> {
        self.admit_public_auth_attempt(
            "oidc_login",
            source_digest,
            None,
            OIDC_LOGIN_SOURCE_ATTEMPTS_PER_MINUTE,
        )
        .await
    }

    async fn admit_public_auth_attempt(
        &self,
        action: &str,
        source_digest: [u8; 32],
        source_target_digest: Option<[u8; 32]>,
        source_limit: i32,
    ) -> Result<bool, TeamError> {
        let mut transaction = self.pool().begin().await?;
        // This high ceiling is resource admission, not a user-facing policy.
        // It bounds attacker-controlled source rows before they are inserted.
        let resource_admitted = consume_public_auth_bucket(
            &mut transaction,
            action,
            "global",
            &[0_u8; 32],
            PUBLIC_AUTH_RESOURCE_ATTEMPTS_PER_MINUTE,
        )
        .await?;
        if !resource_admitted {
            transaction.commit().await?;
            return Ok(false);
        }
        let source_admitted = consume_public_auth_bucket(
            &mut transaction,
            action,
            "source",
            &source_digest,
            source_limit,
        )
        .await?;
        if !source_admitted {
            transaction.rollback().await?;
            return Ok(false);
        }
        let source_target_admitted = if let Some(source_target_digest) = source_target_digest {
            consume_public_auth_bucket(
                &mut transaction,
                action,
                "source_target",
                &source_target_digest,
                SOURCE_TARGET_ATTEMPTS_PER_MINUTE,
            )
            .await?
        } else {
            true
        };
        if !source_target_admitted {
            transaction.rollback().await?;
            return Ok(false);
        }
        sqlx::query(
            "WITH expired AS ( \
               SELECT ctid FROM public_auth_rate_limits \
               WHERE window_started_at <= now() - interval '10 minutes' \
               LIMIT $1 \
             ) \
             DELETE FROM public_auth_rate_limits rate_limit USING expired \
             WHERE rate_limit.ctid = expired.ctid",
        )
        .bind(PUBLIC_AUTH_DELETE_BATCH)
        .execute(&mut *transaction)
        .await?;
        transaction.commit().await?;
        Ok(true)
    }

    pub async fn list_users(
        &self,
        cursor: Option<Uuid>,
        limit: i64,
    ) -> Result<(Vec<UserRecord>, Option<Uuid>), TeamError> {
        let limit = limit.clamp(1, MAX_PAGE_SIZE);
        let rows = sqlx::query(
            "SELECT id, email, display_name, role::text AS role, active, etag, created_at, updated_at \
             FROM users WHERE ($1::uuid IS NULL OR id < $1) ORDER BY id DESC LIMIT $2",
        )
        .bind(cursor)
        .bind(limit + 1)
        .fetch_all(self.pool())
        .await?;
        let users = rows
            .into_iter()
            .map(user_from_row)
            .collect::<Result<Vec<_>, _>>()?;
        let (users, next_cursor) = split_page(users, limit as usize, |user| user.id);
        Ok((users, next_cursor))
    }

    pub async fn user(&self, id: Uuid) -> Result<Option<UserRecord>, TeamError> {
        let row = sqlx::query(
            "SELECT id, email, display_name, role::text AS role, active, etag, created_at, updated_at \
             FROM users WHERE id = $1",
        )
        .bind(id)
        .fetch_optional(self.pool())
        .await?;
        row.map(user_from_row).transpose()
    }

    pub async fn update_user_role(
        &self,
        id: Uuid,
        role: Role,
        expected_etag: Uuid,
        actor: Uuid,
    ) -> Result<UserRecord, TeamError> {
        let mut transaction = self.pool().begin().await?;
        let current = sqlx::query("SELECT etag FROM users WHERE id = $1 FOR UPDATE")
            .bind(id)
            .fetch_optional(&mut *transaction)
            .await?
            .ok_or(TeamError::NotFound)?;
        if current.get::<Uuid, _>("etag") != expected_etag {
            return Err(TeamError::PreconditionFailed);
        }

        let etag = Uuid::now_v7();
        let row = match sqlx::query(
            "UPDATE users SET role = CAST($2 AS user_role), etag = $3, updated_at = now() \
             WHERE id = $1 \
             RETURNING id, email, display_name, role::text AS role, active, etag, created_at, updated_at",
        )
        .bind(id)
        .bind(role.as_str())
        .bind(etag)
        .fetch_one(&mut *transaction)
        .await
        {
            Ok(row) => row,
            Err(error) if is_last_owner_violation(&error) => return Err(TeamError::LastOwner),
            Err(error) => return Err(error.into()),
        };

        let revoked = sqlx::query("DELETE FROM sessions WHERE user_id = $1")
            .bind(id)
            .execute(&mut *transaction)
            .await?
            .rows_affected();
        insert_audit(
            &mut transaction,
            actor,
            "user.role_update",
            "user",
            &id.to_string(),
        )
        .await?;
        if revoked > 0 {
            insert_audit(
                &mut transaction,
                actor,
                "session.revoke_for_role_change",
                "user",
                &id.to_string(),
            )
            .await?;
        }
        transaction.commit().await?;
        user_from_row(row)
    }

    pub async fn update_user_access(
        &self,
        id: Uuid,
        role: Option<Role>,
        active: Option<bool>,
        expected_etag: Uuid,
        actor: Uuid,
    ) -> Result<UserRecord, TeamError> {
        if role.is_none() && active.is_none() {
            return Err(TeamError::Invalid(
                "role or active status is required".to_owned(),
            ));
        }
        let mut transaction = self.pool().begin().await?;
        let current = sqlx::query("SELECT etag FROM users WHERE id = $1 FOR UPDATE")
            .bind(id)
            .fetch_optional(&mut *transaction)
            .await?
            .ok_or(TeamError::NotFound)?;
        if current.get::<Uuid, _>("etag") != expected_etag {
            return Err(TeamError::PreconditionFailed);
        }
        let etag = Uuid::now_v7();
        let row = match sqlx::query(
            "UPDATE users SET \
                 role = COALESCE(CAST($2 AS user_role), role), \
                 active = COALESCE($3, active), etag = $4, updated_at = now() \
             WHERE id = $1 \
             RETURNING id, email, display_name, role::text AS role, active, etag, created_at, updated_at",
        )
        .bind(id)
        .bind(role.map(|role| role.as_str()))
        .bind(active)
        .bind(etag)
        .fetch_one(&mut *transaction)
        .await
        {
            Ok(row) => row,
            Err(error) if is_last_owner_violation(&error) => return Err(TeamError::LastOwner),
            Err(error) => return Err(error.into()),
        };
        let revoked = sqlx::query("DELETE FROM sessions WHERE user_id = $1")
            .bind(id)
            .execute(&mut *transaction)
            .await?
            .rows_affected();
        insert_audit(
            &mut transaction,
            actor,
            "user.access_update",
            "user",
            &id.to_string(),
        )
        .await?;
        if revoked > 0 {
            insert_audit(
                &mut transaction,
                actor,
                "session.revoke_for_access_change",
                "user",
                &id.to_string(),
            )
            .await?;
        }
        transaction.commit().await?;
        user_from_row(row)
    }

    pub async fn update_profile(
        &self,
        id: Uuid,
        display_name: &str,
        expected_etag: Uuid,
    ) -> Result<UserRecord, TeamError> {
        let display_name = display_name.trim();
        if display_name.is_empty() || display_name.chars().count() > 100 {
            return Err(TeamError::Invalid(
                "display name must contain 1-100 characters".to_owned(),
            ));
        }
        let mut transaction = self.pool().begin().await?;
        let row = sqlx::query(
            "UPDATE users SET display_name = $2, etag = $3, updated_at = now()
             WHERE id = $1 AND etag = $4
             RETURNING id, email, display_name, role::text AS role, active, etag,
                       created_at, updated_at",
        )
        .bind(id)
        .bind(display_name)
        .bind(Uuid::now_v7())
        .bind(expected_etag)
        .fetch_optional(&mut *transaction)
        .await?;
        let Some(row) = row else {
            let exists: bool =
                sqlx::query_scalar("SELECT EXISTS (SELECT 1 FROM users WHERE id = $1)")
                    .bind(id)
                    .fetch_one(&mut *transaction)
                    .await?;
            return Err(if exists {
                TeamError::PreconditionFailed
            } else {
                TeamError::NotFound
            });
        };
        insert_audit(
            &mut transaction,
            id,
            "user.profile_update",
            "user",
            &id.to_string(),
        )
        .await?;
        transaction.commit().await?;
        user_from_row(row)
    }

    pub async fn update_local_password(
        &self,
        id: Uuid,
        password_hash: &str,
        expected_etag: Uuid,
        keep_session_id: Uuid,
    ) -> Result<UserRecord, TeamError> {
        let mut transaction = self.pool().begin().await?;
        let row = sqlx::query(
            "UPDATE users SET password_hash = $2, etag = $3, updated_at = now()
             WHERE id = $1 AND etag = $4 AND password_hash IS NOT NULL
             RETURNING id, email, display_name, role::text AS role, active, etag,
                       created_at, updated_at",
        )
        .bind(id)
        .bind(password_hash)
        .bind(Uuid::now_v7())
        .bind(expected_etag)
        .fetch_optional(&mut *transaction)
        .await?;
        let Some(row) = row else {
            let current = sqlx::query(
                "SELECT etag, password_hash IS NOT NULL AS local FROM users WHERE id = $1",
            )
            .bind(id)
            .fetch_optional(&mut *transaction)
            .await?
            .ok_or(TeamError::NotFound)?;
            if !current.get::<bool, _>("local") {
                return Err(TeamError::LocalPasswordUnavailable);
            }
            return Err(TeamError::PreconditionFailed);
        };
        sqlx::query("DELETE FROM sessions WHERE user_id = $1 AND id <> $2")
            .bind(id)
            .bind(keep_session_id)
            .execute(&mut *transaction)
            .await?;
        insert_audit(
            &mut transaction,
            id,
            "user.password_update",
            "user",
            &id.to_string(),
        )
        .await?;
        transaction.commit().await?;
        user_from_row(row)
    }

    /// Adds the first local password to an OIDC-only account. The `IS NULL`
    /// predicate makes enrollment race-safe and keeps ordinary password
    /// changes on the separate current-password-verified path.
    pub async fn enroll_local_password(
        &self,
        id: Uuid,
        password_hash: &str,
        expected_etag: Uuid,
        keep_session_id: Uuid,
    ) -> Result<UserRecord, TeamError> {
        let mut transaction = self.pool().begin().await?;
        let row = sqlx::query(
            "UPDATE users SET password_hash = $2, etag = $3, updated_at = now() \
             WHERE id = $1 AND etag = $4 AND password_hash IS NULL AND active \
             RETURNING id, email, display_name, role::text AS role, active, etag, \
                       created_at, updated_at",
        )
        .bind(id)
        .bind(password_hash)
        .bind(Uuid::now_v7())
        .bind(expected_etag)
        .fetch_optional(&mut *transaction)
        .await?;
        let Some(row) = row else {
            let current = sqlx::query(
                "SELECT etag, password_hash IS NOT NULL AS local \
                 FROM users WHERE id = $1 AND active",
            )
            .bind(id)
            .fetch_optional(&mut *transaction)
            .await?
            .ok_or(TeamError::NotFound)?;
            if current.get::<bool, _>("local") {
                return Err(TeamError::LocalPasswordAlreadyConfigured);
            }
            return Err(TeamError::PreconditionFailed);
        };
        sqlx::query("DELETE FROM sessions WHERE user_id = $1 AND id <> $2")
            .bind(id)
            .bind(keep_session_id)
            .execute(&mut *transaction)
            .await?;
        insert_audit(
            &mut transaction,
            id,
            "user.password_enroll",
            "user",
            &id.to_string(),
        )
        .await?;
        transaction.commit().await?;
        user_from_row(row)
    }

    pub async fn create_invitation<F>(
        &self,
        invitation: NewInvitation,
        replay: ReplayableIdempotency<'_>,
        build_response: F,
    ) -> Result<IdempotencyOutcome<InvitationCreated>, TeamError>
    where
        F: FnOnce(&InvitationCreated) -> Result<IdempotencyResponse, PersistenceError>,
    {
        let email = normalize_email(&invitation.email)?;
        let mut transaction = self.pool().begin().await?;
        match claim_replayable_idempotency(
            &mut transaction,
            invitation.actor,
            "invitation.create",
            &invitation.idempotency_key,
            replay.request_fingerprint(),
            replay.master_key(),
        )
        .await?
        {
            ReplayableIdempotencyClaim::Execute => {}
            ReplayableIdempotencyClaim::Replay(response) => {
                transaction.rollback().await?;
                return Ok(IdempotencyOutcome::Replayed(response));
            }
            ReplayableIdempotencyClaim::Conflict => {
                transaction.rollback().await?;
                return Err(TeamError::IdempotencyConflict);
            }
            ReplayableIdempotencyClaim::InProgress => {
                transaction.rollback().await?;
                return Err(TeamError::IdempotencyInProgress);
            }
        }
        let now = Utc::now();
        if invitation.expires_at <= now || invitation.expires_at > now + Duration::days(30) {
            return Err(TeamError::Invalid(
                "expiration must be within the next 30 days".to_owned(),
            ));
        }
        lock_identity_email(&mut transaction, &email).await?;
        let member_exists: bool =
            sqlx::query_scalar("SELECT EXISTS (SELECT 1 FROM users WHERE email = $1)")
                .bind(&email)
                .fetch_one(&mut *transaction)
                .await?;
        if member_exists {
            return Err(TeamError::EmailAlreadyMember);
        }
        let pending_exists: bool = sqlx::query_scalar(
            "SELECT EXISTS (SELECT 1 FROM invitations WHERE email = $1 \
             AND accepted_at IS NULL AND revoked_at IS NULL AND expires_at > now())",
        )
        .bind(&email)
        .fetch_one(&mut *transaction)
        .await?;
        if pending_exists {
            return Err(TeamError::PendingInvitationExists);
        }
        // Expired pending rows no longer reserve the partial unique index.
        sqlx::query(
            "UPDATE invitations SET revoked_at = now(), revoked_by = $2 \
             WHERE email = $1 AND accepted_at IS NULL AND revoked_at IS NULL AND expires_at <= now()",
        )
        .bind(&email)
        .bind(invitation.actor)
        .execute(&mut *transaction)
        .await?;

        let id = Uuid::now_v7();
        let material = InvitationMaterial::generate();
        let row = match sqlx::query(
            "INSERT INTO invitations \
             (id, email, role, token_digest, invited_by, expires_at, created_at) \
             VALUES ($1, $2, CAST($3 AS user_role), $4, $5, $6, $7) \
             RETURNING id, email, role::text AS role, invited_by, expires_at, accepted_at, revoked_at, created_at",
        )
        .bind(id)
        .bind(&email)
        .bind(invitation.role.as_str())
        .bind(material.token_digest().to_vec())
        .bind(invitation.actor)
        .bind(invitation.expires_at)
        .bind(now)
        .fetch_one(&mut *transaction)
        .await
        {
            Ok(row) => row,
            Err(error) if is_constraint(&error, "invitations_pending_email_idx") => {
                return Err(TeamError::PendingInvitationExists);
            }
            Err(error) => return Err(error.into()),
        };
        insert_audit(
            &mut transaction,
            invitation.actor,
            "invitation.create",
            "invitation",
            &id.to_string(),
        )
        .await?;
        let created = InvitationCreated {
            invitation: invitation_from_row(row)?,
            material,
        };
        let response = build_response(&created)?;
        complete_replayable_idempotency(
            &mut transaction,
            invitation.actor,
            "invitation.create",
            &invitation.idempotency_key,
            replay.request_fingerprint(),
            replay.master_key(),
            &response,
        )
        .await?;
        transaction.commit().await?;
        Ok(IdempotencyOutcome::Executed {
            value: created,
            response,
        })
    }

    pub async fn list_invitations(
        &self,
        cursor: Option<Uuid>,
        limit: i64,
    ) -> Result<(Vec<InvitationRecord>, Option<Uuid>), TeamError> {
        let limit = limit.clamp(1, MAX_PAGE_SIZE);
        let rows = sqlx::query(
            "SELECT id, email, role::text AS role, invited_by, expires_at, accepted_at, revoked_at, created_at \
             FROM invitations WHERE ($1::uuid IS NULL OR id < $1) ORDER BY id DESC LIMIT $2",
        )
        .bind(cursor)
        .bind(limit + 1)
        .fetch_all(self.pool())
        .await?;
        let invitations = rows
            .into_iter()
            .map(invitation_from_row)
            .collect::<Result<Vec<_>, _>>()?;
        let (invitations, next_cursor) =
            split_page(invitations, limit as usize, |invitation| invitation.id);
        Ok((invitations, next_cursor))
    }

    pub async fn revoke_invitation(
        &self,
        id: Uuid,
        actor: Uuid,
        idempotency_key: &str,
    ) -> Result<InvitationRecord, TeamError> {
        let mut transaction = self.pool().begin().await?;
        if !claim_idempotency(
            &mut transaction,
            actor,
            "invitation.revoke",
            idempotency_key,
        )
        .await?
        {
            return Err(TeamError::IdempotencyConflict);
        }
        let row = sqlx::query(
            "UPDATE invitations SET revoked_at = now(), revoked_by = $2 \
             WHERE id = $1 AND accepted_at IS NULL AND revoked_at IS NULL \
             RETURNING id, email, role::text AS role, invited_by, expires_at, accepted_at, revoked_at, created_at",
        )
        .bind(id)
        .bind(actor)
        .fetch_optional(&mut *transaction)
        .await?
        .ok_or(TeamError::InvitationUnavailable)?;
        insert_audit(
            &mut transaction,
            actor,
            "invitation.revoke",
            "invitation",
            &id.to_string(),
        )
        .await?;
        complete_idempotency(
            &mut transaction,
            actor,
            "invitation.revoke",
            idempotency_key,
            &id.to_string(),
        )
        .await?;
        transaction.commit().await?;
        invitation_from_row(row)
    }

    pub async fn accept_invitation(
        &self,
        acceptance: AcceptInvitation,
        session: &SessionMaterial,
        session_ttl: Duration,
    ) -> Result<AcceptedInvitation, TeamError> {
        if acceptance.token.len() != 43
            || acceptance.display_name.trim().is_empty()
            || acceptance.display_name.chars().count() > 100
        {
            return Err(TeamError::InvitationUnavailable);
        }
        let session_expires_at = Utc::now()
            .checked_add_signed(session_ttl)
            .filter(|expires_at| *expires_at > Utc::now())
            .ok_or_else(|| TeamError::Invalid("session lifetime is invalid".to_owned()))?;
        let digest = InvitationMaterial::digest_token(&acceptance.token);
        let mut transaction = self.pool().begin().await?;
        let invitation_email: String =
            sqlx::query_scalar("SELECT email FROM invitations WHERE token_digest = $1")
                .bind(digest.to_vec())
                .fetch_optional(&mut *transaction)
                .await?
                .ok_or(TeamError::InvitationUnavailable)?;
        lock_identity_email(&mut transaction, &invitation_email).await?;
        let invitation = sqlx::query(
            "SELECT id, email, role::text AS role, expires_at, accepted_at, revoked_at \
             FROM invitations WHERE token_digest = $1 FOR UPDATE",
        )
        .bind(digest.to_vec())
        .fetch_optional(&mut *transaction)
        .await?
        .ok_or(TeamError::InvitationUnavailable)?;
        let expires_at: DateTime<Utc> = invitation.get("expires_at");
        if invitation
            .get::<Option<DateTime<Utc>>, _>("accepted_at")
            .is_some()
            || invitation
                .get::<Option<DateTime<Utc>>, _>("revoked_at")
                .is_some()
            || expires_at <= Utc::now()
        {
            return Err(TeamError::InvitationUnavailable);
        }
        let invitation_id: Uuid = invitation.get("id");
        let email: String = invitation.get("email");
        let role = parse_role(invitation.get("role"))?;
        let user_id = Uuid::now_v7();
        let etag = Uuid::now_v7();
        let now = Utc::now();
        let user_row = match sqlx::query(
            "INSERT INTO users \
             (id, email, display_name, password_hash, role, active, etag, created_at, updated_at) \
             VALUES ($1, $2, $3, $4, CAST($5 AS user_role), true, $6, $7, $7) \
             RETURNING id, email, display_name, role::text AS role, active, etag, created_at, updated_at",
        )
        .bind(user_id)
        .bind(&email)
        .bind(acceptance.display_name.trim())
        .bind(&acceptance.password_hash)
        .bind(role.as_str())
        .bind(etag)
        .bind(now)
        .fetch_one(&mut *transaction)
        .await
        {
            Ok(row) => row,
            Err(error) if is_constraint(&error, "users_email_unique") => {
                return Err(TeamError::EmailAlreadyMember);
            }
            Err(error) => return Err(error.into()),
        };
        let updated = sqlx::query(
            "UPDATE invitations SET accepted_at = $2, accepted_by = $3 \
             WHERE id = $1 AND accepted_at IS NULL AND revoked_at IS NULL",
        )
        .bind(invitation_id)
        .bind(now)
        .bind(user_id)
        .execute(&mut *transaction)
        .await?;
        if updated.rows_affected() != 1 {
            return Err(TeamError::InvitationUnavailable);
        }
        insert_audit(
            &mut transaction,
            user_id,
            "invitation.accept",
            "invitation",
            &invitation_id.to_string(),
        )
        .await?;
        insert_audit(
            &mut transaction,
            user_id,
            "user.create",
            "user",
            &user_id.to_string(),
        )
        .await?;
        let session_id = Uuid::now_v7();
        sqlx::query(
            "INSERT INTO sessions \
             (id, user_id, token_digest, csrf_digest, expires_at, last_seen_at, created_at) \
             VALUES ($1, $2, $3, $4, $5, $6, $6)",
        )
        .bind(session_id)
        .bind(user_id)
        .bind(session.token_digest().to_vec())
        .bind(session.csrf_digest().to_vec())
        .bind(session_expires_at)
        .bind(now)
        .execute(&mut *transaction)
        .await?;
        insert_audit(
            &mut transaction,
            user_id,
            "session.create",
            "session",
            &session_id.to_string(),
        )
        .await?;
        transaction.commit().await?;
        Ok(AcceptedInvitation {
            user: user_from_row(user_row)?,
            invitation_id,
            session_id,
        })
    }

    pub async fn list_sessions(
        &self,
        user_id: Uuid,
        cursor: Option<Uuid>,
        limit: i64,
    ) -> Result<(Vec<SessionRecord>, Option<Uuid>), TeamError> {
        let limit = limit.clamp(1, MAX_PAGE_SIZE);
        let rows = sqlx::query(
            "SELECT id, user_id, expires_at, last_seen_at, created_at FROM sessions \
             WHERE user_id = $1 AND expires_at > now() AND ($2::uuid IS NULL OR id < $2) \
             ORDER BY id DESC LIMIT $3",
        )
        .bind(user_id)
        .bind(cursor)
        .bind(limit + 1)
        .fetch_all(self.pool())
        .await?;
        let sessions = rows
            .into_iter()
            .map(|row| SessionRecord {
                id: row.get("id"),
                user_id: row.get("user_id"),
                expires_at: row.get("expires_at"),
                last_seen_at: row.get("last_seen_at"),
                created_at: row.get("created_at"),
            })
            .collect::<Vec<_>>();
        let (sessions, next_cursor) = split_page(sessions, limit as usize, |session| session.id);
        Ok((sessions, next_cursor))
    }

    pub async fn revoke_session(
        &self,
        session_id: Uuid,
        actor: Uuid,
        can_manage_all: bool,
    ) -> Result<(), TeamError> {
        let mut transaction = self.pool().begin().await?;
        let session = sqlx::query("SELECT user_id FROM sessions WHERE id = $1 FOR UPDATE")
            .bind(session_id)
            .fetch_optional(&mut *transaction)
            .await?
            .ok_or(TeamError::NotFound)?;
        let user_id: Uuid = session.get("user_id");
        if user_id != actor && !can_manage_all {
            return Err(TeamError::SessionForbidden);
        }
        sqlx::query("DELETE FROM sessions WHERE id = $1")
            .bind(session_id)
            .execute(&mut *transaction)
            .await?;
        insert_audit(
            &mut transaction,
            actor,
            "session.revoke",
            "session",
            &session_id.to_string(),
        )
        .await?;
        transaction.commit().await?;
        Ok(())
    }
}

async fn consume_public_auth_bucket(
    transaction: &mut sqlx::Transaction<'_, sqlx::Postgres>,
    action: &str,
    scope: &str,
    key_digest: &[u8; 32],
    limit: i32,
) -> Result<bool, sqlx::Error> {
    let admitted: Option<bool> = sqlx::query_scalar(
        "INSERT INTO public_auth_rate_limits \
         (action, scope, key_digest, window_started_at, attempts) \
         VALUES ($1, $2, $3, now(), 1) \
         ON CONFLICT (action, scope, key_digest) DO UPDATE SET \
             window_started_at = CASE \
                 WHEN public_auth_rate_limits.window_started_at <= now() - interval '1 minute' \
                 THEN now() ELSE public_auth_rate_limits.window_started_at END, \
             attempts = CASE \
                 WHEN public_auth_rate_limits.window_started_at <= now() - interval '1 minute' \
                 THEN 1 ELSE public_auth_rate_limits.attempts + 1 END \
         WHERE public_auth_rate_limits.window_started_at <= now() - interval '1 minute' \
            OR public_auth_rate_limits.attempts < $4 \
         RETURNING true",
    )
    .bind(action)
    .bind(scope)
    .bind(key_digest.as_slice())
    .bind(limit)
    .fetch_optional(&mut **transaction)
    .await?;
    Ok(admitted.unwrap_or(false))
}

fn normalize_email(email: &str) -> Result<String, TeamError> {
    let email = email.trim().to_lowercase();
    if email.len() > 254 || !email.contains('@') || email.starts_with('@') || email.ends_with('@') {
        return Err(TeamError::Invalid("email is invalid".to_owned()));
    }
    Ok(email)
}

fn user_from_row(row: sqlx::postgres::PgRow) -> Result<UserRecord, TeamError> {
    Ok(UserRecord {
        id: row.get("id"),
        email: row.get("email"),
        display_name: row.get("display_name"),
        role: parse_role(row.get("role"))?,
        active: row.get("active"),
        etag: row.get("etag"),
        created_at: row.get("created_at"),
        updated_at: row.get("updated_at"),
    })
}

fn invitation_from_row(row: sqlx::postgres::PgRow) -> Result<InvitationRecord, TeamError> {
    Ok(InvitationRecord {
        id: row.get("id"),
        email: row.get("email"),
        role: parse_role(row.get("role"))?,
        invited_by: row.get("invited_by"),
        expires_at: row.get("expires_at"),
        accepted_at: row.get("accepted_at"),
        revoked_at: row.get("revoked_at"),
        created_at: row.get("created_at"),
    })
}

fn parse_role(value: String) -> Result<Role, TeamError> {
    value.parse().map_err(|_| TeamError::CorruptIdentity)
}

async fn lock_identity_email(
    transaction: &mut sqlx::Transaction<'_, sqlx::Postgres>,
    email: &str,
) -> Result<(), sqlx::Error> {
    sqlx::query("SELECT pg_advisory_xact_lock(hashtextextended($1, $2))")
        .bind(email)
        .bind(IDENTITY_EMAIL_LOCK_SEED)
        .execute(&mut **transaction)
        .await?;
    Ok(())
}

async fn insert_audit(
    transaction: &mut sqlx::Transaction<'_, sqlx::Postgres>,
    actor: Uuid,
    action: &str,
    resource_type: &str,
    resource_id: &str,
) -> Result<(), sqlx::Error> {
    sqlx::query(
        "INSERT INTO audit_events \
         (id, actor_user_id, action, resource_type, resource_id, outcome) \
         VALUES ($1, $2, $3, $4, $5, 'success')",
    )
    .bind(Uuid::now_v7())
    .bind(actor)
    .bind(action)
    .bind(resource_type)
    .bind(resource_id)
    .execute(&mut **transaction)
    .await?;
    Ok(())
}

fn is_last_owner_violation(error: &sqlx::Error) -> bool {
    matches!(error, sqlx::Error::Database(database)
        if database.code().as_deref() == Some("23514")
            && database.message().contains("last active owner"))
}

fn is_constraint(error: &sqlx::Error, constraint: &str) -> bool {
    matches!(error, sqlx::Error::Database(database)
        if database.constraint() == Some(constraint))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn roles_are_a_closed_fixed_set() {
        for role in ["owner", "operator", "developer", "viewer"] {
            assert_eq!(role.parse::<Role>().unwrap().as_str(), role);
        }
        assert!("administrator".parse::<Role>().is_err());
    }

    #[test]
    fn invitation_email_normalization_is_strict() {
        assert_eq!(
            normalize_email("  Person@Example.TEST ").unwrap(),
            "person@example.test"
        );
        assert!(normalize_email("not-an-email").is_err());
        assert!(normalize_email("@example.test").is_err());
    }
}
