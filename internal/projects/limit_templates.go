package projects

import (
	"context"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/domain/problem"
	"github.com/tyk-swe/olp/internal/keyauthority"
	"github.com/tyk-swe/olp/internal/platform/database"
)

// ValidateTemplateReferences prevents removing templates still used by live local
// keys/groups, enabled workload mappings or the project policy, including during
// configuration promotion. Workload principals derive limits from their issuer.
func ValidateTemplateReferences(ctx context.Context, q database.Queryer, project string, t keyauthority.LimitTemplates) error {
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
			return problem.Fail(409, "template_in_use", "A referenced template cannot be removed; detach its members first.")
		}
	}
	return nil
}

func (s *Server) projectLimitTemplates(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	id, err := httpkit.IDParam(r, "project_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	if err = p.Project(&id, authz.View); err != nil {
		return httpkit.Reply{}, err
	}
	var templates keyauthority.LimitTemplates
	var etag string
	if err = s.access.Pool.QueryRow(r.Context(), "SELECT limit_templates,etag::text FROM olp.projects WHERE id=$1", id).Scan(&templates, &etag); err != nil {
		return httpkit.Reply{}, err
	}
	return httpkit.Detail(map[string]any{"templates": templates, "etag": etag}, etag), nil
}

func (s *Server) putProjectLimitTemplates(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	id, err := httpkit.IDParam(r, "project_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	var input struct {
		Templates keyauthority.LimitTemplates `json:"templates"`
	}
	if err = httpkit.DecodeUnique(r, &input, 64<<10); err != nil {
		return httpkit.Reply{}, err
	}
	if input.Templates == nil {
		return httpkit.Reply{}, problem.Invalid("templates", "Provide the template map, including an empty object to clear unused templates.")
	}
	if err = input.Templates.ValidateAndNormalize(); err != nil {
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
		if err = ValidateTemplateReferences(r.Context(), tx, id, input.Templates); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		etag, err = WritePolicies(r.Context(), tx, id, Policies{EndUser: current.EndUser, Attribution: current.Attribution, RouteGroups: current.RouteGroups, Budget: current.Budget, Templates: input.Templates, AttributionBudgets: current.AttributionBudgets})
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if _, err = access.AdvanceAuthority(r.Context(), tx); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		return httpkit.Detail(map[string]any{"templates": input.Templates, "etag": etag}, etag), access.Event{Action: "project.limit_templates.update", Resource: "project", ID: id}, nil
	})
}
