use sqlx::{Postgres, Transaction};

use crate::PgStore;

use super::TeamError;

const LOCAL_LOGIN_SOURCE_ATTEMPTS_PER_MINUTE: i32 = 60;
const INVITATION_SOURCE_ATTEMPTS_PER_MINUTE: i32 = 30;
const OIDC_LOGIN_SOURCE_ATTEMPTS_PER_MINUTE: i32 = 60;
const SOURCE_TARGET_ATTEMPTS_PER_MINUTE: i32 = 5;
const PUBLIC_AUTH_RESOURCE_ATTEMPTS_PER_MINUTE: i32 = 10_000;
const PUBLIC_AUTH_DELETE_BATCH: i64 = 1_000;

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
}

async fn consume_public_auth_bucket(
    transaction: &mut Transaction<'_, Postgres>,
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
