package workerhealth

import (
	"context"
	"fmt"
	"github.com/jackc/pgx/v5/pgconn"
	"strings"
)

type Reporter struct {
	task   Task
	region string
}

func New(task Task, region string) Reporter { return Reporter{task: task, region: region} }

// Execer is the subset of pgx used to write health rows, so a checkpoint can
// join a caller's transaction or run on the pool.
type Execer interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

// Task names a fixed worker responsibility that checkpoints its liveness into
// worker_task_health. The values are the stored task labels.
type Task string

const (
	// TaskRequestMetadataConsumer drains the request metadata stream.
	TaskRequestMetadataConsumer Task = "request_metadata_consumer"
	// TaskMaintenance runs retention and rollup maintenance.
	TaskMaintenance Task = "maintenance"
	// TaskCostReconciliation rebuilds spend windows from persisted facts.
	TaskCostReconciliation Task = "cost_reconciliation"
	// TaskEpochDetection confirms gateway epochs that stopped checkpointing.
	TaskEpochDetection Task = "request_metadata_gateway_epoch_detection"
	// TaskMediaReconciliation advances durable media jobs toward a terminal
	// state and retires orphaned upstream objects.
	TaskMediaReconciliation Task = "media_reconciliation"
	// TaskBatchSettlement reconciles accepted batch jobs and their result usage.
	TaskBatchSettlement    Task = "batch_settlement"
	TaskExecutionDebits    Task = "execution_debits"
	TaskRetainedSettlement Task = "retained_settlement"
	// TaskNotificationDelivery evaluates budget threshold rules and delivers
	// every event notification rules subscribe their destinations to.
	TaskNotificationDelivery Task = "notification_delivery"
	// TaskManagedExportDelivery ships facts from operator-managed HTTPS sinks.
	TaskManagedExportDelivery Task = "managed_export_delivery"
	// TaskGrantRefresh refreshes grants ahead of their access tokens' expiry.
	TaskGrantRefresh Task = "grant_refresh"
	// TaskHealthProbes sends the active health probes connections opt into.
	TaskHealthProbes   Task = "health_probes"
	TaskExportDelivery Task = "export_delivery"
	// TaskBillingImport imports provider invoices and reports its liveness.
	TaskBillingImport Task = "billing_import"
	// TaskFocusExport delivers the daily FOCUS datasets to csv_gzip sinks.
	TaskFocusExport       Task = "focus_export"
	TaskCodeAllowanceRead Task = "code_allowance_read"
)

type WorkerTask struct {
	Name       Task
	StaleAfter int64
	Regional   bool
}

func WorkerTasks() []WorkerTask {
	return []WorkerTask{
		{Name: TaskRequestMetadataConsumer, StaleAfter: 20, Regional: true},
		{Name: TaskEpochDetection, StaleAfter: 20},
		{Name: TaskMediaReconciliation, StaleAfter: 20},
		{Name: TaskBatchSettlement, StaleAfter: 180},
		{Name: TaskRetainedSettlement, StaleAfter: 180},
		{Name: TaskMaintenance, StaleAfter: 180},
		{Name: TaskCostReconciliation, StaleAfter: 180, Regional: true},
		{Name: TaskExecutionDebits, StaleAfter: 180},
		{Name: TaskNotificationDelivery, StaleAfter: 180},
		{Name: TaskManagedExportDelivery, StaleAfter: 180},
		{Name: TaskGrantRefresh, StaleAfter: 20},
		{Name: TaskHealthProbes, StaleAfter: 180, Regional: true},
		{Name: TaskExportDelivery, StaleAfter: 30},
		{Name: TaskBillingImport, StaleAfter: 180},
		{Name: TaskFocusExport, StaleAfter: 180},
		{Name: TaskCodeAllowanceRead, StaleAfter: 60},
	}
}

func WorkerStaleAfter(task Task) int64 {
	for _, worker := range WorkerTasks() {
		if worker.Name == task {
			return worker.StaleAfter
		}
	}
	return 0
}

// Outcome is what one worker pass achieved.
type Outcome int

const (
	// OutcomeSuccess is a pass that completed its work.
	OutcomeSuccess Outcome = iota
	// OutcomeFailure is a pass that could not complete.
	OutcomeFailure
	// OutcomeSkipped is a pass that deliberately did nothing, for example
	// because another process holds the lock.
	OutcomeSkipped
)

// Checkpoints advance `checked_at` on every pass and move `last_success_at` or
// `last_progress_at` only when this pass earned it. GREATEST keeps a slow
// writer from moving a timestamp backwards, and the totals accumulate so a
// restart never loses the counts.
const checkpointTaskSQL = `INSERT INTO olp.worker_task_health
        (task, checked_at, last_success_at, last_progress_at,
         successes_total, failures_total, skipped_total)
    VALUES ($1, clock_timestamp(),
        CASE WHEN $2 THEN clock_timestamp() ELSE NULL END,
        CASE WHEN $3 THEN clock_timestamp() ELSE NULL END, $4, $5, $6)
    ON CONFLICT (task) DO UPDATE SET
        checked_at = GREATEST(worker_task_health.checked_at, EXCLUDED.checked_at),
        last_success_at = CASE WHEN $2
            THEN GREATEST(worker_task_health.last_success_at, EXCLUDED.last_success_at)
            ELSE worker_task_health.last_success_at END,
        last_progress_at = CASE WHEN $3
            THEN GREATEST(worker_task_health.last_progress_at, EXCLUDED.last_progress_at)
            ELSE worker_task_health.last_progress_at END,
        successes_total = worker_task_health.successes_total + EXCLUDED.successes_total,
        failures_total = worker_task_health.failures_total + EXCLUDED.failures_total,
        skipped_total = worker_task_health.skipped_total + EXCLUDED.skipped_total`

var regionalCheckpointTaskSQL = strings.NewReplacer(
	"olp.worker_task_health", "olp.regional_worker_task_health",
	"worker_task_health.", "regional_worker_task_health.",
	"(task, checked_at", "(region, task, checked_at",
	"VALUES ($1,", "VALUES ($7, $1,",
	"ON CONFLICT (task)", "ON CONFLICT (region, task)",
).Replace(checkpointTaskSQL)

// CheckpointTask records one pass of a worker task. `progress` says whether the
// pass actually moved work, which is what readiness distinguishes from a worker
// that is merely awake.
func (r Reporter) Checkpoint(ctx context.Context, q Execer, outcome Outcome, progress bool) error {
	task := r.task
	var success bool
	var successes, failures, skipped int64
	switch outcome {
	case OutcomeSuccess:
		success, successes = true, 1
	case OutcomeFailure:
		failures = 1
	case OutcomeSkipped:
		skipped = 1
	default:
		return fmt.Errorf("unknown worker task outcome %d", int(outcome))
	}
	if _, err := q.Exec(ctx, checkpointTaskSQL, string(task), success, progress,
		successes, failures, skipped); err != nil {
		return fmt.Errorf("checkpoint worker task %s: %w", task, err)
	}
	if region := r.region; region != "" && RegionalTask(task) {
		if _, err := q.Exec(ctx, regionalCheckpointTaskSQL, string(task), success, progress, successes, failures, skipped, region); err != nil {
			return fmt.Errorf("checkpoint regional worker task %s: %w", task, err)
		}
	}
	return nil
}

func RegionalTask(task Task) bool {
	for _, worker := range WorkerTasks() {
		if worker.Name == task {
			return worker.Regional
		}
	}
	return false
}
