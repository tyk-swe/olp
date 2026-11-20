package workerhealth

import (
	"context"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5/pgconn"
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
	TaskRequestMetadataConsumer Task = "request_metadata_consumer"
	TaskMaintenance             Task = "maintenance"
	TaskCostReconciliation      Task = "cost_reconciliation"
	TaskEpochDetection          Task = "request_metadata_gateway_epoch_detection"
	TaskMediaReconciliation     Task = "media_reconciliation"
	TaskBatchSettlement         Task = "batch_settlement"
	TaskExecutionDebits         Task = "execution_debits"
	TaskRetainedSettlement      Task = "retained_settlement"
	TaskNotificationDelivery    Task = "notification_delivery"
	TaskManagedExportDelivery   Task = "managed_export_delivery"
	TaskGrantRefresh            Task = "grant_refresh"
	TaskHealthProbes            Task = "health_probes"
	TaskExportDelivery          Task = "export_delivery"
	TaskBillingImport           Task = "billing_import"
	TaskFocusExport             Task = "focus_export"
	TaskCodeAllowanceRead       Task = "code_allowance_read"
)

var Tasks = [...]Task{
	TaskRequestMetadataConsumer,
	TaskEpochDetection,
	TaskMediaReconciliation,
	TaskBatchSettlement,
	TaskRetainedSettlement,
	TaskMaintenance,
	TaskCostReconciliation,
	TaskExecutionDebits,
	TaskNotificationDelivery,
	TaskManagedExportDelivery,
	TaskGrantRefresh,
	TaskHealthProbes,
	TaskExportDelivery,
	TaskBillingImport,
	TaskFocusExport,
	TaskCodeAllowanceRead,
}

func Parse(name string) (Task, bool) {
	task := Task(name)
	return task, slices.Contains(Tasks[:], task)
}

func (t Task) StaleAfter() int64 {
	switch t {
	case TaskRequestMetadataConsumer, TaskEpochDetection, TaskMediaReconciliation, TaskGrantRefresh:
		return 20
	case TaskExportDelivery:
		return 30
	case TaskCodeAllowanceRead:
		return 60
	default:
		return 180
	}
}

// Regional tasks run once per region, so each region checkpoints its own row.
func (t Task) Regional() bool {
	switch t {
	case TaskRequestMetadataConsumer, TaskCostReconciliation, TaskHealthProbes:
		return true
	default:
		return false
	}
}

type Outcome int

const (
	OutcomeSuccess Outcome = iota
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

const regionalCheckpointTaskSQL = `INSERT INTO olp.regional_worker_task_health
        (region, task, checked_at, last_success_at, last_progress_at,
         successes_total, failures_total, skipped_total)
    VALUES ($7, $1, clock_timestamp(),
        CASE WHEN $2 THEN clock_timestamp() ELSE NULL END,
        CASE WHEN $3 THEN clock_timestamp() ELSE NULL END, $4, $5, $6)
    ON CONFLICT (region, task) DO UPDATE SET
        checked_at = GREATEST(regional_worker_task_health.checked_at, EXCLUDED.checked_at),
        last_success_at = CASE WHEN $2
            THEN GREATEST(regional_worker_task_health.last_success_at, EXCLUDED.last_success_at)
            ELSE regional_worker_task_health.last_success_at END,
        last_progress_at = CASE WHEN $3
            THEN GREATEST(regional_worker_task_health.last_progress_at, EXCLUDED.last_progress_at)
            ELSE regional_worker_task_health.last_progress_at END,
        successes_total = regional_worker_task_health.successes_total + EXCLUDED.successes_total,
        failures_total = regional_worker_task_health.failures_total + EXCLUDED.failures_total,
        skipped_total = regional_worker_task_health.skipped_total + EXCLUDED.skipped_total`

// Checkpoint records one pass of a worker task. `progress` says whether the
// pass actually moved work, which is what readiness distinguishes from a worker
// that is merely awake.
func (r Reporter) Checkpoint(ctx context.Context, q Execer, outcome Outcome, progress bool) error {
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
	if _, err := q.Exec(ctx, checkpointTaskSQL, string(r.task), success, progress,
		successes, failures, skipped); err != nil {
		return fmt.Errorf("checkpoint worker task %s: %w", r.task, err)
	}
	if r.region != "" && r.task.Regional() {
		if _, err := q.Exec(ctx, regionalCheckpointTaskSQL, string(r.task), success, progress, successes, failures, skipped, r.region); err != nil {
			return fmt.Errorf("checkpoint regional worker task %s: %w", r.task, err)
		}
	}
	return nil
}
