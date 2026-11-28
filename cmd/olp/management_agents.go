package main

import (
	"net/http"

	"github.com/tyk-swe/olp/internal/agents"
	"github.com/tyk-swe/olp/internal/agents/a2a"
	"github.com/tyk-swe/olp/internal/agents/investigations"
	"github.com/tyk-swe/olp/internal/agents/publications"
	"github.com/tyk-swe/olp/internal/agents/runs"
	"github.com/tyk-swe/olp/internal/agents/tasks"
	"github.com/tyk-swe/olp/internal/grants/personal"
	"github.com/tyk-swe/olp/internal/mcp/accounting"
	"github.com/tyk-swe/olp/internal/mcp/registration"
)

func (m Management) registerAgents(mux *http.ServeMux) {
	plane := m.serving.agents
	a2a.NewManagement(m.Access, plane.tasks, agents.NewTaskOperations(plane.tasks, plane.peers, plane.grants)).Register(mux)
	publications.New(m.Access, m.core.policy, plane.peers.ValidateCard).Register(mux)
	runs.NewManagement(m.Access).Register(mux)
	tasks.NewManagement(m.Access).Register(mux)
	investigations.NewManagement(m.Access).Register(mux)
	mcpServers := registration.New(m.Access, m.core.policy, registration.NewExecutables(m.core.unconfined, m.Access.Pool))
	mcpServers.Register(mux)
	accounting.NewManagement(m.Access).Register(mux)
	personal.New(m.Access, mcpServers).Register(mux)
}
