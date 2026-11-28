package main

import (
	"net/http"

	"github.com/tyk-swe/olp/internal/apikeys"
	"github.com/tyk-swe/olp/internal/auditlog"
	"github.com/tyk-swe/olp/internal/budgets"
	"github.com/tyk-swe/olp/internal/projects"
	"github.com/tyk-swe/olp/internal/settings"
)

func (m Management) registerAccess(mux *http.ServeMux) {
	m.Identity.Register(mux)
	projects.New(m.Access).Register(mux)
	settings.NewAPI(m.Access).Register(mux)
	budgets.NewAPI(m.Access).Register(mux)
	apikeys.NewAPI(m.Access, m.serving.overlaps).Register(mux)
	auditlog.NewAPI(m.Access).Register(mux)
}
