use chrono::Utc;
use olp_domain::Role;
use sqlx::Row;
use uuid::Uuid;

use crate::{
    PgStore, RecentAuthPurpose, SessionMaterial, SessionSecurityContext,
    authentication::{
        consume_recent_authentication, insert_versioned_session, revoke_user_sessions,
    },
    split_page,
};

use super::{
    IdentityError, PasswordSessionRotation, SessionRecord, UserRecord, insert_audit, parse_role,
};

const MAX_PAGE_SIZE: i64 = 100;

impl PgStore {
    pub async fn list_users(
        &self,
        cursor: Option<Uuid>,
        limit: i64,
    ) -> Result<(Vec<UserRecord>, Option<Uuid>), IdentityError> {
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

    pub async fn user(&self, id: Uuid) -> Result<Option<UserRecord>, IdentityError> {
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
    ) -> Result<UserRecord, IdentityError> {
        let mut transaction = self.pool().begin().await?;
        let current = sqlx::query("SELECT etag FROM users WHERE id = $1 FOR UPDATE")
            .bind(id)
            .fetch_optional(&mut *transaction)
            .await?
            .ok_or(IdentityError::NotFound)?;
        if current.get::<Uuid, _>("etag") != expected_etag {
            return Err(IdentityError::PreconditionFailed);
        }

        let etag = Uuid::now_v7();
        let row = match sqlx::query(
            "UPDATE users SET role = CAST($2 AS user_role), security_version = security_version + 1, \
                 etag = $3, updated_at = now() \
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
            Err(error) if is_last_owner_violation(&error) => return Err(IdentityError::LastOwner),
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
    ) -> Result<UserRecord, IdentityError> {
        if role.is_none() && active.is_none() {
            return Err(IdentityError::Invalid(
                "role or active status is required".to_owned(),
            ));
        }
        let mut transaction = self.pool().begin().await?;
        let current = sqlx::query("SELECT etag FROM users WHERE id = $1 FOR UPDATE")
            .bind(id)
            .fetch_optional(&mut *transaction)
            .await?
            .ok_or(IdentityError::NotFound)?;
        if current.get::<Uuid, _>("etag") != expected_etag {
            return Err(IdentityError::PreconditionFailed);
        }
        let etag = Uuid::now_v7();
        let row = match sqlx::query(
            "UPDATE users SET \
                 role = COALESCE(CAST($2 AS user_role), role), \
                 active = COALESCE($3, active), security_version = security_version + 1, \
                 etag = $4, updated_at = now() \
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
            Err(error) if is_last_owner_violation(&error) => return Err(IdentityError::LastOwner),
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
    ) -> Result<UserRecord, IdentityError> {
        let display_name = display_name.trim();
        if display_name.is_empty() || display_name.chars().count() > 100 {
            return Err(IdentityError::Invalid(
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
                IdentityError::PreconditionFailed
            } else {
                IdentityError::NotFound
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
        password_hash: &str,
        expected_etag: Uuid,
        context: SessionSecurityContext,
        replacement: &SessionMaterial,
        session_ttl: chrono::Duration,
    ) -> Result<PasswordSessionRotation, IdentityError> {
        let id = context.user_id;
        let now = Utc::now();
        let expires_at = now
            .checked_add_signed(session_ttl)
            .filter(|expires_at| *expires_at > now)
            .ok_or_else(|| IdentityError::Invalid("session lifetime is invalid".to_owned()))?;
        let mut transaction = self.pool().begin().await?;
        let current = sqlx::query(
            "SELECT etag, password_hash IS NOT NULL AS local, active, security_version \
             FROM users WHERE id = $1 FOR UPDATE",
        )
        .bind(id)
        .fetch_optional(&mut *transaction)
        .await?
        .ok_or(IdentityError::NotFound)?;
        if !current.get::<bool, _>("local") {
            return Err(IdentityError::LocalPasswordUnavailable);
        }
        if !current.get::<bool, _>("active")
            || current.get::<i64, _>("security_version") != context.security_version
            || !session_is_current(&mut transaction, context).await?
        {
            return Err(IdentityError::SessionUnavailable);
        }
        if current.get::<Uuid, _>("etag") != expected_etag {
            return Err(IdentityError::PreconditionFailed);
        }
        let etag = Uuid::now_v7();
        let row = sqlx::query(
            "UPDATE users SET password_hash = $2, security_version = security_version + 1, \
                 etag = $3, updated_at = $4 \
             WHERE id = $1 \
             RETURNING id, email, display_name, role::text AS role, active, etag, \
                       security_version, created_at, updated_at",
        )
        .bind(id)
        .bind(password_hash)
        .bind(etag)
        .bind(now)
        .fetch_one(&mut *transaction)
        .await?;
        let security_version: i64 = row.get("security_version");
        let _revoked = revoke_user_sessions(&mut transaction, id).await?;
        let session_id = insert_versioned_session(
            &mut transaction,
            id,
            security_version,
            replacement,
            expires_at,
            now,
        )
        .await?;
        insert_audit(
            &mut transaction,
            id,
            "user.password_update",
            "user",
            &id.to_string(),
        )
        .await?;
        insert_audit(
            &mut transaction,
            id,
            "session.revoke_for_password_change",
            "user",
            &id.to_string(),
        )
        .await?;
        insert_audit(
            &mut transaction,
            id,
            "session.rotate_for_password_change",
            "session",
            &session_id.to_string(),
        )
        .await?;
        transaction.commit().await?;
        Ok(PasswordSessionRotation {
            user: user_from_row(row)?,
            session_id,
        })
    }

    /// Adds the first local password to an OIDC-only account. The recent-auth
    /// grant, password enrollment, security-version advance, complete session
    /// revocation, and replacement session are one transaction.
    pub async fn enroll_local_password(
        &self,
        password_hash: &str,
        expected_etag: Uuid,
        context: SessionSecurityContext,
        recent_auth_token_digest: [u8; 32],
        replacement: &SessionMaterial,
        session_ttl: chrono::Duration,
    ) -> Result<PasswordSessionRotation, IdentityError> {
        let id = context.user_id;
        let now = Utc::now();
        let expires_at = now
            .checked_add_signed(session_ttl)
            .filter(|expires_at| *expires_at > now)
            .ok_or_else(|| IdentityError::Invalid("session lifetime is invalid".to_owned()))?;
        let mut transaction = self.pool().begin().await?;
        let current = sqlx::query(
            "SELECT etag, password_hash IS NOT NULL AS local, active, security_version \
             FROM users WHERE id = $1 FOR UPDATE",
        )
        .bind(id)
        .fetch_optional(&mut *transaction)
        .await?
        .ok_or(IdentityError::NotFound)?;
        if current.get::<bool, _>("local") {
            return Err(IdentityError::LocalPasswordAlreadyConfigured);
        }
        if !current.get::<bool, _>("active")
            || current.get::<i64, _>("security_version") != context.security_version
        {
            return Err(IdentityError::SessionUnavailable);
        }
        if current.get::<Uuid, _>("etag") != expected_etag {
            return Err(IdentityError::PreconditionFailed);
        }
        if !consume_recent_authentication(
            &mut transaction,
            context.session_id,
            id,
            context.security_version,
            RecentAuthPurpose::PasswordEnrollment,
            None,
            recent_auth_token_digest,
        )
        .await?
        {
            return Err(IdentityError::RecentAuthenticationRequired);
        }
        let etag = Uuid::now_v7();
        let row = sqlx::query(
            "UPDATE users SET password_hash = $2, security_version = security_version + 1, \
                 etag = $3, updated_at = $4 \
             WHERE id = $1 \
             RETURNING id, email, display_name, role::text AS role, active, etag, \
                       security_version, created_at, updated_at",
        )
        .bind(id)
        .bind(password_hash)
        .bind(etag)
        .bind(now)
        .fetch_one(&mut *transaction)
        .await?;
        let security_version: i64 = row.get("security_version");
        let _revoked = revoke_user_sessions(&mut transaction, id).await?;
        let session_id = insert_versioned_session(
            &mut transaction,
            id,
            security_version,
            replacement,
            expires_at,
            now,
        )
        .await?;
        insert_audit(
            &mut transaction,
            id,
            "user.password_enroll",
            "user",
            &id.to_string(),
        )
        .await?;
        insert_audit(
            &mut transaction,
            id,
            "user.authentication_method_change",
            "user",
            &id.to_string(),
        )
        .await?;
        insert_audit(
            &mut transaction,
            id,
            "session.revoke_for_password_enrollment",
            "user",
            &id.to_string(),
        )
        .await?;
        insert_audit(
            &mut transaction,
            id,
            "session.rotate_for_password_enrollment",
            "session",
            &session_id.to_string(),
        )
        .await?;
        transaction.commit().await?;
        Ok(PasswordSessionRotation {
            user: user_from_row(row)?,
            session_id,
        })
    }

    pub async fn list_sessions(
        &self,
        user_id: Uuid,
        cursor: Option<Uuid>,
        limit: i64,
    ) -> Result<(Vec<SessionRecord>, Option<Uuid>), IdentityError> {
        let limit = limit.clamp(1, MAX_PAGE_SIZE);
        let rows = sqlx::query(
            "SELECT session.id, session.user_id, session.expires_at, session.last_seen_at, \
                    session.created_at \
             FROM sessions session JOIN users ON users.id = session.user_id \
             WHERE session.user_id = $1 AND session.expires_at > now() \
               AND users.active AND session.security_version = users.security_version \
               AND ($2::uuid IS NULL OR session.id < $2) \
             ORDER BY session.id DESC LIMIT $3",
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
    ) -> Result<(), IdentityError> {
        let mut transaction = self.pool().begin().await?;
        let session = sqlx::query("SELECT user_id FROM sessions WHERE id = $1 FOR UPDATE")
            .bind(session_id)
            .fetch_optional(&mut *transaction)
            .await?
            .ok_or(IdentityError::NotFound)?;
        let user_id: Uuid = session.get("user_id");
        if user_id != actor && !can_manage_all {
            return Err(IdentityError::SessionForbidden);
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

async fn session_is_current(
    transaction: &mut sqlx::Transaction<'_, sqlx::Postgres>,
    context: SessionSecurityContext,
) -> Result<bool, sqlx::Error> {
    sqlx::query_scalar(
        "SELECT EXISTS ( \
             SELECT 1 FROM sessions \
             WHERE id = $1 AND user_id = $2 AND security_version = $3 \
               AND expires_at > now() \
         )",
    )
    .bind(context.session_id)
    .bind(context.user_id)
    .bind(context.security_version)
    .fetch_one(&mut **transaction)
    .await
}

pub(super) fn user_from_row(row: sqlx::postgres::PgRow) -> Result<UserRecord, IdentityError> {
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

fn is_last_owner_violation(error: &sqlx::Error) -> bool {
    matches!(error, sqlx::Error::Database(database)
        if database.code().as_deref() == Some("23514")
            && database.message().contains("last active owner"))
}
