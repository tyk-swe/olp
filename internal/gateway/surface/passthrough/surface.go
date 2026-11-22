package passthrough

import (
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
	"github.com/tyk-swe/olp/internal/routing"
)

type surface struct{ kernel *kernel.Server }

func Register(k *kernel.Server, mux *http.ServeMux) {
	s := surface{kernel: k}
	for _, method := range routing.PassthroughMethods() {
		mux.HandleFunc(method+" /native/{dialect}/routes/{route}/{path...}", s.serve)
	}
}

func (s surface) refuse(x *kernel.Call, w http.ResponseWriter, e *fault.Error) {
	s.kernel.RefuseOn(x, w, e, "openai")
}

func (s surface) serve(w http.ResponseWriter, r *http.Request) {
	x, authority, ok := s.kernel.EnterPassthrough(w, r)
	if !ok {
		return
	}
	defer s.kernel.Gate.Release(r.Context())
	route, e := s.route(r, x, authority)
	if e != nil {
		s.refuse(x, w, e)
		return
	}
	x.Route = &route
	if e := s.kernel.ReserveState(r.Context(), x, authority, time.Duration(route.OverallTimeout)*time.Millisecond); e != nil {
		s.refuse(x, w, e)
		return
	}
	defer s.kernel.SettleAdmission(r.Context(), x)
	path, query, e := upstreamPath(r, &route)
	if e != nil {
		s.refuse(x, w, e)
		return
	}
	s.exchange(w, r, x, path, query)
}

func (s surface) route(r *http.Request, x *kernel.Call, authority keyauthority.Authority) (routing.Route, *fault.Error) {
	snapshot := x.Snapshot()
	if snapshot == nil {
		return routing.Route{}, fault.ServerError(http.StatusServiceUnavailable, "upstream_unavailable", "No serving release is installed.")
	}
	slug := r.PathValue("route")
	route, ok := snapshot.FindRoute(slug)
	if !ok || !route.IsPassthrough() || route.Passthrough.Dialect != r.PathValue("dialect") || !authority.Allows("inference", slug, route.ProjectID, s.kernel.Now()) || !slices.Contains(authority.Policy.AllowedRoutes, slug) {
		return routing.Route{}, fault.ModelNotFound(slug)
	}
	return route, nil
}

func upstreamPath(r *http.Request, route *routing.Route) (string, url.Values, *fault.Error) {
	if isWebSocketUpgrade(r) {
		return "", nil, fault.InvalidRequest("passthrough_websocket_refused", "Pass-through routes do not accept WebSocket upgrades.", nil)
	}
	if r.URL.IsAbs() || r.URL.Host != "" || r.URL.Fragment != "" {
		return "", nil, pathRefused()
	}
	escaped, ok := passthroughSuffix(r)
	if !ok {
		return "", nil, pathMismatch()
	}
	matched, err := route.Passthrough.Match(r.Method, escaped)
	if err != nil {
		return "", nil, pathRefused()
	}
	if !matched {
		return "", nil, pathMismatch()
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return "", nil, fault.InvalidRequest("passthrough_query_refused", "The pass-through query is malformed.", nil)
	}
	for name := range query {
		if !slices.Contains(route.Passthrough.Query, name) {
			return "", nil, fault.InvalidRequest("passthrough_query_refused", "Query name `"+name+"` is not allowed on this pass-through route.", nil)
		}
	}
	normalized, _ := routing.NormalizePassthroughPath(escaped)
	return normalized, query, nil
}

func pathRefused() *fault.Error {
	return fault.InvalidRequest("passthrough_path_refused", "The pass-through path is not an anchored slash path.", nil)
}

func pathMismatch() *fault.Error {
	return fault.NotFoundError("route_not_found", "The pass-through path does not match this route.")
}

func isWebSocketUpgrade(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket") || strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade")
}

func passthroughSuffix(r *http.Request) (string, bool) {
	parts := strings.Split(r.URL.EscapedPath(), "/")
	if len(parts) < 6 || parts[1] != "native" || parts[3] != "routes" {
		return "", false
	}
	return "/" + strings.Join(parts[5:], "/"), true
}
