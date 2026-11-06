package authority

import (
	"encoding/json"
	"github.com/tyk-swe/olp/internal/routeidentity"
	"maps"
	"slices"
)

// RouteGroups are project-local named sets of ordinary or subscription slugs.
// Missing groups resolve to no routes; references never widen an allowlist.
type RouteGroups map[string][]string

func (g RouteGroups) Validate() error {
	if len(g) > 64 {
		return invalid("groups", "Use at most 64 route groups per project.")
	}
	for name, routes := range g {
		if !routeidentity.ValidSlug(name) || routes == nil || len(routes) > 100 {
			return invalid("groups", "Use group names and route slugs of 1–100 lowercase letters, numbers, dots, underscores or hyphens, and at most 100 routes per group.")
		}
		seen := map[string]bool{}
		for _, route := range routes {
			if !routeidentity.ValidSlug(route) || seen[route] {
				return invalid("groups", "Each group must contain unique valid route slugs.")
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
