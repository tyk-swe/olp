package authority

import (
	"encoding/json"
	"github.com/tyk-swe/olp/internal/routeidentity"
	"maps"
	"math/big"
)

// LimitTemplates supplies independent ceilings at each referencing boundary.
// Templates share policy, not counters. Inline settings may only tighten them.
type LimitTemplates map[string]AdmissionLimits

func (t LimitTemplates) ValidateAndNormalize() error {
	if len(t) > 64 {
		return invalid("templates", "Use at most 64 project limit templates.")
	}
	for name, limits := range t {
		if !routeidentity.ValidSlug(name) {
			return invalid("templates", "Use valid route-style names for templates.")
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

func ValidateTemplateName(name *string) error {
	if name != nil && !routeidentity.ValidSlug(*name) {
		return invalid("limit_template", "Use a valid template name or null.")
	}
	return nil
}

func (t LimitTemplates) Resolve(name *string, local AdmissionLimits) (AdmissionLimits, error) {
	if name == nil {
		return local, nil
	}
	base, ok := t[*name]
	if !ok {
		return AdmissionLimits{}, invalid("limit_template", "The referenced template must exist in the same project.")
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
	return l, l.ValidateAndNormalize()
}

func intersectLimits(a, b AdmissionLimits) AdmissionLimits {
	if a.CostBasis == "" {
		a.CostBasis = b.CostBasis
	}
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
	return AdmissionLimits{minimum(a.RequestsPerMinute, b.RequestsPerMinute), minimum(a.TokensPerMinute, b.TokensPerMinute), minimum(a.MaxConcurrency, b.MaxConcurrency), money(a.DailyCostLimit, b.DailyCostLimit), money(a.MonthlyCostLimit, b.MonthlyCostLimit), money(a.WeeklyCostLimit, b.WeeklyCostLimit), a.CostBasis}
}

func (p KeyPolicy) AdmissionLimits() AdmissionLimits {
	return AdmissionLimits{p.RequestsPerMinute, p.TokensPerMinute, p.MaxConcurrency, p.DailyCostLimit, p.MonthlyCostLimit, p.WeeklyCostLimit, p.CostBasis}
}

func (t LimitTemplates) EndUser(p *EndUserPolicy) (*EndUserPolicy, error) {
	if p == nil || p.LimitTemplate == nil {
		return p, nil
	}
	copy := *p
	var err error
	if copy.Defaults, err = t.Resolve(p.LimitTemplate, p.Defaults); err != nil {
		return nil, err
	}
	copy.Overrides = maps.Clone(p.Overrides)
	for digest, local := range copy.Overrides {
		copy.Overrides[digest], err = t.Resolve(p.LimitTemplate, local)
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
		return invalid("limit_template", "Templates require a project.")
	}
	local, err := t.Resolve(a.Policy.LimitTemplate, a.Policy.AdmissionLimits())
	if err != nil {
		return err
	}
	a.Policy.RequestsPerMinute, a.Policy.TokensPerMinute, a.Policy.MaxConcurrency = local.RequestsPerMinute, local.TokensPerMinute, local.MaxConcurrency
	a.Policy.DailyCostLimit, a.Policy.MonthlyCostLimit = local.DailyCostLimit, local.MonthlyCostLimit
	a.Policy.WeeklyCostLimit = local.WeeklyCostLimit
	a.Policy.CostBasis = local.CostBasis
	if a.Policy.EndUserPolicy, err = t.EndUser(a.Policy.EndUserPolicy); err != nil {
		return err
	}
	if a.ProjectEndUserPolicy, err = t.EndUser(a.ProjectEndUserPolicy); err != nil {
		return err
	}
	group, err := t.Resolve(a.BudgetGroupTemplate, AdmissionLimits{a.BudgetGroupRPM, a.BudgetGroupTPM, a.BudgetGroupConcurrency, a.BudgetGroupDailyCostLimit, a.BudgetGroupMonthlyCostLimit, a.BudgetGroupWeeklyCostLimit, a.BudgetGroupCostBasis})
	if err != nil {
		return err
	}
	a.BudgetGroupCostBasis = group.CostBasis
	a.BudgetGroupRPM, a.BudgetGroupTPM, a.BudgetGroupConcurrency = group.RequestsPerMinute, group.TokensPerMinute, group.MaxConcurrency
	a.BudgetGroupDailyCostLimit, a.BudgetGroupMonthlyCostLimit = group.DailyCostLimit, group.MonthlyCostLimit
	a.BudgetGroupWeeklyCostLimit = group.WeeklyCostLimit
	return nil
}

func (a Authority) GroupLimits() AdmissionLimits {
	return AdmissionLimits{RequestsPerMinute: a.BudgetGroupRPM, TokensPerMinute: a.BudgetGroupTPM, MaxConcurrency: a.BudgetGroupConcurrency, DailyCostLimit: a.BudgetGroupDailyCostLimit, WeeklyCostLimit: a.BudgetGroupWeeklyCostLimit, MonthlyCostLimit: a.BudgetGroupMonthlyCostLimit, CostBasis: a.BudgetGroupCostBasis}
}
