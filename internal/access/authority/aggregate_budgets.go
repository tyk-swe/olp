package authority

import (
	"github.com/tyk-swe/olp/internal/limits"
)

// BudgetPolicy caps aggregate invoiced inference spend. Nil dimensions are unlimited.
type BudgetPolicy struct {
	DailyCostLimit   *string `json:"daily_cost_limit"`
	MonthlyCostLimit *string `json:"monthly_cost_limit"`
	WeeklyCostLimit  *string `json:"weekly_cost_limit"`
	// CostBasis is "cost" (the default) or "charge".
	CostBasis string `json:"cost_basis,omitempty"`
}

func (p *BudgetPolicy) Basis() limits.Basis {
	if p != nil && p.CostBasis == "charge" {
		return limits.BasisCharge
	}
	return limits.BasisCost
}

func (p *BudgetPolicy) Limited() bool {
	return p != nil && (p.DailyCostLimit != nil || p.MonthlyCostLimit != nil || p.WeeklyCostLimit != nil)
}

func (p *BudgetPolicy) ValidateAndNormalize() error {
	if p == nil {
		return nil
	}
	return (AdmissionLimits{DailyCostLimit: p.DailyCostLimit, MonthlyCostLimit: p.MonthlyCostLimit, WeeklyCostLimit: p.WeeklyCostLimit, CostBasis: p.CostBasis}).ValidateAndNormalize()
}
