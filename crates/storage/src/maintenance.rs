use chrono::{DateTime, Utc};
use sqlx::Row;
use thiserror::Error;

use crate::{
    PgStore,
    usage::{USAGE_EVENT_FUTURE_SKEW_MINUTES, USAGE_EVENT_REPLAY_HORIZON_DAYS},
};

const MAINTENANCE_LOCK_ID: i64 = 0x4f4c_505f_4d54; // "OLP_MT"
const USAGE_RECEIPT_DELETE_BATCH: i64 = 250_000;

#[derive(Debug, Error)]
pub enum MaintenanceError {
    #[error("database operation failed")]
    Database(#[from] sqlx::Error),
    #[error("retention setting {key} is invalid")]
    InvalidSetting { key: String },
}

#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub struct MaintenanceReport {
    pub rollup_rows: u64,
    pub gap_rollup_rows: u64,
    pub request_rows: u64,
    pub usage_rows: u64,
    pub audit_rows: u64,
    pub model_discovery_rows: u64,
    pub certification_run_rows: u64,
    pub gap_rows: u64,
    pub usage_epoch_rows: u64,
    pub usage_receipt_rows: u64,
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
        sqlx::query("SELECT set_config('olp.usage_rollup_writer', 'additive-v2', true)")
            .execute(&mut *transaction)
            .await?;
        let locked: bool = sqlx::query_scalar("SELECT pg_try_advisory_xact_lock($1)")
            .bind(MAINTENANCE_LOCK_ID)
            .fetch_one(&mut *transaction)
            .await?;
        if !locked {
            return Ok(MaintenanceReport::default());
        }

        let rows = sqlx::query(
            "SELECT key, value FROM settings WHERE key IN \
             ('retention.requests_days', 'retention.usage_days', 'retention.audit_days')",
        )
        .fetch_all(&mut *transaction)
        .await?;
        let mut requests_days = 30_i64;
        let mut usage_days = 90_i64;
        let mut audit_days = 365_i64;
        for row in rows {
            let key: String = row.get("key");
            let value: String = row.get("value");
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
        let request_rows = sqlx::query("DELETE FROM requests WHERE started_at < $1")
            .bind(request_cutoff)
            .execute(&mut *transaction)
            .await?
            .rows_affected();

        // Delete and aggregate the same row set in one statement. This keeps a
        // late stream event out of the delete set until a later pass and makes
        // repeated rollups additive for hours that already contain retained
        // totals.
        let usage_rollup = sqlx::query(
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
             SELECT (SELECT count(*) FROM rolled) AS rollup_rows, \
                    (SELECT count(*) FROM expired) AS usage_rows",
        )
        .bind(usage_cutoff)
        .fetch_one(&mut *transaction)
        .await?;
        let rollups = u64::try_from(usage_rollup.get::<i64, _>("rollup_rows"))
            .expect("PostgreSQL COUNT is non-negative");
        let usage_rows = u64::try_from(usage_rollup.get::<i64, _>("usage_rows"))
            .expect("PostgreSQL COUNT is non-negative");

        // Lock candidates before deleting them. A concurrent fact insert holds
        // KEY SHARE on its anchor, so SKIP LOCKED leaves that anchor for the
        // next pass instead of cascading a child invisible to this snapshot.
        sqlx::query(
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
        )
        .bind(request_cutoff)
        .execute(&mut *transaction)
        .await?;
        let audit_rows = sqlx::query("DELETE FROM audit_events WHERE occurred_at < $1")
            .bind(audit_cutoff)
            .execute(&mut *transaction)
            .await?
            .rows_affected();
        let model_discovery_rows =
            sqlx::query("DELETE FROM provider_model_discovery_runs WHERE completed_at < $1")
                .bind(audit_cutoff)
                .execute(&mut *transaction)
                .await?
                .rows_affected();
        sqlx::query(
            "UPDATE capability_certification_runs SET status = 'superseded', completed_at = now() \
             WHERE status = 'running' AND lease_expires_at < now()",
        )
        .execute(&mut *transaction)
        .await?;
        // Keep the run and its results while a currently certified tuple cites
        // it as active evidence, even after normal operational history expires.
        let certification_run_rows = sqlx::query(
            "DELETE FROM capability_certification_runs run \
             WHERE run.completed_at < $1 \
               AND NOT EXISTS (SELECT 1 FROM model_capabilities capability \
                               WHERE capability.certification_run_id = run.id \
                                 AND capability.source = 'certified') \
               AND NOT EXISTS (SELECT 1 FROM provider_revision_capabilities revision_capability \
                               JOIN provider_revision_models revision_model \
                                 ON revision_model.id = revision_capability.provider_revision_model_id \
                               JOIN provider_revisions revision \
                                 ON revision.id = revision_model.provider_revision_id \
                               JOIN providers provider ON provider.active_revision_id = revision.id \
                               WHERE revision_capability.certification_run_id = run.id)",
        )
        .bind(audit_cutoff)
        .execute(&mut *transaction)
        .await?
        .rows_affected();
        let gap_rollup = sqlx::query(
            "WITH expired AS ( \
               DELETE FROM usage_ingestion_gaps \
               WHERE reported_at < $1 \
                 AND (deduplication_key IS NULL OR \
                      reported_at < now() - make_interval( \
                          days => $2::integer, mins => $3::integer)) \
               RETURNING gateway_instance, reason, event_count, certainty, \
                         first_observed_at, last_observed_at \
             ), rolled AS ( \
             INSERT INTO usage_gap_hourly \
               (bucket, gateway_instance, reason, event_count, uncertain_gap_count, \
                first_observed_at, last_observed_at) \
             SELECT date_trunc('hour', first_observed_at), gateway_instance, reason, \
                    SUM(event_count), \
                    COUNT(*) FILTER (WHERE certainty = 'lower_bound'::usage_gap_certainty), \
                    MIN(first_observed_at), MAX(last_observed_at) \
             FROM expired \
             GROUP BY date_trunc('hour', first_observed_at), gateway_instance, reason \
             ON CONFLICT (bucket, gateway_instance, reason) DO UPDATE SET \
               event_count = usage_gap_hourly.event_count + EXCLUDED.event_count, \
               uncertain_gap_count = usage_gap_hourly.uncertain_gap_count \
                                     + EXCLUDED.uncertain_gap_count, \
               first_observed_at = LEAST(usage_gap_hourly.first_observed_at, \
                                         EXCLUDED.first_observed_at), \
               last_observed_at = GREATEST(usage_gap_hourly.last_observed_at, \
                                           EXCLUDED.last_observed_at) \
             RETURNING 1 \
             ) \
             SELECT (SELECT count(*) FROM rolled) AS rollup_rows, \
                    (SELECT count(*) FROM expired) AS gap_rows",
        )
        .bind(usage_cutoff)
        .bind(USAGE_EVENT_REPLAY_HORIZON_DAYS)
        .bind(USAGE_EVENT_FUTURE_SKEW_MINUTES)
        .fetch_one(&mut *transaction)
        .await?;
        let gap_rollup_rows = u64::try_from(gap_rollup.get::<i64, _>("rollup_rows"))
            .expect("PostgreSQL COUNT is non-negative");
        let gap_rows = u64::try_from(gap_rollup.get::<i64, _>("gap_rows"))
            .expect("PostgreSQL COUNT is non-negative");
        let usage_epoch_rows = sqlx::query(
            "DELETE FROM usage_gateway_epochs \
             WHERE (gracefully_closed_at IS NOT NULL AND gracefully_closed_at < $1) \
                OR (acknowledged_at IS NOT NULL AND acknowledged_at < $1)",
        )
        .bind(usage_cutoff)
        .execute(&mut *transaction)
        .await?
        .rows_affected();
        let usage_receipt_rows = sqlx::query(
            "WITH expired AS ( \
               SELECT ctid FROM usage_event_receipts \
               WHERE recorded_at < now() - make_interval( \
                   days => $1::integer, mins => $2::integer) \
               LIMIT $3 FOR UPDATE SKIP LOCKED \
             ) \
             DELETE FROM usage_event_receipts receipt USING expired \
             WHERE receipt.ctid = expired.ctid",
        )
        .bind(USAGE_EVENT_REPLAY_HORIZON_DAYS)
        .bind(USAGE_EVENT_FUTURE_SKEW_MINUTES)
        .bind(USAGE_RECEIPT_DELETE_BATCH)
        .execute(&mut *transaction)
        .await?
        .rows_affected();
        let session_rows = sqlx::query("DELETE FROM sessions WHERE expires_at <= $1")
            .bind(now)
            .execute(&mut *transaction)
            .await?
            .rows_affected();
        let invitation_rows =
            sqlx::query("DELETE FROM invitations WHERE expires_at <= $1 AND accepted_at IS NULL")
                .bind(now)
                .execute(&mut *transaction)
                .await?
                .rows_affected();
        let idempotency_rows =
            sqlx::query("DELETE FROM idempotency_records WHERE expires_at <= $1")
                .bind(now)
                .execute(&mut *transaction)
                .await?
                .rows_affected();
        let oidc_flow_rows =
            sqlx::query("DELETE FROM oidc_authorization_flows WHERE expires_at <= $1")
                .bind(now)
                .execute(&mut *transaction)
                .await?
                .rows_affected()
                + sqlx::query("DELETE FROM oidc_login_flow_consumptions WHERE expires_at <= $1")
                    .bind(now)
                    .execute(&mut *transaction)
                    .await?
                    .rows_affected();
        let outbox_rows = sqlx::query(
            "DELETE FROM transactional_outbox \
             WHERE published_at IS NOT NULL AND published_at < $1 - interval '7 days'",
        )
        .bind(now)
        .execute(&mut *transaction)
        .await?
        .rows_affected();
        let media_job_rows = sqlx::query(
            "DELETE FROM async_media_jobs
             WHERE lifecycle_state = 'deleted' AND deleted_at < $1",
        )
        .bind(request_cutoff)
        .execute(&mut *transaction)
        .await?
        .rows_affected();

        transaction.commit().await?;
        Ok(MaintenanceReport {
            rollup_rows: rollups,
            gap_rollup_rows,
            request_rows,
            usage_rows,
            audit_rows,
            model_discovery_rows,
            certification_run_rows,
            gap_rows,
            usage_epoch_rows,
            usage_receipt_rows,
            session_rows,
            invitation_rows,
            idempotency_rows,
            oidc_flow_rows,
            outbox_rows,
            media_job_rows,
        })
    }
}
