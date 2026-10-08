package routes

import (
	"encoding/json"
	"net/http"
	"slices"
	"time"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/runtime"
)

type inspectionKeyContext struct {
	id, reason         string
	allowProviderState bool
}

func (s *Server) inspectionKey(r *http.Request, q access.Queryer, principal access.Principal, selected *string, route runtime.Route) (inspectionKeyContext, error) {
	key := inspectionKeyContext{}
	if selected == nil {
		return key, nil
	}
	var err error
	key.id, err = access.ParseUUID(*selected)
	if err != nil {
		return key, access.Invalid("api_key_id", "Use an API key identifier.")
	}
	var authority access.Authority
	var raw []byte
	var groups access.RouteGroups
	if err := q.QueryRow(r.Context(), "SELECT k.policy,k.expires_at,k.revoked_at,k.project_id::text,COALESCE(p.route_groups,'{}'::jsonb) FROM olp.api_keys k LEFT JOIN olp.projects p ON p.id=k.project_id WHERE k.id=$1", key.id).Scan(&raw, &authority.ExpiresAt, &authority.RevokedAt, &authority.ProjectID, &groups); err != nil {
		return key, err
	}
	if err := principal.Project(authority.ProjectID, access.View); err != nil {
		return key, err
	}
	if !sameProject(authority.ProjectID, route.ProjectID) {
		return key, access.Invalid("api_key_id", "The API key and route must belong to the same project.")
	}
	if err := json.Unmarshal(raw, &authority.Policy); err != nil {
		return key, err
	}
	if err := authority.BindRouteGroups(groups); err != nil {
		return key, err
	}
	key.allowProviderState = authority.Policy.AllowProviderState
	if !authority.Allows("inference", route.Slug, route.ProjectID, time.Now()) {
		key.reason = "api_key_not_authorized"
		if allowed := authority.RouteAllowlist(); allowed != nil && !slices.Contains(allowed, route.Slug) {
			key.reason = "route_not_allowed_for_key"
		}
	}
	return key, nil
}

func applyInspectionKeyReason(decisions []runtime.Decision, reason string) {
	if reason == "" {
		return
	}
	for i := range decisions {
		decisions[i].Eligible = false
		decisions[i].Attempt = nil
		decisions[i].Reason = &reason
	}
}
