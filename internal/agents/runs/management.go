package runs

import (
	"net/http"

	"github.com/tyk-swe/olp/internal/access"
)

type Management struct{ access *access.Server }

func NewManagement(a *access.Server) *Management { return &Management{access: a} }
func (s *Management) Register(mux *http.ServeMux) {
	s.access.Route(mux, "GET /api/v1/agent-runs", s.listRuns)
	s.access.Route(mux, "GET /api/v1/agent-runs/{run_id}", s.getRun)
	s.access.Route(mux, "GET /api/v1/agent-runs/{run_id}/spans", s.listRunSpans)
}
