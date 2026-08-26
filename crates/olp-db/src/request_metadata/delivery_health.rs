use chrono::{DateTime, Utc};
use sqlx::{FromRow, Postgres, QueryBuilder};

use super::reconciliation::{GatewayEpochRecord, GatewayEpochState};
use crate::{
    error::Error as PersistenceError,
    operations::cursor::{Error, Page, Timestamp},
    split_page,
    store::Store,
    worker_health::{WorkerTask, WorkerTaskCheckpointOutcome, checkpoint_worker_task_on},
};

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct ConsumerHealth {
    pub pending_events: u64,
    pub lag_events: u64,
    pub oldest_pending_at: Option<DateTime<Utc>>,
    pub checked_at: DateTime<Utc>,
}

/// The worker reports every five seconds. Four missed checkpoints distinguish
/// a genuinely stale consumer from ordinary scheduling and database jitter.
pub const REQUEST_METADATA_CONSUMER_STALE_AFTER_SECONDS: i64 = 20;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum ConsumerState {
    Unknown,
    Healthy,
    Backlogged,
    Stale,
}

impl ConsumerState {
    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::Unknown => "unknown",
            Self::Healthy => "healthy",
            Self::Backlogged => "backlogged",
            Self::Stale => "stale",
        }
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct ConsumerStatus {
    pub state: ConsumerState,
    pub pending_events: u64,
    pub lag_events: u64,
    pub oldest_pending_at: Option<DateTime<Utc>>,
    pub checked_at: Option<DateTime<Utc>>,
    pub heartbeat_age_seconds: Option<u64>,
}

impl ConsumerStatus {
    #[must_use]
    pub fn from_health(health: Option<ConsumerHealth>, now: DateTime<Utc>) -> Self {
        let Some(health) = health else {
            return Self {
                state: ConsumerState::Unknown,
                pending_events: 0,
                lag_events: 0,
                oldest_pending_at: None,
                checked_at: None,
                heartbeat_age_seconds: None,
            };
        };
        let age = now
            .signed_duration_since(health.checked_at)
            .num_seconds()
            .max(0);
        let age_seconds = u64::try_from(age).map_or(u64::MAX, |value| value);
        let state = if age > REQUEST_METADATA_CONSUMER_STALE_AFTER_SECONDS {
            ConsumerState::Stale
        } else if health.pending_events > 0 || health.lag_events > 0 {
            ConsumerState::Backlogged
        } else {
            ConsumerState::Healthy
        };
        Self {
            state,
            pending_events: health.pending_events,
            lag_events: health.lag_events,
            oldest_pending_at: health.oldest_pending_at,
            checked_at: Some(health.checked_at),
            heartbeat_age_seconds: Some(age_seconds),
        }
    }

    #[must_use]
    pub const fn complete(self) -> bool {
        matches!(self.state, ConsumerState::Healthy)
    }
}

impl Store {
    /// Checkpoints the Valkey consumer-group backlog so health and usage
    /// completeness reflect worker-side stalls, not only gateway-local queue
    /// delivery. This contains counts and timestamps only.
    pub async fn report_request_metadata_consumer_health(
        &self,
        pending_events: u64,
        lag_events: u64,
        oldest_pending_at: Option<DateTime<Utc>>,
    ) -> Result<ConsumerHealth, PersistenceError> {
        self.report_request_metadata_consumer_health_sampled_at_inner(
            pending_events,
            lag_events,
            oldest_pending_at,
            Utc::now(),
        )
        .await
    }

    #[cfg(feature = "test-util")]
    /// Records a request-metadata consumer health sample captured at a caller
    /// supplied time. Production callers use
    /// [`Store::report_request_metadata_consumer_health`].
    pub async fn report_request_metadata_consumer_health_sampled_at(
        &self,
        pending_events: u64,
        lag_events: u64,
        oldest_pending_at: Option<DateTime<Utc>>,
        checked_at: DateTime<Utc>,
    ) -> Result<ConsumerHealth, PersistenceError> {
        self.report_request_metadata_consumer_health_sampled_at_inner(
            pending_events,
            lag_events,
            oldest_pending_at,
            checked_at,
        )
        .await
    }

    async fn report_request_metadata_consumer_health_sampled_at_inner(
        &self,
        pending_events: u64,
        lag_events: u64,
        oldest_pending_at: Option<DateTime<Utc>>,
        checked_at: DateTime<Utc>,
    ) -> Result<ConsumerHealth, PersistenceError> {
        if (pending_events == 0) != oldest_pending_at.is_none() {
            return Err(PersistenceError::InvalidRequestMetadataGap);
        }
        let pending_events = i64::try_from(pending_events)
            .map_err(|_| PersistenceError::InvalidRequestMetadataGap)?;
        let lag_events =
            i64::try_from(lag_events).map_err(|_| PersistenceError::InvalidRequestMetadataGap)?;
        let mut transaction = self.pool().begin().await?;
        let database_now = sqlx::query_scalar!("SELECT clock_timestamp() AS \"database_now!\"")
            .fetch_one(&mut *transaction)
            .await?;
        let checked_at = checked_at.min(database_now);
        if oldest_pending_at
            .is_some_and(|oldest| oldest > checked_at + chrono::Duration::minutes(5))
        {
            return Err(PersistenceError::InvalidRequestMetadataGap);
        }
        let row = sqlx::query!(
            "INSERT INTO request_metadata_consumer_health \
             (singleton, pending_events, lag_events, oldest_pending_at, checked_at) \
             VALUES (true, $1, $2, $3, $4) \
             ON CONFLICT (singleton) DO UPDATE SET \
               pending_events = EXCLUDED.pending_events, \
               lag_events = EXCLUDED.lag_events, \
               oldest_pending_at = EXCLUDED.oldest_pending_at, \
               checked_at = EXCLUDED.checked_at \
             WHERE request_metadata_consumer_health.checked_at <= EXCLUDED.checked_at \
             RETURNING pending_events, lag_events, oldest_pending_at, checked_at",
            pending_events,
            lag_events,
            oldest_pending_at,
            checked_at,
        )
        .fetch_optional(&mut *transaction)
        .await?;
        let health = if let Some(row) = row {
            ConsumerHealth {
                pending_events: u64::try_from(row.pending_events)
                    .map_err(|_| PersistenceError::InvalidRequestMetadataGap)?,
                lag_events: u64::try_from(row.lag_events)
                    .map_err(|_| PersistenceError::InvalidRequestMetadataGap)?,
                oldest_pending_at: row.oldest_pending_at,
                checked_at: row.checked_at,
            }
        } else {
            let row = sqlx::query!(
                "SELECT pending_events, lag_events, oldest_pending_at, checked_at \
                 FROM request_metadata_consumer_health WHERE singleton"
            )
            .fetch_one(&mut *transaction)
            .await?;
            ConsumerHealth {
                pending_events: u64::try_from(row.pending_events)
                    .map_err(|_| PersistenceError::InvalidRequestMetadataGap)?,
                lag_events: u64::try_from(row.lag_events)
                    .map_err(|_| PersistenceError::InvalidRequestMetadataGap)?,
                oldest_pending_at: row.oldest_pending_at,
                checked_at: row.checked_at,
            }
        };
        checkpoint_worker_task_on(
            &mut transaction,
            WorkerTask::RequestMetadataConsumer,
            WorkerTaskCheckpointOutcome::Success,
            false,
        )
        .await?;
        transaction.commit().await?;
        Ok(health)
    }

    pub async fn request_metadata_consumer_health(
        &self,
    ) -> Result<Option<ConsumerHealth>, PersistenceError> {
        let row = sqlx::query!(
            "SELECT pending_events, lag_events, oldest_pending_at, checked_at \
             FROM request_metadata_consumer_health WHERE singleton",
        )
        .fetch_optional(self.pool())
        .await?;
        row.map(|row| {
            Ok(ConsumerHealth {
                pending_events: u64::try_from(row.pending_events)
                    .map_err(|_| PersistenceError::InvalidRequestMetadataGap)?,
                lag_events: u64::try_from(row.lag_events)
                    .map_err(|_| PersistenceError::InvalidRequestMetadataGap)?,
                oldest_pending_at: row.oldest_pending_at,
                checked_at: row.checked_at,
            })
        })
        .transpose()
    }

    pub async fn request_metadata_consumer_status(
        &self,
        now: DateTime<Utc>,
    ) -> Result<ConsumerStatus, PersistenceError> {
        Ok(ConsumerStatus::from_health(
            self.request_metadata_consumer_health().await?,
            now,
        ))
    }

    /// Lists metadata-only gateway process epochs for incident review. The
    /// cursor is ordered by the last durable checkpoint and UUIDv7 epoch ID.
    pub async fn request_metadata_gateway_epochs(
        &self,
        state: Option<GatewayEpochState>,
        cursor: Option<&Timestamp>,
        limit: u16,
    ) -> Result<Page<GatewayEpochRecord>, Error> {
        let page_size = limit.clamp(1, 200);
        let mut query = request_metadata_gateway_epochs_query(state, cursor, page_size);
        let rows = query
            .build_query_as::<RequestMetadataGatewayEpochRow>()
            .fetch_all(self.pool())
            .await?;
        let items = rows
            .into_iter()
            .map(request_metadata_gateway_epoch_from_row)
            .collect::<Result<Vec<_>, _>>()?;
        let (items, next_cursor) = split_page(items, usize::from(page_size), |item| {
            Timestamp {
                at: item.updated_at,
                id: item.process_epoch,
            }
            .encode()
        });
        Ok(Page { items, next_cursor })
    }
}

fn request_metadata_gateway_epochs_query(
    state: Option<GatewayEpochState>,
    cursor: Option<&Timestamp>,
    page_size: u16,
) -> QueryBuilder<Postgres> {
    let mut query = QueryBuilder::<Postgres>::new(
        "SELECT gateway_instance, process_epoch, started_at, updated_at, accepted, persisted, \
                    dropped, abandoned, retrying, writer_closed, gracefully_closed_at, \
                    stale_detected_at, acknowledged_at, acknowledged_by, uncertainty_gap_id, \
                    CASE WHEN stale_detected_at IS NOT NULL \
                         THEN GREATEST(accepted - persisted - abandoned, 0) ELSE 0 END \
                      AS uncertain_lower_bound \
             FROM request_metadata_gateway_epochs WHERE true",
    );
    match state {
        Some(GatewayEpochState::Open) => {
            query.push(" AND gracefully_closed_at IS NULL AND stale_detected_at IS NULL");
        }
        Some(GatewayEpochState::GracefullyClosed) => {
            query.push(" AND gracefully_closed_at IS NOT NULL");
        }
        Some(GatewayEpochState::Unresolved) => {
            query.push(" AND stale_detected_at IS NOT NULL AND acknowledged_at IS NULL");
        }
        Some(GatewayEpochState::Acknowledged) => {
            query.push(" AND stale_detected_at IS NOT NULL AND acknowledged_at IS NOT NULL");
        }
        None => {}
    }
    if let Some(cursor) = cursor {
        query.push(" AND (updated_at, process_epoch) < (");
        query.push_bind(cursor.at);
        query.push(", ");
        query.push_bind(cursor.id);
        query.push(")");
    }
    query.push(" ORDER BY updated_at DESC, process_epoch DESC LIMIT ");
    query.push_bind(i64::from(page_size) + 1);
    query
}

#[derive(Debug, FromRow)]
struct RequestMetadataGatewayEpochRow {
    gateway_instance: String,
    process_epoch: uuid::Uuid,
    started_at: DateTime<Utc>,
    updated_at: DateTime<Utc>,
    accepted: i64,
    persisted: i64,
    dropped: i64,
    abandoned: i64,
    retrying: bool,
    writer_closed: bool,
    gracefully_closed_at: Option<DateTime<Utc>>,
    stale_detected_at: Option<DateTime<Utc>>,
    acknowledged_at: Option<DateTime<Utc>>,
    acknowledged_by: Option<uuid::Uuid>,
    uncertainty_gap_id: Option<uuid::Uuid>,
    uncertain_lower_bound: i64,
}

fn request_metadata_gateway_epoch_from_row(
    row: RequestMetadataGatewayEpochRow,
) -> Result<GatewayEpochRecord, Error> {
    let gracefully_closed_at: Option<DateTime<Utc>> = row.gracefully_closed_at;
    let stale_detected_at: Option<DateTime<Utc>> = row.stale_detected_at;
    let acknowledged_at: Option<DateTime<Utc>> = row.acknowledged_at;
    let state = if gracefully_closed_at.is_some() {
        GatewayEpochState::GracefullyClosed
    } else if stale_detected_at.is_some() && acknowledged_at.is_some() {
        GatewayEpochState::Acknowledged
    } else if stale_detected_at.is_some() {
        GatewayEpochState::Unresolved
    } else {
        GatewayEpochState::Open
    };
    let checked_count = |value| {
        u64::try_from(value)
            .map_err(|_| Error::Persistence(PersistenceError::InvalidRequestMetadataGap))
    };
    Ok(GatewayEpochRecord {
        gateway_instance: row.gateway_instance,
        process_epoch: row.process_epoch,
        state,
        started_at: row.started_at,
        updated_at: row.updated_at,
        accepted: checked_count(row.accepted)?,
        persisted: checked_count(row.persisted)?,
        dropped: checked_count(row.dropped)?,
        abandoned: checked_count(row.abandoned)?,
        uncertain_event_lower_bound: checked_count(row.uncertain_lower_bound)?,
        retrying: row.retrying,
        writer_closed: row.writer_closed,
        gracefully_closed_at,
        stale_detected_at,
        acknowledged_at,
        acknowledged_by: row.acknowledged_by,
        uncertainty_gap_id: row.uncertainty_gap_id,
    })
}

#[cfg(test)]
mod tests {
    use uuid::Uuid;

    use super::*;

    fn epoch_row() -> RequestMetadataGatewayEpochRow {
        let started_at = "2026-08-01T10:00:00Z".parse().unwrap();
        RequestMetadataGatewayEpochRow {
            gateway_instance: "gateway-a".to_owned(),
            process_epoch: Uuid::now_v7(),
            started_at,
            updated_at: started_at + chrono::Duration::seconds(30),
            accepted: 11,
            persisted: 7,
            dropped: 2,
            abandoned: 1,
            retrying: true,
            writer_closed: false,
            gracefully_closed_at: None,
            stale_detected_at: None,
            acknowledged_at: None,
            acknowledged_by: Some(Uuid::now_v7()),
            uncertainty_gap_id: Some(Uuid::now_v7()),
            uncertain_lower_bound: 1,
        }
    }

    #[test]
    fn gateway_epoch_rows_classify_lifecycle_state_and_preserve_evidence() {
        type Mutation = fn(&mut RequestMetadataGatewayEpochRow);
        let cases: [(Mutation, GatewayEpochState); 5] = [
            (|_| {}, GatewayEpochState::Open),
            (
                |row| row.acknowledged_at = Some(row.updated_at),
                GatewayEpochState::Open,
            ),
            (
                |row| row.stale_detected_at = Some(row.updated_at),
                GatewayEpochState::Unresolved,
            ),
            (
                |row| {
                    row.stale_detected_at = Some(row.updated_at);
                    row.acknowledged_at = Some(row.updated_at);
                },
                GatewayEpochState::Acknowledged,
            ),
            (
                |row| {
                    row.gracefully_closed_at = Some(row.updated_at);
                    row.stale_detected_at = Some(row.updated_at);
                    row.acknowledged_at = Some(row.updated_at);
                },
                GatewayEpochState::GracefullyClosed,
            ),
        ];

        for (mutate, expected_state) in cases {
            let mut row = epoch_row();
            let expected_epoch = row.process_epoch;
            let expected_updated_at = row.updated_at;
            mutate(&mut row);
            let record = request_metadata_gateway_epoch_from_row(row).unwrap();

            assert_eq!(record.state, expected_state);
            assert_eq!(record.gateway_instance, "gateway-a");
            assert_eq!(record.process_epoch, expected_epoch);
            assert_eq!(record.updated_at, expected_updated_at);
            assert_eq!(
                (
                    record.accepted,
                    record.persisted,
                    record.dropped,
                    record.abandoned,
                    record.uncertain_event_lower_bound,
                ),
                (11, 7, 2, 1, 1)
            );
            assert!(record.retrying);
            assert!(!record.writer_closed);
        }
    }

    #[test]
    fn gateway_epoch_rows_reject_every_negative_count() {
        type Mutation = fn(&mut RequestMetadataGatewayEpochRow);
        let invalidators: [Mutation; 5] = [
            |row| row.accepted = -1,
            |row| row.persisted = -1,
            |row| row.dropped = -1,
            |row| row.abandoned = -1,
            |row| row.uncertain_lower_bound = -1,
        ];

        for invalidate in invalidators {
            let mut row = epoch_row();
            invalidate(&mut row);
            assert!(matches!(
                request_metadata_gateway_epoch_from_row(row),
                Err(Error::Persistence(
                    PersistenceError::InvalidRequestMetadataGap
                ))
            ));
        }
    }

    #[test]
    fn gateway_epoch_query_applies_each_state_and_cursor_clause() {
        for (state, clause) in [
            (
                GatewayEpochState::Open,
                "gracefully_closed_at IS NULL AND stale_detected_at IS NULL",
            ),
            (
                GatewayEpochState::GracefullyClosed,
                "gracefully_closed_at IS NOT NULL",
            ),
            (
                GatewayEpochState::Unresolved,
                "stale_detected_at IS NOT NULL AND acknowledged_at IS NULL",
            ),
            (
                GatewayEpochState::Acknowledged,
                "stale_detected_at IS NOT NULL AND acknowledged_at IS NOT NULL",
            ),
        ] {
            let query = request_metadata_gateway_epochs_query(Some(state), None, 25);
            let sql = query.sql();
            assert!(
                sql.as_str().contains(clause),
                "missing {clause:?} in {sql:?}"
            );
        }

        let cursor = Timestamp {
            at: "2026-08-01T10:00:00Z".parse().unwrap(),
            id: Uuid::now_v7(),
        };
        let query = request_metadata_gateway_epochs_query(None, Some(&cursor), 25);
        let sql = query.sql();
        let sql = sql.as_str();
        assert!(sql.contains("(updated_at, process_epoch) < ("));
        assert!(sql.contains("ORDER BY updated_at DESC, process_epoch DESC LIMIT"));
        assert!(!sql.contains("stale_detected_at IS NOT NULL AND acknowledged_at"));
    }
}
