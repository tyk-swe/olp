use chrono::Utc;
use sqlx::Row;
use uuid::Uuid;

use super::helpers::{
    authenticated_user_from_row, checked_session_expiry, insert_audit, insert_session, lock_email,
    lock_subject, normalize_display_name, normalize_email, require_current_enabled_configuration,
    validate_subject,
};
use super::{
    CompleteOidcLink, CompleteOidcLogin, OidcAuthenticatedUser, OidcError, OidcIdentityRecord,
};
use crate::PgStore;

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
        let linked = sqlx::query(
            "SELECT u.id, u.email, u.display_name, u.role::text AS role, u.active, \
                    u.password_hash IS NULL AS oidc_only \
             FROM oidc_identities oi JOIN users u ON u.id = oi.user_id \
             WHERE oi.issuer = $1 AND oi.subject = $2 FOR UPDATE OF u",
        )
        .bind(input.issuer)
        .bind(input.subject)
        .fetch_optional(&mut *transaction)
        .await?;

        let user = if let Some(row) = linked {
            if !row.get::<bool, _>("active") {
                return Err(OidcError::InactiveUser);
            }
            let mut user = authenticated_user_from_row(&row)?;
            if row.get::<bool, _>("oidc_only") {
                let mapped_role = input
                    .provisioning_role
                    .ok_or(OidcError::ProvisioningDenied)?;
                if user.role != mapped_role {
                    sqlx::query(
                        "UPDATE users SET role = CAST($2 AS user_role), etag = $3, updated_at = $4 \
                         WHERE id = $1",
                    )
                    .bind(user.id)
                    .bind(mapped_role.as_str())
                    .bind(Uuid::now_v7())
                    .bind(now)
                    .execute(&mut *transaction)
                    .await?;
                    insert_audit(
                        &mut transaction,
                        Some(user.id),
                        "user.role_sync_oidc",
                        "user",
                        &user.id.to_string(),
                        now,
                    )
                    .await?;
                    user.role = mapped_role;
                }
            }
            user
        } else {
            let role = input
                .provisioning_role
                .ok_or(OidcError::ProvisioningDenied)?;
            let email = normalize_email(input.email.ok_or(OidcError::ProvisioningDenied)?)?;
            lock_email(&mut transaction, &email).await?;
            let collision: bool =
                sqlx::query_scalar("SELECT EXISTS (SELECT 1 FROM users WHERE email = $1)")
                    .bind(&email)
                    .fetch_one(&mut *transaction)
                    .await?;
            if collision {
                return Err(OidcError::LinkRequired);
            }
            let user_id = Uuid::now_v7();
            let display_name = normalize_display_name(input.display_name, &email);
            sqlx::query(
                "INSERT INTO users \
                 (id, email, display_name, password_hash, role, active, etag, created_at, updated_at) \
                 VALUES ($1, $2, $3, NULL, CAST($4 AS user_role), true, $5, $6, $6)",
            )
            .bind(user_id)
            .bind(&email)
            .bind(&display_name)
            .bind(role.as_str())
            .bind(Uuid::now_v7())
            .bind(now)
            .execute(&mut *transaction)
            .await?;
            sqlx::query(
                "INSERT INTO oidc_identities \
                 (issuer, subject, user_id, email_at_link, last_login_at, created_at) \
                 VALUES ($1, $2, $3, $4, $5, $5)",
            )
            .bind(input.issuer)
            .bind(input.subject)
            .bind(user_id)
            .bind(&email)
            .bind(now)
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
            OidcAuthenticatedUser {
                id: user_id,
                email,
                display_name,
                role,
            }
        };

        sqlx::query(
            "UPDATE oidc_identities SET last_login_at = $3 \
             WHERE issuer = $1 AND subject = $2",
        )
        .bind(input.issuer)
        .bind(input.subject)
        .bind(now)
        .execute(&mut *transaction)
        .await?;
        insert_session(&mut transaction, user.id, input.session, expires_at, now).await?;
        insert_audit(
            &mut transaction,
            Some(user.id),
            "oidc.login",
            "session",
            &user.id.to_string(),
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
        let user_row = sqlx::query(
            "SELECT id, email, display_name, role::text AS role, active \
             FROM users WHERE id = $1 FOR UPDATE",
        )
        .bind(input.user_id)
        .fetch_optional(&mut *transaction)
        .await?
        .ok_or(OidcError::InactiveUser)?;
        // Hold a lock on the exact initiating session until the identity link
        // commits. This closes the gap between HTTP authentication and the
        // storage transaction: a concurrently revoked or expired session
        // cannot authorize the link, and revocation cannot complete midway.
        let initiating_session = sqlx::query(
            "SELECT id FROM sessions \
             WHERE id = $1 AND user_id = $2 AND expires_at > $3 FOR KEY SHARE",
        )
        .bind(input.session_id)
        .bind(input.user_id)
        .bind(now)
        .fetch_optional(&mut *transaction)
        .await?;
        if initiating_session.is_none() {
            return Err(OidcError::FlowUnavailable);
        }
        if !user_row.get::<bool, _>("active") {
            return Err(OidcError::InactiveUser);
        }
        let existing_subject =
            sqlx::query("SELECT user_id FROM oidc_identities WHERE issuer = $1 AND subject = $2")
                .bind(input.issuer)
                .bind(input.subject)
                .fetch_optional(&mut *transaction)
                .await?;
        if let Some(existing) = existing_subject
            && existing.get::<Uuid, _>("user_id") != input.user_id
        {
            return Err(OidcError::IdentityAlreadyLinked);
        }
        let other_identity: bool = sqlx::query_scalar(
            "SELECT EXISTS (SELECT 1 FROM oidc_identities WHERE issuer = $1 AND user_id = $2 AND subject <> $3)",
        )
        .bind(input.issuer)
        .bind(input.user_id)
        .bind(input.subject)
        .fetch_one(&mut *transaction)
        .await?;
        if other_identity {
            return Err(OidcError::IdentityAlreadyLinked);
        }
        let normalized_claim_email = input.email.and_then(|value| normalize_email(value).ok());
        sqlx::query(
            "INSERT INTO oidc_identities \
             (issuer, subject, user_id, email_at_link, last_login_at, created_at) \
             VALUES ($1, $2, $3, $4, $5, $5) \
             ON CONFLICT (issuer, subject) DO UPDATE SET last_login_at = EXCLUDED.last_login_at",
        )
        .bind(input.issuer)
        .bind(input.subject)
        .bind(input.user_id)
        .bind(normalized_claim_email)
        .bind(now)
        .execute(&mut *transaction)
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
        transaction.commit().await?;
        authenticated_user_from_row(&user_row)
    }

    /// Returns every identity linked to a local user. A configuration can move
    /// to a new issuer over its lifetime, so this intentionally returns a list
    /// instead of pretending the historical relationship is singular.
    pub async fn oidc_identities_for_user(
        &self,
        user_id: Uuid,
    ) -> Result<Vec<OidcIdentityRecord>, OidcError> {
        let rows = sqlx::query(
            "SELECT oi.id, oi.issuer, oi.email_at_link, oi.last_login_at, oi.created_at, \
                    (u.password_hash IS NOT NULL OR count(*) OVER () > 1) AS can_unlink \
             FROM oidc_identities oi \
             JOIN users u ON u.id = oi.user_id \
             WHERE oi.user_id = $1 \
             ORDER BY oi.created_at, oi.id",
        )
        .bind(user_id)
        .fetch_all(self.pool())
        .await?;
        Ok(rows
            .into_iter()
            .map(|row| OidcIdentityRecord {
                id: row.get("id"),
                issuer: row.get("issuer"),
                email_at_link: row.get("email_at_link"),
                last_login_at: row.get("last_login_at"),
                created_at: row.get("created_at"),
                can_unlink: row.get("can_unlink"),
            })
            .collect())
    }

    /// Removes a linked identity only when another authentication method will
    /// remain. The user row and all of its identities are locked so concurrent
    /// unlink requests cannot strand an OIDC-only account.
    pub async fn unlink_oidc_identity(
        &self,
        user_id: Uuid,
        identity_id: Uuid,
    ) -> Result<(), OidcError> {
        let now = Utc::now();
        let mut transaction = self.pool().begin().await?;
        let user = sqlx::query(
            "SELECT password_hash IS NOT NULL AS has_local_password \
             FROM users WHERE id = $1 AND active FOR UPDATE",
        )
        .bind(user_id)
        .fetch_optional(&mut *transaction)
        .await?
        .ok_or(OidcError::InactiveUser)?;
        let identities =
            sqlx::query("SELECT id FROM oidc_identities WHERE user_id = $1 FOR UPDATE")
                .bind(user_id)
                .fetch_all(&mut *transaction)
                .await?;
        if !identities
            .iter()
            .any(|identity| identity.get::<Uuid, _>("id") == identity_id)
        {
            return Err(OidcError::IdentityNotFound);
        }
        if !user.get::<bool, _>("has_local_password") && identities.len() <= 1 {
            return Err(OidcError::LastAuthenticationMethod);
        }
        let deleted = sqlx::query("DELETE FROM oidc_identities WHERE id = $1 AND user_id = $2")
            .bind(identity_id)
            .bind(user_id)
            .execute(&mut *transaction)
            .await?;
        if deleted.rows_affected() != 1 {
            return Err(OidcError::IdentityNotFound);
        }
        insert_audit(
            &mut transaction,
            Some(user_id),
            "oidc.identity_unlink",
            "oidc_identity",
            &identity_id.to_string(),
            now,
        )
        .await?;
        transaction.commit().await?;
        Ok(())
    }
}
