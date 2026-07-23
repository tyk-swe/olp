use chrono::{DateTime, Utc};
use thiserror::Error;

use crate::{
    PgStore,
    request_metadata::{
        REQUEST_METADATA_EVENT_FUTURE_SKEW_MINUTES, REQUEST_METADATA_EVENT_REPLAY_HORIZON_DAYS,
    },
};

const MAINTENANCE_LOCK_ID: i64 = 0x4f4c_505f_4d54; // "OLP_MT"
const REQUEST_METADATA_RECEIPT_DELETE_BATCH: i64 = 250_000;

#[derive(Debug, Error)]
pub enum MaintenanceError {
    #[error("database operation failed")]
    Database(#[from] sqlx::Error),
    #[error("retention setting {key} is invalid")]
    InvalidSetting { key: String },
    #[error("database returned an invalid {name} count")]
    InvalidCount { name: &'static str },
}

#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub struct MaintenanceReport {
    pub rollup_rows: u64,
    pub request_metadata_gap_rollup_rows: u64,
    pub request_rows: u64,
    pub usage_rows: u64,
    pub audit_rows: u64,
    pub request_metadata_gap_rows: u64,
    pub request_metadata_epoch_rows: u64,
    pub request_metadata_receipt_rows: u64,
    pub session_rows: u64,
    pub invitation_rows: u64,
    pub idempotency_rows: u64,
    pub oidc_flow_rows: u64,
    pub outbox_rows: u64,
    pub media_job_rows: u64,
}

impl PgStore {
    /// Rebuilds completed hourly aggregates before enforcing independent
    /// metadata, usage, and audit retention. One PostgreSQL advisory lock keeps
    /// multiple worker replicas from overlapping the same maintenance pass.
    pub async fn run_maintenance(
        &self,
        now: DateTime<Utc>,
    ) -> Result<MaintenanceReport, MaintenanceError> {
        let mut transaction = self.pool().begin().await?;
        sqlx::query!("SELECT set_config('olp.usage_rollup_writer', 'additive-v2', true)")
            .fetch_one(&mut *transaction)
            .await?;
        let locked: bool = sqlx::query_scalar!(
            "SELECT pg_try_advisory_xact_lock($1) AS \"value!\"",
            MAINTENANCE_LOCK_ID
        )
        .fetch_one(&mut *transaction)
        .await?;
        if !locked {
            return Ok(MaintenanceReport::default());
        }

        let rows = sqlx::query!(
            "SELECT key, value FROM settings WHERE key IN \
             ('retention.requests_days', 'retention.usage_days', 'retention.audit_days')",
        )
        .fetch_all(&mut *transaction)
        .await?;
        let mut requests_days = 30_i64;
        let mut usage_days = 90_i64;
        let mut audit_days = 365_i64;
        for row in rows {
            let key: String = row.key;
            let value: String = row.value;
            let parsed = value
                .parse::<i64>()
                .ok()
                .filter(|days| (1..=3_650).contains(days))
                .ok_or_else(|| MaintenanceError::InvalidSetting { key: key.clone() })?;
            match key.as_str() {
                "retention.requests_days" => requests_days = parsed,
                "retention.usage_days" => usage_days = parsed,
                "retention.audit_days" => audit_days = parsed,
                _ => {}
            }
        }

        let request_cutoff = now - chrono::Duration::days(requests_days);
        let usage_cutoff = now - chrono::Duration::days(usage_days);
        let audit_cutoff = now - chrono::Duration::days(audit_days);
        // Delete request metadata before facts, matching ingestion's
        // request -> anchor -> fact lock order. Facts no longer reference the
        // request table, so this does not affect usage retention.
        let request_rows =
            sqlx::query!("DELETE FROM requests WHERE started_at < $1", request_cutoff)
                .execute(&mut *transaction)
                .await?
                .rows_affected();

        // Delete and aggregate the same row set in one statement. This keeps a
        // late stream event out of the delete set until a later pass and makes
        // repeated rollups additive for hours that already contain retained
        // totals.
        let usage_rollup = sqlx::query!(
            "WITH expired AS ( \
               DELETE FROM usage_facts \
               WHERE observed_at < date_trunc('hour', $1::timestamptz) \
               RETURNING route_slug, provider_id, upstream_model, operation, surface, \
                         api_key_id, observed_at, input_tokens, output_tokens, \
                         cached_input_tokens, media_units, estimated_cost, unpriced, \
                         usage_complete, currency \
             ), rolled AS ( \
             INSERT INTO usage_hourly \
             (bucket, route_slug, provider_id, upstream_model, operation, surface, api_key_id, \
              request_count, input_tokens, output_tokens, cached_input_tokens, media_units, \
              estimated_cost, unpriced_count, incomplete_count, currency) \
             SELECT date_trunc('hour', observed_at), route_slug, provider_id, upstream_model, \
                    operation, surface, api_key_id, \
                    COUNT(*), COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0), \
                    COALESCE(SUM(cached_input_tokens), 0), COALESCE(SUM(media_units), 0), \
                    SUM(estimated_cost), COUNT(*) FILTER (WHERE unpriced), \
                    COUNT(*) FILTER (WHERE NOT usage_complete), MAX(currency) \
             FROM expired \
             GROUP BY date_trunc('hour', observed_at), route_slug, provider_id, upstream_model, \
                      operation, surface, api_key_id \
             ON CONFLICT ON CONSTRAINT usage_hourly_dimensions_key DO UPDATE SET \
               request_count = usage_hourly.request_count + EXCLUDED.request_count, \
               input_tokens = usage_hourly.input_tokens + EXCLUDED.input_tokens, \
               output_tokens = usage_hourly.output_tokens + EXCLUDED.output_tokens, \
               estimated_cost = CASE \
                 WHEN usage_hourly.estimated_cost IS NULL AND EXCLUDED.estimated_cost IS NULL \
                 THEN NULL \
                 ELSE COALESCE(usage_hourly.estimated_cost, 0) \
                      + COALESCE(EXCLUDED.estimated_cost, 0) END, \
               cached_input_tokens = usage_hourly.cached_input_tokens \
                                     + EXCLUDED.cached_input_tokens, \
               media_units = usage_hourly.media_units + EXCLUDED.media_units, \
               unpriced_count = usage_hourly.unpriced_count + EXCLUDED.unpriced_count, \
               incomplete_count = usage_hourly.incomplete_count + EXCLUDED.incomplete_count, \
               currency = COALESCE(usage_hourly.currency, EXCLUDED.currency) \
             RETURNING 1 \
             ) \
             SELECT (SELECT count(*) FROM rolled) AS \"rollup_rows!\", \
                    (SELECT count(*) FROM expired) AS \"usage_rows!\"",
            usage_cutoff
        )
        .fetch_one(&mut *transaction)
        .await?;
        let rollups = checked_count(usage_rollup.rollup_rows, "usage rollup")?;
        let usage_rows = checked_count(usage_rollup.usage_rows, "usage")?;

        // Lock candidates before deleting them. A concurrent fact insert holds
        // KEY SHARE on its anchor, so SKIP LOCKED leaves that anchor for the
        // next pass instead of cascading a child invisible to this snapshot.
        sqlx::query!(
            "WITH orphan AS ( \
               SELECT anchor.request_id, anchor.request_started_at \
               FROM usage_request_anchors anchor \
               WHERE anchor.request_started_at < $1 AND NOT EXISTS ( \
                 SELECT 1 FROM usage_facts fact \
                 WHERE fact.request_id = anchor.request_id \
                   AND fact.request_started_at = anchor.request_started_at \
               ) \
               FOR UPDATE OF anchor SKIP LOCKED \
             ) \
             DELETE FROM usage_request_anchors anchor USING orphan \
             WHERE anchor.request_id = orphan.request_id \
               AND anchor.request_started_at = orphan.request_started_at",
            request_cutoff
        )
        .execute(&mut *transaction)
        .await?;
        let audit_rows = sqlx::query!(
            "DELETE FROM audit_events WHERE occurred_at < $1",
            audit_cutoff
        )
        .execute(&mut *transaction)
        .await?
        .rows_affected();
        let request_metadata_gap_rollup = sqlx::query!(
            "WITH expired AS ( \
               DELETE FROM request_metadata_ingestion_gaps \
               WHERE reported_at < $1 \
                 AND (deduplication_key IS NULL OR \
                      reported_at < now() - make_interval( \
                          days => $2::integer, mins => $3::integer)) \
               RETURNING gateway_instance, reason, event_count, certainty, \
                         first_observed_at, last_observed_at \
             ), rolled AS ( \
             INSERT INTO request_metadata_gap_hourly \
               (bucket, gateway_instance, reason, event_count, uncertain_gap_count, \
                first_observed_at, last_observed_at) \
             SELECT date_trunc('hour', first_observed_at), gateway_instance, reason, \
                    SUM(event_count), \
                    COUNT(*) FILTER (WHERE certainty = 'lower_bound'::request_metadata_gap_certainty), \
                    MIN(first_observed_at), MAX(last_observed_at) \
             FROM expired \
             GROUP BY date_trunc('hour', first_observed_at), gateway_instance, reason \
             ON CONFLICT (bucket, gateway_instance, reason) DO UPDATE SET \
               event_count = request_metadata_gap_hourly.event_count + EXCLUDED.event_count, \
               uncertain_gap_count = request_metadata_gap_hourly.uncertain_gap_count \
                                     + EXCLUDED.uncertain_gap_count, \
               first_observed_at = LEAST(request_metadata_gap_hourly.first_observed_at, \
                                         EXCLUDED.first_observed_at), \
               last_observed_at = GREATEST(request_metadata_gap_hourly.last_observed_at, \
                                           EXCLUDED.last_observed_at) \
             RETURNING 1 \
             ) \
             SELECT (SELECT count(*) FROM rolled) AS \"rollup_rows!\", \
                    (SELECT count(*) FROM expired) AS \"gap_rows!\"",
        usage_cutoff, REQUEST_METADATA_EVENT_REPLAY_HORIZON_DAYS, REQUEST_METADATA_EVENT_FUTURE_SKEW_MINUTES)
        .fetch_one(&mut *transaction)
        .await?;
        let request_metadata_gap_rollup_rows = checked_count(
            request_metadata_gap_rollup.rollup_rows,
            "request metadata gap rollup",
        )?;
        let request_metadata_gap_rows =
            checked_count(request_metadata_gap_rollup.gap_rows, "request metadata gap")?;
        let request_metadata_epoch_rows = sqlx::query!(
            "DELETE FROM request_metadata_gateway_epochs \
             WHERE (gracefully_closed_at IS NOT NULL AND gracefully_closed_at < $1) \
                OR (acknowledged_at IS NOT NULL AND acknowledged_at < $1)",
            usage_cutoff
        )
        .execute(&mut *transaction)
        .await?
        .rows_affected();
        let request_metadata_receipt_rows = sqlx::query!(
            "WITH expired AS ( \
               SELECT ctid FROM request_metadata_event_receipts \
               WHERE recorded_at < now() - make_interval( \
                   days => $1::integer, mins => $2::integer) \
               LIMIT $3 FOR UPDATE SKIP LOCKED \
             ) \
             DELETE FROM request_metadata_event_receipts receipt USING expired \
             WHERE receipt.ctid = expired.ctid",
            REQUEST_METADATA_EVENT_REPLAY_HORIZON_DAYS,
            REQUEST_METADATA_EVENT_FUTURE_SKEW_MINUTES,
            REQUEST_METADATA_RECEIPT_DELETE_BATCH
        )
        .execute(&mut *transaction)
        .await?
        .rows_affected();
        let session_rows = sqlx::query!("DELETE FROM sessions WHERE expires_at <= $1", now)
            .execute(&mut *transaction)
            .await?
            .rows_affected();
        let invitation_rows = sqlx::query!(
            "DELETE FROM invitations WHERE expires_at <= $1 AND accepted_at IS NULL",
            now
        )
        .execute(&mut *transaction)
        .await?
        .rows_affected();
        let idempotency_rows = sqlx::query!(
            "DELETE FROM idempotency_records WHERE expires_at <= $1",
            now
        )
        .execute(&mut *transaction)
        .await?
        .rows_affected();
        let oidc_flow_rows = sqlx::query!(
            "DELETE FROM oidc_authorization_flows WHERE expires_at <= $1",
            now
        )
        .execute(&mut *transaction)
        .await?
        .rows_affected()
            + sqlx::query!(
                "DELETE FROM oidc_login_flow_consumptions WHERE expires_at <= $1",
                now
            )
            .execute(&mut *transaction)
            .await?
            .rows_affected();
        let outbox_rows = sqlx::query!(
            "DELETE FROM transactional_outbox \
             WHERE published_at IS NOT NULL AND published_at < $1::timestamptz - interval '7 days'",
            now
        )
        .execute(&mut *transaction)
        .await?
        .rows_affected();
        let media_job_rows = sqlx::query!(
            "DELETE FROM async_media_jobs
             WHERE lifecycle_state = 'deleted' AND deleted_at < $1",
            request_cutoff
        )
        .execute(&mut *transaction)
        .await?
        .rows_affected();

        transaction.commit().await?;
        Ok(MaintenanceReport {
            rollup_rows: rollups,
            request_metadata_gap_rollup_rows,
            request_rows,
            usage_rows,
            audit_rows,
            request_metadata_gap_rows,
            request_metadata_epoch_rows,
            request_metadata_receipt_rows,
            session_rows,
            invitation_rows,
            idempotency_rows,
            oidc_flow_rows,
            outbox_rows,
            media_job_rows,
        })
    }
}

fn checked_count(value: i64, name: &'static str) -> Result<u64, MaintenanceError> {
    u64::try_from(value).map_err(|_| MaintenanceError::InvalidCount { name })
}
