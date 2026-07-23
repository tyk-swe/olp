use chrono::{DateTime, Utc};
use sqlx::{FromRow, Postgres, QueryBuilder};

use super::reconciliation::{RequestMetadataGatewayEpochRecord, RequestMetadataGatewayEpochState};
use crate::{
    OperationsError, OperationsPage, PersistenceError, PgStore, TimestampCursor, split_page,
};

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct RequestMetadataConsumerHealth {
    pub pending_events: u64,
    pub lag_events: u64,
    pub oldest_pending_at: Option<DateTime<Utc>>,
    pub checked_at: DateTime<Utc>,
}

/// The worker reports every five seconds. Four missed checkpoints distinguish
/// a genuinely stale consumer from ordinary scheduling and database jitter.
pub const REQUEST_METADATA_CONSUMER_STALE_AFTER_SECONDS: i64 = 20;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum RequestMetadataConsumerState {
    Unknown,
    Healthy,
    Backlogged,
    Stale,
}

impl RequestMetadataConsumerState {
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
pub struct RequestMetadataConsumerStatus {
    pub state: RequestMetadataConsumerState,
    pub pending_events: u64,
    pub lag_events: u64,
    pub oldest_pending_at: Option<DateTime<Utc>>,
    pub checked_at: Option<DateTime<Utc>>,
    pub heartbeat_age_seconds: Option<u64>,
}

impl RequestMetadataConsumerStatus {
    #[must_use]
    pub fn from_health(health: Option<RequestMetadataConsumerHealth>, now: DateTime<Utc>) -> Self {
        let Some(health) = health else {
            return Self {
                state: RequestMetadataConsumerState::Unknown,
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
            RequestMetadataConsumerState::Stale
        } else if health.pending_events > 0 || health.lag_events > 0 {
            RequestMetadataConsumerState::Backlogged
        } else {
            RequestMetadataConsumerState::Healthy
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
        matches!(self.state, RequestMetadataConsumerState::Healthy)
    }
}

impl PgStore {
    /// Checkpoints the Valkey consumer-group backlog so health and usage
    /// completeness reflect worker-side stalls, not only gateway-local queue
    /// delivery. This contains counts and timestamps only.
    pub async fn report_request_metadata_consumer_health(
        &self,
        pending_events: u64,
        lag_events: u64,
        oldest_pending_at: Option<DateTime<Utc>>,
    ) -> Result<RequestMetadataConsumerHealth, PersistenceError> {
        if (pending_events == 0) != oldest_pending_at.is_none() {
            return Err(PersistenceError::InvalidRequestMetadataGap);
        }
        let pending_events = i64::try_from(pending_events)
            .map_err(|_| PersistenceError::InvalidRequestMetadataGap)?;
        let lag_events =
            i64::try_from(lag_events).map_err(|_| PersistenceError::InvalidRequestMetadataGap)?;
        let checked_at = Utc::now();
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
             RETURNING pending_events, lag_events, oldest_pending_at, checked_at",
            pending_events,
            lag_events,
            oldest_pending_at,
            checked_at
        )
        .fetch_one(self.pool())
        .await?;
        Ok(RequestMetadataConsumerHealth {
            pending_events: u64::try_from(row.pending_events)
                .map_err(|_| PersistenceError::InvalidRequestMetadataGap)?,
            lag_events: u64::try_from(row.lag_events)
                .map_err(|_| PersistenceError::InvalidRequestMetadataGap)?,
            oldest_pending_at: row.oldest_pending_at,
            checked_at: row.checked_at,
        })
    }

    pub async fn request_metadata_consumer_health(
        &self,
    ) -> Result<Option<RequestMetadataConsumerHealth>, PersistenceError> {
        let row = sqlx::query!(
            "SELECT pending_events, lag_events, oldest_pending_at, checked_at \
             FROM request_metadata_consumer_health WHERE singleton",
        )
        .fetch_optional(self.pool())
        .await?;
        row.map(|row| {
            Ok(RequestMetadataConsumerHealth {
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
    ) -> Result<RequestMetadataConsumerStatus, PersistenceError> {
        Ok(RequestMetadataConsumerStatus::from_health(
            self.request_metadata_consumer_health().await?,
            now,
        ))
    }

    /// Lists metadata-only gateway process epochs for incident review. The
    /// cursor is ordered by the last durable checkpoint and UUIDv7 epoch ID.
    pub async fn request_metadata_gateway_epochs(
        &self,
        state: Option<RequestMetadataGatewayEpochState>,
        cursor: Option<&TimestampCursor>,
        limit: u16,
    ) -> Result<OperationsPage<RequestMetadataGatewayEpochRecord>, OperationsError> {
        let page_size = limit.clamp(1, 200);
        let mut query = QueryBuilder::<Postgres>::new(
            "SELECT gateway_instance, process_epoch, started_at, updated_at, accepted, persisted, \
                    dropped, abandoned, retrying, writer_closed, gracefully_closed_at, \
                    stale_detected_at, acknowledged_at, acknowledged_by, \
                    CASE WHEN stale_detected_at IS NOT NULL \
                         THEN GREATEST(accepted - persisted - abandoned, 0) ELSE 0 END \
                      AS uncertain_lower_bound \
             FROM request_metadata_gateway_epochs WHERE true",
        );
        match state {
            Some(RequestMetadataGatewayEpochState::Open) => {
                query.push(" AND gracefully_closed_at IS NULL AND stale_detected_at IS NULL");
            }
            Some(RequestMetadataGatewayEpochState::GracefullyClosed) => {
                query.push(" AND gracefully_closed_at IS NOT NULL");
            }
            Some(RequestMetadataGatewayEpochState::Unresolved) => {
                query.push(" AND stale_detected_at IS NOT NULL AND acknowledged_at IS NULL");
            }
            Some(RequestMetadataGatewayEpochState::Acknowledged) => {
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
        let rows = query
            .build_query_as::<RequestMetadataGatewayEpochRow>()
            .fetch_all(self.pool())
            .await?;
        let items = rows
            .into_iter()
            .map(request_metadata_gateway_epoch_from_row)
            .collect::<Result<Vec<_>, _>>()?;
        let (items, next_cursor) = split_page(items, usize::from(page_size), |item| {
            TimestampCursor {
                at: item.updated_at,
                id: item.process_epoch,
            }
            .encode()
        });
        Ok(OperationsPage { items, next_cursor })
    }
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
    uncertain_lower_bound: i64,
}

fn request_metadata_gateway_epoch_from_row(
    row: RequestMetadataGatewayEpochRow,
) -> Result<RequestMetadataGatewayEpochRecord, OperationsError> {
    let gracefully_closed_at: Option<DateTime<Utc>> = row.gracefully_closed_at;
    let stale_detected_at: Option<DateTime<Utc>> = row.stale_detected_at;
    let acknowledged_at: Option<DateTime<Utc>> = row.acknowledged_at;
    let state = if gracefully_closed_at.is_some() {
        RequestMetadataGatewayEpochState::GracefullyClosed
    } else if stale_detected_at.is_some() && acknowledged_at.is_some() {
        RequestMetadataGatewayEpochState::Acknowledged
    } else if stale_detected_at.is_some() {
        RequestMetadataGatewayEpochState::Unresolved
    } else {
        RequestMetadataGatewayEpochState::Open
    };
    let checked_count = |value| {
        u64::try_from(value)
            .map_err(|_| OperationsError::Persistence(PersistenceError::InvalidRequestMetadataGap))
    };
    Ok(RequestMetadataGatewayEpochRecord {
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
    })
}
