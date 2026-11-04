package a2a

import (
	"errors"
	"net/http"
	"time"

	protocol "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/agents"
	"github.com/tyk-swe/olp/internal/agents/tasks"
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
	m.access.Route(mux, "POST /api/v1/a2a-tasks/{task_id}/cancel", m.remote, access.Deadline(35*time.Second))
}
func (m *Management) remote(r *http.Request, principal access.Principal) (access.Reply, error) {
	id, idErr := access.ParseUUID(r.PathValue("task_id"))
	if idErr != nil {
		return access.Reply{}, access.Fail(http.StatusNotFound, "not_found", "The resource was not found.")
	}
	task, err := m.tasks.Remote(r.Context(), id)
	if errors.Is(err, tasks.ErrNotFound) {
		return access.Reply{}, access.Fail(http.StatusNotFound, "not_found", "The resource was not found.")
	}
	if err != nil {
		return access.Reply{}, err
	}
	if err := principal.Project(&task.ProjectID, access.Change); err != nil {
		return access.Reply{}, err
	}
	remote, remoteErr := func() (*protocol.Task, error) {
		if r.Method == http.MethodPost {
			return m.operations.Cancel(r.Context(), task)
		}
		return m.operations.Get(r.Context(), task)
	}()
	if remoteErr != nil {
		return access.Reply{}, access.Fail(http.StatusBadGateway, "agent_unavailable", "The remote agent is unavailable.")
	}
	return access.OK(remote), nil
}
