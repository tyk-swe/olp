package a2a

import (
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/agents"
	"github.com/tyk-swe/olp/internal/agents/tasks"
	"github.com/tyk-swe/olp/internal/domain/ids"
	"github.com/tyk-swe/olp/internal/domain/problem"
)

type Management struct {
	access     *access.Server
	tasks      *tasks.Store
	operations *agents.TaskOperations
}

func NewManagement(a *access.Server, store *tasks.Store, operations *agents.TaskOperations) *Management {
	return &Management{access: a, tasks: store, operations: operations}
}
func (m *Management) Register(mux *http.ServeMux) {
	m.access.Route(mux, "GET /api/v1/a2a-tasks/{task_id}/remote", m.remote, access.Deadline(35*time.Second))
	m.access.Route(mux, "POST /api/v1/a2a-tasks/{task_id}/cancel", m.cancel, access.Deadline(35*time.Second))
}

func taskID(r *http.Request) (string, error) {
	id, err := ids.Parse(r.PathValue("task_id"))
	if err != nil {
		return "", problem.Fail(http.StatusNotFound, "not_found", "The resource was not found.")
	}
	return id, nil
}

func remoteLookupError(err error) error {
	if errors.Is(err, tasks.ErrNotFound) {
		return problem.Fail(http.StatusNotFound, "not_found", "The resource was not found.")
	}
	return err
}

func (m *Management) cancel(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	id, err := taskID(r)
	if err != nil {
		return httpkit.Reply{}, err
	}
	load := func(tx pgx.Tx, p authz.Principal) (tasks.Remote, error) {
		task, err := m.tasks.RemoteForUpdate(r.Context(), tx, id)
		if err != nil {
			return tasks.Remote{}, remoteLookupError(err)
		}
		if err := p.Project(&task.ProjectID, authz.Change); err != nil {
			return tasks.Remote{}, err
		}
		return task, nil
	}
	task, err := access.Run(m.access, r, func(tx pgx.Tx, p authz.Principal) (tasks.Remote, access.Event, error) {
		task, err := load(tx, p)
		if err != nil {
			return tasks.Remote{}, access.Event{}, err
		}
		return task, access.Event{Action: "a2a_task.cancel_attempt", Resource: "a2a_task", ID: task.ID, Project: &task.ProjectID}, nil
	})
	if err != nil {
		return httpkit.Reply{}, err
	}
	remote, remoteErr := m.operations.Cancel(r.Context(), task)
	if remoteErr != nil {
		return httpkit.Reply{}, problem.Fail(http.StatusBadGateway, "agent_unavailable", "The remote agent is unavailable.")
	}
	return access.Transact(m.access, r, load, func(_ pgx.Tx, _ authz.Principal, current tasks.Remote) (httpkit.Reply, access.Event, error) {
		if current.UpstreamID != task.UpstreamID || current.RevisionID != task.RevisionID || current.Binding != task.Binding {
			return httpkit.Reply{}, access.Event{}, problem.Fail(http.StatusConflict, "a2a_task_changed", "The task changed while cancellation was in progress.")
		}
		return httpkit.OK(remote), access.Event{Action: "a2a_task.cancel", Resource: "a2a_task", ID: current.ID, Project: &current.ProjectID}, nil
	})
}

func (m *Management) remote(r *http.Request, principal authz.Principal) (httpkit.Reply, error) {
	id, err := taskID(r)
	if err != nil {
		return httpkit.Reply{}, err
	}
	task, err := m.tasks.Remote(r.Context(), id)
	if err != nil {
		return httpkit.Reply{}, remoteLookupError(err)
	}
	if err := principal.Project(&task.ProjectID, authz.Change); err != nil {
		return httpkit.Reply{}, err
	}
	remote, remoteErr := m.operations.Get(r.Context(), task)
	if remoteErr != nil {
		return httpkit.Reply{}, problem.Fail(http.StatusBadGateway, "agent_unavailable", "The remote agent is unavailable.")
	}
	return httpkit.OK(remote), nil
}
