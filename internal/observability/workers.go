package observability

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/observability/workerhealth"
)

func regionalWorkerTaskNames() []string {
	return slices.Collect(func(yield func(string) bool) {
		for _, task := range workerhealth.Tasks {
			if task.Regional() && !yield(string(task)) {
				return
			}
		}
	})
}

type TaskState string

const (
	TaskUnknown TaskState = "unknown"
	TaskHealthy TaskState = "healthy"
	TaskStale   TaskState = "stale"
)

// WorkerTaskStatus is one task's durable checkpoint picture.
type WorkerTaskStatus struct {
	Task                  workerhealth.Task
	State                 TaskState
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

func unknownWorkerTasks() *WorkerTaskHealth {
	tasks := make([]WorkerTaskStatus, len(workerhealth.Tasks))
	for i, task := range workerhealth.Tasks {
		tasks[i] = WorkerTaskStatus{Task: task, State: TaskUnknown}
	}
	return &WorkerTaskHealth{Tasks: tasks}
}

func (w *WorkerTaskHealth) Count(expected []workerhealth.Task, state TaskState) int64 {
	var count int64
	for _, task := range expected {
		if w.status(task).State == state {
			count++
		}
	}
	return count
}

func (w *WorkerTaskHealth) LastProgressFor(expected []workerhealth.Task) *time.Time {
	var last *time.Time
	for _, task := range expected {
		if at := w.status(task).LastProgressAt; at != nil && (last == nil || at.After(*last)) {
			last = at
		}
	}
	return last
}

func (w *WorkerTaskHealth) status(task workerhealth.Task) WorkerTaskStatus {
	for _, status := range w.Tasks {
		if status.Task == task {
			return status
		}
	}
	return WorkerTaskStatus{Task: task, State: TaskUnknown}
}

// ReadRegionalWorkerTaskHealth combines local coordination checkpoints with
// shared PostgreSQL task health. An empty region reads the global summary.
// Ages are computed by the database against clock_timestamp(), the same clock
// workers stamp checkpoints with, so replica clock skew cannot falsify
// staleness.
func ReadRegionalWorkerTaskHealth(ctx context.Context, q access.Queryer, region string) (*WorkerTaskHealth, error) {
	rows, err := q.Query(ctx, `SELECT task, checked_at, last_success_at, last_progress_at,
			successes_total, failures_total, skipped_total,
			GREATEST(0, floor(extract(epoch FROM clock_timestamp() - checked_at)))::bigint AS heartbeat_age_seconds,
			CASE WHEN last_success_at IS NULL THEN NULL ELSE
				GREATEST(0, floor(extract(epoch FROM clock_timestamp() - last_success_at)))::bigint
			END AS last_success_age_seconds
		FROM (
			SELECT task,checked_at,last_success_at,last_progress_at,successes_total,failures_total,skipped_total
			FROM olp.worker_task_health WHERE $1='' OR task<>ALL($2::text[])
			UNION ALL
			SELECT task,checked_at,last_success_at,last_progress_at,successes_total,failures_total,skipped_total
			FROM olp.regional_worker_task_health WHERE region=$1 AND task=ANY($2::text[])
		) health ORDER BY task`, region, regionalWorkerTaskNames())
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
		row.Task = workerhealth.Task(task)
		if row.SuccessesTotal < 0 || row.FailuresTotal < 0 || row.SkippedTotal < 0 {
			return nil, errors.New("stored worker task health is invalid")
		}
		byTask[task] = row
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	tasks := make([]WorkerTaskStatus, len(workerhealth.Tasks))
	for i, task := range workerhealth.Tasks {
		tasks[i] = taskStatus(task, byTask)
	}
	return &WorkerTaskHealth{Tasks: tasks}, nil
}

func taskStatus(task workerhealth.Task, stored map[string]WorkerTaskStatus) WorkerTaskStatus {
	row, ok := stored[string(task)]
	if !ok {
		return WorkerTaskStatus{Task: task, State: TaskUnknown}
	}
	row.State = TaskStale
	if row.LastSuccessAgeSeconds != nil && *row.LastSuccessAgeSeconds <= task.StaleAfter() {
		row.State = TaskHealthy
	}
	return row
}

// WorkerRecoveryCounters are the durable cross-replica counters the worker
// plane maintains.
type WorkerRecoveryCounters struct {
	RequestMetadataReclaimed     int64
	RequestMetadataRecovered     int64
	RequestMetadataDuplicates    int64
	RequestMetadataProcessed     int64
	MediaReconciliationGapsTotal int64
}

// ReadWorkerRecoveryCounters loads the durable async worker counters.
func ReadWorkerRecoveryCounters(ctx context.Context, q access.Queryer) (WorkerRecoveryCounters, error) {
	var c WorkerRecoveryCounters
	err := q.QueryRow(ctx, `SELECT request_metadata_reclaimed_total, request_metadata_recovered_total,
			request_metadata_duplicates_total, request_metadata_processed_total,
			media_reconciliation_gaps_total
		FROM olp.async_worker_counters WHERE singleton`).Scan(
		&c.RequestMetadataReclaimed, &c.RequestMetadataRecovered,
		&c.RequestMetadataDuplicates, &c.RequestMetadataProcessed,
		&c.MediaReconciliationGapsTotal)
	if errors.Is(err, pgx.ErrNoRows) {
		return c, nil
	}
	if err != nil {
		return c, fmt.Errorf("read async worker counters: %w", err)
	}
	if c.RequestMetadataReclaimed < 0 || c.RequestMetadataRecovered < 0 ||
		c.RequestMetadataDuplicates < 0 || c.RequestMetadataProcessed < 0 ||
		c.MediaReconciliationGapsTotal < 0 {
		return c, errors.New("stored async worker counters are invalid")
	}
	return c, nil
}
