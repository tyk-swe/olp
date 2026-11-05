package investigations

import (
	"net/http"

	"github.com/tyk-swe/olp/internal/access"
)

type Management struct{ access *access.Server }

func NewManagement(a *access.Server) *Management { return &Management{access: a} }
func (s *Management) Register(mux *http.ServeMux) {
	s.access.Route(mux, "GET /api/v1/investigations", s.listInvestigations)
	s.access.Route(mux, "POST /api/v1/investigations", s.createInvestigation, access.MaxBody(32<<10))
	s.access.Route(mux, "GET /api/v1/investigations/{investigation_id}", s.getInvestigation)
}
