package main

import (
	"errors"
	"net/http"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
	"github.com/tyk-swe/olp/internal/gateway/surface/playground"
	"github.com/tyk-swe/olp/internal/routing"
)

func registerPlayground(server *access.Server, mux *http.ServeMux, gateway *kernel.Server) {
	api := playground.NewAPI(gateway)
	limits := []access.RouteOption{access.MaxBody(playground.BodyLimit), access.Deadline(playground.Timeout)}
	server.Route(mux, "POST /api/v1/playground", func(r *http.Request, principal authz.Principal) (httpkit.Reply, error) {
		var in playground.Request
		if err := httpkit.Decode(r, &in); err != nil {
			return httpkit.Reply{}, err
		}
		out, err := api.Handle(r, principal.ID, playgroundAdmit(principal), in)
		if err != nil {
			return httpkit.Reply{}, playgroundProblem(err)
		}
		return httpkit.Reply{Status: http.StatusOK, Body: out.Body, DisableHTMLEscaping: out.DisableHTMLEscaping}, nil
	}, limits...)
	server.Stream(mux, "POST /api/v1/playground/stream", func(w http.ResponseWriter, r *http.Request, principal authz.Principal) error {
		var in playground.Request
		if err := httpkit.Decode(r, &in); err != nil {
			return err
		}
		return playgroundProblem(api.Stream(w, r, principal.ID, playgroundAdmit(principal), in))
	}, limits...)
}

func playgroundAdmit(principal authz.Principal) playground.Admit {
	return func(route *routing.Route) *fault.Error {
		if principal.Project(route.ProjectID, authz.View) != nil {
			return fault.ModelNotFound(route.Slug)
		}
		return nil
	}
}

func playgroundProblem(err error) error {
	var refused *playground.Error
	if !errors.As(err, &refused) {
		return err
	}
	return &httpkit.Problem{Status: refused.Status, Code: refused.Code, Detail: refused.Message, Field: refused.Field}
}
