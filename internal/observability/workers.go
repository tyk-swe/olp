package observability

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/usage"
)

// WorkerTask is one fixed worker responsibility that checkpoints its liveness
// into olp_go.worker_task_health.
type WorkerTask struct {
	Name       string
	StaleAfter int64 // seconds since last success before the task is stale
}

// WorkerTasks is every fixed responsibility a worker replica may checkpoint.
// RequestMetadataConsumer and CostReconciliation exist only when shared state
// is configured; the rest run on any worker replica.
var WorkerTasks = []WorkerTask{
	{Name: string(usage.TaskRequestMetadataConsumer), StaleAfter: 20},
	{Name: string(usage.TaskEpochDetection), StaleAfter: 20},
	{Name: string(usage.TaskMediaReconciliation), StaleAfter: 20},
	{Name: string(usage.TaskMaintenance), StaleAfter: 180},
	{Name: string(usage.TaskCostReconciliation), StaleAfter: 180},
	{Name: string(usage.TaskBudgetAlertDelivery), StaleAfter: 180},
}

// ValkeyWorkerTasks are the responsibilities expected only when a limiter is
// configured.
var ValkeyWorkerTasks = []string{
	string(usage.TaskRequestMetadataConsumer),
	string(usage.TaskCostReconciliation),
}

const (
	TaskStateUnknown = "unknown"
	TaskStateHealthy = "healthy"
	TaskStateStale   = "stale"
)

// WorkerTaskStatus is one task's durable checkpoint picture.
type WorkerTaskStatus struct {
	Task                  string
	State                 string
	CheckedAt             *time.Time
	LastSuccessAt         *time.Time
	LastProgressAt        *time.Time
	HeartbeatAgeSeconds   *int64
	LastSuccessAgeSeconds *int64
	SuccessesTotal        int64
	FailuresTotal         int64
	SkippedTotal          int64
}

// WorkerTaskHealth summarizes the whole fixed-task set, filling absent rows as
// unknown so a silent task reads honestly.
type WorkerTaskHealth struct {
	Tasks []WorkerTaskStatus
}

// CurrentFor reports whether every expected task has a current success.
func (w *WorkerTaskHealth) CurrentFor(expected []WorkerTask) bool {
	for _, task := range expected {
		if w.status(task.Name).State != TaskStateHealthy {
			return false
		}
	}
	return true
}

// StaleFor counts expected tasks whose last success is too old.
func (w *WorkerTaskHealth) StaleFor(expected []WorkerTask) int64 {
	var count int64
	for _, task := range expected {
		if w.status(task.Name).State == TaskStateStale {
			count++
		}
	}
	return count
}

// UnknownFor counts expected tasks that have never checkpointed.
func (w *WorkerTaskHealth) UnknownFor(expected []WorkerTask) int64 {
	var count int64
	for _, task := range expected {
		if w.status(task.Name).State == TaskStateUnknown {
			count++
		}
	}
	return count
}

// LastProgressFor is the newest progress timestamp among expected tasks.
func (w *WorkerTaskHealth) LastProgressFor(expected []WorkerTask) *time.Time {
	var last *time.Time
	for _, task := range expected {
		if at := w.status(task.Name).LastProgressAt; at != nil && (last == nil || at.After(*last)) {
			last = at
		}
	}
	return last
}

func (w *WorkerTaskHealth) status(name string) WorkerTaskStatus {
	for _, task := range w.Tasks {
		if task.Task == name {
			return task
		}
	}
	return WorkerTaskStatus{Task: name, State: TaskStateUnknown}
}

// ReadWorkerTaskHealth reports per-task freshness. Ages are computed by the
// database against clock_timestamp(), the same clock workers stamp
// checkpoints with, so replica clock skew cannot falsify staleness.
func ReadWorkerTaskHealth(ctx context.Context, q access.Queryer) (*WorkerTaskHealth, error) {
	rows, err := q.Query(ctx, `SELECT task, checked_at, last_success_at, last_progress_at,
			successes_total, failures_total, skipped_total,
			GREATEST(0, floor(extract(epoch FROM clock_timestamp() - checked_at)))::bigint AS heartbeat_age_seconds,
			CASE WHEN last_success_at IS NULL THEN NULL ELSE
				GREATEST(0, floor(extract(epoch FROM clock_timestamp() - last_success_at)))::bigint
			END AS last_success_age_seconds
		FROM olp_go.worker_task_health ORDER BY task`)
	if err != nil {
		return nil, fmt.Errorf("read worker task health: %w", err)
	}
	defer rows.Close()
	byTask := map[string]WorkerTaskStatus{}
	for rows.Next() {
		var row WorkerTaskStatus
		var task string
		if err := rows.Scan(&task, &row.CheckedAt, &row.LastSuccessAt, &row.LastProgressAt,
			&row.SuccessesTotal, &row.FailuresTotal, &row.SkippedTotal,
			&row.HeartbeatAgeSeconds, &row.LastSuccessAgeSeconds); err != nil {
			return nil, err
		}
		row.Task = task
		if row.SuccessesTotal < 0 || row.FailuresTotal < 0 || row.SkippedTotal < 0 {
			return nil, errors.New("stored worker task health is invalid")
		}
		byTask[task] = row
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	summary := &WorkerTaskHealth{Tasks: make([]WorkerTaskStatus, 0, len(WorkerTasks))}
	for _, task := range WorkerTasks {
		row, ok := byTask[task.Name]
		if !ok {
			summary.Tasks = append(summary.Tasks, WorkerTaskStatus{Task: task.Name, State: TaskStateUnknown})
			continue
		}
		row.State = TaskStateStale
		if row.LastSuccessAgeSeconds != nil && *row.LastSuccessAgeSeconds <= task.StaleAfter {
			row.State = TaskStateHealthy
		}
		summary.Tasks = append(summary.Tasks, row)
	}
	return summary, nil
}

// WorkerRecoveryCounters are the durable cross-replica counters the worker
// plane maintains.
type WorkerRecoveryCounters struct {
	RequestMetadataReclaimed  int64
	RequestMetadataRecovered  int64
	RequestMetadataDuplicates int64
	RequestMetadataProcessed  int64
}

// ReadWorkerRecoveryCounters loads the durable async worker counters.
func ReadWorkerRecoveryCounters(ctx context.Context, q access.Queryer) (WorkerRecoveryCounters, error) {
	var c WorkerRecoveryCounters
	err := q.QueryRow(ctx, `SELECT request_metadata_reclaimed_total, request_metadata_recovered_total,
			request_metadata_duplicates_total, request_metadata_processed_total
		FROM olp_go.async_worker_counters WHERE singleton`).Scan(
		&c.RequestMetadataReclaimed, &c.RequestMetadataRecovered,
		&c.RequestMetadataDuplicates, &c.RequestMetadataProcessed)
	if errors.Is(err, pgx.ErrNoRows) {
		return c, nil
	}
	if err != nil {
		return c, fmt.Errorf("read async worker counters: %w", err)
	}
	if c.RequestMetadataReclaimed < 0 || c.RequestMetadataRecovered < 0 ||
		c.RequestMetadataDuplicates < 0 || c.RequestMetadataProcessed < 0 {
		return c, errors.New("stored async worker counters are invalid")
	}
	return c, nil
}
