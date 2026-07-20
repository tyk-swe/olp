use olp_domain::Role;
use sqlx::Row;
use uuid::Uuid;

use crate::{PgStore, split_page};

use super::{IdentityError, SessionRecord, UserRecord, insert_audit, parse_role};

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
        id: Uuid,
        password_hash: &str,
        expected_etag: Uuid,
        keep_session_id: Uuid,
    ) -> Result<UserRecord, IdentityError> {
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
            .ok_or(IdentityError::NotFound)?;
            if !current.get::<bool, _>("local") {
                return Err(IdentityError::LocalPasswordUnavailable);
            }
            return Err(IdentityError::PreconditionFailed);
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
    ) -> Result<UserRecord, IdentityError> {
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
            .ok_or(IdentityError::NotFound)?;
            if current.get::<bool, _>("local") {
                return Err(IdentityError::LocalPasswordAlreadyConfigured);
            }
            return Err(IdentityError::PreconditionFailed);
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

    pub async fn list_sessions(
        &self,
        user_id: Uuid,
        cursor: Option<Uuid>,
        limit: i64,
    ) -> Result<(Vec<SessionRecord>, Option<Uuid>), IdentityError> {
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
