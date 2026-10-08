package access

import (
	"context"
	"encoding/json"
	"maps"
	"math/big"
	"net/http"
)

// LimitTemplates supplies independent ceilings at each referencing boundary.
// Templates share policy, not counters. Inline settings may only tighten them.
type LimitTemplates map[string]AdmissionLimits

func (t LimitTemplates) Validate() error {
	if len(t) > 64 {
		return Invalid("templates", "Use at most 64 project limit templates.")
	}
	for name, limits := range t {
		if !RouteSlug.MatchString(name) {
			return Invalid("templates", "Use valid route-style names for templates.")
		}
		normalized, err := validatedTemplateLimits(limits)
		if err != nil {
			return err
		}
		t[name] = normalized
	}
	return nil
}

func (t LimitTemplates) JSON() []byte {
	if t == nil {
		return []byte("{}")
	}
	b, _ := json.Marshal(t)
	return b
}

func validTemplateName(name *string) error {
	if name != nil && !RouteSlug.MatchString(*name) {
		return Invalid("limit_template", "Use a valid template name or null.")
	}
	return nil
}

func (t LimitTemplates) resolve(name *string, local AdmissionLimits) (AdmissionLimits, error) {
	if name == nil {
		return local, nil
	}
	base, ok := t[*name]
	if !ok {
		return AdmissionLimits{}, Invalid("limit_template", "The referenced template must exist in the same project.")
	}
	local, err := validatedTemplateLimits(local)
	if err != nil {
		return AdmissionLimits{}, err
	}
	base, err = validatedTemplateLimits(base)
	if err != nil {
		return AdmissionLimits{}, err
	}
	return intersectLimits(local, base), nil
}

// Validation may canonicalize decimal strings; keep cached/source pointers intact.
func validatedTemplateLimits(l AdmissionLimits) (AdmissionLimits, error) {
	if l.DailyCostLimit != nil {
		value := *l.DailyCostLimit
		l.DailyCostLimit = &value
	}
	if l.MonthlyCostLimit != nil {
		value := *l.MonthlyCostLimit
		l.MonthlyCostLimit = &value
	}
	if l.WeeklyCostLimit != nil {
		value := *l.WeeklyCostLimit
		l.WeeklyCostLimit = &value
	}
	return l, l.Validate()
}

func intersectLimits(a, b AdmissionLimits) AdmissionLimits {
	minimum := func(a, b *int64) *int64 {
		if a == nil || (b != nil && *b < *a) {
			return b
		}
		return a
	}
	money := func(a, b *string) *string {
		if a == nil {
			return b
		}
		if b == nil {
			return a
		}
		x, _ := new(big.Rat).SetString(*a)
		y, _ := new(big.Rat).SetString(*b)
		if y.Cmp(x) < 0 {
			return b
		}
		return a
	}
	return AdmissionLimits{minimum(a.RequestsPerMinute, b.RequestsPerMinute), minimum(a.TokensPerMinute, b.TokensPerMinute), minimum(a.MaxConcurrency, b.MaxConcurrency), money(a.DailyCostLimit, b.DailyCostLimit), money(a.MonthlyCostLimit, b.MonthlyCostLimit), money(a.WeeklyCostLimit, b.WeeklyCostLimit)}
}

func (p KeyPolicy) AdmissionLimits() AdmissionLimits {
	return AdmissionLimits{p.RequestsPerMinute, p.TokensPerMinute, p.MaxConcurrency, p.DailyCostLimit, p.MonthlyCostLimit, p.WeeklyCostLimit}
}

func (t LimitTemplates) endUser(p *EndUserPolicy) (*EndUserPolicy, error) {
	if p == nil || p.LimitTemplate == nil {
		return p, nil
	}
	copy := *p
	var err error
	if copy.Defaults, err = t.resolve(p.LimitTemplate, p.Defaults); err != nil {
		return nil, err
	}
	copy.Overrides = maps.Clone(p.Overrides)
	for digest, local := range copy.Overrides {
		copy.Overrides[digest], err = t.resolve(p.LimitTemplate, local)
		if err != nil {
			return nil, err
		}
	}
	return &copy, nil
}

func (a Authority) UsesLimitTemplates() bool {
	return a.Policy.LimitTemplate != nil || a.BudgetGroupTemplate != nil || (a.Policy.EndUserPolicy != nil && a.Policy.EndUserPolicy.LimitTemplate != nil) || (a.ProjectEndUserPolicy != nil && a.ProjectEndUserPolicy.LimitTemplate != nil)
}

// BindLimitTemplates compiles immutable effective policies at authority refresh.
func (a *Authority) BindLimitTemplates(t LimitTemplates) error {
	if !a.UsesLimitTemplates() {
		return nil
	}
	if a.ProjectID == nil {
		return Invalid("limit_template", "Templates require a project.")
	}
	local, err := t.resolve(a.Policy.LimitTemplate, a.Policy.AdmissionLimits())
	if err != nil {
		return err
	}
	a.Policy.RequestsPerMinute, a.Policy.TokensPerMinute, a.Policy.MaxConcurrency = local.RequestsPerMinute, local.TokensPerMinute, local.MaxConcurrency
	a.Policy.DailyCostLimit, a.Policy.MonthlyCostLimit = local.DailyCostLimit, local.MonthlyCostLimit
	a.Policy.WeeklyCostLimit = local.WeeklyCostLimit
	if a.Policy.EndUserPolicy, err = t.endUser(a.Policy.EndUserPolicy); err != nil {
		return err
	}
	if a.ProjectEndUserPolicy, err = t.endUser(a.ProjectEndUserPolicy); err != nil {
		return err
	}
	group, err := t.resolve(a.BudgetGroupTemplate, AdmissionLimits{a.BudgetGroupRPM, a.BudgetGroupTPM, a.BudgetGroupConcurrency, a.BudgetGroupDailyCostLimit, a.BudgetGroupMonthlyCostLimit, a.BudgetGroupWeeklyCostLimit})
	if err != nil {
		return err
	}
	a.BudgetGroupRPM, a.BudgetGroupTPM, a.BudgetGroupConcurrency = group.RequestsPerMinute, group.TokensPerMinute, group.MaxConcurrency
	a.BudgetGroupDailyCostLimit, a.BudgetGroupMonthlyCostLimit = group.DailyCostLimit, group.MonthlyCostLimit
	a.BudgetGroupWeeklyCostLimit = group.WeeklyCostLimit
	return nil
}

func checkLimitTemplates(ctx context.Context, q Queryer, project *string, names ...*string) error {
	used := false
	for _, name := range names {
		if name != nil {
			used = true
		}
	}
	if !used {
		return nil
	}
	if project == nil {
		return Invalid("limit_template", "Templates require a project.")
	}
	var templates LimitTemplates
	if err := q.QueryRow(ctx, "SELECT limit_templates FROM olp.projects WHERE id=$1", *project).Scan(&templates); err != nil {
		return err
	}
	for _, name := range names {
		if _, err := templates.resolve(name, AdmissionLimits{}); err != nil {
			return err
		}
	}
	return nil
}

func endUserTemplate(p *EndUserPolicy) *string {
	if p == nil {
		return nil
	}
	return p.LimitTemplate
}

// ValidateTemplateReferences prevents removing templates still used by local
// keys/groups, enabled workload mappings or the project policy, including during
// configuration promotion. Workload principals derive limits from their issuer.
func ValidateTemplateReferences(ctx context.Context, q Queryer, project string, t LimitTemplates) error {
	var names []string
	err := q.QueryRow(ctx, `SELECT ARRAY(SELECT DISTINCT name FROM (
 SELECT policy->>'limit_template' name FROM olp.api_keys WHERE project_id=$1 AND workload_issuer_id IS NULL
 UNION ALL SELECT policy->'end_user_policy'->>'limit_template' FROM olp.api_keys WHERE project_id=$1 AND workload_issuer_id IS NULL
 UNION ALL SELECT limit_template FROM olp.budget_groups WHERE project_id=$1
 UNION ALL SELECT end_user_policy->>'limit_template' FROM olp.projects WHERE id=$1
 UNION ALL SELECT m->>'limit_template' FROM olp.workload_issuers i CROSS JOIN LATERAL jsonb_array_elements(i.document->'mappings') m WHERE (i.document->>'enabled')::boolean AND m->>'project_id'=$1::text
 ) refs WHERE name IS NOT NULL)`, project).Scan(&names)
	if err != nil {
		return err
	}
	for _, name := range names {
		if _, ok := t[name]; !ok {
			return Fail(409, "template_in_use", "A referenced template cannot be removed; detach its members first.")
		}
	}
	return nil
}

func (s *Server) projectLimitTemplates(r *http.Request, p Principal) (Reply, error) {
	id, err := IDParam(r, "project_id")
	if err != nil {
		return Reply{}, err
	}
	if err = p.Project(&id, View); err != nil {
		return Reply{}, err
	}
	var templates LimitTemplates
	var etag string
	if err = s.Pool.QueryRow(r.Context(), "SELECT limit_templates,etag::text FROM olp.projects WHERE id=$1", id).Scan(&templates, &etag); err != nil {
		return Reply{}, err
	}
	return Detail(map[string]any{"templates": templates, "etag": etag}, etag), nil
}

func (s *Server) putProjectLimitTemplates(r *http.Request, _ Principal) (Reply, error) {
	id, err := IDParam(r, "project_id")
	if err != nil {
		return Reply{}, err
	}
	var input struct {
		Templates LimitTemplates `json:"templates"`
	}
	if err = DecodeUnique(r, &input, 64<<10); err != nil {
		return Reply{}, err
	}
	if input.Templates == nil {
		return Reply{}, Invalid("templates", "Provide the template map, including an empty object to clear unused templates.")
	}
	if err = input.Templates.Validate(); err != nil {
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
	if err = ValidateTemplateReferences(r.Context(), tx, id, input.Templates); err != nil {
		return Reply{}, err
	}
	etag = NewID()
	if _, err = tx.Exec(r.Context(), "UPDATE olp.projects SET limit_templates=$2::jsonb,etag=$3,updated_at=now() WHERE id=$1", id, input.Templates.JSON(), etag); err != nil {
		return Reply{}, err
	}
	if _, err = AdvanceAuthority(r.Context(), tx); err != nil {
		return Reply{}, err
	}
	if err = Audit(r.Context(), tx, r, p.Actor(), "project.limit_templates.update", "project", id, "success"); err != nil {
		return Reply{}, err
	}
	return Commit(r, tx, Detail(map[string]any{"templates": input.Templates, "etag": etag}, etag))
}

func (a Authority) GroupLimits() AdmissionLimits {
	return AdmissionLimits{RequestsPerMinute: a.BudgetGroupRPM, TokensPerMinute: a.BudgetGroupTPM, MaxConcurrency: a.BudgetGroupConcurrency, DailyCostLimit: a.BudgetGroupDailyCostLimit, WeeklyCostLimit: a.BudgetGroupWeeklyCostLimit, MonthlyCostLimit: a.BudgetGroupMonthlyCostLimit}
}
