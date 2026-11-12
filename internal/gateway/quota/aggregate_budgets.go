package quota

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"time"

	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/limits/admission"
)

func (a *Admission) ReserveAggregateBudgets(ctx context.Context, authority *keyauthority.Authority, ttl time.Duration, hold CostReservation, costChecked *bool) (*admission.Lease, *fault.Error) {
	labels := authority.BudgetAttribution()
	if !authority.OrganizationBudget.Limited() && !authority.InstallationBudget.Limited() && !authority.ProjectBudget.Limited() && !authority.ProjectAttributionBudgets.Matches(labels) {
		return nil, nil
	}
	var chain *admission.Lease
	for _, boundary := range [...]struct {
		level, subject string
		policy         *keyauthority.BudgetPolicy
	}{
		{"installation", authority.InstallationID, authority.InstallationBudget},
		{"organization", projectBudgetSubject(authority.OrganizationID), authority.OrganizationBudget},
		{"project", projectBudgetSubject(authority.ProjectID), authority.ProjectBudget},
	} {
		if !boundary.policy.Limited() {
			continue
		}
		if e := a.checkCostAccountingOnce(ctx, costChecked); e != nil {
			SettleKey(ctx, chain, false, nil, a.Logger())
			return nil, e
		}
		if !a.Ready() || boundary.subject == "" {
			SettleKey(ctx, chain, false, nil, a.Logger())
			return nil, LimitsUnavailable()
		}
		owner := limits.AggregateBudgetID(boundary.level, boundary.subject)
		lease, failure := a.reserveCostBoundary(ctx, authority.ID, owner, boundary.level, "budget."+boundary.level, authority.BudgetIncreases[owner], boundary.policy, ttl, hold, costChecked)
		if failure != nil {
			SettleKey(ctx, chain, false, nil, a.Logger())
			return nil, failure
		}
		lease.Join(chain)
		chain = lease
	}
	for _, key := range slices.Sorted(maps.Keys(labels)) {
		value := labels[key]
		policy, ok := authority.ProjectAttributionBudgets[key][value]
		if !ok || !policy.Limited() {
			continue
		}
		if e := a.checkCostAccountingOnce(ctx, costChecked); e != nil {
			SettleKey(ctx, chain, false, nil, a.Logger())
			return nil, e
		}
		if !a.Ready() || authority.ProjectID == nil {
			SettleKey(ctx, chain, false, nil, a.Logger())
			return nil, LimitsUnavailable()
		}
		owner := limits.AttributionBudgetID(*authority.ProjectID, key, value)
		lease, failure := a.reserveCostBoundary(ctx, authority.ID, owner, "project attribution", "attribution_budgets."+key+"."+value, authority.BudgetIncreases[owner], &policy, ttl, hold, costChecked)
		if failure != nil {
			SettleKey(ctx, chain, false, nil, a.Logger())
			return nil, failure
		}
		lease.Join(chain)
		chain = lease
	}
	return chain, nil
}

// budgetKind names what an owner enforces in a refusal.
func budgetKind(basis limits.Basis) string {
	if basis == limits.BasisCharge {
		return "charge budget"
	}
	return "cost budget"
}

// Every aggregate boundary uses the same reservation and fail-closed semantics.
// The caller attaches successful leases or refunds earlier boundaries on refusal.
func (a *Admission) reserveCostBoundary(ctx context.Context, keyID, owner, label, param string, increases string, policy *keyauthority.BudgetPolicy, ttl time.Duration, hold CostReservation, costChecked *bool) (*admission.Lease, *fault.Error) {
	if e := a.checkCostAccountingOnce(ctx, costChecked); e != nil {
		return nil, e
	}
	if e := a.CheckPendingCostAccounting(ctx, owner); e != nil {
		return nil, e
	}
	request := hold.On(admission.Request{
		Lookup: limits.AggregateBudgetLookup(owner), LeaseTTL: ttl,
		Cost: admission.Budget(limits.Owner{ID: owner, Kind: limits.OwnerAggregate, Basis: policy.Basis()},
			policy.DailyCostLimit, policy.WeeklyCostLimit, policy.MonthlyCostLimit, increases),
	})
	decision, cancel := context.WithTimeout(ctx, ReserveTimeout)
	defer cancel()
	lease, err := a.limiter.Reserve(decision, request)
	if err == nil {
		return lease, nil
	}
	if exceeded, ok := errors.AsType[*limits.ExceededError](err); ok {
		a.RecordRejection(exceeded.Dimension)
		failure := RateLimitedBasis(exceeded.Dimension, exceeded.RetryAfter, exceeded.Estimate, exceeded.Basis)
		failure.Message = "The " + label + " " + budgetKind(policy.Basis()) + " was exhausted. Unpriced attempts accrue 0."
		failure.Param = &param
		return nil, failure
	}
	return nil, a.Outage(keyID, true, err)
}

func projectBudgetSubject(project *string) string {
	if project == nil {
		return ""
	}
	return *project
}

func budgetBoundaryError(e *fault.Error, name, param string) *fault.Error {
	e.Message = strings.Replace(e.Message, "API key", name, 1)
	e.Param = &param
	return e
}

// budgetBoundary retains only a fixed hierarchy label, never a request label/value,
// route name, owner identifier or provider error text.
func BudgetBoundary(err *fault.Error) string {
	if err == nil || err.Code != "budget_exhausted" {
		return ""
	}
	if err.Param != nil {
		switch *err.Param {
		case "budget.installation":
			return "installation"
		case "budget.organization":
			return "organization"
		case "budget.project":
			return "project"
		case "budget_group":
			return "budget_group"
		case "end_user.key":
			return "key_end_user"
		case "end_user.project":
			return "project_end_user"
		}
		if strings.HasPrefix(*err.Param, "route_limits.") {
			return "key_route"
		}
		if strings.HasPrefix(*err.Param, "attribution_budgets.") {
			return "attribution"
		}
	}
	return "api_key"
}
