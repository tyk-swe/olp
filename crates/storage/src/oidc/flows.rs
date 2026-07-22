use chrono::{DateTime, Duration, Utc};
use sqlx::Row;
use uuid::Uuid;

use super::helpers::{encrypted_from_row, require_current_enabled_configuration, token_digest};
use super::{NewOidcFlow, OidcError, OidcFlowPurpose, OidcFlowRecord};
use crate::PgStore;

const OIDC_FLOW_CAPACITY_LOCK_ID: i64 = 0x4f4c_505f_4f46; // "OLP_OF"
const MAX_ACTIVE_FLOWS: i64 = 10_000;
const MAX_AUTHORIZATION_FLOWS_PER_MINUTE: i64 = 300;
const OIDC_LOGIN_CONSUMPTION_DELETE_BATCH: i64 = 1_000;

impl PgStore {
    /// Persists an authenticated identity-link flow. Anonymous login flows
    /// are encrypted browser cookies and must never create a database row.
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
        sqlx::query("SELECT pg_advisory_xact_lock($1)")
            .bind(OIDC_FLOW_CAPACITY_LOCK_ID)
            .execute(&mut *transaction)
            .await?;
        sqlx::query("DELETE FROM oidc_authorization_flows WHERE expires_at <= now()")
            .execute(&mut *transaction)
            .await?;
        let active_flows: i64 = sqlx::query_scalar("SELECT count(*) FROM oidc_authorization_flows")
            .fetch_one(&mut *transaction)
            .await?;
        if active_flows >= MAX_ACTIVE_FLOWS {
            return Err(OidcError::FlowCapacity);
        }
        let recent_flows: i64 = sqlx::query_scalar(
            "SELECT count(*) FROM oidc_authorization_flows \
             WHERE created_at > now() - interval '1 minute'",
        )
        .fetch_one(&mut *transaction)
        .await?;
        if recent_flows >= MAX_AUTHORIZATION_FLOWS_PER_MINUTE {
            return Err(OidcError::FlowRateLimited);
        }
        sqlx::query(
            "INSERT INTO oidc_authorization_flows \
             (id, configuration_id, configuration_etag, purpose, actor_user_id, actor_session_id, \
              state_digest, browser_binding_digest, client_digest, encrypted_payload, payload_nonce, \
              payload_key_version, expires_at, created_at) \
             VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NULL, $9, $10, $11, $12, now())",
        )
        .bind(flow.id)
        .bind(flow.configuration_id)
        .bind(flow.configuration_etag)
        .bind(flow.purpose.as_str())
        .bind(flow.actor_user_id)
        .bind(flow.actor_session_id)
        .bind(flow.state_digest.to_vec())
        .bind(flow.browser_binding_digest.to_vec())
        .bind(flow.encrypted_payload.ciphertext)
        .bind(flow.encrypted_payload.nonce.to_vec())
        .bind(i32::try_from(flow.encrypted_payload.key_version).map_err(|_| OidcError::Corrupt)?)
        .bind(flow.expires_at)
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
        let consumed: Option<Uuid> = sqlx::query_scalar(
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
        )
        .bind(flow_id)
        .bind(expires_at)
        .bind(OIDC_LOGIN_CONSUMPTION_DELETE_BATCH)
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
        let row = sqlx::query(
            "SELECT id, configuration_id, purpose, actor_user_id, actor_session_id, \
                    encrypted_payload, payload_nonce, payload_key_version \
             FROM oidc_authorization_flows \
             WHERE id = $1 AND state_digest = $2 AND browser_binding_digest = $3 \
               AND expires_at > now() \
             FOR UPDATE",
        )
        .bind(flow_id)
        .bind(token_digest(state).to_vec())
        .bind(token_digest(browser_binding).to_vec())
        .fetch_optional(&mut *transaction)
        .await?
        .ok_or(OidcError::FlowUnavailable)?;
        if row.get::<Option<Uuid>, _>("actor_session_id") != Some(actor_session_id) {
            transaction.rollback().await?;
            return Err(OidcError::FlowSessionMismatch);
        }
        let deleted = sqlx::query("DELETE FROM oidc_authorization_flows WHERE id = $1")
            .bind(flow_id)
            .execute(&mut *transaction)
            .await?;
        if deleted.rows_affected() != 1 {
            return Err(OidcError::Corrupt);
        }
        let flow = OidcFlowRecord {
            id: row.get("id"),
            configuration_id: row.get("configuration_id"),
            purpose: OidcFlowPurpose::parse(row.get("purpose"))?,
            actor_user_id: row.get("actor_user_id"),
            actor_session_id: row.get("actor_session_id"),
            encrypted_payload: encrypted_from_row(
                row.get("payload_key_version"),
                row.get("payload_nonce"),
                row.get("encrypted_payload"),
            )?,
        };
        transaction.commit().await?;
        Ok(flow)
    }
}
