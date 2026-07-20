use chrono::{DateTime, Utc};
use rust_decimal::Decimal;
use sqlx::Row;
use uuid::Uuid;

use super::{buffer::UsageBufferSnapshot, helpers::usage_gap_count_from_decimal};
use crate::{PersistenceError, PgStore};

const USAGE_GATEWAY_EPOCH_LOCK_SEED: i64 = 0x4f4c_505f_5545;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct UsageLossReport {
    pub reported_events: u64,
    pub reported_dropped: u64,
    pub reported_abandoned: u64,
    pub process_epoch_changed: bool,
}

#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub struct UsageEpochDetection {
    pub candidate_epochs: u64,
    pub detected_epochs: u64,
    pub uncertain_event_lower_bound: u64,
}

#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub struct UsageEpochHealth {
    pub open_epochs: u64,
    pub unresolved_epochs: u64,
    pub historical_uncertain_gap_count: u64,
    pub unresolved_event_lower_bound: u64,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct UsageEpochAcknowledgement {
    pub gateway_instance: String,
    pub process_epoch: Uuid,
    pub acknowledged_at: DateTime<Utc>,
    pub acknowledged_by: Option<Uuid>,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum UsageGatewayEpochState {
    Open,
    GracefullyClosed,
    Unresolved,
    Acknowledged,
}

impl UsageGatewayEpochState {
    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::Open => "open",
            Self::GracefullyClosed => "gracefully_closed",
            Self::Unresolved => "unresolved",
            Self::Acknowledged => "acknowledged",
        }
    }
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct UsageGatewayEpochRecord {
    pub gateway_instance: String,
    pub process_epoch: Uuid,
    pub state: UsageGatewayEpochState,
    pub started_at: DateTime<Utc>,
    pub updated_at: DateTime<Utc>,
    pub accepted: u64,
    pub persisted: u64,
    pub dropped: u64,
    pub abandoned: u64,
    pub uncertain_event_lower_bound: u64,
    pub retrying: bool,
    pub writer_closed: bool,
    pub gracefully_closed_at: Option<DateTime<Utc>>,
    pub stale_detected_at: Option<DateTime<Utc>>,
    pub acknowledged_at: Option<DateTime<Utc>>,
    pub acknowledged_by: Option<Uuid>,
}

/// Gateway-local checkpoints are emitted every second. A minute without a
/// checkpoint followed by a separate confirmation pass avoids fabricating an
/// outage when PostgreSQL briefly recovers before the live gateway reporter.
pub const USAGE_GATEWAY_EPOCH_STALE_AFTER_SECONDS: i64 = 60;
const USAGE_GATEWAY_EPOCH_CONFIRM_AFTER_SECONDS: i64 = 10;

async fn record_unclean_usage_epoch(
    transaction: &mut sqlx::Transaction<'_, sqlx::Postgres>,
    gateway_instance: &str,
    row: &sqlx::postgres::PgRow,
    detected_at: DateTime<Utc>,
) -> Result<i64, PersistenceError> {
    let process_epoch: Uuid = row.get("process_epoch");
    let accepted: i64 = row.get("accepted");
    let persisted: i64 = row.get("persisted");
    let abandoned: i64 = row.get("abandoned");
    let last_checkpoint: DateTime<Utc> = row.get("updated_at");
    let lower_bound = accepted
        .saturating_sub(persisted)
        .saturating_sub(abandoned)
        .max(0);
    let gap_id = Uuid::now_v7();
    sqlx::query(
        "INSERT INTO usage_ingestion_gaps \
         (id, gateway_instance, event_count, reason, certainty, first_observed_at, \
          last_observed_at, reported_at) \
         VALUES ($1, $2, $3, 'gateway_epoch_unclean_shutdown', \
                 'lower_bound'::usage_gap_certainty, $4, $5, $5)",
    )
    .bind(gap_id)
    .bind(gateway_instance)
    .bind(lower_bound)
    .bind(last_checkpoint)
    .bind(detected_at.max(last_checkpoint))
    .execute(&mut **transaction)
    .await?;
    sqlx::query(
        "UPDATE usage_gateway_epochs \
         SET stale_detected_at = $1, uncertainty_gap_id = $2 \
         WHERE gateway_instance = $3 AND process_epoch = $4 \
           AND gracefully_closed_at IS NULL AND stale_detected_at IS NULL",
    )
    .bind(detected_at)
    .bind(gap_id)
    .bind(gateway_instance)
    .bind(process_epoch)
    .execute(&mut **transaction)
    .await?;
    Ok(lower_bound)
}

impl PgStore {
    /// Durably checkpoints the current gateway epoch and records exact unseen
    /// local-loss deltas. This runs only in the background reporter; inference
    /// requests never wait for PostgreSQL.
    pub async fn report_usage_buffer_loss(
        &self,
        gateway_instance: &str,
        snapshot: &UsageBufferSnapshot,
    ) -> Result<UsageLossReport, PersistenceError> {
        self.checkpoint_usage_buffer_epoch(gateway_instance, snapshot, false)
            .await
    }

    /// Marks an epoch as gracefully closed after the stream writer has closed
    /// its receiver and accounted for every accepted-but-unpersisted event.
    /// A clean close is never eligible for stale-epoch uncertainty detection.
    pub async fn close_usage_buffer_epoch(
        &self,
        gateway_instance: &str,
        snapshot: &UsageBufferSnapshot,
    ) -> Result<UsageLossReport, PersistenceError> {
        self.checkpoint_usage_buffer_epoch(gateway_instance, snapshot, true)
            .await
    }

    async fn checkpoint_usage_buffer_epoch(
        &self,
        gateway_instance: &str,
        snapshot: &UsageBufferSnapshot,
        graceful_close: bool,
    ) -> Result<UsageLossReport, PersistenceError> {
        let gateway_instance = gateway_instance.trim();
        if gateway_instance.is_empty()
            || gateway_instance.len() > 200
            || gateway_instance.chars().any(char::is_control)
            || snapshot.persisted > snapshot.accepted
            || snapshot.abandoned > snapshot.accepted - snapshot.persisted
            || (graceful_close && !snapshot.gracefully_drained())
        {
            return Err(PersistenceError::InvalidUsageGap);
        }
        let accepted =
            i64::try_from(snapshot.accepted).map_err(|_| PersistenceError::InvalidUsageGap)?;
        let persisted =
            i64::try_from(snapshot.persisted).map_err(|_| PersistenceError::InvalidUsageGap)?;
        let dropped =
            i64::try_from(snapshot.dropped).map_err(|_| PersistenceError::InvalidUsageGap)?;
        let abandoned =
            i64::try_from(snapshot.abandoned).map_err(|_| PersistenceError::InvalidUsageGap)?;
        let now = Utc::now();
        let mut transaction = self.pool().begin().await?;
        sqlx::query("SELECT pg_advisory_xact_lock(hashtextextended($1, $2))")
            .bind(gateway_instance)
            .bind(USAGE_GATEWAY_EPOCH_LOCK_SEED)
            .execute(&mut *transaction)
            .await?;
        let previous = sqlx::query(
            "SELECT accepted, persisted, dropped, abandoned, writer_closed, updated_at, \
                    gracefully_closed_at, stale_detected_at \
             FROM usage_gateway_epochs \
             WHERE gateway_instance = $1 AND process_epoch = $2 FOR UPDATE",
        )
        .bind(gateway_instance)
        .bind(snapshot.process_epoch)
        .fetch_optional(&mut *transaction)
        .await?;
        let process_epoch_changed = previous.is_none();
        if previous.is_none() {
            // A replacement epoch with the same stable gateway identity proves
            // that every older unclosed epoch ended without its shutdown hook.
            let superseded = sqlx::query(
                "SELECT process_epoch, accepted, persisted, abandoned, updated_at \
                 FROM usage_gateway_epochs \
                 WHERE gateway_instance = $1 AND process_epoch <> $2 \
                   AND gracefully_closed_at IS NULL AND stale_detected_at IS NULL \
                 FOR UPDATE",
            )
            .bind(gateway_instance)
            .bind(snapshot.process_epoch)
            .fetch_all(&mut *transaction)
            .await?;
            for epoch in superseded {
                record_unclean_usage_epoch(&mut transaction, gateway_instance, &epoch, now).await?;
            }
        }
        let (previous_dropped, previous_abandoned, previous_checkpoint) =
            if let Some(row) = previous {
                if row
                    .get::<Option<DateTime<Utc>>, _>("stale_detected_at")
                    .is_some()
                {
                    return Err(PersistenceError::InvalidUsageGap);
                }
                let previous_accepted = row.get::<i64, _>("accepted");
                let previous_persisted = row.get::<i64, _>("persisted");
                let previous_dropped = row.get::<i64, _>("dropped");
                let previous_abandoned = row.get::<i64, _>("abandoned");
                let previously_closed = row.get::<bool, _>("writer_closed");
                if accepted < previous_accepted
                    || persisted < previous_persisted
                    || dropped < previous_dropped
                    || abandoned < previous_abandoned
                    || (previously_closed && !snapshot.closed)
                {
                    return Err(PersistenceError::InvalidUsageGap);
                }
                if row
                    .get::<Option<DateTime<Utc>>, _>("gracefully_closed_at")
                    .is_some()
                {
                    if graceful_close
                        && snapshot.closed
                        && accepted == previous_accepted
                        && persisted == previous_persisted
                        && dropped == previous_dropped
                        && abandoned == previous_abandoned
                    {
                        transaction.commit().await?;
                        return Ok(UsageLossReport {
                            reported_events: 0,
                            reported_dropped: 0,
                            reported_abandoned: 0,
                            process_epoch_changed: false,
                        });
                    }
                    return Err(PersistenceError::InvalidUsageGap);
                }
                (
                    previous_dropped,
                    previous_abandoned,
                    row.get::<DateTime<Utc>, _>("updated_at"),
                )
            } else {
                (0_i64, 0_i64, snapshot.started_at)
            };
        let dropped_delta = dropped - previous_dropped;
        let abandoned_delta = abandoned - previous_abandoned;
        let event_count = dropped_delta
            .checked_add(abandoned_delta)
            .ok_or(PersistenceError::InvalidUsageGap)?;
        if event_count > 0 {
            let last_observed_at = snapshot.last_loss_at.unwrap_or(now);
            let first_observed_at = if process_epoch_changed {
                snapshot.first_loss_at.unwrap_or(snapshot.started_at)
            } else {
                snapshot
                    .first_loss_at
                    .map_or(previous_checkpoint, |first| first.max(previous_checkpoint))
            }
            .min(last_observed_at);
            sqlx::query(
                "INSERT INTO usage_ingestion_gaps \
                 (id, gateway_instance, event_count, reason, first_observed_at, \
                  last_observed_at, reported_at) \
                 VALUES ($1, $2, $3, 'gateway_local_buffer_loss', $4, $5, $6)",
            )
            .bind(Uuid::now_v7())
            .bind(gateway_instance)
            .bind(event_count)
            .bind(first_observed_at)
            .bind(last_observed_at)
            .bind(now)
            .execute(&mut *transaction)
            .await?;
        }
        sqlx::query(
            "INSERT INTO usage_gateway_epochs \
             (gateway_instance, process_epoch, started_at, accepted, persisted, dropped, abandoned, \
              retrying, writer_closed, updated_at, gracefully_closed_at) \
             VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, \
                     CASE WHEN $11 THEN $10 ELSE NULL END) \
             ON CONFLICT (gateway_instance, process_epoch) DO UPDATE SET \
               accepted = EXCLUDED.accepted, persisted = EXCLUDED.persisted, \
               dropped = EXCLUDED.dropped, abandoned = EXCLUDED.abandoned, \
               retrying = EXCLUDED.retrying, writer_closed = EXCLUDED.writer_closed, \
               updated_at = EXCLUDED.updated_at, stale_candidate_at = NULL, \
               gracefully_closed_at = CASE WHEN $11 \
                 THEN COALESCE(usage_gateway_epochs.gracefully_closed_at, $10) \
                 ELSE usage_gateway_epochs.gracefully_closed_at END",
        )
        .bind(gateway_instance)
        .bind(snapshot.process_epoch)
        .bind(snapshot.started_at)
        .bind(accepted)
        .bind(persisted)
        .bind(dropped)
        .bind(abandoned)
        .bind(snapshot.retrying)
        .bind(snapshot.closed)
        .bind(now)
        .bind(graceful_close)
        .execute(&mut *transaction)
        .await?;
        // Keep the pre-epoch cumulative checkpoint current during rolling
        // upgrades and rollback. New code never reads it for epoch detection.
        sqlx::query(
            "INSERT INTO usage_loss_reporter_state \
             (gateway_instance, process_epoch, dropped, abandoned, updated_at) \
             VALUES ($1, $2, $3, $4, $5) \
             ON CONFLICT (gateway_instance) DO UPDATE SET \
               process_epoch = EXCLUDED.process_epoch, dropped = EXCLUDED.dropped, \
               abandoned = EXCLUDED.abandoned, updated_at = EXCLUDED.updated_at",
        )
        .bind(gateway_instance)
        .bind(snapshot.process_epoch)
        .bind(dropped)
        .bind(abandoned)
        .bind(now)
        .execute(&mut *transaction)
        .await?;
        transaction.commit().await?;
        Ok(UsageLossReport {
            reported_events: u64::try_from(event_count)
                .map_err(|_| PersistenceError::InvalidUsageGap)?,
            reported_dropped: u64::try_from(dropped_delta)
                .map_err(|_| PersistenceError::InvalidUsageGap)?,
            reported_abandoned: u64::try_from(abandoned_delta)
                .map_err(|_| PersistenceError::InvalidUsageGap)?,
            process_epoch_changed,
        })
    }

    /// Two-pass stale detection makes abrupt loss durable and idempotent. The
    /// first pass records a candidate; a later pass confirms it only if no live
    /// reporter cleared the marker with another checkpoint.
    pub async fn detect_stale_usage_gateway_epochs(
        &self,
        now: DateTime<Utc>,
    ) -> Result<UsageEpochDetection, PersistenceError> {
        let stale_cutoff = now - chrono::Duration::seconds(USAGE_GATEWAY_EPOCH_STALE_AFTER_SECONDS);
        let confirmation_cutoff =
            now - chrono::Duration::seconds(USAGE_GATEWAY_EPOCH_CONFIRM_AFTER_SECONDS);
        let mut transaction = self.pool().begin().await?;
        let rows = sqlx::query(
            "SELECT gateway_instance, process_epoch, accepted, persisted, abandoned, updated_at, \
                    stale_candidate_at \
             FROM usage_gateway_epochs \
             WHERE gracefully_closed_at IS NULL AND stale_detected_at IS NULL \
               AND updated_at < $1 \
             ORDER BY updated_at, gateway_instance, process_epoch \
             LIMIT 100 FOR UPDATE SKIP LOCKED",
        )
        .bind(stale_cutoff)
        .fetch_all(&mut *transaction)
        .await?;
        let mut report = UsageEpochDetection::default();
        for row in rows {
            let gateway_instance: String = row.get("gateway_instance");
            let process_epoch: Uuid = row.get("process_epoch");
            match row.get::<Option<DateTime<Utc>>, _>("stale_candidate_at") {
                Some(candidate_at) if candidate_at <= confirmation_cutoff => {
                    let lower_bound =
                        record_unclean_usage_epoch(&mut transaction, &gateway_instance, &row, now)
                            .await?;
                    report.detected_epochs = report.detected_epochs.saturating_add(1);
                    report.uncertain_event_lower_bound = report
                        .uncertain_event_lower_bound
                        .saturating_add(u64::try_from(lower_bound).unwrap_or(u64::MAX));
                }
                None => {
                    sqlx::query(
                        "UPDATE usage_gateway_epochs SET stale_candidate_at = $1 \
                         WHERE gateway_instance = $2 AND process_epoch = $3 \
                           AND gracefully_closed_at IS NULL AND stale_detected_at IS NULL",
                    )
                    .bind(now)
                    .bind(&gateway_instance)
                    .bind(process_epoch)
                    .execute(&mut *transaction)
                    .await?;
                    report.candidate_epochs = report.candidate_epochs.saturating_add(1);
                }
                Some(_) => {}
            }
        }
        transaction.commit().await?;
        Ok(report)
    }

    pub async fn usage_gateway_epoch_health(&self) -> Result<UsageEpochHealth, PersistenceError> {
        let row = sqlx::query(
            "SELECT count(*) FILTER (WHERE gracefully_closed_at IS NULL \
                                      AND stale_detected_at IS NULL) AS open_epochs, \
                    count(*) FILTER (WHERE stale_detected_at IS NOT NULL \
                                      AND acknowledged_at IS NULL) AS unresolved_epochs, \
                    COALESCE(sum(GREATEST(accepted - persisted - abandoned, 0)) \
                      FILTER (WHERE stale_detected_at IS NOT NULL \
                              AND acknowledged_at IS NULL), 0) AS unresolved_lower_bound, \
                    (SELECT count(*) FROM usage_ingestion_gaps \
                      WHERE certainty = 'lower_bound'::usage_gap_certainty) \
                    + (SELECT COALESCE(sum(uncertain_gap_count), 0) FROM usage_gap_hourly) \
                      AS historical_uncertain_gap_count \
             FROM usage_gateway_epochs",
        )
        .fetch_one(self.pool())
        .await?;
        let historical_uncertain_gap_count = usage_gap_count_from_decimal(
            row.try_get::<Decimal, _>("historical_uncertain_gap_count")?,
        )?;
        let unresolved_event_lower_bound =
            usage_gap_count_from_decimal(row.try_get::<Decimal, _>("unresolved_lower_bound")?)?;
        Ok(UsageEpochHealth {
            open_epochs: u64::try_from(row.get::<i64, _>("open_epochs"))
                .map_err(|_| PersistenceError::InvalidUsageGap)?,
            unresolved_epochs: u64::try_from(row.get::<i64, _>("unresolved_epochs"))
                .map_err(|_| PersistenceError::InvalidUsageGap)?,
            historical_uncertain_gap_count,
            unresolved_event_lower_bound,
        })
    }

    /// Acknowledges durable uncertainty after an operator has reviewed the
    /// incident. This clears current readiness degradation but never deletes
    /// the raw/hourly completeness evidence for the affected time range.
    pub async fn acknowledge_usage_gateway_epoch(
        &self,
        process_epoch: Uuid,
        actor: Uuid,
    ) -> Result<Option<UsageEpochAcknowledgement>, PersistenceError> {
        let mut transaction = self.pool().begin().await?;
        let row = sqlx::query(
            "SELECT gateway_instance, acknowledged_at, acknowledged_by \
             FROM usage_gateway_epochs \
             WHERE process_epoch = $1 AND stale_detected_at IS NOT NULL \
             FOR UPDATE",
        )
        .bind(process_epoch)
        .fetch_optional(&mut *transaction)
        .await?;
        let Some(row) = row else {
            return Ok(None);
        };
        let gateway_instance: String = row.get("gateway_instance");
        let existing_at: Option<DateTime<Utc>> = row.get("acknowledged_at");
        let existing_actor: Option<Uuid> = row.get("acknowledged_by");
        if let Some(acknowledged_at) = existing_at {
            transaction.commit().await?;
            return Ok(Some(UsageEpochAcknowledgement {
                gateway_instance,
                process_epoch,
                acknowledged_at,
                acknowledged_by: existing_actor,
            }));
        }
        let acknowledged_at = Utc::now();
        let acknowledgement = sqlx::query(
            "UPDATE usage_gateway_epochs \
             SET acknowledged_at = GREATEST($1, stale_detected_at), acknowledged_by = $2 \
             WHERE process_epoch = $3 AND acknowledged_at IS NULL \
             RETURNING acknowledged_at, acknowledged_by",
        )
        .bind(acknowledged_at)
        .bind(actor)
        .bind(process_epoch)
        .fetch_one(&mut *transaction)
        .await?;
        let acknowledged_at: DateTime<Utc> = acknowledgement.get("acknowledged_at");
        let acknowledged_by: Option<Uuid> = acknowledgement.get("acknowledged_by");
        sqlx::query(
            "INSERT INTO audit_events \
             (id, actor_user_id, action, resource_type, resource_id, outcome, occurred_at) \
             VALUES ($1, $2, 'usage.gateway_epoch_acknowledge', 'usage_gateway_epoch', \
                     $3, 'success', $4)",
        )
        .bind(Uuid::now_v7())
        .bind(actor)
        .bind(process_epoch.to_string())
        .bind(acknowledged_at)
        .execute(&mut *transaction)
        .await?;
        transaction.commit().await?;
        Ok(Some(UsageEpochAcknowledgement {
            gateway_instance,
            process_epoch,
            acknowledged_at,
            acknowledged_by,
        }))
    }
}
