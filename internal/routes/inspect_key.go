package routes

import (
	"encoding/json"
	"net/http"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
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
	if err := q.QueryRow(r.Context(), "SELECT policy,expires_at,revoked_at,project_id::text FROM olp.api_keys WHERE id=$1", key.id).Scan(&raw, &authority.ExpiresAt, &authority.RevokedAt, &authority.ProjectID); err != nil {
		return key, err
	}
	if !principal.CanProject(authority.ProjectID, false) {
		return key, pgx.ErrNoRows
	}
	if !sameProject(authority.ProjectID, route.ProjectID) {
		return key, access.Invalid("api_key_id", "The API key and route must belong to the same project.")
	}
	if err := json.Unmarshal(raw, &authority.Policy); err != nil {
		return key, err
	}
	key.allowProviderState = authority.Policy.AllowProviderState
	if !authority.Allows("inference", route.Slug, route.ProjectID, time.Now()) {
		key.reason = "api_key_not_authorized"
		if len(authority.Policy.AllowedRoutes) > 0 && !slices.Contains(authority.Policy.AllowedRoutes, route.Slug) {
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
