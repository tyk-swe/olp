package projects

import (
	"bytes"
	"encoding/json"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/authz"
)

func (s *Server) projectEndUserPolicy(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	id, err := httpkit.IDParam(r, "project_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	if err = p.Project(&id, authz.View); err != nil {
		return httpkit.Reply{}, err
	}
	var policy json.RawMessage
	var etag string
	if err = s.access.Pool.QueryRow(r.Context(), "SELECT COALESCE(end_user_policy,'null'::jsonb),etag::text FROM olp.projects WHERE id=$1", id).Scan(&policy, &etag); err != nil {
		return httpkit.Reply{}, err
	}
	return httpkit.Detail(map[string]any{"policy": policy, "etag": etag}, etag), nil
}

func (s *Server) putProjectEndUserPolicy(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	id, err := httpkit.IDParam(r, "project_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	var body struct {
		Policy json.RawMessage `json:"policy"`
	}
	if err = httpkit.Decode(r, &body); err != nil {
		return httpkit.Reply{}, err
	}
	if len(body.Policy) == 0 {
		return httpkit.Reply{}, httpkit.Invalid("policy", "A policy or explicit null is required.")
	}
	var input struct{ Policy *authority.EndUserPolicy }
	decoder := json.NewDecoder(bytes.NewReader(body.Policy))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&input.Policy); err != nil {
		return httpkit.Reply{}, httpkit.Invalid("policy", "The end-user policy is invalid.")
	}
	if err = input.Policy.ValidateAndNormalize(); err != nil {
		return httpkit.Reply{}, err
	}
	return access.Transact(s.access, r, access.Unlocked, func(tx pgx.Tx, p authz.Principal, _ struct{}) (httpkit.Reply, access.Event, error) {
		if err = p.Project(&id, authz.Change); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		current, etag, err := ReadPolicies(r.Context(), tx, id)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if err = httpkit.Match(r, etag); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		var template *string
		if input.Policy != nil {
			template = input.Policy.LimitTemplate
		}
		if err = access.CheckLimitTemplates(r.Context(), tx, &id, template); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		etag, err = WritePolicies(r.Context(), tx, id, Policies{EndUser: input.Policy, Attribution: current.Attribution, RouteGroups: current.RouteGroups, Budget: current.Budget, Templates: current.Templates, AttributionBudgets: current.AttributionBudgets})
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if _, err = access.AdvanceAuthority(r.Context(), tx); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		return httpkit.Detail(map[string]any{"policy": input.Policy, "etag": etag}, etag), access.Event{Action: "project.end_user_policy.update", Resource: "project", ID: id}, nil
	})
}
