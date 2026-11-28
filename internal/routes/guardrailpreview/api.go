package guardrailpreview

import (
	"context"
	"net/http"
	"net/url"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/domain/ids"
	"github.com/tyk-swe/olp/internal/domain/problem"
	"github.com/tyk-swe/olp/internal/guardrails"
	"github.com/tyk-swe/olp/internal/guardrails/attachment"
	"github.com/tyk-swe/olp/internal/platform/database"
	"github.com/tyk-swe/olp/internal/release"
)

type api struct {
	serving func() *release.Release
	query   func(context.Context, string, ...any) pgx.Row
}

func Register(server *access.Server, mux *http.ServeMux, serving func() *release.Release) {
	a := api{serving: serving, query: server.Pool.QueryRow}
	server.Route(mux, "GET /api/v1/guardrails/preview", a.handle)
}

type selection struct {
	routeID, keyID, operation, surface, mode string
	retainsOutput                            bool
}

func parse(r *http.Request) (selection, error) {
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || r.ContentLength != 0 {
		return selection{}, problem.Invalid("preview", "Send only preview query parameters.")
	}
	for name, value := range values {
		if !slices.Contains([]string{"route_id", "api_key_id", "operation", "surface", "mode", "retains_output"}, name) || len(value) != 1 || len(value[0]) > 128 {
			return selection{}, problem.Invalid("preview", "Send only preview query parameters, once each.")
		}
	}
	routeID, err := ids.Parse(values.Get("route_id"))
	if err != nil {
		return selection{}, problem.Invalid("route_id", "Choose a route by ID.")
	}
	keyID, err := ids.Parse(values.Get("api_key_id"))
	if err != nil {
		return selection{}, problem.Invalid("api_key_id", "Choose an API key by ID.")
	}
	mode := values.Get("mode")
	if mode != "unary" && mode != "streaming" {
		return selection{}, problem.Invalid("mode", "Choose unary or streaming.")
	}
	retained := values.Get("retains_output")
	if retained != "" && retained != "true" && retained != "false" {
		return selection{}, problem.Invalid("retains_output", "Use true or false.")
	}
	return selection{routeID, keyID, values.Get("operation"), values.Get("surface"), mode, retained == "true"}, nil
}

type response struct {
	ReleaseID         string                    `json:"release_id"`
	ReleaseSequence   int64                     `json:"release_sequence"`
	Stale             bool                      `json:"stale"`
	PartialVisibility bool                      `json:"partial_visibility"`
	StaticCompatible  *bool                     `json:"static_compatible,omitempty"`
	Entries           []guardrails.PreviewEntry `json:"entries"`
}

func (a api) handle(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	in, err := parse(r)
	if err != nil {
		return httpkit.Reply{}, err
	}
	installed := a.serving()
	var slug string
	var routeProject, keyProject *string
	var latest int64
	err = a.query(r.Context(), `SELECT r.slug,r.project_id::text,k.project_id::text,
		(SELECT COALESCE(max(sequence),0) FROM olp.runtime_releases)
		FROM olp.routes r CROSS JOIN olp.api_keys k WHERE r.id=$1 AND k.id=$2`, in.routeID, in.keyID).
		Scan(&slug, &routeProject, &keyProject, &latest)
	if err != nil {
		return httpkit.Reply{}, database.NotFoundOr(err)
	}
	if p.Project(routeProject, authz.View) != nil || p.Project(keyProject, authz.View) != nil {
		return httpkit.Reply{}, problem.NotFound
	}
	if guardrails.ProjectOf(routeProject) != guardrails.ProjectOf(keyProject) {
		return httpkit.Reply{}, problem.Invalid("api_key_id", "Choose a key in the route's project.")
	}
	if installed == nil || installed.ID == "" {
		return httpkit.Reply{}, problem.Fail(http.StatusServiceUnavailable, "release_unavailable", "No serving release is installed yet.")
	}
	view := installed.Snapshot()
	route, found := view.FindRoute(slug)
	if !found || route.ID != in.routeID || guardrails.ProjectOf(route.ProjectID) != guardrails.ProjectOf(routeProject) {
		return httpkit.Reply{}, problem.NotFound
	}
	profile, err := compatibility(route, in, view.GuardrailWindow())
	if err != nil {
		return httpkit.Reply{}, err
	}
	full := p.AllProjects && p.Authorize(authz.Read) == nil
	entries := view.GuardrailEngine(route.Slug, in.keyID).Preview(profile, func(placement guardrails.Placement) bool {
		return attachment.CanView(p, placement.Scope, placement.ScopeID, route.ProjectID) &&
			p.Project(guardrails.ProjectPointer(placement.ProjectID), authz.View) == nil
	})
	return httpkit.OK(response{installed.ID, installed.Sequence, installed.Sequence < latest, !full, compatible(full, entries), entries}), nil
}

func compatible(full bool, entries []guardrails.PreviewEntry) *bool {
	if !full {
		return nil
	}
	compatible := !slices.ContainsFunc(entries, func(entry guardrails.PreviewEntry) bool { return len(entry.Notes) != 0 })
	return &compatible
}
