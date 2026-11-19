package projects

import (
	"context"
	"net/http"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/database"
)

// ValidateTemplateReferences prevents removing templates still used by live local
// keys/groups, enabled workload mappings or the project policy, including during
// configuration promotion. Workload principals derive limits from their issuer.
func ValidateTemplateReferences(ctx context.Context, q database.Queryer, project string, t authority.LimitTemplates) error {
	var names []string
	err := q.QueryRow(ctx, `SELECT ARRAY(SELECT DISTINCT name FROM (
 SELECT policy->>'limit_template' name FROM olp.api_keys WHERE project_id=$1 AND workload_issuer_id IS NULL AND revoked_at IS NULL
 UNION ALL SELECT policy->'end_user_policy'->>'limit_template' FROM olp.api_keys WHERE project_id=$1 AND workload_issuer_id IS NULL AND revoked_at IS NULL
 UNION ALL SELECT limit_template FROM olp.budget_groups WHERE project_id=$1
 UNION ALL SELECT end_user_policy->>'limit_template' FROM olp.projects WHERE id=$1
 UNION ALL SELECT m->>'limit_template' FROM olp.workload_issuers i CROSS JOIN LATERAL jsonb_array_elements(i.document->'mappings') m WHERE (i.document->>'enabled')::boolean AND m->>'project_id'=$1::text
 ) refs WHERE name IS NOT NULL)`, project).Scan(&names)
	if err != nil {
		return err
	}
	for _, name := range names {
		if _, ok := t[name]; !ok {
			return httpkit.Fail(409, "template_in_use", "A referenced template cannot be removed; detach its members first.")
		}
	}
	return nil
}

func (s *Server) projectLimitTemplates(r *http.Request, p access.Principal) (httpkit.Reply, error) {
	id, err := httpkit.IDParam(r, "project_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	if err = p.Project(&id, access.View); err != nil {
		return httpkit.Reply{}, err
	}
	var templates authority.LimitTemplates
	var etag string
	if err = s.access.Pool.QueryRow(r.Context(), "SELECT limit_templates,etag::text FROM olp.projects WHERE id=$1", id).Scan(&templates, &etag); err != nil {
		return httpkit.Reply{}, err
	}
	return httpkit.Detail(map[string]any{"templates": templates, "etag": etag}, etag), nil
}

func (s *Server) putProjectLimitTemplates(r *http.Request, _ access.Principal) (httpkit.Reply, error) {
	id, err := httpkit.IDParam(r, "project_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	var input struct {
		Templates authority.LimitTemplates `json:"templates"`
	}
	if err = httpkit.DecodeUnique(r, &input, 64<<10); err != nil {
		return httpkit.Reply{}, err
	}
	if input.Templates == nil {
		return httpkit.Reply{}, httpkit.Invalid("templates", "Provide the template map, including an empty object to clear unused templates.")
	}
	if err = input.Templates.ValidateAndNormalize(); err != nil {
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
	if err = ValidateTemplateReferences(r.Context(), tx, id, input.Templates); err != nil {
		return httpkit.Reply{}, err
	}
	etag, err = WritePolicies(r.Context(), tx, id, Policies{EndUser: current.EndUser, Attribution: current.Attribution, RouteGroups: current.RouteGroups, Budget: current.Budget, Templates: input.Templates, AttributionBudgets: current.AttributionBudgets})
	if err != nil {
		return httpkit.Reply{}, err
	}
	if _, err = access.AdvanceAuthority(r.Context(), tx); err != nil {
		return httpkit.Reply{}, err
	}
	if err = access.Audit(r.Context(), tx, r, p.Actor(), "project.limit_templates.update", "project", id, "success"); err != nil {
		return httpkit.Reply{}, err
	}
	return httpkit.Detail(map[string]any{"templates": input.Templates, "etag": etag}, etag), tx.Commit(r.Context())
}
