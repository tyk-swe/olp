package projects

import (
	"bytes"
	"encoding/json"
	"net/http"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/authority"
)

func (s *Server) projectEndUserPolicy(r *http.Request, p access.Principal) (access.Reply, error) {
	id, err := access.IDParam(r, "project_id")
	if err != nil {
		return access.Reply{}, err
	}
	if err = p.Project(&id, access.View); err != nil {
		return access.Reply{}, err
	}
	var policy json.RawMessage
	var etag string
	if err = s.access.Pool.QueryRow(r.Context(), "SELECT COALESCE(end_user_policy,'null'::jsonb),etag::text FROM olp.projects WHERE id=$1", id).Scan(&policy, &etag); err != nil {
		return access.Reply{}, err
	}
	return access.Detail(map[string]any{"policy": policy, "etag": etag}, etag), nil
}

func (s *Server) putProjectEndUserPolicy(r *http.Request, _ access.Principal) (access.Reply, error) {
	id, err := access.IDParam(r, "project_id")
	if err != nil {
		return access.Reply{}, err
	}
	var body struct {
		Policy json.RawMessage `json:"policy"`
	}
	if err = access.Decode(r, &body); err != nil {
		return access.Reply{}, err
	}
	if len(body.Policy) == 0 {
		return access.Reply{}, access.Invalid("policy", "A policy or explicit null is required.")
	}
	var input struct{ Policy *authority.EndUserPolicy }
	decoder := json.NewDecoder(bytes.NewReader(body.Policy))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&input.Policy); err != nil {
		return access.Reply{}, access.Invalid("policy", "The end-user policy is invalid.")
	}
	if err = input.Policy.ValidateAndNormalize(); err != nil {
		return access.Reply{}, err
	}
	tx, p, err := s.access.BeginMutation(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	if err = p.Project(&id, access.Change); err != nil {
		return access.Reply{}, err
	}
	current, etag, err := ReadPolicies(r.Context(), tx, id)
	if err != nil {
		return access.Reply{}, err
	}
	if err = access.Match(r, etag); err != nil {
		return access.Reply{}, err
	}
	var template *string
	if input.Policy != nil {
		template = input.Policy.LimitTemplate
	}
	if err = access.CheckLimitTemplates(r.Context(), tx, &id, template); err != nil {
		return access.Reply{}, err
	}
	etag, err = WritePolicies(r.Context(), tx, id, Policies{EndUser: input.Policy, Attribution: current.Attribution, RouteGroups: current.RouteGroups, Budget: current.Budget, Templates: current.Templates, AttributionBudgets: current.AttributionBudgets})
	if err != nil {
		return access.Reply{}, err
	}
	if _, err = access.AdvanceAuthority(r.Context(), tx); err != nil {
		return access.Reply{}, err
	}
	if err = access.Audit(r.Context(), tx, r, p.Actor(), "project.end_user_policy.update", "project", id, "success"); err != nil {
		return access.Reply{}, err
	}
	return access.Detail(map[string]any{"policy": input.Policy, "etag": etag}, etag), tx.Commit(r.Context())
}
