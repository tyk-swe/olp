use chrono::{DateTime, Utc};
use sqlx::{Connection as _, PgConnection};
use uuid::Uuid;

use crate::{
    error::Error,
    store::Store,
    worker_health::{
        WorkerCounterDeltas, WorkerTask, WorkerTaskCheckpointOutcome, checkpoint_worker_task_on,
        increment_worker_counters_on,
    },
};

use super::OutboxRecord;

// A session-level lock is deliberately distinct from the transaction-level
// runtime compilation lock. The numeric value is the ASCII bytes "OLP_OBX".
const OUTBOX_LEADER_LOCK_ID: i64 = 0x004f_4c50_5f4f_4258;

/// The owner checkpoints every five seconds. Four missed checkpoints make a
/// stopped or wedged publication path stale without turning an ordinary
/// ownership handoff into a permanent incident.
pub const RUNTIME_OUTBOX_STALE_AFTER_SECONDS: i64 = 20;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum RuntimeOutboxState {
    Unknown,
    Healthy,
    Backlogged,
    Stale,
}

impl RuntimeOutboxState {
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
pub struct RuntimeOutboxStatus {
    pub state: RuntimeOutboxState,
    pub pending_rows: u64,
    pub oldest_pending_at: Option<DateTime<Utc>>,
    pub owner_active: bool,
    pub claimed_rows: u64,
    pub checked_at: Option<DateTime<Utc>>,
    pub heartbeat_age_seconds: Option<u64>,
    pub last_progress_at: Option<DateTime<Utc>>,
    pub last_progress_age_seconds: Option<u64>,
}

impl RuntimeOutboxStatus {
    #[must_use]
    pub const fn unknown() -> Self {
        Self {
            state: RuntimeOutboxState::Unknown,
            pending_rows: 0,
            oldest_pending_at: None,
            owner_active: false,
            claimed_rows: 0,
            checked_at: None,
            heartbeat_age_seconds: None,
            last_progress_at: None,
            last_progress_age_seconds: None,
        }
    }

    #[must_use]
    pub const fn complete(self) -> bool {
        matches!(self.state, RuntimeOutboxState::Healthy)
    }

    #[must_use]
    pub const fn ownership_abandoned(self) -> bool {
        self.owner_active && matches!(self.state, RuntimeOutboxState::Stale)
    }
}

/// Exclusive ownership of runtime-hint outbox publication.
///
/// The lock and every read/completion query use this exact PostgreSQL session.
/// It is detached from the pool before lock acquisition, so cancellation,
/// panic, and ordinary drop close the physical connection instead of ever
/// returning a possibly locked session to the pool.
pub struct RuntimeOutboxLeader {
    connection: PgConnection,
}

pub struct RuntimeOutboxLeaderContender {
    connection: PgConnection,
}

pub enum RuntimeOutboxLeadershipProbe {
    Acquired(RuntimeOutboxLeader),
    Pending(RuntimeOutboxLeaderContender),
}

impl Store {
    pub async fn acquire_runtime_outbox_leader(&self) -> Result<RuntimeOutboxLeader, Error> {
        let mut connection = self.pool().acquire().await?.detach();
        // Contenders wait in PostgreSQL instead of opening and closing a new
        // session on every poll. The raw connection cannot return to the pool,
        // including when this await is cancelled after an ambiguous acquire.
        sqlx::query!("SELECT pg_advisory_lock($1)", OUTBOX_LEADER_LOCK_ID)
            .execute(&mut connection)
            .await?;
        let mut leader = RuntimeOutboxLeader { connection };
        leader.record_acquired().await?;
        Ok(leader)
    }

    /// Attempts leadership without waiting. Worker replicas use this bounded
    /// probe so a live contender can durably report that a stale owner still
    /// holds the PostgreSQL advisory lock.
    pub async fn try_acquire_runtime_outbox_leader(
        &self,
    ) -> Result<Option<RuntimeOutboxLeader>, Error> {
        let contender = self.runtime_outbox_leader_contender().await?;
        match contender.try_acquire(self).await? {
            RuntimeOutboxLeadershipProbe::Acquired(leader) => Ok(Some(leader)),
            RuntimeOutboxLeadershipProbe::Pending(contender) => {
                contender.close().await?;
                Ok(None)
            }
        }
    }

    pub async fn runtime_outbox_leader_contender(
        &self,
    ) -> Result<RuntimeOutboxLeaderContender, Error> {
        Ok(RuntimeOutboxLeaderContender {
            connection: self.pool().acquire().await?.detach(),
        })
    }

    async fn report_runtime_outbox_contention(&self) -> Result<(), Error> {
        let mut transaction = self.pool().begin().await?;
        sqlx::query!(
            "UPDATE async_worker_counters SET \
               runtime_outbox_failed_takeovers_total = \
                 runtime_outbox_failed_takeovers_total + 1 \
             WHERE singleton AND EXISTS ( \
               SELECT 1 FROM runtime_outbox_health \
               WHERE singleton AND owner_active \
                 AND checked_at < clock_timestamp() - make_interval(secs => $1::double precision) \
             )",
            RUNTIME_OUTBOX_STALE_AFTER_SECONDS as f64,
        )
        .execute(&mut *transaction)
        .await?;
        checkpoint_worker_task_on(
            &mut transaction,
            WorkerTask::RuntimeOutbox,
            WorkerTaskCheckpointOutcome::Skipped,
            false,
        )
        .await?;
        transaction.commit().await?;
        Ok(())
    }

    pub async fn runtime_outbox_status(&self) -> Result<RuntimeOutboxStatus, Error> {
        let backlog = sqlx::query!(
            "SELECT count(*) AS \"pending_rows!\", min(created_at) AS oldest_pending_at \
             FROM transactional_outbox WHERE published_at IS NULL"
        )
        .fetch_one(self.pool())
        .await?;
        let pending_rows =
            u64::try_from(backlog.pending_rows).map_err(|_| Error::InvalidWorkerHealth)?;
        let health = sqlx::query!(
            "SELECT owner_active, claimed_rows, checked_at, last_progress_at, \
                    GREATEST(0, floor(extract(epoch FROM clock_timestamp() - checked_at)))::bigint \
                      AS \"heartbeat_age_seconds!\", \
                    CASE WHEN last_progress_at IS NULL THEN NULL ELSE \
                      GREATEST(0, floor(extract(epoch FROM clock_timestamp() - last_progress_at)))::bigint \
                    END AS last_progress_age_seconds \
             FROM runtime_outbox_health WHERE singleton"
        )
        .fetch_optional(self.pool())
        .await?;
        let Some(health) = health else {
            return Ok(RuntimeOutboxStatus {
                state: RuntimeOutboxState::Unknown,
                pending_rows,
                oldest_pending_at: backlog.oldest_pending_at,
                owner_active: false,
                claimed_rows: 0,
                checked_at: None,
                heartbeat_age_seconds: None,
                last_progress_at: None,
                last_progress_age_seconds: None,
            });
        };
        let heartbeat_age_seconds =
            u64::try_from(health.heartbeat_age_seconds).map_err(|_| Error::InvalidWorkerHealth)?;
        let last_progress_age_seconds = health
            .last_progress_age_seconds
            .map(u64::try_from)
            .transpose()
            .map_err(|_| Error::InvalidWorkerHealth)?;
        let claimed_rows =
            u64::try_from(health.claimed_rows).map_err(|_| Error::InvalidWorkerHealth)?;
        let state = if heartbeat_age_seconds
            > u64::try_from(RUNTIME_OUTBOX_STALE_AFTER_SECONDS).unwrap_or(0)
        {
            RuntimeOutboxState::Stale
        } else if pending_rows > 0 || claimed_rows > 0 {
            RuntimeOutboxState::Backlogged
        } else {
            RuntimeOutboxState::Healthy
        };
        Ok(RuntimeOutboxStatus {
            state,
            pending_rows,
            oldest_pending_at: backlog.oldest_pending_at,
            owner_active: health.owner_active,
            claimed_rows,
            checked_at: Some(health.checked_at),
            heartbeat_age_seconds: Some(heartbeat_age_seconds),
            last_progress_at: health.last_progress_at,
            last_progress_age_seconds,
        })
    }
}

impl RuntimeOutboxLeaderContender {
    pub async fn try_acquire(
        mut self,
        store: &Store,
    ) -> Result<RuntimeOutboxLeadershipProbe, Error> {
        let acquired: bool = sqlx::query_scalar!(
            "SELECT pg_try_advisory_lock($1) AS \"acquired!\"",
            OUTBOX_LEADER_LOCK_ID
        )
        .fetch_one(&mut self.connection)
        .await?;
        if !acquired {
            store.report_runtime_outbox_contention().await?;
            return Ok(RuntimeOutboxLeadershipProbe::Pending(self));
        }
        let mut leader = RuntimeOutboxLeader {
            connection: self.connection,
        };
        leader.record_acquired().await?;
        Ok(RuntimeOutboxLeadershipProbe::Acquired(leader))
    }

    pub async fn close(self) -> Result<(), Error> {
        self.connection.close().await?;
        Ok(())
    }
}

impl RuntimeOutboxLeader {
    async fn record_acquired(&mut self) -> Result<(), Error> {
        let mut transaction = self.connection.begin().await?;
        let previous = sqlx::query!(
            "SELECT owner_active, claimed_rows FROM runtime_outbox_health \
             WHERE singleton FOR UPDATE"
        )
        .fetch_optional(&mut *transaction)
        .await?;
        let abandoned = previous.as_ref().is_some_and(|row| row.owner_active);
        let abandoned_claims = previous
            .filter(|row| row.owner_active)
            .map_or(0_i64, |row| row.claimed_rows);
        let abandoned_claims =
            u64::try_from(abandoned_claims).map_err(|_| Error::InvalidWorkerHealth)?;
        increment_worker_counters_on(
            &mut transaction,
            WorkerCounterDeltas {
                runtime_outbox_abandoned_ownership: u64::from(abandoned),
                runtime_outbox_abandoned_claims: abandoned_claims,
                ..WorkerCounterDeltas::default()
            },
        )
        .await?;
        // Taking over from a dead owner is not publishing progress. Advancing
        // last_progress_at here would make a publisher that crash-loops before
        // it ever publishes look continuously healthy, and no-progress alerts
        // would never fire. Only a real publication moves that clock.
        sqlx::query!(
            "INSERT INTO runtime_outbox_health \
               (singleton, owner_active, claimed_rows, checked_at, last_progress_at) \
             VALUES (true, true, 0, clock_timestamp(), NULL) \
             ON CONFLICT (singleton) DO UPDATE SET \
               owner_active = true, claimed_rows = 0, checked_at = EXCLUDED.checked_at, \
               last_progress_at = runtime_outbox_health.last_progress_at"
        )
        .execute(&mut *transaction)
        .await?;
        checkpoint_worker_task_on(
            &mut transaction,
            WorkerTask::RuntimeOutbox,
            WorkerTaskCheckpointOutcome::Success,
            false,
        )
        .await?;
        transaction.commit().await?;
        Ok(())
    }

    pub async fn heartbeat(&mut self) -> Result<(), Error> {
        let mut transaction = self.connection.begin().await?;
        let result = sqlx::query!(
            "UPDATE runtime_outbox_health SET owner_active = true, checked_at = clock_timestamp() \
             WHERE singleton"
        )
        .execute(&mut *transaction)
        .await?;
        if result.rows_affected() != 1 {
            return Err(Error::InvalidWorkerHealth);
        }
        checkpoint_worker_task_on(
            &mut transaction,
            WorkerTask::RuntimeOutbox,
            WorkerTaskCheckpointOutcome::Success,
            false,
        )
        .await?;
        transaction.commit().await?;
        Ok(())
    }

    pub async fn pending(&mut self, limit: u16) -> Result<Vec<OutboxRecord>, Error> {
        let rows = sqlx::query!(
            "SELECT id, topic, aggregate_id, payload, created_at \
             FROM transactional_outbox WHERE published_at IS NULL \
             ORDER BY created_at, id LIMIT $1",
            i64::from(limit.clamp(1, 1_000))
        )
        .fetch_all(&mut self.connection)
        .await?;
        Ok(rows
            .into_iter()
            .map(|row| OutboxRecord {
                id: row.id,
                topic: row.topic,
                aggregate_id: row.aggregate_id,
                payload: row.payload,
                created_at: row.created_at,
            })
            .collect())
    }

    /// Marks the row as actively claimed and durably increments its attempt
    /// count before any ambiguous Valkey side effect can occur.
    pub async fn begin_publication(&mut self, id: Uuid) -> Result<Option<u64>, Error> {
        let mut transaction = self.connection.begin().await?;
        let attempt = sqlx::query_scalar!(
            "UPDATE transactional_outbox \
             SET publication_attempts = publication_attempts + 1 \
             WHERE id = $1 AND published_at IS NULL \
             RETURNING publication_attempts",
            id,
        )
        .fetch_optional(&mut *transaction)
        .await?;
        let Some(attempt) = attempt else {
            transaction.commit().await?;
            return Ok(None);
        };
        let attempt = u64::try_from(attempt).map_err(|_| Error::InvalidWorkerHealth)?;
        sqlx::query!(
            "UPDATE runtime_outbox_health \
             SET owner_active = true, claimed_rows = 1, checked_at = clock_timestamp() \
             WHERE singleton"
        )
        .execute(&mut *transaction)
        .await?;
        increment_worker_counters_on(
            &mut transaction,
            WorkerCounterDeltas {
                runtime_outbox_attempts: 1,
                runtime_outbox_repeated_attempts: u64::from(attempt > 1),
                ..WorkerCounterDeltas::default()
            },
        )
        .await?;
        checkpoint_worker_task_on(
            &mut transaction,
            WorkerTask::RuntimeOutbox,
            WorkerTaskCheckpointOutcome::Success,
            false,
        )
        .await?;
        transaction.commit().await?;
        Ok(Some(attempt))
    }

    /// Records an ambiguous or failed side effect while leaving the durable
    /// outbox row pending for the next attempt.
    pub async fn record_publication_retry(&mut self) -> Result<(), Error> {
        let mut transaction = self.connection.begin().await?;
        sqlx::query!(
            "UPDATE runtime_outbox_health SET owner_active = true, claimed_rows = 0, \
                    checked_at = clock_timestamp() WHERE singleton"
        )
        .execute(&mut *transaction)
        .await?;
        increment_worker_counters_on(
            &mut transaction,
            WorkerCounterDeltas {
                runtime_outbox_retry_scheduled: 1,
                ..WorkerCounterDeltas::default()
            },
        )
        .await?;
        checkpoint_worker_task_on(
            &mut transaction,
            WorkerTask::RuntimeOutbox,
            WorkerTaskCheckpointOutcome::Success,
            false,
        )
        .await?;
        transaction.commit().await?;
        Ok(())
    }

    /// Marks a successful publish through the same live session that owns the
    /// advisory lock. SQLx PostgreSQL connections do not reconnect in place:
    /// once that session is lost, this completion cannot reach PostgreSQL and
    /// a replacement leader may safely retry the still-unpublished row.
    pub async fn mark_published(&mut self, id: Uuid) -> Result<bool, Error> {
        let mut transaction = self.connection.begin().await?;
        let result = sqlx::query!(
            "UPDATE transactional_outbox SET published_at = clock_timestamp() \
             WHERE id = $1 AND published_at IS NULL",
            id
        )
        .execute(&mut *transaction)
        .await?;
        let published = result.rows_affected() == 1;
        sqlx::query!(
            "UPDATE runtime_outbox_health SET owner_active = true, claimed_rows = 0, \
                    checked_at = clock_timestamp(), \
                    last_progress_at = CASE WHEN $1 \
                      THEN GREATEST(last_progress_at, clock_timestamp()) \
                      ELSE last_progress_at END \
             WHERE singleton",
            published
        )
        .execute(&mut *transaction)
        .await?;
        increment_worker_counters_on(
            &mut transaction,
            WorkerCounterDeltas {
                runtime_outbox_published: u64::from(published),
                runtime_outbox_duplicate_publications: u64::from(!published),
                ..WorkerCounterDeltas::default()
            },
        )
        .await?;
        checkpoint_worker_task_on(
            &mut transaction,
            WorkerTask::RuntimeOutbox,
            WorkerTaskCheckpointOutcome::Success,
            published,
        )
        .await?;
        transaction.commit().await?;
        Ok(published)
    }

    /// Releases leadership on clean shutdown and closes the physical session.
    /// Error and panic paths drop the detached connection and close its socket.
    pub async fn release(mut self) -> Result<(), Error> {
        let mut transaction = self.connection.begin().await?;
        sqlx::query!(
            "UPDATE runtime_outbox_health SET owner_active = false, claimed_rows = 0, \
                    checked_at = clock_timestamp() WHERE singleton"
        )
        .execute(&mut *transaction)
        .await?;
        checkpoint_worker_task_on(
            &mut transaction,
            WorkerTask::RuntimeOutbox,
            WorkerTaskCheckpointOutcome::Success,
            false,
        )
        .await?;
        let released = sqlx::query_scalar!(
            "SELECT pg_advisory_unlock($1) AS \"released!\"",
            OUTBOX_LEADER_LOCK_ID
        )
        .fetch_one(&mut *transaction)
        .await?;
        if !released {
            return Err(Error::RuntimeOutboxLeadershipLost);
        }
        // The successor can acquire the session lock as soon as the statement
        // above completes, but its summary update waits on this transaction's
        // row lock. It therefore observes owner_active=false after a clean
        // handoff, while a disconnect before commit rolls this update back and
        // is counted as recovered ownership by the successor.
        transaction.commit().await?;
        self.connection.close().await?;
        Ok(())
    }

    #[cfg(feature = "test-util")]
    pub async fn backend_pid(&mut self) -> Result<i32, Error> {
        Ok(sqlx::query_scalar!("SELECT pg_backend_pid() AS \"pid!\"")
            .fetch_one(&mut self.connection)
            .await?)
    }
}
