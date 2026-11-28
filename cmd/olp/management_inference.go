package main

import (
	"net/http"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/gateway/surface/playground"
	jobsmanage "github.com/tyk-swe/olp/internal/media/jobs/manage"
	resourcesmanage "github.com/tyk-swe/olp/internal/resources/manage"
	"github.com/tyk-swe/olp/internal/retrieval"
)

func (m Management) registerInference(mux *http.ServeMux) {
	(&retrieval.Server{Access: m.Access}).Register(mux)
	playgroundAPI := playground.NewAPI(m.serving.gateway)
	playgroundLimits := []access.RouteOption{access.MaxBody(playground.BodyLimit), access.Deadline(playground.Timeout)}
	m.Access.Route(mux, "POST /api/v1/playground", playgroundAPI.Handle, playgroundLimits...)
	m.Access.Stream(mux, "POST /api/v1/playground/stream", playgroundAPI.Stream, playgroundLimits...)
	(&jobsmanage.API{Access: m.Access, Jobs: m.media.service, Spool: m.media.spool, Log: m.log}).Register(mux)
	resourcesmanage.New(m.Access).Register(mux)
}
