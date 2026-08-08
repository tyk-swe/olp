use chrono::{DateTime, Utc};
use sqlx::PgConnection;

use crate::{PersistenceError, PgStore};

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum WorkerTask {
    RuntimeOutbox,
    RequestMetadataConsumer,
    Maintenance,
    RequestMetadataGatewayEpochDetection,
}

impl WorkerTask {
    pub const ALL: [Self; 4] = [
        Self::RuntimeOutbox,
        Self::RequestMetadataConsumer,
        Self::Maintenance,
        Self::RequestMetadataGatewayEpochDetection,
    ];

    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::RuntimeOutbox => "runtime_outbox",
            Self::RequestMetadataConsumer => "request_metadata_consumer",
            Self::Maintenance => "maintenance",
            Self::RequestMetadataGatewayEpochDetection => {
                "request_metadata_gateway_epoch_detection"
            }
        }
    }

    #[must_use]
    pub const fn stale_after_seconds(self) -> i64 {
        match self {
            // These paths normally checkpoint every five seconds.
            Self::RuntimeOutbox
            | Self::RequestMetadataConsumer
            | Self::RequestMetadataGatewayEpochDetection => 20,
            // Maintenance ticks once per minute. Three missed passes allow
            // scheduler and database jitter without hiding a stopped fleet.
            Self::Maintenance => 180,
        }
    }

    fn parse(value: &str) -> Result<Self, PersistenceError> {
        match value {
            "runtime_outbox" => Ok(Self::RuntimeOutbox),
            "request_metadata_consumer" => Ok(Self::RequestMetadataConsumer),
            "maintenance" => Ok(Self::Maintenance),
            "request_metadata_gateway_epoch_detection" => {
                Ok(Self::RequestMetadataGatewayEpochDetection)
            }
            _ => Err(PersistenceError::InvalidStoredValue("worker task")),
        }
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum WorkerTaskCheckpointOutcome {
    Success,
    Failure,
    Skipped,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum WorkerTaskState {
    Unknown,
    Healthy,
    Stale,
}

impl WorkerTaskState {
    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::Unknown => "unknown",
            Self::Healthy => "healthy",
            Self::Stale => "stale",
        }
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct WorkerTaskStatus {
    pub task: WorkerTask,
    pub state: WorkerTaskState,
    pub checked_at: Option<DateTime<Utc>>,
    pub last_success_at: Option<DateTime<Utc>>,
    pub last_progress_at: Option<DateTime<Utc>>,
    pub heartbeat_age_seconds: Option<u64>,
    pub last_success_age_seconds: Option<u64>,
    pub successes_total: u64,
    pub failures_total: u64,
    pub skipped_total: u64,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct WorkerTaskHealthSummary {
    pub tasks: Vec<WorkerTaskStatus>,
}

impl WorkerTaskHealthSummary {
    #[must_use]
    pub fn unknown() -> Self {
        Self {
            tasks: WorkerTask::ALL
                .into_iter()
                .map(|task| WorkerTaskStatus {
                    task,
                    state: WorkerTaskState::Unknown,
                    checked_at: None,
                    last_success_at: None,
                    last_progress_at: None,
                    heartbeat_age_seconds: None,
                    last_success_age_seconds: None,
                    successes_total: 0,
                    failures_total: 0,
                    skipped_total: 0,
                })
                .collect(),
        }
    }

    #[must_use]
    pub fn current(&self) -> bool {
        self.tasks
            .iter()
            .all(|task| task.state == WorkerTaskState::Healthy)
    }

    #[must_use]
    pub fn stale_tasks(&self) -> u64 {
        self.tasks
            .iter()
            .filter(|task| task.state == WorkerTaskState::Stale)
            .count()
            .try_into()
            .unwrap_or(u64::MAX)
    }

    #[must_use]
    pub fn unknown_tasks(&self) -> u64 {
        self.tasks
            .iter()
            .filter(|task| task.state == WorkerTaskState::Unknown)
            .count()
            .try_into()
            .unwrap_or(u64::MAX)
    }

    #[must_use]
    pub fn last_progress_at(&self) -> Option<DateTime<Utc>> {
        self.tasks
            .iter()
            .filter_map(|task| task.last_progress_at)
            .max()
    }
}

#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub struct RequestMetadataConsumerActivity {
    pub reclaimed: u64,
    pub recovered: u64,
    pub duplicates: u64,
    pub processed: u64,
}

impl RequestMetadataConsumerActivity {
    #[must_use]
    pub const fn is_empty(self) -> bool {
        self.reclaimed == 0 && self.recovered == 0 && self.duplicates == 0 && self.processed == 0
    }
}

#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub struct WorkerRecoveryCounters {
    pub request_metadata_reclaimed: u64,
    pub request_metadata_recovered: u64,
    pub request_metadata_duplicates: u64,
    pub request_metadata_processed: u64,
    pub runtime_outbox_attempts: u64,
    pub runtime_outbox_retry_scheduled: u64,
    pub runtime_outbox_repeated_attempts: u64,
    pub runtime_outbox_published: u64,
    pub runtime_outbox_duplicate_publications: u64,
    pub runtime_outbox_abandoned_ownership: u64,
    pub runtime_outbox_abandoned_claims: u64,
    pub runtime_outbox_failed_takeovers: u64,
}

#[derive(Clone, Copy, Debug, Default)]
pub(crate) struct WorkerCounterDeltas {
    pub request_metadata_reclaimed: u64,
    pub request_metadata_recovered: u64,
    pub request_metadata_duplicates: u64,
    pub request_metadata_processed: u64,
    pub runtime_outbox_attempts: u64,
    pub runtime_outbox_retry_scheduled: u64,
    pub runtime_outbox_repeated_attempts: u64,
    pub runtime_outbox_published: u64,
    pub runtime_outbox_duplicate_publications: u64,
    pub runtime_outbox_abandoned_ownership: u64,
    pub runtime_outbox_abandoned_claims: u64,
    pub runtime_outbox_failed_takeovers: u64,
}

impl WorkerCounterDeltas {
    fn checked(self) -> Result<[i64; 12], PersistenceError> {
        [
            self.request_metadata_reclaimed,
            self.request_metadata_recovered,
            self.request_metadata_duplicates,
            self.request_metadata_processed,
            self.runtime_outbox_attempts,
            self.runtime_outbox_retry_scheduled,
            self.runtime_outbox_repeated_attempts,
            self.runtime_outbox_published,
            self.runtime_outbox_duplicate_publications,
            self.runtime_outbox_abandoned_ownership,
            self.runtime_outbox_abandoned_claims,
            self.runtime_outbox_failed_takeovers,
        ]
        .map(|value| i64::try_from(value).map_err(|_| PersistenceError::InvalidWorkerHealth))
        .into_iter()
        .collect::<Result<Vec<_>, _>>()?
        .try_into()
        .map_err(|_| PersistenceError::InvalidWorkerHealth)
    }
}

pub(crate) async fn increment_worker_counters_on(
    connection: &mut PgConnection,
    deltas: WorkerCounterDeltas,
) -> Result<(), PersistenceError> {
    let [
        metadata_reclaimed,
        metadata_recovered,
        metadata_duplicates,
        metadata_processed,
        outbox_attempts,
        outbox_retry_scheduled,
        outbox_repeated_attempts,
        outbox_published,
        outbox_duplicate_publications,
        outbox_abandoned_ownership,
        outbox_abandoned_claims,
        outbox_failed_takeovers,
    ] = deltas.checked()?;
    sqlx::query!(
        "UPDATE async_worker_counters SET \
           request_metadata_reclaimed_total = request_metadata_reclaimed_total + $1, \
           request_metadata_recovered_total = request_metadata_recovered_total + $2, \
           request_metadata_duplicates_total = request_metadata_duplicates_total + $3, \
           request_metadata_processed_total = request_metadata_processed_total + $4, \
           runtime_outbox_attempts_total = runtime_outbox_attempts_total + $5, \
           runtime_outbox_retry_scheduled_total = runtime_outbox_retry_scheduled_total + $6, \
           runtime_outbox_repeated_attempts_total = runtime_outbox_repeated_attempts_total + $7, \
           runtime_outbox_published_total = runtime_outbox_published_total + $8, \
           runtime_outbox_duplicate_publications_total = \
             runtime_outbox_duplicate_publications_total + $9, \
           runtime_outbox_abandoned_ownership_total = \
             runtime_outbox_abandoned_ownership_total + $10, \
           runtime_outbox_abandoned_claims_total = \
             runtime_outbox_abandoned_claims_total + $11, \
           runtime_outbox_failed_takeovers_total = runtime_outbox_failed_takeovers_total + $12 \
         WHERE singleton",
        metadata_reclaimed,
        metadata_recovered,
        metadata_duplicates,
        metadata_processed,
        outbox_attempts,
        outbox_retry_scheduled,
        outbox_repeated_attempts,
        outbox_published,
        outbox_duplicate_publications,
        outbox_abandoned_ownership,
        outbox_abandoned_claims,
        outbox_failed_takeovers,
    )
    .execute(connection)
    .await?;
    Ok(())
}

pub(crate) async fn checkpoint_worker_task_on(
    connection: &mut PgConnection,
    task: WorkerTask,
    outcome: WorkerTaskCheckpointOutcome,
    progress: bool,
) -> Result<(), PersistenceError> {
    let (successes, failures, skipped) = match outcome {
        WorkerTaskCheckpointOutcome::Success => (1_i64, 0_i64, 0_i64),
        WorkerTaskCheckpointOutcome::Failure => (0_i64, 1_i64, 0_i64),
        WorkerTaskCheckpointOutcome::Skipped => (0_i64, 0_i64, 1_i64),
    };
    let success = outcome == WorkerTaskCheckpointOutcome::Success;
    sqlx::query!(
        "INSERT INTO worker_task_health \
           (task, checked_at, last_success_at, last_progress_at, \
            successes_total, failures_total, skipped_total) \
         VALUES ($1, clock_timestamp(), \
                 CASE WHEN $2 THEN clock_timestamp() ELSE NULL END, \
                 CASE WHEN $3 THEN clock_timestamp() ELSE NULL END, $4, $5, $6) \
         ON CONFLICT (task) DO UPDATE SET \
           checked_at = GREATEST(worker_task_health.checked_at, EXCLUDED.checked_at), \
           last_success_at = CASE WHEN $2 \
             THEN GREATEST(worker_task_health.last_success_at, EXCLUDED.last_success_at) \
             ELSE worker_task_health.last_success_at END, \
           last_progress_at = CASE WHEN $3 \
             THEN GREATEST(worker_task_health.last_progress_at, EXCLUDED.last_progress_at) \
             ELSE worker_task_health.last_progress_at END, \
           successes_total = worker_task_health.successes_total + EXCLUDED.successes_total, \
           failures_total = worker_task_health.failures_total + EXCLUDED.failures_total, \
           skipped_total = worker_task_health.skipped_total + EXCLUDED.skipped_total",
        task.as_str(),
        success,
        progress,
        successes,
        failures,
        skipped,
    )
    .execute(connection)
    .await?;
    Ok(())
}

impl PgStore {
    pub async fn report_worker_task_checkpoint(
        &self,
        task: WorkerTask,
        outcome: WorkerTaskCheckpointOutcome,
        progress: bool,
    ) -> Result<(), PersistenceError> {
        let mut transaction = self.pool().begin().await?;
        checkpoint_worker_task_on(&mut transaction, task, outcome, progress).await?;
        transaction.commit().await?;
        Ok(())
    }

    pub async fn report_request_metadata_consumer_activity(
        &self,
        activity: RequestMetadataConsumerActivity,
    ) -> Result<(), PersistenceError> {
        if activity.is_empty() {
            return Ok(());
        }
        let mut transaction = self.pool().begin().await?;
        increment_worker_counters_on(
            &mut transaction,
            WorkerCounterDeltas {
                request_metadata_reclaimed: activity.reclaimed,
                request_metadata_recovered: activity.recovered,
                request_metadata_duplicates: activity.duplicates,
                request_metadata_processed: activity.processed,
                ..WorkerCounterDeltas::default()
            },
        )
        .await?;
        checkpoint_worker_task_on(
            &mut transaction,
            WorkerTask::RequestMetadataConsumer,
            WorkerTaskCheckpointOutcome::Success,
            activity.reclaimed > 0 || activity.processed > 0,
        )
        .await?;
        transaction.commit().await?;
        Ok(())
    }

    pub async fn worker_recovery_counters(
        &self,
    ) -> Result<WorkerRecoveryCounters, PersistenceError> {
        let row = sqlx::query!(
            "SELECT request_metadata_reclaimed_total, request_metadata_recovered_total, \
                    request_metadata_duplicates_total, request_metadata_processed_total, \
                    runtime_outbox_attempts_total, runtime_outbox_retry_scheduled_total, \
                    runtime_outbox_repeated_attempts_total, runtime_outbox_published_total, \
                    runtime_outbox_duplicate_publications_total, \
                    runtime_outbox_abandoned_ownership_total, \
                    runtime_outbox_abandoned_claims_total, runtime_outbox_failed_takeovers_total \
             FROM async_worker_counters WHERE singleton"
        )
        .fetch_one(self.pool())
        .await?;
        let checked =
            |value| u64::try_from(value).map_err(|_| PersistenceError::InvalidWorkerHealth);
        Ok(WorkerRecoveryCounters {
            request_metadata_reclaimed: checked(row.request_metadata_reclaimed_total)?,
            request_metadata_recovered: checked(row.request_metadata_recovered_total)?,
            request_metadata_duplicates: checked(row.request_metadata_duplicates_total)?,
            request_metadata_processed: checked(row.request_metadata_processed_total)?,
            runtime_outbox_attempts: checked(row.runtime_outbox_attempts_total)?,
            runtime_outbox_retry_scheduled: checked(row.runtime_outbox_retry_scheduled_total)?,
            runtime_outbox_repeated_attempts: checked(row.runtime_outbox_repeated_attempts_total)?,
            runtime_outbox_published: checked(row.runtime_outbox_published_total)?,
            runtime_outbox_duplicate_publications: checked(
                row.runtime_outbox_duplicate_publications_total,
            )?,
            runtime_outbox_abandoned_ownership: checked(
                row.runtime_outbox_abandoned_ownership_total,
            )?,
            runtime_outbox_abandoned_claims: checked(row.runtime_outbox_abandoned_claims_total)?,
            runtime_outbox_failed_takeovers: checked(row.runtime_outbox_failed_takeovers_total)?,
        })
    }

    pub async fn worker_task_health(
        &self,
        now: DateTime<Utc>,
    ) -> Result<WorkerTaskHealthSummary, PersistenceError> {
        let rows = sqlx::query!(
            "SELECT task, checked_at, last_success_at, last_progress_at, \
                    successes_total, failures_total, skipped_total \
             FROM worker_task_health ORDER BY task"
        )
        .fetch_all(self.pool())
        .await?;
        let mut tasks = Vec::with_capacity(WorkerTask::ALL.len());
        for task in WorkerTask::ALL {
            let row = rows.iter().find(|row| row.task == task.as_str());
            let Some(row) = row else {
                tasks.push(WorkerTaskStatus {
                    task,
                    state: WorkerTaskState::Unknown,
                    checked_at: None,
                    last_success_at: None,
                    last_progress_at: None,
                    heartbeat_age_seconds: None,
                    last_success_age_seconds: None,
                    successes_total: 0,
                    failures_total: 0,
                    skipped_total: 0,
                });
                continue;
            };
            WorkerTask::parse(&row.task)?;
            let heartbeat_age_seconds = age_seconds(now, row.checked_at);
            let last_success_age_seconds = row.last_success_at.map(|at| age_seconds(now, at));
            let state = if last_success_age_seconds
                .is_some_and(|age| age <= u64::try_from(task.stale_after_seconds()).unwrap_or(0))
            {
                WorkerTaskState::Healthy
            } else {
                WorkerTaskState::Stale
            };
            tasks.push(WorkerTaskStatus {
                task,
                state,
                checked_at: Some(row.checked_at),
                last_success_at: row.last_success_at,
                last_progress_at: row.last_progress_at,
                heartbeat_age_seconds: Some(heartbeat_age_seconds),
                last_success_age_seconds,
                successes_total: u64::try_from(row.successes_total)
                    .map_err(|_| PersistenceError::InvalidWorkerHealth)?,
                failures_total: u64::try_from(row.failures_total)
                    .map_err(|_| PersistenceError::InvalidWorkerHealth)?,
                skipped_total: u64::try_from(row.skipped_total)
                    .map_err(|_| PersistenceError::InvalidWorkerHealth)?,
            });
        }
        Ok(WorkerTaskHealthSummary { tasks })
    }
}

fn age_seconds(now: DateTime<Utc>, at: DateTime<Utc>) -> u64 {
    u64::try_from(now.signed_duration_since(at).num_seconds().max(0)).unwrap_or(u64::MAX)
}
