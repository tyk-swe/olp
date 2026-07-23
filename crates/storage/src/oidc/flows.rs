use chrono::{DateTime, Duration, Utc};
use uuid::Uuid;

use super::helpers::{encrypted_from_row, require_current_enabled_configuration, token_digest};
use super::{NewOidcFlow, OidcError, OidcFlowPurpose, OidcFlowRecord};
use crate::{
    PgStore, RecentAuthPurpose,
    authentication::{consume_recent_authentication, insert_security_audit},
};

const OIDC_FLOW_CAPACITY_LOCK_ID: i64 = 0x4f4c_505f_4f46; // "OLP_OF"
const MAX_ACTIVE_FLOWS: i64 = 10_000;
const MAX_AUTHORIZATION_FLOWS_PER_MINUTE: i64 = 300;
const OIDC_LOGIN_CONSUMPTION_DELETE_BATCH: i64 = 1_000;

impl PgStore {
    /// Persists an authenticated link or reauthentication flow. The exact
    /// initiating session and security version are part of the durable flow.
    /// Link creation additionally consumes its one-time recent-auth grant in
    /// the same transaction as flow insertion.
    pub async fn create_oidc_flow(&self, flow: NewOidcFlow) -> Result<(), OidcError> {
        if flow.purpose == OidcFlowPurpose::Login {
            return Err(OidcError::Invalid(
                "new OIDC login flows are stateless and cannot be persisted".to_owned(),
            ));
        }
        if flow.expires_at <= Utc::now()
            || flow.actor_user_id.is_none()
            || flow.actor_session_id.is_none()
        {
            return Err(OidcError::Invalid(
                "authorization flow metadata is invalid".to_owned(),
            ));
        }
        let (actor_user_id, actor_session_id, actor_security_version) = match (
            flow.actor_user_id,
            flow.actor_session_id,
            flow.actor_security_version,
        ) {
            (Some(user_id), Some(session_id), Some(security_version)) if security_version > 0 => {
                (user_id, session_id, security_version)
            }
            _ => {
                return Err(OidcError::Invalid(
                    "authorization flow security context is invalid".to_owned(),
                ));
            }
        };
        match flow.purpose {
            OidcFlowPurpose::Link
                if flow.recent_auth_purpose.is_none()
                    && flow.recent_auth_resource_id.is_none()
                    && flow.recent_auth_token_digest.is_some() => {}
            OidcFlowPurpose::Reauthenticate
                if flow.recent_auth_purpose.is_some()
                    && flow.recent_auth_token_digest.is_none()
                    && flow.recent_auth_resource_id.is_some()
                        == flow
                            .recent_auth_purpose
                            .is_some_and(RecentAuthPurpose::requires_resource) => {}
            _ => {
                return Err(OidcError::Invalid(
                    "authorization flow purpose metadata is invalid".to_owned(),
                ));
            }
        }

        let mut transaction = self.pool().begin().await?;
        // Configuration updates delete every outstanding redirect while
        // holding this lock. Serialize insertion with that invalidation and
        // reject a flow built from a configuration that changed while its
        // authorization URL was being prepared.
        require_current_enabled_configuration(
            &mut transaction,
            flow.configuration_id,
            flow.configuration_etag,
        )
        .await?;

        match flow.purpose {
            OidcFlowPurpose::Link => {
                if !consume_recent_authentication(
                    &mut transaction,
                    actor_session_id,
                    actor_user_id,
                    actor_security_version,
                    RecentAuthPurpose::OidcLink,
                    None,
                    flow.recent_auth_token_digest.ok_or(OidcError::Corrupt)?,
                )
                .await?
                {
                    return Err(OidcError::RecentAuthenticationRequired);
                }
                insert_security_audit(
                    &mut transaction,
                    actor_user_id,
                    "authentication.recent_consume_for_oidc_link",
                    "session",
                    &actor_session_id.to_string(),
                    Utc::now(),
                )
                .await?;
            }
            OidcFlowPurpose::Reauthenticate => {
                let current: bool = sqlx::query_scalar!(
                    "SELECT EXISTS ( \
                         SELECT 1 FROM sessions session \
                         JOIN users ON users.id = session.user_id \
                         WHERE session.id = $1 AND session.user_id = $2 \
                           AND session.security_version = $3 \
                           AND users.security_version = $3 AND users.active \
                           AND session.expires_at > now() \
                     ) AS \"value!\"",
                    actor_session_id,
                    actor_user_id,
                    actor_security_version
                )
                .fetch_one(&mut *transaction)
                .await?;
                if !current {
                    return Err(OidcError::SessionUnavailable);
                }
            }
            OidcFlowPurpose::Login => unreachable!(),
        }

        sqlx::query!(
            "SELECT pg_advisory_xact_lock($1)",
            OIDC_FLOW_CAPACITY_LOCK_ID
        )
        .fetch_one(&mut *transaction)
        .await?;
        sqlx::query!("DELETE FROM oidc_authorization_flows WHERE expires_at <= now()")
            .execute(&mut *transaction)
            .await?;
        let active_flows: i64 =
            sqlx::query_scalar!("SELECT count(*) AS \"value!\" FROM oidc_authorization_flows")
                .fetch_one(&mut *transaction)
                .await?;
        if active_flows >= MAX_ACTIVE_FLOWS {
            return Err(OidcError::FlowCapacity);
        }
        let recent_flows: i64 = sqlx::query_scalar!(
            "SELECT count(*) AS \"value!\" FROM oidc_authorization_flows \
             WHERE created_at > now() - interval '1 minute'",
        )
        .fetch_one(&mut *transaction)
        .await?;
        if recent_flows >= MAX_AUTHORIZATION_FLOWS_PER_MINUTE {
            return Err(OidcError::FlowRateLimited);
        }
        sqlx::query!(
            "INSERT INTO oidc_authorization_flows \
             (id, configuration_id, configuration_etag, purpose, actor_user_id, \
              actor_session_id, actor_security_version, recent_auth_purpose, \
              recent_auth_resource_id, state_digest, browser_binding_digest, client_digest, \
              encrypted_payload, payload_nonce, payload_key_version, expires_at, created_at) \
             VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, NULL, \
                     $12, $13, $14, $15, now())",
            flow.id,
            flow.configuration_id,
            flow.configuration_etag,
            flow.purpose.as_str(),
            actor_user_id,
            actor_session_id,
            actor_security_version,
            flow.recent_auth_purpose.map(RecentAuthPurpose::as_str),
            flow.recent_auth_resource_id,
            flow.state_digest.to_vec(),
            flow.browser_binding_digest.to_vec(),
            flow.encrypted_payload.ciphertext,
            flow.encrypted_payload.nonce.to_vec(),
            i32::try_from(flow.encrypted_payload.key_version).map_err(|_| OidcError::Corrupt)?,
            flow.expires_at
        )
        .execute(&mut *transaction)
        .await?;
        transaction.commit().await?;
        Ok(())
    }

    /// Atomically claims a browser-held login flow before any provider
    /// exchange. The row contains no PKCE, state, nonce, or identity material;
    /// it exists only until the encrypted cookie's own expiry. A data-modifying
    /// CTE keeps cleanup self-sustaining even in control-only deployments that
    /// do not run the periodic maintenance worker.
    pub async fn consume_oidc_login_flow(
        &self,
        flow_id: Uuid,
        expires_at: DateTime<Utc>,
    ) -> Result<(), OidcError> {
        let now = Utc::now();
        if expires_at <= now || expires_at > now + Duration::minutes(11) {
            return Err(OidcError::FlowUnavailable);
        }
        let consumed: Option<Uuid> = sqlx::query_scalar!(
            "WITH expired AS ( \
               SELECT ctid FROM oidc_login_flow_consumptions \
               WHERE expires_at <= now() LIMIT $3 \
             ), deleted AS ( \
               DELETE FROM oidc_login_flow_consumptions consumption USING expired \
               WHERE consumption.ctid = expired.ctid \
             ) \
             INSERT INTO oidc_login_flow_consumptions (flow_id, expires_at, consumed_at) \
             SELECT $1, $2, now() WHERE $2 > now() \
             ON CONFLICT (flow_id) DO NOTHING \
             RETURNING flow_id",
            flow_id,
            expires_at,
            OIDC_LOGIN_CONSUMPTION_DELETE_BATCH
        )
        .fetch_optional(self.pool())
        .await?;
        consumed.ok_or(OidcError::FlowUnavailable)?;
        Ok(())
    }

    /// Atomically consumes state only when the protected flow identifier,
    /// callback state, browser binding, and exact initiating session all
    /// match. A different live session receives a distinct non-consuming
    /// result so its callback cannot invalidate the initiating browser flow.
    pub async fn consume_oidc_flow(
        &self,
        flow_id: Uuid,
        state: &str,
        browser_binding: &str,
        actor_session_id: Uuid,
    ) -> Result<OidcFlowRecord, OidcError> {
        if state.len() != 43 || browser_binding.len() != 43 {
            return Err(OidcError::FlowUnavailable);
        }
        let mut transaction = self.pool().begin().await?;
        let row = sqlx::query!(
            "SELECT id, configuration_id, purpose, actor_user_id, actor_session_id, \
                    actor_security_version, recent_auth_purpose, recent_auth_resource_id, \
                    encrypted_payload, payload_nonce, payload_key_version \
             FROM oidc_authorization_flows \
             WHERE id = $1 AND state_digest = $2 AND browser_binding_digest = $3 \
               AND expires_at > now() \
             FOR UPDATE",
            flow_id,
            token_digest(state).to_vec(),
            token_digest(browser_binding).to_vec()
        )
        .fetch_optional(&mut *transaction)
        .await?
        .ok_or(OidcError::FlowUnavailable)?;
        if row.actor_session_id != Some(actor_session_id) {
            transaction.rollback().await?;
            return Err(OidcError::FlowSessionMismatch);
        }
        let deleted = sqlx::query!(
            "DELETE FROM oidc_authorization_flows WHERE id = $1",
            flow_id
        )
        .execute(&mut *transaction)
        .await?;
        if deleted.rows_affected() != 1 {
            return Err(OidcError::Corrupt);
        }
        let flow = OidcFlowRecord {
            id: row.id,
            configuration_id: row.configuration_id,
            purpose: OidcFlowPurpose::parse(&row.purpose)?,
            actor_user_id: row.actor_user_id,
            actor_session_id: row.actor_session_id,
            actor_security_version: row.actor_security_version,
            recent_auth_purpose: row
                .recent_auth_purpose
                .map(|value| RecentAuthPurpose::parse(&value).ok_or(OidcError::Corrupt))
                .transpose()?,
            recent_auth_resource_id: row.recent_auth_resource_id,
            encrypted_payload: encrypted_from_row(
                row.payload_key_version,
                row.payload_nonce,
                row.encrypted_payload,
            )?,
        };
        transaction.commit().await?;
        Ok(flow)
    }
}
