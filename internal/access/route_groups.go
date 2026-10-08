package access

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"slices"
)

// RouteGroups are project-local named sets of ordinary or subscription slugs.
// Missing groups resolve to no routes; references never widen an allowlist.
type RouteGroups map[string][]string

func (g RouteGroups) Validate() error {
	if len(g) > 64 {
		return Invalid("groups", "Use at most 64 route groups per project.")
	}
	for name, routes := range g {
		if !RouteSlug.MatchString(name) || routes == nil || len(routes) > 100 {
			return Invalid("groups", "Use group names and route slugs of 1–100 lowercase letters, numbers, dots, underscores or hyphens, and at most 100 routes per group.")
		}
		seen := map[string]bool{}
		for _, route := range routes {
			if !RouteSlug.MatchString(route) || seen[route] {
				return Invalid("groups", "Each group must contain unique valid route slugs.")
			}
			seen[route] = true
		}
	}
	return nil
}
func (g RouteGroups) Equal(other RouteGroups) bool {
	return maps.EqualFunc(g, other, func(a, b []string) bool {
		if len(a) != len(b) {
			return false
		}
		for _, s := range a {
			if !slices.Contains(b, s) {
				return false
			}
		}
		return true
	})
}
func (g RouteGroups) JSON() []byte {
	if len(g) == 0 {
		return []byte("{}")
	}
	value, _ := json.Marshal(g)
	return value
}

// BindRouteGroups compiles the key's references when authority is loaded. The
// private map is immutable afterward, so ordinary requests add no allocation or IO.
func (a *Authority) BindRouteGroups(groups RouteGroups) error {
	a.allowedGroupRoutes = nil
	if len(a.Policy.AllowedRouteGroups) == 0 {
		return nil
	}
	if err := groups.Validate(); err != nil {
		return err
	}
	if a.ProjectID == nil {
		return nil
	}
	a.allowedGroupRoutes = map[string]struct{}{}
	for _, name := range a.Policy.AllowedRouteGroups {
		for _, route := range groups[name] {
			a.allowedGroupRoutes[route] = struct{}{}
		}
	}
	return nil
}
func (a Authority) allowsRoute(route string) bool {
	if len(a.Policy.AllowedRoutes) == 0 && len(a.Policy.AllowedRouteGroups) == 0 {
		return true
	}
	if slices.Contains(a.Policy.AllowedRoutes, route) {
		return true
	}
	_, ok := a.allowedGroupRoutes[route]
	return ok
}
func checkKeyRouteGroups(r *http.Request, q Queryer, names []string, projectID *string) error {
	return checkRouteGroupNames(r.Context(), q, names, projectID)
}
func checkRouteGroupNames(ctx context.Context, q Queryer, names []string, projectID *string) error {
	if len(names) == 0 {
		return nil
	}
	if projectID == nil {
		return Invalid("allowed_route_groups", "Route groups require a project-scoped key.")
	}
	var groups RouteGroups
	if err := q.QueryRow(ctx, "SELECT route_groups FROM olp.projects WHERE id=$1", *projectID).Scan(&groups); err != nil {
		return err
	}
	for _, name := range names {
		if _, ok := groups[name]; !ok {
			return Invalid("allowed_route_groups", "Every referenced group must exist in the key's project.")
		}
	}
	return nil
}
func (s *Server) projectRouteGroups(r *http.Request, p Principal) (Reply, error) {
	id, err := IDParam(r, "project_id")
	if err != nil {
		return Reply{}, err
	}
	if err = p.Project(&id, View); err != nil {
		return Reply{}, err
	}
	var groups RouteGroups
	var etag string
	if err = s.Pool.QueryRow(r.Context(), "SELECT route_groups,etag::text FROM olp.projects WHERE id=$1", id).Scan(&groups, &etag); err != nil {
		return Reply{}, err
	}
	return Detail(map[string]any{"groups": groups, "etag": etag}, etag), nil
}
func (s *Server) putProjectRouteGroups(r *http.Request, _ Principal) (Reply, error) {
	id, err := IDParam(r, "project_id")
	if err != nil {
		return Reply{}, err
	}
	var input struct {
		Groups RouteGroups `json:"groups"`
	}
	if err = DecodeUnique(r, &input, 64<<10); err != nil {
		return Reply{}, err
	}
	if input.Groups == nil {
		return Reply{}, Invalid("groups", "Provide the group map; an empty object removes all groups.")
	}
	if err = input.Groups.Validate(); err != nil {
		return Reply{}, err
	}
	tx, err := s.Begin(r)
	if err != nil {
		return Reply{}, err
	}
	defer tx.Rollback(r.Context())
	p, err := s.Reauthorize(r, tx)
	if err != nil {
		return Reply{}, err
	}
	if err = p.Project(&id, Change); err != nil {
		return Reply{}, err
	}
	etag, err := loadProject(r, tx, id)
	if err != nil {
		return Reply{}, err
	}
	if err = Match(r, etag); err != nil {
		return Reply{}, err
	}
	for _, routes := range input.Groups {
		if err = checkKeyRoutes(r, tx, routes, &id); err != nil {
			return Reply{}, err
		}
	}
	etag = NewID()
	if _, err = tx.Exec(r.Context(), "UPDATE olp.projects SET route_groups=$2::jsonb,etag=$3,updated_at=now() WHERE id=$1", id, input.Groups.JSON(), etag); err != nil {
		return Reply{}, err
	}
	if _, err = AdvanceAuthority(r.Context(), tx); err != nil {
		return Reply{}, err
	}
	if err = Audit(r.Context(), tx, r, p.Actor(), "project.route_groups.update", "project", id, "success"); err != nil {
		return Reply{}, err
	}
	return Commit(r, tx, Detail(map[string]any{"groups": input.Groups, "etag": etag}, etag))
}

// RouteAllowlist is for collection queries: nil is unrestricted; a non-nil
// empty slice grants no routes. The caller owns the returned restricted slice.
func (a Authority) RouteAllowlist() []string {
	if len(a.Policy.AllowedRoutes) == 0 && len(a.Policy.AllowedRouteGroups) == 0 {
		return nil
	}
	routes := make([]string, 0, len(a.Policy.AllowedRoutes)+len(a.allowedGroupRoutes))
	routes = append(routes, a.Policy.AllowedRoutes...)
	for route := range a.allowedGroupRoutes {
		if !slices.Contains(routes, route) {
			routes = append(routes, route)
		}
	}
	return routes
}
