package tasks

import (
	"net/http"

	"github.com/tyk-swe/olp/internal/access"
)

type Management struct{ access *access.Server }

func NewManagement(a *access.Server) *Management { return &Management{access: a} }
func (s *Management) Register(mux *http.ServeMux) {
	s.access.Route(mux, "GET /api/v1/a2a-tasks", s.listTasks)
	s.access.Route(mux, "GET /api/v1/a2a-tasks/{task_id}", s.getTask)
}
