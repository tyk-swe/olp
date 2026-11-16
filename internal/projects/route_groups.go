package projects

import (
	"net/http"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/access/httpkit"
)

func (s *Server) projectRouteGroups(r *http.Request, p access.Principal) (httpkit.Reply, error) {
	id, err := httpkit.IDParam(r, "project_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	if err = p.Project(&id, access.View); err != nil {
		return httpkit.Reply{}, err
	}
	var groups authority.RouteGroups
	var etag string
	if err = s.access.Pool.QueryRow(r.Context(), "SELECT route_groups,etag::text FROM olp.projects WHERE id=$1", id).Scan(&groups, &etag); err != nil {
		return httpkit.Reply{}, err
	}
	return httpkit.Detail(map[string]any{"groups": groups, "etag": etag}, etag), nil
}

func (s *Server) putProjectRouteGroups(r *http.Request, _ access.Principal) (httpkit.Reply, error) {
	id, err := httpkit.IDParam(r, "project_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	var input struct {
		Groups authority.RouteGroups `json:"groups"`
	}
	if err = httpkit.DecodeUnique(r, &input, 64<<10); err != nil {
		return httpkit.Reply{}, err
	}
	if input.Groups == nil {
		return httpkit.Reply{}, httpkit.Invalid("groups", "Provide the group map; an empty object removes all groups.")
	}
	if err = input.Groups.Validate(); err != nil {
		return httpkit.Reply{}, err
	}
	tx, p, err := s.access.BeginMutation(r)
	if err != nil {
		return httpkit.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	if err = p.Project(&id, access.Change); err != nil {
		return httpkit.Reply{}, err
	}
	current, etag, err := ReadPolicies(r.Context(), tx, id)
	if err != nil {
		return httpkit.Reply{}, err
	}
	if err = httpkit.Match(r, etag); err != nil {
		return httpkit.Reply{}, err
	}
	for _, routes := range input.Groups {
		if err = access.ValidateRouteReferences(r, tx, routes, &id); err != nil {
			return httpkit.Reply{}, err
		}
	}
	etag, err = WritePolicies(r.Context(), tx, id, Policies{EndUser: current.EndUser, Attribution: current.Attribution, RouteGroups: input.Groups, Budget: current.Budget, Templates: current.Templates, AttributionBudgets: current.AttributionBudgets})
	if err != nil {
		return httpkit.Reply{}, err
	}
	if _, err = access.AdvanceAuthority(r.Context(), tx); err != nil {
		return httpkit.Reply{}, err
	}
	if err = access.Audit(r.Context(), tx, r, p.Actor(), "project.route_groups.update", "project", id, "success"); err != nil {
		return httpkit.Reply{}, err
	}
	return httpkit.Detail(map[string]any{"groups": input.Groups, "etag": etag}, etag), tx.Commit(r.Context())
}
