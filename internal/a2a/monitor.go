package a2a

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	protocol "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/agentcontract"
	"github.com/tyk-swe/olp/internal/runtime"
)

type TaskMonitor struct {
	Server  *Server
	Log     *slog.Logger
	cursor  string
	horizon string
}

type taskAuthorizer interface {
	AuthorizeAgentTask(string, string, string, string, string, *time.Time) error
}

type delegatedTaskAuthorizer interface {
	AuthorizeDelegatedAgentTask(context.Context, string, string, string, string) error
}

type monitoredTask struct {
	ID               string
	KeyID            string
	ProjectID        string
	PublicationID    string
	RevisionID       string
	UpstreamID       string
	Deadline         time.Time
	WorkloadRevision string
	CallerExpiresAt  *time.Time
	Definition       []byte
	Binding          string
	Delegation       string
}

func (m *TaskMonitor) Run(ctx context.Context) {
	if m == nil || m.Server == nil || m.Server.Ledger == nil || m.Server.Ledger.Pool == nil {
		return
	}
	authority, ok := m.Server.Runtime.(taskAuthorizer)
	if !ok {
		return
	}
	workers := make(chan struct{}, 16)
	var outstanding sync.WaitGroup
	defer outstanding.Wait()
	ticker := time.NewTicker(runtime.PollInterval)
	defer ticker.Stop()
	for {
		m.poll(ctx, authority, workers, &outstanding)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (m *TaskMonitor) poll(ctx context.Context, authority taskAuthorizer, workers chan struct{}, outstanding *sync.WaitGroup) {
	if m.horizon == "" {
		err := m.Server.Ledger.Pool.QueryRow(ctx, `SELECT id::text FROM olp.a2a_tasks ORDER BY id DESC LIMIT 1`).Scan(&m.horizon)
		if errors.Is(err, pgx.ErrNoRows) {
			return
		}
		if err != nil {
			m.warn("agent task monitor could not read tasks")
			return
		}
	}
	for {
		if !m.pollPage(ctx, authority, workers, outstanding) {
			return
		}
	}
}

func (m *TaskMonitor) pollPage(ctx context.Context, authority taskAuthorizer, workers chan struct{}, outstanding *sync.WaitGroup) bool {
	rows, err := m.Server.Ledger.Pool.Query(ctx, `SELECT t.id::text,t.api_key_id::text,t.project_id::text,t.publication_id::text,t.revision_id::text,t.upstream_id,t.deadline_at,COALESCE(t.caller_workload_revision_id::text,''),t.caller_expires_at,v.definition,COALESCE(t.oauth_binding_digest,''),t.delegation_token_digest
 FROM olp.a2a_tasks t JOIN olp.agent_publication_revisions v ON v.id=t.revision_id
 WHERE t.state IN ('active','outcome_unknown') AND (t.cancel_claimed_until IS NULL OR t.cancel_claimed_until<=now()) AND t.id>COALESCE(NULLIF($1,'')::uuid,'00000000-0000-0000-0000-000000000000'::uuid) AND t.id<=$2::uuid
 ORDER BY t.id LIMIT 128`, m.cursor, m.horizon)
	if err != nil {
		m.warn("agent task monitor could not read tasks")
		return false
	}
	tasks, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (monitoredTask, error) {
		var task monitoredTask
		err := row.Scan(&task.ID, &task.KeyID, &task.ProjectID, &task.PublicationID, &task.RevisionID, &task.UpstreamID, &task.Deadline, &task.WorkloadRevision, &task.CallerExpiresAt, &task.Definition, &task.Binding, &task.Delegation)
		return task, err
	})
	if err != nil {
		m.warn("agent task monitor could not read tasks")
		return false
	}
	for _, task := range tasks {
		if time.Now().Before(task.Deadline) && m.authorized(ctx, authority, task) {
			m.cursor = task.ID
			continue
		}
		select {
		case workers <- struct{}{}:
		default:
			return false
		}
		claimed, err := m.claim(ctx, task.ID)
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
	if len(tasks) < 128 {
		m.cursor = ""
		m.horizon = ""
		return false
	}
	return true
}

func (m *TaskMonitor) authorized(ctx context.Context, authority taskAuthorizer, task monitoredTask) bool {
	if authority.AuthorizeAgentTask(task.KeyID, task.ProjectID, task.PublicationID, task.RevisionID, task.WorkloadRevision, task.CallerExpiresAt) != nil {
		return false
	}
	if task.Delegation == "" {
		return true
	}
	delegated, ok := m.Server.Runtime.(delegatedTaskAuthorizer)
	return ok && delegated.AuthorizeDelegatedAgentTask(ctx, task.Delegation, task.KeyID, task.ProjectID, task.PublicationID) == nil
}

func (m *TaskMonitor) claim(ctx context.Context, taskID string) (bool, error) {
	tag, err := m.Server.Ledger.Pool.Exec(ctx, `UPDATE olp.a2a_tasks SET cancel_claimed_until=now()+interval '30 seconds'
 WHERE id=$1 AND state IN ('active','outcome_unknown') AND (cancel_claimed_until IS NULL OR cancel_claimed_until<=now())`, taskID)
	return err == nil && tag.RowsAffected() == 1, err
}

func (m *TaskMonitor) cancel(parent context.Context, task monitoredTask) {
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	state := m.cancelState(ctx, task)
	writeCtx, writeCancel := context.WithTimeout(context.WithoutCancel(parent), 5*time.Second)
	defer writeCancel()
	if err := m.Server.Ledger.recordTaskState(writeCtx, task.ID, state); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		m.warn("agent task monitor could not record task state")
	}
}

func (m *TaskMonitor) cancelState(ctx context.Context, task monitoredTask) string {
	var definition agentcontract.Draft
	if json.Unmarshal(task.Definition, &definition) != nil || definition.Agent == nil || definition.ProjectID != task.ProjectID {
		return "outcome_unknown"
	}
	credential, err := m.Server.taskCredential(ctx, task, *definition.Agent)
	if err != nil {
		return "outcome_unknown"
	}
	defer clear(credential)
	transport, headers, cleanup, err := m.Server.transportWithCredential(ctx, *definition.Agent, credential)
	if err != nil {
		return "outcome_unknown"
	}
	defer cleanup()
	result, err := transport.CancelTask(ctx, headers, &protocol.CancelTaskRequest{ID: protocol.TaskID(task.UpstreamID)})
	if err != nil || result == nil || string(result.ID) != task.UpstreamID {
		return "outcome_unknown"
	}
	switch state := taskState(result.Status.State); state {
	case "completed", "failed", "canceled":
		return state
	default:
		return "outcome_unknown"
	}
}

func (s *Server) taskCredential(ctx context.Context, task monitoredTask, definition agentcontract.Definition) ([]byte, error) {
	if definition.OAuth == nil && task.Binding == "" {
		return nil, nil
	}
	if definition.OAuth == nil || task.Binding == "" || s.Grants == nil {
		return nil, errors.New("agent grant unavailable")
	}
	return s.Grants.ResolveAgentTask(ctx, task.Binding, task.PublicationID, task.ProjectID, task.RevisionID, definition)
}

func (m *TaskMonitor) warn(message string) {
	if m.Log != nil {
		m.Log.Warn(message)
	}
}
