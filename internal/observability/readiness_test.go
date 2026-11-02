package observability

import (
	"slices"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/media"
	"github.com/tyk-swe/olp/internal/usage"
)

func TestReadinessMediaReconciliationReflectsDegradation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		summary *media.Summary
		gaps    int64
		want    string
	}{
		{"healthy", &media.Summary{Pending: 2}, 0, "ok"},
		{"failed", &media.Summary{Failed: 3}, 0, "degraded"},
		{"stale", &media.Summary{Stale: 1}, 0, "degraded"},
		{"gaps", &media.Summary{}, 1, "degraded"},
		{"unread", nil, 0, "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			probe := &storeProbe{database: "ok", media: tc.summary, tasks: &WorkerTaskHealth{}}
			degraded := mediaDegraded(tc.summary, tc.gaps)
			h := readinessResponse(&State{}, time.Now(), nil, probe, nil,
				true, true, true, false, degraded, false, false, tc.gaps)
			if h.MediaReconciliation != tc.want {
				t.Fatalf("media_reconciliation = %q, want %q", h.MediaReconciliation, tc.want)
			}
			if degraded && h.Status != "degraded" {
				t.Fatalf("status = %q, want degraded", h.Status)
			}
		})
	}
}

func TestReadinessDetectsStaleFinancialWorkers(t *testing.T) {
	for _, stopped := range []usage.Task{usage.TaskBatchSettlement, usage.TaskRetainedSettlement, usage.TaskExecutionDebits} {
		t.Run(string(stopped), func(t *testing.T) {
			health := &WorkerTaskHealth{}
			for _, task := range usage.WorkerTasks() {
				state := TaskStateHealthy
				if task.Name == stopped {
					state = TaskStateStale
				}
				health.Tasks = append(health.Tasks, WorkerTaskStatus{Task: string(task.Name), State: state})
			}
			for _, configured := range []bool{false, true} {
				if health.CurrentFor(expectedTasks(configured)) || health.StaleFor(expectedTasks(configured)) != 1 {
					t.Fatal("stopped financial worker was considered healthy")
				}
			}
		})
	}
}

func TestReadinessOnlyRequiresRegionalWorkersWithLimiter(t *testing.T) {
	for _, tc := range []struct {
		task                   usage.Task
		requiredWithoutLimiter bool
	}{
		{usage.TaskRequestMetadataConsumer, false},
		{usage.TaskCostReconciliation, false},
		{usage.TaskHealthProbes, false},
		{usage.TaskEpochDetection, true},
	} {
		t.Run(string(tc.task), func(t *testing.T) {
			health := &WorkerTaskHealth{Tasks: slices.Collect(func(yield func(WorkerTaskStatus) bool) {
				for _, task := range usage.WorkerTasks() {
					if task.Name != tc.task && !yield(WorkerTaskStatus{Task: string(task.Name), State: TaskStateHealthy}) {
						return
					}
				}
			})}
			if health.CurrentFor(expectedTasks(true)) {
				t.Fatal("readiness accepted a missing worker with a limiter")
			}
			if got, want := health.CurrentFor(expectedTasks(false)), !tc.requiredWithoutLimiter; got != want {
				t.Fatalf("readiness without a limiter = %t, want %t", got, want)
			}
		})
	}
}
