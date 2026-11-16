package models

import (
	"context"
	"net/http"
	"slices"
	"time"

	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/ingress"
	"github.com/tyk-swe/olp/internal/release"
	"github.com/tyk-swe/olp/internal/routing"
)

// Dependencies are the collaborators the catalog serves through. Begin
// assigns the request identity and headers every inference response carries
// and returns the release the request is pinned to.
type Dependencies struct {
	Gate     *ingress.Gate
	Begin    func(http.ResponseWriter, *http.Request) *release.Release
	Now      func() time.Time
	Workflow func(context.Context, *release.Release, keyauthority.Authority, routing.Route, string) *routing.WorkflowCapability
}

// Handler serves the model catalog surfaces.
type Handler struct {
	gate     *ingress.Gate
	begin    func(http.ResponseWriter, *http.Request) *release.Release
	now      func() time.Time
	workflow func(context.Context, *release.Release, keyauthority.Authority, routing.Route, string) *routing.WorkflowCapability
}

func New(deps Dependencies) *Handler {
	return &Handler{gate: deps.Gate, begin: deps.Begin, now: deps.Now, workflow: deps.Workflow}
}

type capabilities struct {
	routing.RouteCapabilities
	GatewayExecution *routing.WorkflowCapability `json:"gateway_execution,omitempty"`
}

func (h *Handler) capabilities(ctx context.Context, rel *release.Release, authority keyauthority.Authority, route routing.Route, surface string) capabilities {
	return capabilities{
		RouteCapabilities: routing.EffectiveCapabilities(rel.Snapshot(), route),
		GatewayExecution:  h.workflow(ctx, rel, authority, route, surface),
	}
}

func (h *Handler) object(ctx context.Context, rel *release.Release, authority keyauthority.Authority, route *routing.Route) map[string]any {
	return map[string]any{
		"id":           route.Slug,
		"object":       "model",
		"created":      route.PublishedAt.Unix(),
		"owned_by":     "openllmproxy",
		"capabilities": h.capabilities(ctx, rel, authority, *route, "openai"),
	}
}

func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	rel := h.begin(w, r)
	authority, e := h.gate.Authenticate(r, "models_read")
	if e != nil {
		fault.WriteError(w, e)
		return
	}
	now := h.now()
	data := []map[string]any{}
	for _, slug := range rel.Snapshot().RouteSlugs() {
		if authority.Allows("models_read", slug, rel.Snapshot().Route(slug).ProjectID, now) {
			route := rel.Snapshot().Route(slug)
			data = append(data, h.object(r.Context(), rel, authority, &route))
		}
	}
	fault.WriteJSON(w, map[string]any{"object": "list", "data": data})
}

func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	rel := h.begin(w, r)
	authority, e := h.gate.Authenticate(r, "models_read")
	if e != nil {
		fault.WriteError(w, e)
		return
	}
	slug := r.PathValue("model")
	route, ok := rel.Snapshot().FindRoute(slug)
	if !ok || !authority.Allows("models_read", slug, route.ProjectID, h.now()) {
		fault.WriteError(w, fault.ModelNotFound(slug))
		return
	}
	fault.WriteJSON(w, h.object(r.Context(), rel, authority, &route))
}

func (h *Handler) native(ctx context.Context, rel *release.Release, authority keyauthority.Authority, route routing.Route, surface string) map[string]any {
	caps := h.capabilities(ctx, rel, authority, route, surface)
	if surface == "anthropic" {
		return map[string]any{"id": route.Slug, "type": "model", "display_name": route.Slug, "created_at": route.PublishedAt.UTC().Format("2006-01-02T15:04:05Z"), "capabilities": caps}
	}
	methods := []string{}
	if slices.Contains(route.Operations, "generation") {
		methods = append(methods, "generateContent", "streamGenerateContent")
	}
	if slices.Contains(route.Operations, "token_count") {
		methods = append(methods, "countTokens")
	}
	return map[string]any{"name": "models/" + route.Slug, "displayName": route.Slug, "supportedGenerationMethods": methods, "capabilities": caps}
}

func (h *Handler) Native(w http.ResponseWriter, r *http.Request) {
	rel := h.begin(w, r)
	surface := fault.RequestSurface(r)
	authority, e := h.gate.Authenticate(r, "models_read")
	if e != nil {
		fault.WriteSurfaceError(w, e, surface)
		return
	}
	if slug := r.PathValue("model"); slug != "" {
		route, ok := rel.Snapshot().FindRoute(slug)
		if !ok || !authority.Allows("models_read", slug, route.ProjectID, h.now()) {
			fault.WriteSurfaceError(w, fault.ModelNotFound(slug), surface)
			return
		}
		fault.WriteJSON(w, h.native(r.Context(), rel, authority, route, surface))
		return
	}
	rows := []map[string]any{}
	for _, slug := range rel.Snapshot().RouteSlugs() {
		if authority.Allows("models_read", slug, rel.Snapshot().Route(slug).ProjectID, h.now()) {
			rows = append(rows, h.native(r.Context(), rel, authority, rel.Snapshot().Route(slug), surface))
		}
	}
	if surface == "anthropic" {
		var first, last any
		if len(rows) > 0 {
			first = rows[0]["id"]
			last = rows[len(rows)-1]["id"]
		}
		fault.WriteJSON(w, map[string]any{"data": rows, "has_more": false, "first_id": first, "last_id": last})
	} else {
		fault.WriteJSON(w, map[string]any{"models": rows})
	}
}
