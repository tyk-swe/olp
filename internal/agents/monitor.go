package agents

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/tyk-swe/olp/internal/agents/tasks"
)

type TaskMonitor struct {
	store      MonitorTasks
	operations *TaskOperations
	authority  TaskAuthority
	log        *slog.Logger
	// The cursor survives worker saturation so the next poll resumes the same scan.
	cursor  string
	horizon string
}

type TaskAuthority interface {
	AuthorizeAgentTask(string, string, string, string, string, *time.Time) error
	AuthorizeDelegatedAgentTask(context.Context, string, string, string, string) error
}
type MonitorTasks interface {
	Horizon(context.Context) (string, error)
	Page(context.Context, string, string) ([]tasks.Remote, error)
	Claim(context.Context, string) (bool, error)
}

func NewMonitor(store MonitorTasks, operations *TaskOperations, authority TaskAuthority, log *slog.Logger) *TaskMonitor {
	return &TaskMonitor{store: store, operations: operations, authority: authority, log: log}
}

func (m *TaskMonitor) Run(ctx context.Context) {
	workers := make(chan struct{}, 16)
	var outstanding sync.WaitGroup
	defer outstanding.Wait()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		m.poll(ctx, workers, &outstanding)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (m *TaskMonitor) poll(ctx context.Context, workers chan struct{}, outstanding *sync.WaitGroup) {
	if m.horizon == "" {
		horizon, err := m.store.Horizon(ctx)
		m.horizon = horizon
		if horizon == "" && err == nil {
			return
		}
		if err != nil {
			m.warn("agent task monitor could not read tasks")
			return
		}
	}
	for {
		if !m.pollPage(ctx, workers, outstanding) {
			return
		}
	}
}

func (m *TaskMonitor) pollPage(ctx context.Context, workers chan struct{}, outstanding *sync.WaitGroup) bool {
	page, err := m.store.Page(ctx, m.cursor, m.horizon)
	if err != nil {
		m.warn("agent task monitor could not read tasks")
		return false
	}

	for _, task := range page {
		if time.Now().Before(task.Deadline) && m.authorized(ctx, task) {
			m.cursor = task.ID
			continue
		}
		select {
		case workers <- struct{}{}:
		default:
			return false
		}
		claimed, err := m.store.Claim(ctx, task.ID)
		m.cursor = task.ID
		if err != nil || !claimed {
			<-workers
			if err != nil {
				m.warn("agent task monitor could not claim a task")
			}
			continue
		}
		outstanding.Add(1)
		go func() {
			defer outstanding.Done()
			defer func() { <-workers }()
			m.cancel(ctx, task)
		}()
	}
	if len(page) < 128 {
		m.cursor = ""
		m.horizon = ""
		return false
	}
	return true
}

func (m *TaskMonitor) authorized(ctx context.Context, task tasks.Remote) bool {
	if m.authority.AuthorizeAgentTask(task.KeyID, task.ProjectID, task.PublicationID, task.RevisionID, task.WorkloadRevision, task.CallerExpiresAt) != nil {
		return false
	}
	if task.Delegation == "" {
		return true
	}
	return m.authority.AuthorizeDelegatedAgentTask(ctx, task.Delegation, task.KeyID, task.ProjectID, task.PublicationID) == nil
}

func (m *TaskMonitor) warn(message string) {
	if m.log != nil {
		m.log.Warn(message)
	}
}

func (m *TaskMonitor) cancel(parent context.Context, task tasks.Remote) {
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	if _, err := m.operations.perform(ctx, task, true, true); err != nil {
		bounded, stop := context.WithTimeout(context.WithoutCancel(parent), 5*time.Second)
		defer stop()
		if stateErr := m.operations.states.RecordState(bounded, task.ID, tasks.Unknown); stateErr != nil && !errors.Is(stateErr, tasks.ErrNotFound) {
			m.warn("agent task monitor could not record task state")
		}
	}
}
