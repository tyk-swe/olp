use chrono::{DateTime, Duration, Utc};
use olp_engine::domain::auth::Permission;
use sqlx::FromRow;
use uuid::Uuid;

use crate::{
    audit_events::{AuditEvent, record_audit_event},
    authentication::insert_versioned_session,
    error::Error as PersistenceError,
    idempotency::{
        Outcome, Replayable, ReplayableIdempotencyClaim, Response, claim_idempotency,
        claim_replayable_idempotency, complete_idempotency, complete_replayable_idempotency,
    },
    security::session_material::{InvitationMaterial, SessionMaterial},
    split_page,
    store::Store,
};

use super::{
    AcceptInvitation, AcceptedInvitation, Error, InvitationCreated, InvitationRecord,
    NewInvitation, accounts::UserRow, parse_role,
};

const MAX_PAGE_SIZE: i64 = 100;
const IDENTITY_EMAIL_LOCK_SEED: i64 = 0x4f4c_505f_4944;

impl Store {
    pub async fn create_invitation<F>(
        &self,
        invitation: NewInvitation,
        replay: Replayable<'_>,
        build_response: F,
    ) -> Result<Outcome<InvitationCreated>, Error>
    where
        F: FnOnce(&InvitationCreated) -> Result<Response, PersistenceError>,
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
                return Ok(Outcome::Replayed(response));
            }
            ReplayableIdempotencyClaim::Conflict => {
                transaction.rollback().await?;
                return Err(Error::IdempotencyConflict);
            }
            ReplayableIdempotencyClaim::InProgress => {
                transaction.rollback().await?;
                return Err(Error::IdempotencyInProgress);
            }
        }
        let now = Utc::now();
        if invitation.expires_at <= now || invitation.expires_at > now + Duration::days(30) {
            return Err(Error::Invalid(
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
            return Err(Error::EmailAlreadyMember);
        }
        let pending_exists: bool = sqlx::query_scalar!(
            "SELECT EXISTS (SELECT 1 FROM invitations WHERE email = $1 \
             AND accepted_at IS NULL AND revoked_at IS NULL AND expires_at > now()) AS \"value!\"",
            &email
        )
        .fetch_one(&mut *transaction)
        .await?;
        if pending_exists {
            return Err(Error::PendingInvitationExists);
        }
        // Expired pending rows no longer reserve the partial unique index.
        // Record the timeout in its own column: stamping revoked_at here would
        // rewrite a passive expiry as this operator's deliberate revocation.
        sqlx::query!(
            "UPDATE invitations SET expired_at = now() \
             WHERE email = $1 AND accepted_at IS NULL AND revoked_at IS NULL \
               AND expired_at IS NULL AND expires_at <= now()",
            &email
        )
        .execute(&mut *transaction)
        .await?;

        let id = Uuid::now_v7();
        let material = InvitationMaterial::generate();
        let row = match sqlx::query_as!(
            InvitationRow,
            "INSERT INTO invitations \
             (id, email, role, token_digest, invited_by, expires_at, created_at) \
             VALUES ($1, $2, CAST($3::text AS user_role), $4, $5, $6, $7) \
             RETURNING id, email, role::text AS \"role!\", invited_by, expires_at, accepted_at, \
                       revoked_at, expired_at, created_at, \
                       (SELECT u.email FROM users u WHERE u.id = invitations.invited_by) \
                         AS \"invited_by_email?\", \
                       (SELECT u.email FROM users u WHERE u.id = invitations.accepted_by) \
                         AS \"accepted_by_email?\", \
                       (SELECT u.email FROM users u WHERE u.id = invitations.revoked_by) \
                         AS \"revoked_by_email?\"",
            id,
            &email,
            invitation.role.as_str(),
            material.token_digest().to_vec(),
            invitation.actor,
            invitation.expires_at,
            now
        )
        .fetch_one(&mut *transaction)
        .await
        {
            Ok(row) => row,
            Err(error) if is_constraint(&error, "invitations_pending_email_idx") => {
                return Err(Error::PendingInvitationExists);
            }
            Err(error) => return Err(error.into()),
        };
        record_audit_event(
            &mut *transaction,
            AuditEvent {
                provenance: self.provenance(),
                actor: Some(invitation.actor),
                action: "invitation.create",
                resource_type: "invitation",
                resource_id: Some(&id.to_string()),
                outcome: "success",
                occurred_at: None,
            },
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
        Ok(Outcome::Executed {
            value: created,
            response,
        })
    }

    pub async fn list_invitations(
        &self,
        cursor: Option<Uuid>,
        limit: i64,
    ) -> Result<(Vec<InvitationRecord>, Option<Uuid>), Error> {
        let limit = limit.clamp(1, MAX_PAGE_SIZE);
        let rows = sqlx::query_as!(
            InvitationRow,
            "SELECT i.id, i.email, i.role::text AS \"role!\", i.invited_by, i.expires_at, \
                    i.accepted_at, i.revoked_at, i.expired_at, i.created_at, \
                    inviter.email AS \"invited_by_email?\", \
                    accepter.email AS \"accepted_by_email?\", \
                    revoker.email AS \"revoked_by_email?\" \
             FROM invitations i \
             LEFT JOIN users inviter ON inviter.id = i.invited_by \
             LEFT JOIN users accepter ON accepter.id = i.accepted_by \
             LEFT JOIN users revoker ON revoker.id = i.revoked_by \
             WHERE ($1::uuid IS NULL OR i.id < $1) ORDER BY i.id DESC LIMIT $2",
            cursor,
            limit + 1
        )
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
    ) -> Result<InvitationRecord, Error> {
        let mut transaction = self.pool().begin().await?;
        if !claim_idempotency(
            &mut transaction,
            actor,
            "invitation.revoke",
            idempotency_key,
        )
        .await?
        {
            return Err(Error::IdempotencyConflict);
        }
        let row = sqlx::query_as!(
            InvitationRow,
            "UPDATE invitations SET revoked_at = now(), revoked_by = $2 \
             WHERE id = $1 AND accepted_at IS NULL AND revoked_at IS NULL AND expired_at IS NULL \
             RETURNING id, email, role::text AS \"role!\", invited_by, expires_at, accepted_at, \
                       revoked_at, expired_at, created_at, \
                       (SELECT u.email FROM users u WHERE u.id = invitations.invited_by) \
                         AS \"invited_by_email?\", \
                       (SELECT u.email FROM users u WHERE u.id = invitations.accepted_by) \
                         AS \"accepted_by_email?\", \
                       (SELECT u.email FROM users u WHERE u.id = invitations.revoked_by) \
                         AS \"revoked_by_email?\"",
            id,
            actor
        )
        .fetch_optional(&mut *transaction)
        .await?
        .ok_or(Error::InvitationUnavailable)?;
        record_audit_event(
            &mut *transaction,
            AuditEvent {
                provenance: self.provenance(),
                actor: Some(actor),
                action: "invitation.revoke",
                resource_type: "invitation",
                resource_id: Some(&id.to_string()),
                outcome: "success",
                occurred_at: None,
            },
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
    ) -> Result<AcceptedInvitation, Error> {
        if acceptance.token.len() != 43
            || acceptance.display_name.trim().is_empty()
            || acceptance.display_name.chars().count() > 100
        {
            return Err(Error::InvitationUnavailable);
        }
        let session_expires_at = Utc::now()
            .checked_add_signed(session_ttl)
            .filter(|expires_at| *expires_at > Utc::now())
            .ok_or_else(|| Error::Invalid("session lifetime is invalid".to_owned()))?;
        let digest = InvitationMaterial::digest_token(&acceptance.token);
        let mut transaction = self.pool().begin().await?;
        let invitation_email: String = sqlx::query_scalar!(
            "SELECT email FROM invitations WHERE token_digest = $1",
            digest.to_vec()
        )
        .fetch_optional(&mut *transaction)
        .await?
        .ok_or(Error::InvitationUnavailable)?;
        lock_identity_email(&mut transaction, &invitation_email).await?;
        let invitation = sqlx::query!(
            "SELECT id, email, role::text AS \"role!\", invited_by, expires_at, \
                    accepted_at, revoked_at, expired_at \
             FROM invitations WHERE token_digest = $1 FOR UPDATE",
            digest.to_vec()
        )
        .fetch_optional(&mut *transaction)
        .await?
        .ok_or(Error::InvitationUnavailable)?;
        let expires_at: DateTime<Utc> = invitation.expires_at;
        if invitation.accepted_at.is_some()
            || invitation.revoked_at.is_some()
            || invitation.expired_at.is_some()
            || expires_at <= Utc::now()
        {
            return Err(Error::InvitationUnavailable);
        }
        // An invitation is an out-of-band grant that outlives the session it
        // was minted from. Sessions are revoked when a user is demoted or
        // deactivated; this token must not survive that either, or a former
        // owner's pending invitation still redeems into a live owner.
        let inviter = sqlx::query!(
            "SELECT role::text AS \"role!\", active FROM users WHERE id = $1",
            invitation.invited_by
        )
        .fetch_optional(&mut *transaction)
        .await?
        .ok_or(Error::InvitationUnavailable)?;
        if !inviter.active || !parse_role(inviter.role)?.allows(Permission::ManageAccess) {
            return Err(Error::InvitationUnavailable);
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
                return Err(Error::EmailAlreadyMember);
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
            return Err(Error::InvitationUnavailable);
        }
        record_audit_event(
            &mut *transaction,
            AuditEvent {
                provenance: self.provenance(),
                actor: Some(user_id),
                action: "invitation.accept",
                resource_type: "invitation",
                resource_id: Some(&invitation_id.to_string()),
                outcome: "success",
                occurred_at: None,
            },
        )
        .await?;
        record_audit_event(
            &mut *transaction,
            AuditEvent {
                provenance: self.provenance(),
                actor: Some(user_id),
                action: "user.create",
                resource_type: "user",
                resource_id: Some(&user_id.to_string()),
                outcome: "success",
                occurred_at: None,
            },
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
        record_audit_event(
            &mut *transaction,
            AuditEvent {
                provenance: self.provenance(),
                actor: Some(user_id),
                action: "session.create",
                resource_type: "session",
                resource_id: Some(&session_id.to_string()),
                outcome: "success",
                occurred_at: None,
            },
        )
        .await?;
        transaction.commit().await?;
        Ok(AcceptedInvitation {
            user: super::accounts::user_from_row(user_row)?,
        })
    }
}

/// Retires the invitations a user minted once they can no longer grant access.
/// Sessions are revoked on demotion or deactivation; a pending invitation is an
/// out-of-band grant that would otherwise outlive that revocation.
pub(crate) async fn retire_invitations_on_access_loss(
    transaction: &mut sqlx::Transaction<'_, sqlx::Postgres>,
    user_id: Uuid,
    role: olp_engine::domain::auth::Role,
    active: bool,
    actor: Uuid,
) -> Result<u64, sqlx::Error> {
    if active && role.allows(Permission::ManageAccess) {
        return Ok(0);
    }
    let revoked = sqlx::query!(
        "UPDATE invitations SET revoked_at = now(), revoked_by = $2 \
         WHERE invited_by = $1 AND accepted_at IS NULL AND revoked_at IS NULL \
           AND expired_at IS NULL",
        user_id,
        actor
    )
    .execute(&mut **transaction)
    .await?
    .rows_affected();
    Ok(revoked)
}

pub(super) fn normalize_email(email: &str) -> Result<String, Error> {
    let email = email.trim().to_lowercase();
    if email.len() > 254 || !email.contains('@') || email.starts_with('@') || email.ends_with('@') {
        return Err(Error::Invalid("email is invalid".to_owned()));
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
    expired_at: Option<DateTime<Utc>>,
    created_at: DateTime<Utc>,
    invited_by_email: Option<String>,
    accepted_by_email: Option<String>,
    revoked_by_email: Option<String>,
}

fn invitation_from_row(row: InvitationRow) -> Result<InvitationRecord, Error> {
    Ok(InvitationRecord {
        id: row.id,
        email: row.email,
        role: parse_role(row.role)?,
        invited_by: row.invited_by,
        expires_at: row.expires_at,
        accepted_at: row.accepted_at,
        revoked_at: row.revoked_at,
        expired_at: row.expired_at,
        created_at: row.created_at,
        invited_by_email: row.invited_by_email,
        accepted_by_email: row.accepted_by_email,
        revoked_by_email: row.revoked_by_email,
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
