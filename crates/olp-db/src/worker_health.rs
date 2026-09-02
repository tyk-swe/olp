use chrono::{DateTime, Utc};
use sqlx::PgConnection;

use crate::{error::Error, store::Store};

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum WorkerTask {
    RuntimeOutbox,
    RequestMetadataConsumer,
    Maintenance,
    CostReconciliation,
    RequestMetadataGatewayEpochDetection,
}

impl WorkerTask {
    pub const ALL: [Self; 5] = [
        Self::RuntimeOutbox,
        Self::RequestMetadataConsumer,
        Self::Maintenance,
        Self::CostReconciliation,
        Self::RequestMetadataGatewayEpochDetection,
    ];

    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::RuntimeOutbox => "runtime_outbox",
            Self::RequestMetadataConsumer => "request_metadata_consumer",
            Self::Maintenance => "maintenance",
            Self::CostReconciliation => "cost_reconciliation",
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
            Self::Maintenance | Self::CostReconciliation => 180,
        }
    }

    fn parse(value: &str) -> Result<Self, Error> {
        match value {
            "runtime_outbox" => Ok(Self::RuntimeOutbox),
            "request_metadata_consumer" => Ok(Self::RequestMetadataConsumer),
            "maintenance" => Ok(Self::Maintenance),
            "cost_reconciliation" => Ok(Self::CostReconciliation),
            "request_metadata_gateway_epoch_detection" => {
                Ok(Self::RequestMetadataGatewayEpochDetection)
            }
            _ => Err(Error::InvalidStoredValue("worker task")),
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
    pub fn current_for(&self, expected_tasks: &[WorkerTask]) -> bool {
        self.tasks
            .iter()
            .filter(|task| expected_tasks.contains(&task.task))
            .all(|task| task.state == WorkerTaskState::Healthy)
    }

    #[must_use]
    pub fn stale_tasks_for(&self, expected_tasks: &[WorkerTask]) -> u64 {
        self.tasks
            .iter()
            .filter(|task| expected_tasks.contains(&task.task))
            .filter(|task| task.state == WorkerTaskState::Stale)
            .count()
            .try_into()
            .unwrap_or(u64::MAX)
    }

    #[must_use]
    pub fn unknown_tasks_for(&self, expected_tasks: &[WorkerTask]) -> u64 {
        self.tasks
            .iter()
            .filter(|task| expected_tasks.contains(&task.task))
            .filter(|task| task.state == WorkerTaskState::Unknown)
            .count()
            .try_into()
            .unwrap_or(u64::MAX)
    }

    #[must_use]
    pub fn last_progress_at(&self) -> Option<DateTime<Utc>> {
        self.last_progress_at_for(&WorkerTask::ALL)
    }

    #[must_use]
    pub fn last_progress_at_for(&self, expected_tasks: &[WorkerTask]) -> Option<DateTime<Utc>> {
        self.tasks
            .iter()
            .filter(|task| expected_tasks.contains(&task.task))
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
    fn checked(self) -> Result<[i64; 12], Error> {
        let checked = |value| i64::try_from(value).map_err(|_| Error::InvalidWorkerHealth);
        Ok([
            checked(self.request_metadata_reclaimed)?,
            checked(self.request_metadata_recovered)?,
            checked(self.request_metadata_duplicates)?,
            checked(self.request_metadata_processed)?,
            checked(self.runtime_outbox_attempts)?,
            checked(self.runtime_outbox_retry_scheduled)?,
            checked(self.runtime_outbox_repeated_attempts)?,
            checked(self.runtime_outbox_published)?,
            checked(self.runtime_outbox_duplicate_publications)?,
            checked(self.runtime_outbox_abandoned_ownership)?,
            checked(self.runtime_outbox_abandoned_claims)?,
            checked(self.runtime_outbox_failed_takeovers)?,
        ])
    }
}

pub(crate) async fn increment_worker_counters_on(
    connection: &mut PgConnection,
    deltas: WorkerCounterDeltas,
) -> Result<(), Error> {
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
) -> Result<(), Error> {
    let (success, successes, failures, skipped) = worker_task_checkpoint_values(outcome);
    // Callers that also touch outbox health or counters must keep the order:
    // runtime_outbox_health, async_worker_counters, then worker_task_health.
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

impl Store {
    pub async fn report_worker_task_checkpoint(
        &self,
        task: WorkerTask,
        outcome: WorkerTaskCheckpointOutcome,
        progress: bool,
    ) -> Result<(), Error> {
        let mut transaction = self.pool().begin().await?;
        checkpoint_worker_task_on(&mut transaction, task, outcome, progress).await?;
        transaction.commit().await?;
        Ok(())
    }

    pub async fn report_request_metadata_consumer_activity(
        &self,
        activity: RequestMetadataConsumerActivity,
    ) -> Result<(), Error> {
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

    pub async fn worker_recovery_counters(&self) -> Result<WorkerRecoveryCounters, Error> {
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
        let checked = |value| u64::try_from(value).map_err(|_| Error::InvalidWorkerHealth);
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

    /// Reports per-task freshness. Ages are computed by the database against
    /// `clock_timestamp()`, the same clock the workers stamp their checkpoints
    /// with. Subtracting a caller's wall clock instead would turn host clock
    /// skew between the API and worker processes into false staleness (or a
    /// dead worker reported as freshly successful).
    pub async fn worker_task_health(&self) -> Result<WorkerTaskHealthSummary, Error> {
        let rows = sqlx::query!(
            "SELECT task, checked_at, last_success_at, last_progress_at, \
                    successes_total, failures_total, skipped_total, \
                    GREATEST(0, floor(extract(epoch FROM clock_timestamp() - checked_at)))::bigint \
                      AS \"heartbeat_age_seconds!\", \
                    CASE WHEN last_success_at IS NULL THEN NULL ELSE \
                      GREATEST(0, floor(extract(epoch FROM clock_timestamp() - last_success_at)))::bigint \
                    END AS last_success_age_seconds \
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
            let heartbeat_age_seconds = checked_age(row.heartbeat_age_seconds)?;
            let last_success_age_seconds =
                row.last_success_age_seconds.map(checked_age).transpose()?;
            let state = worker_task_state(task, last_success_age_seconds);
            tasks.push(WorkerTaskStatus {
                task,
                state,
                checked_at: Some(row.checked_at),
                last_success_at: row.last_success_at,
                last_progress_at: row.last_progress_at,
                heartbeat_age_seconds: Some(heartbeat_age_seconds),
                last_success_age_seconds,
                successes_total: u64::try_from(row.successes_total)
                    .map_err(|_| Error::InvalidWorkerHealth)?,
                failures_total: u64::try_from(row.failures_total)
                    .map_err(|_| Error::InvalidWorkerHealth)?,
                skipped_total: u64::try_from(row.skipped_total)
                    .map_err(|_| Error::InvalidWorkerHealth)?,
            });
        }
        Ok(WorkerTaskHealthSummary { tasks })
    }
}

fn worker_task_checkpoint_values(outcome: WorkerTaskCheckpointOutcome) -> (bool, i64, i64, i64) {
    match outcome {
        WorkerTaskCheckpointOutcome::Success => (true, 1, 0, 0),
        WorkerTaskCheckpointOutcome::Failure => (false, 0, 1, 0),
        WorkerTaskCheckpointOutcome::Skipped => (false, 0, 0, 1),
    }
}

fn worker_task_state(task: WorkerTask, last_success_age_seconds: Option<u64>) -> WorkerTaskState {
    if last_success_age_seconds
        .is_some_and(|age| age <= u64::try_from(task.stale_after_seconds()).unwrap_or(0))
    {
        WorkerTaskState::Healthy
    } else {
        WorkerTaskState::Stale
    }
}

fn checked_age(seconds: i64) -> Result<u64, Error> {
    u64::try_from(seconds).map_err(|_| Error::InvalidWorkerHealth)
}

#[cfg(test)]
mod tests {
    use chrono::TimeZone as _;

    use super::*;

    fn task_status(
        task: WorkerTask,
        state: WorkerTaskState,
        last_progress_at: Option<DateTime<Utc>>,
    ) -> WorkerTaskStatus {
        WorkerTaskStatus {
            task,
            state,
            checked_at: None,
            last_success_at: None,
            last_progress_at,
            heartbeat_age_seconds: None,
            last_success_age_seconds: None,
            successes_total: 0,
            failures_total: 0,
            skipped_total: 0,
        }
    }

    #[test]
    fn summary_filters_expected_tasks_for_current_counts_and_progress() {
        let now = Utc.timestamp_opt(1_000, 0).unwrap();
        let summary = WorkerTaskHealthSummary {
            tasks: vec![
                task_status(
                    WorkerTask::RuntimeOutbox,
                    WorkerTaskState::Healthy,
                    Some(now),
                ),
                task_status(
                    WorkerTask::RequestMetadataConsumer,
                    WorkerTaskState::Unknown,
                    Some(now - chrono::Duration::seconds(10)),
                ),
                task_status(WorkerTask::Maintenance, WorkerTaskState::Stale, None),
            ],
        };

        assert!(!summary.current_for(&WorkerTask::ALL));
        assert!(summary.current_for(&[WorkerTask::RuntimeOutbox]));
        assert!(!summary.current_for(&[WorkerTask::RuntimeOutbox, WorkerTask::Maintenance]));
        assert_eq!(summary.stale_tasks_for(&[WorkerTask::Maintenance]), 1);
        assert_eq!(
            summary.unknown_tasks_for(&[WorkerTask::RequestMetadataConsumer]),
            1
        );
        assert_eq!(
            summary.last_progress_at_for(&[
                WorkerTask::RuntimeOutbox,
                WorkerTask::RequestMetadataConsumer
            ]),
            Some(now)
        );
    }

    #[test]
    fn unknown_summary_marks_every_fixed_task_unknown() {
        let summary = WorkerTaskHealthSummary::unknown();

        assert_eq!(summary.tasks.len(), WorkerTask::ALL.len());
        assert!(!summary.current_for(&WorkerTask::ALL));
        assert_eq!(summary.stale_tasks_for(&WorkerTask::ALL), 0);
        assert_eq!(
            summary.unknown_tasks_for(&WorkerTask::ALL),
            WorkerTask::ALL.len() as u64
        );
        assert_eq!(summary.last_progress_at(), None);
    }

    #[test]
    fn checked_age_rejects_a_negative_database_age() {
        // SQL clamps at zero, so a negative age means the reader and the
        // stored checkpoint disagree about the shape of the row, not that a
        // timestamp is in the future.
        assert_eq!(checked_age(7).unwrap(), 7);
        assert_eq!(checked_age(0).unwrap(), 0);
        assert!(matches!(checked_age(-1), Err(Error::InvalidWorkerHealth)));
    }

    #[test]
    fn worker_task_names_thresholds_and_states_are_closed() {
        for (task, name, stale_after) in [
            (WorkerTask::RuntimeOutbox, "runtime_outbox", 20),
            (
                WorkerTask::RequestMetadataConsumer,
                "request_metadata_consumer",
                20,
            ),
            (WorkerTask::Maintenance, "maintenance", 180),
            (WorkerTask::CostReconciliation, "cost_reconciliation", 180),
            (
                WorkerTask::RequestMetadataGatewayEpochDetection,
                "request_metadata_gateway_epoch_detection",
                20,
            ),
        ] {
            assert_eq!(task.as_str(), name);
            assert_eq!(task.stale_after_seconds(), stale_after);
            assert_eq!(WorkerTask::parse(name).unwrap(), task);
        }
        assert!(WorkerTask::parse("unexpected").is_err());

        for (state, name) in [
            (WorkerTaskState::Unknown, "unknown"),
            (WorkerTaskState::Healthy, "healthy"),
            (WorkerTaskState::Stale, "stale"),
        ] {
            assert_eq!(state.as_str(), name);
        }
    }

    #[test]
    fn checkpoint_and_health_state_boundaries_are_exact() {
        for (outcome, expected) in [
            (WorkerTaskCheckpointOutcome::Success, (true, 1, 0, 0)),
            (WorkerTaskCheckpointOutcome::Failure, (false, 0, 1, 0)),
            (WorkerTaskCheckpointOutcome::Skipped, (false, 0, 0, 1)),
        ] {
            assert_eq!(worker_task_checkpoint_values(outcome), expected);
        }

        for task in WorkerTask::ALL {
            let threshold = u64::try_from(task.stale_after_seconds()).unwrap();
            assert_eq!(
                worker_task_state(task, Some(threshold)),
                WorkerTaskState::Healthy
            );
            assert_eq!(
                worker_task_state(task, Some(threshold + 1)),
                WorkerTaskState::Stale
            );
            assert_eq!(worker_task_state(task, None), WorkerTaskState::Stale);
        }
    }

    #[test]
    fn counter_deltas_preserve_binding_order_and_reject_overflow() {
        let deltas = WorkerCounterDeltas {
            request_metadata_reclaimed: 1,
            request_metadata_recovered: 2,
            request_metadata_duplicates: 3,
            request_metadata_processed: 4,
            runtime_outbox_attempts: 5,
            runtime_outbox_retry_scheduled: 6,
            runtime_outbox_repeated_attempts: 7,
            runtime_outbox_published: 8,
            runtime_outbox_duplicate_publications: 9,
            runtime_outbox_abandoned_ownership: 10,
            runtime_outbox_abandoned_claims: 11,
            runtime_outbox_failed_takeovers: 12,
        };
        assert_eq!(
            deltas.checked().unwrap(),
            [1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12]
        );

        type Mutation = fn(&mut WorkerCounterDeltas);
        let invalidators: [Mutation; 12] = [
            |value| value.request_metadata_reclaimed = u64::MAX,
            |value| value.request_metadata_recovered = u64::MAX,
            |value| value.request_metadata_duplicates = u64::MAX,
            |value| value.request_metadata_processed = u64::MAX,
            |value| value.runtime_outbox_attempts = u64::MAX,
            |value| value.runtime_outbox_retry_scheduled = u64::MAX,
            |value| value.runtime_outbox_repeated_attempts = u64::MAX,
            |value| value.runtime_outbox_published = u64::MAX,
            |value| value.runtime_outbox_duplicate_publications = u64::MAX,
            |value| value.runtime_outbox_abandoned_ownership = u64::MAX,
            |value| value.runtime_outbox_abandoned_claims = u64::MAX,
            |value| value.runtime_outbox_failed_takeovers = u64::MAX,
        ];
        for invalidate in invalidators {
            let mut value = WorkerCounterDeltas::default();
            invalidate(&mut value);
            assert!(matches!(value.checked(), Err(Error::InvalidWorkerHealth)));
        }
    }

    #[test]
    fn consumer_activity_is_empty_only_when_every_counter_is_zero() {
        assert!(RequestMetadataConsumerActivity::default().is_empty());
        for activity in [
            RequestMetadataConsumerActivity {
                reclaimed: 1,
                ..Default::default()
            },
            RequestMetadataConsumerActivity {
                recovered: 1,
                ..Default::default()
            },
            RequestMetadataConsumerActivity {
                duplicates: 1,
                ..Default::default()
            },
            RequestMetadataConsumerActivity {
                processed: 1,
                ..Default::default()
            },
        ] {
            assert!(!activity.is_empty());
        }
    }
}
