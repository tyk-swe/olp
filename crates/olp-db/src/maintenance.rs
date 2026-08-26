use chrono::{DateTime, Utc};
use sqlx::{Connection as _, PgConnection, Postgres, Transaction};
use thiserror::Error;

use crate::{
    request_metadata::{
        REQUEST_METADATA_EVENT_FUTURE_SKEW_MINUTES, REQUEST_METADATA_EVENT_REPLAY_HORIZON_DAYS,
    },
    store::Store,
};

pub const MAINTENANCE_LOCK_ID: i64 = 0x4f4c_505f_4d54; // "OLP_MT"
const RETENTION_DELETE_BATCH: i64 = 50_000;
const REQUEST_METADATA_RECEIPT_DELETE_BATCH: i64 = 250_000;

#[derive(Debug, Error)]
pub enum Error {
    #[error("database operation failed")]
    Database(#[from] sqlx::Error),
    #[error("retention setting {key} is invalid")]
    InvalidSetting { key: String },
    #[error("database returned an invalid {name} count")]
    InvalidCount { name: &'static str },
}

#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub struct Report {
    pub lock_acquired: bool,
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

struct Cutoffs {
    request: DateTime<Utc>,
    usage: DateTime<Utc>,
    audit: DateTime<Utc>,
}

impl Store {
    /// Rebuilds completed hourly aggregates before enforcing independent
    /// metadata, usage, and audit retention. One PostgreSQL advisory lock keeps
    /// multiple worker replicas from overlapping the same maintenance pass.
    /// Its checked-out session closes on cancellation and spans batch commits.
    pub async fn run_maintenance(&self, now: DateTime<Utc>) -> Result<Report, Error> {
        let mut connection = self.pool().acquire().await?;
        connection.close_on_drop();
        let locked: bool = sqlx::query_scalar!(
            "SELECT pg_try_advisory_lock($1) AS \"value!\"",
            MAINTENANCE_LOCK_ID
        )
        .fetch_one(&mut *connection)
        .await?;
        if !locked {
            return Ok(Report::default());
        }
        let cutoffs = retention_cutoffs(&mut connection, now).await?;
        let request_rows = purge_expired_requests(&mut connection, cutoffs.request).await?;
        let (rollup_rows, usage_rows) =
            roll_up_attempt_usage(&mut connection, cutoffs.usage).await?;
        roll_up_compatibility_usage(&mut connection, cutoffs.usage).await?;
        purge_orphaned_usage_anchors(&mut connection, cutoffs.request).await?;
        let audit_rows = purge_expired_audit_events(&mut connection, cutoffs.audit).await?;
        let mut report = Report {
            lock_acquired: true,
            rollup_rows,
            request_rows,
            usage_rows,
            audit_rows,
            ..Report::default()
        };
        purge_expiring_records(&mut connection, now, &cutoffs, &mut report).await?;
        Ok(report)
    }
}

async fn retention_cutoffs(
    connection: &mut PgConnection,
    now: DateTime<Utc>,
) -> Result<Cutoffs, Error> {
    // A read-only settings lookup needs no transaction of its own.
    let rows = sqlx::query!(
        "SELECT key, value FROM settings WHERE key IN \
         ('retention.requests_days', 'retention.usage_days', 'retention.audit_days')",
    )
    .fetch_all(&mut *connection)
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
            .ok_or_else(|| Error::InvalidSetting { key: key.clone() })?;
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
    Ok(Cutoffs {
        request: request_cutoff,
        usage: usage_cutoff,
        audit: audit_cutoff,
    })
}

async fn purge_expired_requests(
    connection: &mut PgConnection,
    request_cutoff: DateTime<Utc>,
) -> Result<u64, Error> {
    // Delete request metadata before facts, matching ingestion's
    // request -> anchor -> fact lock order. Facts no longer reference the
    // request table, so this does not affect usage retention.
    let mut request_rows = 0;
    loop {
        let mut transaction = connection.begin().await?;
        let rows = sqlx::query!(
            "WITH expired AS ( \
               SELECT id, started_at FROM requests WHERE started_at < $1 \
               LIMIT $2 FOR UPDATE SKIP LOCKED \
             ) \
             DELETE FROM requests request USING expired \
             WHERE request.id = expired.id AND request.started_at = expired.started_at",
            request_cutoff,
            RETENTION_DELETE_BATCH
        )
        .execute(&mut *transaction)
        .await?
        .rows_affected();
        transaction.commit().await?;
        request_rows += rows;
        if rows < RETENTION_DELETE_BATCH as u64 {
            break;
        }
    }
    Ok(request_rows)
}

/// Delete and aggregate the same row set in one statement. This keeps a late
/// stream event out of the delete set until a later pass and makes repeated
/// rollups additive for hours that already contain retained totals.
async fn roll_up_attempt_usage(
    connection: &mut PgConnection,
    usage_cutoff: DateTime<Utc>,
) -> Result<(u64, u64), Error> {
    let mut rollups = 0;
    let mut usage_rows = 0;
    loop {
        let mut transaction = connection.begin().await?;
        sqlx::query!("SELECT set_config('olp.usage_rollup_writer', 'additive-v2', true)")
            .fetch_one(&mut *transaction)
            .await?;
        let (rolled, expired) = attempt_usage_rollup_batch(&mut transaction, usage_cutoff).await?;
        let batch_rollups = checked_count(rolled, "usage rollup")?;
        let batch_usage_rows = checked_count(expired, "usage")?;
        transaction.commit().await?;
        rollups += batch_rollups;
        usage_rows += batch_usage_rows;
        if batch_usage_rows < RETENTION_DELETE_BATCH as u64 {
            break;
        }
    }
    Ok((rollups, usage_rows))
}

async fn attempt_usage_rollup_batch(
    transaction: &mut Transaction<'_, Postgres>,
    usage_cutoff: DateTime<Utc>,
) -> Result<(i64, i64), Error> {
    let batch = sqlx::query!(
        "WITH candidates AS ( \
       SELECT ctid FROM attempt_usage_facts \
       WHERE observed_at \
             < date_trunc('hour', $1::timestamptz AT TIME ZONE 'UTC') AT TIME ZONE 'UTC' \
       LIMIT $2 FOR UPDATE SKIP LOCKED \
     ), expired AS ( \
       DELETE FROM attempt_usage_facts fact USING candidates \
       WHERE fact.ctid = candidates.ctid \
       RETURNING route_slug, provider_id, upstream_model, operation, surface, \
                 api_key_id, observed_at, input_tokens, output_tokens, \
                 cached_input_tokens, media_units, estimated_cost, currency, \
                 request_counted, provider_request_counted, model_request_counted, \
                 target_request_counted, request_unpriced_counted, \
                 provider_unpriced_counted, model_unpriced_counted, \
                 target_unpriced_counted, request_incomplete_counted, \
                 provider_incomplete_counted, model_incomplete_counted, \
                 target_incomplete_counted \
     ), rolled AS ( \
     INSERT INTO attempt_usage_hourly \
     (bucket, route_slug, provider_id, upstream_model, operation, surface, api_key_id, \
      request_count, provider_request_count, model_request_count, target_request_count, \
      input_tokens, output_tokens, cached_input_tokens, media_units, estimated_cost, \
      request_unpriced_count, provider_unpriced_count, model_unpriced_count, \
      target_unpriced_count, request_incomplete_count, provider_incomplete_count, \
      model_incomplete_count, target_incomplete_count, currency) \
     SELECT date_trunc('hour', observed_at AT TIME ZONE 'UTC') AT TIME ZONE 'UTC', \
            route_slug, provider_id, upstream_model, \
            operation, surface, api_key_id, \
            COUNT(*) FILTER (WHERE request_counted), \
            COUNT(*) FILTER (WHERE provider_request_counted), \
            COUNT(*) FILTER (WHERE model_request_counted), \
            COUNT(*) FILTER (WHERE target_request_counted), \
            COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0), \
            COALESCE(SUM(cached_input_tokens), 0), COALESCE(SUM(media_units), 0), \
            SUM(estimated_cost), \
            COUNT(*) FILTER (WHERE request_unpriced_counted), \
            COUNT(*) FILTER (WHERE provider_unpriced_counted), \
            COUNT(*) FILTER (WHERE model_unpriced_counted), \
            COUNT(*) FILTER (WHERE target_unpriced_counted), \
            COUNT(*) FILTER (WHERE request_incomplete_counted), \
            COUNT(*) FILTER (WHERE provider_incomplete_counted), \
            COUNT(*) FILTER (WHERE model_incomplete_counted), \
            COUNT(*) FILTER (WHERE target_incomplete_counted), MAX(currency) \
     FROM expired \
     GROUP BY date_trunc('hour', observed_at AT TIME ZONE 'UTC') AT TIME ZONE 'UTC', \
              route_slug, provider_id, upstream_model, \
              operation, surface, api_key_id \
     ON CONFLICT ON CONSTRAINT attempt_usage_hourly_dimensions_key DO UPDATE SET \
       request_count = attempt_usage_hourly.request_count + EXCLUDED.request_count, \
       provider_request_count = attempt_usage_hourly.provider_request_count \
                                + EXCLUDED.provider_request_count, \
       model_request_count = attempt_usage_hourly.model_request_count \
                             + EXCLUDED.model_request_count, \
       target_request_count = attempt_usage_hourly.target_request_count \
                              + EXCLUDED.target_request_count, \
       input_tokens = attempt_usage_hourly.input_tokens + EXCLUDED.input_tokens, \
       output_tokens = attempt_usage_hourly.output_tokens + EXCLUDED.output_tokens, \
       cached_input_tokens = attempt_usage_hourly.cached_input_tokens \
                             + EXCLUDED.cached_input_tokens, \
       media_units = attempt_usage_hourly.media_units + EXCLUDED.media_units, \
       estimated_cost = CASE \
         WHEN attempt_usage_hourly.estimated_cost IS NULL \
              AND EXCLUDED.estimated_cost IS NULL THEN NULL \
         ELSE COALESCE(attempt_usage_hourly.estimated_cost, 0) \
              + COALESCE(EXCLUDED.estimated_cost, 0) END, \
       request_unpriced_count = attempt_usage_hourly.request_unpriced_count \
                                + EXCLUDED.request_unpriced_count, \
       provider_unpriced_count = attempt_usage_hourly.provider_unpriced_count \
                                 + EXCLUDED.provider_unpriced_count, \
       model_unpriced_count = attempt_usage_hourly.model_unpriced_count \
                              + EXCLUDED.model_unpriced_count, \
       target_unpriced_count = attempt_usage_hourly.target_unpriced_count \
                               + EXCLUDED.target_unpriced_count, \
       request_incomplete_count = attempt_usage_hourly.request_incomplete_count \
                                  + EXCLUDED.request_incomplete_count, \
       provider_incomplete_count = attempt_usage_hourly.provider_incomplete_count \
                                   + EXCLUDED.provider_incomplete_count, \
       model_incomplete_count = attempt_usage_hourly.model_incomplete_count \
                                + EXCLUDED.model_incomplete_count, \
       target_incomplete_count = attempt_usage_hourly.target_incomplete_count \
                                 + EXCLUDED.target_incomplete_count, \
       currency = COALESCE(attempt_usage_hourly.currency, EXCLUDED.currency) \
     RETURNING 1 \
     ) \
     SELECT (SELECT count(*) FROM rolled) AS \"rollup_rows!\", \
            (SELECT count(*) FROM expired) AS \"usage_rows!\"",
        usage_cutoff,
        RETENTION_DELETE_BATCH
    )
    .fetch_one(&mut **transaction)
    .await?;
    Ok((batch.rollup_rows, batch.usage_rows))
}

/// Retains the request-level compatibility aggregate for older readers. It is
/// never used for provider/model attribution by current code.
async fn roll_up_compatibility_usage(
    connection: &mut PgConnection,
    usage_cutoff: DateTime<Utc>,
) -> Result<(), Error> {
    loop {
        let mut transaction = connection.begin().await?;
        sqlx::query!("SELECT set_config('olp.usage_rollup_writer', 'additive-v2', true)")
            .fetch_one(&mut *transaction)
            .await?;
        let _hourly_mirror_setting =
            sqlx::query!("SELECT set_config('olp.attempt_usage_hourly_mirror', 'off', true)")
                .fetch_one(&mut *transaction)
                .await?;
        let _legacy_archive_setting =
            sqlx::query!("SELECT set_config('olp.attempt_usage_legacy_archive', 'off', true)")
                .fetch_one(&mut *transaction)
                .await?;
        let compatibility_usage_rollup = sqlx::query!(
            "WITH candidates AS ( \
           SELECT ctid FROM usage_facts \
           WHERE observed_at \
                 < date_trunc('hour', $1::timestamptz AT TIME ZONE 'UTC') AT TIME ZONE 'UTC' \
           LIMIT $2 FOR UPDATE SKIP LOCKED \
         ), expired AS ( \
           DELETE FROM usage_facts fact USING candidates \
           WHERE fact.ctid = candidates.ctid \
           RETURNING route_slug, provider_id, upstream_model, operation, surface, \
                     api_key_id, observed_at, input_tokens, output_tokens, \
                     cached_input_tokens, media_units, estimated_cost, unpriced, \
                     usage_complete, currency \
         ), rolled AS ( \
         INSERT INTO usage_hourly \
         (bucket, route_slug, provider_id, upstream_model, operation, surface, api_key_id, \
          request_count, input_tokens, output_tokens, cached_input_tokens, media_units, \
          estimated_cost, unpriced_count, incomplete_count, currency) \
         SELECT date_trunc('hour', observed_at AT TIME ZONE 'UTC') AT TIME ZONE 'UTC', \
                route_slug, provider_id, upstream_model, \
                operation, surface, api_key_id, \
                COUNT(*), COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0), \
                COALESCE(SUM(cached_input_tokens), 0), COALESCE(SUM(media_units), 0), \
                SUM(estimated_cost), COUNT(*) FILTER (WHERE unpriced), \
                COUNT(*) FILTER (WHERE NOT usage_complete), MAX(currency) \
         FROM expired \
         GROUP BY date_trunc('hour', observed_at AT TIME ZONE 'UTC') AT TIME ZONE 'UTC', \
                  route_slug, provider_id, upstream_model, \
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
            usage_cutoff,
            RETENTION_DELETE_BATCH
        )
        .fetch_one(&mut *transaction)
        .await?;
        let batch_usage_rows =
            checked_count(compatibility_usage_rollup.usage_rows, "compatibility usage")?;
        transaction.commit().await?;
        if batch_usage_rows < RETENTION_DELETE_BATCH as u64 {
            break;
        }
    }
    Ok(())
}

async fn purge_orphaned_usage_anchors(
    connection: &mut PgConnection,
    request_cutoff: DateTime<Utc>,
) -> Result<(), Error> {
    // Lock candidates before deleting them. A concurrent fact insert holds
    // KEY SHARE on its anchor, so SKIP LOCKED leaves that anchor for the
    // next pass instead of cascading a child invisible to this snapshot.
    loop {
        let mut transaction = connection.begin().await?;
        let rows = sqlx::query!(
            "WITH orphan AS ( \
           SELECT anchor.request_id, anchor.request_started_at \
           FROM usage_request_anchors anchor \
           WHERE anchor.request_started_at < $1 AND NOT EXISTS ( \
             SELECT 1 FROM usage_facts fact \
             WHERE fact.request_id = anchor.request_id \
               AND fact.request_started_at = anchor.request_started_at \
           ) AND NOT EXISTS ( \
             SELECT 1 FROM attempt_usage_facts fact \
             WHERE fact.request_id = anchor.request_id \
               AND fact.request_started_at = anchor.request_started_at \
           ) \
           LIMIT $2 \
           FOR UPDATE OF anchor SKIP LOCKED \
         ) \
         DELETE FROM usage_request_anchors anchor USING orphan \
         WHERE anchor.request_id = orphan.request_id \
           AND anchor.request_started_at = orphan.request_started_at",
            request_cutoff,
            RETENTION_DELETE_BATCH
        )
        .execute(&mut *transaction)
        .await?
        .rows_affected();
        transaction.commit().await?;
        if rows < RETENTION_DELETE_BATCH as u64 {
            break;
        }
    }
    Ok(())
}

async fn purge_expired_audit_events(
    connection: &mut PgConnection,
    audit_cutoff: DateTime<Utc>,
) -> Result<u64, Error> {
    let mut audit_rows = 0;
    loop {
        let mut transaction = connection.begin().await?;
        let rows = sqlx::query!(
            "WITH expired AS ( \
               SELECT ctid FROM audit_events WHERE occurred_at < $1 \
               LIMIT $2 FOR UPDATE SKIP LOCKED \
             ) \
             DELETE FROM audit_events audit USING expired \
             WHERE audit.ctid = expired.ctid",
            audit_cutoff,
            RETENTION_DELETE_BATCH
        )
        .execute(&mut *transaction)
        .await?
        .rows_affected();
        transaction.commit().await?;
        audit_rows += rows;
        if rows < RETENTION_DELETE_BATCH as u64 {
            break;
        }
    }
    Ok(audit_rows)
}

async fn purge_expiring_records(
    connection: &mut PgConnection,
    now: DateTime<Utc>,
    cutoffs: &Cutoffs,
    report: &mut Report,
) -> Result<(), Error> {
    let mut transaction = connection.begin().await?;
    sqlx::query!("SELECT set_config('olp.usage_rollup_writer', 'additive-v2', true)")
        .fetch_one(&mut *transaction)
        .await?;
    let (rolled, expired) = request_metadata_gap_rollup(&mut transaction, cutoffs.usage).await?;
    report.request_metadata_gap_rollup_rows = checked_count(rolled, "request metadata gap rollup")?;
    report.request_metadata_gap_rows = checked_count(expired, "request metadata gap")?;
    report.request_metadata_epoch_rows = sqlx::query!(
        "DELETE FROM request_metadata_gateway_epochs \
         WHERE (gracefully_closed_at IS NOT NULL AND gracefully_closed_at < $1) \
            OR (acknowledged_at IS NOT NULL AND acknowledged_at < $1)",
        cutoffs.usage
    )
    .execute(&mut *transaction)
    .await?
    .rows_affected();
    report.request_metadata_receipt_rows = sqlx::query!(
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
    report.session_rows = sqlx::query!("DELETE FROM sessions WHERE expires_at <= $1", now)
        .execute(&mut *transaction)
        .await?
        .rows_affected();
    report.invitation_rows = sqlx::query!(
        "DELETE FROM invitations WHERE expires_at <= $1 AND accepted_at IS NULL",
        now
    )
    .execute(&mut *transaction)
    .await?
    .rows_affected();
    report.idempotency_rows = sqlx::query!(
        "DELETE FROM idempotency_records WHERE expires_at <= $1",
        now
    )
    .execute(&mut *transaction)
    .await?
    .rows_affected();
    report.oidc_flow_rows = sqlx::query!(
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
    report.outbox_rows = sqlx::query!(
        "DELETE FROM transactional_outbox \
         WHERE published_at IS NOT NULL AND published_at < $1::timestamptz - interval '7 days'",
        now
    )
    .execute(&mut *transaction)
    .await?
    .rows_affected();
    report.media_job_rows = sqlx::query!(
        "DELETE FROM async_media_jobs
         WHERE lifecycle_state = 'deleted' AND deleted_at < $1",
        cutoffs.request
    )
    .execute(&mut *transaction)
    .await?
    .rows_affected();
    transaction.commit().await?;
    Ok(())
}

async fn request_metadata_gap_rollup(
    transaction: &mut Transaction<'_, Postgres>,
    usage_cutoff: DateTime<Utc>,
) -> Result<(i64, i64), Error> {
    let batch = sqlx::query!(
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
         SELECT date_trunc('hour', first_observed_at AT TIME ZONE 'UTC') AT TIME ZONE 'UTC', \
                gateway_instance, reason, \
                SUM(event_count), \
                COUNT(*) FILTER (WHERE certainty = 'lower_bound'::request_metadata_gap_certainty), \
                MIN(first_observed_at), MAX(last_observed_at) \
         FROM expired \
         GROUP BY date_trunc('hour', first_observed_at AT TIME ZONE 'UTC') AT TIME ZONE 'UTC', \
                  gateway_instance, reason \
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
        usage_cutoff,
        REQUEST_METADATA_EVENT_REPLAY_HORIZON_DAYS,
        REQUEST_METADATA_EVENT_FUTURE_SKEW_MINUTES
    )
    .fetch_one(&mut **transaction)
    .await?;
    Ok((batch.rollup_rows, batch.gap_rows))
}

fn checked_count(value: i64, name: &'static str) -> Result<u64, Error> {
    u64::try_from(value).map_err(|_| Error::InvalidCount { name })
}
