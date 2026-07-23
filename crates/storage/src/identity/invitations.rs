use chrono::{DateTime, Duration, Utc};
use sqlx::FromRow;
use uuid::Uuid;

use crate::{
    IdempotencyOutcome, IdempotencyResponse, InvitationMaterial, PersistenceError, PgStore,
    ReplayableIdempotency, SessionMaterial,
    authentication::insert_versioned_session,
    split_page,
    store::{
        ReplayableIdempotencyClaim, claim_idempotency, claim_replayable_idempotency,
        complete_idempotency, complete_replayable_idempotency,
    },
};

use super::{
    AcceptInvitation, AcceptedInvitation, IdentityError, InvitationCreated, InvitationRecord,
    NewInvitation, accounts::UserRow, insert_audit, parse_role,
};

const MAX_PAGE_SIZE: i64 = 100;
const IDENTITY_EMAIL_LOCK_SEED: i64 = 0x4f4c_505f_4944;

impl PgStore {
    pub async fn create_invitation<F>(
        &self,
        invitation: NewInvitation,
        replay: ReplayableIdempotency<'_>,
        build_response: F,
    ) -> Result<IdempotencyOutcome<InvitationCreated>, IdentityError>
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
                return Err(IdentityError::IdempotencyConflict);
            }
            ReplayableIdempotencyClaim::InProgress => {
                transaction.rollback().await?;
                return Err(IdentityError::IdempotencyInProgress);
            }
        }
        let now = Utc::now();
        if invitation.expires_at <= now || invitation.expires_at > now + Duration::days(30) {
            return Err(IdentityError::Invalid(
                "expiration must be within the next 30 days".to_owned(),
            ));
        }
        lock_identity_email(&mut transaction, &email).await?;
        let member_exists: bool = sqlx::query_scalar!(
            "SELECT EXISTS (SELECT 1 FROM users WHERE email = $1) AS \"value!\"",
            &email
        )
        .fetch_one(&mut *transaction)
        .await?;
        if member_exists {
            return Err(IdentityError::EmailAlreadyMember);
        }
        let pending_exists: bool = sqlx::query_scalar!(
            "SELECT EXISTS (SELECT 1 FROM invitations WHERE email = $1 \
             AND accepted_at IS NULL AND revoked_at IS NULL AND expires_at > now()) AS \"value!\"",
            &email
        )
        .fetch_one(&mut *transaction)
        .await?;
        if pending_exists {
            return Err(IdentityError::PendingInvitationExists);
        }
        // Expired pending rows no longer reserve the partial unique index.
        sqlx::query!(
            "UPDATE invitations SET revoked_at = now(), revoked_by = $2 \
             WHERE email = $1 AND accepted_at IS NULL AND revoked_at IS NULL AND expires_at <= now()",
        &email, invitation.actor)
        .execute(&mut *transaction)
        .await?;

        let id = Uuid::now_v7();
        let material = InvitationMaterial::generate();
        let row = match sqlx::query_as!(
            InvitationRow,
            "INSERT INTO invitations \
             (id, email, role, token_digest, invited_by, expires_at, created_at) \
             VALUES ($1, $2, CAST($3::text AS user_role), $4, $5, $6, $7) \
             RETURNING id, email, role::text AS \"role!\", invited_by, expires_at, accepted_at, revoked_at, created_at",
        id, &email, invitation.role.as_str(), material.token_digest().to_vec(), invitation.actor, invitation.expires_at, now)
        .fetch_one(&mut *transaction)
        .await
        {
            Ok(row) => row,
            Err(error) if is_constraint(&error, "invitations_pending_email_idx") => {
                return Err(IdentityError::PendingInvitationExists);
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
    ) -> Result<(Vec<InvitationRecord>, Option<Uuid>), IdentityError> {
        let limit = limit.clamp(1, MAX_PAGE_SIZE);
        let rows = sqlx::query_as!(
            InvitationRow,
            "SELECT id, email, role::text AS \"role!\", invited_by, expires_at, accepted_at, revoked_at, created_at \
             FROM invitations WHERE ($1::uuid IS NULL OR id < $1) ORDER BY id DESC LIMIT $2",
        cursor, limit + 1)
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
    ) -> Result<InvitationRecord, IdentityError> {
        let mut transaction = self.pool().begin().await?;
        if !claim_idempotency(
            &mut transaction,
            actor,
            "invitation.revoke",
            idempotency_key,
        )
        .await?
        {
            return Err(IdentityError::IdempotencyConflict);
        }
        let row = sqlx::query_as!(
            InvitationRow,
            "UPDATE invitations SET revoked_at = now(), revoked_by = $2 \
             WHERE id = $1 AND accepted_at IS NULL AND revoked_at IS NULL \
             RETURNING id, email, role::text AS \"role!\", invited_by, expires_at, accepted_at, revoked_at, created_at",
        id, actor)
        .fetch_optional(&mut *transaction)
        .await?
        .ok_or(IdentityError::InvitationUnavailable)?;
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
    ) -> Result<AcceptedInvitation, IdentityError> {
        if acceptance.token.len() != 43
            || acceptance.display_name.trim().is_empty()
            || acceptance.display_name.chars().count() > 100
        {
            return Err(IdentityError::InvitationUnavailable);
        }
        let session_expires_at = Utc::now()
            .checked_add_signed(session_ttl)
            .filter(|expires_at| *expires_at > Utc::now())
            .ok_or_else(|| IdentityError::Invalid("session lifetime is invalid".to_owned()))?;
        let digest = InvitationMaterial::digest_token(&acceptance.token);
        let mut transaction = self.pool().begin().await?;
        let invitation_email: String = sqlx::query_scalar!(
            "SELECT email FROM invitations WHERE token_digest = $1",
            digest.to_vec()
        )
        .fetch_optional(&mut *transaction)
        .await?
        .ok_or(IdentityError::InvitationUnavailable)?;
        lock_identity_email(&mut transaction, &invitation_email).await?;
        let invitation = sqlx::query!(
            "SELECT id, email, role::text AS \"role!\", expires_at, accepted_at, revoked_at \
             FROM invitations WHERE token_digest = $1 FOR UPDATE",
            digest.to_vec()
        )
        .fetch_optional(&mut *transaction)
        .await?
        .ok_or(IdentityError::InvitationUnavailable)?;
        let expires_at: DateTime<Utc> = invitation.expires_at;
        if invitation.accepted_at.is_some()
            || invitation.revoked_at.is_some()
            || expires_at <= Utc::now()
        {
            return Err(IdentityError::InvitationUnavailable);
        }
        let invitation_id: Uuid = invitation.id;
        let email: String = invitation.email;
        let role = parse_role(invitation.role)?;
        let user_id = Uuid::now_v7();
        let etag = Uuid::now_v7();
        let now = Utc::now();
        let user_row = match sqlx::query_as!(
            UserRow,
            "INSERT INTO users \
             (id, email, display_name, password_hash, role, active, etag, created_at, updated_at) \
             VALUES ($1, $2, $3, $4, CAST($5::text AS user_role), true, $6, $7, $7) \
             RETURNING id, email, display_name, role::text AS \"role!\", active, etag, created_at, updated_at",
        user_id, &email, acceptance.display_name.trim(), &acceptance.password_hash, role.as_str(), etag, now)
        .fetch_one(&mut *transaction)
        .await
        {
            Ok(row) => row,
            Err(error) if is_constraint(&error, "users_email_unique") => {
                return Err(IdentityError::EmailAlreadyMember);
            }
            Err(error) => return Err(error.into()),
        };
        let updated = sqlx::query!(
            "UPDATE invitations SET accepted_at = $2, accepted_by = $3 \
             WHERE id = $1 AND accepted_at IS NULL AND revoked_at IS NULL",
            invitation_id,
            now,
            user_id
        )
        .execute(&mut *transaction)
        .await?;
        if updated.rows_affected() != 1 {
            return Err(IdentityError::InvitationUnavailable);
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
        let session_id = insert_versioned_session(
            &mut transaction,
            user_id,
            1,
            session,
            session_expires_at,
            now,
        )
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
            user: super::accounts::user_from_row(user_row)?,
            invitation_id,
            session_id,
        })
    }
}

pub(super) fn normalize_email(email: &str) -> Result<String, IdentityError> {
    let email = email.trim().to_lowercase();
    if email.len() > 254 || !email.contains('@') || email.starts_with('@') || email.ends_with('@') {
        return Err(IdentityError::Invalid("email is invalid".to_owned()));
    }
    Ok(email)
}

#[derive(Debug, FromRow)]
struct InvitationRow {
    id: Uuid,
    email: String,
    role: String,
    invited_by: Uuid,
    expires_at: DateTime<Utc>,
    accepted_at: Option<DateTime<Utc>>,
    revoked_at: Option<DateTime<Utc>>,
    created_at: DateTime<Utc>,
}

fn invitation_from_row(row: InvitationRow) -> Result<InvitationRecord, IdentityError> {
    Ok(InvitationRecord {
        id: row.id,
        email: row.email,
        role: parse_role(row.role)?,
        invited_by: row.invited_by,
        expires_at: row.expires_at,
        accepted_at: row.accepted_at,
        revoked_at: row.revoked_at,
        created_at: row.created_at,
    })
}

async fn lock_identity_email(
    transaction: &mut sqlx::Transaction<'_, sqlx::Postgres>,
    email: &str,
) -> Result<(), sqlx::Error> {
    sqlx::query!(
        "SELECT pg_advisory_xact_lock(hashtextextended($1, $2))",
        email,
        IDENTITY_EMAIL_LOCK_SEED
    )
    .fetch_one(&mut **transaction)
    .await?;
    Ok(())
}

fn is_constraint(error: &sqlx::Error, constraint: &str) -> bool {
    matches!(error, sqlx::Error::Database(database)
        if database.constraint() == Some(constraint))
}
