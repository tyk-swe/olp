use crate::access::policy::Role;
use chrono::DateTime;
use chrono::Utc;
use sqlx::FromRow;
use uuid::Uuid;

use crate::access::audit_events::record_success;
use crate::access::authentication::RecentAuthPurpose;
use crate::access::authentication::SessionSecurityContext;
use crate::access::authentication::consume_recent_authentication;
use crate::access::authentication::insert_versioned_session;
use crate::access::authentication::revoke_user_sessions;
use crate::crypto::session_material::SessionMaterial;
use crate::database::query::split_page;
use crate::database::reads::MAX_PAGE_SIZE;

use crate::access::identity::Error;
use crate::access::identity::PasswordSessionRotation;
use crate::access::identity::SessionRecord;
use crate::access::identity::UserRecord;
use crate::access::identity::invitations::retire_invitations_on_access_loss;
use crate::access::identity::locks::lock_user;
use crate::access::identity::parse_role;

pub async fn list_users(
    pool: &sqlx::PgPool,
    cursor: Option<Uuid>,
    limit: i64,
) -> Result<(Vec<UserRecord>, Option<Uuid>), Error> {
    let limit = limit.clamp(1, i64::from(MAX_PAGE_SIZE));
    let rows = sqlx::query_as::<_, UserRow>(
        "SELECT id, email, display_name, role::text AS \"role\", active, etag, created_at, updated_at \
             FROM users WHERE ($1::uuid IS NULL OR id < $1) ORDER BY id DESC LIMIT $2",
    )
    .bind(cursor)
    .bind(limit + 1)
        .fetch_all(pool)
        .await?;
    let users = rows
        .into_iter()
        .map(user_from_row)
        .collect::<Result<Vec<_>, _>>()?;
    let (users, next_cursor) = split_page(users, limit as usize, |user| user.id);
    Ok((users, next_cursor))
}

pub async fn user(pool: &sqlx::PgPool, id: Uuid) -> Result<Option<UserRecord>, Error> {
    let row = sqlx::query_as::<_, UserRow>(
        "SELECT id, email, display_name, role::text AS \"role\", active, etag, created_at, updated_at \
             FROM users WHERE id = $1",
    )
    .bind(id)
        .fetch_optional(pool)
        .await?;
    row.map(user_from_row).transpose()
}

pub async fn update_user_role(
    pool: &sqlx::PgPool,
    provenance: &crate::database::RequestProvenance,
    id: Uuid,
    role: Role,
    expected_etag: Uuid,
    actor: Uuid,
) -> Result<UserRecord, Error> {
    let mut transaction = pool.begin().await?;
    let current = lock_user(&mut transaction, id)
        .await?
        .ok_or(Error::NotFound)?;
    if current.etag != expected_etag {
        return Err(Error::PreconditionFailed);
    }

    let etag = Uuid::now_v7();
    let row = match sqlx::query_as::<_, UserRow>(
        "UPDATE users SET role = CAST($2::text AS user_role), security_version = security_version + 1, \
                 etag = $3, updated_at = now() \
             WHERE id = $1 \
             RETURNING id, email, display_name, role::text AS \"role\", active, etag, created_at, updated_at",
    )
    .bind(id)
    .bind(role.as_str())
    .bind(etag)
        .fetch_one(&mut *transaction)
        .await
        {
            Ok(row) => row,
            Err(error) if is_last_owner_violation(&error) => return Err(Error::LastOwner),
            Err(error) => return Err(error.into()),
        };

    let revoked = sqlx::query("DELETE FROM sessions WHERE user_id = $1")
        .bind(id)
        .execute(&mut *transaction)
        .await?
        .rows_affected();
    let retired =
        retire_invitations_on_access_loss(&mut transaction, id, role, row.active, Some(actor))
            .await?;
    record_success(
        &mut *transaction,
        provenance,
        actor,
        "user.role_update",
        "user",
        id,
    )
    .await?;
    if retired > 0 {
        record_success(
            &mut *transaction,
            provenance,
            actor,
            "invitation.revoke_for_role_change",
            "user",
            id,
        )
        .await?;
    }
    if revoked > 0 {
        record_success(
            &mut *transaction,
            provenance,
            actor,
            "session.revoke_for_role_change",
            "user",
            id,
        )
        .await?;
    }
    transaction.commit().await?;
    user_from_row(row)
}

pub async fn update_user_access(
    pool: &sqlx::PgPool,
    provenance: &crate::database::RequestProvenance,
    id: Uuid,
    role: Option<Role>,
    active: Option<bool>,
    expected_etag: Uuid,
    actor: Uuid,
) -> Result<UserRecord, Error> {
    if role.is_none() && active.is_none() {
        return Err(Error::Invalid(
            "role or active status is required".to_owned(),
        ));
    }
    let mut transaction = pool.begin().await?;
    let current = lock_user(&mut transaction, id)
        .await?
        .ok_or(Error::NotFound)?;
    if current.etag != expected_etag {
        return Err(Error::PreconditionFailed);
    }
    let etag = Uuid::now_v7();
    let row = match sqlx::query_as::<_, UserRow>(
        "UPDATE users SET \
                 role = COALESCE(CAST($2::text AS user_role), role), \
                 active = COALESCE($3, active), security_version = security_version + 1, \
                 etag = $4, updated_at = now() \
             WHERE id = $1 \
             RETURNING id, email, display_name, role::text AS \"role\", active, etag, created_at, updated_at",
    )
    .bind(id)
    .bind(role.map(|role| role.as_str()))
    .bind(active)
    .bind(etag)
        .fetch_one(&mut *transaction)
        .await
        {
            Ok(row) => row,
            Err(error) if is_last_owner_violation(&error) => return Err(Error::LastOwner),
            Err(error) => return Err(error.into()),
        };
    let revoked = sqlx::query("DELETE FROM sessions WHERE user_id = $1")
        .bind(id)
        .execute(&mut *transaction)
        .await?
        .rows_affected();
    let retired = retire_invitations_on_access_loss(
        &mut transaction,
        id,
        parse_role(row.role.clone())?,
        row.active,
        Some(actor),
    )
    .await?;
    record_success(
        &mut *transaction,
        provenance,
        actor,
        "user.access_update",
        "user",
        id,
    )
    .await?;
    if retired > 0 {
        record_success(
            &mut *transaction,
            provenance,
            actor,
            "invitation.revoke_for_access_change",
            "user",
            id,
        )
        .await?;
    }
    if revoked > 0 {
        record_success(
            &mut *transaction,
            provenance,
            actor,
            "session.revoke_for_access_change",
            "user",
            id,
        )
        .await?;
    }
    transaction.commit().await?;
    user_from_row(row)
}

pub async fn update_profile(
    pool: &sqlx::PgPool,
    provenance: &crate::database::RequestProvenance,
    id: Uuid,
    display_name: &str,
    expected_etag: Uuid,
) -> Result<UserRecord, Error> {
    let display_name = display_name.trim();
    if display_name.is_empty() || display_name.chars().count() > 100 {
        return Err(Error::Invalid(
            "display name must contain 1-100 characters".to_owned(),
        ));
    }
    let mut transaction = pool.begin().await?;
    let row = sqlx::query_as::<_, UserRow>(
        "UPDATE users SET display_name = $2, etag = $3, updated_at = now()
             WHERE id = $1 AND etag = $4
             RETURNING id, email, display_name, role::text AS \"role\", active, etag,
                       created_at, updated_at",
    )
    .bind(id)
    .bind(display_name)
    .bind(Uuid::now_v7())
    .bind(expected_etag)
    .fetch_optional(&mut *transaction)
    .await?;
    let Some(row) = row else {
        let exists: bool = sqlx::query_scalar::<_, bool>(
            "SELECT EXISTS (SELECT 1 FROM users WHERE id = $1) AS \"value\"",
        )
        .bind(id)
        .fetch_one(&mut *transaction)
        .await?;
        return Err(if exists {
            Error::PreconditionFailed
        } else {
            Error::NotFound
        });
    };
    record_success(
        &mut *transaction,
        provenance,
        id,
        "user.profile_update",
        "user",
        id,
    )
    .await?;
    transaction.commit().await?;
    user_from_row(row)
}

pub async fn update_local_password(
    pool: &sqlx::PgPool,
    provenance: &crate::database::RequestProvenance,
    password_hash: &str,
    expected_etag: Uuid,
    context: SessionSecurityContext,
    replacement: &SessionMaterial,
    session_ttl: chrono::Duration,
) -> Result<PasswordSessionRotation, Error> {
    let id = context.user_id;
    let now = Utc::now();
    let expires_at = now
        .checked_add_signed(session_ttl)
        .filter(|expires_at| *expires_at > now)
        .ok_or_else(|| Error::Invalid("session lifetime is invalid".to_owned()))?;
    let mut transaction = pool.begin().await?;
    let current = lock_user(&mut transaction, id)
        .await?
        .ok_or(Error::NotFound)?;
    if !current.has_local_password {
        return Err(Error::LocalPasswordUnavailable);
    }
    if !current.active
        || current.security_version != context.security_version
        || !session_is_current(&mut transaction, context).await?
    {
        return Err(Error::SessionUnavailable);
    }
    if current.etag != expected_etag {
        return Err(Error::PreconditionFailed);
    }
    let etag = Uuid::now_v7();
    let row = sqlx::query_as::<_, PasswordUserRow>(
        "UPDATE users SET password_hash = $2, security_version = security_version + 1, \
                 etag = $3, updated_at = $4 \
             WHERE id = $1 \
             RETURNING id, email, display_name, role::text AS \"role\", active, etag, \
                       security_version, created_at, updated_at",
    )
    .bind(id)
    .bind(password_hash)
    .bind(etag)
    .bind(now)
    .fetch_one(&mut *transaction)
    .await?;
    let security_version = row.security_version;
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
    record_success(
        &mut *transaction,
        provenance,
        id,
        "user.password_update",
        "user",
        id,
    )
    .await?;
    record_success(
        &mut *transaction,
        provenance,
        id,
        "session.revoke_for_password_change",
        "user",
        id,
    )
    .await?;
    record_success(
        &mut *transaction,
        provenance,
        id,
        "session.rotate_for_password_change",
        "session",
        session_id,
    )
    .await?;
    transaction.commit().await?;
    Ok(PasswordSessionRotation {
        user: user_from_row(row.into_user())?,
        session_id,
    })
}

/// Adds the first local password to an OIDC-only account. The recent-auth
/// grant, password enrollment, security-version advance, complete session
/// revocation, and replacement session are one transaction.
#[allow(clippy::too_many_arguments)]
pub async fn enroll_local_password(
    pool: &sqlx::PgPool,
    provenance: &crate::database::RequestProvenance,
    password_hash: &str,
    expected_etag: Uuid,
    context: SessionSecurityContext,
    recent_auth_token_digest: [u8; 32],
    replacement: &SessionMaterial,
    session_ttl: chrono::Duration,
) -> Result<PasswordSessionRotation, Error> {
    let id = context.user_id;
    let now = Utc::now();
    let expires_at = now
        .checked_add_signed(session_ttl)
        .filter(|expires_at| *expires_at > now)
        .ok_or_else(|| Error::Invalid("session lifetime is invalid".to_owned()))?;
    let mut transaction = pool.begin().await?;
    admit_password_enrollment(
        &mut transaction,
        context,
        expected_etag,
        recent_auth_token_digest,
    )
    .await?;
    let etag = Uuid::now_v7();
    let row = sqlx::query_as::<_, PasswordUserRow>(
        "UPDATE users SET password_hash = $2, security_version = security_version + 1, \
                 etag = $3, updated_at = $4 \
             WHERE id = $1 \
             RETURNING id, email, display_name, role::text AS \"role\", active, etag, \
                       security_version, created_at, updated_at",
    )
    .bind(id)
    .bind(password_hash)
    .bind(etag)
    .bind(now)
    .fetch_one(&mut *transaction)
    .await?;
    let security_version = row.security_version;
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
    crate::access::identity::accounts::record_password_enrollment(
        provenance,
        &mut transaction,
        id,
        session_id,
    )
    .await?;
    transaction.commit().await?;
    Ok(PasswordSessionRotation {
        user: user_from_row(row.into_user())?,
        session_id,
    })
}

pub(crate) async fn record_password_enrollment(
    provenance: &crate::database::RequestProvenance,
    transaction: &mut sqlx::Transaction<'_, sqlx::Postgres>,
    id: Uuid,
    session_id: Uuid,
) -> Result<(), Error> {
    for (action, resource_type, resource_id) in [
        ("user.password_enroll", "user", id),
        ("user.authentication_method_change", "user", id),
        ("session.revoke_for_password_enrollment", "user", id),
        (
            "session.rotate_for_password_enrollment",
            "session",
            session_id,
        ),
    ] {
        record_success(
            &mut **transaction,
            provenance,
            id,
            action,
            resource_type,
            resource_id,
        )
        .await?;
    }
    Ok(())
}

pub async fn list_sessions(
    pool: &sqlx::PgPool,
    user_id: Uuid,
    cursor: Option<Uuid>,
    limit: i64,
) -> Result<(Vec<SessionRecord>, Option<Uuid>), Error> {
    let limit = limit.clamp(1, i64::from(MAX_PAGE_SIZE));
    let rows = sqlx::query_as::<_, ListSessionsRow>(
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
    .fetch_all(pool)
    .await?;
    let sessions = rows
        .into_iter()
        .map(|row| SessionRecord {
            id: row.id,
            user_id: row.user_id,
            expires_at: row.expires_at,
            last_seen_at: row.last_seen_at,
            created_at: row.created_at,
        })
        .collect::<Vec<_>>();
    let (sessions, next_cursor) = split_page(sessions, limit as usize, |session| session.id);
    Ok((sessions, next_cursor))
}

pub async fn revoke_session(
    pool: &sqlx::PgPool,
    provenance: &crate::database::RequestProvenance,
    session_id: Uuid,
    actor: Uuid,
    can_manage_all: bool,
) -> Result<(), Error> {
    let mut transaction = pool.begin().await?;
    let session = sqlx::query_as::<_, RevokeSessionRow>(
        "SELECT user_id FROM sessions WHERE id = $1 FOR UPDATE",
    )
    .bind(session_id)
    .fetch_optional(&mut *transaction)
    .await?
    .ok_or(Error::NotFound)?;
    let user_id: Uuid = session.user_id;
    if user_id != actor && !can_manage_all {
        return Err(Error::SessionForbidden);
    }
    sqlx::query("DELETE FROM sessions WHERE id = $1")
        .bind(session_id)
        .execute(&mut *transaction)
        .await?;
    record_success(
        &mut *transaction,
        provenance,
        actor,
        "session.revoke",
        "session",
        session_id,
    )
    .await?;
    transaction.commit().await?;
    Ok(())
}

async fn session_is_current(
    transaction: &mut sqlx::Transaction<'_, sqlx::Postgres>,
    context: SessionSecurityContext,
) -> Result<bool, sqlx::Error> {
    sqlx::query_scalar::<_, bool>(
        "SELECT EXISTS ( \
             SELECT 1 FROM sessions \
             WHERE id = $1 AND user_id = $2 AND security_version = $3 \
               AND expires_at > now() \
         ) AS \"value\"",
    )
    .bind(context.session_id)
    .bind(context.user_id)
    .bind(context.security_version)
    .fetch_one(&mut **transaction)
    .await
}

#[derive(Debug, FromRow)]
pub(crate) struct UserRow {
    pub(crate) id: Uuid,
    pub(crate) email: String,
    pub(crate) display_name: String,
    pub(crate) role: String,
    pub(crate) active: bool,
    pub(crate) etag: Uuid,
    pub(crate) created_at: DateTime<Utc>,
    pub(crate) updated_at: DateTime<Utc>,
}

/// Locks the account and checks every precondition for adding a first local
/// password, consuming the recent-authentication grant on success.
async fn admit_password_enrollment(
    transaction: &mut sqlx::Transaction<'_, sqlx::Postgres>,
    context: SessionSecurityContext,
    expected_etag: Uuid,
    recent_auth_token_digest: [u8; 32],
) -> Result<(), Error> {
    let id = context.user_id;
    let current = lock_user(transaction, id).await?.ok_or(Error::NotFound)?;
    if current.has_local_password {
        return Err(Error::LocalPasswordAlreadyConfigured);
    }
    if !current.active || current.security_version != context.security_version {
        return Err(Error::SessionUnavailable);
    }
    if current.etag != expected_etag {
        return Err(Error::PreconditionFailed);
    }
    if !consume_recent_authentication(
        transaction,
        context.session_id,
        id,
        context.security_version,
        RecentAuthPurpose::PasswordEnrollment,
        None,
        recent_auth_token_digest,
    )
    .await?
    {
        return Err(Error::RecentAuthenticationRequired);
    }
    Ok(())
}

#[derive(Debug, FromRow)]
struct PasswordUserRow {
    id: Uuid,
    email: String,
    display_name: String,
    role: String,
    active: bool,
    etag: Uuid,
    security_version: i64,
    created_at: DateTime<Utc>,
    updated_at: DateTime<Utc>,
}

impl PasswordUserRow {
    fn into_user(self) -> UserRow {
        UserRow {
            id: self.id,
            email: self.email,
            display_name: self.display_name,
            role: self.role,
            active: self.active,
            etag: self.etag,
            created_at: self.created_at,
            updated_at: self.updated_at,
        }
    }
}

pub(crate) fn user_from_row(row: UserRow) -> Result<UserRecord, Error> {
    Ok(UserRecord {
        id: row.id,
        email: row.email,
        display_name: row.display_name,
        role: parse_role(row.role)?,
        active: row.active,
        etag: row.etag,
        created_at: row.created_at,
        updated_at: row.updated_at,
    })
}

/// The `prevent_last_owner_change` trigger installed in migration 0001 is the
/// single enforcement point for the last-owner invariant; the domain-level
/// check was removed so every control-plane path, present and future, is
/// covered. This recognises the check violation the trigger raises.
fn is_last_owner_violation(error: &sqlx::Error) -> bool {
    matches!(error, sqlx::Error::Database(database)
        if database.code().as_deref() == Some("23514")
            && database.message().contains("last active owner"))
}

#[derive(sqlx::FromRow)]
struct ListSessionsRow {
    id: uuid::Uuid,
    user_id: uuid::Uuid,
    expires_at: chrono::DateTime<chrono::Utc>,
    last_seen_at: chrono::DateTime<chrono::Utc>,
    created_at: chrono::DateTime<chrono::Utc>,
}

#[derive(sqlx::FromRow)]
struct RevokeSessionRow {
    user_id: uuid::Uuid,
}
