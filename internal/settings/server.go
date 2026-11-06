package settings

import (
	"context"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
)

type Server struct {
	access  *access.Server
	publish func(context.Context, pgx.Tx, string) error
}

func New(auth *access.Server, publish func(context.Context, pgx.Tx, string) error) *Server {
	return &Server{access: auth, publish: publish}
}

func (s *Server) Register(mux *http.ServeMux) {
	for pattern, handler := range map[string]access.Handler{
		"GET /api/v1/settings":       s.settings,
		"GET /api/v1/settings/{key}": s.setting,
		"PUT /api/v1/settings/{key}": s.updateSetting,
	} {
		s.access.Route(mux, pattern, handler)
	}
}
