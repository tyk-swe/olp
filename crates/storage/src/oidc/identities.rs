use chrono::Utc;
use uuid::Uuid;

use super::helpers::{
    AuthenticatedUserRow, authenticated_user_from_row, checked_session_expiry, insert_audit,
    insert_session, lock_email, lock_subject, normalize_display_name, normalize_email,
    require_current_enabled_configuration, validate_subject,
};
use super::{
    CompleteOidcLink, CompleteOidcLogin, CompleteOidcReauthentication, OidcAuthenticatedUser,
    OidcError, OidcIdentityRecord, UnlinkOidcIdentity,
};
use crate::{
    PgStore, RecentAuthPurpose, SessionSecurityContext,
    authentication::{
        consume_recent_authentication, install_recent_authentication, revoke_user_sessions,
    },
};

impl PgStore {
    pub async fn complete_oidc_login(
        &self,
        input: CompleteOidcLogin<'_>,
    ) -> Result<OidcAuthenticatedUser, OidcError> {
        validate_subject(input.subject)?;
        let now = Utc::now();
        let expires_at = checked_session_expiry(now, input.session_ttl)?;
        let mut transaction = self.pool().begin().await?;
        require_current_enabled_configuration(
            &mut transaction,
            input.configuration_id,
            input.configuration_etag,
        )
        .await?;
        lock_subject(&mut transaction, input.issuer, input.subject).await?;
        let linked = sqlx::query_as!(
            LinkedLoginRow,
            "SELECT u.id, u.email, u.display_name, u.role::text AS \"role!\", u.active, \
                    u.security_version, u.password_hash IS NULL AS \"oidc_only!\" \
             FROM oidc_identities oi JOIN users u ON u.id = oi.user_id \
             WHERE oi.issuer = $1 AND oi.subject = $2 FOR UPDATE OF u",
            input.issuer,
            input.subject
        )
        .fetch_optional(&mut *transaction)
        .await?;

        let (user, security_version) = if let Some(row) = linked {
            if !row.active {
                return Err(OidcError::InactiveUser);
            }
            let mut user = authenticated_user_from_row(row.authenticated())?;
            let mut security_version: i64 = row.security_version;
            if row.oidc_only {
                let mapped_role = input
                    .provisioning_role
                    .ok_or(OidcError::ProvisioningDenied)?;
                if user.role != mapped_role {
                    security_version = sqlx::query_scalar!(
                        "UPDATE users SET role = CAST($2::text AS user_role), \
                             security_version = security_version + 1, etag = $3, updated_at = $4 \
                         WHERE id = $1 RETURNING security_version",
                        user.id,
                        mapped_role.as_str(),
                        Uuid::now_v7(),
                        now
                    )
                    .fetch_one(&mut *transaction)
                    .await?;
                    let _revoked = revoke_user_sessions(&mut transaction, user.id).await?;
                    insert_audit(
                        &mut transaction,
                        Some(user.id),
                        "user.role_sync_oidc",
                        "user",
                        &user.id.to_string(),
                        now,
                    )
                    .await?;
                    insert_audit(
                        &mut transaction,
                        Some(user.id),
                        "session.revoke_for_oidc_role_change",
                        "user",
                        &user.id.to_string(),
                        now,
                    )
                    .await?;
                    user.role = mapped_role;
                }
            }
            (user, security_version)
        } else {
            let role = input
                .provisioning_role
                .ok_or(OidcError::ProvisioningDenied)?;
            let email = normalize_email(input.email.ok_or(OidcError::ProvisioningDenied)?)?;
            lock_email(&mut transaction, &email).await?;
            let collision: bool = sqlx::query_scalar!(
                "SELECT EXISTS (SELECT 1 FROM users WHERE email = $1) AS \"value!\"",
                &email
            )
            .fetch_one(&mut *transaction)
            .await?;
            if collision {
                return Err(OidcError::LinkRequired);
            }
            let user_id = Uuid::now_v7();
            let display_name = normalize_display_name(input.display_name, &email);
            sqlx::query!(
                "INSERT INTO users \
                 (id, email, display_name, password_hash, role, active, etag, created_at, updated_at) \
                 VALUES ($1, $2, $3, NULL, CAST($4::text AS user_role), true, $5, $6, $6)",
            user_id, &email, &display_name, role.as_str(), Uuid::now_v7(), now)
            .execute(&mut *transaction)
            .await?;
            sqlx::query!(
                "INSERT INTO oidc_identities \
                 (issuer, subject, user_id, email_at_link, last_login_at, created_at) \
                 VALUES ($1, $2, $3, $4, $5, $5)",
                input.issuer,
                input.subject,
                user_id,
                &email,
                now
            )
            .execute(&mut *transaction)
            .await?;
            insert_audit(
                &mut transaction,
                Some(user_id),
                "user.create_oidc",
                "user",
                &user_id.to_string(),
                now,
            )
            .await?;
            (
                OidcAuthenticatedUser {
                    id: user_id,
                    email,
                    display_name,
                    role,
                },
                1,
            )
        };

        sqlx::query!(
            "UPDATE oidc_identities SET last_login_at = $3 \
             WHERE issuer = $1 AND subject = $2",
            input.issuer,
            input.subject,
            now
        )
        .execute(&mut *transaction)
        .await?;
        let session_id = insert_session(
            &mut transaction,
            user.id,
            security_version,
            input.session,
            expires_at,
            now,
        )
        .await?;
        insert_audit(
            &mut transaction,
            Some(user.id),
            "session.create",
            "session",
            &session_id.to_string(),
            now,
        )
        .await?;
        insert_audit(
            &mut transaction,
            Some(user.id),
            "oidc.login",
            "session",
            &session_id.to_string(),
            now,
        )
        .await?;
        transaction.commit().await?;
        Ok(user)
    }

    pub async fn complete_oidc_link(
        &self,
        input: CompleteOidcLink<'_>,
    ) -> Result<OidcAuthenticatedUser, OidcError> {
        validate_subject(input.subject)?;
        let now = Utc::now();
        let expires_at = checked_session_expiry(now, input.session_ttl)?;
        let mut transaction = self.pool().begin().await?;
        require_current_enabled_configuration(
            &mut transaction,
            input.configuration_id,
            input.configuration_etag,
        )
        .await?;
        // Keep the subject-before-user order used by login completion, then
        // lock the user before its initiating session. User access changes
        // lock the user before deleting its sessions, so this avoids a
        // session/user deadlock while retaining the session-revocation fence.
        lock_subject(&mut transaction, input.issuer, input.subject).await?;
        let user_row = sqlx::query_as!(
            LinkedUserRow,
            "SELECT id, email, display_name, role::text AS \"role!\", active, security_version \
             FROM users WHERE id = $1 FOR UPDATE",
            input.user_id
        )
        .fetch_optional(&mut *transaction)
        .await?
        .ok_or(OidcError::InactiveUser)?;
        // Hold a lock on the exact initiating session until the identity link
        // commits. This closes the gap between HTTP authentication and the
        // storage transaction: a concurrently revoked or expired session
        // cannot authorize the link, and revocation cannot complete midway.
        let initiating_session = sqlx::query!(
            "SELECT id FROM sessions \
             WHERE id = $1 AND user_id = $2 AND expires_at > $3 FOR KEY SHARE",
            input.session_id,
            input.user_id,
            now
        )
        .fetch_optional(&mut *transaction)
        .await?;
        if initiating_session.is_none() {
            return Err(OidcError::FlowUnavailable);
        }
        if !user_row.active {
            return Err(OidcError::InactiveUser);
        }
        if user_row.security_version != input.security_version {
            return Err(OidcError::SessionUnavailable);
        }
        let current_session: bool = sqlx::query_scalar!(
            "SELECT EXISTS ( \
                 SELECT 1 FROM sessions \
                 WHERE id = $1 AND user_id = $2 AND security_version = $3 \
                   AND expires_at > now() \
             ) AS \"value!\"",
            input.session_id,
            input.user_id,
            input.security_version
        )
        .fetch_one(&mut *transaction)
        .await?;
        if !current_session {
            return Err(OidcError::SessionUnavailable);
        }
        let existing_subject = sqlx::query!(
            "SELECT user_id FROM oidc_identities WHERE issuer = $1 AND subject = $2",
            input.issuer,
            input.subject
        )
        .fetch_optional(&mut *transaction)
        .await?;
        if let Some(existing) = existing_subject
            && existing.user_id != input.user_id
        {
            return Err(OidcError::IdentityAlreadyLinked);
        }
        let other_identity: bool = sqlx::query_scalar!(
            "SELECT EXISTS (SELECT 1 FROM oidc_identities \
             WHERE issuer = $1 AND user_id = $2 AND subject <> $3) AS \"value!\"",
            input.issuer,
            input.user_id,
            input.subject
        )
        .fetch_one(&mut *transaction)
        .await?;
        if other_identity {
            return Err(OidcError::IdentityAlreadyLinked);
        }
        let normalized_claim_email = input.email.and_then(|value| normalize_email(value).ok());
        sqlx::query!(
            "INSERT INTO oidc_identities \
             (issuer, subject, user_id, email_at_link, last_login_at, created_at) \
             VALUES ($1, $2, $3, $4, $5, $5) \
             ON CONFLICT (issuer, subject) DO UPDATE SET last_login_at = EXCLUDED.last_login_at",
            input.issuer,
            input.subject,
            input.user_id,
            normalized_claim_email,
            now
        )
        .execute(&mut *transaction)
        .await?;
        let security_version: i64 = sqlx::query_scalar!(
            "UPDATE users SET security_version = security_version + 1, \
                 etag = $2, updated_at = $3 WHERE id = $1 RETURNING security_version",
            input.user_id,
            Uuid::now_v7(),
            now
        )
        .fetch_one(&mut *transaction)
        .await?;
        let _revoked = revoke_user_sessions(&mut transaction, input.user_id).await?;
        let session_id = insert_session(
            &mut transaction,
            input.user_id,
            security_version,
            input.replacement_session,
            expires_at,
            now,
        )
        .await?;
        insert_audit(
            &mut transaction,
            Some(input.user_id),
            "oidc.identity_link",
            "user",
            &input.user_id.to_string(),
            now,
        )
        .await?;
        insert_audit(
            &mut transaction,
            Some(input.user_id),
            "user.authentication_method_change",
            "user",
            &input.user_id.to_string(),
            now,
        )
        .await?;
        insert_audit(
            &mut transaction,
            Some(input.user_id),
            "session.revoke_for_oidc_link",
            "user",
            &input.user_id.to_string(),
            now,
        )
        .await?;
        insert_audit(
            &mut transaction,
            Some(input.user_id),
            "session.rotate_for_oidc_link",
            "session",
            &session_id.to_string(),
            now,
        )
        .await?;
        transaction.commit().await?;
        authenticated_user_from_row(user_row.authenticated())
    }

    /// Completes a fresh IdP authentication only when the asserted provider
    /// identity is already linked to the exact local user and initiating
    /// session. The resulting grant remains purpose-bound and one-time.
    pub async fn complete_oidc_reauthentication(
        &self,
        input: CompleteOidcReauthentication<'_>,
    ) -> Result<(), OidcError> {
        validate_subject(input.subject)?;
        if input.resource_id.is_some() != input.purpose.requires_resource()
            || input.grant_ttl <= chrono::Duration::zero()
            || input.grant_ttl > chrono::Duration::minutes(15)
        {
            return Err(OidcError::Invalid(
                "recent-authentication metadata is invalid".to_owned(),
            ));
        }
        let now = Utc::now();
        let grant_expires_at = now.checked_add_signed(input.grant_ttl).ok_or_else(|| {
            OidcError::Invalid("recent-authentication lifetime is invalid".to_owned())
        })?;
        let mut transaction = self.pool().begin().await?;
        require_current_enabled_configuration(
            &mut transaction,
            input.configuration_id,
            input.configuration_etag,
        )
        .await?;
        lock_subject(&mut transaction, input.issuer, input.subject).await?;
        let linked_and_current: bool = sqlx::query_scalar!(
            "SELECT EXISTS ( \
                 SELECT 1 FROM oidc_identities identity \
                 JOIN users ON users.id = identity.user_id \
                 JOIN sessions session ON session.user_id = users.id \
                 WHERE identity.issuer = $1 AND identity.subject = $2 \
                   AND users.id = $3 AND users.active \
                   AND users.security_version = $5 \
                   AND session.id = $4 AND session.security_version = $5 \
                   AND session.expires_at > now() \
             ) AS \"value!\"",
            input.issuer,
            input.subject,
            input.user_id,
            input.session_id,
            input.security_version
        )
        .fetch_one(&mut *transaction)
        .await?;
        if !linked_and_current {
            return Err(OidcError::ReauthenticationIdentityMismatch);
        }
        if let Some(identity_id) = input.resource_id {
            let target_belongs_to_user: bool = sqlx::query_scalar!(
                "SELECT EXISTS (SELECT 1 FROM oidc_identities WHERE id = $1 AND user_id = $2) AS \"value!\"",
                identity_id,
                input.user_id
            )
            .fetch_one(&mut *transaction)
            .await?;
            if !target_belongs_to_user {
                return Err(OidcError::IdentityNotFound);
            }
        }
        if !install_recent_authentication(
            &mut transaction,
            SessionSecurityContext {
                session_id: input.session_id,
                user_id: input.user_id,
                security_version: input.security_version,
            },
            input.purpose,
            input.resource_id,
            input.material,
            grant_expires_at,
            now,
        )
        .await?
        {
            return Err(OidcError::SessionUnavailable);
        }
        transaction.commit().await?;
        Ok(())
    }

    /// Returns every identity linked to a local user. A configuration can move
    /// to a new issuer over its lifetime, so this intentionally returns a list
    /// instead of pretending the historical relationship is singular.
    pub async fn oidc_identities_for_user(
        &self,
        user_id: Uuid,
    ) -> Result<Vec<OidcIdentityRecord>, OidcError> {
        let rows = sqlx::query!(
            "SELECT oi.id, oi.issuer, oi.email_at_link, oi.last_login_at, oi.created_at, \
                    (u.password_hash IS NOT NULL OR count(*) OVER () > 1) AS \"can_unlink!\" \
             FROM oidc_identities oi \
             JOIN users u ON u.id = oi.user_id \
             WHERE oi.user_id = $1 \
             ORDER BY oi.created_at, oi.id",
            user_id
        )
        .fetch_all(self.pool())
        .await?;
        Ok(rows
            .into_iter()
            .map(|row| OidcIdentityRecord {
                id: row.id,
                issuer: row.issuer,
                email_at_link: row.email_at_link,
                last_login_at: row.last_login_at,
                created_at: row.created_at,
                can_unlink: row.can_unlink,
            })
            .collect())
    }

    /// Removes a linked identity only when another authentication method will
    /// remain. Grant consumption, method removal, security-version advance,
    /// complete revocation, and replacement-session insertion are atomic.
    pub async fn unlink_oidc_identity(
        &self,
        input: UnlinkOidcIdentity<'_>,
    ) -> Result<Uuid, OidcError> {
        let now = Utc::now();
        let expires_at = checked_session_expiry(now, input.session_ttl)?;
        let mut transaction = self.pool().begin().await?;
        let user = sqlx::query!(
            "SELECT password_hash IS NOT NULL AS \"has_local_password!\", active, security_version \
             FROM users WHERE id = $1 FOR UPDATE",
            input.user_id
        )
        .fetch_optional(&mut *transaction)
        .await?
        .ok_or(OidcError::InactiveUser)?;
        if !user.active {
            return Err(OidcError::InactiveUser);
        }
        if user.security_version != input.security_version {
            return Err(OidcError::SessionUnavailable);
        }
        if !consume_recent_authentication(
            &mut transaction,
            input.session_id,
            input.user_id,
            input.security_version,
            RecentAuthPurpose::OidcUnlink,
            Some(input.identity_id),
            input.recent_auth_token_digest,
        )
        .await?
        {
            return Err(OidcError::RecentAuthenticationRequired);
        }
        let identities = sqlx::query!(
            "SELECT id FROM oidc_identities WHERE user_id = $1 FOR UPDATE",
            input.user_id
        )
        .fetch_all(&mut *transaction)
        .await?;
        if !identities
            .iter()
            .any(|identity| identity.id == input.identity_id)
        {
            return Err(OidcError::IdentityNotFound);
        }
        if !user.has_local_password && identities.len() <= 1 {
            return Err(OidcError::LastAuthenticationMethod);
        }
        let deleted = sqlx::query!(
            "DELETE FROM oidc_identities WHERE id = $1 AND user_id = $2",
            input.identity_id,
            input.user_id
        )
        .execute(&mut *transaction)
        .await?;
        if deleted.rows_affected() != 1 {
            return Err(OidcError::IdentityNotFound);
        }
        let security_version: i64 = sqlx::query_scalar!(
            "UPDATE users SET security_version = security_version + 1, \
                 etag = $2, updated_at = $3 WHERE id = $1 RETURNING security_version",
            input.user_id,
            Uuid::now_v7(),
            now
        )
        .fetch_one(&mut *transaction)
        .await?;
        let _revoked = revoke_user_sessions(&mut transaction, input.user_id).await?;
        let session_id = insert_session(
            &mut transaction,
            input.user_id,
            security_version,
            input.replacement_session,
            expires_at,
            now,
        )
        .await?;
        insert_audit(
            &mut transaction,
            Some(input.user_id),
            "oidc.identity_unlink",
            "oidc_identity",
            &input.identity_id.to_string(),
            now,
        )
        .await?;
        insert_audit(
            &mut transaction,
            Some(input.user_id),
            "user.authentication_method_change",
            "user",
            &input.user_id.to_string(),
            now,
        )
        .await?;
        insert_audit(
            &mut transaction,
            Some(input.user_id),
            "session.revoke_for_oidc_unlink",
            "user",
            &input.user_id.to_string(),
            now,
        )
        .await?;
        insert_audit(
            &mut transaction,
            Some(input.user_id),
            "session.rotate_for_oidc_unlink",
            "session",
            &session_id.to_string(),
            now,
        )
        .await?;
        transaction.commit().await?;
        Ok(session_id)
    }
}

#[derive(Debug, sqlx::FromRow)]
struct LinkedLoginRow {
    id: Uuid,
    email: String,
    display_name: String,
    role: String,
    active: bool,
    security_version: i64,
    oidc_only: bool,
}

impl LinkedLoginRow {
    fn authenticated(&self) -> AuthenticatedUserRow {
        AuthenticatedUserRow {
            id: self.id,
            email: self.email.clone(),
            display_name: self.display_name.clone(),
            role: self.role.clone(),
        }
    }
}

#[derive(Debug, sqlx::FromRow)]
struct LinkedUserRow {
    id: Uuid,
    email: String,
    display_name: String,
    role: String,
    active: bool,
    security_version: i64,
}

impl LinkedUserRow {
    fn authenticated(self) -> AuthenticatedUserRow {
        AuthenticatedUserRow {
            id: self.id,
            email: self.email,
            display_name: self.display_name,
            role: self.role,
        }
    }
}
