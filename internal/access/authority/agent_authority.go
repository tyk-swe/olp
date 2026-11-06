package authority

import (
	"slices"
	"time"
)

// AllowsResource is deliberately separate from route authorization. A route
// group, inference scope, or installation-wide key cannot grant project tools.
// The caller also checks the resource's current publication and revocation.
func (a Authority) AllowsResource(scope, id, project string, now time.Time) bool {
	if a.ProjectID == nil || *a.ProjectID != project || a.RevokedAt != nil || a.ExpiresAt != nil && !a.ExpiresAt.After(now) || !slices.Contains(a.Policy.Scopes, scope) {
		return false
	}
	switch scope {
	case "tools":
		return slices.Contains(a.Policy.AllowedToolsets, id)
	case "agents":
		return slices.Contains(a.Policy.AllowedAgents, id)
	case "traces_write":
		return id == ""
	default:
		return false
	}
}
