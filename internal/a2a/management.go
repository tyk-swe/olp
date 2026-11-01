package a2a

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	protocol "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/agentcontract"
)

type Management struct {
	Access *access.Server
	Server *Server
}

func (m *Management) Register(mux *http.ServeMux) {
	m.Access.Route(mux, "GET /api/v1/a2a-tasks/{task_id}/remote", m.remote, access.Deadline(35*time.Second))
	m.Access.Route(mux, "POST /api/v1/a2a-tasks/{task_id}/cancel", m.remote, access.Deadline(35*time.Second))
}

func (m *Management) remote(r *http.Request, principal access.Principal) (access.Reply, error) {
	id := r.PathValue("task_id")
	if parsed, err := uuid.Parse(id); err != nil || parsed == uuid.Nil || parsed.String() != id {
		return access.Reply{}, access.Fail(http.StatusNotFound, "not_found", "The resource was not found.")
	}
	var task monitoredTask
	err := m.Access.Pool.QueryRow(r.Context(), `SELECT t.id::text,t.project_id::text,t.publication_id::text,t.revision_id::text,t.upstream_id,COALESCE(t.oauth_binding_digest,''),v.definition
 FROM olp.a2a_tasks t JOIN olp.agent_publication_revisions v ON v.id=t.revision_id AND v.publication_id=t.publication_id
 WHERE t.id=$1`, id).Scan(&task.ID, &task.ProjectID, &task.PublicationID, &task.RevisionID, &task.UpstreamID, &task.Binding, &task.Definition)
	if errors.Is(err, pgx.ErrNoRows) {
		return access.Reply{}, access.Fail(http.StatusNotFound, "not_found", "The resource was not found.")
	}
	if err != nil {
		return access.Reply{}, err
	}
	if err := principal.Project(&task.ProjectID, access.Change); err != nil {
		return access.Reply{}, err
	}
	var draft agentcontract.Draft
	if json.Unmarshal(task.Definition, &draft) != nil || draft.Agent == nil || draft.ProjectID != task.ProjectID || !supportedProtocol(draft.Agent.Protocol) {
		return access.Reply{}, access.Fail(http.StatusBadGateway, "agent_unavailable", "The remote agent is unavailable.")
	}
	credential, err := m.Server.taskCredential(r.Context(), task, *draft.Agent)
	if err != nil {
		return access.Reply{}, access.Fail(http.StatusBadGateway, "agent_unavailable", "The remote agent is unavailable.")
	}
	defer clear(credential)
	if _, err := m.Server.loadCardWithCredential(r.Context(), *draft.Agent, credential); err != nil {
		return access.Reply{}, access.Fail(http.StatusBadGateway, "agent_unavailable", "The remote agent is unavailable.")
	}
	transport, headers, cleanup, err := m.Server.transportWithCredential(r.Context(), *draft.Agent, credential)
	if err != nil {
		return access.Reply{}, access.Fail(http.StatusBadGateway, "agent_unavailable", "The remote agent is unavailable.")
	}
	defer cleanup()
	if r.Method == http.MethodPost {
		if err := m.Server.Ledger.recordTaskState(r.Context(), task.ID, "outcome_unknown"); err != nil {
			return access.Reply{}, err
		}
	}
	remote, err := func() (*protocol.Task, error) {
		if r.Method == http.MethodPost {
			return transport.CancelTask(r.Context(), headers, &protocol.CancelTaskRequest{ID: protocol.TaskID(task.UpstreamID)})
		}
		return transport.GetTask(r.Context(), headers, &protocol.GetTaskRequest{ID: protocol.TaskID(task.UpstreamID)})
	}()
	if err != nil || remote == nil || string(remote.ID) != task.UpstreamID {
		return access.Reply{}, access.Fail(http.StatusBadGateway, "agent_unavailable", "The remote agent is unavailable.")
	}
	if err := m.Server.Ledger.recordTaskState(r.Context(), task.ID, taskState(remote.Status.State)); err != nil {
		return access.Reply{}, err
	}
	return access.OK(publicTask(remote, task.ID)), nil
}
